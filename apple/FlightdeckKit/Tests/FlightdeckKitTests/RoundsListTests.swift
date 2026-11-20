import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The Rounds tab's list (app.md 21.2): sections by trigger, rows across habs, the words of
/// each row, and what a Capcom job's row says it may do. Also the Capcom tab's words (4.10).
@MainActor
final class RoundsListTests: XCTestCase {
    private let utc = TimeZone(identifier: "UTC")!
    private let en = Locale(identifier: "en_GB")
    /// Wednesday 2026-11-18 12:00 UTC.
    private let now = Date(timeIntervalSince1970: 1_795_003_200)

    private func round(_ name: String, mode: RoundMode = .fresh, trigger: RoundTrigger, project: String? = "p-tide",
                       prompt: String? = "Do the thing.\nMore words.", actions: [RoundAction]? = nil,
                       next: Date? = nil, paused: Bool = false, last: RoundRun? = nil) -> Round {
        Round(actions: actions, createdAt: "2026-11-01T00:00:00Z", id: "r-\(name)", lastRun: last, mode: mode,
              name: name, nextSlot: next.map(RoundText.stamp), paused: paused ? true : nil, project: project,
              prompt: prompt, revision: 1, trigger: trigger, updatedAt: "2026-11-01T00:00:00Z")
    }

    private func run(_ state: RoundRunState, ended: Date, outcome: String? = nil) -> RoundRun {
        let at = RoundText.stamp(ended)
        return RoundRun(created: at, endedAt: at, id: "run", number: 1,
                        outcome: outcome.map { RoundRunOutcome(at: at, word: $0) }, round: "r", startedAt: at,
                        state: state, trigger: .cron)
    }

    private func hab(_ id: String, _ label: String, online: Bool = true, _ rounds: [Round]) -> RoundHab {
        RoundHab(id: id, label: label, online: online, rows: rounds.map { RoundRow(habID: id, round: $0) })
    }

    private func sections(_ habs: [RoundHab], eventActions: String? = nil) -> [RoundsListSection] {
        RoundsList.sections(habs, projectNames: ["h1": ["p-tide": "tide-tables"], "h2": ["p-tide": "tide-tables"]],
                            eventActions: eventActions, now: now, timeZone: utc, locale: en)
    }

    private let daily = RoundTrigger(cron: "0 0 * * *", kind: .cron, zone: "UTC")
    private let onNotice = RoundTrigger(event: "notice.raised", kind: .event)

    func testSectionsSplitByTriggerAcrossHabs() {
        let s = sections([
            hab("h1", "workbench", [
                round("b-later", trigger: daily, next: now.addingTimeInterval(7200)),
                round("checks", mode: .capcom, trigger: onNotice, project: nil, actions: [.handOff]),
                round("by-hand", trigger: RoundTrigger(kind: .manual)),
            ]),
            hab("h2", "attic", [
                round("a-soon", trigger: daily, next: now.addingTimeInterval(3600)),
                round("asleep", trigger: daily, paused: true),
                round("brief", mode: .capcom, trigger: daily, project: nil, next: now.addingTimeInterval(9000)),
            ]),
        ])
        XCTAssertEqual(s.map(\.kind), [.schedule, .events, .byHand])
        XCTAssertEqual(s.map(\.title), ["On a schedule", "When something happens", "By hand"])
        XCTAssertEqual(s[0].rows.map(\.title), ["a-soon", "b-later", "brief", "asleep"],
                       "soonest next run first across habs; a scheduled Capcom job stays with the schedule; paused last")
        XCTAssertEqual(s[1].rows.map(\.title), ["checks"])
        XCTAssertEqual(s[1].note, "Capcom handles these", "every row is a Capcom job")
        XCTAssertNil(s[0].note, "not every row is a Capcom job")
        XCTAssertNil(s[2].note)
    }

    func testEmptySectionsAreLeftOut() {
        XCTAssertEqual(sections([hab("h1", "workbench", [round("x", trigger: daily)])]).map(\.kind), [.schedule])
        XCTAssertTrue(sections([hab("h1", "workbench", [])]).isEmpty)
    }

    func testTiesBreakByHabLabel() {
        let a = round("same", trigger: RoundTrigger(kind: .manual))
        let s = sections([hab("h1", "workbench", [a]), hab("h2", "attic", [a])])
        XCTAssertEqual(s[0].rows.map(\.meta), ["by hand · tide-tables · attic", "by hand · tide-tables · workbench"])
    }

