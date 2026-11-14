![BOND](.github/banner.png)

# Bond — bonded internet

Bond combines several Internet connections into one. A venue's flaky WiFi, a
wired line, three people's phones: every uplink you can reach carries part of
the traffic, and when one of them dies the rest keep going without the
connection dropping. It is built for the case where the connection has to
hold — a live stream from somewhere with bad connectivity — and it is
self-hosted end to end.

```
                 LAN / hotspot / USB            cellular
  ┌──────────┐  ───────────────────▶ [phone: Share mode] ───────────────┐
  │   HUB    │  ───────────────────▶ [phone: Share mode] ───────────────┤   paths
  │ laptop / │  ── tethered phone (appears as an interface) ────────────┤
  │ Pi /     │  ── ethernet or WiFi (the "primary pipe") ───────────────┼──▶ ┌────────────┐
  │ router   │  ── WebSocket fallback where UDP is blocked ─────────────┘    │ AGGREGATOR │──▶ Internet
  └────┬─────┘                                                               │  any VPS   │
       │ optional: share with the LAN                                        └────────────┘
  [encoder, cameras, consoles]
```

- The **aggregator** runs on a machine with a public address (AWS, any VPS). It
  is where your traffic rejoins the Internet.
- A **hub** is the machine you want better Internet on. It opens a *path*
  through every uplink it has, and sends each packet down whichever path gets
  it there soonest.
- A **relay** lends its uplink to hubs nearby. A phone in Share mode forwards a
  hub's (already encrypted) packets over its cellular connection.

Inside is WireGuard — real, unmodified `wireguard-go` — with Bond plugged in
underneath as its transport. Bond splits WireGuard's datagrams across paths and
puts them back in order on the other side. Because the splitting happens below
the tunnel, everything that runs over IP works: TCP, UDP, ICMP, SRT, RTMP,
WebRTC, games, other VPNs.

## Quick start

On a Mac, or on Linux with Homebrew:

```bash
brew install cartercole/tap/bond
```

Each release also has archives for Linux on x86, ARM64 and 32-bit ARM (a
Raspberry Pi). To build it yourself you need Go 1.27+: `make build` puts
`bondd` and `bond` in `bin/`.

**1. Aggregator** — on a Linux server with UDP and TCP port 7777 open:

```bash
deploy/push.sh user@your-server
```

That cross-compiles, installs a systemd service and prints a **join code** and
QR. (Or by hand on the server: `sudo bondd server --public <its address>`. On
AWS, Google Cloud or Azure, the Terraform in `deploy/aws`, `deploy/gcp` or
`deploy/azure` creates the VM first. `deploy/docker/` runs it as an
unprivileged container.)

Everything the hubs send to the Internet and receive from it passes through
the aggregator, and the big clouds bill traffic out at roughly $0.08–0.12 per
GB: a 6 Mbit/s stream is about 2.7 GB an hour. Providers that include a few TB
of traffic with a small VM (Lightsail, Hetzner, DigitalOcean) cost far less
for heavy use, and `push.sh` works on them the same way.

At home or in an office, where the server sits behind a router, the
aggregator asks the router with UPnP to forward port 7777 to it, keeps the
forward renewed and removes it when it stops. `bond status` says whether that
worked. If the router has UPnP off, switch it on, or forward UDP and TCP 7777
to the server yourself (`--no-upnp` stops the aggregator asking).

**2. Hub** — on the machine to speed up:

```bash
sudo bondd hub --join 'bond://…the code…'
```

All of the machine's traffic now goes through the bond. Without root, add
`--userspace`: nothing is captured, and you point applications at the SOCKS5
proxy it opens on `127.0.0.1:1080` instead.

The tunnel carries IPv4. So that IPv6 does not quietly carry half your traffic
around the bond, a hub that captures everything turns IPv6 to the Internet off
while it runs (connections are refused at once and applications use IPv4;
the local network is unaffected, and it is undone when the hub stops).
`--allow-ipv6` leaves it alone.

**3. Add uplinks.** Nothing to configure:

- Plug in a phone and turn on USB tethering or its hotspot. It shows up as a
  network interface and becomes a path within a couple of seconds.
