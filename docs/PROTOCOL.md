# How Bond works

This is the reference for what goes on the wire and why the engine makes the
decisions it does. The code is the authority; this explains it.

## Layers

```
application traffic
  IP packet                         read from a TUN device (or a userspace TCP/IP stack)
    WireGuard datagram              encryption, peer authentication — unmodified wireguard-go
      bond datagram                 28-byte header: which session, which path, what order
        UDP  (or one WebSocket binary message, as a fallback)
```

Bond is WireGuard's transport. wireguard-go lets its "bind" — the thing that
sends and receives datagrams — be replaced; Bond's bind sends each datagram
over one or more *paths* and delivers what arrives, in order, as if it had
come from a single peer. WireGuard's notion of the peer's address is a whole
bonded *session* (`bond:<session id>`), which stays the same however the
paths underneath come and go.

## The bond header

```
 0      1        2       3       4          8          12          16         20        28
+------+--------+-------+-------+----------+----------+-----------+----------+---------+---------
|ver|ty| pathID | flags | class | session  | path seq | global seq| send ts  |   tag   | payload
+------+--------+-------+-------+----------+----------+-----------+----------+---------+---------
```

| Field | |
|---|---|
| `ver`/`type` | version 1; DATA (a WireGuard datagram), PROBE (feedback), HELLO (path announcement) |
| `pathID` | chosen by the hub; the aggregator echoes it |
| `flags` | direction (hub→aggregator or back); whether other copies of this datagram were sent |
| `class` | bulk, realtime, control |
| `session` | which hub↔aggregator bond |
| `path seq` | counts every datagram sent on this path: loss and liveness are measured against it |
| `global seq` | counts data datagrams across all paths: reordering and duplicate removal use it |
| `send ts` | sender's clock in ms, echoed back to measure round trips |
| `tag` | SipHash-2-4 over the entire datagram, keyed by a per-session key from the join |

The header is not secret (it is comparable to what QUIC leaves visible) but it
is authenticated, payload included. Without the tag, anyone who could inject a
packet could move a path to their own address, mark sequence numbers as
already seen, or forge the feedback that scheduling relies on. The direction
bit is covered by the tag so a party's own datagrams cannot be bounced back at
it.

Tunnel MTU is 1280: 1280 + 32 (WireGuard) + 28 (bond) + 48 (IPv6/UDP) = 1388,
which fits any path that carries 1500-byte packets with room for PPPoE,
CLAT and the like.

## Paths

A path is one way of getting a datagram from hub to aggregator:

- **direct** — a UDP socket pinned to one network interface
  (`SO_BINDTODEVICE` on Linux, `IP_BOUND_IF` on macOS and iOS,
  `Network.bindSocket` on Android). Pinning is what lets the tunnel own the
  default route while the bond's own sockets still leave by the physical
  interfaces.
- **relay** — a UDP socket to a relay on the LAN, which forwards to the
  aggregator from its own uplink. The relay keeps a NAT-style mapping per hub
  socket and is otherwise stateless.
- **ws** — a WebSocket to the aggregator, one binary message per datagram.

The hub opens paths; the aggregator learns them from what arrives and replies
to wherever a path's newest datagram came from, so a path survives its address
changing (a phone moving between cells, a NAT rebinding).

### Coming up and going down

A path is **up** only after a round trip: the peer has to echo a timestamp
this end sent since the path was created or last declared down. Receiving
traffic is not enough — replaying old datagrams must not be able to make a
dead path look alive or bring a forged one up. For the same reason only
datagrams that advance the path sequence count as signs of life or may move
the path's address.

Bring-up takes one round trip (hub) and one and a half (aggregator): each end
answers immediately on a path that is not up yet instead of waiting for its
next idle probe.

A path goes **down** when it has been silent too long: 300–800 ms (scaled by
its round-trip time) while data is being sent on it and feedback is expected,
longer while idle. When nothing is up, datagrams are still sent — on the path
heard from most recently — because a wrong guess about liveness should not
become a blackout.

### Feedback

Every path carries a PROBE each way at least every 250 ms, and every 25 ms
while data is arriving. A probe reports cumulative counters — highest path
sequence seen, datagrams and bytes received — and echoes the newest send
timestamp with how long it was held. From that the other end derives:

- round-trip time (smoothed, variance, and a windowed minimum),
- loss (datagrams the peer counted against sequence numbers it advanced over),
- delivery rate, and from it an estimate of the path's capacity,
- how many bytes are in flight: sent but not yet reported seen.

Counters are cumulative so a lost probe costs nothing.

### A path that comes back

An uplink that goes away and returns — a tether replugged, a phone changing
access point — gets a new socket, and its sequence numbers start again, but
the hub may well give it the same path id. To the aggregator those datagrams
would look like old traffic arriving late and be ignored. So each HELLO also
carries an *instance* number, higher for every path the hub opens. A higher
instance on a known id tells the aggregator this is a new path: it forgets the
old one's sequence numbers, takes the new address, and makes it prove itself
with a round trip like any other. A lower or equal instance is a replay.

### Restarts

Each process has a *generation* number, larger on every start (taken from the
clock, and from a file so that it still increases on a machine whose clock went
backwards). HELLO carries it. When an end sees a higher generation from its
peer it forgets the peer's sequence numbers, marks every path down until it
proves itself again, and — on a hub — tells WireGuard to handshake at once
rather than after its fifteen-second dead-peer timer. Recovery from an
aggregator restart takes about a second.

## Scheduling

Each datagram goes to the path with the lowest estimated arrival time:

