import Foundation
import Security

/// Small secrets by account name: device keys today.
public protocol SecretStore: Sendable {
    func read(account: String) throws -> Data?
    func write(account: String, data: Data) throws
    func delete(account: String) throws
}

/// The Keychain: generic password items, usable only while the device is
/// unlocked, this device only, never synchronised, so neither iCloud Keychain
/// nor a backup carries them to another device (app.md 10.4).
///
/// On iOS the items live in the data protection Keychain. The Mac app is not
/// sandboxed and may be signed ad hoc, which the data protection Keychain
/// refuses, so on the Mac the items go to the login keychain.
public struct KeychainStore: SecretStore {
    public let service: String

    public init(service: String = "flightdeck.ssh-key") {
        self.service = service
    }

    private func base(_ account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecAttrSynchronizable as String: false,
        ]
    }

    public func read(account: String) throws -> Data? {
        var q = base(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let rc = SecItemCopyMatching(q as CFDictionary, &out)
        if rc == errSecItemNotFound { return nil }
        guard rc == errSecSuccess else { throw Self.error("read", rc) }
        return out as? Data
    }

    public func write(account: String, data: Data) throws {
        // Update in place, so a failed write never loses the old key.
        let update: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
        ]
        let rc0 = SecItemUpdate(base(account) as CFDictionary, update as CFDictionary)
        if rc0 == errSecSuccess { return }
        guard rc0 == errSecItemNotFound else { throw Self.error("write", rc0) }
        var q = base(account)
        q[kSecValueData as String] = data
        q[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
        q[kSecAttrLabel as String] = "Flightdeck SSH key"
        let rc = SecItemAdd(q as CFDictionary, nil)
        guard rc == errSecSuccess else { throw Self.error("write", rc) }
    }

    public func delete(account: String) throws {
        let rc = SecItemDelete(base(account) as CFDictionary)
        guard rc == errSecSuccess || rc == errSecItemNotFound else { throw Self.error("delete", rc) }
    }

    static func error(_ op: String, _ rc: OSStatus) -> TransportError {
        let msg = SecCopyErrorMessageString(rc, nil) as String? ?? "OSStatus \(rc)"
        return TransportError("keychain \(op): \(msg)")
    }
}

/// An in-memory store for tests and previews.
public final class MemorySecretStore: SecretStore, @unchecked Sendable {
    private let lock = NSLock()
    private var items: [String: Data] = [:]

    public init() {}

    public func read(account: String) throws -> Data? { lock.withLock { items[account] } }
    public func write(account: String, data: Data) throws { lock.withLock { items[account] = data } }
    public func delete(account: String) throws { _ = lock.withLock { items.removeValue(forKey: account) } }
}
