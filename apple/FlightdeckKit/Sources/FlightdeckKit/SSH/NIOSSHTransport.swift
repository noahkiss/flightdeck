import Foundation
import NIOCore
import NIOSSH
import os

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
public final class NIOSSHTransport: Transport, @unchecked Sendable {
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
    private var conns: [SSHConnection] = []
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
        conns.filter(\.isActive).count
    }

    func open(_ kind: StreamKind, command: String) async throws -> ByteStream {
        if kind == .control {
            generation += 1
            retireOld()
        }
        var rejected = 0
        while true {
            let conn = try await connection(fresh: kind == .control)
            conn.children += 1
            // Strong: the pool lives while any of its channels does.
            let once = Once { [self] in
                Task { await self.childEnded(conn) }
            }
            do {
                return try await conn.openSession(command: command, onEnd: { once.run() }, onFirstData: { ColdStart.markFirstReply() })
            } catch let e as NIOSSHError where e.type == .channelSetupRejected && rejected == 0 && kind == .data {
                // The host's session limit: one more connection for the next channels.
                rejected += 1
                conn.full = true
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
        for c in conns { c.close() }
        conns.removeAll()
    }

    private func childEnded(_ c: SSHConnection) {
        c.children = max(0, c.children - 1)
        if c.children == 0 {
            // No channel left: close it. The next stream dials again, so
            // nothing stays open in the background.
            c.close()
            conns.removeAll { $0 === c }
        }
    }

    /// Closes older connections that carry no channel.
    private func retireOld() {
        for c in conns where c.generation != generation && c.children == 0 {
            c.close()
        }
        conns.removeAll { !$0.isActive || ($0.generation != generation && $0.children == 0) }
    }

    /// A connection of the current generation with room, else a new one.
    private func connection(fresh: Bool) async throws -> SSHConnection {
        conns.removeAll { !$0.isActive && $0.children == 0 }
        if !fresh, let c = conns.last(where: { $0.generation == generation && !$0.full && $0.isActive }) {
            return c
        }
        let gen = generation
        let resetsAtStart = resets
        if !fresh, let d = dialing, d.gen == gen {
            return try await d.task.value
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
        let c = try await task.value
        c.generation = gen
        if resets != resetsAtStart {
            // closeAll ran meanwhile.
            c.close()
            throw CancellationError()
        }
        conns.append(c)
        return c
    }
}

/// Runs a closure once.
final class Once: @unchecked Sendable {
    private let lock = NSLock()
    private var f: (() -> Void)?
    init(_ f: @escaping () -> Void) { self.f = f }
    func run() {
        let g: (() -> Void)? = lock.withLock {
            let g = f
            f = nil
            return g
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
                            await race.waitFailure(orFor: stagger)
                        }
                    }
                }
            }
        } onCancel: {
            race.cancel()
        }
    }
}

final class DialRace: @unchecked Sendable {
    private let lock = NSLock()
    private var cont: CheckedContinuation<SSHConnection, Error>?
    private var errors: [Int: Error] = [:]
    private var left: Int
    private var done = false
    private var tasks: [Task<Void, Never>] = []
    private var failureWaiter: CheckedContinuation<Void, Never>?

    init(count: Int) { left = count }

    var isDone: Bool { lock.withLock { done } }

    func begin(_ c: CheckedContinuation<SSHConnection, Error>) {
        lock.withLock { cont = c }
    }

    func track(_ t: Task<Void, Never>) {
        lock.withLock { tasks.append(t) }
    }

    /// Returns false when another endpoint already won.
    func win(_ conn: SSHConnection) -> Bool {
        let (c, w, others): (CheckedContinuation<SSHConnection, Error>?, CheckedContinuation<Void, Never>?, [Task<Void, Never>]) = lock.withLock {
            if done { return (nil, nil, []) }
            done = true
            let c = cont
            cont = nil
            let w = failureWaiter
            failureWaiter = nil
            return (c, w, tasks)
        }
        guard let c else { return false }
        w?.resume()
        c.resume(returning: conn)
        for t in others { t.cancel() }
        return true
    }

    func lose(_ i: Int, _ error: Error) {
        let (c, w, err): (CheckedContinuation<SSHConnection, Error>?, CheckedContinuation<Void, Never>?, Error?) = lock.withLock {
            errors[i] = error
            left -= 1
            let w = failureWaiter
            failureWaiter = nil
            if left == 0 && !done {
                done = true
                let c = cont
                cont = nil
                return (c, w, Self.best(errors))
            }
            return (nil, w, nil)
        }
        w?.resume()
        if let c, let err { c.resume(throwing: err) }
    }

    func cancel() {
        let (c, w, ts): (CheckedContinuation<SSHConnection, Error>?, CheckedContinuation<Void, Never>?, [Task<Void, Never>]) = lock.withLock {
            let w = failureWaiter
            failureWaiter = nil
            if done { return (nil, w, tasks) }
            done = true
            let c = cont
            cont = nil
            return (c, w, tasks)
        }
        w?.resume()
        for t in ts { t.cancel() }
        c?.resume(throwing: CancellationError())
    }

    /// Returns when an attempt fails or after `d`, whichever comes first.
    func waitFailure(orFor d: Duration) async {
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            let resumeNow: Bool = lock.withLock {
                if done { return true }
                failureWaiter = c
                return false
            }
            if resumeNow {
                c.resume()
                return
            }
            Task {
                try? await Task.sleep(for: d)
                let w: CheckedContinuation<Void, Never>? = self.lock.withLock {
                    let w = self.failureWaiter
                    self.failureWaiter = nil
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
/// per process (task-iv89iwd8: cold start to first hab reply).
public enum ColdStart {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var value: Duration?

    /// Process start to the first hab reply, once one arrived.
    public static var firstReply: Duration? { lock.withLock { value } }

    static func markFirstReply() {
        let d: Duration? = lock.withLock {
            guard value == nil, let start = processStart() else { return nil }
            let secs = Date().timeIntervalSince(start)
            value = .milliseconds(Int64(secs * 1000))
            return value
        }
        if let d {
            let ms = d.components.seconds * 1000 + d.components.attoseconds / 1_000_000_000_000_000
            Logger(subsystem: "com.flightdeck.app", category: "ssh").notice("cold start to first hab reply: \(ms) ms")
            print("flightdeck: cold start to first hab reply: \(ms) ms")
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
