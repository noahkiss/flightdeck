import FlightdeckShared
import Foundation
import HabAPI
import os
#if os(iOS)
import ActivityKit
#endif

/// Which session the Live Activity tracks (app.md 9.1): the one the owner
/// last sent a message to from this device, or the one that last needed
/// them, whichever came later.
public struct ActivityPick: Sendable, Equatable {
    public var sent: (target: SessionTarget, at: Date)?
    public var needed: (target: SessionTarget, at: Date)?

    public init() {}

    public static func == (a: ActivityPick, b: ActivityPick) -> Bool {
        a.sent?.target == b.sent?.target && a.sent?.at == b.sent?.at && a.needed?.target == b.needed?.target && a.needed?.at == b.needed?.at
    }

    public var tracked: SessionTarget? {
        switch (sent, needed) {
        case let (s?, n?): return n.at > s.at ? n.target : s.target
        case let (s?, nil): return s.target
        case let (nil, n?): return n.target
        default: return nil
        }
    }

    /// Whether to start an activity for the tracked session when the app
    /// leaves the screen. Only a hab with push can keep it current; without
    /// push nothing would update it once the app is suspended, so none starts
    /// (app.md 9.2, push not set up).
    public static func shouldStart(row: BoardRow?, pushRegistered: Bool, running: Bool) -> Bool {
        guard let row, !running, pushRegistered, !row.offline, row.target.session != nil else { return false }
        return row.state == .working || row.state == .blocked
    }
}

/// What the tracker needs from ActivityKit, so its rules run in tests on the
/// Mac. `SystemActivities` is the iOS implementation.
@MainActor
public protocol ActivityHost: AnyObject {
    var enabled: Bool { get }
    /// Sessions with an activity that has not ended.
    func running() -> [String]
    /// Starts an activity and returns its push tokens as they come.
    func start(_ attributes: SessionActivityAttributes, _ state: SessionActivityAttributes.ContentState) throws -> AsyncStream<Data>
    func update(session: String, _ state: SessionActivityAttributes.ContentState) async
    func end(session: String) async
}

/// Starts and ends the session's Live Activity and gives its push token to
/// the session's hab with `push.register_activity`; that hab alone updates
/// it (app.md 9.1, 18.1). iPhone and iPad only.
///
/// ActivityKit starts an activity only while the app is in front, so `start`
/// runs when the scene turns inactive on its way out, not once it is in the
/// background.
@MainActor
public final class LiveActivityTracker {
    private let board: Board
    private let push: PushRegistrar
    private let caller: ApprovalAnswerer
    private let host: ActivityHost?
    public private(set) var pick = ActivityPick()
    private var blocked: Set<String> = []
    /// Per running activity: its hab's host id, its push token, its last content.
    private var live: [String: (host: String, token: String?, state: SessionActivityAttributes.ContentState)] = [:]
    private var tokenTasks: [String: Task<Void, Never>] = [:]
    private let log = Logger(subsystem: "com.flightdeck.app", category: "activity")

    public init(board: Board, push: PushRegistrar, caller: ApprovalAnswerer, host: ActivityHost?) {
        self.board = board
        self.push = push
        self.caller = caller
        self.host = host
    }

    /// A message from this device reached the hab.
    public func noteSent(_ t: SessionTarget, at: Date = Date()) {
        pick.sent = (t, at)
    }

    /// A session newly entered `blocked`: it needs the owner.
    public func boardChanged(at: Date = Date()) {
        let rows = board.rows.filter { $0.state == .blocked && $0.target.session != nil }
        let now = Set(rows.map(\.id))
        if let r = rows.first(where: { !blocked.contains($0.id) }) {
            pick.needed = (r.target, at)
        }
        blocked = now
    }

