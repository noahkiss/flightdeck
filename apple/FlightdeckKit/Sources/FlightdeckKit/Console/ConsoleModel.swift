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
    /// The last input sent; the next one waits for it.
    @ObservationIgnored private var inputTail: Task<Void, Never>?
    /// Inputs numbered in the order given, and the last number not to send.
    @ObservationIgnored private var inputsQueued = 0
    @ObservationIgnored private var dropInputsThrough = 0
    @ObservationIgnored private var listGeneration: Int?
    @ObservationIgnored private var reading: Set<UInt64> = []
    /// How often the model checks for a control reconnect, and reads the
    /// output of an open running card again.
    @ObservationIgnored public var pollEvery: Duration = .seconds(2)
    /// A card reads at most this many bytes, from the output's end.
    @ObservationIgnored public var outputTail: UInt64 = 256 << 10
    /// A card shows at most this many lines, the last ones.
    public static let maxLines = 500
    /// The most bytes one `command.output` call returns (the hab's default).
    @ObservationIgnored var readLimit = 1 << 20
    /// A running card's output so far: the last `outputTail` bytes and the
    /// offset to read on from, so each poll reads only what is new.
    @ObservationIgnored private var running: [UInt64: (next: UInt64?, tail: Data, cut: Bool)] = [:]
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
        // Inputs still queued go nowhere once the console is closed.
        dropInputsThrough = inputsQueued
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

    /// What the input line writes for `line`: control characters other than
    /// tab and newline dropped; text with a newline goes as one bracketed
    /// paste, so the shell does not run each line on its own; then Enter.
    static func inputText(_ line: String) -> String {
        var text = String(String.UnicodeScalarView(line.replacingOccurrences(of: "\r\n", with: "\n").unicodeScalars.filter {
            $0 == "\t" || $0 == "\n" || ($0.value >= 0x20 && $0.value != 0x7f && !(0x80...0x9f).contains($0.value))
        }))
        if text.contains("\n") {
            text = "\u{1B}[200~" + text + "\u{1B}[201~"
        }
        return text + "\r"
    }

    /// Types `line` and Enter into the pane.
    public func run(_ line: String) {
        input(Self.inputText(line))
    }

    /// Sends bytes as they are (Ctrl+C is "\u{03}").
    public func sendRaw(_ text: String) {
        input(text)
    }

    /// Sends input in the order the owner gave it: each call waits for the
    /// one before, so a Ctrl+C never reaches the shell ahead of the command
    /// it should stop. When a call fails, the inputs queued behind it are
    /// dropped with that one error, instead of each waiting out its own
    /// timeout on a dead link; input given after the failure goes again.
    private func input(_ text: String) {
        let c = calls
        let previous = inputTail
        inputsQueued += 1
        let n = inputsQueued
        inputTail = Task { [weak self] in
            await previous?.value
            guard let self, n > self.dropInputsThrough else { return }
            do {
                try await c.input(text)
            } catch {
                self.lastError = String(describing: error)
                self.dropInputsThrough = self.inputsQueued
            }
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
        let read: (data: Data, cut: Bool)?
        if card.command.state == .running {
            read = await readRunning(id)
        } else {
            running[id] = nil
            read = await readEnded(card.command)
        }
        reading.remove(id)
        guard let read else { return }
        // The command went on or ended while the read was out: what came back
        // is stale. Read again while the card is open.
        if let now = list.card(id), now.command.state != card.command.state || now.command.end != card.command.end {
            if expanded.contains(id) { await readOutput(id) }
            return
        }
        var cut = read.cut
        var lines = TerminalText.lines(read.data)
        if lines.count > Self.maxLines {
            lines.removeFirst(lines.count - Self.maxLines)
            cut = true
        }
        list.setOutput(id, lines: lines, cut: cut)
    }

    /// An ended command: at most the last `outputTail` bytes of its range.
    private func readEnded(_ c: Command) async -> (data: Data, cut: Bool)? {
        var next: UInt64?
        var cut = false
        if let end = c.end, end - c.output > outputTail {
            next = end - outputTail
            cut = true
        }
        var data = Data()
        do {
            while true {
                let r = try await calls.output(c.id, next)
                data.append(r.data)
                if r.gap { cut = true }
                if !r.more || r.data.isEmpty || data.count >= Int(outputTail) { break }
                next = r.end
            }
        } catch {
            lastError = String(describing: error)
            return nil
        }
        return (data, cut)
    }

    /// A running command: what is new since the last read, added to the
    /// held tail, which keeps its last `outputTail` bytes.
    private func readRunning(_ id: UInt64) async -> (data: Data, cut: Bool)? {
        var held = running[id] ?? (next: nil, tail: Data(), cut: false)
        do {
            while true {
                let r = try await calls.output(id, held.next)
                held.tail.append(r.data)
                if r.gap { held.cut = true }
                if held.tail.count > Int(outputTail) {
                    held.tail.removeFirst(held.tail.count - Int(outputTail))
                    held.cut = true
                }
                held.next = r.end
                // Caught up: the hab had less than a full read, or nothing.
                if !r.more || r.data.count < readLimit { break }
            }
        } catch {
            lastError = String(describing: error)
            return nil
        }
        running[id] = held
        return (held.tail, held.cut)
    }
}
