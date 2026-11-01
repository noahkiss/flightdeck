import Darwin
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

@MainActor
final class BoardTests: XCTestCase {
    func testTwoHabsMergeIntoOneList() async {
        let a = await sampleHab("alpha-host")
        let b = FakeHab(hostID: "beta-host")
        await b.load(
            projects: [Fixture.project("p1", name: "tide-tables", sessions: ["s1"])],
            sessions: [Fixture.session("s1", name: "claude", pane: "%1", project: "p1")],
            panes: [Fixture.pane("%1", session: "s1")],
            agents: [Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .question)]
        )
        let fleet = Fleet()
        fleet.add(HabConnection(id: "a", transport: a.transport(), timing: fastTiming()), label: nil)
        fleet.add(HabConnection(id: "b", transport: b.transport(), timing: fastTiming()), label: "beta")
        await fleet.setActive(true)
        await eventually("both online") { fleet.board.onlineCount == 2 }
        await eventually("rows") { fleet.board.rows.count == 3 }

        let rows = fleet.board.rows
        // The same ids on two habs stay two rows, each labelled by its hab.
        XCTAssertEqual(Set(rows.map(\.id)), ["a/s/s1", "a/s/s2", "b/s/s1"])
        XCTAssertEqual(rows.first?.id, "b/s/s1", "blocked ranks first")
        XCTAssertEqual(rows.first?.host, "beta")
        XCTAssertEqual(rows.first { $0.id == "a/s/s1" }?.host, "alpha-host", "no config label: the host id")
        XCTAssertEqual(fleet.board.sections.map(\.kind), [.needsYou, .working, .other])
        XCTAssertEqual(fleet.board.needsYouCount, 1)