    /// The app is leaving the screen (scene inactive): start the tracked
    /// session's activity while ActivityKit still allows it. One activity at
    /// a time (9.1): any other is ended first.
    public func start() async {
        guard let host, host.enabled, let t = pick.tracked, let session = t.session else { return }
        let row = board.rows.first { $0.target == t }
        guard ActivityPick.shouldStart(row: row, pushRegistered: push.status[t.habID] == .registered, running: host.running().contains(session)),
              let row, let hab = board.habs.first(where: { $0.id == t.habID }),
              let habHost = hab.state.welcome?.host.id
        else { return }
        for other in host.running() where other != session {
            await end(session: other)
        }
        let attrs = SessionActivityAttributes(session: session, name: row.title, project: row.project, hab: habHost, habLabel: hab.label)
        let state = Self.initialState(row: row, request: OpenRequest.collect(board.habs).first { $0.habID == t.habID && $0.pane == t.pane }, host: habHost)
        do {
            let tokens = try host.start(attrs, state)
            live[session] = (habHost, nil, state)
            log.notice("live activity started")
            let caller = self.caller
            let device = push.deviceID
            tokenTasks[session] = Task { @MainActor [weak self] in
                for await data in tokens {
                    let token = data.map { String(format: "%02x", $0) }.joined()
                    self?.live[session]?.token = token
                    _ = try? await caller.call(host: habHost, HabMethods.pushRegisterActivity, PushActivityParams(deviceId: device, session: session, token: token))
                }
            }
        } catch {
            log.notice("live activity not started: \(String(describing: error), privacy: .private)")
        }
    }

    /// The first content: an open permission the app can answer shows its
    /// title and command with Allow and Deny at once, since the hab sends no
    /// update until the state changes again.
    static func initialState(row: BoardRow, request: OpenRequest?, host: String) -> SessionActivityAttributes.ContentState {
        let since = row.since.map(Fixture.stamp) ?? Fixture.stamp(Date())
        var s = SessionActivityAttributes.ContentState(
            state: row.state?.rawValue ?? "working", kind: row.blockedKind?.rawValue, title: row.title, detail: row.context,
            pane: row.target.pane, buttons: false, since: since, hab: host, harness: row.harness)
        if let r = request, r.answerable, row.state == .blocked {
            s.request = r.requestID
            s.buttons = true
            s.title = r.title
        }
        return s
    }

    /// An answer from the activity landed or was refused: its buttons go now,
    /// without waiting for the hab's push (app.md 7.5).
    public func answered(pane: String, request: String, outcome: AnswerOutcome) async {
        switch outcome {
        case .answered, .refused, .probablyAnswered: break
        default: return
        }
        guard let host else { return }
        for (session, a) in live where a.state.pane == pane && a.state.request == request {
            var s = a.state
            s.buttons = false
            s.request = nil
            live[session]?.state = s
            await host.update(session: session, s)
        }
    }

    /// The owner opened a session in the app: its activity ends (app.md 9.1).
    public func opened(_ t: SessionTarget) async {
        guard let session = t.session, let host, host.running().contains(session) else { return }
        await end(session: session)
    }

    /// Ends an activity and tells its hab to drop the token, so the hab stops
    /// pushing to an activity that is gone.
    private func end(session: String) async {
        tokenTasks.removeValue(forKey: session)?.cancel()
        let a = live.removeValue(forKey: session)
        await host?.end(session: session)
        if let a, let token = a.token {
            _ = try? await caller.call(host: a.host, HabMethods.pushUnregister, PushUnregisterParams(deviceId: push.deviceID, token: token))
        }
    }
}

#if os(iOS)
/// ActivityKit behind `ActivityHost`.
@MainActor
public final class SystemActivities: ActivityHost {
    public init() {}

    public var enabled: Bool { ActivityAuthorizationInfo().areActivitiesEnabled }

    private var all: [Activity<SessionActivityAttributes>] {
        Activity<SessionActivityAttributes>.activities.filter { $0.activityState == .active || $0.activityState == .stale }
    }

    public func running() -> [String] { all.map(\.attributes.session) }

    public func start(_ attributes: SessionActivityAttributes, _ state: SessionActivityAttributes.ContentState) throws -> AsyncStream<Data> {
        let a = try Activity.request(attributes: attributes, content: .init(state: state, staleDate: nil), pushType: .token)
        return AsyncStream { c in
            let t = Task {
                for await d in a.pushTokenUpdates { c.yield(d) }
                c.finish()
            }
            c.onTermination = { _ in t.cancel() }
        }
    }

    public func update(session: String, _ state: SessionActivityAttributes.ContentState) async {
        for a in all where a.attributes.session == session {
            await a.update(.init(state: state, staleDate: nil))
        }
    }

    public func end(session: String) async {
        for a in all where a.attributes.session == session {
            await a.end(nil, dismissalPolicy: .immediate)
        }
    }
}
#endif