- Run `bondd relay --join <code>` on anything on the same LAN that has its own
  connection, or turn on **Share my connection** in the phone app. Hubs find
  relays on the LAN by themselves, as long as the network lets its devices
  reach each other (see [Lending several phones](#lending-several-phones)).

**4. Watch it.**

```bash
bond status
```

```
hub studio-mac  10.207.0.2  connected to 203.0.113.5:7777  (kernel mode)
up 8.8 Mbit/s   down 41.3 Mbit/s   reorder window 22 ms

PATH           KIND    POLICY   STATE  RTT    LOSS  SEND CAP    UP          DOWN
en0            direct  primary  up     18 ms  0.0%  6.5 Mbit/s  5.9 Mbit/s  28.1 Mbit/s
en7            direct  bonded   up     54 ms  0.2%  3.5 Mbit/s  2.9 Mbit/s  11.0 Mbit/s
relay:Pixel-9  relay   bonded   up     61 ms  0.4%  2.8 Mbit/s  0 bit/s     2.2 Mbit/s
```

The same thing, live, is a dashboard at <http://127.0.0.1:7780> on any machine
running `bondd`. On the aggregator it shows every device. To change anything
there, or to see the join QR, open the address `bond dashboard` prints
(`sudo bond dashboard` when bondd runs as a service): it carries the daemon's
admin token, which only the user bondd runs as can read.

**5. Keep it running.** `sudo bondd service install hub` (or `server`, `relay`, `rendezvous`)
installs a systemd unit on Linux or a launchd daemon on macOS. Pass `--join` and
it joins on the spot, so the code never ends up in a unit file (`--via` is kept
in the state directory for the same reason). A service runs as root, so it
only runs a binary root alone can change: one installed by Homebrew belongs to
you, and the service runs a copy instead. After an upgrade (`brew upgrade
bond`, or a new binary) `sudo bondd service restart hub` brings that copy up to
date and puts the new version to work. Every device in a mesh needs the same
protocol version: upgrade the aggregator, any rendezvous, hubs, relays and the
phone apps together.

## Paths and policies

Every path has a policy. Change it with `bond path <name> <policy>` or from the
dashboard; the choice is remembered.

| Policy | Meaning |
|---|---|
| `primary` | Preferred while healthy. The interface your machine was already using starts as this. |
| `bonded` | Used alongside whenever that gets a packet there sooner. Tethers and relays start as this. |
| `backup` | Idle — a few probes a second — until nothing else is healthy. For metered data you only want spent in an emergency. |
| `off` | Never used. |

There are no speeds to enter. Bond measures each path's round trip, loss and
how much it actually delivers, and places every packet on the path where it
will arrive first. With little traffic that is just the fastest path. As a
path fills its queue grows and traffic spills onto the next, so under load
each path ends up carrying what it can.

## Priority device

```bash
bond priority add 192.168.8.50      # a device, by address (or MAC in gateway mode)
bond priority add 1935              # a port: RTMP
bond priority add udp/9000-9010     # a protocol and port range: SRT
bond priority all                   # everything this machine sends
```

Priority traffic is sent on the **two best paths at once** and is never held
back to be put in order, so a path stalling or dying costs it nothing. While
priority traffic is flowing, other traffic is shed before the paths' queues get
deep. Replies are prioritised too, without configuring the aggregator: the hub
marks priority packets (DSCP EF) and the aggregator remembers the flows.
Traffic an application already marks EF is honoured as it is.

## Sharing the bond with other devices

```bash
sudo bondd hub --gateway
```

turns the hub into a router: point other devices' default gateway at it (or
make it the DHCP gateway) and they are on the bond with no software — the
usual setup for an encoder, cameras or a console behind a Raspberry Pi. On a
Linux hub, priority can then be by LAN address or MAC. (macOS can do gateway
mode through `pf`, without per-device priority. It has not been tested.)

## When UDP is blocked

Some networks only let web traffic through. If an uplink's UDP path stays dead
for eight seconds, the hub opens a WebSocket path over that uplink instead, and
closes it again once UDP is back. `--transport ws` forces it.

That WebSocket goes to TCP port 7777, which a network that blocks UDP often
blocks as well. With `--tls :443` the aggregator also serves it over TLS on
port 443 and tells devices to use `wss://<its public address>/v1/ws`, which
looks like ordinary HTTPS:

```bash
sudo bondd server --public bond.example.com --tls :443
```

When `--public` is a DNS name pointing at the aggregator, it gets a
certificate from Let's Encrypt and renews it (which accepts their subscriber
agreement). Let's Encrypt checks the name by connecting to port 443, so that
port has to be reachable from the Internet. With an IP address, or until Let's
Encrypt succeeds, the aggregator serves a certificate it made itself;
`--tls-cert` and `--tls-key` use yours instead and pick up renewals. Hubs do
not check the certificate. Every datagram is already sealed by the bond and
by WireGuard, and not checking lets the fallback through proxies that inspect
HTTPS and re-sign it. Open TCP 443 as well as 7777. Behind a router, UPnP
forwards both.