        // An event on one hab changes only that hab's row.
        await a.put(Fixture.agent(pane: "%1", harness: "claude", state: .error))
        await eventually("error row") { fleet.board.rows.first { $0.id == "a/s/s1" }?.rank == .error }
        XCTAssertEqual(fleet.board.rows.first { $0.id == "b/s/s1" }?.rank, .blocked)
        await fleet.setActive(false)
    }

    func testRanksAndSections() {
        func rank(_ st: AgentStatus?, exited: Bool = false, seen: Bool = false) -> BoardRank {
            let s = Fixture.session("s", name: "n", pane: "%1", project: nil, exited: exited)
            return BoardRows.rank(session: s, agent: st.map { Fixture.agent(pane: "%1", harness: "claude", state: $0) }, seen: seen)
        }
        XCTAssertEqual(rank(.blocked), .blocked)
        XCTAssertEqual(rank(.error), .error)
        XCTAssertEqual(rank(.done), .doneUnseen)
        XCTAssertEqual(rank(.done, seen: true), .atRest)
        XCTAssertEqual(rank(.working), .working)
        XCTAssertEqual(rank(.idle), .atRest)
        XCTAssertEqual(rank(.none), .other)
        XCTAssertEqual(rank(.exited), .other)
        XCTAssertEqual(rank(nil), .other)
        XCTAssertEqual(rank(.working, exited: true), .other)
        XCTAssertEqual(BoardRank.doneUnseen.section, .needsYou)
        XCTAssertEqual(BoardRank.atRest.section, .done)
    }

    func testRowTitleAndContextFromPermission() {
        var store = HabStore()
        store.reset(StructureResult(
            agents: [Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission,
                                   request: Fixture.request(title: "Run tests?", tool: "Bash", command: "make test"))],
            host: HostInfo(id: "h"), panes: [Fixture.pane("%1", session: "s1"), Fixture.pane("%9", session: nil)],
            projects: [Fixture.project("p1", name: "tide-tables")], seq: 0,
            sessions: [Fixture.session("s1", name: "claude", pane: "%1", project: "p1", model: "m", title: "Session title")]))
        let rows = BoardRows.build(habID: "x", host: "h", store: store, seen: [])
        let r = rows.first { $0.id == "x/s/s1" }!
        XCTAssertEqual(r.title, "Run tests?")
        XCTAssertEqual(r.context, "make test")
        XCTAssertEqual(r.project, "tide-tables")
        XCTAssertEqual(r.host, "h")
        let unfiled = rows.first { $0.id == "x/p/%9" }!
        XCTAssertEqual(unfiled.rank, .other)
        XCTAssertNil(unfiled.target.session)
    }

    func testOpenMarksDoneSeenAndCallsHook() async {
        let board = Board()
        board.register(id: "h", label: nil, endpoint: "fake")
        var st = HabState()
        st.status = .online
        st.store.reset(StructureResult(
            agents: [Fixture.agent(pane: "%1", harness: "claude", state: .done)],
            host: HostInfo(id: "h"), panes: [Fixture.pane("%1", session: "s1")], projects: [], seq: 0,
            sessions: [Fixture.session("s1", name: "claude", pane: "%1", project: nil)]))
        board.update("h", st)
        XCTAssertEqual(board.rows.first?.rank, .doneUnseen)
        var opened: SessionTarget?
        board.onOpen = { opened = $0 }
        board.open(board.rows[0])
        XCTAssertEqual(opened, SessionTarget(habID: "h", session: "s1", pane: "%1"))
        XCTAssertEqual(board.rows.first?.rank, .atRest)
        XCTAssertEqual(board.needsYouCount, 0)
    }

    func testRowMovesOnlyWhenItsGroupChanges() {
        var clock = Date(timeIntervalSince1970: 1_000_000)
        let board = Board(now: { clock })
        board.register(id: "h", label: nil, endpoint: "fake")
        func state(_ agents: [(String, AgentStatus)]) -> HabState {
            var st = HabState()
            st.status = .online
            st.store.reset(StructureResult(
                agents: agents.map { Fixture.agent(pane: $0.0, harness: "claude", state: $0.1, at: Date(timeIntervalSince1970: 0)) },
                host: HostInfo(id: "h"),
                panes: agents.map { Fixture.pane($0.0, session: "s" + $0.0) }, projects: [], seq: 0,
                sessions: agents.map { Fixture.session("s" + $0.0, name: $0.0, pane: $0.0, project: nil) }))
            return st
        }
        board.update("h", state([("a", .working), ("b", .working)]))
        let firstOrder = board.rows.map(\.id)
        // Another change inside the same group does not move rows.
        clock += 10
        var st = state([("a", .working), ("b", .working)])
        var s = st.store.sessions["sb"]!
        s.title = "renamed"
        st.store.reset(StructureResult(agents: Array(st.store.agents.values), host: HostInfo(id: "h"),
                                       panes: Array(st.store.panes.values), projects: [], seq: 0,
                                       sessions: [st.store.sessions["sa"]!, s]))
        board.update("h", st)
        XCTAssertEqual(board.rows.map(\.id), firstOrder)
        // b leaves working and comes back: now it is the newest in the group.
        clock += 10
        board.update("h", state([("a", .working), ("b", .idle)]))
        clock += 10
        board.update("h", state([("a", .working), ("b", .working)]))
        XCTAssertEqual(board.rows.map(\.id), ["h/s/sb", "h/s/sa"])
    }

    func testOfflineRowsDimAndUpdateNeededHides() {
        let board = Board()
        board.register(id: "h", label: "desk", endpoint: "fake")
        var st = HabState()
        st.store.reset(StructureResult(agents: [], host: HostInfo(id: "h"), panes: [], projects: [], seq: 0,
                                       sessions: [Fixture.session("s1", name: "n", pane: nil, project: nil)]))
        let lost = Date(timeIntervalSince1970: 5000)
        st.status = .offline
        st.lostAt = lost
        board.update("h", st)
        XCTAssertEqual(board.rows.first?.offline, true)
        XCTAssertEqual(board.rows.first?.offlineSince, lost)
        st.status = .updateNeeded(.hab)
        board.update("h", st)
        XCTAssertTrue(board.rows.isEmpty, "a hab that needs an update shows no stale rows")
    }

    func testDemoModeShowsBothHostsAcrossRanks() async {
        let fleet = Fleet()
        await DemoFleet.populate(fleet, script: false)
        await fleet.setActive(true)
        await eventually("both online") { fleet.board.onlineCount == 2 }
        await eventually("rows") { fleet.board.rows.count >= 10 }
        let rows = fleet.board.rows
        XCTAssertEqual(Set(rows.map(\.host)), ["workbench", "attic"])
        XCTAssertEqual(Set(rows.map(\.rank)), Set(BoardRank.allCases), "demo covers every rank")
        await fleet.setActive(false)
    }
}

