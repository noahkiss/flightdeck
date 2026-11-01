import Foundation
import HabAPI

/// Which side must update when the API revisions do not overlap.
public enum UpdateSide: String, Sendable, Equatable {
    case app
    case hab
}

public enum HabStatus: Sendable, Equatable {
    /// Not connected on purpose: not started, or the app is in the background.
    case stopped
    /// Connecting, or reconnecting after a loss, with fewer than
    /// `HabTiming.offlineAfter` failures in a row.
    case connecting
    /// A live control connection with its snapshot applied.
    case online
    /// `HabTiming.offlineAfter` attempts in a row failed. Retries go on.
    case offline
    /// The API revisions do not overlap. No retry until the next `start`.
    case updateNeeded(UpdateSide)
}

/// One `chat.message` event: the message, its seq, and which connection
/// (generation) carried it. Seqs compare only within one generation.
public struct ChatUpdate: Sendable {
    public var message: ChatMessage
    public var seq: UInt64
    public var generation: Int
}

/// Everything the board needs from one hab. Published whole on each change.
public struct HabState: Sendable, Equatable {
    public var status: HabStatus = .stopped
    public var store = HabStore()
    /// The last welcome; nil before the first.
    public var welcome: Welcome?
    /// Why the last attempt or connection failed, for the connections list.
    public var lastError: String?
    /// When the hab was last seen online, or when the first attempt failed.
    /// The board shows it as "offline since".
    public var lostAt: Date?
    /// How many times a welcome brought a new epoch after an earlier one.
    public var epochChanges = 0
    /// Whether the last welcome kept the earlier client id (the token was taken).
    public var resumed = false
    /// How many snapshots were applied, over all connections.
    public var snapshots = 0
}

/// Timings of the control connection. The defaults follow hab-logic-api.md 3.5
/// and app.md 10.2; tests shorten them.
public struct HabTiming: Sendable {
    /// Send `hab.ping` when nothing was sent for this long.
    public var pingInterval: Duration = .seconds(15)
    /// Treat the connection as dead when nothing arrived for this long.
    public var deadAfter: Duration = .seconds(45)
    /// The second retry waits about this long; each later one doubles it.
    public var minBackoff: Duration = .milliseconds(500)
    /// Retries are at most this far apart, before jitter.
    public var maxBackoff: Duration = .seconds(30)
    /// The hello's timeout, unless the transport names its own (ssh needs longer).
    public var helloTimeout: Duration = .seconds(10)
    public var callTimeout: Duration = .seconds(30)
    /// Failures in a row before the hab shows offline.
    public var offlineAfter = 3
    /// How often the liveness check runs.
    public var tick: Duration = .seconds(1)
    /// A connection that stayed online this long ended in success: the next
    /// retry goes at once. A shorter one counts as a failure and backs off, so
    /// a hab that drops every connection after the snapshot is not hammered.
    public var stableAfter: Duration = .seconds(10)

    public init() {}
}

