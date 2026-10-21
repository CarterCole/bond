import Foundation
import NetworkExtension
import Observation
import os

/// The VPN configuration and the running tunnel, as the app sees them.
@MainActor @Observable
final class TunnelController {
    enum Failure: LocalizedError {
        case notConnected
        case noReply

        var errorDescription: String? {
            switch self {
            case .notConnected: "Bond is not connected."
            case .noReply: "The tunnel did not answer."
            }
        }
    }

    /// `.invalid` until a VPN configuration exists.
    private(set) var status: NEVPNStatus = .invalid
    /// The tunnel's latest status; nil while it is not up.
    private(set) var report: TunnelReport?
    /// The last thing that went wrong, in the words of whatever reported it.
    private(set) var problem: String?

    /// Set from the tap on the switch until iOS reports a status of its own;
    /// saving the configuration (and, the first time, the user allowing it)
    /// comes in between.
    private var isStartRequested = false

    @ObservationIgnored private var manager: NETunnelProviderManager?
    @ObservationIgnored private var observer: (any NSObjectProtocol)?
    @ObservationIgnored private var isStarting = false
    private let log = Logger(subsystem: BondID.app, category: "tunnel")

    /// The switch position: on from the moment the tunnel is asked to start.
    var isOn: Bool { isStartRequested || status == .connecting || isUp }

    /// The extension is running and will answer messages.
    var isUp: Bool { status == .connected || status == .reasserting }

    /// Finds the VPN configuration a previous run saved, if there is one.
    func load() async {
        do {
            let managers = try await NETunnelProviderManager.loadAllFromPreferences()
            adopt(managers.first {
                ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == BondID.tunnelBundle
            })
        } catch {
            // Nothing the user can act on yet: switching Bond on saves a
            // configuration afresh and reports what goes wrong then.
            log.error("loading the VPN configuration: \(error.localizedDescription, privacy: .public)")
        }
    }

    func connect(to device: TunnelDescription) async {
        problem = nil
        SharedSettings.lastStartError = nil
        isStartRequested = true
        do {
            let manager = try await savedManager(for: device)
            try manager.connection.startVPNTunnel()
        } catch {
            isStartRequested = false
            #if targetEnvironment(simulator)
            problem = "The iOS Simulator cannot run VPN tunnels; try this on an iPhone. (iOS said: \(error.localizedDescription))"
            #else
            problem = error.localizedDescription
            #endif
        }
    }

    func disconnect() {
        manager?.connection.stopVPNTunnel()
    }

    /// Stops the tunnel and deletes the VPN configuration from Settings.
    func remove() async throws {
        guard let manager else { return }
        manager.connection.stopVPNTunnel()
        try await manager.removeFromPreferences()
        adopt(nil)
        problem = nil
    }

    /// Keeps `report` fresh for as long as the tunnel is up, then returns.
    /// Run it again whenever `status` changes.
    func monitor() async {
        while isUp, !Task.isCancelled {
            do {
                try await exchange(.status)
            } catch {
                log.error("status: \(error.localizedDescription, privacy: .public)")
            }
            try? await Task.sleep(for: .seconds(1))
        }
    }

    /// Asks the running tunnel to change something.
    func perform(_ request: TunnelRequest) async {
        do {
            problem = try await exchange(request).error
        } catch {
            problem = error.localizedDescription
        }
    }

    // MARK: Configuration

    private func savedManager(for device: TunnelDescription) async throws -> NETunnelProviderManager {
        let manager = manager ?? NETunnelProviderManager()
        let tunnel = NETunnelProviderProtocol()
        tunnel.providerBundleIdentifier = BondID.tunnelBundle
        tunnel.serverAddress = device.aggregator
        manager.protocolConfiguration = tunnel
        manager.localizedDescription = "Bond"
        // Also what makes this the active VPN if another app's was.
        manager.isEnabled = true
        try await manager.saveToPreferences()
        // A configuration that has just been saved refuses to start until it
        // has been read back.
        try await manager.loadFromPreferences()
        adopt(manager)
        return manager
    }

    private func adopt(_ manager: NETunnelProviderManager?) {
        if let observer { NotificationCenter.default.removeObserver(observer) }
        observer = nil
        self.manager = manager
        report = nil
        status = manager?.connection.status ?? .invalid
        guard let manager else { return }
        observer = NotificationCenter.default.addObserver(
            forName: .NEVPNStatusDidChange, object: manager.connection, queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated { self?.statusChanged() }
        }
    }

    private func statusChanged() {
        guard let connection = manager?.connection else { return }
        status = connection.status
        isStartRequested = false
        switch status {
        case .connecting:
            isStarting = true
        case .connected:
            isStarting = false
        case .disconnected, .invalid:
            report = nil
            if isStarting { explainFailedStart(of: connection) }
            isStarting = false
        default:
            break
        }
    }

    /// A tunnel that fails to start just goes back to "disconnected". The
    /// extension leaves its reason in the shared settings; if it never ran,
    /// iOS may know why.
    private func explainFailedStart(of connection: NEVPNConnection) {
        if let reason = SharedSettings.lastStartError {
            problem = reason
            return
        }
        connection.fetchLastDisconnectError { [weak self] error in
            let reason = error?.localizedDescription
            Task { @MainActor in
                if let reason { self?.problem = reason }
            }
        }
    }

    // MARK: Messages

    @discardableResult
    private func exchange(_ request: TunnelRequest) async throws -> TunnelResponse {
        guard isUp, let session = manager?.connection as? NETunnelProviderSession else {
            throw Failure.notConnected
        }
        let message = try JSONEncoder().encode(request)
        let reply: Data? = try await withCheckedThrowingContinuation { continuation in
            do {
                try session.sendProviderMessage(message) { continuation.resume(returning: $0) }
            } catch {
                continuation.resume(throwing: error)
            }
        }
        guard let reply else { throw Failure.noReply }
        let response = try JSONDecoder().decode(TunnelResponse.self, from: reply)
        // The tunnel may have gone down while the answer was on its way.
        if isUp { report = try TunnelReport(response) }
        return response
    }
}
