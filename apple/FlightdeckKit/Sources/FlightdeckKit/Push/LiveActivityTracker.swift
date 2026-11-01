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

/// Starts and ends the session's Live Activity and gives its push token to
/// the session's hab with `push.register_activity`; that hab alone updates
/// it (app.md 9.1, 18.1). iPhone and iPad only.
@MainActor
public final class LiveActivityTracker {
    private let board: Board
    private let push: PushRegistrar
    private let caller: ApprovalAnswerer
    public private(set) var pick = ActivityPick()
    private var blocked: Set<String> = []
    private let log = Logger(subsystem: "com.flightdeck.app", category: "activity")

    public init(board: Board, push: PushRegistrar, caller: ApprovalAnswerer) {
        self.board = board
        self.push = push
        self.caller = caller
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

    /// The app left the screen: start the tracked session's activity.
    public func background() {
        #if os(iOS)
        guard let t = pick.tracked, let session = t.session else { return }
        let row = board.rows.first { $0.target == t }
        let running = Activity<SessionActivityAttributes>.activities.contains { $0.attributes.session == session && $0.activityState == .active }
        guard ActivityPick.shouldStart(row: row, pushRegistered: push.status[t.habID] == .registered, running: running),
              let row, ActivityAuthorizationInfo().areActivitiesEnabled,
              let hab = board.habs.first(where: { $0.id == t.habID }),
              let host = hab.state.welcome?.host.id
        else { return }
        let attrs = SessionActivityAttributes(session: session, name: row.title, project: row.project, hab: host, habLabel: hab.label)
        let state = SessionActivityAttributes.ContentState(
            state: row.state?.rawValue ?? "working", kind: row.blockedKind?.rawValue, title: row.title, detail: row.context,
            pane: t.pane, buttons: false, since: row.since.map(Fixture.stamp) ?? Fixture.stamp(Date()), hab: host, harness: row.harness)
        do {
            let a = try Activity.request(attributes: attrs, content: .init(state: state, staleDate: nil), pushType: .token)
            log.notice("live activity started")
            let caller = self.caller
            let device = push.deviceID
            Task.detached {
                for await data in a.pushTokenUpdates {
                    let token = data.map { String(format: "%02x", $0) }.joined()
                    // The app is on its way to the background: keep it running
                    // until the hab has the token.
                    await withExpiringActivity("register the Live Activity token") {
                        _ = try? await caller.call(host: host, HabMethods.pushRegisterActivity, PushActivityParams(deviceId: device, session: session, token: token))
                    }
                }
            }
        } catch {
            log.notice("live activity not started: \(String(describing: error), privacy: .public)")
        }
        #endif
    }

    /// The owner opened a session in the app: its activity ends (app.md 9.1).
    public func opened(_ t: SessionTarget) {
        #if os(iOS)
        guard let session = t.session else { return }
        for a in Activity<SessionActivityAttributes>.activities where a.attributes.session == session {
            Task { await a.end(nil, dismissalPolicy: .immediate) }
        }
        #endif
    }
}

#if os(iOS)
/// Runs `work` while asking iOS for time to finish it in the background.
private func withExpiringActivity(_ reason: String, _ work: @escaping @Sendable () async -> Void) async {
    let once = ResumeOnce()
    await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
        once.set(c)
        ProcessInfo.processInfo.performExpiringActivity(withReason: reason) { expired in
            if expired {
                // Out of time (or none given): stop waiting; the call may still land.
                once.resume()
                return
            }
            let done = DispatchSemaphore(value: 0)
            Task {
                await work()
                done.signal()
            }
            done.wait()
            once.resume()
        }
    }
}

private final class ResumeOnce: @unchecked Sendable {
    private let lock = NSLock()
    private var c: CheckedContinuation<Void, Never>?
    func set(_ c: CheckedContinuation<Void, Never>) { lock.withLock { self.c = c } }
    func resume() {
        let c: CheckedContinuation<Void, Never>? = lock.withLock {
            defer { self.c = nil }
            return self.c
        }
        c?.resume()
    }
}
#endif
