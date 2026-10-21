# Bond for Android

The phone end of Bond. The app can do two things for a mesh, independently and at the same time:

- **Bond this phone**: a VPN that carries the phone's traffic over Wi-Fi and mobile data together.
- **Share my connection**: a background relay. Hubs on the same Wi-Fi send their (already encrypted)
  datagrams to the phone, which forwards them to the aggregator over its mobile data. Hubs find the
  phone by DNS-SD.

All protocol work is done by the Go core (`core/mobile`), which gomobile compiles to
`app/libs/bond.aar`. The app supplies what only Android can: the VPN consent and TUN device, the list
of networks and the binding of sockets to them, staying alive in the background, and the DNS-SD
announcement.

## Build

From the repository root, which also rebuilds the Go core:

    make android

Or just the app, when `app/libs/bond.aar` is already there:

    cd android
    ./gradlew assembleDebug        # app/build/outputs/apk/debug/app-debug.apk
    ./gradlew testDebugUnitTest lintDebug

You need JDK 17 or newer and an Android SDK with `platforms;android-37.0` and `build-tools;37.0.0`.
Point Gradle at the SDK with `ANDROID_HOME` or a `local.properties` holding `sdk.dir=...` (not checked
in). Rebuilding the core (`make mobile-android`) also needs an NDK; the app itself does not.

`./gradlew assembleRelease` produces a minified but **unsigned** APK: add a signing config before
distributing one.

## Install and use

    adb install -r app/build/outputs/apk/debug/app-debug.apk

1. On the aggregator, get a join code (`bond code`, or its dashboard).
2. In the app, scan the QR code or paste the code, name the device, and join.
3. Switch on either mode, or both.

Requires Android 10 (API 29) or newer.

### What each mode needs

| | Bond this phone | Share my connection |
|---|---|---|
| Android asks for | VPN consent; notifications (optional) | notifications (optional); unrestricted battery use (optional, asked once); on Android 17, "nearby devices" (local network) access |
| Networks | Wi-Fi and/or mobile data | mobile data to share, and Wi-Fi shared with the hub |
| Runs as | `VpnService`, foreground type `systemExempted` | foreground service, type `connectedDevice` |
| Survives | process death (restarted by Android); Android's always-on VPN can start it at boot | process death, reboot and app update (restarted if it was on) |

Sharing can also be switched from a Quick Settings tile ("Share connection").

Scanning uses Google's code scanner, which runs inside Google Play services, so the app has no camera
permission. Play services downloads the scanner the first time it is needed (the manifest asks for it
at install time); until it has, and on devices without Play services, paste the code.

Sharing is meant to be left on: it comes back after a reboot, after an update, and when the app is
next opened if Android stopped it in between. It stops when switched off (in the app, from its
notification or from the tile) or when the phone leaves the mesh.

## How it is put together

    app/src/main/kotlin/com/cartertech/bond/
      core/    ConfigStore (the config, i.e. the keys), BondPlatform (the core's socket hook),
               Uplinks (the Wi-Fi and cellular networks Android is asked to keep up), JSON models
      vpn/     BondVpnService
      relay/   RelayService, RelayAdvertiser (NsdManager), BootReceiver, ShareTileService
      ui/      Compose screens, the view model, and the walk through Android's permission prompts

Things that are the way they are for a reason:

- **The config never leaves the device.** It lives in `noBackupFilesDir`; backup and device transfer
  are switched off in the manifest as well.
- **Every core call is made on one background thread** (`CoreDispatcher`), so starting, stopping,
  polling and settings changes of the client and the relay never interleave.
- **Socket binding.** The core calls `BondPlatform.bindSocket` for each socket. It is protected from
  the VPN and attached to the `Network` it was opened for (`wifi`, `cellular`, or `lan` = Wi-Fi). If
  the named network is not there the answer is no, and the core does without the socket rather than
  let it leave by the default network.
- **The aggregator's address is looked up before a tunnel exists** (`Mobile.refresh`, just before the
  client or the relay starts) and stored in the config, so the core never depends on DNS that its own
  tunnel carries.
