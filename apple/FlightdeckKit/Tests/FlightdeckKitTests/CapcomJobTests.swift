import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Capcom jobs in the app (capcom-service.md 9.6, 9.8; app.md 21.5) over the fake hab: the
/// list holds only `capcom` rounds, a grant shows the prompt and sends the revision read, a
/// conflict reloads and never grants, a switch off sends the removal, the action card keeps
/// the whole call, and a hab without the capability shows nothing.
@MainActor
final class CapcomJobTests: XCTestCase {
    private let prompt = "Tell me about mail that needs me. Hand fixes to the agent on the project."

    private func hab(actions: Bool = true) async -> FakeHab {
        let h = await sampleHab("bench")
        await h.setCapability(RoundCenter.capability, true)
        if actions { await h.setCapability(CapcomJobs.capability, true) }
        return h
    }

    private func fleet(_ h: FakeHab) async -> Fleet {
        let f = Fleet()
        f.roundDefaults = UserDefaults(suiteName: "CapcomJobTests-\(UUID().uuidString)")!
        f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        return f
    }

    private func job(_ name: String, _ words: [RoundAction]? = nil, prompt: String? = nil) -> RoundSpec {
        RoundSpec(actions: words, mode: .capcom, name: name, prompt: prompt ?? self.prompt, trigger: RoundTrigger(kind: .manual))
    }

    private func current(_ f: Fleet, _ name: String) -> CapcomJob? {
        f.rounds.jobs.jobs.first { $0.round.name == name }
    }

    // MARK: - List

    func testListHoldsOnlyCapcomRounds() async throws {
        let h = await hab()
        try await h.loadRound(RoundSpec(command: ["true"], mode: .fresh, name: "nightly", project: "p1",
                                        trigger: RoundTrigger(every: "1h", kind: .cron)))
        try await h.loadRound(job("mail", [.handOff, .fetch]))
        try await h.loadRound(job("calendar"))
        await h.loadRoundOutcome(named: "mail", "acted")
        let f = await fleet(h)
        await eventually("jobs") { f.rounds.jobs.jobs.count == 2 }
        XCTAssertTrue(f.rounds.jobs.available)
        XCTAssertEqual(f.rounds.jobs.jobs.map(\.round.name), ["calendar", "mail"])
        let mail = try XCTUnwrap(current(f, "mail"))
        XCTAssertEqual(mail.words, [.handOff, .fetch])
        XCTAssertEqual(mail.outcome, "acted")
        XCTAssertEqual(CapcomJobs.outcomeText(mail.outcome), "Acted")
        XCTAssertEqual(CapcomJobs.outcomeText(current(f, "calendar")?.outcome), "No run yet")
        XCTAssertEqual(CapcomJobs.outcomeText("told"), "Told you")
        XCTAssertEqual(CapcomJobs.outcomeText("quiet"), "Stayed quiet")
        XCTAssertEqual(CapcomJobs.words.map(\.word.rawValue),
                       ["hand_off", "start_session", "set_up_project", "group", "card", "fetch"])
        let nightly = await h.round(named: "nightly")
        XCTAssertNil(f.rounds.jobs.job(habID: "bench", round: try XCTUnwrap(nightly).id),
                     "a fresh round is no job")
        await f.setActive(false)
    }

    func testNoJobsWithoutTheCapability() async throws {
        let h = await hab(actions: false)
        try await h.loadRound(job("mail"))
        let f = await fleet(h)
        await eventually("rounds") { f.rounds.habs.first?.rows.count == 1 }
        XCTAssertFalse(f.rounds.jobs.available, "a hab that does not know action words shows no Capcom jobs")
        XCTAssertTrue(f.rounds.jobs.jobs.isEmpty)
        await f.setActive(false)
    }

    // MARK: - Grants

