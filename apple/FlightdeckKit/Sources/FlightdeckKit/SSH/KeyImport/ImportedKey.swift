import CryptoKit
import Foundation
import LocalAuthentication
import NIOSSH
import os

/// An imported OpenSSH key as kept in the secret store: the raw private
/// part, never the file or its passphrase.
struct StoredImportedKey: Codable {
    var type: ImportedKeyType
    /// The Ed25519 seed or the ECDSA private scalar.
    var raw: Data
    var comment: String
    /// The per-key Face ID flag of earlier builds; read once by the
    /// migration into the device-wide setting (app.md 10.7), never written.
    var biometry: Bool?
    var created: Date
}

/// An imported OpenSSH key loaded from the store (app.md 10.4).
public struct ImportedKey: SSHKeySource {
    public let id: String
    public var kind: SSHKeyKind { .imported }
    public let type: ImportedKeyType
    public let publicKey: NIOSSHPublicKey
    public let comment: String
    public let created: Date
    private let material: ImportedKeyMaterial
    private let faceID: FaceIDSetting

    /// The device-wide setting, read at each use (review S1).
    public var requiresBiometry: Bool { faceID() }

    init(id: String, stored: StoredImportedKey, faceID: @escaping FaceIDSetting) throws {
        self.id = id
        self.type = stored.type
        self.comment = stored.comment
        self.faceID = faceID
        self.created = stored.created
        material = try ImportedKeyMaterial(type: stored.type, raw: stored.raw, comment: stored.comment)
        publicKey = material.publicKey
    }

    public func privateKey() throws -> NIOSSHPrivateKey {
        try material.privateKey()
    }

    /// With "Require Face ID for SSH keys" on, asks for it before handing out the key.
    /// The Keychain item itself is guarded by the device lock only.
    public func signingKey() async throws -> NIOSSHPrivateKey {
        if requiresBiometry {
            let ctx = LAContext()
            _ = try await ctx.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, localizedReason: "Use the SSH key")
        }
        return try privateKey()
    }

    /// The line for a host's authorized keys file, with the key's own comment.
    public var authorizedKeysLine: String {
        authorizedKeysLine(comment: comment.isEmpty ? DeviceKeys.comment : comment)
    }
}

extension DeviceKeys {
    /// The prefix of every imported key's id; the rest comes from the
    /// public key, so the same key imported twice keeps one item.
    static let importedPrefix = "imported-"

    /// The store id for a public key.
    public static func importedID(for key: NIOSSHPublicKey) -> String {
        let blob = SSHWireReader.blob(of: key)
        return importedPrefix + SHA256.hash(data: blob).prefix(12).map { String(format: "%02x", $0) }.joined()
    }

    /// Stores a decrypted key. Returns the key and whether it was there
    /// already (same public key), in which case the stored one is replaced.
    @discardableResult
    public func importKey(_ m: ImportedKeyMaterial) throws -> (key: ImportedKey, replaced: Bool) {
        _ = try faceID()
        let id = Self.importedID(for: m.publicKey)
        let replaced = try store.read(account: id) != nil
        let stored = StoredImportedKey(type: m.type, raw: m.raw, comment: m.comment, created: Date())
        try store.write(account: id, data: JSONEncoder().encode(stored))
        let k = try ImportedKey(id: id, stored: stored, faceID: faceIDSetting)
        cacheKey(k)
        return (k, replaced)
    }

    /// Every imported key in the store, oldest first. An item that does not
    /// decode is skipped and logged, so it cannot hide the others; a Keychain
    /// error (a locked device) still throws.
    public func importedKeys() throws -> [ImportedKey] {
        var out: [ImportedKey] = []
        for account in try store.accounts() where account.hasPrefix(Self.importedPrefix) {
            do {
                if let k = try key(account) as? ImportedKey { out.append(k) }
            } catch is DecodingError {
                Self.log.error("imported key \(account, privacy: .public) does not decode; skipped")
            } catch is KeyImportError {
                Self.log.error("imported key \(account, privacy: .public) is not a valid key; skipped")
            }
        }
        return out.sorted { $0.created < $1.created }
    }

    static let log = Logger(subsystem: "com.flightdeck.app", category: "ssh")

    func loadImported(id: String, data: Data, faceID: @escaping FaceIDSetting) throws -> ImportedKey {
        try ImportedKey(id: id, stored: JSONDecoder().decode(StoredImportedKey.self, from: data), faceID: faceID)
    }
}
