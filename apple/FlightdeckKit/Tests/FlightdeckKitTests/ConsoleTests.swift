import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The console's cards (app.md 6).
///
/// Input: `cmd/gff-hab/testdata/console/<name>.commands.json`, what
/// command.list and command.output answer for a hand-written stream of
/// prompt marks (`<name>.marks.json`, fed through the hab's tracker by
/// `TestConsoleFixtures`). Output: the card list as plain text
/// (`ConsoleRender.text`), every card opened, compared with
/// `Snapshots/console-<name>.txt`. `FLIGHTDECK_UPDATE_SNAPSHOTS=1` rewrites it.
@MainActor
final class ConsoleTests: XCTestCase {
    static var fixtureDir: URL {
        ChatSnapshotTests.repo.appendingPathComponent("cmd/gff-hab/testdata/console")
    }

    static func fixture(_ name: String) throws -> [CommandOutput] {
        let url = fixtureDir.appendingPathComponent(name + ".commands.json")
        return try JSONDecoder().decode([CommandOutput].self, from: Data(contentsOf: url))
    }

    /// Calls that answer from fixture outputs, and an event stream the test feeds.
    final class FakeCalls: @unchecked Sendable {
        let lock = NSLock()
        var outputs: [UInt64: CommandOutput] = [:]
        var commands: [Command] = []
        var more = false
        var generation = 1
        var inputs: [String] = []
        var lists = 0
        var outputReads = 0
        /// Answers output reads first, when set; nil falls back to `outputs`.
        var outputHook: (@Sendable (UInt64, UInt64?) async -> CommandOutput?)?
        /// Runs before an input is recorded, when set: a slow link.
        var inputHook: (@Sendable (String) async -> Void)?
        let events: AsyncStream<CommandUpdate>
        let sink: AsyncStream<CommandUpdate>.Continuation

        /// Serves every fixture output; lists the first `listed` commands (all by default).
        init(_ fixture: [CommandOutput], listed: Int? = nil) {
            for o in fixture { outputs[o.command.id] = o }
            commands = fixture.prefix(listed ?? fixture.count).map(\.command)
            (events, sink) = AsyncStream.makeStream(of: CommandUpdate.self)
        }

        func with<T>(_ f: () -> T) -> T { lock.withLock(f) }

        var calls: ConsoleModel.Calls {
            ConsoleModel.Calls(
                list: { [self] before in
                    with {
                        lists += 1
                        let cs = commands.filter { before == nil || $0.id < before! }
                        return CommandList(commands: cs, more: more, seq: 1)
                    }
                },
                output: { [self] id, from in
                    if let hook = with({ outputHook }), let o = await hook(id, from) {
                        with { outputReads += 1 }
                        return o
                    }
                    let o: CommandOutput? = with {
                        outputReads += 1
                        return outputs[id]
                    }
                    guard let o else { throw RPCError(code: -1, message: "not found", data: nil) }
                    return o
                },
                input: { [self] text in
                    if let hook = with({ inputHook }) { await hook(text) }
                    with { inputs.append(text) }
                },
                updates: { [self] in events },
                generation: { [self] in with { generation } }
            )
        }
    }

