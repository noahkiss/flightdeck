import Foundation
import HabAPI
import NIOSSH
import Observation
import os

/// The in-app SSH carrier as the app sees it: the device key, the hab entries
/// and the flows that change them (app.md 10.1, 10.4, 18.8).
@MainActor
@Observable
public final class SSHSetup {
    public let keys: DeviceKeys
    public private(set) var entries: [SSHHabEntry] = []
    /// The default device key, once loaded or made.
    public private(set) var deviceKey: GeneratedKey?
    /// Why the key could not be loaded or made, for Settings.
    public private(set) var keyError: String?
    /// A pairing offer opened from a link, waiting for its sheet.
    public var pendingCode: PairingCode?
    /// A pairing link that did not parse, for its sheet.
    public var pendingCodeError: String?
    /// Imported OpenSSH keys (app.md 10.4), oldest first.
    public private(set) var importedKeys: [ImportedKey] = []
    /// Why the imported keys could not be listed (a locked device).
    public private(set) var importedKeysError: String?
    /// A key file opened from the share sheet or Files, waiting for the import sheet.
    public var pendingKeyFile: PendingKeyFile?

    @ObservationIgnored private let listURL: URL?
    @ObservationIgnored private weak var fleet: Fleet?
    @ObservationIgnored private let timing: SSHTiming
    @ObservationIgnored private let clientLabel: String
    @ObservationIgnored private let clientVersion: String
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "ssh")

    public init(keys: DeviceKeys, listURL: URL?, fleet: Fleet?, timing: SSHTiming = SSHTiming(), clientLabel: String = "phone", clientVersion: String = "0") {
        self.keys = keys
        self.listURL = listURL
        self.fleet = fleet
        self.timing = timing
        self.clientLabel = clientLabel
        self.clientVersion = clientVersion
        do {
            entries = try SSHHabList.load(from: listURL).habs
        } catch {
            log.error("ssh hab list unreadable: \(String(describing: error), privacy: .public)")
        }
    }

    /// Loads the default key, making it on first use. A locked device leaves
    /// it nil with the reason in `keyError`; call again later.
    @discardableResult
    public func loadKey() -> GeneratedKey? {
        if let k = deviceKey { return k }
        do {
            let k = try keys.defaultKey()
            deviceKey = k
            keyError = nil
            // The public key only, so a host can be set up from the log too.
            let line = k.authorizedKeysLine(comment: DeviceKeys.comment)
            log.notice("device key \(k.kind.rawValue, privacy: .public) \(k.fingerprint, privacy: .public)")
            #if DEBUG
            log.debug("device key line: \(line, privacy: .public)")
            print("flightdeck: device key: \(line)")
            #endif
            return k
        } catch {
            keyError = DeviceKeys.explain(error).description
            return nil
        }
    }

    /// The line to add to a host's authorized keys.
    public var authorizedKeysLine: String? {
        deviceKey?.authorizedKeysLine(comment: DeviceKeys.comment)
    }

    /// Adds a connection to the fleet for every entry.
    public func connectAll() {
        for e in entries { attach(e) }
    }

    private func attach(_ e: SSHHabEntry) {
        guard let fleet else { return }
        let t: Transport
        do {
            t = try e.transport(keys: keys, timing: timing)
        } catch {
            t = BrokenTransport(endpoint: "ssh " + e.destination, error: String(describing: error))
        }
        fleet.add(HabConnection(id: e.connectionID, transport: t, clientLabel: clientLabel, clientVersion: clientVersion), label: e.label)
    }

    // MARK: - Add a hab by hand (app.md 18.8)

    /// Connects with nothing pinned and returns the host key to confirm.
    /// The probe never signs, so any key will do.
    public func probe(_ dest: SSHDestination, keyID: String = DeviceKeys.defaultID) async throws -> NIOSSHPublicKey {
        let key: any SSHKeySource
        do {
            key = try self.key(keyID)
        } catch let e as MissingKey {
            throw e
        } catch {
            throw DeviceKeys.explain(error)
        }
        return try await NIOSSHTransport.probeHostKey(dest.endpoint, user: dest.user, key: key, timing: timing)
    }

    /// Checks an entry with one `hab.ping` over its own connection.
    public func test(_ e: SSHHabEntry) async throws -> Duration {
        let t = try e.transport(keys: keys, timing: timing)
        return try await HabOneShot.call(t, HabMethods.habPing, PingParams(), clientLabel: clientLabel, clientVersion: clientVersion).elapsed
    }

    /// Stores an entry and connects it.
    public func add(_ e: SSHHabEntry) throws {
        var next = entries
        next.removeAll { $0.id == e.id }
        next.append(e)
        try SSHHabList(habs: next).save(to: listURL)
        entries = next
        attach(e)
    }

    /// Forgets an entry and drops its connection.
    public func remove(_ e: SSHHabEntry) async throws {
        let next = entries.filter { $0.id != e.id }
        try SSHHabList(habs: next).save(to: listURL)
        entries = next
        await fleet?.remove(id: e.connectionID)
    }

    /// Makes a new default key. Every host must get the new public key.
    public func replaceKey(biometry: Bool) throws {
        let k = try keys.create(id: DeviceKeys.defaultID, biometry: biometry)
        deviceKey = k
        keyError = nil
        // Running connections hold the old key; rebuild them.
        let current = entries
        Task {
            for e in current { await fleet?.remove(id: e.connectionID) }
            for e in current { attach(e) }
        }
    }

    // MARK: - Imported keys (app.md 10.4)

    /// Reads the imported keys from the store.
    public func loadImportedKeys() {
        do {
            importedKeys = try keys.importedKeys()
            importedKeysError = nil
        } catch {
            importedKeysError = DeviceKeys.explain(error).description
        }
    }

    /// Decrypts a key file and stores the key. The passphrase and the file's
    /// bytes stay with the caller, which drops them; only the raw key reaches
    /// the Keychain. Decryption runs off the main actor.
    public func importKey(_ file: OpenSSHKeyFile, passphrase: String?) async throws -> (key: ImportedKey, replaced: Bool) {
        let keys = self.keys
        let work = Task.detached(priority: .userInitiated) { () throws -> (key: ImportedKey, replaced: Bool) in
            let m = try file.decrypt(passphrase: passphrase)
            try Task.checkCancellation()
            return try keys.importKey(m)
        }
        // A dismissed sheet cancels its task; pass that on to the derivation.
        let r = try await withTaskCancellationHandler {
            try await work.value
        } onCancel: {
            work.cancel()
        }
        // The fingerprint only: the key never goes to a log.
        log.notice("imported key \(r.key.type.rawValue, privacy: .public) \(r.key.fingerprint, privacy: .public)")
        loadImportedKeys()
        return r
    }

    /// Turns "Require Face ID" on or off for an imported key.
    public func setBiometry(_ on: Bool, forImported id: String) throws {
        try keys.setBiometry(on, forImported: id)
        loadImportedKeys()
        reconnect { $0.keyID == id }
    }

    /// Removes an imported key. Entries that used it say "no key" until one
    /// is chosen.
    public func deleteImportedKey(_ id: String) throws {
        try keys.delete(id)
        loadImportedKeys()
        reconnect { $0.keyID == id }
    }

    /// The hab entries that sign with this key.
    public func entries(using keyID: String) -> [SSHHabEntry] {
        entries.filter { $0.keyID == keyID }
    }

    /// Whether the key an entry names is usable: missing after a restore from
    /// a backup (which carries no keys), unavailable while the device is locked.
    public func keyState(_ e: SSHHabEntry) -> KeyState {
        do {
            _ = try key(e.keyID)
            return .present
        } catch is MissingKey {
            return .missing
        } catch {
            return .unavailable(DeviceKeys.explain(error).description)
        }
    }

    /// Points an entry at another key and reconnects it.
    public func setKey(_ keyID: String, for e: SSHHabEntry) throws {
        guard let i = entries.firstIndex(where: { $0.id == e.id }) else { return }
        var next = entries
        next[i].keyID = keyID
        try SSHHabList(habs: next).save(to: listURL)
        entries = next
        reconnect { $0.id == e.id }
    }

    /// The key with this id: the generated one or an imported one. Throws
    /// `MissingKey` when there is none, and the store's error otherwise.
    public func key(_ id: String) throws -> any SSHKeySource {
        if id == DeviceKeys.defaultID {
            if let k = loadKey() { return k }
            throw TransportError(keyError ?? "no device key")
        }
        guard let k = try keys.key(id) else { throw MissingKey() }
        return k
    }

    /// Takes a key file opened from another app, for the import sheet. A
    /// copy iOS placed in the app's Inbox is removed once read.
    public func openKeyFile(_ url: URL) {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        do {
            // At most 64 KiB: "Open in" may hand over any file, a video too.
            let data = try OpenSSHKeyFile.read(contentsOf: url)
            pendingKeyFile = PendingKeyFile(name: url.lastPathComponent, data: data)
        } catch let e as KeyImportError {
            pendingKeyFile = PendingKeyFile(name: url.lastPathComponent, error: e.description)
        } catch {
            pendingKeyFile = PendingKeyFile(name: url.lastPathComponent, error: "Could not read the file: \(error.localizedDescription)")
        }
        #if os(iOS)
        // "Open in" copies the file into this app's own Inbox; drop that copy.
        let home = NSHomeDirectory()
        if url.path.hasPrefix(home), url.path.contains("/Documents/Inbox/") || url.path.contains("-Inbox/") {
            try? FileManager.default.removeItem(at: url)
        }
        #endif
    }

    private func reconnect(_ match: (SSHHabEntry) -> Bool) {
        let affected = entries.filter(match)
        guard !affected.isEmpty else { return }
        Task {
            for e in affected { await fleet?.remove(id: e.connectionID) }
            for e in affected { attach(e) }
        }
    }

    // MARK: - Pairing links

    /// Opens a `flightdeck://pair` link: parses it and shows the pairing sheet.
    public func open(url: URL) {
        do {
            pendingCode = try PairingCode.parse(url.absoluteString)
            pendingCodeError = nil
        } catch {
            pendingCode = nil
            pendingCodeError = String(describing: error)
        }
    }

    // MARK: - One call from the background

    /// Runs one call on the entry's own connection: the call site for
    /// notification actions (a ping now, `approval.answer` later).
    public nonisolated static func perform<P: Encodable & Sendable, R: Decodable & Sendable>(
        entry: SSHHabEntry,
        keys: DeviceKeys,
        _ method: HabMethod<P, R>,
        _ params: P,
        clientLabel: String = "phone",
        deadline: Duration = .seconds(20)
    ) async throws -> HabOneShot.Result<R> {
        let t = try entry.transport(keys: keys)
        return try await HabOneShot.call(t, method, params, clientLabel: clientLabel, deadline: deadline)
    }

    public func entry(id: String) -> SSHHabEntry? {
        entries.first { $0.id == id }
    }
}

/// A key file handed to the app, before the import sheet reads it.
public struct PendingKeyFile: Identifiable, Sendable {
    public let id = UUID()
    public let name: String
    public let data: Data
    /// Why the file could not be read; the sheet shows it instead of parsing.
    public let error: String?

    public init(name: String, data: Data) {
        self.name = name
        self.data = data
        self.error = nil
    }

    public init(name: String, error: String) {
        self.name = name
        self.data = Data()
        self.error = error
    }
}

/// Whether the key a hab entry names can be used now.
public enum KeyState: Equatable, Sendable {
    case present
    /// No such key on this device: deleted, or not carried by a backup.
    case missing
    /// The key exists but cannot be read now, with the reason (a locked device).
    case unavailable(String)
}

/// No key with the id an entry names.
public struct MissingKey: Error, CustomStringConvertible {
    public var description: String { "no key: pair again or choose a key" }
}

/// A transport that always fails with one message: an entry whose key is gone.
struct BrokenTransport: Transport {
    let endpoint: String
    let error: String
    func open(_ kind: StreamKind) async throws -> ByteStream {
        throw TransportError(error)
    }
}
