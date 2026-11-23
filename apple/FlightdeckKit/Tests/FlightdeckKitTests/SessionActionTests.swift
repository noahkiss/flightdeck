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
        await c.rename(try XCTUnwrap(row(f, "s1")), to: " tide-fix ")
        XCTAssertNil(c.failure)
        await eventually("renamed") { self.row(f, "s1")?.name == "tide-fix" }

        // The hab's refusals come back in its words, as a sentence.
        await c.rename(try XCTUnwrap(row(f, "s1")), to: "docs")
        XCTAssertEqual(c.failure?.title, "Couldn't rename tide-fix")
        XCTAssertEqual(c.failure?.message, "A session named \"docs\" exists.")
        c.failure = nil
        await c.rename(try XCTUnwrap(row(f, "s1")), to: "two words")
        XCTAssertEqual(c.failure?.message, "Session name \"two words\": use 1 to 64 of A-Z a-z 0-9 . _ -.")
        c.failure = nil
        await c.rename(try XCTUnwrap(row(f, "s1")), to: "capcom-x")
        XCTAssertTrue(c.failure?.message?.contains("kept for the Capcom service") ?? false)
        c.failure = nil

        // A blank or unchanged name sends nothing.
        let before = await hab.sessionActions.count
        await c.rename(try XCTUnwrap(row(f, "s1")), to: "  ")
        await c.rename(try XCTUnwrap(row(f, "s1")), to: "tide-fix")
        let after = await hab.sessionActions.count
        XCTAssertEqual(after, before)
        await f.setActive(false)
    }

    func testCloseEndsARunningSessionAndRemovesAnExitedOne() async throws {
        let (f, hab) = await setup()
        let c = f.sessionActions
        await c.close(try XCTUnwrap(row(f, "s2")))
        XCTAssertNil(c.failure)
        await eventually("exited") { self.row(f, "s2")?.sessionExited == true }
        XCTAssertNil(c.removed, "an ended session stays, exited")
        var calls = await hab.sessionActions
        XCTAssertEqual(calls, ["pane.close %2"])

        // Close again: the record goes.
        let exited = try XCTUnwrap(row(f, "s2"))
        XCTAssertEqual(SessionActionCenter.closeKind(exited), .remove)
        await c.close(exited)
        await eventually("removed") { self.row(f, "s2") == nil }
        XCTAssertEqual(c.removed, exited.id)
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
        await c.close(asked)
        XCTAssertEqual(c.failure?.title, "tides has ended.")
        let calls = await hab.sessionActions
        XCTAssertEqual(calls, [])
        await f.setActive(false)
    }

    func testRestartMovesToTheNewPane() async throws {
        let (f, hab) = await setup()
        let c = f.sessionActions
        await c.restart(try XCTUnwrap(row(f, "s1")))
        XCTAssertNil(c.failure)
        let t = try XCTUnwrap(c.restarted)
        XCTAssertEqual(t.session, "s1")
        XCTAssertNotEqual(t.pane, "%1")
        await eventually("the new pane") { self.row(f, "s1")?.target.pane == t.pane }

        // The hab refuses while the agent works; the alert shows its words.
        await c.restart(try XCTUnwrap(row(f, "s2")))
        XCTAssertEqual(c.failure?.title, "Couldn't restart docs")
        XCTAssertEqual(c.failure?.message, "Session docs: the agent is working; a restart waits until it is idle or done.")
        let calls = await hab.sessionActions
        XCTAssertEqual(calls, ["restart s1", "restart s2"])
        await f.setActive(false)
    }

    func testAnOfflineHabFails() async throws {
        let (f, _) = await setup()
        let r = try XCTUnwrap(row(f, "s1"))
        await f.setActive(false)
        await f.sessionActions.rename(r, to: "elsewhere")
        XCTAssertEqual(f.sessionActions.failure?.title, "Couldn't rename tides")
        XCTAssertNotNil(f.sessionActions.failure?.message)
    }
}