    func testRowWords() throws {
        let s = sections([hab("h1", "workbench", [
            round("passed", trigger: daily, next: now.addingTimeInterval(3600), last: run(.succeeded, ended: now.addingTimeInterval(-3 * 86400))),
            round("failed", trigger: daily, prompt: nil, next: now.addingTimeInterval(7200), last: run(.failed, ended: now.addingTimeInterval(-9 * 3600))),
            round("fresh", trigger: daily, prompt: "\n  Line one  \nline two", next: now.addingTimeInterval(9000)),
            round("asleep", trigger: daily, paused: true, last: run(.succeeded, ended: now)),
        ])])
        let rows = Dictionary(uniqueKeysWithValues: s[0].rows.map { ($0.title, $0) })
        let passed = try XCTUnwrap(rows["passed"])
        XCTAssertEqual(passed.summary, "Do the thing.", "the prompt's first line")
        XCTAssertEqual(passed.meta, "every 1d in UTC · tide-tables · workbench")
        XCTAssertEqual(passed.status, "Succeeded")
        XCTAssertEqual(passed.statusState, .succeeded)
        XCTAssertEqual(passed.when, "3 days ago")
        XCTAssertNil(passed.reach)

        let failed = try XCTUnwrap(rows["failed"])
        XCTAssertNil(failed.summary)
        XCTAssertEqual(failed.status, "Failed")
        XCTAssertEqual(failed.when, "9 hours ago")

        let fresh = try XCTUnwrap(rows["fresh"])
        XCTAssertEqual(fresh.summary, "Line one")
        XCTAssertEqual(fresh.status, "Next", "no run yet: the next run")
        XCTAssertEqual(fresh.when, "today 14:30")

        let asleep = try XCTUnwrap(rows["asleep"])
        XCTAssertTrue(asleep.paused)
        XCTAssertEqual(asleep.meta, "Paused · every 1d in UTC · tide-tables · workbench")
        XCTAssertNil(asleep.status, "a paused row shows Unpause, not its last run")
    }

    func testCommandRoundShowsItsCommandAndOfflineHab() throws {
        var r = round("cmd", trigger: RoundTrigger(kind: .manual), project: "p-unknown", prompt: nil)
        r.command = ["make", "backup"]
        let row = try XCTUnwrap(sections([hab("h1", "workbench", online: false, [r])]).first?.rows.first)
        XCTAssertEqual(row.summary, "make backup")
        XCTAssertEqual(row.meta, "by hand · p-unknown · workbench (offline)", "an unknown project shows its id")
    }

    func testCapcomJobRow() throws {
        let acted = run(.succeeded, ended: now.addingTimeInterval(-2 * 3600), outcome: "acted")
        let s = sections([hab("h1", "workbench", [
            round("checks", mode: .capcom, trigger: onNotice, project: nil, actions: [.handOff], last: acted),
            round("reviews", mode: .capcom, trigger: onNotice, project: nil),
        ])])
        let rows = Dictionary(uniqueKeysWithValues: s[0].rows.map { ($0.title, $0) })
        let checks = try XCTUnwrap(rows["checks"])
        XCTAssertEqual(checks.meta, "Acted today 10:00 · on notice.raised · workbench")
        XCTAssertEqual(checks.reach, .actsAlone)
        XCTAssertNil(checks.status, "a job shows what it may do, not a run state")
        let reviews = try XCTUnwrap(rows["reviews"])
        XCTAssertEqual(reviews.meta, "No run yet · on notice.raised · workbench")
        XCTAssertEqual(reviews.reach, .tellsYou)
    }

    func testPausedJobShowsUnpauseNotItsReach() throws {
        let s = sections([hab("h1", "workbench", online: false, [
            round("checks", mode: .capcom, trigger: onNotice, project: nil, actions: [.handOff], paused: true),
        ])])
        let row = try XCTUnwrap(s.first?.rows.first)
        XCTAssertTrue(row.paused, "the row shows Unpause")
        XCTAssertFalse(row.online, "Unpause waits for the hab")
        XCTAssertEqual(row.reach, .actsAlone, "still a job, so its section's note holds")
        XCTAssertEqual(s.first?.note, "Capcom handles these")
        XCTAssertTrue(row.meta.hasPrefix("Paused · No run yet"), row.meta)
    }

