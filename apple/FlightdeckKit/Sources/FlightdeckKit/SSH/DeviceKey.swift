import CryptoKit
import Foundation
import LocalAuthentication
import NIOSSH

/// Where a device key's private part lives.
public enum SSHKeyKind: String, Codable, Sendable {
    /// A P-256 key made in the Secure Enclave. Its stored form is a blob
    /// wrapped by this device's enclave, useless anywhere else.
    case secureEnclave = "secure-enclave"
    /// A P-256 key in software: the simulator, unit tests, a Mac with no enclave.
    case software
    /// An OpenSSH key the user imported (app.md 10.4), kept in the Keychain.
    case imported
}

/// One SSH key this device signs with: the generated key (`GeneratedKey`)
/// or an imported OpenSSH key (`ImportedKey`).
public protocol SSHKeySource: Sendable {
    /// A stable id that hab entries name.
    var id: String { get }
    var kind: SSHKeyKind { get }
    var publicKey: NIOSSHPublicKey { get }
    /// Whether using the key asks for Face ID: the device-wide setting, or
    /// a Secure Enclave key made with Face ID built in (app.md 10.7).
    var requiresBiometry: Bool { get }
    /// The private key for one signing session. Throws when the device is
    /// locked or the key is gone.
    func privateKey() throws -> NIOSSHPrivateKey
    /// The private key for one connection, unlocked before the SSH handshake
    /// needs it, so no prompt runs on the network thread.
    func signingKey() async throws -> NIOSSHPrivateKey
}

extension SSHKeySource {
    public func signingKey() async throws -> NIOSSHPrivateKey {
        try privateKey()
    }

    /// The SHA-256 fingerprint, as `ssh-keygen -l` prints it.
    public var fingerprint: String { SSHFingerprint.of(publicKey) }

    /// The line for a host's authorized keys file.
    public func authorizedKeysLine(comment: String) -> String {
        String(openSSHPublicKey: publicKey) + " " + comment
    }
}

public enum SSHFingerprint {
    /// `SHA256:` and the unpadded base64 of the SHA-256 of the key's wire form.
    public static func of(_ key: NIOSSHPublicKey) -> String {
        let line = String(openSSHPublicKey: key)
        let parts = line.split(separator: " ")
        guard parts.count >= 2, let blob = Data(base64Encoded: String(parts[1])) else { return "?" }
        return of(blob: blob)
    }

    /// The fingerprint of a key's wire form.
    public static func of(blob: Data) -> String {
        let digest = Data(SHA256.hash(data: blob)).base64EncodedString()
        return "SHA256:" + digest.replacingOccurrences(of: "=", with: "")
    }

    /// Whether `s` looks like a SHA-256 fingerprint: the prefix and 43
    /// base64 characters (32 bytes, unpadded).
    public static func isValid(_ s: String) -> Bool {
        guard s.hasPrefix("SHA256:") else { return false }
        let b = s.dropFirst(7)
        guard b.count == 43 else { return false }
        return Data(base64Encoded: b + "=") != nil
    }
}

/// A generated P-256 key, as kept in the secret store.
struct StoredKey: Codable {
    var kind: SSHKeyKind
    /// The Secure Enclave blob or the raw software key.
    var data: Data
    /// The per-key Face ID flag of earlier builds. Read once by the
    /// migration into the device-wide setting, then dropped; never written.
    var biometry: Bool?
    var created: Date
}

/// A generated device key loaded from the store.
public struct GeneratedKey: SSHKeySource {
    public let id: String
    public let kind: SSHKeyKind
    public let publicKey: NIOSSHPublicKey
    public let requiresBiometry: Bool
    public let created: Date
    private let data: Data

