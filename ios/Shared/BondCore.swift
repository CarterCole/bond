import Bond
import Foundation

/// The Go core (Bond.xcframework, Go package `mobile`) with Swift manners:
/// errors are thrown and structured results are decoded.
enum BondCore {
    static var version: String { MobileVersion() }

    /// The DNS-SD service type relays are found by: "_bond-relay._udp".
    static var relayServiceType: String { MobileRelayServiceType() }

    /// Registers this phone with the aggregator named in the join code and
    /// returns its config, which holds private keys. Blocks on the network for
    /// up to twenty seconds: keep it off the main thread.
    static func join(code: String, deviceName: String) throws -> String {
        try call { MobileJoin(code, deviceName, "phone", $0) }
    }

    static func describe(_ config: String) throws -> TunnelDescription {
        let json = try call { MobileDescribe(config, $0) }
        return try JSONDecoder.core.decode(TunnelDescription.self, from: Data(json.utf8))
    }

    private static func call(_ function: (NSErrorPointer) -> String) throws -> String {
        var error: NSError?
        let result = function(&error)
        if let error { throw error }
        return result
    }
}