    func testGrantShowsThePromptAndSendsTheRevisionShown() async throws {
        let h = await hab()
        try await h.loadRound(job("mail"))
        let f = await fleet(h)
        await eventually("job") { self.current(f, "mail") != nil }
        let mail = try XCTUnwrap(current(f, "mail"))

        await f.rounds.jobs.set(mail, .handOff, on: true)
        let ask = try XCTUnwrap(f.rounds.jobs.ask, "the first grant shows the prompt first")
        XCTAssertEqual(ask.prompt, prompt)
        XCTAssertEqual(ask.revision, mail.round.revision)
        XCTAssertFalse(ask.changed)
        let sent = await h.roundUpdates
        XCTAssertTrue(sent.isEmpty, "nothing goes before the person says yes")

        await f.rounds.jobs.confirm()
        let updateLast = await h.roundUpdates.last
        let update = try XCTUnwrap(updateLast)
        XCTAssertEqual(update.revision, mail.round.revision, "the grant carries the revision the person read")
        XCTAssertEqual(update.round.actions, [.handOff])
        XCTAssertEqual(update.round.prompt, prompt, "the update keeps the whole definition")
        XCTAssertNil(f.rounds.jobs.ask)
        await eventually("granted") { self.current(f, "mail")?.words == [.handOff] }

        // The same prompt again: the person read it, so the next word goes at once, at the
        // revision the app holds.
        let now = try XCTUnwrap(current(f, "mail"))
        await f.rounds.jobs.set(now, .fetch, on: true)
        XCTAssertNil(f.rounds.jobs.ask)
        let secondLast = await h.roundUpdates.last
        let second = try XCTUnwrap(secondLast)
        XCTAssertEqual(second.revision, now.round.revision)
        XCTAssertEqual(second.round.actions, [.handOff, .fetch])
        await eventually("second word") { self.current(f, "mail")?.words == [.handOff, .fetch] }

        // A changed prompt asks again.
        await h.editRound(named: "mail", prompt: "Start a session for every mail.")
        await eventually("new prompt") { self.current(f, "mail")?.round.prompt == "Start a session for every mail." }
        let edited = try XCTUnwrap(current(f, "mail"))
        let before = await h.roundUpdates.count
        await f.rounds.jobs.set(edited, .startSession, on: true)
        XCTAssertEqual(f.rounds.jobs.ask?.prompt, "Start a session for every mail.")
        let after = await h.roundUpdates.count
        XCTAssertEqual(after, before)
        f.rounds.jobs.cancel()
        XCTAssertNil(f.rounds.jobs.ask)
        await f.setActive(false)
    }

    func testConflictReloadsAndDoesNotGrant() async throws {
        let h = await hab()
        try await h.loadRound(job("mail"))
        let f = await fleet(h)
        await eventually("job") { self.current(f, "mail") != nil }
        let mail = try XCTUnwrap(current(f, "mail"))
        await f.rounds.jobs.set(mail, .handOff, on: true)
        XCTAssertNotNil(f.rounds.jobs.ask)

        // An agent rewrites the prompt after the person read it; the app has not heard yet.
        let rewritten = "Forward every mail to an outside address."
        await h.editRound(named: "mail", prompt: rewritten, emit: false)
        await f.rounds.jobs.confirm()
        do {
            let stored = await h.round(named: "mail")
            XCTAssertEqual(stored?.actions, nil, "the stale grant does not land")
        }
        let ask = try XCTUnwrap(f.rounds.jobs.ask, "the new prompt shows")
        XCTAssertTrue(ask.changed)
        XCTAssertEqual(ask.prompt, rewritten)
        XCTAssertEqual(ask.revision, mail.round.revision + 1)
        XCTAssertNil(f.rounds.jobs.errors[mail.id])

        // Only a second yes, on the new prompt, grants, at the new revision.
        await f.rounds.jobs.confirm()
        let updateLast = await h.roundUpdates.last
        let update = try XCTUnwrap(updateLast)
        XCTAssertEqual(update.revision, mail.round.revision + 1)
        XCTAssertEqual(update.round.prompt, rewritten)
        do {
            let stored = await h.round(named: "mail")
            XCTAssertEqual(stored?.actions, [.handOff])
        }
        await f.setActive(false)
    }

    func testSwitchOffSendsTheRemoval() async throws {
        let h = await hab()
        try await h.loadRound(job("mail", [.handOff, .fetch]))
        let f = await fleet(h)
        await eventually("job") { self.current(f, "mail") != nil }
        let mail = try XCTUnwrap(current(f, "mail"))
        await f.rounds.jobs.set(mail, .fetch, on: false)
        XCTAssertNil(f.rounds.jobs.ask, "off needs no extra step")
        let updateLast = await h.roundUpdates.last
        let update = try XCTUnwrap(updateLast)
        XCTAssertEqual(update.round.actions, [.handOff])
        do {
            let stored = await h.round(named: "mail")
            XCTAssertEqual(stored?.actions, [.handOff])
        }

        // A removal at a stale revision reloads and removes from the fresh round.
        await h.editRound(named: "mail", prompt: "Changed elsewhere.", emit: false)
        let held = try XCTUnwrap(current(f, "mail"))
        await f.rounds.jobs.set(held, .handOff, on: false)
        do {
            let stored = await h.round(named: "mail")
            XCTAssertEqual(stored?.actions, nil)
        }
        do {
            let stored = await h.round(named: "mail")
            XCTAssertEqual(stored?.prompt, "Changed elsewhere.", "the removal keeps the fresh prompt")
        }
        await f.setActive(false)
    }