/// The app holds no open stream 10 s after it goes to the background.
@MainActor
final class LifecycleTests: XCTestCase {
    func testBackgroundClosesEveryFakeStream() async {
        let a = await sampleHab("one")
        let b = await sampleHab("two")
        let fleet = Fleet()
        fleet.checkDelay = .milliseconds(200)
        fleet.add(HabConnection(id: "a", transport: a.transport(), timing: fastTiming()), label: nil)
        fleet.add(HabConnection(id: "b", transport: b.transport(), timing: fastTiming()), label: nil)
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 2 }
        XCTAssertEqual(fleet.openStreams(), 2)
        await fleet.setActive(false)
        XCTAssertEqual(fleet.openStreams(), 0, "closed when setActive returns")
        await eventually("background check ran") { fleet.lastBackgroundCheck != nil }
        XCTAssertEqual(fleet.lastBackgroundCheck, 0)
        await eventually("habs saw the close") { let x = await a.connectionCount; let y = await b.connectionCount; return x == 0 && y == 0 }
        // Rows stay while in the background; the hab shows disconnected.
        XCTAssertEqual(fleet.board.rows.count, 4)
        XCTAssertTrue(fleet.board.habs.allSatisfy { $0.state.status == .stopped })
        // Back in the foreground: reconnect, with the token.
        await fleet.setActive(true)
        await eventually("online again") { fleet.board.onlineCount == 2 }
        let hellos = await a.hellos
        XCTAssertNotNil(hellos.last?.reconnectToken)
        await fleet.setActive(false)
    }

    func testMacLastWindowClosedDisconnects() async {
        let a = await sampleHab()
        let fleet = Fleet()
        fleet.add(HabConnection(id: "a", transport: a.transport(), timing: fastTiming()), label: nil)
        fleet.windowOpened()
        fleet.windowOpened()
        await eventually("online") { fleet.board.onlineCount == 1 }
        fleet.windowClosed()
        try? await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(fleet.board.onlineCount, 1, "one window still open")
        fleet.windowClosed()
        await eventually("closed") { fleet.openStreams() == 0 && !fleet.isActive }
    }

    /// The same check over a real Unix socket: the fd closes and the peer sees end of file.
    func testBackgroundClosesARealSocket() async throws {
        let dir = URL(fileURLWithPath: "/tmp").appendingPathComponent("fdk-\(getpid())-\(UInt32.random(in: 0...UInt32.max))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let path = dir.appendingPathComponent("hab.sock").path
        let server = try SocketHab(path: path)
        defer { server.shutdown() }

        let before = OpenStreams.live
        let fleet = Fleet()
        fleet.checkDelay = .milliseconds(200)
        fleet.add(HabConnection(id: "u", transport: UnixSocketTransport(path: path), timing: fastTiming()), label: nil)
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 1 }
        XCTAssertEqual(OpenStreams.live, before + 1)
        await fleet.setActive(false)
        await eventually("fd closed") { OpenStreams.live == before }
        await eventually("peer saw EOF") { server.closedConnections == 1 }
        await eventually("background check ran") { fleet.lastBackgroundCheck != nil }
        XCTAssertEqual(fleet.lastBackgroundCheck, 0)
    }
}

@MainActor
final class ConfigTests: XCTestCase {
    func testDecodeAndTransports() throws {
        let json = #"{"habs":[{"label":"desk","local":{"socket":"/tmp/x/hab.sock"}},{"ssh":{"host":"buildbox","options":["-p","2222"]}}]}"#
        let c = try HabListConfig.decode(Data(json.utf8))
        XCTAssertEqual(c.habs.count, 2)
        let t0 = try c.habs[0].transport(environment: [:]) as! UnixSocketTransport
        XCTAssertEqual(t0.path, "/tmp/x/hab.sock")
        let t1 = try c.habs[1].transport(environment: [:]) as! SSHTransport
        XCTAssertEqual(try t1.arguments(), ["-p", "2222", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
                                            "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
                                            "--", "buildbox", "gff-hab", "connect"])
        XCTAssertEqual(c.habs[1].id(index: 1), "ssh:buildbox")
    }

