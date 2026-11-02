import FlightdeckShared
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Rounds in the app (rounds.md 10, app.md 21) over the fake hab: the list's order and marks,
/// round events by revision, run states, the actions and their refusals, the run session that
/// opens with its badge, Delete that asks first, the board filter, notification opens, the
/// terminal filter for live run panes, and the form.
@MainActor
final class RoundTests: XCTestCase {
    private func hab(_ host: String = "bench") async -> FakeHab {
        let h = await sampleHab(host)
        await h.setCapability(RoundCenter.capability, true)
        return h
    }

    private func fleet(_ habs: [FakeHab]) async -> Fleet {
        let f = Fleet()
        f.roundDefaults = UserDefaults(suiteName: "RoundTests-\(UUID().uuidString)")!
        for h in habs {
            f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        return f
    }

    private func spec(_ name: String, every: String? = "1h", kind: RoundTriggerKind = .cron, command: [String]? = ["true"]) -> RoundSpec {
        RoundSpec(command: command, mode: .fresh, name: name, project: "p1",
                  trigger: RoundTrigger(every: every, kind: kind))
    }

    private func rows(_ f: Fleet, _ hab: String = "bench") -> [RoundRow] {
        f.rounds.habs.first { $0.id == hab }?.rows ?? []
    }

    // MARK: - List

    func testListOrdersAndMarksRounds() async throws {
        let h = await hab()
        var later = spec("later", every: nil, kind: .once)
        later.trigger.at = Fixture.stamp(Date().addingTimeInterval(2 * 86400))
        try await h.loadRound(later)
        try await h.loadRound(spec("often", every: "1m"))
        try await h.loadRound(spec("by-hand", every: nil, kind: .manual))
        try await h.loadRound(spec("asleep", every: "1m"))
        try await h.loadRound(spec("beta", every: nil, kind: .manual))
        let f = await fleet([h])
        await eventually("rounds") { self.rows(f).count == 5 }
        let asleep = try XCTUnwrap(rows(f).first { $0.round.name == "asleep" })
        await f.rounds.setPaused(asleep, true)
        await eventually("paused") { self.rows(f).first { $0.round.name == "asleep" }?.mark == .paused }
        XCTAssertEqual(rows(f).map(\.round.name), ["often", "later", "beta", "by-hand", "asleep"],
                       "soonest next run first, then by hand by name, then paused")
        XCTAssertEqual(rows(f).map(\.mark), [nil, nil, nil, nil, .paused])
        XCTAssertNil(rows(f).last?.round.nextSlot, "a paused round has no next run")

        // Completed sorts last, and shows completed over paused.
        var done = rows(f)[0].round
        done.completed = true
        done.paused = true
        let ordered = RoundCenter.order(rows(f).map(\.round).filter { $0.id != done.id } + [done])
        XCTAssertEqual(ordered.last?.name, "often")
        XCTAssertEqual(RoundRow(habID: "bench", round: done).mark, .completed)
        await f.setActive(false)
    }

    func testNoRoundsUIWithoutTheCapability() async {
        let h = await sampleHab("plain")
        let f = await fleet([h])
        XCTAssertFalse(f.rounds.any)
        XCTAssertTrue(f.rounds.habs.isEmpty)
        await f.setActive(false)
    }

    func testHigherRevisionReplacesARowAndALowerOneIsIgnored() async throws {
        let h = await hab()
        try await h.loadRound(spec("nightly", every: "1d"))
        let f = await fleet([h])
        await eventually("round") { self.rows(f).count == 1 }
        var r = try XCTUnwrap(rows(f).first?.round)
        XCTAssertEqual(r.revision, 1)
        r.revision = 3
        r.prompt = nil
        r.command = ["make", "check"]
        await h.emit(type: "round.updated", id: r.id, revision: 3, data: r)
        await eventually("revision 3") { self.rows(f).first?.round.revision == 3 }
        XCTAssertEqual(rows(f).first?.round.command, ["make", "check"])
        var stale = r
        stale.revision = 2
        stale.command = ["old"]
        await h.emit(type: "round.updated", id: r.id, revision: 2, data: stale)
        // A later event proves the stale one was read.
        var newer = r
        newer.revision = 4
        newer.name = "nightly"
        await h.emit(type: "round.updated", id: r.id, revision: 4, data: newer)
        await eventually("revision 4") { self.rows(f).first?.round.revision == 4 }
        XCTAssertEqual(rows(f).first?.round.command, ["make", "check"], "revision 2 changed nothing")
        // A delete is a round.updated with deleted at the next revision.
        var gone = newer
        gone.revision = 5
        gone.deleted = true
        await h.emit(type: "round.updated", id: r.id, revision: 5, data: gone)
        await eventually("deleted") { self.rows(f).isEmpty }
        await f.setActive(false)
    }

    func testRunUpdatedMovesARunsState() async throws {
        let h = await hab()
        try await h.loadRound(spec("check", every: "1h"))
        let f = await fleet([h])
        await eventually("round") { self.rows(f).count == 1 }
        await f.rounds.runNow(rows(f)[0])
        XCTAssertNil(f.rounds.errors[rows(f)[0].id])
        await eventually("running") { self.rows(f).first?.round.lastRun?.state == .running }
        let run = try XCTUnwrap(rows(f).first?.liveRun)
        XCTAssertEqual(f.rounds.habRuns(habID: "bench", round: run.round).first?.state, .running)
        await h.setRunState(run.id, .failed, reason: "exit status 2")
        await eventually("failed") { self.rows(f).first?.round.lastRun?.state == .failed }
        XCTAssertNil(rows(f).first?.liveRun)
        XCTAssertEqual(rows(f).first?.round.lastRun?.reason, "exit status 2")
        // The round page merges its page with the updates seen since.
        let page = try await f.rounds.runs(habID: "bench", round: run.round)
        XCTAssertEqual(page.runs.map(\.state), [.failed])
        var later = run
        later.id = "later"
        later.number = 2
        later.state = .queued
        XCTAssertEqual(RoundPageRuns.merge(page.runs, [later]).map(\.number), [2, 1])
        await f.setActive(false)
    }

    func testRunNowOnALiveRoundShowsTheConflict() async throws {
        let h = await hab()
        try await h.loadRound(spec("check", every: "1h"))
        let f = await fleet([h])
        await eventually("round") { self.rows(f).count == 1 }
        await f.rounds.runNow(rows(f)[0])
        await eventually("running") { self.rows(f).first?.liveRun != nil }
        await f.rounds.runNow(rows(f)[0])
        XCTAssertEqual(f.rounds.errors[rows(f)[0].id], "The round has a live run.")
        // A paused round refuses a run by hand too.
        let run = try XCTUnwrap(rows(f).first?.liveRun)
        await h.setRunState(run.id, .succeeded)
        await f.rounds.setPaused(rows(f)[0], true)
        await eventually("paused") { self.rows(f).first?.mark == .paused }
        await f.rounds.runNow(rows(f)[0])
        XCTAssertEqual(f.rounds.errors[rows(f)[0].id], "The round is paused.")
        await f.rounds.setPaused(rows(f)[0], false)
        await eventually("unpaused") { self.rows(f).first?.mark == nil }
        XCTAssertNil(f.rounds.errors[rows(f)[0].id], "a new action clears the last refusal")
        await f.setActive(false)
    }

    func testStaleEditIsRefusedWithTheHabsText() async throws {
        let h = await hab()
        try await h.loadRound(spec("check", every: "1h"))
        let f = await fleet([h])
        await eventually("round") { self.rows(f).count == 1 }
        let r = rows(f)[0].round
        var d = RoundDraft(habID: "bench", round: r)
        XCTAssertEqual(d.when, .every)
        XCTAssertEqual(d.every, "1h")
        XCTAssertEqual(d.runs, .command)
        d.every = "30m"
        let s = try d.spec().get()
        try await f.rounds.update(habID: "bench", id: r.id, revision: r.revision, s)
        await eventually("edited") { self.rows(f).first?.round.revision == 2 }
        XCTAssertEqual(rows(f).first?.round.trigger.cron, "*/30 * * * *")
        do {
            try await f.rounds.update(habID: "bench", id: r.id, revision: r.revision, s)
            XCTFail("a stale edit went through")
        } catch {
            XCTAssertEqual(RoundCenter.text(error), "round check is at revision 2, not 1; read it again")
        }
        await f.setActive(false)
    }

    // MARK: - The flow: create, Run now, the run session with its badge, Delete asks first

    func testCreateRunNowOpensTheRunSessionWithItsBadgeAndDeleteAsksFirst() async throws {
        let h = await hab()
        let f = await fleet([h])
        XCTAssertTrue(f.rounds.any)
        var d = RoundDraft(habID: "bench")
        d.name = "lint"
        d.project = "p1"
        d.runs = .command
        d.command = "make lint"
        d.when = .every
        d.every = "15m"
        let made = try await f.rounds.create(habID: "bench", try d.spec().get(), runNow: false)
        XCTAssertEqual(made.command, ["make", "lint"])
        await eventually("listed") { self.rows(f).map(\.round.name) == ["lint"] }
        let row = rows(f)[0]

        await f.rounds.runNow(row)
        await eventually("run session opens") { f.rounds.openRequest != nil }
        let target = try XCTUnwrap(f.rounds.openRequest)
        let run = try XCTUnwrap(rows(f).first?.liveRun)
        XCTAssertEqual(target.session, run.session)
        let boardRow = try XCTUnwrap(f.board.rows.first { $0.target.session == target.session })
        XCTAssertEqual(boardRow.round, "lint", "the board row carries the round badge")
        XCTAssertEqual(f.rounds.badge(habID: "bench", session: target.session!), "lint")
        let chat = f.chat(for: target)
        XCTAssertEqual(chat.context.round, "lint", "the session view's header carries it too")
        XCTAssertTrue(f.rounds.liveRun(habID: "bench", pane: target.pane ?? boardRow.target.pane!))

        // Delete asks first: nothing goes to the hab until the yes.
        f.rounds.askDelete(row)
        XCTAssertEqual(f.rounds.deleteAsk?.id, row.id)
        var calls = await h.roundCalls
        XCTAssertFalse(calls.contains("round.delete"))
        f.rounds.deleteAsk = nil // Cancel
        XCTAssertNil(f.rounds.confirmDelete())
        calls = await h.roundCalls
        XCTAssertFalse(calls.contains("round.delete"), "a cancelled delete sends nothing")
        // The dialog's Delete: the button's action runs, then the dialog clears its binding
        // before any task the action started gets to run.
        f.rounds.askDelete(row)
        let deleting = f.rounds.confirmDelete()
        f.rounds.deleteAsk = nil
        await deleting?.value
        await eventually("deleted") { self.rows(f).isEmpty }
        calls = await h.roundCalls
        XCTAssertEqual(calls.filter { $0 == "round.delete" }.count, 1)
        // The run session keeps its badge with the round gone.
        XCTAssertEqual(f.board.rows.first { $0.target.session == target.session }?.round, "Round")
        await f.setActive(false)
    }

    func testCreateWithRunNowOpensTheRunSession() async throws {
        let h = await hab()
        let f = await fleet([h])
        _ = try await f.rounds.create(habID: "bench", spec("first"), runNow: true)
        await eventually("open") { f.rounds.openRequest?.session != nil }
        XCTAssertEqual(f.board.rows.first { $0.target == f.rounds.openRequest }?.round, "first")
        await f.setActive(false)
    }

    func testCreateRefusalCarriesTheHabsText() async throws {
        let h = await hab()
        try await h.loadRound(spec("taken"))
        let f = await fleet([h])
        do {
            try await f.rounds.create(habID: "bench", spec("taken"), runNow: false)
            XCTFail("created a second round by one name")
        } catch {
            XCTAssertEqual(RoundCenter.text(error), "a round named taken exists")
        }
        await f.setActive(false)
    }

    // MARK: - Run sessions on the board, notifications, the terminal

    func testFilterHidesRunSessionsButNotOnesThatNeedYou() async throws {
        let h = await hab()
        let f = await fleet([h])
        _ = try await f.rounds.create(habID: "bench", spec("first"), runNow: true)
        await eventually("run row") { f.board.rows.contains { $0.round != nil } }
        XCTAssertTrue(f.rounds.showRunSessions, "shown by default")
        let hidden = RoundCenter.filter(f.board.sections, showRunSessions: false)
        XCTAssertFalse(hidden.flatMap(\.rows).contains { $0.round != nil })
        XCTAssertEqual(hidden.flatMap(\.rows).count, f.board.rows.count - 1)
        // A blocked run session still shows.
        let pane = try XCTUnwrap(f.board.rows.first { $0.round != nil }?.target.pane)
        await h.put(Fixture.agent(pane: pane, harness: "claude", state: .blocked, kind: .permission,
                                  request: Fixture.request(title: "Run make", tool: "Bash")))
        await eventually("blocked") { f.board.rows.first { $0.round != nil }?.rank == .blocked }
        XCTAssertTrue(RoundCenter.filter(f.board.sections, showRunSessions: false).flatMap(\.rows).contains { $0.round != nil })
        // The choice is kept.
        f.rounds.showRunSessions = false
        XCTAssertEqual(f.roundDefaults.object(forKey: RoundCenter.showKey) as? Bool, false)
        await f.setActive(false)
    }

    func testNotificationOpenOpensTheRunSessionOnceItsHabShowsIt() async throws {
        let h = await hab()
        let f = await fleet([h])
        // A banner tapped while its hab is offline waits for the hab to show the session.
        await f.setActive(false)
        f.rounds.open(PushMeta(kind: "round", hab: "bench", session: "s-later", notice: "n1", round: "r1", run: "r1-run1"))
        XCTAssertNil(f.rounds.openRequest)
        await h.put(Fixture.session("s-later", name: "later", pane: nil, project: "p1"))
        await f.setActive(true)
        await eventually("opened") { f.rounds.openRequest?.session == "s-later" }
        // A request banner is the approval sheet's; a banner with no session opens nothing.
        f.rounds.openRequest = nil
        f.rounds.open(PushMeta(kind: "permission", hab: "bench", session: "s1", pane: "%1", request: "q"))
        f.rounds.open(PushMeta(kind: "notice", hab: "bench"))
        // A permission or question banner with no request id is the approval sheet's alone,
        // so one tap does not open both the sheet and the session.
        f.rounds.open(PushMeta(kind: "permission", hab: "bench", session: "s1", pane: "%1"))
        f.rounds.open(PushMeta(kind: "question", hab: "bench", session: "s1", pane: "%1"))
        f.rounds.boardChanged()
        XCTAssertNil(f.rounds.openRequest)
        // A done banner with no request opens its session.
        f.rounds.open(PushMeta(kind: "done", hab: "bench", session: "s1", pane: "%1"))
        XCTAssertEqual(f.rounds.openRequest?.session, "s1")
        f.rounds.openRequest = nil
        // A banner for a session its online hab no longer has is dropped, and does not open
        // the session should one by that id come later.
        f.rounds.open(PushMeta(kind: "round", hab: "bench", session: "s-gone", round: "r1", run: "r1-run2"))
        await h.put(Fixture.session("s-other", name: "other", pane: nil, project: "p1"))
        await eventually("other") { f.board.rows.contains { $0.target.session == "s-other" } }
        await h.put(Fixture.session("s-gone", name: "gone", pane: nil, project: "p1"))
        await eventually("gone listed") { f.board.rows.contains { $0.target.session == "s-gone" } }
        XCTAssertNil(f.rounds.openRequest, "the tap was dropped while its hab was online without the session")
        // Open on a round banner maps to the open intent.
        let m = PushMeta(kind: "round", hab: "bench", session: "s1", round: "r1", run: "x")
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.openAction, meta: m), .open(m))
        let decoded = PushMeta(userInfo: [PushMeta.userInfoKey: ["v": 1, "kind": "round", "hab": "bench", "session": "s1", "round": "r1", "run": "x"]])
        XCTAssertEqual(decoded?.round, "r1")
        XCTAssertEqual(decoded?.run, "x")
        await f.setActive(false)
    }

    func testLiveRunPaneEndsWithTheRun() async throws {
        let h = await hab()
        let f = await fleet([h])
        _ = try await f.rounds.create(habID: "bench", spec("first"), runNow: true)
        await eventually("run row") { f.board.rows.contains { $0.round != nil } }
        let pane = try XCTUnwrap(f.board.rows.first { $0.round != nil }?.target.pane)
        XCTAssertTrue(f.rounds.liveRun(habID: "bench", pane: pane))
        XCTAssertFalse(f.rounds.liveRun(habID: "bench", pane: "%1"), "a session with no round")
        let run = try XCTUnwrap(rows(f).first?.liveRun)
        await h.setRunState(run.id, .takenOver)
        await eventually("ended") { !f.rounds.liveRun(habID: "bench", pane: pane) }
        await f.setActive(false)
    }

    func testAnOlderRunsKeptPaneIsNotLiveAfterAReconnect() async throws {
        let h = await hab()
        let f = await fleet([h])
        _ = try await f.rounds.create(habID: "bench", spec("first"), runNow: true)
        await eventually("run 1") { self.rows(f).first?.round.lastRun?.session != nil }
        let one = try XCTUnwrap(rows(f).first?.round.lastRun)
        // A failed run keeps its pane and its session does not exit.
        await h.setRunState(one.id, .failed, reason: "exit status 1")
        await eventually("failed") { self.rows(f).first?.round.lastRun?.state == .failed }
        await f.rounds.runNow(rows(f)[0])
        await eventually("run 2") { self.rows(f).first?.round.lastRun.map { $0.number == 2 && $0.session != nil } == true }
        let two = try XCTUnwrap(rows(f).first?.round.lastRun)
        func pane(_ run: RoundRun) -> String? { f.board.rows.first { $0.target.session == run.session }?.target.pane }
        let p1 = try XCTUnwrap(pane(one)), p2 = try XCTUnwrap(pane(two))
        // A reconnect brings a snapshot, which forgets the run updates seen before it.
        await h.dropConnections()
        await eventually("online again") { f.board.habs[0].state.status == .online && f.board.habs[0].state.roundRuns.isEmpty }
        XCTAssertFalse(f.rounds.liveRun(habID: "bench", pane: p1), "the round's last run is a newer one: run 1 ended")
        XCTAssertTrue(f.rounds.liveRun(habID: "bench", pane: p2), "run 2 is the round's live last run")
        // A round that is gone, with its run unseen since the reconnect, does not keep the
        // filter on in that pane.
        await h.setRunState(two.id, .failed)
        f.rounds.askDelete(rows(f)[0])
        await f.rounds.confirmDelete()?.value
        await eventually("deleted") { self.rows(f).isEmpty }
        await h.dropConnections()
        await eventually("online after delete") {
            f.board.habs[0].state.status == .online && f.board.habs[0].state.roundRuns.isEmpty
        }
        XCTAssertFalse(f.rounds.liveRun(habID: "bench", pane: p2))
        await f.setActive(false)
    }

    func testRunEndSendsRunUpdatedThenRoundUpdated() async throws {
        let h = await hab()
        let f = await fleet([h])
        _ = try await f.rounds.create(habID: "bench", spec("first"), runNow: true)
        await eventually("running") { self.rows(f).first?.round.lastRun?.state == .running }
        let before = try XCTUnwrap(rows(f).first?.round)
        let run = try XCTUnwrap(before.lastRun)
        await h.setRunState(run.id, .succeeded, result: "ok")
        // The hab's order: the run's update, then the round at its next revision with that
        // run as its last run.
        let held = await h.round(named: "first")
        let r = try XCTUnwrap(held)
        XCTAssertEqual(r.revision, before.revision + 1)
        await eventually("round updated") { self.rows(f).first?.round.revision == r.revision }
        let row = try XCTUnwrap(rows(f).first)
        XCTAssertEqual(row.round.lastRun?.id, run.id)
        XCTAssertEqual(row.round.lastRun?.state, .succeeded)
        XCTAssertEqual(row.round.lastRun?.result, "ok")
        XCTAssertNil(row.liveRun)
        XCTAssertEqual(f.rounds.habRuns(habID: "bench", round: run.round).first?.state, .succeeded)
        await f.setActive(false)
    }

    func testARunStartedHereStopsWaitingToOpen() async throws {
        let h = await hab()
        await h.setRoundSpawn(false) // queued under the host limit: no session yet
        try await h.loadRound(spec("slow"))
        let f = await fleet([h])
        await eventually("round") { self.rows(f).count == 1 }
        await f.rounds.runNow(rows(f)[0])
        XCTAssertTrue(f.rounds.following)
        // The owner opens another session: the run no longer opens over it.
        f.rounds.sessionOpened()
        XCTAssertFalse(f.rounds.following)
        // Or the time limit passes.
        await eventually("queued") { self.rows(f).first?.round.lastRun?.state == .queued }
        await h.setRunState(try XCTUnwrap(rows(f).first?.round.lastRun?.id), .skipped)
        await eventually("ended") { self.rows(f).first?.liveRun == nil }
        let start = Date()
        f.rounds.now = { start }
        await f.rounds.runNow(rows(f)[0])
        XCTAssertTrue(f.rounds.following)
        f.rounds.now = { start.addingTimeInterval(RoundCenter.followLimit - 1) }
        f.rounds.boardChanged()
        XCTAssertTrue(f.rounds.following)
        f.rounds.now = { start.addingTimeInterval(RoundCenter.followLimit + 1) }
        f.rounds.boardChanged()
        XCTAssertFalse(f.rounds.following)
        await f.setActive(false)
    }

    func testTerminalFilterDropsFocusAndPointerReportsInALiveRunPane() {
        func keep(_ s: String, owner: Bool, liveRun: Bool) -> Bool {
            !TerminalReplyFilter.keep(Data(s.utf8), owner: owner, liveRun: liveRun).isEmpty
        }
        let focusIn = "\u{1b}[I", focusOut = "\u{1b}[O"
        let motion = "\u{1b}[<35;10;5M", wheel = "\u{1b}[<64;10;5M", click = "\u{1b}[<0;10;5M"
        let legacyWheel = "\u{1b}[M" + String(UnicodeScalar(UInt8(32 + 64))) + "!!"
        for s in [focusIn, focusOut, motion, wheel, legacyWheel] {
            XCTAssertTrue(keep(s, owner: true, liveRun: false), "the owner's \(s.debugDescription) passes elsewhere")
            XCTAssertFalse(keep(s, owner: true, liveRun: true), "a live run pane drops \(s.debugDescription)")
        }
        XCTAssertTrue(keep(click, owner: true, liveRun: true), "a click is meant as input")
        XCTAssertTrue(keep("a", owner: true, liveRun: true), "typing passes")
        XCTAssertTrue(keep("\u{1b}[1;2R", owner: true, liveRun: true), "the owner's Shift+F3 still passes")
    }

    // MARK: - Words and the form

    func testTriggerAndSlotWords() throws {
        let utc = TimeZone(identifier: "UTC")!
        let locale = Locale(identifier: "en_GB")
        XCTAssertEqual(RoundText.trigger(RoundTrigger(cron: "0 * * * *", kind: .cron, zone: "Europe/Berlin")), "every 1h in Europe/Berlin")
        XCTAssertEqual(RoundText.trigger(RoundTrigger(cron: "*/5 * * * *", kind: .cron, zone: "Europe/Berlin")), "every 5m")
        XCTAssertEqual(RoundText.trigger(RoundTrigger(cron: "0 9 * * 1-5", kind: .cron, zone: "UTC")), "cron 0 9 * * 1-5 in UTC")
        XCTAssertEqual(RoundText.trigger(RoundTrigger(kind: .manual)), "by hand")
        let now = try XCTUnwrap(RoundText.parse("2026-11-02T08:00:00Z"))
        let once = RoundTrigger(at: "2026-11-05T09:00:00Z", kind: .once)
        // The day's punctuation comes from the platform's locale data ("Thu 5 Nov", "Thu, 5 Nov").
        let onceText = RoundText.trigger(once, now: now, timeZone: utc, locale: locale)
        XCTAssertTrue(onceText.hasPrefix("once at Thu") && onceText.hasSuffix("5 Nov 09:00"),
                      "a once trigger names its own day, not today: \(onceText)")
        XCTAssertEqual(RoundText.trigger(RoundTrigger(at: "2026-11-03T09:00:00Z", kind: .once), now: now, timeZone: utc, locale: locale),
                       "once at tomorrow 09:00")
        XCTAssertEqual(RoundText.slot("2026-11-02T09:00:00Z", roundZone: "UTC", now: now, timeZone: utc, locale: locale), "today 09:00",
                       "the same clock: one time")
        XCTAssertEqual(RoundText.slot("2026-11-03T09:00:00Z", roundZone: "Asia/Tokyo", now: now, timeZone: utc, locale: locale),
                       "tomorrow 09:00 (18:00 Asia/Tokyo)", "the viewer's zone, then the round's")
        var run = RoundRun(created: "2026-11-02T08:00:00Z", id: "x", number: 1, round: "r", state: .succeeded, trigger: .manual)
        run.startedAt = "2026-11-02T08:00:00Z"
        run.endedAt = "2026-11-02T09:05:00Z"
        XCTAssertEqual(RoundText.duration(run), "1 h 5 min")
        XCTAssertEqual(RoundText.firstLine("\n  All checks pass.\nmore"), "All checks pass.")
    }

    func testDraftChecksWhatItNeeds() throws {
        var d = RoundDraft(habID: "bench")
        XCTAssertThrowsError(try d.spec().get())
        d.name = "daily"
        XCTAssertEqual(d.spec().failureText, "Choose a project.")
        d.project = "p1"
        XCTAssertEqual(d.spec().failureText, "Write the prompt.")
        d.prompt = "Summarise yesterday's commits."
        d.when = .cron
        XCTAssertEqual(d.spec().failureText, "Write a cron expression.")
        d.cron = "0 9 * * *"
        d.zone = "Europe/Berlin"
        let s = try d.spec().get()
        XCTAssertEqual(s.trigger, RoundTrigger(cron: "0 9 * * *", kind: .cron, zone: "Europe/Berlin"))
        XCTAssertEqual(s.harness, "claude")
        XCTAssertEqual(s.timeoutMs, 3_600_000)
        XCTAssertNil(s.maxRuns)
        d.mode = .resume
        XCTAssertEqual(d.spec().failureText, "Choose the session to resume.")
        d.target = "s1"
        let r = try d.spec().get()
        XCTAssertEqual(r.target, "s1")
        XCTAssertNil(r.project)
        XCTAssertNil(r.onFinish)
    }

    func testEditKeepsTheOnceZoneAndATimeoutInSeconds() throws {
        var r = Round(catchUp: .once, createdAt: "2026-11-01T00:00:00Z", id: "r1", mode: .fresh, name: "later",
                      project: "p1", prompt: "go", revision: 3,
                      trigger: RoundTrigger(at: "2026-11-05T09:00:00Z", kind: .once, zone: "Asia/Tokyo"),
                      updatedAt: "2026-11-01T00:00:00Z")
        r.harness = "claude"
        r.timeoutMs = 30_000
        var d = RoundDraft(habID: "bench", round: r)
        var s = try d.spec().get()
        XCTAssertEqual(s.trigger, RoundTrigger(at: "2026-11-05T09:00:00.000Z", kind: .once, zone: "Asia/Tokyo"),
                       "the zone goes back with the time, so the hab sees the trigger unchanged")
        XCTAssertEqual(s.timeoutMs, 30_000, "an untouched timeout stays as it was")
        d.timeoutMinutes = 5
        s = try d.spec().get()
        XCTAssertEqual(s.timeoutMs, 300_000)
        r.timeoutMs = nil
        s = try RoundDraft(habID: "bench", round: r).spec().get()
        XCTAssertEqual(s.timeoutMs, 3_600_000)
    }

    func testHabSwitchInTheFormMapsTheProjectAndClearsTheTarget() async throws {
        let laptop = await hab("laptop")
        let server = await hab("server")
        let f = await fleet([laptop, server])
        var d = RoundDraft(habID: "laptop")
        d.project = "p1"
        d.target = "s1"
        f.rounds.move(&d, to: "server")
        XCTAssertEqual(d.habID, "server")
        XCTAssertEqual(d.project, "p1", "the same project on the other hab")
        XCTAssertNil(d.target, "a session belongs to its own hab")
        await laptop.put(Fixture.project("p1", name: "other-name"))
        await eventually("project changed") { f.board.habs.first?.state.store.projects["p1"]?.name == "other-name" }
        f.rounds.move(&d, to: "laptop")
        XCTAssertNil(d.project, "the laptop has no project like the server's p1")
        await f.setActive(false)
    }

    func testAlwaysOnSuggestionNamesAHabWithTheProject() async throws {
        let laptop = await hab("laptop")
        let server = await hab("server")
        let f = await fleet([laptop, server])
        XCTAssertNil(f.rounds.suggestion(habID: "laptop", project: "p1"), "no hab is always on")
        f.capcom.alwaysOn = ["server"]
        XCTAssertEqual(f.rounds.suggestion(habID: "laptop", project: "p1")?.id, "server")
        XCTAssertNil(f.rounds.suggestion(habID: "server", project: "p1"), "already always on")
        await server.put(Fixture.project("p1", name: "other-name"))
        await eventually("project changed") { f.board.habs.last?.state.store.projects["p1"]?.name == "other-name" }
        XCTAssertNil(f.rounds.suggestion(habID: "laptop", project: "p1"), "the server lacks the project")
        await f.setActive(false)
    }
}

private extension Result where Failure == RoundActionError {
    var failureText: String? {
        if case .failure(let e) = self { return e.description }
        return nil
    }
}