    func testCardSnapshotFromRecordedMarks() async throws {
        let fx = try Self.fixture("basic")
        let fake = FakeCalls(fx)
        let m = ConsoleModel(pane: "%9", calls: fake.calls, terminal: nil)
        m.start()
        defer { m.stop() }
        await eventually("loaded") { m.loaded }
        XCTAssertEqual(m.list.cards.count, fx.count)

        // Acceptance: ls, false and sleep 2 with the right exit codes and durations.
        let byText = Dictionary(uniqueKeysWithValues: m.list.cards.map { ($0.title, $0) })
        XCTAssertEqual(byText["ls"]?.status, "Exit 0")
        XCTAssertEqual(byText["false"]?.status, "Exit 1")
        XCTAssertEqual(byText["false"]?.failed, true)
        XCTAssertEqual(byText["sleep 2"]?.status, "Exit 0")
        XCTAssertEqual(byText["sleep 2"]?.duration ?? 0, 2.004, accuracy: 0.001)
        XCTAssertEqual(byText["make test"]?.status, "Ended", "a command with no D mark")
        XCTAssertEqual(byText["tail -f app.log"]?.status, "Running")
        XCTAssertEqual(byText["cd src"]?.command.cwd, "/home/user/project")
        XCTAssertEqual(byText["vim notes.txt"]?.command.cwd, "/home/user/project/src")

        // Folded: no output read. Opened: each card reads its range.
        XCTAssertEqual(fake.with { fake.outputReads }, 0)
        for c in m.list.cards { m.toggle(c.id) }
        await eventually("outputs") { m.list.cards.allSatisfy { $0.output != nil } }

        let text = ConsoleRender.text(m.list)
        let golden = ChatSnapshotTests.snapshotDir.appendingPathComponent("console-basic.txt")
        if ProcessInfo.processInfo.environment["FLIGHTDECK_UPDATE_SNAPSHOTS"] == "1" {
            try text.write(to: golden, atomically: true, encoding: .utf8)
        }
        let want = try String(contentsOf: golden, encoding: .utf8)
        XCTAssertEqual(text, want, "card snapshot differs; FLIGHTDECK_UPDATE_SNAPSHOTS=1 rewrites it")

        // Folding hides the output again and reads nothing.
        let reads = fake.with { fake.outputReads }
        m.toggle(m.list.cards[0].id)
        XCTAssertFalse(m.expanded.contains(m.list.cards[0].id))
        XCTAssertEqual(fake.with { fake.outputReads }, reads)
    }

    /// Events add and end cards; an ended card that was open reads its output
    /// again; `dropped` and a new connection read the list again.
    func testEventsAndCatchUp() async throws {
        let fx = try Self.fixture("basic")
        let fake = FakeCalls(fx, listed: 2)
        let m = ConsoleModel(pane: "%9", calls: fake.calls, terminal: nil)
        m.pollEvery = .milliseconds(50)
        m.start()
        defer { m.stop() }
        await eventually("loaded") { m.list.cards.count == 2 }
        XCTAssertEqual(fake.with { fake.lists }, 1)

        // Another pane's command is not this console's.
        var other = fx[2].command
        other.id = 1
        fake.sink.yield(CommandUpdate(pane: "%1", command: other, generation: 1))

        // sleep 2 starts: a running card.
        var running = fx[2].command
        running.end = nil
        running.endedAt = nil
        running.exit = nil
        running.state = .running
        fake.sink.yield(CommandUpdate(pane: "%9", command: running, generation: 1))
        await eventually("running card") { m.list.cards.count == 3 }
        XCTAssertEqual(m.list.cards.last?.status, "Running")
        m.toggle(running.id)
        await eventually("running output") { m.list.card(running.id)?.output != nil }

        // It ends: the card shows the exit and duration, and reads its output again.
        let reads = fake.with { fake.outputReads }
        fake.sink.yield(CommandUpdate(pane: "%9", command: fx[2].command, generation: 1))
        await eventually("ended") { m.list.cards.last?.status == "Exit 0" }
        await eventually("read again") { fake.with { fake.outputReads } > reads }
        XCTAssertEqual(m.list.cards.map(\.id), fx.prefix(3).map(\.command.id))

        // The hab left events out: the list is read again.
        fake.with { fake.commands = fx.prefix(4).map(\.command) }
        var dropped = fx[3].command
        dropped.dropped = 2
        fake.sink.yield(CommandUpdate(pane: "%9", command: dropped, generation: 1))
        await eventually("reloaded") { m.list.cards.count == 4 }
        XCTAssertEqual(fake.with { fake.lists }, 2)

        // A reconnect (new generation) with no event: the poll reads the list again.
        fake.with {
            fake.commands = fx.prefix(5).map(\.command)
            fake.generation = 2
        }
        await eventually("after reconnect") { m.list.cards.count == 5 }
    }