    func testEntryNeedsOneEndpoint() {
        XCTAssertThrowsError(try HabListConfig.decode(Data(#"{"habs":[{"label":"x"}]}"#.utf8)))
        XCTAssertThrowsError(try HabListConfig.decode(Data(#"{"habs":[{"local":{},"ssh":{"host":"h"}}]}"#.utf8)))
    }

    func testMissingFileIsLocalOnlyAndEnvPathWins() throws {
        let missing = ["FLIGHTDECK_HABS": "/tmp/does-not-exist-\(UUID().uuidString).json"]
        XCTAssertEqual(try HabListConfig.load(environment: missing), .localOnly)
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("habs-\(UUID().uuidString).json")
        try Data(#"{"habs":[{"ssh":{"host":"h1"}}]}"#.utf8).write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }
        let c = try HabListConfig.load(environment: ["FLIGHTDECK_HABS": url.path])
        XCTAssertEqual(c.habs.first?.ssh?.host, "h1")
    }

    func testDefaultSocketPath() {
        XCTAssertEqual(UnixSocketTransport.defaultPath(environment: ["GFF_HAB_SOCKET": "/tmp/s.sock"]), "/tmp/s.sock")
        let d = UnixSocketTransport.defaultPath(environment: [:])
        XCTAssertTrue(d?.hasSuffix("Library/Application Support/gff/hab.sock") ?? false, d ?? "nil")
    }

    func testSSHRefusesOptionLikeHost() {
        XCTAssertThrowsError(try SSHTransport(host: "-oProxyCommand=x").arguments())
        XCTAssertThrowsError(try SSHTransport(host: "a b").arguments())
    }
}

/// A tiny hab on a real Unix socket: answers hello and subscribe, counts closed connections.
final class SocketHab: @unchecked Sendable {
    private let fd: Int32
    private let lock = NSLock()
    private var closed = 0
    private var stopped = false

    var closedConnections: Int {
        lock.lock(); defer { lock.unlock() }
        return closed
    }

    init(path: String) throws {
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            raw.copyBytes(from: bytes)
            raw[bytes.count] = 0
        }
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard rc == 0, listen(fd, 4) == 0 else { throw TransportError("bind \(path): \(String(cString: strerror(errno)))") }
        let t = Thread { [self] in acceptLoop() }
        t.start()
    }

    func shutdown() {
        lock.lock(); stopped = true; lock.unlock()
        Darwin.shutdown(fd, SHUT_RDWR)
        Darwin.close(fd)
    }

    private func acceptLoop() {
        while true {
            let c = accept(fd, nil, nil)
            if c < 0 { return }
            let t = Thread { [self] in serve(c) }
            t.start()
        }
    }

    private func serve(_ c: Int32) {
        var on: Int32 = 1
        setsockopt(c, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        var lines = LineBuffer()
        var buf = [UInt8](repeating: 0, count: 4096)
        func send(_ s: String) {
            let d = Array((s + "\n").utf8)
            _ = d.withUnsafeBytes { Darwin.write(c, $0.baseAddress, $0.count) }
        }
        loop: while true {
            let n = buf.withUnsafeMutableBytes { Darwin.read(c, $0.baseAddress, $0.count) }
            if n <= 0 { break }
            for line in (try? lines.append(Data(buf[0..<n]))) ?? [] {
                guard let obj = (try? JSONSerialization.jsonObject(with: line)) as? [String: Any],
                      let id = obj["id"] as? Int, let m = obj["method"] as? String else { continue }
                switch m {
                case "hab.hello":
                    send(#"{"jsonrpc":"2.0","id":\#(id),"result":{"build":"t","capabilities":[],"client_id":"c1","epoch":"e1","host":{"id":"sockhost"},"reconnect_token":"tok","revision":1,"seq":0}}"#)
                case "events.subscribe":
                    send(#"{"jsonrpc":"2.0","id":\#(id),"result":{"host":{"id":"sockhost"},"panes":[],"projects":[],"seq":0,"sessions":[]}}"#)
                default:
                    send(#"{"jsonrpc":"2.0","id":\#(id),"result":{"seq":0}}"#)
                }
            }
        }
        Darwin.close(c)
        lock.lock(); closed += 1; lock.unlock()
    }
}