/// The control connection to one hab: hello, subscribe with scope host, the
/// snapshot, then events by the seq rule; heartbeat and ping; reconnect with
/// the token and backoff.
///
/// There is no event replay (hab-logic-api.md Q5). A reconnect sends the
/// token, subscribes again, applies the snapshot, then only events with a seq
/// above the snapshot's.
public actor HabConnection {
    public nonisolated let id: String
    public nonisolated let transport: Transport
    /// Every state change, newest only: a slow reader skips to the latest.
    public nonisolated let updates: AsyncStream<HabState>
    private let updatesSink: AsyncStream<HabState>.Continuation

    private let timing: HabTiming
    private let clientLabel: String?
    private let clientVersion: String

    public private(set) var state = HabState()

    private var token: String?
    private var clientID: String?
    private var epoch: String?

    private var runTask: Task<Void, Never>?
    private var rpc: RPCClient?
    private var lastSeq: UInt64 = 0
    private var snapshotApplied = false
    private var buffered: [Event] = []
    private var sessionEnd: CheckedContinuation<Void, Error>?
    private var sessionEndError: Error?
    private var reachedOnline = false
    private var onlineSince: ContinuousClock.Instant?
    private var wake: CheckedContinuation<Void, Never>?
    private var wakeGen = 0
    /// Listeners for `chat.message` events (app.md 4.1), by listener id.
    private var chatTaps: [Int: AsyncStream<ChatUpdate>.Continuation] = [:]
    private var nextTap = 1
    /// Counts live connections, so a chat listener can tell its seq apart.
    private var generation = 0

    public init(id: String, transport: Transport, timing: HabTiming = HabTiming(), clientLabel: String? = nil, clientVersion: String = "0") {
        self.id = id
        self.transport = transport
        self.timing = timing
        self.clientLabel = clientLabel
        self.clientVersion = clientVersion
        (updates, updatesSink) = AsyncStream.makeStream(of: HabState.self, bufferingPolicy: .bufferingNewest(1))
    }

    /// Starts connecting, and keeps reconnecting until `stop`. Does nothing
    /// while already running.
    public func start() {
        guard runTask == nil else { return }
        if case .updateNeeded = state.status {} else if state.status != .online {
            state.status = .connecting
        }
        publish()
        runTask = Task { await self.run() }
    }

    /// Closes the connection and stops retrying. Returns once the stream is
    /// closed. Objects stay, so the board keeps its last rows.
    public func stop() async {
        runTask?.cancel()
        runTask = nil
        let r = rpc
        if let r {
            endSession(r, CancellationError())
            rpc = nil
        }
        fireWake(nil)
        if state.status == .online { state.lostAt = Date() }
        state.status = .stopped
        publish()
        await r?.close()
    }

    /// Cuts a retry wait short, for a network change or a return to the foreground.
    public func retryNow() {
        fireWake(nil)
    }

    /// Sends a request on the live connection.
    public func call<P, R>(_ method: HabMethod<P, R>, _ params: P) async throws -> R {
        guard let r = rpc, state.status == .online else {
            throw ConnectionClosed(detail: "hab \(id) is not online")
        }
        return try await r.call(method, params, timeout: timing.callTimeout)
    }

    /// `chat.message` events of this hab, in seq order, from now until the
    /// stream is dropped. Every hab session's messages come; the listener filters.
    public func chatUpdates() -> AsyncStream<ChatUpdate> {
        let (stream, sink) = AsyncStream.makeStream(of: ChatUpdate.self)
        let id = nextTap
        nextTap += 1
        chatTaps[id] = sink
        sink.onTermination = { [weak self] _ in
            Task { await self?.dropTap(id) }
        }
        return stream
    }

    /// Calls a method on the live connection and says which connection
    /// answered, so a result's seq can be matched with `chatUpdates`.
    public func callTagged<P, R>(_ method: HabMethod<P, R>, _ params: P) async throws -> (R, Int) {
        let g = generation
        let r = try await call(method, params)
        return (r, g)
    }

    private func dropTap(_ id: Int) {
        chatTaps[id] = nil
    }

    // MARK: - Run loop

    private struct Permanent: Error {
        var side: UpdateSide
        var hint: String
    }

    private func run() async {
        var failures = 0
        while !Task.isCancelled {
            if failures > 0 {
                await sleep(Self.backoff(attempt: failures, lo: timing.minBackoff, hi: timing.maxBackoff))
                if Task.isCancelled { break }
            }
            reachedOnline = false
            onlineSince = nil
            do {
                try await runSession()
            } catch let p as Permanent {
                if Task.isCancelled { break }
                state.status = .updateNeeded(p.side)
                state.lastError = p.hint
                publish()
                runTask = nil
                return
            } catch {
                if Task.isCancelled { break }
                state.lastError = Self.describe(error)
            }
            if Task.isCancelled { break }
            if reachedOnline {
                state.lostAt = Date()
                let stable = onlineSince.map { ContinuousClock.now - $0 >= timing.stableAfter } ?? false
                if stable {
                    // A long-lived connection ended: the first retry goes at once.
                    failures = 0
                    state.status = .connecting
                } else {
                    failures += 1
                    state.status = failures >= timing.offlineAfter ? .offline : .connecting
                }
            } else {
                failures += 1
                if state.lostAt == nil { state.lostAt = Date() }
                if failures >= timing.offlineAfter { state.status = .offline }
            }
            publish()
        }
    }

    private func runSession() async throws {
        let stream = try await transport.open(.control)
        if Task.isCancelled {
            stream.close()
            throw CancellationError()
        }
        let r = RPCClient(stream: stream)
        rpc = r
        generation += 1
        lastSeq = 0
        snapshotApplied = false
        buffered = []
        sessionEnd = nil
        sessionEndError = nil
        await r.start()
        let inbound = Task {
            for await n in r.inbound {
                await self.handle(n, from: r)
            }
            // Keep the client's own reason (a read error, an over-long message) when it has one.
            let why = await r.endError ?? ConnectionClosed(detail: stream.diagnostics)
            await self.endSession(r, why)
        }
        var liveness: Task<Void, Never>?
        defer {
            inbound.cancel()
            liveness?.cancel()
            if rpc === r { rpc = nil }
            Task { await r.close() }
        }

        let welcome = try await hello(r)
        let newEpoch = epoch != nil && epoch != welcome.epoch
        state.resumed = clientID != nil && welcome.clientId == clientID && !newEpoch
        token = welcome.reconnectToken
        clientID = welcome.clientId
        if newEpoch { state.epochChanges += 1 }
        epoch = welcome.epoch
        state.welcome = welcome
        liveness = Task { await self.livenessLoop(r) }

        let snap = try await r.call(HabMethods.eventsSubscribe, SubscribeParams(scope: Scope(kind: .host)), timeout: timing.callTimeout)
        guard rpc === r else { throw CancellationError() }
        applySnapshot(snap, from: r)
        state.status = .online
        state.lastError = nil
        reachedOnline = true
        onlineSince = .now
        publish()

        try await waitSessionEnd()
    }

    private func hello(_ r: RPCClient) async throws -> Welcome {
        let params = HelloParams(
            client: ClientInfo(kind: .app, name: "Flightdeck", version: clientVersion),
            label: clientLabel,
            minRevision: HabAPI.minRevision,
            reconnectToken: token,
            revision: HabAPI.revision
        )
        let w: Welcome
        do {
            w = try await r.call(HabMethods.habHello, params, timeout: transport.helloTimeout ?? timing.helloTimeout)
        } catch let e as RPCError where e.kind == .revisionUnsupported {
            let side: UpdateSide = (e.data?.server.map { $0.max < HabAPI.minRevision } ?? false) ? .hab : .app
            throw Permanent(side: side, hint: e.data?.hint ?? e.message)
        }
        if w.revision < HabAPI.minRevision {
            throw Permanent(side: .hab, hint: "the hab speaks API revision \(w.revision); this app needs \(HabAPI.minRevision) or later")
        }
        if w.revision > HabAPI.revision {
            throw Permanent(side: .app, hint: "the hab chose API revision \(w.revision); this app speaks up to \(HabAPI.revision)")
        }
        return w
    }

    private func applySnapshot(_ snap: StructureResult, from r: RPCClient) {
        // Every snapshot replaces this hab's objects. Within an epoch it is
        // newer than anything held; with a new epoch revisions may restart lower.
        state.store.reset(snap)
        state.snapshots += 1
        lastSeq = snap.seq
        snapshotApplied = true
        let waiting = buffered.sorted { $0.seq < $1.seq }
        buffered = []
        for e in waiting {
            applyEvent(e, from: r)
        }
    }

    // MARK: - Notifications

    private struct SeqGap: Error, CustomStringConvertible {
        var want: UInt64
        var got: UInt64
        var description: String { "event seq gap: expected \(want), got \(got)" }
    }

    private struct Dead: Error, CustomStringConvertible {
        var description: String { "no message from the hab; connection presumed dead" }
    }

    private struct HabClosed: Error, CustomStringConvertible {
        var reason: String
        var description: String { "the hab closed the connection: \(reason)" }
    }

    private func handle(_ n: Inbound, from r: RPCClient) {
        guard rpc === r else { return }
        switch n.method {
        case HabNotification.event:
            guard let e = try? n.params(Event.self) else { return }
            if snapshotApplied {
                applyEvent(e, from: r)
            } else {
                buffered.append(e)
            }
        case HabNotification.heartbeat:
            guard snapshotApplied, let h = try? n.params(Heartbeat.self) else { return }
            if h.seq > lastSeq {
                endSession(r, SeqGap(want: lastSeq + 1, got: h.seq))
            }
        case HabNotification.closing:
            let reason = (try? n.params(Closing.self))?.reason.rawValue ?? "unknown"
            endSession(r, HabClosed(reason: reason))
        default:
            break
        }
    }

    private func applyEvent(_ e: Event, from r: RPCClient) {
        if e.seq <= lastSeq { return }
        if e.seq != lastSeq + 1 {
            // A gap is a bug on the hab's side; handle it as a backlog close.
            endSession(r, SeqGap(want: lastSeq + 1, got: e.seq))
            return
        }
        lastSeq = e.seq
        if e.type == "chat.message", !chatTaps.isEmpty, let m = try? e.data.decode(ChatMessage.self) {
            let u = ChatUpdate(message: m, seq: e.seq, generation: generation)
            for (_, t) in chatTaps { t.yield(u) }
        }
        if state.store.apply(e) { publish() }
    }

    private func livenessLoop(_ r: RPCClient) async {
        while !Task.isCancelled {
            try? await Task.sleep(for: timing.tick)
            guard !Task.isCancelled, rpc === r else { return }
            let now = ContinuousClock.now
            if now - (await r.lastReceived) > timing.deadAfter {
                endSession(r, Dead())
                return
            }
            if now - (await r.lastSent) >= timing.pingInterval {
                let t = timing.callTimeout
                Task { _ = try? await r.call(HabMethods.habPing, PingParams(), timeout: t) }
            }
        }
    }

    // MARK: - Session end and waits

    private func endSession(_ r: RPCClient, _ error: Error) {
        guard rpc === r else { return }
        if let c = sessionEnd {
            sessionEnd = nil
            c.resume(throwing: error)
        } else if sessionEndError == nil {
            sessionEndError = error
        }
        Task { await r.close() }
    }

    private func waitSessionEnd() async throws {
        if let e = sessionEndError { throw e }
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
            sessionEnd = c
        }
    }

    private func sleep(_ d: Duration) async {
        wakeGen += 1
        let gen = wakeGen
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            wake = c
            Task {
                try? await Task.sleep(for: d)
                await self.fireWake(gen)
            }
        }
    }

    private func fireWake(_ gen: Int?) {
        if let gen, gen != wakeGen { return }
        if let c = wake {
            wake = nil
            c.resume()
        }
    }

    private func publish() {
        updatesSink.yield(state)
    }

    /// The wait before retry `attempt` (1 or more): `lo` doubling up to `hi`,
    /// then a random point in its upper half, so many clients do not retry in step.
    static func backoff(attempt: Int, lo: Duration, hi: Duration) -> Duration {
        var d = lo
        var i = 1
        while i < attempt && d < hi {
            d = d * 2
            i += 1
        }
        if d > hi { d = hi }
        let half = d / 2
        let ns = max(Int64(1), Int64(half.components.seconds) * 1_000_000_000 + half.components.attoseconds / 1_000_000_000)
        return half + .nanoseconds(Int64.random(in: 0...ns))
    }

    static func describe(_ error: Error) -> String {
        String(describing: error)
    }
}
