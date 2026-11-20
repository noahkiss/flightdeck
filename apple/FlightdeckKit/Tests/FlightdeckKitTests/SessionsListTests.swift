import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The Sessions list of the app shell (app.md 3.1, 3.8, 3.9): one item kind per row that needs
/// you, confirmations in Needs you, groups as one row that folds their members, plain state
/// words, the standing Capcom row, and the first-run rules.
@MainActor
final class SessionsListTests: XCTestCase {
    private func row(_ id: String, hab: String = "h", session: String? = nil, state: AgentStatus?, kind: BlockedKind? = nil,
                     rank: BoardRank) -> BoardRow {
        BoardRow(id: id, target: SessionTarget(habID: hab, session: session ?? id, pane: "%" + id), host: "bench", title: id,
                 project: "tide-tables", state: state, blockedKind: kind, rank: rank)
    }

    private func card(_ id: String) -> ConfirmCard {
        ConfirmCard(habID: "h", habLabel: "bench", confirmation: Confirmation(
            created: Fixture.stamp(Date()), from: "claude-2", id: id, kind: .start,
            start: ConfirmStart(folder: "/work/tide-tables", harness: "claude", host: "attic", name: "claude-x", project: "tide-tables"), state: .open),
            capcom: false, offline: false)
    }

    private func sections(_ rows: [BoardRow]) -> [BoardSection] {
        BoardSectionKind.allCases.compactMap { k in
            let r = rows.filter { $0.rank.section == k }
            return r.isEmpty ? nil : BoardSection(kind: k, rows: r)
        }
    }

    func testItemKinds() {
        XCTAssertEqual(SessionsList.item(row("p", state: .blocked, kind: .permission, rank: .blocked)),
                       .permission(row("p", state: .blocked, kind: .permission, rank: .blocked)))
        XCTAssertEqual(SessionsList.item(row("q", state: .blocked, kind: .question, rank: .blocked)),
                       .question(row("q", state: .blocked, kind: .question, rank: .blocked)))
        // Sign-in, trust and errors open the session: no row action of their own.
        for k: BlockedKind in [.auth, .trust] {
            let r = row("a", state: .blocked, kind: k, rank: .blocked)
            XCTAssertEqual(SessionsList.item(r), .session(r))
        }
        let e = row("e", state: .error, rank: .error)
        XCTAssertEqual(SessionsList.item(e), .session(e))
    }

    func testSectionsConfirmationsAndCounts() {
        let rows = [
            row("p", state: .blocked, kind: .permission, rank: .blocked),
            row("w", state: .working, rank: .working),
            row("d", state: .done, rank: .atRest),
        ]
        let out = SessionsList.build(sections: sections(rows), confirms: [card("c1")], groups: [])
        XCTAssertEqual(out.map(\.kind), [.needsYou, .working, .done])
        XCTAssertEqual(out[0].items.map(\.id), ["p", "confirm/h/c1"])
        XCTAssertEqual(out[0].title, "Needs you · 2")
        XCTAssertEqual(out[1].title, "Working · 1")

        // A confirmation alone still makes a Needs you section.
        let only = SessionsList.build(sections: [], confirms: [card("c1")], groups: [])
        XCTAssertEqual(only.map(\.kind), [.needsYou])
        XCTAssertTrue(SessionsList.build(sections: [], confirms: [], groups: []).isEmpty)
    }

    func testGroupFoldsMembersButNotOnesThatNeedYou() {
        let member = row("m1", state: .working, rank: .working)
        let asking = row("m2", state: .blocked, kind: .permission, rank: .blocked)
        let other = row("w", state: .working, rank: .working)
        let g = GroupSummary(id: "h/g/g1", title: "Split the parser", habLabel: "bench", members: 3, working: 1, waiting: 2,
                             over: false, since: nil, memberTargets: [member.target, asking.target])
        let out = SessionsList.build(sections: sections([member, asking, other]), confirms: [], groups: [g])
        XCTAssertEqual(out.first { $0.kind == .needsYou }?.items.map(\.id), ["m2"])
        XCTAssertEqual(out.first { $0.kind == .working }?.items.map(\.id), ["w", "h/g/g1"])

        // A member on another hab with the same session id does not fold.
        let elsewhere = row("m1", hab: "other", state: .working, rank: .working)
        let out2 = SessionsList.build(sections: sections([elsewhere]), confirms: [], groups: [g])
        XCTAssertEqual(out2.first { $0.kind == .working }?.items.map(\.id), ["m1", "h/g/g1"])

        // An ended group shows in Done.
        var over = g
        over.over = true
        let out3 = SessionsList.build(sections: [], confirms: [], groups: [over])
        XCTAssertEqual(out3.map(\.kind), [.done])
        XCTAssertEqual(over.line, "Group of 3 · Ended · bench")
        XCTAssertEqual(g.line, "Group of 3 · 1 working, 2 waiting · bench")
    }

