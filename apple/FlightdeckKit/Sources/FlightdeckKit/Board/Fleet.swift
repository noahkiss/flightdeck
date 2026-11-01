import Foundation
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

    private var consumers: [Task<Void, Never>] = []
    private var windows = 0
    private var check: Task<Void, Never>?
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
        consumers.append(Task { @MainActor in
            for await s in conn.updates {
                b.update(conn.id, s)
            }
        })
        if isActive {
            Task { await conn.start() }
        }
    }

    /// Connects every hab (visible) or closes every connection (background).
    /// Returns once every connection has started or closed.
    public func setActive(_ active: Bool) async {
        guard active != isActive else { return }
        isActive = active
        check?.cancel()
        if active {
            log.info("foreground: connecting \(self.connections.count) habs")
            for c in connections { await c.start() }
            for c in connections { await c.retryNow() }
        } else {
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

    /// iPhone and iPad: the scene phase decides. `inactive` (a sheet from the
    /// system, the app switcher) changes nothing.
    public func scene(active: Bool?) {
        guard let active else { return }
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

    /// Streams still open: real carriers (sockets and pipes) plus fake ones.
    public func openStreams() -> Int {
        var n = OpenStreams.live
        for c in connections {
            if let f = c.transport as? FakeTransport { n += f.openStreams }
        }
        return n
    }
}