- **The app excludes itself from its own VPN** (`addDisallowedApplication`). `protect()` only reaches
  sockets opened while the VPN service exists, and a VPN's routing rule outranks the network a socket
  is bound to: a relay that was already sharing would have its traffic pulled into the tunnel the
  moment bonding was switched on. Nothing in the app is meant to travel through the bond anyway.
- **IPv6 is blocked while bonding.** The tunnel is IPv4-only; adding no IPv6 route makes Android drop
  IPv6 rather than let it bypass the VPN.
- **Replaced networks.** When Android replaces a network (Wi-Fi roam, mobile data reconnect) the
  core's sockets stay bound to the one that is gone. The VPN service gives the core a new `gen` for
  that network, which makes it reopen the path. The relay service calls `networksChanged()` when
  mobile data is replaced or comes back, and moves the relay's hub-facing socket (which the core keeps
  for the relay's whole life) to the new Wi-Fi network itself.
- **A tunnel that disappears is noticed.** If the TUN interface goes away under the service, it stops
  and says so instead of looking connected while traffic bypasses it.
- **Android 17 local network permission** (`ACCESS_LOCAL_NETWORK`) is requested before sharing, and
  before joining or bonding when the aggregator has a LAN address. Older releases ignore it.
- **Lint's `ForegroundServicePermission` is suppressed for the VPN service**: lint only knows the
  exact-alarm route to `systemExempted`, not the VPN one.

## What has and has not been verified

Verified on the Android 17 (API 37) emulator, arm64, against a local userspace aggregator:

- Build, unit tests, lint; install; join by pasted code, including the local network prompt.
- Sharing: foreground service type accepted, DNS-SD registration with the right name, port and TXT
  records, a desktop hub forwarding real traffic through the phone (the hub was pointed at the
  emulator by address), the Quick Settings tile, restart after process death, after an update of the
  app and after a reboot.
- Bonding: VPN consent, tunnel up, traffic through it, policy and priority changes reaching the
  aggregator and persisting, the Disconnect action.
- Both at once, in either order; sockets carrying the right network's mark (checked with `ss`).
- Wi-Fi and mobile data going away and coming back as new networks: the bonded path was back up
  within about 10 ms of Android reporting the new network, and the relay forwarded again for its hub
  without being restarted. With mobile data off the relay forwards nothing, as intended.

Not verified, because an emulator cannot show it or it was not tried:

- **Anything on a real phone.** In particular, real Wi-Fi and real mobile data up at the same time,
  throughput, and how carriers treat the traffic.
- **Long background runs of sharing**: Doze, app standby and manufacturers' own battery managers
  (Samsung, Xiaomi, OnePlus and others kill background apps beyond what stock Android does), with and
  without the battery exemption.
- **Wi-Fi power save with the screen off.** The relay holds a high-performance/low-latency Wi-Fi
  lock, but from Android 14 that lock is documented to apply only in limited conditions, so the hub to
  phone hop may get slower when the screen is off.
- **Discovery across a real LAN**: that a hub actually finds the phone through `NsdManager`'s
  announcement (only the registration was checked), and behaviour on networks that block multicast.
- **Android 10 to 16.** Only API 37 was run. The foreground service types, `requestNetwork` and the
  permission prompts differ between releases.
- **Scanning a QR code** (the emulator has no usable camera), and devices without Google Play
  services.
- **Always-on VPN and "Block connections without VPN"**, VPN revocation by another VPN app, work
  profiles, and dual-SIM phones.
- **An aggregator named by hostname**, IPv6-only mobile networks (the core then needs the carrier's
  NAT64/CLAT to reach an IPv4 aggregator), captive portals, and Ethernet (only Wi-Fi and mobile data
  are bonded).
- The Quick Settings tile from a locked screen, and battery use of either mode.
