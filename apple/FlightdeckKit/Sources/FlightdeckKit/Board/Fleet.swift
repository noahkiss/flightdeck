import Foundation
import HabAPI
import os

/// Every hab connection and the board they feed, plus the lifecycle rule:
/// connected while visible, disconnected in the background (app.md 12.1).
///
/// - iPhone and iPad: visible while the scene is active.
/// - Mac: visible while any window is open.
@MainActor
public final class Fleet {
    public let board: Board
    public private(set) var connections: [HabConnection] = []
    /// Demo or test habs kept alive with the fleet.
    public var keepAlive: [AnyObject] = []
    public private(set) var isActive = false
    /// Open terminal views; their data connections follow the lifecycle too.
    public let terminals = TerminalRegistry()
    /// The in-app SSH carrier's keys and hab entries; nil in demo mode.
    public var ssh: SSHSetup?
    /// Push registration with every hab (app.md 8.1); nil in demo mode.
    public var push: PushRegistrar?
    /// The approval sheet and answers from outside the app (app.md 7).
    public var approvals: ApprovalCenter?
    /// Host id to connection, for answers from the background.
    public var book: HabBook?
    /// The session's Live Activity (app.md 9); iPhone and iPad only.
    public var activities: LiveActivityTracker?
    /// `session.focus` reports for the session views on screen (app.md 8.6).
    public private(set) lazy var focus = FocusReporter { [weak self] habID, params in
        guard let conn = self?.connections.first(where: { $0.id == habID }) else { return }
        _ = try? await conn.call(HabMethods.sessionFocus, params)
    }

    private var consumers: [Task<Void, Never>] = []
    private var windows = 0
    private var check: Task<Void, Never>?
    private var wanted = false
    private var transition: Task<Void, Never>?
    private let log = Logger(subsystem: "com.flightdeck.app", category: "lifecycle")

    /// How long after going to the background the open-stream check runs.
    public var checkDelay: Duration = .seconds(10)
    /// The last result of that check: open streams after the delay.
    public private(set) var lastBackgroundCheck: Int?

    public init(board: Board) {
        self.board = board
    }

    public convenience init() {
        self.init(board: Board())
    }

    /// Adds a hab connection and feeds its states to the board.
    public func add(_ conn: HabConnection, label: String?) {
        connections.append(conn)
        board.register(id: conn.id, label: label, endpoint: conn.transport.endpoint)
        let b = board
        consumers.append(Task { @MainActor [weak self] in
            var lastSnapshot = 0
            for await s in conn.updates {
                b.update(conn.id, s)
                self?.refreshChats(habID: conn.id)
                // Each connection applies one snapshot first: a new count while
                // online is a new connection, with a client that knows nothing.
                if s.status == .online, s.snapshots != lastSnapshot {
                    lastSnapshot = s.snapshots
                    self?.connectionOnline(conn, s)
                } else if s.status != .online {
                    self?.push?.connectionOffline(id: conn.id)
                }
                self?.approvals?.boardChanged()
                self?.activities?.boardChanged()
            }
        })
        if isActive {
            Task { await conn.start() }
        }
    }

    /// Stops a hab's connection and removes it and its rows.
    public func remove(id: String) async {
        guard let i = connections.firstIndex(where: { $0.id == id }) else { return }
        let conn = connections.remove(at: i)
        consumers.remove(at: i).cancel()
        await conn.stop()
        board.unregister(id: id)
        push?.forget(id: id)
        book?.forget(connection: id)
    }

    /// A connection reached online: note its host id for background answers,
    /// register for push, then repeat the focus the new client does not know.
    /// The registration binds this client to the device id, so the focus
    /// report after it counts for the device (hab-logic-api.md 3.9, Presence).
    private func connectionOnline(_ conn: HabConnection, _ s: HabState) {
        guard let w = s.welcome else { return }
        let label = board.habs.first { $0.id == conn.id }?.label ?? w.host.id
        book?.record(host: w.host.id, connection: conn.id, label: label)
        push?.connectionOnline(conn, welcome: w)
        let id = conn.id
        Task { @MainActor [weak self] in
            await self?.push?.settle(id: id)
            self?.focus.connectionOnline(habID: id)
        }
    }

