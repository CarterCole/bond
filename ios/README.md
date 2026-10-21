# Bond for iOS

The iPhone app: a SwiftUI app (`Bond`) and a packet-tunnel extension
(`BondTunnel`) around the Go core, which is built with gomobile into
`Frameworks/Bond.xcframework`.

- **Bond this phone** — a VPN that uses Wi-Fi and cellular at the same time.
- **Share my connection** — instructions for Personal Hotspot / USB (which
  needs no code), and an experimental relay that lends this phone's cellular
  connection to hubs on the same Wi-Fi while the VPN is up.

## Build

From the repository root:

    make ios

That builds the core (`make mobile-ios`), generates `ios/Bond.xcodeproj` from
`ios/project.yml` with XcodeGen, and compiles both targets for the Simulator
without code signing. To work in Xcode:

    make mobile-ios
    cd ios && xcodegen generate && open Bond.xcodeproj

The project file is generated; change `project.yml`, not the project.

Needs XcodeGen (`brew install xcodegen`); built with Xcode 27. Deployment
target iOS 17. Swift 6 language mode with `SWIFT_APPROACHABLE_CONCURRENCY`.

## In the Simulator

Run the `Bond` scheme from Xcode. Joining a mesh works, and so does everything
on screen, but **the tunnel does not**: iOS Simulators cannot run packet-tunnel
extensions, and switching Bond on says so.

The app built by `make ios` is unsigned, so it has no entitlements and cannot
use the Keychain ("A required entitlement isn't present"). It is a compile
check. Xcode's own Simulator builds are signed to run locally and work.

## On an iPhone

1. You need a team in the paid Apple Developer Program. A free personal team
   cannot use the Network Extension entitlement.
2. Create `ios/Config/Local.xcconfig` (it is ignored by git) with your team
   and an identifier prefix your team can register:

       DEVELOPMENT_TEAM = ABCDE12345
       BOND_ID_PREFIX = com.example

   Without a prefix the identifiers are `com.cartertech.*`.
3. With automatic signing, Xcode should register the two App IDs and their
   capabilities the first time you build for a device. If you manage them by
   hand, both `<prefix>.bond` and `<prefix>.bond.tunnel` need **Network
   Extensions** (packet tunnel) and **App Groups** with `group.<prefix>.bond`.
4. Run the `Bond` scheme on the phone. Join with the code your server prints,
   then switch Bond on; iOS asks once whether Bond may add a VPN configuration.

`BOND_ID_PREFIX` is the only place the identifiers are set: `project.yml`, both
`.entitlements` files and both `Info.plist`s are built from it, and
`Shared/BondID.swift` reads it back from the bundle.

On the phone, Bond needs cellular data allowed (Settings › Cellular), and the
experimental sharing needs Local Network access, which the app prompts for
when the switch is first turned on.

The extension logs to the unified log: in Console, filter on subsystem
`<prefix>.bond.tunnel`. Category `core` is the Go core's own log.
If the tunnel fails to start, the app shows the reason under the switch.

## How it fits together

    Shared/       compiled into both targets
      BondCore        the Go core's functions, throwing Swift errors
      ConfigStore     the device config (private keys) in the Keychain,
                      shared with the extension through the App Group
      TunnelMessage   the JSON the app and the extension exchange
      SharedSettings  the share switch and the last start error
    Bond/         the app
      Model/          AppModel (joined or not), TunnelController (the VPN
                      configuration, status and messages)
      Views/          the join screen and the main screen
    BondTunnel/   the extension
      PacketTunnelProvider   start, stop, messages
      TunnelEngine           owns the core's client and relay
      TunnelFileDescriptor   finds the utun socket to hand to the core
      NetworkWatcher         which of Wi-Fi, cellular, Ethernet exist
      RelayAdvertiser        Bonjour announcement of the relay

The app never talks to the core while the tunnel runs; it sends the extension
a request (`status`, `setPolicy`, `setPriority`, `setShare`) once a second or
on a tap, and every answer carries the core's status documents.

How the extension and the core share the tunnel:

- iOS creates and configures the utun device from the settings in
  `TunnelSettings` (addresses, routes, DNS, MTU). The core is handed a
  duplicate of its descriptor and owns that duplicate from then on: it closes
  it when the client stops and if starting fails. The extension never does.
- The mesh is IPv4 inside. The tunnel is still given an IPv6 address
  (`fd7a:b0ad::` plus the last two octets of its IPv4 address) and the IPv6
  default route, so IPv6 packets go into the tunnel and are dropped there
  instead of going around the bond. Apps fall back to IPv4.
- Once the tunnel is up, names are looked up through it, so the aggregator's
  own name cannot be. The config remembers where that name last resolved to
  and the core connects there. To keep it current when the aggregator has
  moved, the extension asks the core to look the name up (`MobileRefresh`)
  each time it starts, before it applies the tunnel's settings.
- The config is stored exactly as the core returns it: from `MobileJoin`, from
  `MobileRefresh`, and from `configJSON()` after a policy or priority change
  and when the tunnel stops. The extension only ever replaces an existing
  config, so it cannot bring one back after "Leave Mesh".

## What has not been verified

There was no iPhone and no signing identity when this was written. Verified:
both targets compile for the Simulator and for a device (unsigned); in the
Simulator the app joins a real aggregator, stores and deletes its config, and
shows its screens; on macOS, the pieces of the extension that do not need a
tunnel behave (finding kernel control sockets, the network watcher, the Bonjour
announcement — which a desktop hub then discovered — decoding the core's real
status documents, and the tunnel settings as built for NetworkExtension).

Since then, on an iPhone 16 Pro Max with iOS 26.6.1, a build of the app
*without* the tunnel extension, signed by a free personal team (which cannot
sign Network Extensions): it installs, its QR scanner reads a join code off a
screen, it joins a live aggregator over Wi-Fi, and its config survives in the
Keychain under the App Group on a real provisioning profile. Switching Bond on
there fails with "permission denied", as it must without the entitlement.

Not verified, and to be tried on a real phone with a paid team's build:

- The tunnel itself: that the utun descriptor is found, that the core starts
  on it, that traffic flows.
- That IPv6 routed into the tunnel and dropped there makes apps fall back to
  IPv4 quickly enough to go unnoticed, on dual-stack and on IPv6-only
  (NAT64) networks.
- That iOS lets the extension use cellular while the phone is on Wi-Fi, for
  the phone's own traffic and, separately, for the relay.
- The relay end to end: hubs reaching the phone's UDP port over Wi-Fi, and the
  Local Network permission as it applies to an extension.
- The extension's memory use against iOS's limit of about 50 MB.
- The Keychain item being readable by the extension through the App Group, and
  with the phone locked.
- Signing with the Network Extension entitlement, and the VPN permission
  prompt.
- How the app reports a tunnel that fails to start, and behaviour across
  sleep, network changes and iOS stopping the extension.

Known gaps:

- Nothing is reachable over IPv6 while Bond is on: the mesh carries IPv4 only.
- Path policies and the priority switch can only be changed while connected:
  the running core applies and stores them.
- "Leave Mesh" forgets the mesh on the phone. The aggregator still lists the
  device until it is removed there.
- The phone does not look for relays on the LAN itself.
- There is no app icon.
