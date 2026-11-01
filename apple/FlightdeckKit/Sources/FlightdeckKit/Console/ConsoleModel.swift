import Foundation
import HabAPI
import Observation
import os

/// One console (app.md 6): the command cards of a shell pane, from the hab's
/// prompt marks, and the pane's terminal for full-screen programs. The
/// terminal model runs as long as the console is open, so its stream tells
/// when a program enters the alternate screen.
@MainActor
@Observable
public final class ConsoleModel {
    public nonisolated let pane: String
    public private(set) var list = ConsoleCardList()
    /// True once the first `command.list` answered.
    public private(set) var loaded = false
    public private(set) var lastError: String?
    /// Cards the owner opened.
    public private(set) var expanded: Set<UInt64> = []
    /// The input line.
    public var draft = ""
    /// The owner chose the terminal view by hand.
    public var terminalChosen = false

    /// The pane's terminal; nil in tests that need no stream.
    @ObservationIgnored public let terminal: TerminalModel?

    /// What the model needs from the hab, so tests can feed it.
    public struct Calls: Sendable {
        public var list: @Sendable (_ before: UInt64?) async throws -> CommandList
        public var output: @Sendable (_ command: UInt64, _ from: UInt64?) async throws -> CommandOutput
        public var input: @Sendable (_ text: String) async throws -> Void
        /// This hab's command events; the model keeps its pane's.
        public var updates: @Sendable () async -> AsyncStream<CommandUpdate>
        /// The live connection's generation; a change means events were missed.
        public var generation: @Sendable () async -> Int

        public init(
            list: @escaping @Sendable (UInt64?) async throws -> CommandList,
            output: @escaping @Sendable (UInt64, UInt64?) async throws -> CommandOutput,
            input: @escaping @Sendable (String) async throws -> Void,
            updates: @escaping @Sendable () async -> AsyncStream<CommandUpdate>,
            generation: @escaping @Sendable () async -> Int
        ) {
            self.list = list
            self.output = output
            self.input = input
            self.updates = updates
            self.generation = generation
        }

        public init(pane: String, connection: HabConnection) {
            list = { before in
                try await connection.call(HabMethods.commandList, CommandListParams(before: before, pane: pane))
            }
            output = { id, from in
                try await connection.call(HabMethods.commandOutput, CommandOutputParams(command: id, from: from, pane: pane))
            }
            input = { text in
                _ = try await connection.call(HabMethods.paneInput, InputParams(pane: pane, text: text))
            }
            updates = { await connection.commandUpdates() }
            generation = { await connection.connectionGeneration }
        }
    }

