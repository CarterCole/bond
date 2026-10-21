import Bond
import Foundation

/// The parts of the core only the tunnel extension runs.
extension BondCore {
    struct NothingReturned: LocalizedError {
        var errorDescription: String? { "The Bond core returned neither a result nor an error." }
    }

    /// Looks the aggregator's name up and returns the config with the answer
    /// recorded — the very same config if nothing moved or the lookup failed.
    /// Blocks for up to a few seconds.
    static func refresh(_ config: String) -> String {
        MobileRefresh(config)
    }

    /// Starts bonding. The core takes ownership of `tunFD` and closes it when
    /// the client stops, or if starting fails.
    static func startClient(
        config: String, tunFD: Int32, networks: String, platform: MobilePlatformProtocol
    ) throws -> MobileClient {
        var error: NSError?
        guard let client = MobileStartClient(config, tunFD, networks, platform, &error) else {
            throw error ?? NothingReturned()
        }
        return client
    }

    /// Starts forwarding for hubs on the LAN, out through `interface`, on the
    /// core's default relay port.
    static func startRelay(
        config: String, network: String, interface: String, platform: MobilePlatformProtocol
    ) throws -> MobileRelay {
        var error: NSError?
        guard let relay = MobileStartRelay(config, network, interface, 0, platform, &error) else {
            throw error ?? NothingReturned()
        }
        return relay
    }
}
