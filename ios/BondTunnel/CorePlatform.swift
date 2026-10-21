import Bond
import Foundation
import os

enum Log {
    static let tunnel = Logger(subsystem: BondID.tunnelBundle, category: "tunnel")
    static let core = Logger(subsystem: BondID.tunnelBundle, category: "core")
}

/// What the core asks of the operating system.
final class CorePlatform: NSObject, MobilePlatformProtocol, Sendable {
    /// Declined on iOS: the core then pins each socket to its interface with
    /// IP_BOUND_IF, and sockets opened by a packet tunnel provider are never
    /// routed into its own tunnel, so there is nothing else to do.
    func bindSocket(_ fd: Int32, network: String?) -> Bool { false }

    func log(_ line: String?) {
        Log.core.info("\(line ?? "", privacy: .public)")
    }
}
