import Bond
import Foundation

/// Runs the core for the packet tunnel: the bonding client and, while sharing
/// is switched on, the relay.
///
/// An actor on a dispatch queue of its own rather than on Swift's shared
/// pool, because the core's calls block: starting the relay resolves a name,
/// stopping waits for goroutines to finish.
actor TunnelEngine {
    enum Failure: LocalizedError {
        case notRunning
        case badRequest

        var errorDescription: String? {
            switch self {
            case .notRunning: "Bond is not connected."
            case .badRequest: "The tunnel did not understand the request."
            }
        }
    }

    private let queue = DispatchSerialQueue(label: "com.cartertech.bond.tunnel.engine")
    nonisolated var unownedExecutor: UnownedSerialExecutor { queue.asUnownedSerialExecutor() }

    private let platform = CorePlatform()
    private let watcher = NetworkWatcher()
    private let advertiser = RelayAdvertiser()

    private var config = ""
    private var mesh: TunnelDescription?
    private var client: MobileClient?

    private var shareEnabled = false
    private var shareProblem: String?
    private var relay: MobileRelay?
    private var relayInterface: String?

    /// Starts looking for uplinks. Called before the tunnel's settings are
    /// applied, which takes long enough that the list is normally complete by
    /// the time the client starts; anything later arrives as a change.
    func watchNetworks() {
        watcher.start { [weak self] in
            Task { await self?.networksChanged() }
        }
    }

    /// Brings the config's record of where the aggregator is up to date, and
    /// stores it if it changed.
    ///
    /// This has to happen before the tunnel's settings are applied. From then
    /// on names are looked up through the tunnel, which cannot be used to find
    /// the aggregator the tunnel itself depends on; the core connects on the
    /// address remembered here.
    func refresh(_ config: String) -> String {
        let current = BondCore.refresh(config)
        guard current != config else { return config }
        do {
            try ConfigStore.update(current)
        } catch {
            Log.tunnel.error("storing the refreshed config: \(error.localizedDescription, privacy: .public)")
        }
        return current
    }

    func start(config: String, mesh: TunnelDescription, tunnelFD: Int32, share: Bool) throws {
        self.config = config
        self.mesh = mesh
        shareEnabled = share
        // The copy is the core's from the moment it is handed over, whether
        // starting succeeds or throws: the core closes it, and nothing here may.
        let descriptor = try TunnelFileDescriptor.duplicateForCore(tunnelFD)
        client = try BondCore.startClient(
            config: config, tunFD: descriptor, networks: watcher.networksJSON, platform: platform)
        reconcileRelay()
    }

    func stop() {
        watcher.stop()
        stopRelay()
        // The core also keeps what it learned while running, such as where
        // the aggregator's name last resolved to.
        try? saveConfig()
        client?.stop()
        client = nil
    }

    /// Answers a message from the app: carries out the request, then reports
    /// how things stand.
    func respond(to message: Data) -> Data? {
        var failure: String?
        do {
            try perform(JSONDecoder().decode(TunnelRequest.self, from: message))
        } catch {
            failure = error.localizedDescription
        }
        let response = TunnelResponse(
            error: failure, status: client?.status(), relay: relay?.status(), share: shareState)
        return try? JSONEncoder().encode(response)
    }

    private func perform(_ request: TunnelRequest) throws {
        switch request.command {
        case .status:
            break
        case .setPolicy:
            guard let path = request.path, let policy = request.policy else { throw Failure.badRequest }
            try running().setPolicy(path, policy: policy)
            try saveConfig()
        case .setPriority:
            guard let all = request.enabled else { throw Failure.badRequest }
            try running().setPriority(all)
            try saveConfig()
        case .setShare:
            guard let enabled = request.enabled else { throw Failure.badRequest }
            shareEnabled = enabled
            reconcileRelay()
        }
    }

    private func running() throws -> MobileClient {
        guard let client else { throw Failure.notRunning }
        return client
    }

    /// The core keeps the user's choices in its config. Store what it has now,
    /// so they are still there the next time the tunnel starts.
    private func saveConfig() throws {
        guard let client else { return }
        let current = client.configJSON()
        try ConfigStore.update(current)
        config = current
    }

    private func networksChanged() {
        guard let client else { return }
        do {
            try client.setNetworks(watcher.networksJSON)
        } catch {
            Log.tunnel.error("setNetworks: \(error.localizedDescription, privacy: .public)")
        }
        reconcileRelay()
    }

    // MARK: Sharing

    /// Runs the relay exactly when it can do something: the tunnel is up,
    /// sharing is switched on and there is a cellular connection to lend.
    private func reconcileRelay() {
        let cellular = watcher.interface(of: "cellular")
        guard client != nil, shareEnabled, let cellular, let mesh else {
            stopRelay()
            shareProblem = shareEnabled && cellular == nil
                ? "There is no cellular connection to share at the moment."
                : nil
            return
        }
        if relay != nil, relayInterface == cellular { return }
        stopRelay()
        do {
            let relay = try BondCore.startRelay(
                config: config, network: "cellular", interface: cellular, platform: platform)
            advertiser.start(
                name: mesh.name, type: BondCore.relayServiceType, port: relay.port(),
                txt: mesh.relayTxt ?? [])
            self.relay = relay
            relayInterface = cellular
            shareProblem = nil
        } catch {
            shareProblem = error.localizedDescription
        }
    }

    private func stopRelay() {
        advertiser.stop()
        relay?.stop()
        relay = nil
        relayInterface = nil
    }

    private var shareState: ShareState {
        var state = ShareState(enabled: shareEnabled, running: relay != nil, problem: shareProblem)
        guard relay != nil else { return state }
        switch advertiser.state {
        case .advertised(let name): state.advertisedAs = name
        case .failed(let reason): state.problem = reason
        case .pending: break
        }
        return state
    }
}
