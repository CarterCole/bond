import Foundation

/// What the tunnel last told the app: the core's status documents, decoded.
struct TunnelReport: Equatable {
    var hub: HubStatus?
    var relay: RelayStatus?
    var share: ShareState

    init(_ response: TunnelResponse) throws {
        hub = try response.status.map { try JSONDecoder.core.decode(HubStatus.self, from: Data($0.utf8)) }
        relay = try response.relay.map { try JSONDecoder.core.decode(RelayStatus.self, from: Data($0.utf8)) }
        share = response.share
    }
}

/// The parts of the core's hub status document the app shows. Lists the core
/// may leave out or send as null are optional.
struct HubStatus: Decodable, Equatable {
    struct Priority: Decodable, Equatable {
        var all: Bool?
    }

    struct Session: Decodable, Equatable {
        var reorderHoldMs: Double?
        var paths: [PathStatus]?
    }

    var name: String
    var address: String
    var aggregator: String
    /// WireGuard has a session with the aggregator.
    var connected: Bool
    var uptimeSec: Int
    var realtimePackets: UInt64
    var priority: Priority?
    var session: Session

    var paths: [PathStatus] { session.paths ?? [] }
    var isPriorityDevice: Bool { priority?.all ?? false }
}

struct PathStatus: Decodable, Equatable, Identifiable {
    /// "wifi", "cellular", "ws:wifi", "relay:…": also what policies are set by.
    var name: String
    /// "direct", "ws" (WebSocket fallback) or "relay".
    var kind: String
    var policy: String
    var up: Bool
    var rttMs: Double
    /// A fraction: 0.02 is 2 %.
    var loss: Double
    var txBps: Double
    var rxBps: Double
    var txBytes: UInt64
    var rxBytes: UInt64

    var id: String { name }
}

/// How a path may be used; the raw values are the core's.
enum PathPolicy: String, CaseIterable, Identifiable {
    case primary, bonded, backup, off

    var id: Self { self }

    var title: String {
        switch self {
        case .primary: "Primary"
        case .bonded: "Bonded"
        case .backup: "Backup"
        case .off: "Off"
        }
    }
}

struct RelayStatus: Decodable, Equatable {
    struct Counters: Decodable, Equatable {
        /// Hubs currently sending through this phone.
        var clients: Int
        var upBytes: UInt64
        var downBytes: UInt64
        var port: Int
    }

    var uptimeSec: Int
    var relay: Counters
}
