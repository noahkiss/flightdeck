import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// A hab's state for board tests: online (unless told), its host id, and one session per agent.
@MainActor
func linkHabState(host: String?, online: Bool = true, agents: [(pane: String, session: String, state: AgentStatus, kind: BlockedKind?)]) -> HabState {
    var st = HabState()
    st.status = online ? .online : .offline
    st.store.reset(StructureResult(
        agents: agents.map { Fixture.agent(pane: $0.pane, harness: "claude", state: $0.state, kind: $0.kind,
                                           request: $0.kind == .permission ? Fixture.request(title: "Run?", tool: "Bash", command: "ls") : nil) },
        host: HostInfo(id: host ?? ""), panes: agents.map { Fixture.pane($0.pane, session: $0.session) }, projects: [], seq: 0,
        sessions: agents.map { Fixture.session($0.session, name: $0.session, pane: $0.pane, project: nil) }))
    return st
}

/// Links to a session, Handoff and session windows (app.md 3.14, 3.15).
@MainActor
final class SessionLinkTests: XCTestCase {
    func testLinkRoundTrip() throws {
        let l = SessionLink(hab: "studio host", session: "s-1&x")
        XCTAssertEqual(l.url.scheme, "flightdeck")
        XCTAssertEqual(l.url.host, "session")
        XCTAssertEqual(SessionLink(url: l.url), l, "query values are escaped and read back")
        XCTAssertEqual(SessionLink(url: URL(string: "flightdeck://session?hab=h&session=s")!), SessionLink(hab: "h", session: "s"))
        XCTAssertNil(SessionLink(url: URL(string: "flightdeck://session?hab=h")!), "no session")
        XCTAssertNil(SessionLink(url: URL(string: "flightdeck://session?hab=&session=s")!), "an empty hab")
        XCTAssertNil(SessionLink(url: URL(string: "flightdeck://request?hab=h&session=s")!), "another link")
        XCTAssertNil(SessionLink(url: URL(string: "https://session?hab=h&session=s")!), "another scheme")
    }

    func testHandoffCarriesIdsOnly() throws {
        let l = SessionLink(hab: "h", session: "s")
        XCTAssertEqual(Set(l.userInfo.keys), ["hab", "session"])
        XCTAssertEqual(SessionLink(userInfo: l.userInfo), l)
        XCTAssertNil(SessionLink(userInfo: ["hab": "h"]))
        XCTAssertNil(SessionLink(userInfo: nil))
        XCTAssertTrue(SessionLink.activityType.hasSuffix(".session"))
        XCTAssertEqual(SessionLink.handoffDwell, .seconds(3))
        // A window's value is the same ids, so it survives a restart and restores.
        let data = try JSONEncoder().encode(l)
        XCTAssertEqual(try JSONDecoder().decode(SessionLink.self, from: data), l)
    }

    func testLinkOfARowUsesTheHostID() {
        let board = Board()
        board.register(id: "conn-1", label: "Studio", endpoint: "fake")
        board.update("conn-1", linkHabState(host: "studio-host", agents: [("%1", "s1", .idle, nil)]))
        let row = board.rows[0]
        XCTAssertEqual(SessionLink.of(row, board: board), SessionLink(hab: "studio-host", session: "s1"))
        let pane = boardRow("conn-1/p/%9", hab: "conn-1", session: nil, pane: "%9")
        XCTAssertNil(SessionLink.of(pane, board: board), "an unfiled pane has no link")
    }

    func testResolveByHostIDOrConnectionID() {
        let board = Board()
        board.register(id: "conn-1", label: nil, endpoint: "fake")
        board.register(id: "conn-2", label: nil, endpoint: "fake")
        // conn-2 has not said its host id: a link to an unknown host waits for it.
        board.update("conn-1", linkHabState(host: "alpha", agents: [("%1", "s1", .idle, nil)]))
        XCTAssertEqual(SessionLink(hab: "alpha", session: "s1").resolve(in: board), .row(board.rows.first { $0.id == "conn-1/s/s1" }!))
        XCTAssertEqual(SessionLink(hab: "conn-1", session: "s1").resolve(in: board).rowID, "conn-1/s/s1", "the local id works on this device")
        XCTAssertEqual(SessionLink(hab: "alpha", session: "s9").resolve(in: board), .waiting, "the session may still come")
        XCTAssertEqual(SessionLink(hab: "beta", session: "s1").resolve(in: board), .waiting, "conn-2 may be beta")
        board.update("conn-2", linkHabState(host: "gamma", agents: []))
        XCTAssertEqual(SessionLink(hab: "beta", session: "s1").resolve(in: board), .unknownHab, "every hab said its host id")
    }

    func testPendingLinkWaitsAMinute() {
        let start = Date()
        let p = PendingLink(SessionLink(hab: "h", session: "s"), now: start)
        XCTAssertFalse(p.expired(now: start.addingTimeInterval(SessionRestore.limit)))
        XCTAssertTrue(p.expired(now: start.addingTimeInterval(SessionRestore.limit + 1)))
    }

    func testFleetHandsSessionLinksBack() {
        let fleet = Fleet()
        let l = SessionLink(hab: "h", session: "s")
        XCTAssertEqual(fleet.open(url: l.url), l)
        XCTAssertNil(fleet.open(url: URL(string: "flightdeck://usage?account=x")!), "other links stay with their handlers")
        XCTAssertNil(fleet.open(url: URL(string: "flightdeck://session?hab=h")!), "a malformed session link is ignored")
    }

    /// Only known links reach a handler; a malformed session link and an unknown host are
    /// ignored, never handed to pairing (app.md 3.15).
    func testLinkKinds() {
        func kind(_ s: String) -> AppLink { AppLink.kind(URL(string: s)!) }
        XCTAssertEqual(kind("flightdeck://session?hab=h&session=s"), .session(SessionLink(hab: "h", session: "s")))
        XCTAssertEqual(kind("flightdeck://session?hab=h"), .unknown)
        XCTAssertEqual(kind("flightdeck://session"), .unknown)
        XCTAssertEqual(kind("flightdeck://anything?x=1"), .unknown)
        XCTAssertEqual(kind("flightdeck://"), .unknown)
        XCTAssertEqual(kind("https://example.com/pair"), .unknown)
        XCTAssertEqual(kind("flightdeck://pair?v=1"), .pair)
        XCTAssertEqual(kind("flightdeck://usage?account=x"), .usage)
        XCTAssertEqual(kind("flightdeck://request?hab=h"), .request)
        XCTAssertEqual(AppLink.kind(URL(fileURLWithPath: "/tmp/x.txt")), .file)
    }
}

extension SessionLink.Resolution {
    var rowID: String? {
        if case .row(let r) = self { return r.id }
        return nil
    }
}
