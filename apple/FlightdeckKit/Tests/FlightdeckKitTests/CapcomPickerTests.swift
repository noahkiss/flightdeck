import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The Capcom host picker, the home followed through `catalog.updated`, and the entry row's
/// agent state (app.md 4.10, capcom.md 7.3).
@MainActor
final class CapcomPickerTests: XCTestCase {
    private func hab(_ host: String, capcom: Bool = true) async -> FakeHab {
        let h = await sampleHab(host)
        if capcom { await h.setCapability("capcom", true) }
        return h
    }

    /// A fleet over `habs`, labelled by host and started; waits until every hab is online and
    /// the entry left `loading`.
    private func fleet(_ habs: [FakeHab]) async -> Fleet {
        let f = Fleet()
        for h in habs {
            f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        await eventually("home read") { f.capcom.entry != .loading }
        return f
    }

    func testAssignCallsSelfWithACounterAboveEveryOneSeen() async throws {
        let a = await hab("bench")
        let b = await hab("attic")
        let bID = b.deviceID
        await a.setCapcomHome(assignment: 3, home: bID)
        await b.setCapcomHome(assignment: 3, home: bID)
        let f = await fleet([a, b])
        XCTAssertEqual(f.capcom.entry, .ready(habID: "attic"))
        XCTAssertEqual(f.capcom.highestAssignment, 3)
        XCTAssertEqual(f.capcom.hostChoices.map(\.isHome), [false, true], "the current home is marked")

        let ok = await f.capcom.assign(habID: "bench")
        XCTAssertTrue(ok)
        let sent = await a.capcomAssigns
        XCTAssertEqual(sent, [CapcomAssignParams(assignment: 4, host: "self")], "self, one above the highest seen")
        let other = await b.capcomAssigns
        XCTAssertTrue(other.isEmpty, "only the chosen hab is asked")
        // The answer is the new belief at once, before any catalog read.
        XCTAssertEqual(f.capcom.beliefs["bench"], .home(assignment: 4, device: a.deviceID))
        XCTAssertEqual(f.capcom.entry, .ready(habID: "bench"))
        XCTAssertEqual(f.capcom.hostChoices.map(\.isHome), [true, false])
        XCTAssertNil(f.capcom.assignFailure)
        XCTAssertNil(f.capcom.assigning)
        await f.setActive(false)
    }

    func testOfflineAndCapcomlessHabsCannotBeChosen() async throws {
        let a = await hab("bench")
        let b = await hab("attic", capcom: false)
        let c = await hab("cellar")
        await a.setCapcomHome(assignment: 0, home: nil)
        await c.setCapcomHome(assignment: 0, home: nil)
        let f = await fleet([a, b, c])
        await c.setSilent(true)
        await c.dropConnections()
        await eventually("cellar not online") { f.board.habs.first { $0.id == "cellar" }?.state.status != .online }
        let choices = f.capcom.hostChoices
        XCTAssertEqual(choices.map(\.id), ["bench", "attic", "cellar"], "every hab, in the app's order")
        XCTAssertEqual(choices.map(\.unavailable), [nil, "No Capcom", "Offline"])
        XCTAssertEqual(f.capcom.suggestedHost, "bench")

        let offline = await f.capcom.assign(habID: "cellar")
        XCTAssertFalse(offline)
        let none = await f.capcom.assign(habID: "attic")
        XCTAssertFalse(none)
        let sentB = await b.capcomAssigns
        let sentC = await c.capcomAssigns
        XCTAssertTrue(sentB.isEmpty && sentC.isEmpty, "a hab that cannot be chosen gets no call")
        XCTAssertNil(f.capcom.homeDevice)
        await f.setActive(false)
    }

    func testSuggestionPrefersAHabMarkedAlwaysOn() async throws {
        let a = await hab("bench")
        let b = await hab("attic")
        await a.setCapcomHome(assignment: 0, home: nil)
        await b.setCapcomHome(assignment: 0, home: nil)
        await b.setAlwaysOn(true)
        let f = await fleet([a, b])
        XCTAssertEqual(f.capcom.entry, .unassigned)
        XCTAssertEqual(f.capcom.suggestedHost, "attic")
        await b.setAlwaysOn(false)
        await f.capcom.refreshHome()
        XCTAssertEqual(f.capcom.suggestedHost, "bench", "else the first hab in the app")
        await f.setActive(false)
    }

    func testAssignFromTheUnassignedStateStartsAFreshStandingConversation() async throws {
        let a = await hab("bench")
        await a.setCapcomHome(assignment: 0, home: nil)
        let f = await fleet([a])
        let view = UUID()
        f.capcom.viewAppeared(view)
        XCTAssertEqual(f.capcom.entry, .unassigned)
        let ok = await f.capcom.assign(habID: "bench")
        XCTAssertTrue(ok)
        let sent = await a.capcomAssigns
        XCTAssertEqual(sent.first?.assignment, 1)
        await eventually("opened on the new home") { f.capcom.standing?.target.habID == "bench" }
        f.capcom.viewDisappeared(view)
        await f.setActive(false)
    }

    func testCatalogUpdatedReadsTheHomeAgainWithoutReconnecting() async throws {
        let a = await hab("bench")
        let b = await hab("attic")
        let aID = a.deviceID, bID = b.deviceID
        await a.setCapcomHome(assignment: 1, home: aID)
        await b.setCapcomHome(assignment: 1, home: aID)
        let f = await fleet([a, b])
        XCTAssertEqual(f.capcom.entry, .ready(habID: "bench"))
        let snapshots = f.board.habs.map(\.state.snapshots)
        let gets = await b.catalogGets
        // Assigned from the command line on attic: attic's own catalog changes.
        await b.reassignCapcom(assignment: 2, home: bID)
        await eventually("attic is home") { f.capcom.entry == .ready(habID: "attic") }
        let after = await b.catalogGets
        XCTAssertGreaterThan(after, gets, "read again")
        XCTAssertEqual(f.board.habs.map(\.state.snapshots), snapshots, "no reconnect")
        // A peer's catalog change alone reads nothing.
        let aGets = await a.catalogGets
        await a.emit(type: "catalog.updated", id: bID, revision: nil, data: CatalogUpdated(deviceId: bID, generation: 9))
        try await Task.sleep(for: .milliseconds(200))
        let aAfter = await a.catalogGets
        XCTAssertEqual(aAfter, aGets)
        await f.setActive(false)
    }

    func testRowShowsTheStandingConversationsState() async throws {
        let a = await hab("bench")
        await a.loadCapcomSession("capcom-s1", name: "capcom", pane: "%c1", standing: true)
        let f = await fleet([a])
        XCTAssertEqual(f.capcom.standingTarget?.session, "capcom-s1", "from the home's catalog, before any open")
        await eventually("row") { f.capcom.standingRow(in: f.board.rows) != nil }
        XCTAssertEqual(f.capcom.standingRow(in: f.board.rows)?.state, .idle)
        await a.put(Fixture.agent(pane: "%c1", harness: "claude", state: .working, session: "capcom-s1"))
        await eventually("working") { f.capcom.standingRow(in: f.board.rows)?.state == .working }
        await a.put(Fixture.agent(pane: "%c1", harness: "claude", state: .blocked, kind: .permission, session: "capcom-s1"))
        await eventually("needs you") { f.capcom.standingRow(in: f.board.rows)?.state == .blocked }
        XCTAssertEqual(f.capcom.standingRow(in: f.board.rows)?.stateText(now: Date()).hasPrefix("Permission"), true)
        // Another home: the row has no state until that home names its standing conversation.
        let b = FakeHab.deviceID(host: "elsewhere")
        await a.reassignCapcom(assignment: 2, home: b)
        await eventually("home moved") { f.capcom.entry == .notInApp }
        XCTAssertNil(f.capcom.standingRow(in: f.board.rows))
        await f.setActive(false)
    }
}
