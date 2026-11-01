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

    /// libghostty's view state; the view shows it.
    @ObservationIgnored public let view: TerminalViewState

    @ObservationIgnored private let calls: Calls
    @ObservationIgnored private let stream: PaneStream
    @ObservationIgnored private let session: InMemoryTerminalSession
    @ObservationIgnored private let flags = Flags()
    @ObservationIgnored private var sizeTask: Task<Void, Never>?
    @ObservationIgnored private var started = false
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
        var owner: Bool {
            get { lock.withLock { _owner } }
            set { lock.withLock { _owner = newValue } }
        }
    }

    public convenience init(pane: String, connection: HabConnection) {
        self.init(pane: pane, stream: PaneStream(pane: pane, connection: connection), calls: Calls(pane: pane, connection: connection))
    }

    public init(pane: String, stream: PaneStream, calls: Calls) {
        self.pane = pane
        self.stream = stream
        self.calls = calls
        let input = calls.input
        let flags = flags
        session = InMemoryTerminalSession(
            write: { data in
                // Keys, paste and mouse reports go to the pane; the terminal's
                // own replies to queries do not (hab-logic-api.md 5.6).
                input.enqueue(TerminalReplyFilter.keep(data, owner: flags.owner))
            },
            resize: { _ in
                // The grid follows the surface, which follows the pane's size.
                // The PTY's size is set by the size rule, never from here.
            },
            suppressesPixelOnlyResizes: true
        )
        view = TerminalViewState(controller: Self.controller)
        view.configuration = TerminalSurfaceOptions(backend: .inMemory(session))
        #if os(iOS)
        view.inputAccessoryItems = [
            .esc, .tab, .ctrl, .alt, .divider,
            .arrowLeft, .arrowUp, .arrowDown, .arrowRight, .divider,
            .symbol("|"), .symbol("/"), .symbol("~"), .symbol("-"), .symbol("_"), .paste,
        ]
        #endif
    }

    /// One libghostty app for every terminal view: no padding, so the grid is
    /// exactly what the size rule lays out.
    static let controller = TerminalController { b in
        b.withWindowPaddingX(0)
        b.withWindowPaddingY(0)
        #if os(iOS)
        b.withFontSize(11)
        #else
        b.withFontSize(13)
        #endif
    }

    // MARK: - Lifecycle

    /// Opens the data stream. `from` resumes from an offset kept earlier.
    public func start(from offset: UInt64? = nil) {
        guard !started else { return }
        started = true
        let s = stream
        let apply: PaneStream.Apply = { [weak self] e in await self?.apply(e) }
        Task { await s.start(from: offset, apply: apply) }
    }

    /// Closes the data stream. The hab keeps no state for a view.
    public func stop() {
        sizeTask?.cancel()
        Task { await stream.close() }
    }

    /// The pump the terminal's key output goes through; tests type with it.
    var inputPumpForTests: PaneInputPump { calls.input }

    /// The holder offset after the last applied byte, to resume later.
    public func heldOffset() async -> UInt64? { await stream.offset }

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
        guard let m, m.cellWidthPixels > 0, m.cellHeightPixels > 0, displayScale > 0 else { return }
        let cell = CGSize(width: CGFloat(m.cellWidthPixels) / displayScale, height: CGFloat(m.cellHeightPixels) / displayScale)
        guard cell != sizing.cell else { return }
        sizing.cell = cell
        sizeRuleChanged()
    }

    // MARK: - Stream events

    func apply(_ e: PaneStreamEvent) async {
        switch e {
        case .welcome(let w):
            sizing.me = await calls.clientID()
            setPane(cols: w.cols, rows: w.rows, owner: w.owner)
            status = .live
        case .snapshot(_, _, let data):
            // A snapshot redraws a new terminal: reset this one, clear its
            // scrollback, then draw.
            var d = Data("\u{1B}c\u{1B}[3J".utf8)
            d.append(data)
            session.receive(d)
            status = .live
        case .bytes(_, let data):
            session.receive(data)
        case .resize(let r):
            if sizing.me == nil { sizing.me = await calls.clientID() }
            setPane(cols: r.cols, rows: r.rows, owner: r.owner)
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

    private func setPane(cols: Int, rows: Int, owner: String?) {
        var s = sizing
        s.cols = max(1, cols)
        s.rows = max(1, rows)
        s.owner = owner
        guard s != sizing else { return }
        let wasOwner = sizing.isOwner
        sizing = s
        flags.owner = s.isOwner
        if s.isOwner != wasOwner { sentSize = nil }
        sizeRuleChanged()
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
                self.lastError = String(describing: error)
                self.log.notice("pane \(self.pane, privacy: .public): set size: \(String(describing: error), privacy: .public)")
            }
        }
    }
}
