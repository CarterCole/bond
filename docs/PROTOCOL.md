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
| `ver`/`type` | version 2; DATA (a WireGuard datagram), PROBE (feedback), HELLO (path announcement) |
| `pathID` | chosen by the hub; the aggregator echoes it |
| `flags` | direction (hub→aggregator or back); whether other copies of this datagram were sent |
| `class` | bulk, realtime, control |
| `session` | which hub↔aggregator bond |
| `path seq` | counts every datagram sent on this path: loss and liveness are measured against it |
| `global seq` | counts data datagrams across all paths: reordering and duplicate removal use it |
| `send ts` | sender's clock in ms, echoed back to measure round trips |
| `tag` | SipHash-2-4 over the entire datagram, keyed by the session's key from the join bound to the sender's generation (below) |

The header is not secret (it is comparable to what QUIC leaves visible) but it
is authenticated, payload included. Without the tag, anyone who could inject a
packet could move a path to their own address, mark sequence numbers as
already seen, or forge the feedback that scheduling relies on. The direction
bit is covered by the tag so a party's own datagrams cannot be bounced back at
it.

The key is not the session key itself but one derived from it for each run of
the sending process: two SipHash outputs under the session key, of
`bondgen0` and `bondgen1` each followed by the run's generation (see
[Restarts](#restarts)). A datagram recorded during one run carries a tag no
later run accepts. Version 1 tagged with the session key directly, which let a
single recorded datagram, replayed after either end restarted, take a path
over. Version 1 and 2 devices cannot talk to each other; an aggregator logs it
when one tries.

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
  It is plain on the aggregator's TCP port, or over TLS (`wss://`) when the
  aggregator serves it with `--tls` or sits behind a Cloudflare Tunnel. The
  hub does not check the certificate: the datagrams are sealed already, and
  TLS is there to look like HTTPS to networks that allow nothing else.

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
access point — gets a new socket, and the hub may well give it the same path
id. Its sequence numbers carry on from where the last path with that id left
off, never starting again within a run, so nothing the old socket sent can
pass for new. Each HELLO also carries an *instance* number, higher for every
path the hub opens. A higher instance on a known id tells the aggregator this
is a new path: it forgets what the far end had counted, takes the new address
from the first datagram that advances the sequence, and makes the path prove
itself with a round trip like any other. A HELLO of a lower instance is old
traffic, and dropped. The aggregator numbers its own paths the same way, so a
path it forgot after a minute's silence and makes again carries on too.

### Restarts

Each process has a *generation* number, larger on every start (taken from the
clock, and from a file so that it still increases on a machine whose clock went
backwards; a phone, which has no such file, keeps it in its config, and the
app stores that before every start). HELLO carries it, and is tagged under the
key of the generation it names. Anything else is accepted only under the key
of the generation adopted from the peer's HELLOs, so an end that has just
started takes nothing but a HELLO, and a HELLO naming an older generation
than one already adopted is dropped. When an end sees a higher generation from
its peer it forgets the peer's sequence numbers, marks every path down until
it proves itself again, and — on a hub — tells WireGuard to handshake at once
rather than after its fifteen-second dead-peer timer. A hub announces a path
again every 200 ms whenever it goes down, until the aggregator answers, so an
aggregator that has restarted learns its generation within a moment of
noticing. Recovery from an aggregator restart takes about a second.

Only the newest datagram on a path counts as a sign of life, may move the
path, or has its feedback digested: the counters are cumulative, so an older
report says nothing new, and a replayed one could only skew the estimates.

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
impostor cannot produce a reply the device will accept, and the reply is
bound to the request it answers (its nonce is in the reply's additional data),
so one recorded on its way to one device is no use to another. The request
names the protocol version the device speaks; a device or aggregator on the
other version is told, in a reply it can read, to update. The exchange runs
over plain HTTP on the aggregator's TCP port, which accepts at most 64
connections from one address and 2048 in all, and gives a request 15 seconds
to arrive.

The standing join code works until it is replaced (`bond code --rotate`);
removing a device does not stop it joining again with a code it still holds.

A device that joins again with the same WireGuard key keeps its identity.

## Finding the aggregator

A join code may name the aggregator by hostname. Once a hub's tunnel owns the
default route — and on phones, DNS — that name can only be looked up *through*
the tunnel, which cannot come up until the aggregator has been found. So a
device records where the name resolved to when it joined, refreshes that on
every successful lookup, and connects on the remembered address whenever a
lookup fails. A hub also does its first lookup before it touches the routing
table.

## Through a rendezvous

An aggregator started with `--via <pairing code>` needs no inbound port. It
dials out to a rendezvous on a public host, and the join codes it hands out
name the rendezvous, so hubs, relays and phones send there exactly what they
would send to an aggregator. Nothing changes for them.
[TOPOLOGY.md](TOPOLOGY.md) has the design and what comes after.

**Pairing.** A pairing code, `bond-rv://host:port/SECRET`, holds a 160-bit
secret like a join code's. The aggregator opens a WebSocket to
`/v1/rendezvous` on the rendezvous's TCP port and sends a hello sealed
(XChaCha20-Poly1305) under a key derived from the secret, carrying 32 random
bytes. The welcome comes back sealed the same way and bound to the hello,
carrying 32 random bytes of the rendezvous's own and a link id. Both ends
derive the connection's keys from the secret and both sets of random bytes: a
link key, and a key for each direction of control messages. A replayed hello
earns a welcome that nobody can use.

**Control** messages are sealed with a counter for nonce, which the reader
insists on, so none can be replayed, reordered or dropped unnoticed. The first
must be a registration: the mesh id, every node's session id and every join
token's id. It is sent again whenever those change, and before the answer to
every join, so a device that has just joined is routed before it sends
anything. A registration lasts as long as its connection; a new connection
for the same mesh replaces it. Joins reach the rendezvous as ordinary POSTs to
`/v1/join`, go to the aggregator that registered the token id the request
names, and are answered the same way back, sealed under the join code
throughout.

**The link** is a UDP socket from the aggregator to the rendezvous's UDP port.
Each datagram crosses it in a frame:

```
 0       1        5          9          13         21
+-------+--------+----------+----------+----------+-------------------
| type  |  link  |  handle  | link seq |   tag    | bond datagram ...
+-------+--------+----------+----------+----------+-------------------
```

| Field | |
|---|---|
| `type` | data, ping or pong: 0xB1–0xB3. A bond datagram has its version in those four bits, so the two share a port |
| `link` | which aggregator's link, from the welcome |
| `handle` | the rendezvous's number for one hub socket: a UDP address and port, or a WebSocket connection |
| `link seq` | counts frames each way |
| `tag` | SipHash-2-4 over the frame, keyed by the link key |

The aggregator pings every second until the first pong, then whenever the link
has been idle for 25 seconds, which keeps the NAT mapping in front of it open.
A frame that verifies and is newer than any before moves the link to wherever
it came from, as a path roams, so a home line whose address changes carries
on.

To the aggregator a handle is a remote like any other, and a path through the
rendezvous moves between handles by the usual rule: only a datagram that
verifies and advances the path sequence moves it. A rendezvous that misroutes
can break a path but not redirect one. The first such datagram from a handle
also makes the aggregator *confirm* it on the control connection; the
rendezvous answers with the address it sees the hub socket at. A full 1340-byte datagram is 1409
bytes on the link with IPv6 and UDP headers.

**What the rendezvous refuses:** datagrams that do not parse as travelling from
a hub to an aggregator, or whose session nobody registered; frames that fail
their tag; replies that are not going from an aggregator to a hub, or are for
a handle silent for a minute; more than 1024 hub sockets for one aggregator.
It cannot check a datagram's tag, so anyone who knows a session id can make
hub sockets, from as many addresses as they can send from. A socket the
aggregator has not confirmed is forgotten after ten seconds' silence, and
makes way for a newcomer when the 1024 are taken; confirmed ones never do.
It has no session keys, so it routes on the session id alone. A session id
another aggregator registered first stays with that one; the late aggregator
is told and logs it, and that device would have to join again to get another
id.

## IPv4 only, deliberately

The tunnel carries IPv4. A hub that captures all traffic therefore makes the
IPv6 Internet unreachable for as long as it runs — reject routes for `::/1`
and `8000::/1`, so connections fail immediately rather than time out and
applications fall back to IPv4 at once. Without that, a dual-stack network
would send much of the machine's traffic around the bond. On Android an
unconfigured address family is blocked by the VPN itself; on iOS the app
routes IPv6 into the tunnel, where it is dropped.

## Known limits

- **Replay within a run.** A datagram replayed during the run that sent it
  verifies, but is never the newest on its path once a newer one has
  arrived, so it moves nothing. Against an aggregator that has just started,
  a replayed HELLO of the hub's current run can make a path, pointing at the
  replayer, until the hub's own next datagram on it — at most a quarter of a
  second — takes it back; never sent a round trip, it is forgotten in ten
  seconds.
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
- **The rendezvous speaks plain HTTP.** Its control connection and WebSocket
  paths use its TCP port without TLS. Everything on them is sealed, but they
  do not look like HTTPS to a network that allows nothing else.
- **One pairing code per rendezvous.** Every aggregator holding it is trusted
  not to claim another's mesh or sessions. Nor is there yet a limit on the
  rate any one source may send to a rendezvous.
- **Sequence numbers are 32-bit** with serial-number arithmetic: fine at any
  realistic rate, but a session idle through more than 2³¹ of the peer's
  datagrams would misjudge order once.
