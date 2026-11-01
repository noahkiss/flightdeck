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
                    let o: CommandOutput? = with {
                        outputReads += 1
                        return outputs[id]
                    }
                    guard let o else { throw RPCError(code: -1, message: "not found", data: nil) }
                    return o
                },
                input: { [self] text in with { inputs.append(text) } },
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
        init(_ store: HabStore) { self.store = store }
        var calls: ConsoleOpener.Calls {
            .init(
                store: { [self] in lock.withLock { store } },
                create: { [self] p in lock.withLock { created.append(p) }; return SessionCreated(pane: "%new", seq: 1, session: "s-new") },
                close: { [self] id in lock.withLock { closed.append(id) } }
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
}