    /// A hab-side running command: a growing buffer that `command.output`
    /// serves from any offset, at most `limit` bytes per call.
    final class Growing: @unchecked Sendable {
        let lock = NSLock()
        let command: Command
        var bytes = Data()
        var froms: [UInt64?] = []
        let limit: Int
        init(_ c: Command, limit: Int) { command = c; self.limit = limit }
        func append(_ s: String) { lock.withLock { bytes.append(contentsOf: s.utf8) } }
        func read(_ from: UInt64?) -> CommandOutput {
            lock.withLock {
                froms.append(from)
                let start = Int((from ?? command.output) - command.output)
                let end = min(bytes.count, start + limit)
                return CommandOutput(command: command, data: bytes.subdata(in: start..<end),
                                     end: command.output + UInt64(end), gap: false, more: true, seq: 1, start: command.output + UInt64(start))
            }
        }
    }

    /// An open running card reads on from where it stopped, so output past
    /// the first read shows, and keeps only its last part.
    func testRunningCardReadsOnFromItsCursor() async throws {
        var c = try Self.fixture("basic")[6].command // tail -f, running
        c.state = .running
        let g = Growing(c, limit: 8)
        for i in 1...6 { g.append("line \(i)\r\n") }
        let fake = FakeCalls([CommandOutput(command: c, data: Data(), end: c.output, gap: false, more: true, seq: 1, start: c.output)])
        fake.outputHook = { id, from in id == c.id ? g.read(from) : nil }
        let m = ConsoleModel(pane: "%9", calls: fake.calls, terminal: nil)
        m.readLimit = 8
        m.outputTail = 40
        m.pollEvery = .milliseconds(50)
        m.start()
        defer { m.stop() }
        await eventually("loaded") { m.list.cards.count == 1 }
        m.toggle(c.id)
        await eventually("first read") { m.list.card(c.id)?.output?.last == "line 6" }
        XCTAssertEqual(m.list.card(c.id)?.outputCut, true, "40 bytes keep the last lines only")
        XCTAssertFalse(m.list.card(c.id)?.output?.contains("line 1") ?? true)

        let before = g.lock.withLock { g.froms.count }
        g.append("line 7\r\n")
        await eventually("new line") { m.list.card(c.id)?.output?.last == "line 7" }
        // The poll after the first read started where the first read ended.
        let froms = g.lock.withLock { Array(g.froms[before...]) }
        XCTAssertEqual(froms.first ?? nil, c.output + 48, "read on from the cursor, not from the start")
    }

    /// The command ends while a read of its running output is out: the card
    /// gets the ended output, not the stale running part.
    func testEndDuringAReadReadsAgain() async throws {
        let fx = try Self.fixture("basic")
        let done = fx[0] // ls, ended
        var runningCmd = done.command
        runningCmd.state = .running
        runningCmd.end = nil
        runningCmd.endedAt = nil
        runningCmd.exit = nil
        let fake = FakeCalls([done], listed: 0)
        fake.with { fake.commands = [runningCmd] }
        let gate = Gate()
        fake.outputHook = { id, from in
            guard await gate.isFirst() else { return nil }
            await gate.wait()
            return CommandOutput(command: runningCmd, data: Data("partial\r\n".utf8), end: runningCmd.output + 9, gap: false, more: true, seq: 1, start: runningCmd.output)
        }
        let m = ConsoleModel(pane: "%9", calls: fake.calls, terminal: nil)
        m.pollEvery = .seconds(60)
        m.start()
        defer { m.stop() }
        await eventually("loaded") { m.list.cards.count == 1 }
        m.toggle(runningCmd.id)
        await eventually("read out") { await gate.waiting }
        fake.sink.yield(CommandUpdate(pane: "%9", command: done.command, generation: 1))
        await eventually("ended") { m.list.cards.first?.status == "Exit 0" }
        await gate.open()
        await eventually("ended output") { m.list.card(done.command.id)?.output?.first == "README.md  src" }
        XCTAssertFalse(m.list.card(done.command.id)?.output?.contains("partial") ?? true)
    }

