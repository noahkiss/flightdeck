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
    /// Whether the key can sign while the device is locked, once it has been
    /// unlocked after a restart (transport.md 7.2).
    var signsWhileLocked: Bool { get }
    /// The keys to offer at SSH auth, in order, each with its fingerprint,
    /// unlocked like `signingKey`. One key offers itself; a key move offers
    /// the new key, then the old one.
    func signingKeys() async throws -> [(fingerprint: String, key: NIOSSHPrivateKey)]
    /// The host took the key with this fingerprint.
    func accepted(fingerprint: String)
}

extension SSHKeySource {
    public func signingKey() async throws -> NIOSSHPrivateKey {
        try privateKey()
    }

    public var signsWhileLocked: Bool { true }

    public func signingKeys() async throws -> [(fingerprint: String, key: NIOSSHPrivateKey)] {
        [(fingerprint, try await signingKey())]
    }

    public func accepted(fingerprint: String) {}

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

/// Reads the device-wide "Require Face ID for SSH keys" setting at the
/// moment a key is used, so a change applies to keys already loaded.
public typealias FaceIDSetting = @Sendable () -> Bool

/// A generated P-256 key, as kept in the secret store.
struct StoredKey: Codable {
    var kind: SSHKeyKind
    /// The Secure Enclave blob or the raw software key.
    var data: Data
    /// The per-key Face ID flag of earlier builds. Read once by the
    /// migration into the device-wide setting, then dropped; never written.
    var biometry: Bool?
    var created: Date
    /// When the key can sign: `afterFirstUnlock` for keys this build makes.
    /// None on keys of earlier builds, whose enclave keys sign only while
    /// the device is unlocked (transport.md 7.2).
    var access: String?

    static let afterFirstUnlock = "after-first-unlock"
    /// Only tests write it: a software key that acts like an earlier
    /// build's enclave key.
    static let whenUnlocked = "when-unlocked"

    /// An enclave key's access rule is fixed when it is made; a software
    /// key has none, so it follows its Keychain item.
    var signsWhileLocked: Bool {
        access == Self.afterFirstUnlock || (access == nil && kind == .software)
    }
}

/// A generated device key loaded from the store.
public struct GeneratedKey: SSHKeySource {
    public let id: String
    public let kind: SSHKeyKind
    public let publicKey: NIOSSHPublicKey
    public let created: Date
    private let data: Data
    private let faceID: FaceIDSetting
    /// An enclave key made by an earlier build with `.biometryCurrentSet`:
    /// it asks for Face ID whatever the setting says.
    public let biometryBuiltIn: Bool
    public let signsWhileLocked: Bool

    /// Read at each use, not at load (review S1).
    public var requiresBiometry: Bool { biometryBuiltIn || faceID() }

    init(id: String, stored: StoredKey, faceID: @escaping FaceIDSetting) throws {
        self.faceID = faceID
        self.id = id
        self.kind = stored.kind
        self.data = stored.data
        self.created = stored.created
        self.signsWhileLocked = stored.signsWhileLocked
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
        biometryBuiltIn = stored.kind == .secureEnclave && Self.enclaveNeedsUser(stored.data)
    }

    /// Whether a Secure Enclave key needs the user to sign: earlier builds
    /// made the key with `.biometryCurrentSet` when its Face ID switch was
    /// on. A signature with no interaction allowed tells, so no flag is kept.
    /// Only the "needs interaction" refusal counts; any other failure (the
    /// device locked a moment ago, a key Face ID re-enrolment voided) says
    /// nothing about Face ID and surfaces at the dial instead.
    static func enclaveNeedsUser(_ data: Data) -> Bool {
        let ctx = LAContext()
        ctx.interactionNotAllowed = true
        do {
            let k = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: data, authenticationContext: ctx)
            _ = try k.signature(for: Data("flightdeck".utf8))
            return false
        } catch {
            return isNotInteractive(error)
        }
    }

    /// LocalAuthentication's "interaction not allowed", however CryptoKit wraps it.
    static func isNotInteractive(_ error: Error) -> Bool {
        var e: NSError? = error as NSError
        while let n = e {
            if n.domain == LAError.errorDomain && n.code == LAError.Code.notInteractive.rawValue { return true }
            e = n.userInfo[NSUnderlyingErrorKey] as? NSError
        }
        return false
    }

