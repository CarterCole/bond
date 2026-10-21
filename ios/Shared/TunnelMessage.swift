import Foundation

/// What the app asks of the running tunnel (`sendProviderMessage`), as JSON.
struct TunnelRequest: Codable, Sendable {
    enum Command: String, Codable, Sendable {
        case status, setPolicy, setPriority, setShare
    }

    var command: Command
    var path: String?
    var policy: String?
    var enabled: Bool?

    static let status = TunnelRequest(command: .status)

    static func setPolicy(path: String, policy: String) -> TunnelRequest {
        TunnelRequest(command: .setPolicy, path: path, policy: policy)
    }

    static func setPriority(_ all: Bool) -> TunnelRequest {
        TunnelRequest(command: .setPriority, enabled: all)
    }

    static func setShare(_ enabled: Bool) -> TunnelRequest {
        TunnelRequest(command: .setShare, enabled: enabled)
    }
}

/// The tunnel's answer to every request: whether it worked, and how things
/// stand now.
struct TunnelResponse: Codable, Sendable {
    /// Why the request failed, in the core's words; nil if it worked.
    var error: String?
    /// The core's hub status document, still as JSON: the extension passes it
    /// through rather than spend its small memory budget decoding it.
    var status: String?
    /// The core's relay status document, as JSON, while the relay is running.
    var relay: String?
    var share: ShareState
}

/// The state of the experimental Wi-Fi relay.
struct ShareState: Codable, Equatable, Sendable {
    var enabled = false
    var running = false
    /// The Bonjour name hubs see, once the announcement is up.
    var advertisedAs: String?
    /// Why sharing is not working, if it is switched on and is not.
    var problem: String?
}
