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
        await f.rounds.confirmDelete()
        calls = await h.roundCalls
        XCTAssertFalse(calls.contains("round.delete"), "a cancelled delete sends nothing")
        f.rounds.askDelete(row)
        await f.rounds.confirmDelete()
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
        // A banner for a session the board does not show yet waits for it.
        f.rounds.open(PushMeta(kind: "notice", hab: "bench", session: "s-later", notice: "n1", round: "r1", run: "r1-run1"))
        XCTAssertNil(f.rounds.openRequest)
        await h.put(Fixture.session("s-later", name: "later", pane: nil, project: "p1"))
        await eventually("opened") { f.rounds.openRequest?.session == "s-later" }
        // A request banner is the approval sheet's; a banner with no session opens nothing.
        f.rounds.openRequest = nil
        f.rounds.open(PushMeta(kind: "permission", hab: "bench", session: "s1", pane: "%1", request: "q"))
        f.rounds.open(PushMeta(kind: "notice", hab: "bench"))
        f.rounds.boardChanged()
        XCTAssertNil(f.rounds.openRequest)
        // Open on a round banner maps to the open intent.
        let m = PushMeta(kind: "notice", hab: "bench", session: "s1", round: "r1", run: "x")
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.openAction, meta: m), .open(m))
        let decoded = PushMeta(userInfo: [PushMeta.userInfoKey: ["v": 1, "kind": "notice", "hab": "bench", "session": "s1", "round": "r1", "run": "x"]])
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