    actor Gate {
        private var first = true
        private(set) var waiting = false
        private var cont: CheckedContinuation<Void, Never>?
        func isFirst() -> Bool { defer { first = false }; return first }
        func wait() async {
            waiting = true
            await withCheckedContinuation { cont = $0 }
        }
        func open() { cont?.resume(); cont = nil }
    }

    func testInputLineTypesTheCommandAndEnter() async {
        let fake = FakeCalls([])
        let m = ConsoleModel(pane: "%9", calls: fake.calls, terminal: nil)
        m.draft = "sleep 2"
        m.send()
        XCTAssertEqual(m.draft, "")
        m.sendRaw("\u{03}")
        await eventually("typed") { fake.with { fake.inputs.count } == 2 }
        XCTAssertEqual(fake.with { fake.inputs }, ["sleep 2\r", "\u{03}"])
    }

    /// A slow first call still lands first: Ctrl+C never overtakes the
    /// command it should stop.
    func testInputKeepsOrderOverASlowCall() async {
        let fake = FakeCalls([])
        fake.with {
            fake.inputHook = { text in
                if text.hasSuffix("\r") { try? await Task.sleep(for: .milliseconds(100)) }
            }
        }
        let m = ConsoleModel(pane: "%9", calls: fake.calls, terminal: nil)
        m.draft = "sleep 2"
        m.send()
        m.sendRaw("\u{03}")
        m.run("echo ok")
        await eventually("typed") { fake.with { fake.inputs.count } == 3 }
        XCTAssertEqual(fake.with { fake.inputs }, ["sleep 2\r", "\u{03}", "echo ok\r"])
    }

    /// A fresh page replaces the list, but a card newer than the page (an
    /// event that came while the page was read) stays.
    func testReplaceKeepsACardNewerThanThePage() throws {
        let fx = try Self.fixture("basic")
        var list = ConsoleCardList()
        list.merge(fx.map(\.command))
        list.replace(CommandList(commands: Array(fx.prefix(3).map(\.command)), more: true, seq: 1))
        XCTAssertEqual(list.cards.map(\.id), fx.prefix(3).map(\.command.id) + fx.suffix(4).map(\.command.id))
        list.replace(CommandList(commands: [fx[6].command], more: false, seq: 2))
        XCTAssertEqual(list.cards.map(\.id), [fx[6].command.id], "older cards not on the page go")
    }

    /// The input line drops control characters and sends pasted lines as one
    /// bracketed paste, so the shell does not run them one by one.
    func testInputLineGuards() {
        XCTAssertEqual(ConsoleModel.inputText("ls\t-la"), "ls\t-la\r")
        XCTAssertEqual(ConsoleModel.inputText("echo \u{1B}[2Jx\u{03}\u{7F}\u{9B}"), "echo [2Jx\r")
        XCTAssertEqual(ConsoleModel.inputText("cd /tmp\nrm -rf x"), "\u{1B}[200~cd /tmp\nrm -rf x\u{1B}[201~\r")
        XCTAssertEqual(ConsoleModel.inputText("a\r\nb"), "\u{1B}[200~a\nb\u{1B}[201~\r")
    }

    func testEmptyListIsLoadedWithNoCards() async {
        let fake = FakeCalls([])
        let m = ConsoleModel(pane: "%9", calls: fake.calls, terminal: nil)
        m.start()
        defer { m.stop() }
        await eventually("loaded") { m.loaded }
        XCTAssertTrue(m.list.cards.isEmpty)
    }

