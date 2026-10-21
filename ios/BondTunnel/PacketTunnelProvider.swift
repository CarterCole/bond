import Foundation
import NetworkExtension

/// The VPN: iOS starts this extension when Bond is switched on and keeps it
/// alive while the tunnel is up. Everything else lives in `TunnelEngine`.
final class PacketTunnelProvider: NEPacketTunnelProvider {
    enum StartFailure: LocalizedError {
        case notJoined
        case noTunnelDevice

        var errorDescription: String? {
            switch self {
            case .notJoined: "This phone has not joined a mesh. Open Bond to join one."
            case .noTunnelDevice: "Could not find the tunnel device iOS created."
            }
        }
    }

    private let engine = TunnelEngine()

    override func startTunnel(options: [String: NSObject]? = nil) async throws {
        do {
            try await start()
            SharedSettings.lastStartError = nil
            Log.tunnel.info("tunnel started")
        } catch {
            let reason = error.localizedDescription
            Log.tunnel.error("tunnel failed to start: \(reason, privacy: .public)")
            // Left where the app can read it, and also returned in a form
            // whose message survives the trip to another process.
            SharedSettings.lastStartError = reason
            await engine.stop()
            throw NSError(
                domain: BondID.tunnelBundle, code: 1, userInfo: [NSLocalizedDescriptionKey: reason])
        }
    }

    private func start() async throws {
        await engine.watchNetworks()
        guard let stored = try ConfigStore.load() else { throw StartFailure.notJoined }
        // While name lookups are still the system's own.
        let config = await engine.refresh(stored)
        let mesh = try BondCore.describe(config)
        // The utun device is only certain to be configured, and so worth
        // handing to the core, once the settings have been applied.
        try await setTunnelNetworkSettings(TunnelSettings.make(for: mesh))
        guard let utun = TunnelFileDescriptor.find() else { throw StartFailure.noTunnelDevice }
        try await engine.start(
            config: config, mesh: mesh, tunnelFD: utun, share: SharedSettings.shareEnabled)
    }

    override func stopTunnel(with reason: NEProviderStopReason) async {
        Log.tunnel.info("tunnel stopping (reason \(reason.rawValue))")
        await engine.stop()
    }

    override func handleAppMessage(_ messageData: Data) async -> Data? {
        await engine.respond(to: messageData)
    }
}
