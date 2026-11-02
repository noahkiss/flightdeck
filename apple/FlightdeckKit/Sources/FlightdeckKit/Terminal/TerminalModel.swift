import Foundation
import GhosttyTerminal
import HabAPI
import Observation
import os

/// One pane in the terminal view: its data stream feeds libghostty, the
/// terminal's key output goes to `pane.input`, and the size rule decides when
/// this client sends a size (app.md 5.1, hab-logic-api.md 6).
@MainActor
@Observable
public final class TerminalModel {
    public enum Status: Equatable, Sendable {
        case connecting
        case live
        case retrying(String)
        case exited(ExitedFrame)
        case gone
        case closed
    }

    public nonisolated let pane: String
    public private(set) var status: Status = .connecting
    public private(set) var sizing = PaneSizing()
    /// The size this client last sent with `pane.set_desired_size`.
    public private(set) var sentSize: (cols: Int, rows: Int)?
    public private(set) var lastError: String?
    /// Set when a `pane.input` call failed and the input queued behind it was
    /// dropped (app.md 5.3); the view shows it until dismissed.
    public private(set) var inputNotice: String?
    /// True while the pane shows the alternate screen (a full-screen
    /// program); the console hands over to this view then (app.md 6.1).
    public private(set) var alternateScreen = false
    @ObservationIgnored private var screenMode = ScreenModeScanner()

    /// libghostty's view state; the view shows it.
    @ObservationIgnored public let view: TerminalViewState