    // MARK: - Alternate screen

    func testScreenModeScanner() {
        func run(_ chunks: [String]) -> [Bool] {
            var s = ScreenModeScanner()
            return chunks.map { s.feed(Data($0.utf8)); return s.alternate }
        }
        XCTAssertEqual(run(["\u{1B}[?1049h", "x", "\u{1B}[?1049l"]), [true, true, false])
        XCTAssertEqual(run(["\u{1B}[?47h", "\u{1B}[?1047l"]), [true, false])
        XCTAssertEqual(run(["\u{1B}[?25;1049h"]), [true], "one of several modes")
        XCTAssertEqual(run(["\u{1B}[1049h", "\u{1B}[?1048h", "\u{1B}[?10490h"]), [false, false, false], "not private, or another mode")
        XCTAssertEqual(run(["\u{1B}[?1049h", "\u{1B}c"]), [true, false], "RIS leaves it")
        XCTAssertEqual(run(["\u{1B}[?1049\u{18}h"]), [false], "CAN aborts")
        XCTAssertEqual(run(["\u{1B}[?1049$h"]), [false], "an intermediate makes another sequence")
        // Split byte by byte.
        var s = ScreenModeScanner()
        for b in "ab\u{1B}[?1049h".utf8 { s.feed(Data([b])) }
        XCTAssertTrue(s.alternate)
    }

