import Foundation
import Network

/// Makes iOS ask for local-network access now.
///
/// The relay runs in the tunnel extension, and its permission is the app's.
/// iOS asks the first time the local network is touched, so a short Bonjour
/// browse from the app puts the question on screen while the user is looking
/// at the switch that needs it, not at some later moment in the background.
@MainActor
enum LocalNetworkPrompt {
    private static var browser: NWBrowser?

    static func raise() {
        guard browser == nil else { return }
        let browser = NWBrowser(for: .bonjour(type: BondCore.relayServiceType, domain: nil), using: .udp)
        browser.start(queue: .main)
        self.browser = browser
        Task {
            try? await Task.sleep(for: .seconds(5))
            browser.cancel()
            self.browser = nil
        }
    }
}