    /// Connects every hab (visible) or closes every connection (background).
    /// Returns once every connection has started or closed. Calls run one after
    /// another, and the last wanted state wins, so a quick background and
    /// foreground never leaves some habs stopped.
    public func setActive(_ active: Bool) async {
        wanted = active
        let previous = transition
        let t = Task { @MainActor in
            await previous?.value
            await self.converge()
        }
        transition = t
        await t.value
    }

    /// Applies `wanted` until it holds; a call that arrived meanwhile changes it.
    private func converge() async {
        while wanted != isActive {
            let active = wanted
            isActive = active
            check?.cancel()
            if active {
                log.info("foreground: connecting \(self.connections.count) habs")
                for c in connections { await c.start() }
                for c in connections { await c.retryNow() }
                terminals.setActive(true)
                focus.setActive(true)
            } else {
                // Tell the habs the session left the screen before the
                // connections close; a hab that misses it drops the focus
                // with the connection anyway.
                focus.setActive(false)
                await Self.within(.seconds(1)) { await self.focus.flush() }
                terminals.setActive(false)
                for c in connections { await c.stop() }
                // iOS may suspend the app before the delayed check runs, so log the count now too.
                log.notice("background: closed \(self.connections.count) habs, \(self.openStreams()) open streams")
                let delay = checkDelay
                check = Task { @MainActor [weak self] in
                    try? await Task.sleep(for: delay)
                    guard let self, !Task.isCancelled, !self.isActive else { return }
                    let n = self.openStreams()
                    self.lastBackgroundCheck = n
                    self.log.notice("background check: \(n) open streams \(delay.components.seconds) s after going to the background")
                }
            }
        }
    }

    /// Runs `work`, giving up on it after `limit`.
    static func within(_ limit: Duration, _ work: @escaping @MainActor () async -> Void) async {
        await withTaskGroup(of: Void.self) { g in
            g.addTask { await work() }
            g.addTask { try? await Task.sleep(for: limit) }
            await g.next()
            g.cancelAll()
        }
    }

    /// iPhone and iPad: the scene phase decides. `inactive` (a sheet from the
    /// system, the app switcher) keeps the connections; it only starts the
    /// Live Activity, which ActivityKit allows only while the app is in front.
    public func scene(active: Bool?) {
        guard let active else {
            // On its way out the scene turns inactive first: the last moment
            // ActivityKit lets the app start a Live Activity (app.md 9.1).
            if isActive { Task { await activities?.start() } }
            return
        }
        Task { await setActive(active) }
    }

    /// Mac: a window opened.
    public func windowOpened() {
        windows += 1
        if windows == 1 { Task { await setActive(true) } }
    }

    /// Mac: a window closed. The last one disconnects every hab.
    public func windowClosed() {
        windows = max(0, windows - 1)
        if windows == 0 { Task { await setActive(false) } }
    }

    /// Finds a hab's connection by id, for views opened from a board row.
    public var lookup: HabConnectionLookup {
        HabConnectionLookup { [weak self] id in self?.connections.first { $0.id == id } }
    }

    // MARK: - Chat

    private final class WeakChat {
        weak var model: ChatModel?
        init(_ m: ChatModel) { model = m }
    }

    private var chats: [WeakChat] = []

    /// The chat model of a session, shared while any view holds it.
    public func chat(for target: SessionTarget) -> ChatModel {
        chats.removeAll { $0.model == nil }
        if let m = chats.first(where: { $0.model?.target == target })?.model { return m }
        let conn = connections.first { $0.id == target.habID }
        let m = ChatModel(target: target, connection: conn)
        m.onSent = { [weak self] t in self?.activities?.noteSent(t) }
        if let h = board.habs.first(where: { $0.id == target.habID }) {
            m.update(ChatContext.of(target, hab: h))
        }
        chats.append(WeakChat(m))
        return m
    }

    private func refreshChats(habID: String) {
        guard let h = board.habs.first(where: { $0.id == habID }) else { return }
        for w in chats {
            guard let m = w.model, m.target.habID == habID else { continue }
            m.update(ChatContext.of(m.target, hab: h))
        }
    }

    /// Streams still open: real carriers (sockets and pipes) plus fake ones.
    public func openStreams() -> Int {
        var n = OpenStreams.live
        for c in connections {
            if let f = c.transport as? FakeTransport { n += f.openStreams }
        }
        return n
    }
}
