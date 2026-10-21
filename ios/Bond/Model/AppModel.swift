import Foundation
import Observation

/// What the app knows: whether this phone has joined a mesh, and its tunnel.
@MainActor @Observable
final class AppModel {
    /// This phone's place in the mesh; nil until it has joined one.
    private(set) var device: TunnelDescription?
    /// Why the stored config could not be used, if there is one and it could not.
    private(set) var storageProblem: String?
    /// The experimental "share over Wi-Fi" switch.
    private(set) var shareEnabled = SharedSettings.shareEnabled

    let tunnel = TunnelController()

    init() {
        do {
            device = try ConfigStore.load().map(BondCore.describe)
        } catch {
            storageProblem = error.localizedDescription
        }
    }

    /// Joins the mesh a join code is for. Throws the core's own error.
    func join(code: String, deviceName: String) async throws {
        let config = try await blocking { try BondCore.join(code: code, deviceName: deviceName) }
        let device = try BondCore.describe(config)
        try ConfigStore.save(config)
        storageProblem = nil
        self.device = device
    }

    /// Removes the VPN configuration and forgets the mesh and its keys.
    func leave() async throws {
        try await tunnel.remove()
        try ConfigStore.delete()
        SharedSettings.reset()
        shareEnabled = false
        device = nil
    }

    func setShare(_ enabled: Bool) async {
        shareEnabled = enabled
        // Stored for the next time the tunnel starts; told to the tunnel now
        // if it is running.
        SharedSettings.shareEnabled = enabled
        if enabled { LocalNetworkPrompt.raise() }
        if tunnel.isUp { await tunnel.perform(.setShare(enabled)) }
    }
}
