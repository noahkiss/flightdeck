import Foundation
import NIOCore
import NIOSSH
import Synchronization

/// A hab over SSH, in the app: swift-nio-ssh with the device key
/// (transport.md 4.3, 4.8). Works on iOS and macOS.
///
/// - One SSH connection per hab carries the control stream and one session
///   channel per data stream (hab-logic-api.md Q2). Each channel runs the
///   remote command, `gff-hab connect` by default; the hab tells control from
///   data by the first byte.
/// - Each new control stream dials a fresh connection. Data streams use the
///   connection of the newest control stream. An older connection takes no new
///   channels and closes when its last channel does, so a dead TCP connection
///   left behind by a network change never catches a new stream.
/// - When the host refuses a channel (OpenSSH allows 10 per connection), the
///   next data stream dials one more connection (transport.md 7.6).
/// - The host key must match the pinned fingerprint, checked before auth.
public final class NIOSSHTransport: Transport, Sendable {
    public let user: String
    public let endpoints: [SSHEndpoint]
    public let command: String
    public let hostKey: String
    public let key: SSHKeySource
    private let pool: SSHPool

    /// The command each channel runs when the entry names none.
    public static let defaultCommand = "gff-hab connect"

    public init(user: String, endpoints: [SSHEndpoint], hostKeyFingerprint: String, key: SSHKeySource, command: String? = nil, timing: SSHTiming = SSHTiming()) {
        self.user = user
        self.endpoints = endpoints
        self.command = (command?.isEmpty ?? true) ? Self.defaultCommand : command!
        self.hostKey = hostKeyFingerprint
        self.key = key
        pool = SSHPool(user: user, endpoints: endpoints, check: .pinned(hostKeyFingerprint), key: key, timing: timing)
    }

    public var endpoint: String {
        "ssh " + user + "@" + (endpoints.first?.description ?? "-")
    }

    /// As the ssh subprocess carrier: connect, key exchange, auth and the
    /// remote command all run before the hab can answer.
    public var helloTimeout: Duration? { .seconds(25) }

    public func open(_ kind: StreamKind) async throws -> ByteStream {
        try await pool.open(kind, command: command)
    }

    public func reset() async {
        await pool.closeAll()
    }

    /// SSH connections open now, for tests.
    public func connectionCount() async -> Int {
        await pool.liveConnections()
    }

    /// Connects to `endpoint` with nothing pinned and returns the host key the
    /// server presents, for the user to confirm. Stops before auth.
    public static func probeHostKey(_ endpoint: SSHEndpoint, user: String, key: SSHKeySource, timing: SSHTiming = SSHTiming()) async throws -> NIOSSHPublicKey {
        do {
            let c = try await SSHConnection.dial(endpoint, user: user, key: key, check: .probe, timing: timing)
            c.close()
            throw TransportError("the host accepted a connection without a host key check")
        } catch let u as HostKeyUnconfirmed {
            return u.key
        }
    }
}

