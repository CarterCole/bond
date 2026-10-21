import Foundation
import Security

/// Where the device's config lives. It holds the device's private keys, so it
/// is kept in the Keychain; the App Group doubles as the Keychain access group
/// that lets the tunnel extension read it.
enum ConfigStore {
    struct KeychainError: LocalizedError {
        let status: OSStatus

        var errorDescription: String? {
            let reason = SecCopyErrorMessageString(status, nil) as String? ?? "error \(status)"
            return "Keychain: \(reason)"
        }
    }

    /// The stored config, or nil if this phone has not joined a mesh.
    static func load() throws -> String? {
        var query = item
        query[kSecReturnData] = true
        query[kSecMatchLimit] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        switch status {
        case errSecSuccess:
            return (result as? Data).map { String(decoding: $0, as: UTF8.self) }
        case errSecItemNotFound:
            return nil
        default:
            throw KeychainError(status: status)
        }
    }

    /// Stores the config of a mesh this phone has just joined.
    static func save(_ config: String) throws {
        guard try !replace(with: config) else { return }
        var new = item
        new[kSecValueData] = Data(config.utf8)
        // The tunnel may be started while the phone is locked, but the keys
        // should never leave this phone in a backup.
        new[kSecAttrAccessible] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let status = SecItemAdd(new as CFDictionary, nil)
        guard status == errSecSuccess else { throw KeychainError(status: status) }
    }

    /// Replaces the stored config with a newer version of itself. Does nothing
    /// if there is none: the tunnel must not bring back a config the app
    /// deleted on leaving the mesh while the tunnel was still stopping.
    static func update(_ config: String) throws {
        _ = try replace(with: config)
    }

    /// Whether there was a stored config to replace.
    private static func replace(with config: String) throws -> Bool {
        let status = SecItemUpdate(item as CFDictionary, [kSecValueData: Data(config.utf8)] as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw KeychainError(status: status)
        }
        return status == errSecSuccess
    }

    static func delete() throws {
        let status = SecItemDelete(item as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw KeychainError(status: status)
        }
    }

    private static var item: [CFString: Any] {
        [
            kSecClass: kSecClassGenericPassword,
            kSecAttrService: BondID.app,
            kSecAttrAccount: "config",
            kSecAttrAccessGroup: BondID.appGroup,
        ]
    }
}
