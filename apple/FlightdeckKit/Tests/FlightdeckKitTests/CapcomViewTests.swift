import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The Capcom view (app.md 4.10): the home from each hab's catalog, the entry's states, the
/// standing conversation opened with `capcom.open` and no context, and New conversation in
/// its place.
@MainActor
final class CapcomViewTests: XCTestCase {
    private func hab(_ host: String, capcom: Bool = true) async -> FakeHab {
        let h = await sampleHab(host)
        if capcom { await h.setCapability("capcom", true) }
        return h
    }

    /// A fleet over `habs`, labelled by host, started; waits until every hab is online and
    /// the entry left `loading`.
    private func fleet(_ habs: [FakeHab], start: Bool = true) async -> Fleet {
        let f = Fleet()
        for h in habs {
            let host = h.hostID
            f.add(HabConnection(id: host, transport: h.transport(), timing: fastTiming()), label: host)
        }
        guard start else { return f }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        await eventually("home read") { f.capcom.entry != .loading }
        return f
    }

    func testHomeOnTheOneHabOpensTheStandingConversation() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        XCTAssertEqual(f.capcom.entry, .ready(habID: "bench"))
        XCTAssertEqual(f.capcom.entryText(f.capcom.entry), "bench")
        await f.capcom.openStanding()
        let m = try XCTUnwrap(f.capcom.standing)
        XCTAssertEqual(m.target.session, "capcom-s1")
        let opens = await a.capcomOpens
        XCTAssertEqual(opens, [CapcomOpenParams()], "no context, not new")
        XCTAssertNil(f.capcom.openRequest, "the Capcom view shows it; the board opens nothing")
        XCTAssertTrue(f.chat(for: SessionTarget(habID: "bench", session: "capcom-s1", pane: nil)) === m, "one model per session")
        await eventually("capcom context") { m.context.isCapcom }
        // Opening again returns the same standing conversation.
        await f.capcom.openStanding()
        XCTAssertTrue(f.capcom.standing === m)
        await f.setActive(false)
    }

    func testHighestAssignmentPicksTheHomeAcrossHabs() async throws {
        let a = await hab("bench")
        let b = await hab("attic")
        let bID = b.deviceID
        let aID = a.deviceID
        // bench saw a later assignment than attic: the home is attic.
        await a.setCapcomHome(assignment: 2, home: bID)
        await b.setCapcomHome(assignment: 1, home: aID)
        let f = await fleet([a, b])
        XCTAssertEqual(f.capcom.homeDevice, bID)
        XCTAssertEqual(f.capcom.entry, .ready(habID: "attic"))
        // attic's own belief is older, so its fake refuses; give it the same view first.
        await b.setCapcomHome(assignment: 2, home: bID)
        await f.capcom.openStanding()
        XCTAssertEqual(f.capcom.standing?.target.habID, "attic")
        let aOpens = await a.capcomOpens
        XCTAssertTrue(aOpens.isEmpty, "only the home is asked")
        await f.setActive(false)
    }

    func testUnassignedThenCheckAgain() async throws {
        let a = await hab("bench")
        await a.setCapcomHome(assignment: 0, home: nil)
        let f = await fleet([a])
        XCTAssertEqual(f.capcom.entry, .unassigned)
        await f.capcom.openStanding()
        XCTAssertNil(f.capcom.standing)
        let none = await a.capcomOpens
        XCTAssertTrue(none.isEmpty, "no open without a home")
        let aID = a.deviceID
        await a.setCapcomHome(assignment: 1, home: aID)
        await f.capcom.refreshHome()
        XCTAssertEqual(f.capcom.entry, .ready(habID: "bench"))
        await f.setActive(false)
    }

    func testHabWithoutCapcomNamesNoHome() async throws {
        let a = await hab("bench", capcom: false)
        let f = await fleet([a])
        XCTAssertEqual(f.capcom.entry, .unassigned)
        XCTAssertEqual(f.capcom.beliefs["bench"], CapcomBelief.none)
        let gets = await a.catalogGets
        XCTAssertEqual(gets, 0, "no catalog read without the capability")
        await f.setActive(false)
    }

    func testHomeOfflineThenBack() async throws {
        let a = await hab("bench")
        let b = await hab("attic")
        let bID = b.deviceID
        await a.setCapcomHome(assignment: 1, home: bID)
        await b.setCapcomHome(assignment: 1, home: bID)
        let f = await fleet([a, b])
        XCTAssertEqual(f.capcom.entry, .ready(habID: "attic"))
        await b.setSilent(true)
        await b.dropConnections()
        await eventually("attic not online") { f.capcom.entry == .offline(habID: "attic") }
        XCTAssertEqual(f.capcom.entryText(f.capcom.entry), "attic · offline")
        await f.capcom.openStanding()
        XCTAssertNil(f.capcom.standing, "no open while the home is offline")
        await b.setSilent(false)
        await eventually("attic back") { f.capcom.entry == .ready(habID: "attic") }
        await f.capcom.openStanding()
        XCTAssertEqual(f.capcom.standing?.target.habID, "attic")
        await f.setActive(false)
    }

    func testLoadingWhileAHabConnectsAndNoHabOnlineWhenStopped() async throws {
        let a = await hab("bench")
        let f = await fleet([a], start: false)
        XCTAssertEqual(f.capcom.entry, .noHabOnline)
        await a.setSilent(true)
        await f.setActive(true)
        await eventually("connecting") { f.board.habs.first?.state.status == .connecting }
        XCTAssertEqual(f.capcom.entry, .loading)
        await a.setSilent(false)
        await eventually("ready") { f.capcom.entry == .ready(habID: "bench") }
        await f.setActive(false)
    }

    func testHomeNotInThisApp() async throws {
        let a = await hab("bench")
        await a.setCapcomHome(assignment: 1, home: "SHA256:elsewhere")
        let f = await fleet([a])
        XCTAssertEqual(f.capcom.entry, .notInApp)
        await f.setActive(false)
    }

    func testReassignedHomeIsFoundAfterTheOldOneRefuses() async throws {
        let a = await hab("bench")
        let b = await hab("attic")
        let aID = a.deviceID
        let bID = b.deviceID
        await a.setCapcomHome(assignment: 1, home: aID)
        await b.setCapcomHome(assignment: 1, home: aID)
        let f = await fleet([a, b])
        XCTAssertEqual(f.capcom.entry, .ready(habID: "bench"))
        // Re-assigned to attic while the app held the old catalogs.
        await a.setCapcomHome(assignment: 2, home: bID)
        await b.setCapcomHome(assignment: 2, home: bID)
        await f.capcom.openStanding()
        XCTAssertEqual(f.capcom.standing?.target.habID, "attic")
        XCTAssertNil(f.capcom.standingFailure)
        let aOpens = await a.capcomOpens
        XCTAssertEqual(aOpens.count, 1, "the old home refused once")
        await f.setActive(false)
    }

    func testRefusedOpenShowsTheHabsText() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        await a.failNextCapcom(kind: .invalid, hint: "the Capcom folder cannot be written")
        await f.capcom.openStanding()
        XCTAssertNil(f.capcom.standing)
        XCTAssertEqual(f.capcom.standingFailure, "the Capcom folder cannot be written")
        await f.capcom.openStanding()
        XCTAssertNotNil(f.capcom.standing)
        XCTAssertNil(f.capcom.standingFailure, "a success clears it")
        await f.setActive(false)
    }

    func testNewConversationReplacesTheStandingOne() async throws {
        let a = await hab("bench")
        await a.addNotice(id: "n7", title: "Build failed")
        let f = await fleet([a])
        await f.capcom.openStanding()
        let old = try XCTUnwrap(f.capcom.standing)
        await eventually("capcom context") { old.context.isCapcom }
        let ok = await old.attach(CapcomContextRef(kind: .notification, ref: "n7"))
        XCTAssertTrue(ok)
        let moved = await f.capcom.newConversation(from: old, draft: "why did it fail?")
        XCTAssertTrue(moved)
        let m = try XCTUnwrap(f.capcom.standing)
        XCTAssertFalse(m === old)
        XCTAssertEqual(m.target.session, "capcom-s2")
        XCTAssertEqual(m.handoff, "why did it fail?")
        XCTAssertEqual(m.pending.map(\.title), ["Build failed"])
        XCTAssertTrue(old.pending.isEmpty, "detached from the old one")
        XCTAssertNil(f.capcom.openRequest, "the Capcom view swaps in place")
        let opens = await a.capcomOpens
        XCTAssertEqual(opens.last?.new, true)
        await f.setActive(false)
    }

    func testNewConversationFromAnOlderSessionAsksTheBoard() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        await f.capcom.openStanding()
        let standing = try XCTUnwrap(f.capcom.standing)
        // An older Capcom session opened from its board row is not the standing one.
        let older = await f.capcom.open(habID: "bench", context: [], new: true)
        let o = try XCTUnwrap(older)
        f.capcom.shown()
        XCTAssertTrue(f.capcom.standing === standing)
        let moved = await f.capcom.newConversation(from: o, draft: "")
        XCTAssertTrue(moved)
        XCTAssertEqual(f.capcom.openRequest?.session, "capcom-s3")
        XCTAssertTrue(f.capcom.standing === standing, "the Capcom view keeps what it showed")
        await f.setActive(false)
    }

    func testDemoModeShowsTheCapcomView() async throws {
        let f = Fleet()
        await DemoFleet.populate(f, script: false)
        await f.setActive(true)
        await eventually("ready") { f.capcom.entry == .ready(habID: "demo-workbench") }
        await f.capcom.openStanding()
        let m = try XCTUnwrap(f.capcom.standing)
        XCTAssertEqual(m.target.session, "capcom-s1", "the loaded standing conversation, not a new one")
        XCTAssertEqual(m.pending.map(\.title), ["Tests failed in seed-catalog"])
        m.start()
        await eventually("messages") { m.messages.count == 4 }
        m.stop()
        await f.setActive(false)
    }

    func testRemovedHabDropsItsBelief() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        XCTAssertNotNil(f.capcom.homeDevice)
        await f.remove(id: "bench")
        XCTAssertNil(f.capcom.homeDevice)
        XCTAssertEqual(f.capcom.entry, .noHabOnline)
    }
}