    /// The terminal model follows the alternate screen from its stream: the
    /// console switches on it.
    func testTerminalModelFollowsTheAlternateScreen() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 80, rows: 24)
        let conn = HabConnection(id: "mac", transport: hab.transport(), timing: fastTiming(), clientLabel: "mac")
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let t = TerminalModel(pane: "%1", connection: conn)
        t.start()
        defer { t.stop() }
        await eventually("live") { t.status == .live }
        let console = ConsoleModel(pane: "%1", calls: FakeCalls([]).calls, terminal: t)
        XCTAssertFalse(console.showsTerminal)

        await hab.terminals.write("%1", Data("vim\r\n\u{1B}[?1049h\u{1B}[H\u{1B}[2J~".utf8))
        await eventually("alternate") { t.alternateScreen }
        XCTAssertTrue(console.showsTerminal)
        XCTAssertTrue(console.fullScreenProgram)

        await hab.terminals.write("%1", Data("\u{1B}[?10".utf8))
        await hab.terminals.write("%1", Data("49l$ ".utf8))
        await eventually("main screen") { !t.alternateScreen }
        XCTAssertFalse(console.showsTerminal)

        console.terminalChosen = true
        XCTAssertTrue(console.showsTerminal)
    }

    /// A snapshot taken while a full-screen program runs (a reconnect mid-vim)
    /// carries `?1049h`: the console shows the terminal. A later snapshot of
    /// the main screen hands back to the cards.
    func testSnapshotCarriesTheAlternateScreen() async throws {
        let hab = FakeHab(hostID: "box")
        let conn = HabConnection(id: "mac", transport: hab.transport(), timing: fastTiming(), clientLabel: "mac")
        let t = TerminalModel(pane: "%1", connection: conn)
        let console = ConsoleModel(pane: "%1", calls: FakeCalls([]).calls, terminal: t)
        await t.apply(.snapshot(offset: 100, encoding: "vt", data: Data("\u{1B}[?1049h\u{1B}[H~\r\n~".utf8)), generation: 0)
        XCTAssertTrue(t.alternateScreen)
        XCTAssertTrue(console.showsTerminal)
        await t.apply(.snapshot(offset: 200, encoding: "vt", data: Data("$ ".utf8)), generation: 0)
        XCTAssertFalse(t.alternateScreen, "a snapshot starts a new terminal on the main screen")
        XCTAssertFalse(console.showsTerminal)
    }

    // MARK: - Shell setup

    /// The shipped zsh and bash lines encode the folder and the command line
    /// byte by byte, so `#`, `%`, `?`, spaces and non-ASCII text survive.
    func testShellSetupEncodesFolderAndCommand() throws {
        #if os(macOS)
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("fd \(UUID().uuidString.prefix(6))#%?é")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let rc = dir.appendingPathComponent("rc")
        let shells: [(path: String, setup: String, args: [String])] = [
            ("/bin/zsh", ConsoleShellSetup.zsh, ["-f", "-c"]),
            ("/opt/homebrew/bin/bash", ConsoleShellSetup.bash, ["--norc", "-c"]),
            ("/usr/local/bin/bash", ConsoleShellSetup.bash, ["--norc", "-c"]),
        ]
        var ran = 0
        for sh in shells where FileManager.default.isExecutableFile(atPath: sh.path) {
            try ("PS1='$ '\n" + sh.setup + "\n").write(to: rc, atomically: true, encoding: .utf8)
            let p = Process()
            p.executableURL = URL(fileURLWithPath: sh.path)
            p.arguments = sh.args + [#"source "$1"; cd "$2" || exit 9; _fd_precmd; type _fd_preexec >/dev/null 2>&1 && _fd_preexec 'echo é #1'; true"#, sh.path, rc.path, dir.path]
            p.environment = ["PATH": "/usr/bin:/bin", "LANG": "en_US.UTF-8", "HOME": dir.path]
            let out = Pipe()
            p.standardOutput = out
            try p.run()
            p.waitUntilExit()
            XCTAssertEqual(p.terminationStatus, 0, sh.path)
            let text = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            let osc7 = try XCTUnwrap(text.components(separatedBy: "\u{1B}]7;file://").dropFirst().first?.components(separatedBy: "\u{07}").first, sh.path)
            let path = String(osc7.drop { $0 != "/" })
            XCTAssertEqual(path.removingPercentEncoding.map { URL(fileURLWithPath: $0).resolvingSymlinksInPath().path },
                           dir.resolvingSymlinksInPath().path, "\(sh.path): \(path)")
            XCTAssertFalse(path.contains("#") || path.contains("?") || path.contains(" "), path)
            if sh.path.hasSuffix("zsh") {
                // Swift compares strings by canonical equivalence: é may arrive decomposed.
                let line = text.components(separatedBy: "cmdline_url=").dropFirst().first?.components(separatedBy: "\u{07}").first
                XCTAssertEqual(line?.removingPercentEncoding, "echo é #1", "\(sh.path): \(text.debugDescription)")
            }
            ran += 1
        }
        XCTAssertGreaterThan(ran, 0)
        #endif
    }

    // MARK: - Output text

    func testTerminalText() {
        func lines(_ s: String) -> [String] { TerminalText.lines(Data(s.utf8)) }
        XCTAssertEqual(lines("a\r\nb\r\n"), ["a", "b"])
        XCTAssertEqual(lines("\u{1B}[1;34msrc\u{1B}[0m  x\r\n"), ["src  x"])
        XCTAssertEqual(lines("50%\r100%\r\n"), ["100%"])
        XCTAssertEqual(lines("loading...\u{1B}[K\rok\u{1B}[K\r\n"), ["ok"])
        XCTAssertEqual(lines("ab\u{08}c\r\n"), ["ac"])
        XCTAssertEqual(lines("a\tb\r\n"), ["a       b"])
        XCTAssertEqual(lines("x\u{1B}]0;title\u{07}y\u{1B}]8;;http://e\u{1B}\\z\r\n"), ["xyz"])
        XCTAssertEqual(lines("before\r\n\u{1B}[?1049hdrawn\r\nscreen\u{1B}[?1049lafter\r\n"), ["before", "after"])
        XCTAssertEqual(lines("é ✓\r\n"), ["é ✓"])
        XCTAssertEqual(lines("no newline"), ["no newline"])
        XCTAssertEqual(lines(""), [])
        // zsh's partial-line mark (PROMPT_SP) after output that ended with a newline.
        XCTAssertEqual(lines("a\r\n\u{1B}[1m\u{1B}[7m%\u{1B}[27m\u{1B}[1m\u{1B}[0m   \r \r"), ["a"])
    }

    // MARK: - Opening a console

    final class OpenerCalls: @unchecked Sendable {
        let lock = NSLock()
        var store: HabStore
        var created: [SessionCreateParams] = []
        var closed: [String] = []
        /// The folder of the session pane's newest command.
        var cwd: String?
        /// Errors the next creates throw, in order.
        var createErrors: [Error] = []
        /// The store after a create failed (another device's console arrived).
        var storeAfterFailure: HabStore?
        init(_ store: HabStore) { self.store = store }
        var calls: ConsoleOpener.Calls {
            .init(
                store: { [self] in lock.withLock { store } },
                create: { [self] p in
                    try lock.withLock {
                        created.append(p)
                        if !createErrors.isEmpty {
                            if let s = storeAfterFailure { store = s }
                            throw createErrors.removeFirst()
                        }
                    }
                    return SessionCreated(pane: "%new", seq: 1, session: "s-new")
                },
                close: { [self] id in lock.withLock { closed.append(id) } },
                newestCwd: { [self] _ in lock.withLock { cwd } }
            )
        }
    }

    private func store(_ sessions: [Session], panes: [Pane]) -> HabStore {
        HabStore(StructureResult(host: HostInfo(id: "box"), panes: panes,
                                 projects: [Fixture.project("p1", name: "tide", sessions: sessions.map(\.id))], seq: 1, sessions: sessions))
    }

    func testOpenerStartsAShellInTheSessionsProject() async throws {
        let o = OpenerCalls(store([Fixture.session("s1", name: "claude", pane: "%1", project: "p1")], panes: [Fixture.pane("%1", session: "s1")]))
        let pane = try await ConsoleOpener.open(session: "s1", calls: o.calls)
        XCTAssertEqual(pane, "%new")
        XCTAssertEqual(o.created, [SessionCreateParams(name: "claude console", project: "p1")])
        XCTAssertEqual(o.closed, [])
    }

    func testOpenerReusesARunningConsole() async throws {
        let o = OpenerCalls(store([
            Fixture.session("s1", name: "claude", pane: "%1", project: "p1"),
            Fixture.session("s2", name: "claude console", pane: "%2", project: "p1"),
        ], panes: [Fixture.pane("%1", session: "s1"), Fixture.pane("%2", session: "s2")]))
        let pane = try await ConsoleOpener.open(session: "s1", calls: o.calls)
        XCTAssertEqual(pane, "%2")
        XCTAssertTrue(o.created.isEmpty)
    }

    func testOpenerReplacesAnExitedConsole() async throws {
        let old = Fixture.session("s2", name: "claude console", pane: "%2", project: "p1", exited: true)
        let o = OpenerCalls(store([Fixture.session("s1", name: "claude", pane: "%1", project: "p1"), old],
                                  panes: [Fixture.pane("%1", session: "s1")]))
        let pane = try await ConsoleOpener.open(session: "s1", calls: o.calls)
        XCTAssertEqual(pane, "%new")
        XCTAssertEqual(o.closed, ["s2"])
        XCTAssertEqual(o.created.count, 1)
    }

    /// A running console whose pane event has not arrived yet is the
    /// console: it is reused, never closed.
    func testOpenerNeverClosesARunningConsole() async throws {
        let o = OpenerCalls(store([
            Fixture.session("s1", name: "claude", pane: "%1", project: "p1"),
            Fixture.session("s2", name: "claude console", pane: "%2", project: "p1"),
        ], panes: [Fixture.pane("%1", session: "s1")]))
        let pane = try await ConsoleOpener.open(session: "s1", calls: o.calls)
        XCTAssertEqual(pane, "%2")
        XCTAssertEqual(o.closed, [])
        XCTAssertTrue(o.created.isEmpty)

        // Running with no pane yet: an error, and still nothing closed.
        let starting = OpenerCalls(store([
            Fixture.session("s1", name: "claude", pane: "%1", project: "p1"),
            Fixture.session("s2", name: "claude console", pane: nil, project: "p1"),
        ], panes: [Fixture.pane("%1", session: "s1")]))
        do {
            _ = try await ConsoleOpener.open(session: "s1", calls: starting.calls)
            XCTFail("a console still starting gave a pane")
        } catch ConsoleOpener.Failure.starting {}
        XCTAssertEqual(starting.closed, [])
        XCTAssertTrue(starting.created.isEmpty)
    }

    /// The console starts in the folder of the session's newest command; a
    /// folder that is gone leaves the choice to the hab.
    func testOpenerStartsInTheSessionsFolder() async throws {
        let st = store([Fixture.session("s1", name: "sh", pane: "%1", project: "p1")], panes: [Fixture.pane("%1", session: "s1")])
        let o = OpenerCalls(st)
        o.cwd = "/work/tree-b"
        _ = try await ConsoleOpener.open(session: "s1", calls: o.calls)
        XCTAssertEqual(o.created, [SessionCreateParams(cwd: "/work/tree-b", name: "sh console", project: "p1")])

        let gone = OpenerCalls(st)
        gone.cwd = "/work/removed"
        gone.createErrors = [RPCError(code: -32000, message: "invalid", data: ErrorData(hint: "cwd is not a directory", kind: .invalid, seq: 1))]
        let pane = try await ConsoleOpener.open(session: "s1", calls: gone.calls)
        XCTAssertEqual(pane, "%new")
        XCTAssertEqual(gone.created.map(\.cwd), ["/work/removed", nil])
    }

    /// Two opens at once: the second create meets a conflict and returns the
    /// console the first one started.
    func testOpenerConflictReturnsTheOtherConsole() async throws {
        let s1 = Fixture.session("s1", name: "claude", pane: "%1", project: "p1")
        let o = OpenerCalls(store([s1], panes: [Fixture.pane("%1", session: "s1")]))
        o.createErrors = [RPCError(code: -32000, message: "conflict", data: ErrorData(hint: "a session named it exists", kind: .conflict, seq: 1))]
        o.storeAfterFailure = store([s1, Fixture.session("s9", name: "claude console", pane: "%9", project: "p1")],
                                    panes: [Fixture.pane("%1", session: "s1"), Fixture.pane("%9", session: "s9")])
        let pane = try await ConsoleOpener.open(session: "s1", calls: o.calls)
        XCTAssertEqual(pane, "%9")
    }

    func testIsConsole() {
        let a = Fixture.session("s1", name: "claude", pane: "%1", project: "p1")
        let c = Fixture.session("s2", name: "claude console", pane: "%2", project: "p1")
        let lone = Fixture.session("s3", name: "logs console", pane: "%3", project: "p1")
        let st = store([a, c, lone], panes: [])
        XCTAssertTrue(ConsoleOpener.isConsole(c, in: st))
        XCTAssertFalse(ConsoleOpener.isConsole(a, in: st))
        XCTAssertFalse(ConsoleOpener.isConsole(lone, in: st), "no session named logs")

        // The session view hides its Console button on a console.
        var hab = Board.Hab(id: "h", configLabel: nil, endpoint: "local")
        hab.state.store = st
        XCTAssertTrue(ChatContext.of(SessionTarget(habID: "h", session: "s2", pane: nil), hab: hab).isConsole)
        XCTAssertFalse(ChatContext.of(SessionTarget(habID: "h", session: "s1", pane: nil), hab: hab).isConsole)
    }
}
