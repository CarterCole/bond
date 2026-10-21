import Foundation
import Network
import os

/// Knows which uplinks exist right now.
///
/// There is one monitor per kind of interface, because a single unconstrained
/// monitor only describes the path iOS currently prefers — Wi-Fi, when there
/// is Wi-Fi — and the point of bonding is to use the others as well.
///
/// `start` and `stop` belong to the owner (the engine, on its queue); the
/// rest may be called from anywhere.
final class NetworkWatcher: @unchecked Sendable {
    private struct Uplink {
        let name: String
        let type: NWInterface.InterfaceType
        let policy: String
    }

    private struct Network: Encodable {
        let name: String
        let iface: String
        let policy: String
    }

    /// In the order the core should see them. `policy` is only the default,
    /// used until the user chooses one for that path.
    private static let uplinks = [
        Uplink(name: "wifi", type: .wifi, policy: "primary"),
        Uplink(name: "cellular", type: .cellular, policy: "bonded"),
        Uplink(name: "ethernet", type: .wiredEthernet, policy: "bonded"),
    ]

    private let queue = DispatchQueue(label: "com.cartertech.bond.tunnel.networks")
    private var monitors: [NWPathMonitor] = []
    /// Uplink name → interface name ("en0", "pdp_ip0") for the uplinks that exist.
    private let interfaces = OSAllocatedUnfairLock(initialState: [String: String]())

    /// Starts watching. `onChange` is called, on a private queue, whenever an
    /// uplink appears, disappears or moves to another interface.
    func start(onChange: @escaping @Sendable () -> Void) {
        guard monitors.isEmpty else { return }
        let interfaces = interfaces
        monitors = Self.uplinks.map { uplink in
            let monitor = NWPathMonitor(requiredInterfaceType: uplink.type)
            monitor.pathUpdateHandler = { path in
                let interface = path.status == .satisfied
                    ? path.availableInterfaces.first { $0.type == uplink.type }?.name
                    : nil
                let changed = interfaces.withLock { known in
                    guard known[uplink.name] != interface else { return false }
                    known[uplink.name] = interface
                    return true
                }
                if changed { onChange() }
            }
            monitor.start(queue: queue)
            return monitor
        }
    }

    func stop() {
        monitors.forEach { $0.cancel() }
        monitors = []
        interfaces.withLock { $0 = [:] }
    }

    /// The interface an uplink is on, if the uplink exists at the moment.
    func interface(of uplink: String) -> String? {
        interfaces.withLock { $0[uplink] }
    }

    /// The uplinks that exist, as the core's networks list:
    /// `[{"name":"wifi","iface":"en0","policy":"primary"}, …]`.
    var networksJSON: String {
        let known = interfaces.withLock { $0 }
        let networks = Self.uplinks.compactMap { uplink in
            known[uplink.name].map { Network(name: uplink.name, iface: $0, policy: uplink.policy) }
        }
        let data = (try? JSONEncoder().encode(networks)) ?? Data("[]".utf8)
        return String(decoding: data, as: UTF8.self)
    }
}
