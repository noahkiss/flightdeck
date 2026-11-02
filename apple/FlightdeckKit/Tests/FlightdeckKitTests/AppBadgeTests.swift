import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The app badge (app.md 7.9): the needs-you count, written only while the
/// app is in front with a hab online, and only on a change.
@MainActor
final class AppBadgeTests: XCTestCase {
    func testWhenItWrites() {
        XCTAssertTrue(AppBadge.writes(active: true, online: 1, habs: 2))
        XCTAssertTrue(AppBadge.writes(active: true, online: 0, habs: 0), "no habs: 0 is the true count")
        XCTAssertFalse(AppBadge.writes(active: true, online: 0, habs: 2), "every hab offline: keep the last count")
        XCTAssertFalse(AppBadge.writes(active: false, online: 2, habs: 2), "background: keep the last count")
    }

    func testDockLabel() {
        XCTAssertEqual(AppBadge.label(0), "")
        XCTAssertEqual(AppBadge.label(3), "3")
        XCTAssertEqual(AppBadge.label(-1), "")
    }

    func testWritesOnChangeOnlyAndRetriesARefusal() async {
        var writes: [Int] = []
        var allowed = false
        let b = AppBadge { n in
            writes.append(n)
            return allowed
        }
        b.update(2, active: true, online: 1, habs: 1)
        await b.settle()
        XCTAssertEqual(writes, [2])
        XCTAssertNil(b.shown, "refused: nothing shown")
        allowed = true
        b.update(2, active: true, online: 1, habs: 1)
        await b.settle()
        XCTAssertEqual(writes, [2, 2], "a refused write is tried again")
        XCTAssertEqual(b.shown, 2)
        b.update(2, active: true, online: 1, habs: 1)
        await b.settle()
        XCTAssertEqual(writes, [2, 2], "no change, no write")
        b.update(0, active: false, online: 0, habs: 1)
        await b.settle()
        XCTAssertEqual(b.shown, 2, "going to the background keeps the count")
        b.update(0, active: true, online: 1, habs: 1)
        await b.settle()
        XCTAssertEqual(writes, [2, 2, 0])
    }

    /// A count that changes while a write is on its way is written after it.
    func testChangeDuringAWrite() async {
        var writes: [Int] = []
        var release: CheckedContinuation<Void, Never>?
        let b = AppBadge { n in
            writes.append(n)
            if n == 1 { await withCheckedContinuation { release = $0 } }
            return true
        }
        b.update(1, active: true, online: 1, habs: 1)
        await eventually("first write") { release != nil }
        b.update(2, active: true, online: 1, habs: 1)
        b.update(3, active: true, online: 1, habs: 1)
        release?.resume()
        await eventually("last count") { b.shown == 3 }
        XCTAssertEqual(writes, [1, 3])
    }

    /// Through the fleet: a hab's question and a waiting confirmation set the
    /// badge; the background keeps it.
    func testFleetFeedsTheBadge() async {
        let hab = FakeHab(hostID: "alpha-host")
        await hab.load(
            projects: [Fixture.project("p1", name: "tide-tables", sessions: ["s1"])],
            sessions: [Fixture.session("s1", name: "claude", pane: "%1", project: "p1")],
            panes: [Fixture.pane("%1", session: "s1")],
            agents: [Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .question)]
        )
        await hab.setCapability("confirm", true)
        let fleet = Fleet()
        var shown: [Int] = []
        fleet.badge = AppBadge { n in
            shown.append(n)
            return true
        }
        XCTAssertEqual(shown, [], "inactive: nothing written")
        fleet.add(HabConnection(id: "a", transport: hab.transport(), timing: fastTiming()), label: nil)
        await fleet.setActive(true)
        await eventually("question counted") { shown.last == 1 }
        await hab.requestConfirmation(ConfirmTests.confirmation("q", created: Date()))
        await eventually("confirmation counted") { shown.last == 2 }
        await fleet.setActive(false)
        await eventually("offline") { fleet.board.onlineCount == 0 }
        XCTAssertEqual(fleet.badge?.shown, 2, "the background keeps the last count")
    }
}
