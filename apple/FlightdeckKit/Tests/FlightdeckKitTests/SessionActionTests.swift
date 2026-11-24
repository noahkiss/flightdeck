import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Close, rename and restart from a row (app.md 3.11), over the fake hab, which answers as
/// the real one.
@MainActor
final class SessionActionTests: XCTestCase {
    private func setup(restart: Bool = true) async -> (Fleet, FakeHab) {
        let hab = FakeHab(hostID: "bench")
        await hab.load(
            projects: [Fixture.project("p1", name: "tide-tables", sessions: ["s1", "s2", "s3"])],
            sessions: [
                Fixture.session("s1", name: "tides", pane: "%1", project: "p1"),
                Fixture.session("s2", name: "docs", pane: "%2", project: "p1"),
                Fixture.session("s3", name: "old", pane: nil, project: "p1", exited: true),
            ],
            panes: [Fixture.pane("%1", session: "s1"), Fixture.pane("%2", session: "s2"), Fixture.pane("%9", session: nil)],
            agents: [
                Fixture.agent(pane: "%1", harness: "claude", state: .idle, session: "native-1"),
                Fixture.agent(pane: "%2", harness: "codex", state: .working, session: "native-2"),
            ])
        await hab.setCapability(SessionActionCenter.restartCapability, restart)
        let f = Fleet()
        f.add(HabConnection(id: "bench", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await f.setActive(true)
        await eventually("online") { f.board.rows.count == 4 }
        return (f, hab)
    }

    private func row(_ f: Fleet, _ session: String) -> BoardRow? { f.board.rows.first { $0.target.session == session } }

    func testWhatARowOffers() async throws {
        let (f, _) = await setup()
        let c = f.sessionActions
        XCTAssertEqual(c.offers(try XCTUnwrap(row(f, "s1"))), [.rename, .restart, .copyName, .close], "idle, on a hab that restarts")
        XCTAssertEqual(c.offers(try XCTUnwrap(row(f, "s2"))), [.rename, .copyName, .close], "working: no restart")
        XCTAssertEqual(c.offers(try XCTUnwrap(row(f, "s3"))), [.rename, .copyName, .close], "exited: no restart")
        let unfiled = try XCTUnwrap(f.board.rows.first { $0.target.session == nil })
        XCTAssertEqual(c.offers(unfiled), [.close])
        XCTAssertEqual(SessionActionCenter.closeKind(unfiled), .endPane)
        XCTAssertEqual(SessionActionCenter.closeKind(try XCTUnwrap(row(f, "s1"))), .end)
        XCTAssertEqual(SessionActionCenter.closeKind(try XCTUnwrap(row(f, "s3"))), .remove)

        var off = try XCTUnwrap(row(f, "s1"))
        off.offline = true
        XCTAssertEqual(c.offers(off), [.copyName], "an offline hab's row: copy only")
        var owned = try XCTUnwrap(row(f, "s1"))
        owned.ownedElsewhere = true
        XCTAssertEqual(c.offers(owned), [.copyName], "Capcom and watcher sessions: copy only")
        XCTAssertEqual(SessionActionCenter.question(try XCTUnwrap(row(f, "s1"))).title, "End tides on bench?")
        XCTAssertEqual(SessionActionCenter.question(try XCTUnwrap(row(f, "s3"))).button, "Remove")
        await f.setActive(false)
    }

    func testRestartNeedsTheCapability() async throws {
        let (f, _) = await setup(restart: false)
        XCTAssertEqual(f.sessionActions.offers(try XCTUnwrap(row(f, "s1"))), [.rename, .copyName, .close])
        await f.setActive(false)
    }

    func testRename() async throws {
        let (f, hab) = await setup()
        let c = f.sessionActions
        var fail = await c.rename(try XCTUnwrap(row(f, "s1")), to: " tide-fix ")
        XCTAssertNil(fail)
        await eventually("renamed") { self.row(f, "s1")?.name == "tide-fix" }

        // The hab's refusals come back in its words, as a sentence.
        fail = await c.rename(try XCTUnwrap(row(f, "s1")), to: "docs")
        XCTAssertEqual(fail?.title, "Couldn't rename tide-fix")
        XCTAssertEqual(fail?.message, "A session named \"docs\" exists.")
        fail = await c.rename(try XCTUnwrap(row(f, "s1")), to: "two words")
        XCTAssertEqual(fail?.message, "Session name \"two words\": use 1 to 64 of A-Z a-z 0-9 . _ -.")
        fail = await c.rename(try XCTUnwrap(row(f, "s1")), to: "capcom-x")
        XCTAssertTrue(fail?.message?.contains("kept for the Capcom service") ?? false)

        // A blank or unchanged name sends nothing.
        let before = await hab.sessionActions.count
        fail = await c.rename(try XCTUnwrap(row(f, "s1")), to: "  ")
        XCTAssertNil(fail)
        fail = await c.rename(try XCTUnwrap(row(f, "s1")), to: "tide-fix")
        XCTAssertNil(fail)
        let after = await hab.sessionActions.count
        XCTAssertEqual(after, before)
        XCTAssertTrue(c.running.isEmpty, "no row stays marked running")
        await f.setActive(false)
    }

    func testCloseEndsARunningSessionAndRemovesAnExitedOne() async throws {
        let (f, hab) = await setup()
        let c = f.sessionActions
        let fail = await c.close(try XCTUnwrap(row(f, "s2")))
        XCTAssertNil(fail)
        await eventually("exited") { self.row(f, "s2")?.sessionExited == true }
        XCTAssertNil(c.removal, "an ended session stays, exited")
        var calls = await hab.sessionActions
        XCTAssertEqual(calls, ["pane.close %2"])

        // Close again: the record goes, and every window hears of it.
        let exited = try XCTUnwrap(row(f, "s2"))
        XCTAssertEqual(SessionActionCenter.closeKind(exited), .remove)
        let again = await c.close(exited)
        XCTAssertNil(again)
        await eventually("removed") { self.row(f, "s2") == nil }
        XCTAssertEqual(c.removal?.rowID, exited.id)
        calls = await hab.sessionActions
        XCTAssertEqual(calls.last, "close s2")
        await f.setActive(false)
    }

    func testEndAfterTheProgramEndedSendsNothing() async throws {
        let (f, hab) = await setup()
        let c = f.sessionActions
        let asked = try XCTUnwrap(row(f, "s1"))
        // The program ends while the question is up.
        await hab.endPane("%1")
        await eventually("exited") { self.row(f, "s1")?.sessionExited == true }
        let fail = await c.close(asked)
        XCTAssertEqual(fail?.title, "tides has ended.")
        let calls = await hab.sessionActions
        XCTAssertEqual(calls, [])
        await f.setActive(false)
    }

    func testRemoveAfterTheSessionRunsAgainSendsNothing() async throws {
        let (f, hab) = await setup()
        let c = f.sessionActions
        let asked = try XCTUnwrap(row(f, "s3"))
        XCTAssertEqual(SessionActionCenter.closeKind(asked), .remove)
        // Another client resumes the session while the question is up.
        await hab.put(Fixture.pane("%7", session: "s3"))
        await hab.put(Fixture.session("s3", name: "old", pane: "%7", project: "p1"))
        await eventually("running") { self.row(f, "s3")?.sessionExited == false }
        let fail = await c.close(asked)
        XCTAssertEqual(fail?.title, "old is running again.")
        XCTAssertNil(c.removal)
        let calls = await hab.sessionActions
        XCTAssertEqual(calls, [], "no session.close for a running session: its program would be left unfiled")
        await f.setActive(false)
    }

    func testRestartMovesToTheNewPane() async throws {
        let (f, hab) = await setup()
        let c = f.sessionActions
        var fail = await c.restart(try XCTUnwrap(row(f, "s1")))
        XCTAssertNil(fail)
        let t = try XCTUnwrap(c.restart?.target)
        XCTAssertEqual(t.session, "s1")
        XCTAssertNotEqual(t.pane, "%1")
        await eventually("the new pane") { self.row(f, "s1")?.target.pane == t.pane }

        // The hab refuses while the agent works; the alert shows its words.
        fail = await c.restart(try XCTUnwrap(row(f, "s2")))
        XCTAssertEqual(fail?.title, "Couldn't restart docs")
        XCTAssertEqual(fail?.message, "Session docs: the agent is working; a restart waits until it is idle or done.")
        XCTAssertEqual(c.restart?.target, t, "a refusal is no restart to follow")
        let calls = await hab.sessionActions
        XCTAssertEqual(calls, ["restart s1", "restart s2"])
        await f.setActive(false)
    }

    func testAnOfflineHabFails() async throws {
        let (f, _) = await setup()
        let r = try XCTUnwrap(row(f, "s1"))
        await f.setActive(false)
        let fail = await f.sessionActions.rename(r, to: "elsewhere")
        XCTAssertEqual(fail?.title, "Couldn't rename tides")
        XCTAssertNotNil(fail?.message)
    }

    func testAskShowsItsFailureInItsOwnWindow() async {
        let a = SessionAsk(), b = SessionAsk()
        let r = boardRow("bench/s/s1")
        a.askClose(r)
        XCTAssertTrue(a.closingShown)
        XCTAssertFalse(b.closingShown, "a question shows only in the window that asked")
        await a.run { SessionActionFailure(title: "claude has ended.") }
        XCTAssertFalse(a.closingShown)
        XCTAssertEqual(a.closing?.id, r.id, "the question keeps its row while it dismisses")
        XCTAssertEqual(a.failure?.title, "claude has ended.")
        XCTAssertNil(b.failure)
    }
}
