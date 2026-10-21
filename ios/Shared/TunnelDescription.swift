import Foundation

/// What the core says about a config (`MobileDescribe`): who this device is in
/// the mesh and the parameters the packet tunnel must be set up with.
struct TunnelDescription: Decodable, Equatable, Sendable {
    var name: String
    var kind: String
    var meshId: String
    var address: String
    var prefix: Int
    var gateway: String
    var aggregator: String
    var mtu: Int
    var dns: [String]?
    /// TXT entries ("key=value") a relay must advertise.
    var relayTxt: [String]?
}

extension JSONDecoder {
    /// The core writes snake_case keys.
    static var core: JSONDecoder {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return decoder
    }
}
