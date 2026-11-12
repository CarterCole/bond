# Hidden nodes and link topology

**Status: phase 1 is built** — a rendezvous, and an aggregator that
registers with it (see [Order of work](#order-of-work)). PROTOCOL.md describes
it as built, under [Through a rendezvous](PROTOCOL.md#through-a-rendezvous).
Everything else here is a design. Where this and PROTOCOL.md or the code
disagree, they win.

Two additions, the second leaning a little on the first:

1. **Hidden nodes.** An aggregator with no address its hubs can reach —
   behind a router that will not forward a port, behind carrier NAT, or
   plugged into a gateway beside the router that holds the public address —
   dials out to a *rendezvous* on a public host, and hubs reach it through
   that. Once each end knows the other's public address, they try to talk
   directly.
2. **Link topology.** Every node describes its links — what kind, how fast
   each way, whether up and down share one budget, whether they cost money —
   and the mesh keeps the whole graph. New paths start from what is known
   instead of a guess, upload and download delay are kept apart, and later
   the graph is how a hub chooses between aggregators.

## What this is not

- **Not anonymity.** A Tor onion service hides its address from everyone,
  the relays it uses included: it and its clients each reach a rendezvous
  point through a three-hop circuit. Bond's hidden node is an aggregator,
  where its hubs' traffic joins the Internet, so every site they visit sees
  its address anyway. Hiding buys reachability without an open port, and a
  join code that names the rendezvous rather than the aggregator. The
  rendezvous operator sees both ends' addresses. Tor itself is no use as a
  transport: it carries TCP streams, not datagrams, and adds hundreds of
  milliseconds.
- **Not BGP.** BGP announces which prefixes can be reached through whom, with
  policy attached, and deliberately says nothing about the links inside a
  network. What is wanted here — the links, their speeds and properties, and
  every node holding the whole graph — is link-state routing, the family of
  OSPF and IS-IS. Something BGP-shaped would only be worth having between
  meshes run by different people.
- **Not routing hop by hop.** Bond's reordering and capacity estimates assume
  one sender and one receiver per path. A path can run through other nodes,
  as relay paths do today, but those nodes only forward. Nothing in the middle
  schedules, reorders or retransmits, because that would hide from the ends
  the loss and delay they steer by.

## Part 1: hidden nodes

```
  hub ──── udp ───▶ ┌────────────┐ ◀══ link: udp, dialled out ════ ┌───────────────────┐
  hub ──── wss ───▶ │ RENDEZVOUS │ ◀══ control: wss, dialled out ══│ hidden AGGREGATOR │──▶ Internet
  relay ── udp ───▶ └────────────┘                                 └─────────┬─────────┘
  hub ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ punched path, when both NATs allow it ─ ─ ─ ─ ─ ─┘
```

A **rendezvous** is a relay on a public host. Instead of forwarding to an
aggregator address it was given, it forwards to aggregators that have dialled
in to it. Like a relay it can read nothing: the datagrams are sealed by
WireGuard and tagged with keys it does not have.

### Pairing

```bash
bondd rendezvous --public rv.example.com                   # on a VPS: prints a pairing code
sudo bondd server --via 'bond-rv://rv.example.com:7777/…'   # on the hidden aggregator
```

A pairing code has a join code's form and strength: 160 random bits that
never cross the network. The aggregator proves it holds the code with the same
sealed exchange a join uses, and the key for the link (below) is derived from
the code and fresh randomness from both ends. One rendezvous can serve several
aggregators. For now it has a single pairing code, so aggregators that share
it trust each other; a code for each is for later.

The aggregator keeps two connections to the rendezvous, both dialled out, so
no inbound port is needed:

- **control** — a WebSocket on the rendezvous's TCP port, carrying the
  registration, joins and notices about new sources, every message sealed.
  It is not on TLS yet, so it does not look like HTTPS;
- **link** — a UDP socket, carrying bonded traffic. What it sends keeps the
  NAT mapping in front of the aggregator alive.

Registration names the mesh and lists what the rendezvous routes by: the
session ids of the mesh's nodes and the ids of its live join tokens. Both are
kept current as devices join and are removed and as codes are minted and
revoked. Session ids are random 32-bit numbers; if one comes up that another
aggregator has registered, the rendezvous refuses it and says so. For now the
aggregator only logs that, and the device has to join again for another id.
Drawing again before the device is told its id is for later.

### Joining

The join code names the rendezvous — `bond://rv.example.com:7777/TOKEN`, the
format unchanged. The device posts its sealed request to the rendezvous, which
finds the aggregator that registered the token id the request names, passes
the envelope along the control connection and returns the reply. The exchange
is sealed end to end under the token, so the rendezvous learns only that a
join happened. The addresses in the reply (`udp`, `ws`) are the rendezvous's.

### Datagrams

Between hub and rendezvous nothing changes: these are ordinary bond
datagrams, over UDP or a WebSocket. The rendezvous reads the session id from
the header (it cannot check the tag), finds the aggregator that registered it,
and passes the datagram along the link in a frame:

```
 0       1        5          9          13         21
+-------+--------+----------+----------+----------+-------------------
| type  |  link  |  handle  | link seq |   tag    | bond datagram ...
+-------+--------+----------+----------+----------+-------------------
```

| Field | |
|---|---|
| `type` | data, ping or pong |
| `link` | which aggregator's link this is, so the rendezvous knows which key to check a frame with whatever address it comes from |
| `handle` | the rendezvous's name for where the datagram came from: a UDP address and port, or a WebSocket connection |
| `link seq` | counts frames each way on the link; roaming uses it as it uses a path's sequence |
| `tag` | SipHash-2-4 over the frame, keyed by the link key |

When a source is new, the rendezvous tells the aggregator, over the control
connection, the handle it has given it and the address it was seen from (a
frame that gets there first is handled all the same). The aggregator gives each handle its own sender, as it gives each
WebSocket connection one today, so a path through the rendezvous is an
ordinary path whose remote happens to be a handle. Roaming needs nothing new:
the aggregator moves a path to a new handle only when the datagram inside
verifies against the session key and advances the path sequence, which the
rendezvous cannot fake. A rendezvous that misroutes can cost a path, not
redirect one.

Replies go back the same way. The rendezvous delivers a reply only to a handle
that has sent something in the last minute, as a NAT would, and only if it is
a datagram going from an aggregator to a hub, so an aggregator cannot use it
to send whatever it likes wherever it likes.

A full datagram from a hub — 1340 bytes: the tunnel's 1280, WireGuard's 32 and
bond's 28 — is 1409 on the link with IPv6 and UDP headers, which still fits
any path that carries 1500-byte packets, PPPoE included.

The link itself roams the way a path does: a frame that verifies and advances
the link sequence from a new address moves the link there. An aggregator on a
home line whose address changes overnight carries on.

### What a rendezvous refuses

A relay forwards only for addresses on its own LAN. A rendezvous has to take
datagrams from anywhere, so it is bounded in other ways:

- only datagrams that parse as travelling from a hub to an aggregator, for a
  session some aggregator registered;
- only to the aggregator that registered that session, one datagram out for
  each one in: nothing to amplify;
- a cap on handles per aggregator (1024), and, not built yet, on the rate
  from any one source;
- nothing on the link that does not verify.

Refusals are counted, as a relay's are.

### Punching through

Once the aggregator knows the address a hub's uplink was seen from (the
handle notice) and its own address as the rendezvous sees it, the two can try
to reach each other directly:

1. The aggregator tells the hub where to try: a `try` list in its HELLO, which
   carries the session tag, so only the aggregator can say it.
2. The hub sends from the same socket as that uplink's rendezvous path,
   because that socket's NAT mapping is the one the rendezvous saw. The
   aggregator sends from its link socket to the hub's observed address, which
   opens its own NAT to the reply.
3. The first datagram to arrive with a good tag makes a new path, which
   proves itself with a round trip like any other.

When the direct path is up, the rendezvous path over the same uplink becomes
`backup`: a few probes a second, ready if the direct one fails, and not
carrying a second copy of realtime traffic.

Punching works only when the NATs on both sides map *independently of the
endpoint* (RFC 4787): the address the rendezvous saw is the address anyone
else will reach. A rendezvous listening on two ports can tell, the way STUN
does (RFC 5780), by comparing the mappings it sees. Behind a NAT that maps per
destination the aggregator does not ask the hub to try. Home routers usually
map independently; carrier NAT on phones often does not, so phones on
cellular will often stay on the rendezvous. Whether the AT&T gateway and
Google Wifi used for testing map independently is not known yet.

NAT mappings for UDP are supposed to last at least two minutes (RFC 4787) and
are often shorter. Every path probes at least every 250 ms, which keeps its
own mappings open; the link sends a keepalive every 25 seconds when nothing
else is crossing it.

### What it costs

Through a rendezvous, every byte leaves the VPS once: uploads on their way to
the aggregator, downloads on their way to the hub. That is what an aggregator
on the same VPS would cost, plus a hop of latency, and the VPS's bandwidth
still caps the bond. If punching fails, nothing has been saved and running the
aggregator on the VPS is simpler. A rendezvous pays off when the direct paths
come up, or when the traffic should leave from the hidden node's own
connection — a home line, an office. `bond status` and the dashboard on the
aggregator mark which paths go through a rendezvous.

### Reachable and hidden at once

An aggregator that UPnP or `--public` makes reachable could list both its own
address and the rendezvous. Hubs use only the first address in `udp` today, so
until they open paths to several, an aggregator given `--via` lists the
rendezvous alone. The rendezvous can also tell an aggregator whether it is
reachable after all, by sending to the address the aggregator claims and
seeing whether it arrives.

### What changes where

| | Phase 1 (built): one rendezvous, no punching | Later |
|---|---|---|
| aggregator | `--via`; control and link clients; a sender per handle; registration kept current | `try` lists; listing its own address as well |
| rendezvous | new: `bondd rendezvous` and `core/rendezvous` | TLS; a pairing code per aggregator; NAT classification on a second port |
| hub | nothing | reading `try`; paths sharing an uplink's socket; several aggregator addresses |
| relay, phone apps | nothing | nothing |

## Part 2: link topology

### What a node says about itself

Each node publishes an **advert**: what it is, how it can be reached, and its
links.

```
advert
  node       the node's WireGuard public key
  gen, seq   process generation and a counter; the newest (gen, seq) wins
  expires    an advert not refreshed in time disappears
  roles      exit (aggregator) | rendezvous | relay | hub
  reach      public [host:port …] | via [rendezvous …]
  nat        none | endpoint-independent | per-destination | unknown
  links      [link …]

link
  id         stable within the node: the path name, en0 or relay:Pixel-9
  peer       the node at the far end
  via        nodes in between: a relay, a rendezvous
  kind       direct | relay | ws | rendezvous | punched
  medium     ethernet | wifi | cellular | satellite | unknown
  up, down   measured capacity each way, bits/s
  limit      declared caps each way, if the user set any
  shared     up and down draw on one budget: yes | no | unknown
  iface      the interface's own link rate and duplex, where the OS reports it
  group      shared-risk labels: links that would fail or fill together
  rtt_min, loss, mtu, metered, policy
```

Most of it is measured already or can be asked of the OS:

- **medium** and **metered** come from the platform: Android's network
  capabilities (transport type, `NOT_METERED`), Apple's interface type and
  `isExpensive`, the interface type under `/sys/class/net` on Linux. A
  tethered phone looks like Ethernet to the hub; its driver (`rndis_host`,
  `cdc_ether`, `ipheth`) gives it away.
- **iface** is the network card's negotiated rate and duplex, as `ethtool`
  shows it: 1 Gbit/s full duplex to the router, or a Wi-Fi PHY rate. It bounds
  the hop to the next box, not the Internet behind it, and is never taken as
  capacity.
- **up** and **down** exist already, at opposite ends: each end of a path
  estimates capacity for what it sends, so the hub knows the upload and the
  aggregator the download. The advert puts the halves together.
- **shared** is whether traffic one way takes capacity from the other (below).
- **group** names what links have in common: the same Wi-Fi network, the same
  cellular carrier in the same place, the same relay. Two phones on one
  carrier in one room share a cell; two paths through one relay share its
  uplink. Labels are hashed with a key per mesh, so the graph shows that two
  links share something without saying what.

Relays have no tunnel. A relay puts its medium and whether it is metered in
its mDNS TXT record, next to `mesh` and `name`, and a hub that uses it carries
them into its own advert.

### Up, down and duplex

For scheduling, duplex in the textbook sense matters less than two
questions: how fast is each direction, and does traffic one way take capacity
from the other?

| Medium | Each way | One budget? |
|---|---|---|
| Ethernet to a router | full duplex | no; the line behind the router decides |
| Wi-Fi | one transmitter at a time on a channel | yes: an upload and a download compete for airtime |
| LTE on paired bands (FDD) | full duplex | no |
| LTE TDD, most mid-band 5G | time slots, split mostly for downlink | no: the network fixes the split, so the uplink is just small |
| Cable (DOCSIS) | separate channels, far more down than up | no |

Even on a link that does not share a budget, a busy download hurts upload
placement today; PROTOCOL.md lists it as a known limit. A path's cost counts
half its round-trip time, and a full download queue inflates the round trip
whichever direction is asking. The fix is one-way delay, and every probe
already carries what it takes:

```
when the echoed datagram arrived, receiver's clock  =  RxClock − EchoDelay
one-way delay + clock offset                        =  RxClock − EchoDelay − EchoTS
```

The sender works this out from the probes it receives. The two clocks differ
by an unknown, slowly drifting offset; a windowed minimum of the figure (ten
seconds, as `MinRTT` uses) is that offset plus the propagation delay, and what
rises above the minimum is queueing in the sender's direction alone. The
placement cost becomes

```
cost = (min rtt / 2  +  forward queueing  +  queue) × (1 + 8 × loss)  [+ 10 ms if bonded]
```

so an upload pays for the upload queue and not for the download's. Nothing on
the wire changes.

**shared** is then measured rather than declared: on a link that shares a
budget, forward capacity falls and forward queueing rises when the reverse
direction gets busy; on one that does not, they stay put. A link known to be
shared lets the scheduler expect upload capacity to drop when a big download
starts, instead of finding out from loss. Wi-Fi starts as `unknown`, not
`yes`, because many Wi-Fi links are limited by the line behind them rather
than by the air.

### Declared limits

The README promises there are no speeds to enter, and that stays true. A user
may set caps — `bond path en7 --up 2M --down 10M`, or mark a link metered — to
protect a data plan or leave room for other people on a shared line. A cap
only ever lowers a measured figure.

### Sharing the graph

While a mesh has one aggregator, adverts go to it inside the tunnel: each hub
posts its advert to the aggregator's tunnel address when it changes and every
30 seconds, and an advert expires after 90. WireGuard already ties a tunnel
address to a key, so the aggregator knows who sent each advert and a node can
describe only itself; nothing needs signing. The aggregator adds its own view
of every path and of each rendezvous it is registered with, and serves the
merged graph at `/v1/topology`: to its dashboard in full, and to each hub only
what it uses — its own links, the exits and the rendezvous points.

With several aggregators, adverts have to travel between them. Then they need
signatures — an Ed25519 key per node, registered at join, since WireGuard's
keys cannot sign — and flooding with sequence numbers, as OSPF does. That
waits until there is more than one aggregator to choose from.

### What uses it

- **Starting estimates.** A new path starts at the capacity last measured on
  that link instead of the 2 Mbit/s every path assumes now. That matters after
  a restart, after an uplink comes back on a new socket, and for a second hub
  using a relay the first has already measured.
- **Placement**, with forward queueing as above.
- **Realtime copies** on links in different shared-risk groups, so that both
  copies do not die in the same cell.
- **Punching**, from the `nat` fields.
- **Choosing an exit** once a mesh has several aggregators (on the README's
  list of things not built yet), by the scheduler's own cost or by the user's
  choice.
- **The dashboard**, which can draw the mesh as a graph.

### Trusting it

An advert is a claim. A relay could claim a fast link to attract traffic and
then drop it. The scheduler never prefers a claim to a measurement: claims set
starting points and break ties, and once data has crossed a path for a round
trip, its own measurements govern, as they do now. Declared caps only lower
figures.

## Order of work

1. **Rendezvous and hidden aggregator** — one rendezvous, no punching, no
   change to hubs or apps. Done when an aggregator with no inbound port (the
   home test machine, plugged in beside the router) serves a hub on cellular
   through a rendezvous on a VPS, keeps going when its home address changes,
   and `bond status` shows which paths are relayed.

   **Built.** Tested over loopback, with the aggregator's link going through a
   simulated NAT that lets in only what the aggregator asked for: joining
   through the rendezvous, a 2 MB download over two paths, the NAT moving the
   link to a new port mid-run, WebSocket and relay paths through the
   rendezvous, and the rendezvous restarting. The real binaries have run on
   one Mac: a rendezvous, an aggregator registered with it and a hub fetching
   through both, with `bond status` marking the hub's path as coming through
   the rendezvous. **Not yet done:** the real case above, on the home test
   machine and a VPS.
2. **One-way delay** in the placement cost. Small, independent, and it closes
   a known limit. Done when, in simulation, uploads on a path whose download
   is saturated are placed as if the download were not there.
3. **Punching**, with NAT classification. Done when a hub on one home network
   reaches a hidden aggregator on another directly, and a hub behind a NAT
   that maps per destination does not try.
4. **Adverts and the graph** — `/v1/topology`, starting estimates, shared-risk
   groups for realtime copies, the graph on the dashboard.
5. **Several aggregators** — choosing an exit, signed adverts, flooding
   between aggregators.

## Open questions

- **Pairing or joining.** A rendezvous could join the mesh with an ordinary
  join code, as a relay does. Pairing lets one rendezvous serve meshes run by
  different people; joining means one fewer kind of code. This design pairs.
- **More than one rendezvous**, for redundancy, needs hubs that open paths to
  several addresses: the same change several aggregators need.
- **A hidden aggregator with two uplinks** could keep a link open over each,
  but then the rendezvous chooses which one each datagram crosses, which is
  scheduling in the middle. Registering each uplink as its own rendezvous
  address, with its own paths from the hubs, may be better. Unsettled.
- **What the graph reveals.** Adverts carry public addresses and, hashed,
  which networks devices share. Whoever runs the aggregator sees all of it;
  whether hubs should see anything of each other's is open.
