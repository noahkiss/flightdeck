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
    public func probe(_ dest: SSHDestination) async throws -> NIOSSHPublicKey {
        guard let key = loadKey() else { throw TransportError(keyError ?? "no device key") }
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

/// A transport that always fails with one message: an entry whose key is gone.
struct BrokenTransport: Transport {
    let endpoint: String
    let error: String
    func open(_ kind: StreamKind) async throws -> ByteStream {
        throw TransportError(error)
    }
}