    @ObservationIgnored private let calls: Calls
    @ObservationIgnored private var tasks: [Task<Void, Never>] = []
    @ObservationIgnored private var listGeneration: Int?
    @ObservationIgnored private var reading: Set<UInt64> = []
    /// How often the model checks for a control reconnect, and reads the
    /// output of an open running card again.
    @ObservationIgnored public var pollEvery: Duration = .seconds(2)
    /// A card reads at most this many bytes, from the output's end.
    @ObservationIgnored public var outputTail: UInt64 = 256 << 10
    /// A card shows at most this many lines, the last ones.
    public static let maxLines = 500
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "console")

    public convenience init(pane: String, connection: HabConnection) {
        self.init(pane: pane, calls: Calls(pane: pane, connection: connection),
                  terminal: TerminalModel(pane: pane, connection: connection))
    }

    public init(pane: String, calls: Calls, terminal: TerminalModel?) {
        self.pane = pane
        self.calls = calls
        self.terminal = terminal
    }

    /// The view shows the terminal: a full-screen program runs, or the owner
    /// chose it.
    public var showsTerminal: Bool {
        terminalChosen || (terminal?.alternateScreen ?? false)
    }

    /// A full-screen program holds the pane now.
    public var fullScreenProgram: Bool { terminal?.alternateScreen ?? false }

    // MARK: - Lifecycle

    /// Reads the newest page and follows the pane's command events.
    public func start() {
        guard tasks.isEmpty else { return }
        let c = calls
        let pane = pane
        tasks.append(Task { [weak self] in
            let stream = await c.updates()
            await self?.reload()
            for await u in stream where u.pane == pane {
                guard let self else { return }
                await self.apply(u)
            }
        })
        let every = pollEvery
        tasks.append(Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: every)
                guard !Task.isCancelled, let self else { return }
                await self.poll()
            }
        })
    }

    public func stop() {
        for t in tasks { t.cancel() }
        tasks = []
    }

    // MARK: - Actions

    /// Opens or folds a card. Opening reads its output when it has none.
    public func toggle(_ id: UInt64) {
        if expanded.contains(id) {
            expanded.remove(id)
            return
        }
        expanded.insert(id)
        if list.card(id)?.output == nil {
            Task { await readOutput(id) }
        }
    }

    /// Sends the input line to the shell as typed text, then Enter.
    public func send() {
        let line = draft
        draft = ""
        run(line)
    }

    /// Types `line` and Enter into the pane.
    public func run(_ line: String) {
        let c = calls
        Task { [weak self] in
            do {
                try await c.input(line + "\r")
            } catch {
                self?.lastError = String(describing: error)
            }
        }
    }

    /// Sends bytes as they are (Ctrl+C is "\u{03}").
    public func sendRaw(_ text: String) {
        let c = calls
        Task { [weak self] in
            do { try await c.input(text) } catch { self?.lastError = String(describing: error) }
        }
    }

    /// Reads the page before the oldest card.
    public func loadOlder() async {
        guard list.more, let first = list.cards.first?.id else { return }
        do {
            let page = try await calls.list(first)
            list.merge(page.commands)
            list.more = page.more
        } catch {
            lastError = String(describing: error)
        }
    }

    // MARK: - Updates

    /// Reads the newest page again; held output of unchanged cards stays.
    public func reload() async {
        do {
            let g = await calls.generation()
            let page = try await calls.list(nil)
            let olderHeld = list.cards.first.map { first in page.commands.first.map { first.id < $0.id } ?? false } ?? false
            if olderHeld {
                // Keep older cards already read; the page holds the newest.
                list.merge(page.commands)
            } else {
                list.replace(page)
            }
            listGeneration = g
            loaded = true
            lastError = nil
            refreshOpen()
        } catch {
            lastError = String(describing: error)
            log.notice("console \(self.pane, privacy: .public): command.list: \(String(describing: error), privacy: .public)")
        }
    }

    func apply(_ u: CommandUpdate) async {
        if let g = listGeneration, g != u.generation {
            await reload()
            return
        }
        if (u.command.dropped ?? 0) > 0 {
            await reload()
            return
        }
        list.merge([u.command])
        loaded = true
        refreshOpen()
    }

    private func poll() async {
        let g = await calls.generation()
        if let held = listGeneration, held != g {
            await reload()
            return
        }
        // An open card of a running command reads its output again.
        for c in list.cards where c.command.state == .running && expanded.contains(c.id) {
            await readOutput(c.id)
        }
    }

    /// Open cards that lost their output (it went on, or ended) read it again.
    private func refreshOpen() {
        for id in expanded where list.card(id)?.output == nil {
            Task { await readOutput(id) }
        }
    }

    func readOutput(_ id: UInt64) async {
        guard let card = list.card(id), !reading.contains(id) else { return }
        reading.insert(id)
        defer { reading.remove(id) }
        var from: UInt64?
        var cut = false
        if let end = card.command.end, end - card.command.output > outputTail {
            from = end - outputTail
            cut = true
        }
        var data = Data()
        do {
            var next = from
            while true {
                let r = try await calls.output(id, next)
                data.append(r.data)
                if r.gap { cut = true }
                if !r.more || r.data.isEmpty || r.command.state == .running || data.count >= Int(outputTail) { break }
                next = r.end
            }
        } catch {
            lastError = String(describing: error)
            return
        }
        var lines = TerminalText.lines(data)
        if lines.count > Self.maxLines {
            lines.removeFirst(lines.count - Self.maxLines)
            cut = true
        }
        list.setOutput(id, lines: lines, cut: cut)
    }
}