Devices learn the URL when they join. Ones that joined earlier keep the old
URL until they join again (`bond join <code>`: they keep their identity and
address).

A Cloudflare Tunnel in front of the aggregator's TCP port does the same job
without opening 443 on it: see [deploy/cloudflare](deploy/cloudflare/README.md),
which also explains why Cloudflare can carry the fallback but not the bond
itself.

## When the aggregator has no public address

An aggregator needs a port that hubs can reach. Behind a router that will not
forward one, behind carrier NAT, or plugged into a gateway beside the router
that holds the public address, it has none. A **rendezvous** on any small
public server fixes that:

```bash
bondd rendezvous --public rv.example.com                   # on the public server: prints a pairing code
sudo bondd service install server --via 'bond-rv://…the code…'   # on the aggregator
```

The aggregator dials out to the rendezvous and keeps the connection open, so
it needs no port forward and asks for none. The join codes it prints name the
rendezvous: hubs, relays and phones send there what they would have sent to
the aggregator, and need nothing new. The rendezvous sees sealed datagrams
and the session and token ids it routes by, nothing else. Open UDP and TCP
7777 on it. On the aggregator, `bond status` says whether it is registered and
marks the paths that come through the rendezvous.

All the traffic still crosses the rendezvous, so its bandwidth bounds the
bond, and on a cloud VM its egress costs what an aggregator's there would. It
is worth it when the traffic should leave from the aggregator's own
connection: a home line, an office. Devices that joined the aggregator
directly have to join again (`bond join <code>`; they keep their identity and
address). `--via` replaces `--public` and `--tls`, and the rendezvous serves
the WebSocket fallback on plain TCP 7777 for now.
[docs/TOPOLOGY.md](docs/TOPOLOGY.md) has the design and what comes next, such
as hubs reaching the aggregator directly once both NATs allow it.

## Phones

[android/](android/) is a Kotlin app and [ios/](ios/) a Swift app. Both embed
the same Go core (`core/mobile`, built with gomobile) and offer:

- **Bond this phone** — a VPN that uses WiFi and cellular together.
- **Share my connection** — become a relay for hubs on the same network. On
  Android this is a foreground service that survives in the background,
  restarts after a reboot, and has a quick-settings tile. On iPhone the
  dependable way to lend a connection is Personal Hotspot or USB tethering,
  which needs no app at all; the in-app relay is experimental because iOS
  only keeps it alive inside a running VPN and may refuse cellular while on
  WiFi.

`make android` and `make ios` build them (the first needs an Android SDK with
an NDK, the second Xcode and XcodeGen). The Android app has been run in an
emulator; neither has been run on a real phone, and the iOS tunnel cannot
run anywhere but a real phone. Each folder's README says exactly what was and
was not tried, and what you need to set — for iOS, your developer team and
the Network Extension and App Group capabilities.

### Lending several phones

A phone in Share mode lends its cellular connection. Its WiFi is only the hop
to the hub, so it needs one WiFi network, the one the hub is on, and three
phones on the hub's WiFi lend three cellular connections. Two things get in
the way:

- **Client isolation.** Venue and guest WiFi often stops the devices on it
  from reaching each other. The hub then never sees the phone's announcement
  and cannot reach its port (UDP 7778), so the phone lends nothing and no
  `relay:` path appears in `bond status`.
- **The hub has one WiFi radio too.** A hub that joins a phone's hotspot over
  WiFi leaves the venue's WiFi, so it swaps one uplink for another instead of
  adding one.

Ways around them (none tried here with real phones):

- **USB tethering.** Each phone becomes its own interface and no WiFi is
  involved. It also charges the phone, and it is the dependable way to lend
  an iPhone's connection. A powered USB hub takes several phones.
- **A network of your own.** A travel router in repeater mode joins the
  venue's WiFi and offers its own network, where nothing is isolated. The hub
  and the phones in Share mode join that. The hub's one interface then
  carries the venue's connection, through the router, as well as a path
  through each phone.
- **More WiFi adapters on a Linux hub.** One USB WiFi adapter per phone
  hotspot: each is its own interface and so its own path, with no app on the
  phones. macOS supports few USB WiFi adapters.

## With Tailscale

Bond and Tailscale solve different problems and stack cleanly:

