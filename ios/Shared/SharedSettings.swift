import Foundation

/// Small settings both processes read, kept in the App Group's defaults.
enum SharedSettings {
    /// The experimental "share over Wi-Fi" switch. The app writes it; the
    /// tunnel reads it when it starts.
    static var shareEnabled: Bool {
        get { defaults.bool(forKey: "shareEnabled") }
        set { defaults.set(newValue, forKey: "shareEnabled") }
    }

    /// Why the tunnel last failed to start, written by the extension — the
    /// app has no other dependable way to learn the reason.
    static var lastStartError: String? {
        get { defaults.string(forKey: "lastStartError") }
        set { defaults.set(newValue, forKey: "lastStartError") }
    }

    static func reset() {
        defaults.removeObject(forKey: "shareEnabled")
        defaults.removeObject(forKey: "lastStartError")
    }

    private static var defaults: UserDefaults {
        UserDefaults(suiteName: BondID.appGroup) ?? .standard
    }
}
