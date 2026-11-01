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
}

/// One SSH key this device signs with. The key source is pluggable: the
/// generated key today, an imported OpenSSH key later (task-1csglxtc).
public protocol SSHKeySource: Sendable {
    /// A stable id that hab entries name.
    var id: String { get }
    var kind: SSHKeyKind { get }
    var publicKey: NIOSSHPublicKey { get }
    /// Whether using the key asks for Face ID.
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
    var biometry: Bool
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

    init(id: String, stored: StoredKey) throws {
        self.id = id
        self.kind = stored.kind
        self.data = stored.data
        self.requiresBiometry = stored.biometry
        self.created = stored.created
        switch stored.kind {
        case .secureEnclave:
            // Loading the public part needs no authentication.
            let k = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: stored.data)
            publicKey = NIOSSHPrivateKey(secureEnclaveP256Key: k).publicKey
        case .software:
            publicKey = NIOSSHPrivateKey(p256Key: try P256.Signing.PrivateKey(rawRepresentation: stored.data)).publicKey
        }
    }

    /// With Face ID on, asks for it first and signs with that authentication,
    /// so signing during the handshake does not prompt.
    public func signingKey() async throws -> NIOSSHPrivateKey {
        guard requiresBiometry, kind == .secureEnclave else { return try privateKey() }
        let ctx = LAContext()
        ctx.localizedReason = "Use the SSH key"
        _ = try await ctx.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, localizedReason: "Use the SSH key")
        return NIOSSHPrivateKey(secureEnclaveP256Key: try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: data, authenticationContext: ctx))
    }

    public func privateKey() throws -> NIOSSHPrivateKey {
        switch kind {
        case .secureEnclave:
            return NIOSSHPrivateKey(secureEnclaveP256Key: try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: data))
        case .software:
            return NIOSSHPrivateKey(p256Key: try P256.Signing.PrivateKey(rawRepresentation: data))
        }
    }
}

/// The device's SSH keys, kept in a secret store (the Keychain in the app).
public final class DeviceKeys: @unchecked Sendable {
    /// The id of the key made on first start.
    public static let defaultID = "device"

    private let store: SecretStore
    private let lock = NSLock()
    private var cache: [String: GeneratedKey] = [:]
    /// Whether new keys go to the Secure Enclave when it exists.
    private let preferEnclave: Bool

    public init(store: SecretStore, preferEnclave: Bool = true) {
        self.store = store
        self.preferEnclave = preferEnclave
    }

    /// Whether this device has a Secure Enclave (false in the simulator).
    public static var enclaveAvailable: Bool { SecureEnclave.isAvailable }

    /// The key with this id, or nil when there is none.
    public func key(_ id: String) throws -> GeneratedKey? {
        if let k = lock.withLock({ cache[id] }) { return k }
        guard let data = try store.read(account: id) else { return nil }
        let k = try GeneratedKey(id: id, stored: JSONDecoder().decode(StoredKey.self, from: data))
        lock.withLock { cache[id] = k }
        return k
    }

    /// The default key, made on first use.
    public func defaultKey() throws -> GeneratedKey {
        if let k = try key(Self.defaultID) { return k }
        return try create(id: Self.defaultID, biometry: false)
    }

    /// Makes a new key under `id`, replacing any key there. With `biometry`,
    /// every use asks for Face ID (owner decision: off by default).
    @discardableResult
    public func create(id: String, biometry: Bool) throws -> GeneratedKey {
        let stored: StoredKey
        if preferEnclave && SecureEnclave.isAvailable {
            var access: SecAccessControl?
            var error: Unmanaged<CFError>?
            var flags: SecAccessControlCreateFlags = [.privateKeyUsage]
            if biometry { flags.insert(.biometryCurrentSet) }
            access = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, flags, &error)
            guard let access else {
                throw TransportError("cannot make the key's access rule: \(error?.takeRetainedValue().localizedDescription ?? "unknown")")
            }
            let k = try SecureEnclave.P256.Signing.PrivateKey(accessControl: access)
            stored = StoredKey(kind: .secureEnclave, data: k.dataRepresentation, biometry: biometry, created: Date())
        } else {
            stored = StoredKey(kind: .software, data: P256.Signing.PrivateKey().rawRepresentation, biometry: false, created: Date())
        }
        try store.write(account: id, data: JSONEncoder().encode(stored))
        let k = try GeneratedKey(id: id, stored: stored)
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