- **Tailscale over Bond.** Run Tailscale on the hub as usual. Its traffic
  rides the bond like everything else, so your tailnet connection survives an
  uplink dying.
- **Reach the mesh from your tailnet.** On a kernel-mode aggregator, run
  `tailscale up --advertise-routes=10.207.0.0/16` and tailnet devices can reach
  every device on the bond. (This combination has not been tried here.)

Tailscale's own transport cannot be replaced from outside, so bonding has to
be Bond's WireGuard, not Tailscale's.

## Security

- Traffic between hub and aggregator is WireGuard. Relays and anything else
  on a path see ciphertext only.
- A **join code** is a secret: its token never crosses the network, it keys
  the encryption of the join exchange. Holding it lets you join; without it
  you can neither join nor impersonate the aggregator, and a reply recorded
  on its way to one device is no use to another. `bond code --once
  --ttl 10m` mints a limited one; `bond codes` lists them and `bond code
  revoke <id>` stops one working.
- `bond kick <id>` removes a device. It could join again with a code it still
  holds: `bond code --rotate` replaces the standing one.
- Path metadata is authenticated per session and per run of each end, so
  nobody else can redirect a path, traffic recorded before a restart cannot
  be replayed after it, and a path only counts as up after a round trip that
  replayed traffic cannot fake.
- The aggregator refuses to forward to its own loopback and link-local space
  (the cloud metadata service).
- A relay forwards only to its own mesh's aggregator, only for local
  addresses, only what looks like a hub's datagram.
- A rendezvous forwards only datagrams for sessions an aggregator registered,
  only to that aggregator, and replies only to sockets that have sent to it in
  the last minute. Registering takes its pairing code, which, like a join
  code, never crosses the network.
- The status API listens on loopback and rejects requests a web page could
  forge. Codes, and every change, need the daemon's admin token, which only
  the user bondd runs as can read; nor are codes written to logs.
- Anyone can open TCP connections to an aggregator or rendezvous: they are
  capped, per address and in all, and idle ones are closed. A rendezvous
  keeps the hub sockets its aggregators have confirmed when strangers' would
  crowd them out.
- A service never runs a binary that anyone but root could replace.
- Behind a router, the aggregator asks it (UPnP) to forward its port (and the
  TLS port, with `--tls`) and nothing else, and removes the forwards when it
  stops. It ignores anything on
  the LAN that claims to be the router but points somewhere else.

[docs/PROTOCOL.md](docs/PROTOCOL.md) has the details and the known limits.

## What has been tested

Measured, not estimated:

| | Result |
|---|---|
| Linux kernel mode, two interfaces shaped to 6 + 3 Mbit/s, upload to the Internet | 5.20 and 2.62 Mbit/s alone, **7.83 bonded** |
| Same setup, faster interface cut 3 s into an upload | upload completes; interface rejoins 2 s after it returns |
| Linux kernel mode, whole container routed through the bond | HTTPS, ICMP ping and raw UDP all work |
| Same, with IPv6 enabled on the machine | IPv6 to the Internet refused instantly while bonded, dual-stack sites load over IPv4 in 0.2 s, routes restored on exit |
| Simulated 5 + 3 + 4 Mbit/s paths with 10–40 ms delays, TCP | 4.5 Mbit/s on the best path alone, **8.3 bonded**, in order |
| 25 % loss on both of two paths | priority traffic 92 % delivered, ordinary 76 % |
| macOS hub (no root) to a local aggregator, download from the Internet | 71 Mbit/s through the bond against 80 direct |
| Path becomes usable after appearing | one round trip |
| An uplink disappears and comes back (same path id, new socket) | usable again in about 10 ms |
| Android app in an emulator: join, VPN up, traffic through the tunnel, both modes at once | works; WiFi off/on restores the path in ~10 ms |
| Android app as a relay: a desktop hub with no other uplink downloads through the phone | works; nothing leaks over WiFi while mobile data is off |
| iOS app in the simulator: join against a live aggregator; its relay advertisement found by a desktop hub | works (the tunnel itself cannot run in a simulator) |
| Aggregator restarted under a running hub | traffic resumes in about 1 s |
| `deploy/push.sh` to an Ubuntu 24.04 server on a home LAN, then again to upgrade | installs the service, runs in kernel mode, prints the join code; the second run restarts it on the new binary |
| Aggregator behind a simulated NAT, registered with a rendezvous, a hub on two paths, all over loopback | joins through the rendezvous; 2 MB download; carries on when the NAT moves the link to a new port and after the rendezvous restarts; WebSocket and relay paths work through it |
| The real binaries on one Mac: `bondd rendezvous`, `bondd server --via`, a relay, and a hub in `--userspace` mode with two paths, its own uplink and the relay | 99 MB downloads from the Internet through rendezvous and aggregator; kill the relay mid-download and its path is marked down within a second and the download completes; `bond status` and the dashboard show each role |
| Tagging v0.1.0: the release workflow, then `brew install cartercole/tap/bond` on macOS (Apple Silicon) | tests pass on GitHub, archives published, tap updated; installs `bondd` and `bond`; `brew test` starts an aggregator and gets its join code; `brew audit --strict --online` passes |