/// The SSH connections of one hab.
actor SSHPool {
    let user: String
    let endpoints: [SSHEndpoint]
    let check: HostKeyCheck
    let key: SSHKeySource
    let timing: SSHTiming

    /// One connection and the pool's counts for it.
    private struct Slot {
        let conn: SSHConnection
        /// The pool generation it belongs to; older ones take no new channels.
        let generation: Int
        /// Open session channels.
        var children = 0
        /// The host refused a channel (OpenSSH's MaxSessions): take no more.
        var full = false
    }

    private var slots: [Slot] = []
    private var generation = 0
    private var resets = 0
    private var dialing: (gen: Int, task: Task<SSHConnection, Error>)?

    init(user: String, endpoints: [SSHEndpoint], check: HostKeyCheck, key: SSHKeySource, timing: SSHTiming) {
        self.user = user
        self.endpoints = endpoints
        self.check = check
        self.key = key
        self.timing = timing
    }

    func liveConnections() -> Int {
        slots.filter(\.conn.isActive).count
    }

    func open(_ kind: StreamKind, command: String) async throws -> ByteStream {
        if kind == .control {
            generation += 1
            retireOld()
        }
        var rejected = 0
        while true {
            let conn = try await connection(fresh: kind == .control)
            update(conn) { $0.children += 1 }
            // Strong: the pool lives while any of its channels does.
            let once = Once { [self] in
                Task { await self.childEnded(conn) }
            }
            do {
                return try await conn.openSession(command: command, onEnd: { once.run() }, onFirstData: { ColdStart.markFirstReply() })
            } catch let e as NIOSSHError where e.type == .channelSetupRejected && rejected == 0 && kind == .data {
                // The host's session limit: one more connection for the next channels.
                rejected += 1
                update(conn) { $0.full = true }
                once.run()
                continue
            } catch {
                once.run()
                throw error
            }
        }
    }

    func closeAll() {
        generation += 1
        resets += 1
        dialing?.task.cancel()
        dialing = nil
        for s in slots { s.conn.close() }
        slots.removeAll()
    }

    private func update(_ c: SSHConnection, _ change: (inout Slot) -> Void) {
        if let i = slots.firstIndex(where: { $0.conn === c }) { change(&slots[i]) }
    }

    private func childEnded(_ c: SSHConnection) {
        guard let i = slots.firstIndex(where: { $0.conn === c }) else { return }
        slots[i].children = max(0, slots[i].children - 1)
        if slots[i].children == 0 {
            // No channel left: close it. The next stream dials again, so
            // nothing stays open in the background.
            c.close()
            slots.remove(at: i)
        }
    }

    /// Closes older connections that carry no channel.
    private func retireOld() {
        for s in slots where s.generation != generation && s.children == 0 {
            s.conn.close()
        }
        slots.removeAll { !$0.conn.isActive || ($0.generation != generation && $0.children == 0) }
    }

    /// A connection of the current generation with room, else a new one.
    private func connection(fresh: Bool) async throws -> SSHConnection {
        slots.removeAll { !$0.conn.isActive && $0.children == 0 }
        if !fresh, let s = slots.last(where: { $0.generation == generation && !$0.full && $0.conn.isActive }) {
            return s.conn
        }
        let gen = generation
        let resetsAtStart = resets
        if !fresh, let d = dialing, d.gen == gen {
            let c = try await d.task.value
            if resets != resetsAtStart { throw CancellationError() }
            try Self.requireActive(c)
            // This caller may resume before the dialer does: whichever comes
            // first adds the slot, so the caller's count always lands.
            register(c, generation: gen)
            return c
        }
        let (user, endpoints, check, key, timing) = (self.user, self.endpoints, self.check, self.key, self.timing)
        let task = Task {
            try await SSHDial.first(endpoints, stagger: timing.stagger) { ep in
                try await SSHConnection.dial(ep, user: user, key: key, check: check, timing: timing)
            }
        }
        dialing = (gen, task)
        defer {
            if dialing?.task == task { dialing = nil }
        }
        // The dial is its own task so a data stream can share it; the caller
        // that started it cancels it when the caller is cancelled.
        let c = try await withTaskCancellationHandler {
            try await task.value
        } onCancel: {
            task.cancel()
        }
        if resets != resetsAtStart {
            // closeAll ran meanwhile.
            c.close()
            throw CancellationError()
        }
        // A sharer that resumed first may have used the connection and seen it
        // close: then it is gone, and its slot with it. Never register it again.
        try Self.requireActive(c)
        register(c, generation: gen)
        return c
    }

    /// Throws when a dialed connection closed before this caller could use it.
    private static func requireActive(_ c: SSHConnection) throws {
        guard c.isActive else { throw TransportError("the SSH connection closed before a channel opened") }
    }

    /// Adds a slot for a new connection, once.
    private func register(_ c: SSHConnection, generation gen: Int) {
        if !slots.contains(where: { $0.conn === c }) {
            slots.append(Slot(conn: c, generation: gen))
        }
    }
}

/// Runs a closure once.
final class Once: Sendable {
    private let f: Mutex<(@Sendable () -> Void)?>
    init(_ f: @escaping @Sendable () -> Void) { self.f = Mutex(f) }
    func run() {
        let g = f.withLock { f in
            defer { f = nil }
            return f
        }
        g?()
    }
}

/// Dials several endpoints in order, each a short time after the previous one
/// (sooner when the previous one failed), and keeps the first that completes.
enum SSHDial {
    static func first(_ endpoints: [SSHEndpoint], stagger: Duration, attempt: @escaping @Sendable (SSHEndpoint) async throws -> SSHConnection) async throws -> SSHConnection {
        guard !endpoints.isEmpty else { throw TransportError("the hab has no endpoint") }
        if endpoints.count == 1 { return try await attempt(endpoints[0]) }
        let race = DialRace(count: endpoints.count)
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (c: CheckedContinuation<SSHConnection, Error>) in
                race.begin(c)
                Task {
                    for (i, ep) in endpoints.enumerated() {
                        if race.isDone { break }
                        let seen = race.failureCount
                        let t = Task {
                            do {
                                let conn = try await attempt(ep)
                                if !race.win(conn) { conn.close() }
                            } catch {
                                race.lose(i, error)
                            }
                        }
                        race.track(t)
                        if i < endpoints.count - 1 {
                            await race.waitFailure(after: seen, orFor: stagger)
                        }
                    }
                }
            }
        } onCancel: {
            race.cancel()
        }
    }
}

final class DialRace: Sendable {
    private struct State {
        var cont: CheckedContinuation<SSHConnection, Error>?
        var errors: [Int: Error] = [:]
        var left: Int
        var done = false
        var tasks: [Task<Void, Never>] = []
        var failureWaiter: CheckedContinuation<Void, Never>?
        var failures = 0
        var waitGen = 0
    }