    /// With Face ID required, asks for it first and signs with that
    /// authentication, so signing during the handshake does not prompt.
    public func signingKey() async throws -> NIOSSHPrivateKey {
        guard requiresBiometry else { return try privateKey() }
        return try privateKey(authenticated: try await Self.faceIDContext())
    }

    /// Asks for Face ID once; the context then signs with no prompt.
    static func faceIDContext() async throws -> LAContext {
        let ctx = LAContext()
        ctx.localizedReason = "Use the SSH key"
        _ = try await ctx.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, localizedReason: "Use the SSH key")
        return ctx
    }

    /// The private key, signing with an authentication already made.
    func privateKey(authenticated ctx: LAContext) throws -> NIOSSHPrivateKey {
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
    static let faceIDAccount = FaceIDState.account
    /// Store accounts with this prefix hold settings, never keys.
    static let settingPrefix = "setting-"

    let store: SecretStore
    private let lock = NSLock()
    private var cache: [String: any SSHKeySource] = [:]
    /// Whether new keys go to the Secure Enclave when it exists.
    private let preferEnclave: Bool
    /// The Face ID setting. Keys hold it too, so they read it at each use
    /// (review S1); it holds no key, so there is no cycle.
    private let setting: FaceIDState
    private var upgraded = false
    private var moveObserver: (@Sendable () -> Void)?

    public init(store: SecretStore, preferEnclave: Bool = true) {
        self.store = store
        self.preferEnclave = preferEnclave
        setting = FaceIDState(store: store)
    }

    /// Whether this device has a Secure Enclave (false in the simulator).
    public static var enclaveAvailable: Bool { SecureEnclave.isAvailable }

    /// The key with this id, generated or imported, or nil when there is none.
    public func key(_ id: String) throws -> (any SSHKeySource)? {
        if let k = lock.withLock({ cache[id] }) { return k }
        // The item first: a missing key reads as missing even when the
        // setting cannot be read yet.
        guard !id.hasPrefix(Self.settingPrefix), let data = try store.read(account: id) else { return nil }
        _ = try faceID()
        let k: any SSHKeySource
        if id.hasPrefix(Self.importedPrefix) {
            k = try loadImported(id: id, data: data, faceID: faceIDSetting)
        } else {
            k = try GeneratedKey(id: id, stored: JSONDecoder().decode(StoredKey.self, from: data), faceID: faceIDSetting)
        }
        cacheKey(k)
        return k
    }

    // MARK: - Face ID (app.md 10.7)

    /// The device-wide "Require Face ID for SSH keys" setting: every key,
    /// generated or imported, asks for Face ID before each dial when it is on.
    public func faceID() throws -> Bool {
        try setting.value()
    }

    /// What keys read at each use.
    var faceIDSetting: FaceIDSetting {
        let s = setting
        return { s.current() }
    }

    /// Changes the setting. The caller asks for authentication before
    /// turning it off. Loaded keys read it at their next use.
    public func setFaceID(_ on: Bool) throws {
        try setting.set(on)
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
        _ = try faceID()
        let stored: StoredKey
        if preferEnclave && SecureEnclave.isAvailable {
            var error: Unmanaged<CFError>?
            // After first unlock, so Approve and Deny from a notification can
            // sign while the device is locked (transport.md 7.2).
            let access = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly, [.privateKeyUsage], &error)
            guard let access else {
                throw TransportError("cannot make the key's access rule: \(error?.takeRetainedValue().localizedDescription ?? "unknown")")
            }
            let k = try SecureEnclave.P256.Signing.PrivateKey(accessControl: access)
            stored = StoredKey(kind: .secureEnclave, data: k.dataRepresentation, created: Date(), access: StoredKey.afterFirstUnlock)
        } else {
            stored = StoredKey(kind: .software, data: P256.Signing.PrivateKey().rawRepresentation, created: Date(), access: StoredKey.afterFirstUnlock)
        }
        try store.write(account: id, data: JSONEncoder().encode(stored))
        let k = try GeneratedKey(id: id, stored: stored, faceID: faceIDSetting)
        lock.withLock { cache[id] = k }
        return k
    }

    /// Moves every stored item to the current storage class (transport.md
    /// 7.2). Runs once per process; a locked device fails and the next call
    /// tries again.
    public func upgradeStore() {
        guard !lock.withLock({ upgraded }) else { return }
        do {
            try store.upgradeProtection()
            lock.withLock { upgraded = true }
        } catch {
            Self.log.notice("keychain upgrade failed, will retry: \(String(describing: error), privacy: .public)")
        }
    }

    // MARK: - Key move (transport.md 7.2)

    /// Where the new key waits while hosts learn it.
    public static let nextID = "device-next"
    /// The ids of the hab entries whose host took the new key.
    static let moveAccount = settingPrefix + "key-move"

    struct MoveRecord: Codable {
        var accepted: [String]
    }

    /// Called after a host takes the new key, on any thread.
    public var onKeyMove: (@Sendable () -> Void)? {
        get { lock.withLock { moveObserver } }
        set { lock.withLock { moveObserver = newValue } }
    }

    /// The new key while a move runs.
    public func nextKey() throws -> GeneratedKey? {
        try key(Self.nextID) as? GeneratedKey
    }

    /// Starts a move when the default key cannot sign while locked: an
    /// earlier build's enclave key, whose access rule cannot change. A new
    /// key is made beside it; the old one keeps working meanwhile. Returns
    /// the new key, or nil when no move is needed (and clears any leftover).
    @discardableResult
    public func startKeyMove() throws -> GeneratedKey? {
        let d = try defaultKey()
        if d.signsWhileLocked {
            try cancelKeyMove()
            return nil
        }
        if let n = try nextKey() { return n }
        let n = try create(id: Self.nextID)
        Self.log.notice("key move started: \(n.fingerprint, privacy: .public)")
        return n
    }

    /// The hab entries whose host took the new key.
    public func movedEntries() throws -> Set<String> {
        guard let d = try store.read(account: Self.moveAccount) else { return [] }
        return Set(try JSONDecoder().decode(MoveRecord.self, from: d).accepted)
    }

    /// Records that this entry's host took the new key.
    public func noteMoved(entry: String) {
        do {
            var s = try movedEntries()
            guard !s.contains(entry) else { return }
            s.insert(entry)
            try store.write(account: Self.moveAccount, data: JSONEncoder().encode(MoveRecord(accepted: s.sorted())))
        } catch {
            Self.log.error("key move record: \(String(describing: error), privacy: .public)")
            return
        }
        onKeyMove?()
    }

    /// The new key becomes the default key. The new key is written first,
    /// so a stop halfway leaves a default key that signs while locked, and
    /// the next start clears the rest.
    public func finishKeyMove() throws {
        guard let data = try store.read(account: Self.nextID) else { return }
        try store.write(account: Self.defaultID, data: data)
        _ = lock.withLock { cache.removeValue(forKey: Self.defaultID) }
        try cancelKeyMove()
        Self.log.notice("key move finished")
    }

    /// Drops the new key and the record.
    public func cancelKeyMove() throws {
        try store.delete(account: Self.nextID)
        try store.delete(account: Self.moveAccount)
        _ = lock.withLock { cache.removeValue(forKey: Self.nextID) }
    }

    /// The key an entry signs with: during a move, the default key offers
    /// the new key first, then the old one.
    public func signingSource(keyID: String, entry: String) throws -> (any SSHKeySource)? {
        guard let k = try key(keyID) else { return nil }
        guard keyID == Self.defaultID, let d = k as? GeneratedKey, !d.signsWhileLocked, let n = try nextKey() else { return k }
        return MovingKey(next: n, old: d, entry: entry, keys: self)
    }

    /// Whether an answer from a notification can sign while the device is
    /// locked (app.md 16.4). A key that cannot be read means the device has
    /// not been unlocked since it started. A missing key is not a lock
    /// problem: the dial reports it.
    public func lockedSigning(keyID: String, entry: String) -> LockedSigning {
        do {
            guard let k = try signingSource(keyID: keyID, entry: entry) else { return .ready }
            return k.signsWhileLocked ? .ready : .keyLocked
        } catch {
            return .beforeFirstUnlock
        }
    }

    /// Whether the store can be read now, past any cache: false before the
    /// device's first unlock after a restart.
    public func storeReadable() -> Bool {
        do {
            _ = try store.read(account: Self.defaultID)
            return true
        } catch {
            return false
        }
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
            return TransportError("the device is locked: unlock it to use its SSH key")
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

/// Whether a key can sign now, with the device locked.
public enum LockedSigning: Equatable, Sendable {
    /// It can: the device was unlocked once since it started.
    case ready
    /// The key cannot be read: the device has not been unlocked since it started.
    case beforeFirstUnlock
    /// The key signs only while the device is unlocked: an earlier build's
    /// key, while the host has not taken the new one yet.
    case keyLocked
}

/// The default key during a key move: offers the new key, then the old one,
/// and records the host that takes the new key.
struct MovingKey: SSHKeySource {
    let next: GeneratedKey
    let old: GeneratedKey
    let entry: String
    let keys: DeviceKeys

    var id: String { DeviceKeys.defaultID }
    var kind: SSHKeyKind { next.kind }
    var publicKey: NIOSSHPublicKey { next.publicKey }
    var requiresBiometry: Bool { next.requiresBiometry || old.requiresBiometry }
    func privateKey() throws -> NIOSSHPrivateKey { try next.privateKey() }

    /// Locked, only the new key signs; it helps once this host took it.
    var signsWhileLocked: Bool {
        (try? keys.movedEntries().contains(entry)) ?? false
    }

    /// Face ID at most once for both keys. The old key may fail (the device
    /// is locked); the new one alone is offered then.
    func signingKeys() async throws -> [(fingerprint: String, key: NIOSSHPrivateKey)] {
        let ctx: LAContext? = requiresBiometry ? try await GeneratedKey.faceIDContext() : nil
        func load(_ k: GeneratedKey) throws -> NIOSSHPrivateKey {
            if let ctx { return try k.privateKey(authenticated: ctx) }
            return try k.privateKey()
        }
        var out: [(fingerprint: String, key: NIOSSHPrivateKey)] = []
        var firstError: Error?
        for k in [next, old] {
            do {
                out.append((k.fingerprint, try load(k)))
            } catch {
                if firstError == nil { firstError = error }
            }
        }
        if out.isEmpty, let firstError { throw firstError }
        return out
    }

    func accepted(fingerprint: String) {
        if fingerprint == next.fingerprint { keys.noteMoved(entry: entry) }
    }
}

/// The "Require Face ID for SSH keys" setting: one item in the keys' store
/// (no item means off), and the migration of earlier per-key flags into it.
final class FaceIDState: @unchecked Sendable {
    static let account = "setting-face-id"

    private let store: SecretStore
    /// Held for the whole migration and each change, so a first migration on
    /// another thread cannot undo a change (review N2).
    private let lock = NSRecursiveLock()
    private var migrated = false
    /// The setting as last read or written; nil until first read. The app
    /// makes one `DeviceKeys`, so this copy is the truth once read.
    private var cached: Bool?

    init(store: SecretStore) {
        self.store = store
    }

    func value() throws -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if let v = cached { return v }
        try migrate()
        let v = try store.read(account: Self.account) != nil
        cached = v
        return v
    }

    /// For keys at each use. A setting that cannot be read counts as on:
    /// the check fails closed.
    func current() -> Bool {
        (try? value()) ?? true
    }

    func set(_ on: Bool) throws {
        lock.lock()
        defer { lock.unlock() }
        try migrate()
        if on {
            try store.write(account: Self.account, data: Data("on".utf8))
        } else {
            try store.delete(account: Self.account)
        }
        cached = on
    }

    /// Folds the per-key Face ID flags of earlier builds into the setting:
    /// any key that had it on turns the setting on, then every stored key is
    /// written again without its flag. The setting is written first, so a
    /// stop halfway leaves it on and the next run finishes the rest.
    private func migrate() throws {
        if migrated { return }
        var anyOn = false
        var rewrites: [(String, Data)] = []
        for account in try store.accounts() where account != Self.account {
            guard let data = try store.read(account: account) else { continue }
            if account.hasPrefix(DeviceKeys.importedPrefix) {
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
            try store.write(account: Self.account, data: Data("on".utf8))
        }
        for (account, data) in rewrites {
            try store.write(account: account, data: data)
        }
        if !rewrites.isEmpty {
            DeviceKeys.log.notice("moved \(rewrites.count, privacy: .public) per-key Face ID flags into the setting (on: \(anyOn, privacy: .public))")
        }
        migrated = true
    }
}
