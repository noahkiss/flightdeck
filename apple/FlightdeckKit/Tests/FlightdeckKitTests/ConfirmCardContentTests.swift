import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The round and group confirmation cards (capcom.md 6.3, groups.md 3.2): title, rows, items
/// and the done line.
final class ConfirmCardContentTests: XCTestCase {
    static func round(runNow: Bool? = true, command: [String]? = nil, prompt: String? = "Summarise the open tasks",
                      project: String? = "tide-tables", trigger: String? = "every 1h") -> ConfirmRound {
        let spec = RoundSpec(command: command, harness: command == nil ? "claude" : nil, mode: .fresh,
                             model: command == nil ? "opus" : nil, name: "nightly-notes",
                             projectRemote: "git@example.com:tide/tide-tables.git", prompt: command == nil ? prompt : nil,
                             trigger: RoundTrigger(every: "1h", kind: .cron))
        return ConfirmRound(host: "boxb", project: project, round: spec, runNow: runNow, trigger: trigger)
    }

    static func group(amend: Bool? = nil) -> ConfirmGroup {
        let plan = GroupPlan(
            brief: "Move the tide API to v2.",
            items: [
                GroupPlanItem(id: "api", title: "New endpoints", touches: ["api/"]),
                GroupPlanItem(after: "api", id: "docs", review: "off", title: "Docs for v2"),
            ],
            lane: GroupLane(fixInPlace: true, target: "main"),
            members: [
                GroupPlanMember(harness: "claude", host: "boxb", item: "api", model: "opus", name: "claude-3fa9c2-lead-1", role: .lead),
                GroupPlanMember(harness: "codex", item: "api", role: .reviewer),
                GroupPlanMember(harness: "codex", item: "docs", role: .lead),
                GroupPlanMember(harness: "opencode", host: "boxc", item: "tests", role: .worker),
            ],
            project: "tide-tables", title: "Tide API v2")
        return ConfirmGroup(amend: amend, group: "3fa9c2", plan: plan, trust: "New worktrees of tide-tables on boxb and boxc, for claude, codex",
                            warnings: ["boxc is offline"])
    }

    /// An amend as the hab files it: items and members, no title or project.
    static func bareAmend() -> ConfirmGroup {
        let m = GroupPlanMember(harness: "codex", item: "api", role: .worker)
        return ConfirmGroup(amend: true, group: "3fa9c2", plan: GroupPlan(items: [], members: [m, m], project: "", title: ""),
                            trust: "")
    }

    func testBareAmend() {
        let g = Self.bareAmend()
        XCTAssertEqual(ConfirmCardContent.groupFacts(g), [.init("Group", "3fa9c2", mono: true)])
        let w = "worker · boxa · \(harnessName("codex"))"
        XCTAssertEqual(ConfirmCardContent.groupItems(g, home: "boxa"), [.init(id: "api", title: "", notes: [], members: [w, w])])
    }

    static func confirmation(kind: ConfirmKind, state: ConfirmState = .open, record: Bool? = nil,
                             round: ConfirmRound? = nil, group: ConfirmGroup? = nil, result: ConfirmResult? = nil) -> Confirmation {
        Confirmation(created: "2027-01-15T08:00:00Z", group: group, id: "c1", kind: kind, record: record,
                     result: result, round: round, state: state)
    }

    func testTitles() {
        typealias C = ConfirmCardContentTests
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .round, round: C.round())), "Make a round?")
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .round, record: true, round: C.round())), "Round")
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .group, group: C.group())), "Start a group?")
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .group, record: true, group: C.group())), "Group start")
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .group, group: C.group(amend: true))), "Change the group?")
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .group, record: true, group: C.group(amend: true))), "Group change")
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .start)), "Start a session?")
        XCTAssertEqual(ConfirmCardContent.title(C.confirmation(kind: .setup, record: true)), "Project set-up")
    }

    func testRoundFacts() {
        let f = ConfirmCardContent.roundFacts(Self.round())
        XCTAssertEqual(f, [
            .init("Runner", "boxb"),
            .init("Name", "nightly-notes"),
            .init("Mode", "fresh"),
            .init("Trigger", "every 1h"),
            .init("Project", "tide-tables"),
            .init("Harness", harnessName("claude")),
            .init("Model", "opus"),
            .init("Run now", "Yes"),
        ])
    }

    func testRoundCommandRemoteAndOwnTrigger() {
        let f = ConfirmCardContent.roundFacts(Self.round(runNow: false, command: ["make", "nightly"], project: nil, trigger: nil))
        XCTAssertEqual(f, [
            .init("Runner", "boxb"),
            .init("Name", "nightly-notes"),
            .init("Mode", "fresh"),
            .init("Trigger", "every 1h"),
            .init("Project", "git@example.com:tide/tide-tables.git", mono: true),
            .init("Command", "make nightly", mono: true),
        ])
    }

    func testRoundTriggerFromCron() {
        var r = Self.round(trigger: nil)
        r.round.trigger = RoundTrigger(cron: "0 9 * * 1-5", kind: .cron, zone: "Europe/Berlin")
        XCTAssertEqual(ConfirmCardContent.trigger(r), "cron 0 9 * * 1-5 in Europe/Berlin")
        r.round.trigger = RoundTrigger(kind: .manual)
        XCTAssertEqual(ConfirmCardContent.trigger(r), "by hand")
    }

    func testGroupFactsAndItems() {
        let g = Self.group()
        XCTAssertEqual(ConfirmCardContent.groupFacts(g), [
            .init("Task", "Tide API v2"),
            .init("Project", "tide-tables"),
            .init("Target branch", "main"),
            .init("Fix in place", "Yes"),
        ])
        let items = ConfirmCardContent.groupItems(g, home: "boxa")
        let claude = harnessName("claude"), codex = harnessName("codex"), opencode = harnessName("opencode")
        XCTAssertEqual(items, [
            .init(id: "api", title: "New endpoints", notes: ["Touches api/"],
                  members: ["lead · claude-3fa9c2-lead-1 · boxb · \(claude), opus", "reviewer · boxa · \(codex)"]),
            .init(id: "docs", title: "Docs for v2", notes: ["After api", "Review off"], members: ["lead · boxa · \(codex)"]),
            .init(id: "tests", title: "", notes: [], members: ["worker · boxc · \(opencode)"]),
        ])
    }

    func testDoneText() {
        typealias C = ConfirmCardContentTests
        let r = C.confirmation(kind: .round, state: .done, round: C.round(),
                               result: ConfirmResult(host: "boxb", name: "nightly-notes", pane: "", round: "r-1", session: ""))
        XCTAssertEqual(ConfirmCenter.outcome(r), "Made round nightly-notes on boxb")
        let g = C.confirmation(kind: .group, state: .done, group: C.group(),
                               result: ConfirmResult(host: "boxa", pane: "", session: ""))
        XCTAssertEqual(ConfirmCenter.outcome(g), "Started group Tide API v2 on boxa")
        let a = C.confirmation(kind: .group, state: .done, group: C.group(amend: true))
        XCTAssertEqual(ConfirmCenter.outcome(a), "Changed group Tide API v2")
        // The hab's amend sends only items and members: the group goes by its id.
        let bare = C.confirmation(kind: .group, state: .done, group: C.bareAmend(),
                                  result: ConfirmResult(host: "boxa", pane: "", session: ""))
        XCTAssertEqual(ConfirmCenter.outcome(bare), "Changed group 3fa9c2 on boxa")
        let f = C.confirmation(kind: .round, state: .failed, round: C.round())
        XCTAssertEqual(ConfirmCenter.outcome(f), "Failed")
    }
}