    init(id: String, stored: StoredKey, faceID: Bool) throws {
        self.id = id
        self.kind = stored.kind
        self.data = stored.data
        self.created = stored.created
        switch stored.kind {
        case .secureEnclave:
            // Loading the public part needs no authentication.
            let k = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: stored.data)
            publicKey = NIOSSHPrivateKey(secureEnclaveP256Key: k).publicKey
        case .software:
            publicKey = NIOSSHPrivateKey(p256Key: try P256.Signing.PrivateKey(rawRepresentation: stored.data)).publicKey
        case .imported:
            throw TransportError("an imported key is not a generated key")
        }
        requiresBiometry = faceID || (stored.kind == .secureEnclave && Self.enclaveNeedsUser(stored.data))
    }

    /// Whether a Secure Enclave key needs the user to sign: earlier builds
    /// made the key with `.biometryCurrentSet` when its Face ID switch was
    /// on. A signature with no interaction allowed tells, so no flag is kept.
    static func enclaveNeedsUser(_ data: Data) -> Bool {
        let ctx = LAContext()
        ctx.interactionNotAllowed = true
        do {
            let k = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: data, authenticationContext: ctx)
            _ = try k.signature(for: Data("flightdeck".utf8))
            return false
        } catch {
            return true
        }
    }

    /// With Face ID required, asks for it first and signs with that
    /// authentication, so signing during the handshake does not prompt.
    public func signingKey() async throws -> NIOSSHPrivateKey {
        guard requiresBiometry else { return try privateKey() }
        let ctx = LAContext()
        ctx.localizedReason = "Use the SSH key"
        _ = try await ctx.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, localizedReason: "Use the SSH key")
        guard kind == .secureEnclave else { return try privateKey() }
        return NIOSSHPrivateKey(secureEnclaveP256Key: try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: data, authenticationContext: ctx))
    }

    public func privateKey() throws -> NIOSSHPrivateKey {
        switch kind {
        case .secureEnclave:
            return NIOSSHPrivateKey(secureEnclaveP256Key: try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: data))
        case .software, .imported:
            return NIOSSHPrivateKey(p256Key: try P256.Signing.PrivateKey(rawRepresentation: data))
        }
    }
}

/// The device's SSH keys, kept in a secret store (the Keychain in the app).
public final class DeviceKeys: @unchecked Sendable {
    /// The id of the key made on first start.
    public static let defaultID = "device"
    /// The store account of the device-wide "Require Face ID for SSH keys"
    /// setting. No item means off.
    static let faceIDAccount = "setting-face-id"

    let store: SecretStore
    private let lock = NSLock()
    private var cache: [String: any SSHKeySource] = [:]
    /// Whether new keys go to the Secure Enclave when it exists.
    private let preferEnclave: Bool
    /// Whether the per-key flags of earlier builds were folded into the setting.
    private var migrated = false

    public init(store: SecretStore, preferEnclave: Bool = true) {
        self.store = store
        self.preferEnclave = preferEnclave
    }

    /// Whether this device has a Secure Enclave (false in the simulator).
    public static var enclaveAvailable: Bool { SecureEnclave.isAvailable }

    /// The key with this id, generated or imported, or nil when there is none.
    public func key(_ id: String) throws -> (any SSHKeySource)? {
        if let k = lock.withLock({ cache[id] }) { return k }
        // The item first: a missing key reads as missing even when the
        // setting cannot be read yet.
        guard id != Self.faceIDAccount, let data = try store.read(account: id) else { return nil }
        let on = try faceID()
        let k: any SSHKeySource
        if id.hasPrefix(Self.importedPrefix) {
            k = try loadImported(id: id, data: data, faceID: on)
        } else {
            k = try GeneratedKey(id: id, stored: JSONDecoder().decode(StoredKey.self, from: data), faceID: on)
        }
        cacheKey(k)
        return k
    }

    // MARK: - Face ID (app.md 10.7)

    /// The device-wide "Require Face ID for SSH keys" setting: every key,
    /// generated or imported, asks for Face ID before each dial when it is on.
    public func faceID() throws -> Bool {
        try migrate()
        return try store.read(account: Self.faceIDAccount) != nil
    }

    /// Changes the setting. The caller asks for authentication before
    /// turning it off. Loaded keys are dropped so the next use reads it.
    public func setFaceID(_ on: Bool) throws {
        try migrate()
        if on {
            try store.write(account: Self.faceIDAccount, data: Data("on".utf8))
        } else {
            try store.delete(account: Self.faceIDAccount)
        }
        lock.withLock { cache.removeAll() }
    }

