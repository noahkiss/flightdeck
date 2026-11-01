import Foundation
import HabAPI

/// Keeps each hab told which of its sessions is on screen (app.md 8.6,
/// hab-logic-api.md 3.9 Push, Presence tiers): `session.focus` with `active`
/// true when a session view shows in an active window, again every
/// `interval` while it stays, and `active` false when it leaves or the window
/// goes inactive. The hab holds banners for a session focused within 60 s and
/// sends none to a device in the foreground within 180 s.
///
/// The interval is 25 s, under the hab's "at least every 30 s", so a late
/// timer or a slow call never lets the 60 s hold lapse while the owner looks.
@MainActor
public final class FocusReporter {
    public typealias Send = @MainActor (_ habID: String, _ params: SessionFocusParams) async -> Void

    private let send: Send
    private let interval: Duration
    /// Views showing each session; a Mac may show one session in two windows.
    private var shown: [Key: Int] = [:]
    private var active = false
    /// Sessions last reported `active` true.
    private var reported: Set<Key> = []
    private var ticker: Task<Void, Never>?
    /// Calls run in order, so a false never overtakes the true before it.
    private var queue: Task<Void, Never>?

    struct Key: Hashable {
        var hab: String
        var session: String
    }

    public init(interval: Duration = .seconds(25), send: @escaping Send) {
        self.interval = interval
        self.send = send
    }

    /// A session view appeared.
    public func show(_ target: SessionTarget) {
        guard let s = target.session else { return }
        shown[Key(hab: target.habID, session: s), default: 0] += 1
        converge()
    }

    /// A session view went away.
    public func hide(_ target: SessionTarget) {
        guard let s = target.session else { return }
        let k = Key(hab: target.habID, session: s)
        guard let n = shown[k] else { return }
        shown[k] = n > 1 ? n - 1 : nil
        converge()
    }

    /// The window (iPhone, iPad: the scene; Mac: any window) became active or
    /// stopped being active.
    public func setActive(_ on: Bool) {
        guard on != active else { return }
        active = on
        converge()
    }

    /// A hab connected again: its new client knows no focus yet.
    public func connectionOnline(habID: String) {
        let keys = reported.filter { $0.hab == habID }
        guard !keys.isEmpty else { return }
        enqueue(keys.map { ($0, true) })
    }

    /// Waits for the calls queued so far; for tests and for going to the
    /// background after the last false went out.
    public func flush() async {
        await queue?.value
    }

    private var wanted: Set<Key> {
        active ? Set(shown.keys) : []
    }

    private func converge() {
        let want = wanted
        let gone = reported.subtracting(want)
        let new = want.subtracting(reported)
        reported = want
        var calls: [(Key, Bool)] = []
        for k in gone.sorted(by: Self.order) { calls.append((k, false)) }
        for k in new.sorted(by: Self.order) { calls.append((k, true)) }
        if !calls.isEmpty { enqueue(calls) }
        if want.isEmpty {
            ticker?.cancel()
            ticker = nil
        } else if ticker == nil || !new.isEmpty {
            // Restart the period from the last true sent.
            ticker?.cancel()
            ticker = Task { @MainActor [weak self, interval] in
                while !Task.isCancelled {
                    try? await Task.sleep(for: interval)
                    guard !Task.isCancelled, let self else { return }
                    self.enqueue(self.reported.sorted(by: Self.order).map { ($0, true) })
                }
            }
        }
    }

    private func enqueue(_ calls: [(Key, Bool)]) {
        let previous = queue
        let send = self.send
        queue = Task { @MainActor in
            await previous?.value
            for (k, on) in calls {
                await send(k.hab, SessionFocusParams(active: on, session: k.session))
            }
        }
    }

    private static func order(_ a: Key, _ b: Key) -> Bool {
        (a.hab, a.session) < (b.hab, b.session)
    }
}
