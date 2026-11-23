import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

private func row(_ id: String, hab: String = "bench", session: String? = "s1", pane: String? = "%1", title: String = "claude",
                 name: String? = "claude", project: String? = nil, harness: String? = nil, model: String? = nil,
                 state: AgentStatus? = .idle, rank: BoardRank = .atRest) -> BoardRow {
    var r = BoardRow(id: id, target: SessionTarget(habID: hab, session: session, pane: pane), host: hab, title: title,
                     project: project, model: model, harness: harness, state: state, rank: rank)
    r.name = name
    return r
}

/// A defaults suite of its own, emptied before and after the test.
private func suite(_ name: String) -> UserDefaults {
    let d = UserDefaults(suiteName: name)!
    d.removePersistentDomain(forName: name)
    return d
}

/// The composer's drafts (app.md 4.2).
@MainActor
final class DraftStoreTests: XCTestCase {
    func testDraftsOutliveTheViewAndTheApp() {
        let name = "flightdeck.test.drafts"
        let d = suite(name)
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

/// Reopen where the owner left it (app.md 3.12).
final class SessionRestoreTests: XCTestCase {
    func testRestoreWaitsForTheRowWithinTheLimit() throws {
        XCTAssertNil(SessionRestore(saved: "", now: Date()), "nothing was open")
        let start = Date()
        let r = try XCTUnwrap(SessionRestore(saved: "bench/s/s1", now: start))
        XCTAssertNil(r.row(in: [], now: start), "the board has not listed it yet")
        let rows = [row("bench/s/s2", session: "s2"), row("bench/s/s1")]
        XCTAssertEqual(r.row(in: rows, now: start.addingTimeInterval(5))?.id, "bench/s/s1")
        XCTAssertFalse(r.expired(now: start.addingTimeInterval(SessionRestore.limit)))
        let late = start.addingTimeInterval(SessionRestore.limit + 1)
        XCTAssertTrue(r.expired(now: late))
        XCTAssertNil(r.row(in: rows, now: late), "a row that comes later opens nothing")
    }

    func testTheCapcomMarkMatchesNoRow() throws {
        let r = try XCTUnwrap(SessionRestore(saved: SessionRestore.capcom, now: Date()))
        XCTAssertNil(r.row(in: [row("bench/s/s1")], now: Date()))
    }
}

/// The board's search (app.md 3.6).
@MainActor
final class BoardSearchTests: XCTestCase {
    private let rows = [
        row("bench/s/s1", title: "Fix the tide tables", name: "tides", project: "tide-tables", harness: "claude", model: "opus"),
        row("bench/s/s2", session: "s2", title: "Café menu", name: "menu", project: "paper-lantern", harness: "codex"),
        row("lab/s/s3", hab: "lab", session: "s3", title: "zsh", name: "zsh", project: "tide-tables"),
    ]

    private var sections: [BoardSection] { [BoardSection(kind: .done, rows: rows)] }

    private func ids(_ q: String, _ scope: BoardSearchScope = .all) -> [String] {
        BoardSearch.filter(sections, query: q, scope: scope).flatMap(\.rows).map(\.id)
    }

    func testMatchesNamesTheAppHolds() {
        XCTAssertEqual(ids(""), rows.map(\.id), "an empty query filters nothing")
        XCTAssertEqual(ids("TIDE"), ["bench/s/s1", "lab/s/s3"], "any case, title or project")
        XCTAssertEqual(ids("cafe"), ["bench/s/s2"], "accents ignored")
        XCTAssertEqual(ids("claude code"), ["bench/s/s1"], "the harness by its name")
        XCTAssertEqual(ids("opus"), ["bench/s/s1"], "the model")
        XCTAssertEqual(ids("lab"), ["lab/s/s3"], "the hab")
        XCTAssertEqual(ids("tide zsh"), ["lab/s/s3"], "every word must match")
        XCTAssertEqual(ids("nothing-like-this"), [])
        XCTAssertTrue(BoardSearch.filter(sections, query: "nothing", scope: .all).isEmpty, "an emptied section goes")
    }

    func testHostScope() {
        XCTAssertEqual(ids("", .hab("lab")), ["lab/s/s3"])
        XCTAssertEqual(ids("tide", .hab("bench")), ["bench/s/s1"])
        XCTAssertFalse(BoardSearch.isActive(query: "  ", scope: .all))
        XCTAssertTrue(BoardSearch.isActive(query: "", scope: .hab("lab")))
        XCTAssertEqual(BoardSearch.fit(.hab("gone"), habs: ["bench"]), .all, "a hab that left falls back to All")
        XCTAssertEqual(BoardSearch.fit(.hab("bench"), habs: ["bench"]), .hab("bench"))
    }

    func testSessionsListCountsFollowTheFilter() {
        let list = SessionsList.build(sections: sections, confirms: [], groups: [
            GroupSummary(id: "lab/g/g1", title: "Docs sweep", habLabel: "lab", members: 2, working: 1, waiting: 1,
                         over: false, since: nil, memberTargets: []),
        ])
        let got = BoardSearch.filter(list, query: "tide", scope: .all)
        XCTAssertEqual(got.map(\.title), ["Done · 2"])
        let groups = BoardSearch.filter(list, query: "docs", scope: .hab("lab"))
        XCTAssertEqual(groups.flatMap(\.items).map(\.id), ["lab/g/g1"], "a group matches on its title and hab")
        XCTAssertTrue(BoardSearch.filter(list, query: "docs", scope: .hab("bench")).isEmpty)
    }
}

/// Reconnect on a network change (app.md 10.2).
@MainActor
final class NetworkWatchTests: XCTestCase {
    private let wifi = NetworkPathReading(usable: true, interfaces: ["en0"])
    private let cell = NetworkPathReading(usable: true, interfaces: ["pdp_ip0"])
    private let none = NetworkPathReading(usable: false, interfaces: [])

    func testRule() {
        XCTAssertFalse(NetworkWatch.retries(after: nil, wifi), "the first reading is the state at start")
        XCTAssertTrue(NetworkWatch.retries(after: none, wifi), "the network came back")
        XCTAssertTrue(NetworkWatch.retries(after: wifi, cell), "Wi-Fi to cellular")
        XCTAssertFalse(NetworkWatch.retries(after: wifi, wifi), "nothing changed")
        XCTAssertFalse(NetworkWatch.retries(after: wifi, none), "no network: nothing to retry on")
        XCTAssertEqual(NetworkPathReading(usable: true, interfaces: ["b", "a"]), NetworkPathReading(usable: true, interfaces: ["a", "b"]))
    }

    func testFleetRetriesOnAChangeWhileVisible() async {
        let f = Fleet()
        let (stream, sink) = AsyncStream.makeStream(of: NetworkPathReading.self)
        f.watchNetwork(stream)
        await f.setActive(true)
        sink.yield(wifi)
        sink.yield(none)
        sink.yield(wifi)
        await eventually("one retry") { f.retries == 1 }
        sink.yield(cell)
        await eventually("two retries") { f.retries == 2 }

        // In the background a change retries nothing: the connections are closed (app.md 12.1).
        await f.setActive(false)
        sink.yield(wifi)
        sink.yield(none)
        sink.yield(wifi)
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(f.retries, 2)
        sink.finish()
    }

    func testRefreshRetriesAndWaitsForTheHabs() async {
        let hab = await sampleHab("bench")
        let f = Fleet()
        f.add(HabConnection(id: "bench", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await f.setActive(true)
        await f.refresh(limit: .seconds(5))
        XCTAssertEqual(f.retries, 1)
        XCTAssertTrue(f.board.habs.allSatisfy { $0.state.status == .online })
        await f.setActive(false)
    }
}

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