```
cost = (srtt / 2  +  queue) × (1 + 8 × loss)  [+ 10 ms if the path is "bonded" rather than "primary"]

queue = max(0, in-flight bytes − 1.25 × capacity × (min rtt + 25 ms)) / capacity
```

The subtraction matters. A path running at capacity always has about
*capacity × round trip* bytes unacknowledged; only what exceeds that is a
queue. Without the allowance a long path looks congested merely for being
long and is never filled. The factor of 1.25 lets a path be driven a little
past its estimate, which is how the estimate finds headroom.

The capacity estimate rises when the peer reports receiving more than the
estimate, and falls only when the path was offered clearly more than it
delivered while holding a backlog. An idle or lightly used path keeps what was
last learned about it; a download does not talk a hub out of its uplinks.

**Policies** pick the candidates. Healthy `primary` and `bonded` paths first;
if there are none, healthy `backup` paths; then paths that are up but lossy;
last, anything not switched off. A path is unhealthy after two seconds above
20 % loss — long enough that a TCP flow overrunning a queue for a moment does
not trigger a failover.

**Classes** decide how many copies and how they are treated:

- *bulk* — one path. Reordered on receipt.
- *realtime* — the two cheapest paths, reaching into backups for the second
  copy if need be. Delivered on arrival, never held. While realtime traffic is
  flowing, bulk datagrams are dropped rather than sent into a queue deeper
  than 100 ms, which is how the bulk flows' own congestion control learns to
  make room.
- *control* — WireGuard handshakes and keepalives: every live path.

When a path that has been idle is put back to work, the first round trip's
worth of datagrams placed on it are also sent on a path already in use. The
receiver gives up on a missing datagram once every path it knows to be
delivering has moved past it, and it cannot know an idle path has woken until
something arrives on it; without the overlap, the first datagrams on the
reawakened path would be written off and arrive late, which a TCP flow reads
as loss just as it is trying to speed up.

### Which class is a datagram?

Classification needs the plaintext packet, but the bind only ever sees
WireGuard's ciphertext. The tunnel wrapper classifies each packet as
WireGuard reads it from the TUN and queues the answer; the bind takes answers
off the queue as the encrypted datagrams come out, matching by position and by
length (WireGuard keeps a peer's packets in order and pads deterministically).
If the two ever slip — WireGuard dropped a packet mid-handshake — a datagram
gets the wrong class for a moment. It still gets delivered.

Priority rules match devices, ports and DSCP. Matched packets are stamped
DSCP EF before encryption. The aggregator, seeing EF on what comes out of the
tunnel, records the flow and gives the replies the same class. In gateway mode
the hub's firewall does the per-device marking before NAT hides the device.

## Reordering

The receiver delivers data datagrams in global-sequence order. A gap at the
head is given up on when either

- every path that has delivered data in the last second has delivered
  something newer (the datagram is lost, not late), or
- the datagram waiting behind the gap has been held for the *hold* time.

Hold adapts: three quarters of the spread between the delivering paths'
round-trip times, plus twice the largest variance, plus extra after any
datagram arrives that had already been given up on. With one path delivering
there is nothing to wait for and the hold is 5 ms. Late arrivals are still
delivered, once; duplicates are dropped.

## Joining

A join code is `bond://host:port/TOKEN`, the token being 160 random bits.
The device sends the aggregator a request naming the token by a hash and
encrypted (XChaCha20-Poly1305) under a key derived from it; the reply is
encrypted the same way, with the direction bound in. The device learns the
aggregator's WireGuard key, its own address, and its session id and key. The
token itself never crosses the network. An eavesdropper learns nothing; an
impostor cannot produce a reply the device will accept. The exchange runs over
plain HTTP on the aggregator's TCP port.

A device that joins again with the same WireGuard key keeps its identity.

## Finding the aggregator

A join code may name the aggregator by hostname. Once a hub's tunnel owns the
default route — and on phones, DNS — that name can only be looked up *through*
the tunnel, which cannot come up until the aggregator has been found. So a
device records where the name resolved to when it joined, refreshes that on
every successful lookup, and connects on the remembered address whenever a
lookup fails. A hub also does its first lookup before it touches the routing
table.

## IPv4 only, deliberately

The tunnel carries IPv4. A hub that captures all traffic therefore makes the
IPv6 Internet unreachable for as long as it runs — reject routes for `::/1`
and `8000::/1`, so connections fail immediately rather than time out and
applications fall back to IPv4 at once. Without that, a dual-stack network
would send much of the machine's traffic around the bond. On Android an
unconfigured address family is blocked by the VPN itself; on iOS the app
routes IPv6 into the tunnel, where it is dropped.

## Known limits

- **Replay across restarts.** Session keys outlive a process, so a datagram
  recorded during an earlier run still carries a valid tag. The round-trip
  rule stops replays from raising paths within a run; across a restart, a
  replayed datagram can occupy a path slot on the aggregator for ten seconds.
  It can cause no traffic to be sent anywhere new and nothing to be decrypted.
- **Reverse-direction congestion.** Round-trip time cannot tell which direction
  is slow. A saturated download inflates the cost of the same path for
  uploads. One-way delay measurement would fix it.
- **The WebSocket path** is TCP and stalls on loss. It is there to stay
  connected, not to be fast.
- **The class queue is a heuristic** (see above).
- **Userspace aggregator and ICMP.** In `--userspace` mode the aggregator
  terminates TCP and UDP and re-originates them; pings to the Internet are
  answered by the aggregator itself, not the real destination. Kernel mode
  forwards real ICMP.
- **Sequence numbers are 32-bit** with serial-number arithmetic: fine at any
  realistic rate, but a session idle through more than 2³¹ of the peer's
  datagrams would misjudge order once.