    /// Folds the per-key Face ID flags of earlier builds into the setting:
    /// any key that had it on turns the setting on, then every stored key is
    /// written again without its flag. The setting is written first, so a
    /// stop halfway leaves it on and the next run finishes the rest.
    private func migrate() throws {
        if lock.withLock({ migrated }) { return }
        var anyOn = false
        var rewrites: [(String, Data)] = []
        for account in try store.accounts() where account != Self.faceIDAccount {
            guard let data = try store.read(account: account) else { continue }
            if account.hasPrefix(Self.importedPrefix) {
                guard var k = try? JSONDecoder().decode(StoredImportedKey.self, from: data), let b = k.biometry else { continue }
                anyOn = anyOn || b
                k.biometry = nil
                rewrites.append((account, try JSONEncoder().encode(k)))
            } else {
                guard var k = try? JSONDecoder().decode(StoredKey.self, from: data), let b = k.biometry else { continue }
                anyOn = anyOn || b
                k.biometry = nil
                rewrites.append((account, try JSONEncoder().encode(k)))
            }
        }
        if anyOn {
            try store.write(account: Self.faceIDAccount, data: Data("on".utf8))
        }
        for (account, data) in rewrites {
            try store.write(account: account, data: data)
        }
        if !rewrites.isEmpty {
            Self.log.notice("moved \(rewrites.count, privacy: .public) per-key Face ID flags into the setting (on: \(anyOn, privacy: .public))")
        }
        lock.withLock { migrated = true }
    }

    func cacheKey(_ k: any SSHKeySource) {
        lock.withLock { cache[k.id] = k }
    }

    /// The default key, made on first use.
    public func defaultKey() throws -> GeneratedKey {
        if let k = try key(Self.defaultID) as? GeneratedKey { return k }
        return try create(id: Self.defaultID)
    }

    /// Makes a new key under `id`, replacing any key there. Face ID is the
    /// device-wide setting, an app check: the key itself has no biometric
    /// rule, so turning the setting on or off keeps the key.
    @discardableResult
    public func create(id: String) throws -> GeneratedKey {
        let on = try faceID()
        let stored: StoredKey
        if preferEnclave && SecureEnclave.isAvailable {
            var error: Unmanaged<CFError>?
            let access = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, [.privateKeyUsage], &error)
            guard let access else {
                throw TransportError("cannot make the key's access rule: \(error?.takeRetainedValue().localizedDescription ?? "unknown")")
            }
            let k = try SecureEnclave.P256.Signing.PrivateKey(accessControl: access)
            stored = StoredKey(kind: .secureEnclave, data: k.dataRepresentation, created: Date())
        } else {
            stored = StoredKey(kind: .software, data: P256.Signing.PrivateKey().rawRepresentation, created: Date())
        }
        try store.write(account: id, data: JSONEncoder().encode(stored))
        let k = try GeneratedKey(id: id, stored: stored, faceID: on)
        lock.withLock { cache[id] = k }
        return k
    }

    /// Removes a key. Hab entries that named it have no key until one is chosen.
    public func delete(_ id: String) throws {
        try store.delete(account: id)
        _ = lock.withLock { cache.removeValue(forKey: id) }
    }

    /// A key error in words: the locked device is the common one.
    public static func explain(_ error: Error) -> TransportError {
        if let t = error as? TransportError { return t }
        if let la = error as? LAError {
            return TransportError("Face ID did not unlock the SSH key: \(la.localizedDescription)")
        }
        if (error as NSError).code == Int(errSecInteractionNotAllowed) {
            return TransportError("the device is locked: its SSH key works only while it is unlocked")
        }
        if (error as NSError).code == Int(errSecUserCanceled) {
            return TransportError("the SSH key was not unlocked: Face ID was cancelled")
        }
        return TransportError("the device key is not usable: \(error.localizedDescription)")
    }

    /// The comment for the authorized keys line: the app and the device kind.
    public static var comment: String {
        #if os(macOS)
        return "flightdeck-mac"
        #else
        return "flightdeck-ios"
        #endif
    }
}