    func testGroupSummaryCounts() {
        var g = GroupTests.group()
        g.members.append(GroupMember(harness: "codex", host: "bench", item: "ui", name: "codex-2", role: .reviewer, state: "planned"))
        let card = GroupCard(habID: "h", habLabel: "bench", group: g, online: true)
        let s = GroupSummary.make(card) { m in
            m.session.map { SessionTarget(habID: "h", session: $0, pane: nil) }
        }
        // Working lead, failed lead (not counted), planned reviewer (waiting).
        XCTAssertEqual(s.members, 3)
        XCTAssertEqual(s.working, 1)
        XCTAssertEqual(s.waiting, 1)
        XCTAssertEqual(s.memberTargets.map(\.session), ["s2"])
        XCTAssertEqual(s.title, "Split the parser")
        XCTAssertFalse(s.over)
    }

    func testWithoutKeepsTheRowWhileItNeedsYou() {
        let capcom = row("cap", state: .idle, rank: .atRest)
        let out = SessionsList.without(capcom, in: sections([capcom, row("w", state: .working, rank: .working)]))
        XCTAssertEqual(out.map(\.kind), [.working])

        let asking = row("cap", state: .blocked, kind: .question, rank: .blocked)
        XCTAssertEqual(SessionsList.without(asking, in: sections([asking])).first?.rows.map(\.id), ["cap"])
    }

    func testStateWords() {
        XCTAssertEqual(row("a", state: .blocked, kind: .permission, rank: .blocked).stateWord, "Needs permission")
        XCTAssertEqual(row("a", state: .blocked, kind: .question, rank: .blocked).stateWord, "Asks a question")
        XCTAssertEqual(row("a", state: .blocked, kind: .auth, rank: .blocked).stateWord, "Needs sign-in")
        XCTAssertEqual(row("a", state: .error, rank: .error).stateWord, "Stopped with an error")
        XCTAssertEqual(row("a", state: .working, rank: .working).stateWord, "Working")
        XCTAssertEqual(row("a", state: nil, rank: .other).stateWord, "No agent")
        var unfiled = row("a", state: nil, rank: .other)
        unfiled.target.session = nil
        XCTAssertEqual(unfiled.stateWord, "Unfiled")
    }

    func testFirstRun() {
        XCTAssertTrue(FirstRun.shows(habs: 0, demo: false))
        XCTAssertFalse(FirstRun.shows(habs: 0, demo: true))
        XCTAssertFalse(FirstRun.shows(habs: 1, demo: false))

        // Push waits for a hab online, never asks in demo mode, and asks once.
        XCTAssertFalse(FirstRun.asksForPush(online: 0, demo: false, asked: false))
        XCTAssertTrue(FirstRun.asksForPush(online: 1, demo: false, asked: false))
        XCTAssertFalse(FirstRun.asksForPush(online: 1, demo: true, asked: false))
        XCTAssertFalse(FirstRun.asksForPush(online: 2, demo: false, asked: true))
    }

    /// The demo fleet seeds every kind the Sessions list shows: each needs-you kind, working,
    /// done, a group that folds its member, and two habs.
    @MainActor
    func testDemoSeedCoversTheSessionsList() async {
        let fleet = Fleet()
        fleet.confirms = AppFleet.confirmCenter(fleet)
        fleet.roundDefaults = UserDefaults(suiteName: "SessionsListTests-\(UUID().uuidString)")!
        await DemoFleet.populate(fleet, script: false)
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 2 && fleet.groups.boardCards.count == 1 && fleet.confirms?.approvals.count == 1 }
        let groups = fleet.groups.boardCards.map { c in GroupSummary.make(c) { fleet.groups.target(c, $0) } }
        let out = SessionsList.build(sections: fleet.board.sections, confirms: fleet.confirms?.approvals ?? [], groups: groups)
        let needs = out.first { $0.kind == .needsYou }?.items ?? []
        XCTAssertTrue(needs.contains { if case .permission = $0 { return true } else { return false } })
        XCTAssertTrue(needs.contains { if case .question = $0 { return true } else { return false } })
        XCTAssertTrue(needs.contains { if case .confirm = $0 { return true } else { return false } })
        XCTAssertTrue(needs.contains { if case .session(let r) = $0 { return r.state == .error } else { return false } })
        let working = out.first { $0.kind == .working }?.items ?? []
        XCTAssertTrue(working.contains { if case .group(let g) = $0 { return g.members == 2 && g.working == 1 && g.waiting == 1 } else { return false } })
        // The group's member session folds into the group's row.
        XCTAssertFalse(working.contains { $0.id == "demo-workbench/s/s6" })
        XCTAssertNotNil(out.first { $0.kind == .done })
        // The migration request carries an id, so Allow and Deny are on.
        XCTAssertTrue(fleet.board.rows.contains { $0.title == "Run the database migration?" })
        await fleet.setActive(false)
    }
}