    private let state: Mutex<State>

    init(count: Int) { state = Mutex(State(left: count)) }

    var isDone: Bool { state.withLock { $0.done } }

    /// Attempts that failed so far.
    var failureCount: Int { state.withLock { $0.failures } }

    func begin(_ c: CheckedContinuation<SSHConnection, Error>) {
        state.withLock { $0.cont = c }
    }

    func track(_ t: Task<Void, Never>) {
        state.withLock { $0.tasks.append(t) }
    }

    /// Returns false when another endpoint already won.
    func win(_ conn: SSHConnection) -> Bool {
        let (c, w, others): (CheckedContinuation<SSHConnection, Error>?, CheckedContinuation<Void, Never>?, [Task<Void, Never>]) = state.withLock { s in
            if s.done { return (nil, nil, []) }
            s.done = true
            let c = s.cont
            s.cont = nil
            let w = s.failureWaiter
            s.failureWaiter = nil
            return (c, w, s.tasks)
        }
        guard let c else { return false }
        w?.resume()
        c.resume(returning: conn)
        for t in others { t.cancel() }
        return true
    }

    func lose(_ i: Int, _ error: Error) {
        let (c, w, err): (CheckedContinuation<SSHConnection, Error>?, CheckedContinuation<Void, Never>?, Error?) = state.withLock { s in
            s.errors[i] = error
            s.left -= 1
            s.failures += 1
            let w = s.failureWaiter
            s.failureWaiter = nil
            if s.left == 0 && !s.done {
                s.done = true
                let c = s.cont
                s.cont = nil
                return (c, w, Self.best(s.errors))
            }
            return (nil, w, nil)
        }
        w?.resume()
        if let c, let err { c.resume(throwing: err) }
    }

    func cancel() {
        let (c, w, ts): (CheckedContinuation<SSHConnection, Error>?, CheckedContinuation<Void, Never>?, [Task<Void, Never>]) = state.withLock { s in
            let w = s.failureWaiter
            s.failureWaiter = nil
            if s.done { return (nil, w, s.tasks) }
            s.done = true
            let c = s.cont
            s.cont = nil
            return (c, w, s.tasks)
        }
        w?.resume()
        for t in ts { t.cancel() }
        c?.resume(throwing: CancellationError())
    }

    /// Returns when more than `seen` attempts have failed, or after `d`,
    /// whichever comes first.
    func waitFailure(after seen: Int, orFor d: Duration) async {
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            let gen: Int? = state.withLock { s in
                if s.done || s.failures > seen { return nil }
                s.waitGen += 1
                s.failureWaiter = c
                return s.waitGen
            }
            guard let gen else {
                c.resume()
                return
            }
            Task {
                try? await Task.sleep(for: d)
                // A sleep from an earlier wait must not end a later one.
                let w: CheckedContinuation<Void, Never>? = self.state.withLock { s in
                    guard s.waitGen == gen else { return nil }
                    let w = s.failureWaiter
                    s.failureWaiter = nil
                    return w
                }
                w?.resume()
            }
        }
    }

    /// A host key or auth refusal says more than a timeout elsewhere;
    /// otherwise the first endpoint's error.
    static func best(_ errors: [Int: Error]) -> Error {
        let ordered = errors.sorted { $0.key < $1.key }.map(\.value)
        if let e = ordered.first(where: { $0 is HostKeyChanged || $0 is SSHKeyRefused || $0 is HostKeyUnconfirmed }) { return e }
        return ordered.first ?? TransportError("no endpoint answered")
    }
}

/// The time from process start to the first byte any hab sent, logged once
/// per process (cold start to first hab reply).
public enum ColdStart {
    private static let first = Mutex<Duration?>(nil)

    /// Process start to the first hab reply, once one arrived.
    public static var firstReply: Duration? { first.withLock { $0 } }

    static func markFirstReply() {
        let d: Duration? = first.withLock { value in
            guard value == nil, let start = processStart() else { return nil }
            let secs = Date().timeIntervalSince(start)
            value = .milliseconds(Int64(secs * 1000))
            return value
        }
        if let d {
            let ms = d.components.seconds * 1000 + d.components.attoseconds / 1_000_000_000_000_000
            AppLog(category: "ssh").notice("cold start to first hab reply: \(ms) ms")
            #if DEBUG
            print("flightdeck: cold start to first hab reply: \(ms) ms")
            #endif
        }
    }

    /// When this process started, from the kernel.
    static func processStart() -> Date? {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()]
        guard sysctl(&mib, u_int(mib.count), &info, &size, nil, 0) == 0 else { return nil }
        let tv = info.kp_proc.p_starttime
        return Date(timeIntervalSince1970: TimeInterval(tv.tv_sec) + TimeInterval(tv.tv_usec) / 1_000_000)
    }
}