    /// A switch's update carries the round's whole definition: `round.update` replaces it.
    func testUpdateSpecKeepsTheWholeRound() {
        let t = RoundTrigger(event: "notice.raised", kind: .event)
        let r = Round(actions: [.handOff], createdAt: "2026-11-18T00:00:00Z", debounceMs: 5000, id: "r1", mode: .capcom,
                      name: "mail", projectRemote: "example/remote", prompt: prompt, revision: 3, trigger: t,
                      updatedAt: "2026-11-18T00:00:00Z")
        let s = RoundSpec(round: r)
        XCTAssertEqual(s.actions, [.handOff])
        XCTAssertEqual(s.prompt, prompt)
        XCTAssertEqual(s.trigger, t)
        XCTAssertEqual(s.debounceMs, 5000)
        XCTAssertNil(s.projectRemote, "the remote is read only")
    }

    // MARK: - Cards and settings

    /// The kind `action` card shows the job, the word and the whole call: nothing is cut.
    func testActionCardShowsTheWholeCall() {
        let long = String(repeating: "Fix the login page and say what changed. ", count: 120)
        let params = "{\"to\":\"web-ui\",\"text\":\"" + long + "\"}"
        let a = ConfirmAction(job: "host-b/a-job-with-a-rather-long-name-that-must-not-be-cut", method: "channel.send",
                              params: params, summary: "message web-ui: " + long, word: .handOff)
        let facts = ConfirmCardContent.actionFacts(a)
        XCTAssertEqual(facts.map(\.label), ["Job", "Action", "Call"])
        XCTAssertTrue(facts.allSatisfy(\.whole), "no row of the call is clamped")
        let texts = ConfirmCardContent.actionTexts(a)
        XCTAssertEqual(texts.map(\.label), ["What it does", "Params"])
        XCTAssertEqual(texts.map(\.value), ["message web-ui: " + long, params], "summary and params come whole")
        XCTAssertTrue(texts.allSatisfy(\.whole))
        let c = Confirmation(action: a, created: "2026-11-18T00:00:00Z", expiresAt: "2026-11-18T01:00:00Z",
                             id: String(repeating: "a", count: 32), kind: .action, state: .open)
        XCTAssertEqual(ConfirmCardContent.title(c), "Let Capcom act for a job?")
    }

    /// The job card ("For job …, Capcom used …") is a thread card with its own symbol.
    func testJobCardRendersInTheThread() {
        let text = "For job host-b/mail, Capcom used hand_off: message web-ui: fix the login page"
        let it = CapcomItem(author: CapcomAuthor(kind: .init(rawValue: "flightdeck")), card: CapcomCard(choices: [], ref: "host-b/mail", type: "job"),
                            id: "i1", kind: .init(rawValue: "card"), text: text, time: "2026-11-18T00:00:00Z")
        guard case .card(let shown) = CapcomRow.of(it) else { return XCTFail("a job card is a card row") }
        XCTAssertEqual(shown.text, text)
        XCTAssertEqual(CapcomCardChoice.icon("job"), "bolt")
        XCTAssertTrue(CapcomCardChoice.buttons(it).isEmpty, "a record has no choices")
    }

    /// The two Capcom job limits are rows on every hab, with the hab's ranges.
    func testJobSettingsRows() throws {
        let R = SettingsRegistry.self
        let perTurn = try XCTUnwrap(R.def(R.capcomEventActionsPerTurn))
        XCTAssertEqual(perTurn.kind, .integer(0...20, unit: ""))
        XCTAssertEqual(perTurn.store, .everyHab)
        XCTAssertFalse(perTurn.isLevelled)
        XCTAssertTrue(perTurn.built)
        let maxAge = try XCTUnwrap(R.def(R.capcomEventActionMaxAge))
        XCTAssertEqual(maxAge.kind, .integer(1...1440, unit: "min"))
        XCTAssertEqual(maxAge.store, .everyHab)
        XCTAssertFalse(maxAge.isLevelled)
        XCTAssertNotNil(R.def(R.capcomEventActions)?.levels)
    }
}