`make test` runs the suite (about a minute): unit tests, the engine against
simulated impaired links, the whole stack in one process, and real nodes over
loopback sockets.

**Not tested**, because it needs hardware or accounts this was not built with:

- Either phone app's tunnel on a real phone. The iOS app has joined an
  aggregator from a real iPhone, but in a build without its tunnel extension
  (that needs a paid developer team), so finding the tunnel device, using
  cellular and WiFi together from the extension, and the in-app relay are
  unproven.
- A hub behind real carrier NAT with real phones as relays, and real
  simultaneous WiFi + cellular on a phone.
- macOS kernel mode (`sudo bondd hub` without `--userspace`) and macOS
  gateway mode: they need root. The Linux equivalents are tested.
- The AWS, Google Cloud and Azure Terraform (each passes `validate`; none has
  been applied), `push.sh` to a cloud VM, the Docker image and the Cloudflare
  Tunnel setup.
- The Homebrew formula on an Intel Mac or on Linux. `bondd service install`
  and `restart` from a Homebrew install need root, so only the path they
  write into the unit is tested.
- UPnP port forwarding on a real router. It is tested against a simulated
  one; a real one (a Google OnHub) has only been asked who it is.
- A rendezvous on a real public server with the aggregator behind a real NAT.

## Not built yet

- Port-forwarding from the aggregator's public address to a device on the
  bond (in kernel mode a plain `iptables` DNAT rule to the device's `10.207.x.x`
  address does it today).
- IPv6 *inside* the tunnel. Paths themselves can be IPv4 or IPv6, but the
  traffic carried is IPv4, and IPv6 is switched off while bonded (see above).
- Forward error correction for priority traffic (it is duplicated instead).
- Several aggregators with automatic choice between them.
- Hubs reaching an aggregator directly once a rendezvous has introduced them
  (hole punching), more than one rendezvous, and TLS on the rendezvous: see
  [docs/TOPOLOGY.md](docs/TOPOLOGY.md).
- Leaving a mesh from the device: "Leave" in the apps forgets the mesh
  locally; remove the device on the aggregator with `bond kick <id>`.
- Windows.

## Layout

```
cmd/bondd        the daemon: server | hub | relay | rendezvous | service
cmd/bond         the command-line client
core/wire        the bond header and its authentication tag
core/sched       per-path measurements and the placement decision
core/reorder     putting striped datagrams back in order
core/bond        the multipath engine; WireGuard's transport
core/tunnel      WireGuard, kernel TUN setup, the userspace TCP/IP stack
core/classify    priority rules and flow learning
core/control     join codes and the aggregator's registry
core/path        UDP and WebSocket transports, pinning sockets to interfaces
core/relay       the relay forwarder
core/rendezvous  meeting point for aggregators with no public address
core/discovery   finding relays on the LAN
core/gateway     firewall rules for the aggregator and for router mode
core/portmap     asking the router in front of the aggregator to forward its port
core/node        aggregator, hub and relay assembled from the above
core/mobile      the same core as the phone apps call it
core/sim         impaired links for tests
web/             the dashboard
site/            the project page at bond.cartercole.net
android/ ios/    the phone apps
deploy/          AWS, Google Cloud, Azure, Docker, Cloudflare, the Homebrew formula,
                 and push.sh for any server
```

## License

Copyright © 2026 Carter Cole. Bond is free software under the
[GNU Affero General Public License, version 3 only](LICENSE)
(`AGPL-3.0-only`). You can use, change and share it; if you run a changed
version as a service for others (an aggregator, say), you must offer those
users your changed source.

It includes [wireguard-go](https://git.zx2c4.com/wireguard-go) (MIT) and
other permissively licensed modules: BSD, ISC, MIT and Apache-2.0. Every
release archive lists them with their licenses in `NOTICE`
(`deploy/notices.sh` writes it). WireGuard is a registered trademark of
Jason A. Donenfeld.