    /// The home's `capcom.event_actions` reaches the job rows (capcom-service.md 9.6).
    func testHomeEventActionsFromTheSnapshot() async throws {
        let h = await sampleHab("bench")
        await h.setCapability(RoundCenter.capability, true)
        await h.loadSetting(SettingsRegistry.capcomEventActions, .string("ask"))
        try await h.loadRound(RoundSpec(actions: [.handOff], mode: .capcom, name: "checks", prompt: "Hand it off.",
                                        trigger: RoundTrigger(event: "notice.raised", kind: .event)))
        let f = Fleet()
        f.roundDefaults = UserDefaults(suiteName: "RoundsListTests-\(UUID().uuidString)")!
        f.add(HabConnection(id: "bench", transport: h.transport(), timing: fastTiming()), label: "bench")
        await f.setActive(true)
        await eventually("round") { f.rounds.habs.first?.rows.count == 1 }
        XCTAssertEqual(f.rounds.listSections(home: "bench").first?.rows.first?.reach, .asksFirst)
        XCTAssertEqual(f.rounds.listSections(home: nil).first?.rows.first?.reach, .actsAlone, "no home: the built-in allow")
        await f.setActive(false)
    }

    func testReachFollowsTheHomesEventActions() {
        let some: Set<RoundAction> = [.handOff], none: Set<RoundAction> = []
        XCTAssertEqual(CapcomJobReach.of(words: some, eventActions: "allow"), .actsAlone)
        XCTAssertEqual(CapcomJobReach.of(words: none, eventActions: "allow"), .tellsYou)
        XCTAssertEqual(CapcomJobReach.of(words: some, eventActions: "ask"), .asksFirst)
        XCTAssertEqual(CapcomJobReach.of(words: none, eventActions: "ask"), .tellsYou)
        XCTAssertEqual(CapcomJobReach.of(words: none, eventActions: "all"), .actsAlone, "Bypass needs no grant")
        XCTAssertEqual(CapcomJobReach.of(words: some, eventActions: "off"), .tellsYou)
        XCTAssertEqual(CapcomJobReach.of(words: some, eventActions: nil), .actsAlone,
                       "unknown reads as the built-in allow: never claim a check the hab may skip")
        XCTAssertEqual(CapcomJobReach.of(words: some, eventActions: "later"), .actsAlone)
        XCTAssertEqual(sections([hab("h1", "workbench", [round("j", mode: .capcom, trigger: onNotice, actions: [.handOff])])],
                                eventActions: "ask").first?.rows.first?.reach, .asksFirst)
    }

    /// The demo's Rounds tab: the rounds of both habs in one list (app.md 21.2).
    func testDemoRoundsTab() async throws {
        let f = Fleet()
        f.roundDefaults = UserDefaults(suiteName: "RoundsListTests-\(UUID().uuidString)")!
        await DemoFleet.populate(f, script: false)
        await f.setActive(true)
        await eventually("rounds") { f.rounds.habs.flatMap(\.rows).count == 5 }
        let s = f.rounds.listSections(home: "demo-workbench")
        XCTAssertEqual(s.map(\.kind), [.schedule, .events])
        XCTAssertEqual(Set(s[0].rows.map(\.title)), ["update-dependencies", "nightly-link-check", "morning-brief", "weekly-summary"])
        XCTAssertEqual(s[0].rows.last?.title, "weekly-summary", "paused last")
        let deps = try XCTUnwrap(s[0].rows.first { $0.title == "update-dependencies" })
        XCTAssertEqual(deps.status, "Succeeded")
        XCTAssertTrue(deps.meta.hasSuffix("· tide-tables · attic"), deps.meta)
        XCTAssertEqual(s[0].rows.first { $0.title == "nightly-link-check" }?.status, "Failed")
        XCTAssertEqual(s[1].rows.map(\.title), ["build-failures"])
        XCTAssertEqual(s[1].rows.first?.reach, .actsAlone)
        XCTAssertEqual(s[0].rows.first { $0.title == "morning-brief" }?.reach, .tellsYou)
        await f.setActive(false)
    }

    // MARK: - The Capcom tab

    func testCapcomTabWords() async throws {
        let f = Fleet()
        await DemoFleet.populate(f, script: false)
        await f.setActive(true)
        await eventually("ready") { f.capcom.entry == .ready(habID: "demo-workbench") }
        XCTAssertEqual(f.capcom.tabSubtitle(f.capcom.entry), "Runs on workbench · answers and routes anything")
        XCTAssertEqual(f.capcom.tabSubtitle(.unassigned), "No Capcom host assigned")
        XCTAssertEqual(f.capcom.tabSubtitle(.offline(habID: "demo-workbench")), "workbench · offline")
        await f.setActive(false)
        XCTAssertEqual(ChatAnswer.placeholder(.message, gate: .now, agent: nil, capcom: true), "Ask Capcom")
        XCTAssertEqual(ChatAnswer.placeholder(.message, gate: .now, agent: nil), "Message")
    }
}