    @ObservationIgnored private let calls: Calls
    /// Makes the data stream; a view that reappears gets a new one.
    @ObservationIgnored private let makeStream: () -> PaneStream
    @ObservationIgnored private var stream: PaneStream?
    /// Counts streams, so events from a closed one are ignored.
    @ObservationIgnored private var generation = 0
    /// The holder offset after the last byte this model applied. The screen
    /// lives as long as the model, so a new stream resumes from here.
    @ObservationIgnored public private(set) var appliedOffset: UInt64?
    /// The last snapshot, while no bytes have followed it: when a resize frame
    /// right after it changes the size, it is drawn again at the new size.
    @ObservationIgnored private var replaySnapshot: Data?
    @ObservationIgnored private let session: InMemoryTerminalSession
    @ObservationIgnored private let flags = Flags()
    @ObservationIgnored private var sizeTask: Task<Void, Never>?
    @ObservationIgnored private var started = false
    @ObservationIgnored private var suspended = false
    /// libghostty's grid as it last reported it.
    @ObservationIgnored private var grid: (cols: Int, rows: Int)?
    @ObservationIgnored private var gridSettled = false
    @ObservationIgnored private var settleTask: Task<Void, Never>?
    /// How long the grid must hold still before output reaches it.
    @ObservationIgnored public var settleDelay: Duration = .milliseconds(100)
    /// Output that waits until the grid equals the pane's size.
    @ObservationIgnored private var pending: [Data] = []
    @ObservationIgnored private var pendingBytes = 0
    @ObservationIgnored private var needsSnapshot = false
    /// Output held past this is dropped, and a fresh snapshot asked for instead.
    @ObservationIgnored var pendingLimit = 4 << 20
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "terminal")

    /// Wait after a size change before the size goes out; the hab throttles too (6.6).
    @ObservationIgnored public var sizeDelay: Duration = .milliseconds(120)

    /// What the model needs from the hab, so tests can count calls.
    public struct Calls: Sendable {
        public var clientID: @Sendable () async -> String?
        public var setSize: @Sendable (_ cols: Int, _ rows: Int) async throws -> Void
        public var redraw: @Sendable (RedrawMode) async throws -> Void
        public var take: @Sendable () async throws -> Void
        public var input: PaneInputPump

        public init(pane: String, connection: HabConnection) {
            clientID = { await connection.state.welcome?.clientId }
            setSize = { c, r in _ = try await connection.call(HabMethods.paneSetDesiredSize, DesiredSizeParams(cols: c, pane: pane, rows: r)) }
            redraw = { m in _ = try await connection.call(HabMethods.paneRedraw, RedrawParams(mode: m, pane: pane)) }
            take = { _ = try await connection.call(HabMethods.paneTake, PaneRef(pane: pane)) }
            input = PaneInputPump(pane: pane, connection: connection)
        }
    }

    /// Thread-safe facts the terminal's write callback reads off the main thread.
    final class Flags: @unchecked Sendable {
        private let lock = NSLock()
        private var _owner = false
        private var _liveRun = false
        var owner: Bool {
            get { lock.withLock { _owner } }
            set { lock.withLock { _owner = newValue } }
        }
        var liveRun: Bool {
            get { lock.withLock { _liveRun } }
            set { lock.withLock { _liveRun = newValue } }
        }
    }

    /// The pane runs a round's live run (app.md 21.4): focus reports, mouse motion and the
    /// wheel are dropped even while this client owns the pane, so a viewer who only focuses
    /// or scrolls never ends the run taken over.
    public var liveRun: Bool {
        get { flags.liveRun }
        set { flags.liveRun = newValue }
    }

    public convenience init(pane: String, connection: HabConnection) {
        self.init(pane: pane, makeStream: { PaneStream(pane: pane, connection: connection) }, calls: Calls(pane: pane, connection: connection))
    }

    public init(pane: String, makeStream: @escaping () -> PaneStream, calls: Calls) {
        self.pane = pane
        self.makeStream = makeStream
        self.calls = calls
        let input = calls.input
        let flags = flags
        session = InMemoryTerminalSession(
            write: { data in
                // Keys, paste and mouse reports go to the pane; the terminal's
                // own replies to queries do not (hab-logic-api.md 5.6).
                input.enqueue(TerminalReplyFilter.keep(data, owner: flags.owner, liveRun: flags.liveRun))
            },
            resize: { _ in
                // The grid follows the surface, which follows the pane's size.
                // The PTY's size is set by the size rule, never from here.
            },
            suppressesPixelOnlyResizes: true
        )
        view = TerminalViewState(controller: Self.controller)
        view.configuration = TerminalSurfaceOptions(backend: .inMemory(session))
        input.setOnError { [weak self] e in
            Task { @MainActor in self?.inputDropped(e) }
        }
        #if os(iOS)
        view.inputAccessoryItems = [
            .esc, .tab, .ctrl, .alt, .divider,
            .arrowLeft, .arrowUp, .arrowDown, .arrowRight, .divider,
            .symbol("|"), .symbol("/"), .symbol("~"), .symbol("-"), .symbol("_"), .paste,
        ]
        #endif
    }

    /// The terminal's background; the view fills the space around the surface with it.
    public static let background = "#1e1e1e"

    /// One libghostty app for every terminal view: no padding, so the grid is
    /// exactly what the size rule lays out.
    static let controller = TerminalController { b in
        b.withBackground(TerminalModel.background)
        b.withWindowPaddingX(0)
        b.withWindowPaddingY(0)
        #if os(iOS)
        b.withFontSize(11)
        #else
        b.withFontSize(13)
        #endif
    }

    // MARK: - Lifecycle

    /// Opens the data stream. After a `stop` it opens a new one that resumes
    /// from the last applied offset: the screen is still here, so the hab sends
    /// only what was missed, or a snapshot.
    public func start() {
        suspended = false
        guard !started else { return }
        started = true
        generation += 1
        let gen = generation
        let s = makeStream()
        stream = s
        let from = appliedOffset
        let apply: PaneStream.Apply = { [weak self] e in await self?.apply(e, generation: gen) }
        Task { await s.start(from: from, apply: apply) }
    }

    /// Background: closes the stream of a visible view, to open it again on
    /// `resume`.
    public func suspend() {
        guard started else { return }
        stop()
        suspended = true
    }

    /// Foreground: opens the stream that `suspend` closed.
    public func resume() {
        guard suspended else { return }
        start()
    }

    /// Closes the data stream: the view is hidden, or the app went to the
    /// background. `start` resumes it.
    public func stop() {
        suspended = false
        guard started else { return }
        started = false
        sizeTask?.cancel()
        let s = stream
        stream = nil
        if case .exited = status {} else if status != .gone { status = .connecting }
        Task { await s?.close() }
    }

    /// Hides the notice of dropped input.
    public func dismissInputNotice() {
        inputNotice = nil
    }

    /// The notice for dropped input: it names the connection only when that was the cause.
    nonisolated static func inputNoticeText(_ e: InputDropped) -> String {
        e.connectionLost ? "Some input was not sent. The connection dropped." : "Some input was not sent."
    }

    private func inputDropped(_ e: InputDropped) {
        inputNotice = Self.inputNoticeText(e)
        lastError = e.description
        log.notice("pane \(self.pane, privacy: .public): \(e.description, privacy: .public)")
    }

    /// The pump the terminal's key output goes through; tests type with it.
    var inputPumpForTests: PaneInputPump { calls.input }

    /// The holder offset after the last applied byte, to resume later.
    public func heldOffset() -> UInt64? { appliedOffset }

    // MARK: - Actions

    /// A fresh snapshot, then the PTY one column narrower and back
    /// (`pane.redraw` nudge, hab-logic-api.md 6.7). Never Ctrl+L.
    public func redraw() {
        let c = calls
        Task { [weak self] in
            do { try await c.redraw(.nudge) } catch { self?.lastError = String(describing: error) }
        }
    }

    /// "Fit to this screen": take the size now (`pane.take`).
    public func fitToScreen() {
        let c = calls
        Task { [weak self] in
            do { try await c.take() } catch { self?.lastError = String(describing: error) }
        }
    }

    /// The view's space changed.
    public func containerChanged(_ size: CGSize) {
        guard size != sizing.container else { return }
        sizing.container = size
        sizeRuleChanged()
    }

    /// libghostty reported its grid; the cell size is what the size rule needs.
    public func surfaceChanged(_ m: TerminalGridMetrics?, displayScale: CGFloat) {
        guard let m else { return }
        let g = (cols: Int(m.columns), rows: Int(m.rows))
        if grid == nil || grid! != g {
            // libghostty reports a new grid before its parser has resized, so
            // output waits until the grid has held still for a moment.
            grid = g
            gridSettled = false
            settleTask?.cancel()
            let wait = settleDelay
            settleTask = Task { [weak self] in
                try? await Task.sleep(for: wait)
                guard !Task.isCancelled, let self else { return }
                self.gridSettled = true
                self.flush()
            }
        }
        guard m.cellWidthPixels > 0, m.cellHeightPixels > 0, displayScale > 0 else { return }
        let cell = CGSize(width: CGFloat(m.cellWidthPixels) / displayScale, height: CGFloat(m.cellHeightPixels) / displayScale)
        guard cell != sizing.cell else { return }
        sizing.cell = cell
        sizeRuleChanged()
    }

    // MARK: - Stream events

    func apply(_ e: PaneStreamEvent, generation gen: Int) async {
        guard gen == generation else { return }
        switch e {
        case .welcome(let w):
            // The client id changes when the control connection reconnects
            // without its token, so it is read again on every welcome and resize.
            let me = await calls.clientID()
            guard gen == generation else { return }
            setPane(cols: w.cols, rows: w.rows, owner: w.owner, me: me)
            status = .live
        case .snapshot(let off, _, let data):
            // A snapshot redraws a new terminal: reset this one, clear its
            // scrollback, then draw.
            var d = Data("\u{1B}c\u{1B}[3J".utf8)
            d.append(data)
            pending = []
            pendingBytes = 0
            needsSnapshot = false
            replaySnapshot = d
            appliedOffset = off
            screenMode.reset()
            noteScreenMode(data)
            feed(d)
            status = .live
        case .bytes(let off, let data):
            replaySnapshot = nil
            appliedOffset = off + UInt64(data.count)
            noteScreenMode(data)
            feed(data)
        case .resize(let r):
            let me = await calls.clientID()
            guard gen == generation else { return }
            setPane(cols: r.cols, rows: r.rows, owner: r.owner, me: me)
        case .exited(let x):
            status = .exited(x)
        case .retrying(let why):
            if case .exited = status { return }
            status = .retrying(why)
        case .ended(let end):
            switch end {
            case .exited: if case .exited = status {} else { status = .exited(ExitedFrame()) }
            case .gone: status = .gone
            case .closed: status = .closed
            }
        }
    }

    private func noteScreenMode(_ data: Data) {
        screenMode.feed(data)
        if screenMode.alternate != alternateScreen { alternateScreen = screenMode.alternate }
    }

    /// The terminal must parse output at the PTY's size, or rows wrap wrongly.
    /// The surface follows the pane's size a moment later (a layout pass), so
    /// output waits until libghostty reports the same grid.
    private func feed(_ data: Data) {
        if pending.isEmpty, gridMatches {
            session.receive(data)
            return
        }
        pending.append(data)
        pendingBytes += data.count
        if pendingBytes > pendingLimit {
            pending = []
            pendingBytes = 0
            needsSnapshot = true
        }
    }

    private var gridMatches: Bool {
        guard let g = grid, gridSettled else { return false }
        return g.cols == sizing.cols && g.rows == sizing.rows
    }

    private func flush() {
        guard gridMatches else { return }
        if needsSnapshot {
            needsSnapshot = false
            let c = calls
            Task { try? await c.redraw(.snapshot) }
        }
        for d in pending { session.receive(d) }
        pending = []
        pendingBytes = 0
    }

    /// Bytes waiting for the grid; tests read it.
    var heldBytes: Int { pendingBytes }

    /// Tests stand in for libghostty's grid report.
    func gridForTests(cols: Int, rows: Int) {
        grid = (cols, rows)
        gridSettled = true
        flush()
    }

    private func setPane(cols: Int, rows: Int, owner: String?, me: String?) {
        var s = sizing
        s.cols = max(1, cols)
        s.rows = max(1, rows)
        s.owner = owner
        s.me = me
        guard s != sizing else { return }
        let wasOwner = sizing.isOwner
        let resized = s.cols != sizing.cols || s.rows != sizing.rows
        sizing = s
        if resized, let snap = replaySnapshot {
            // The hab sends a redraw's snapshot before its resize frame. With
            // no bytes in between, the snapshot was drawn at the old size:
            // draw it again once the grid has the new one.
            pending = []
            pendingBytes = 0
            feed(snap)
        }
        flags.owner = s.isOwner
        if s.isOwner != wasOwner { sentSize = nil }
        sizeRuleChanged()
        flush()
    }

    /// While this client owns the pane, the PTY follows this view's space.
    /// Otherwise nothing is sent: viewing never resizes (6.1, 6.2).
    private func sizeRuleChanged() {
        sizeTask?.cancel()
        guard sizing.isOwner, sizing.hasContainer else { return }
        let want = sizing.desired
        if want.cols == sizing.cols && want.rows == sizing.rows {
            sentSize = want
            return
        }
        if let s = sentSize, s == want { return }
        let delay = sizeDelay
        let c = calls
        sizeTask = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled, let self else { return }
            self.sentSize = want
            do {
                try await c.setSize(want.cols, want.rows)
            } catch {
                // Not sent: the next change sends it again.
                if let s = self.sentSize, s == want { self.sentSize = nil }
                self.lastError = String(describing: error)
                self.log.notice("pane \(self.pane, privacy: .public): set size: \(String(describing: error), privacy: .public)")
            }
        }
    }
}
