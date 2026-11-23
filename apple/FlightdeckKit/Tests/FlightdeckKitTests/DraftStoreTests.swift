import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The composer's drafts (app.md 4.2).
@MainActor
final class DraftStoreTests: XCTestCase {
    func testDraftsOutliveTheViewAndTheApp() {
        let name = "flightdeck.test.drafts"
        let d = emptySuite(name)
        defer { d.removePersistentDomain(forName: name) }
        let a = SessionTarget(habID: "bench", session: "s1", pane: "%1")
        let store = DraftStore(defaults: d)
        XCTAssertEqual(store[a], "")
        store[a] = "half a thought"
        // A restart's new pane is the same session: the draft stays.
        XCTAssertEqual(store[SessionTarget(habID: "bench", session: "s1", pane: "%9")], "half a thought")
        XCTAssertEqual(store[SessionTarget(habID: "other", session: "s1", pane: "%1")], "", "per hab")

        // A new store on the same defaults is the app after iOS ended it.
        XCTAssertEqual(DraftStore(defaults: d)[a], "half a thought")

        // Send clears it, and an empty draft leaves no key behind.
        store[a] = ""
        XCTAssertTrue(store.drafts.isEmpty)
        XCTAssertNil(d.object(forKey: DraftStore.key))

        // Whitespace is kept while typing: a draft that starts with a space is a draft.
        store[a] = " "
        XCTAssertEqual(store[a], " ")
    }

    func testPruneDropsSessionsTheHabNoLongerLists() {
        let store = DraftStore(defaults: nil)
        store[SessionTarget(habID: "bench", session: "s1", pane: "%1")] = "keep"
        store[SessionTarget(habID: "bench", session: "s2", pane: "%2")] = "gone"
        store[SessionTarget(habID: "bench", session: nil, pane: "%7")] = "pane"
        store[SessionTarget(habID: "other", session: "s2", pane: "%2")] = "other hab"
        store.prune(habID: "bench", sessions: ["s1"], panes: ["%1", "%7"])
        XCTAssertEqual(Set(store.drafts.values), ["keep", "pane", "other hab"])
        store.forget(habID: "bench")
        XCTAssertEqual(Array(store.drafts.values), ["other hab"])
    }

    func testFleetPrunesWhenTheHabComesOnline() async {
        let hab = await sampleHab("bench")
        let f = Fleet()
        f.drafts = DraftStore(defaults: nil)
        f.drafts[SessionTarget(habID: "bench", session: "s1", pane: "%1")] = "keep"
        f.drafts[SessionTarget(habID: "bench", session: "gone", pane: "%5")] = "drop"
        f.add(HabConnection(id: "bench", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await f.setActive(true)
        await eventually("pruned") { f.drafts.drafts.count == 1 }
        XCTAssertEqual(f.drafts[SessionTarget(habID: "bench", session: "s1", pane: nil)], "keep")
        await f.setActive(false)
    }
}
