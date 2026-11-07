import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Agent groups in the app (groups.md 5.4, 3.2) over the fake hab: records from the snapshot
/// and `group.updated` by revision, the board's order and cut-off, the coordinator's cards,
/// member rows that open their session and the member mark on the board, the actions and
/// their refusals, and the plan card's edits.
@MainActor
final class GroupTests: XCTestCase {
    static let t0 = Date(timeIntervalSince1970: 1_800_000_000)

    private func hab(_ host: String = "bench", member: Bool = false) async -> FakeHab {
        let h = FakeHab(hostID: host)
        var s2 = Fixture.session("s2", name: "claude-3fa9c2-lead-1", pane: "%2", project: "p1")
        if member { s2.group = SessionGroup(id: "g1", item: "api", role: .lead) }
        await h.load(
            projects: [Fixture.project("p1", name: "tide-tables", sessions: ["s1", "s2"])],
            sessions: [Fixture.session("s1", name: "coord", pane: "%1", project: "p1"), s2],
            panes: [Fixture.pane("%1", session: "s1"), Fixture.pane("%2", session: "s2")],
            agents: [Fixture.agent(pane: "%1", harness: "claude", state: .idle)])
        await h.setCapability(GroupCenter.capability, true)
        return h
    }

    private func fleet(_ habs: [FakeHab]) async -> Fleet {
        let f = Fleet()
        f.confirms = AppFleet.confirmCenter(f)
        f.roundDefaults = UserDefaults(suiteName: "GroupTests-\(UUID().uuidString)")!
        for h in habs {
            f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        return f
    }

    static func plan() -> GroupPlan {
        GroupPlan(brief: "Split the parser", items: [
            GroupPlanItem(id: "api", title: "API"),
            GroupPlanItem(after: "api", id: "ui", title: "UI"),
        ], members: [
            GroupPlanMember(harness: "claude", item: "api", role: .lead),
            GroupPlanMember(harness: "codex", host: "beta", item: "ui", role: .lead),
            GroupPlanMember(harness: "codex", item: "api", role: .reviewer),
        ], project: "tide-tables", title: "Split the parser")
    }

    static func group(_ id: String = "g1", state: String = "running", created: Date = t0, ended: Date? = nil) -> GroupRecord {
        GroupRecord(confirmation: GroupConfirmationRef(id: "none"),
                    coordinator: GroupCoordinator(address: "bench/coord", name: "coord", session: "s1"),
                    created: Fixture.stamp(created), ended: ended.map(Fixture.stamp), generation: 1, home: "SHA256:fake-bench", id: id,
                    inbox: [], items: [
                        GroupItem(branch: "group/\(id)/api", id: "api", lead: "claude-3fa9c2-lead-1", state: "working", title: "API"),
                        GroupItem(after: "api", id: "ui", reason: "waits for api to reach review", state: "queued", title: "UI"),
                    ], lane: GroupLaneState(queue: []), leftovers: [],
                    members: [
                        GroupMember(agent: AgentStatus(rawValue: "working"), contextPct: 31, harness: "claude", host: "bench", item: "api",
                                    name: "claude-3fa9c2-lead-1", role: .lead, session: "s2", state: "running"),
                        GroupMember(harness: "codex", host: "beta", item: "ui", name: "codex-3fa9c2-lead-2", reason: "the harness exited",
                                    role: .lead, state: "failed"),
                    ],
                    pause: GroupPause(), plan: plan(), project: "tide-tables", revision: 1, state: state, title: "Split the parser",
                    updated: Fixture.stamp(ended ?? created), waits: [])
    }

    // MARK: - Records

    func testSnapshotThenEventsByRevision() async throws {
        let h = await hab()
        await h.loadGroup(Self.group())
        let f = await fleet([h])
        await eventually("card") { f.groups.all.count == 1 }
        XCTAssertEqual(f.groups.all.first?.group.state, "running")

        var g = Self.group()
        g.state = "paused"
        g.pause.user = Fixture.stamp(Date())
        g.waits = ["disk on beta"]
        await h.emit(type: "group.updated", id: "g1", revision: 3, data: g)
        await eventually("revision 3") { f.groups.all.first?.group.revision == 3 }
        let card = try XCTUnwrap(f.groups.all.first)
        XCTAssertEqual(card.group.state, "paused")
        XCTAssertEqual(GroupText.waits(card.group), "waiting: disk on beta")
        XCTAssertEqual(GroupText.pause(card.group), "Paused by you")

        // A lower revision changes nothing.
        g.state = "running"
        await h.emit(type: "group.updated", id: "g1", revision: 2, data: g)
        await h.emit(type: "group.updated", id: "g2", revision: 1, data: Self.group("g2"))
        await eventually("g2") { f.groups.all.count == 2 }
        XCTAssertEqual(f.groups.all.first { $0.group.id == "g1" }?.group.state, "paused")
        await f.setActive(false)
    }

    func testNoGroupsWithoutTheCapability() async {
        let h = await hab()
        await h.loadGroup(Self.group())
        await h.setCapability(GroupCenter.capability, false)
        let f = await fleet([h])
        XCTAssertTrue(f.groups.all.isEmpty)
        await f.setActive(false)
    }

    func testOrderCutOffAndCoordinator() async throws {
        let now = Self.t0.addingTimeInterval(3 * 86400)
        let cards = [
            GroupCard(habID: "a", habLabel: "a", group: Self.group("old", state: "ended", created: Self.t0, ended: Self.t0.addingTimeInterval(3600)), online: true),
            GroupCard(habID: "a", habLabel: "a", group: Self.group("new", state: "running", created: Self.t0.addingTimeInterval(60)), online: true),
            GroupCard(habID: "a", habLabel: "a", group: Self.group("ask", state: "confirming", created: Self.t0), online: true),
            GroupCard(habID: "a", habLabel: "a", group: Self.group("run", state: "running", created: Self.t0), online: true),
            GroupCard(habID: "a", habLabel: "a", group: Self.group("late", state: "dropped", created: now.addingTimeInterval(-60)), online: true),
        ]
        XCTAssertEqual(GroupCenter.order(cards).map(\.group.id), ["ask", "new", "run", "late", "old"],
                       "confirming, then running newest first, then ended and dropped")

        let h = await hab()
        await h.loadGroup(Self.group("old", state: "ended", created: Self.t0, ended: Self.t0.addingTimeInterval(3600)))
        await h.loadGroup(Self.group("run"))
        let f = await fleet([h])
        f.groups.now = { now }
        await eventually("cards") { f.groups.all.count == 2 }
        XCTAssertEqual(f.groups.boardCards.map(\.group.id), ["run"], "an end over a day ago leaves the board")
        XCTAssertEqual(Set(f.groups.cards(coordinator: SessionTarget(habID: "bench", session: "s1", pane: "%1")).map(\.group.id)), ["old", "run"],
                       "the coordinator's view keeps every group it made")
        XCTAssertTrue(f.groups.cards(coordinator: SessionTarget(habID: "bench", session: "s2", pane: "%2")).isEmpty)
        XCTAssertTrue(f.groups.cards(coordinator: SessionTarget(habID: "other", session: "s1", pane: nil)).isEmpty)
        let old = try XCTUnwrap(f.groups.all.first { $0.group.id == "old" })
        XCTAssertFalse(old.canPause || old.canResume || old.canClose, "an ended group offers no action")
        XCTAssertEqual(GroupText.elapsed(old.group, now: now), "1h 0m")
        await f.setActive(false)
    }

    func testTextLines() {
        var g = Self.group()
        XCTAssertEqual(GroupText.counts(g), "0 of 2 landed · 1 started", "a member not started is not counted")
        XCTAssertEqual(GroupText.memberState(g.members[0]), "working", "a running member shows its agent state")
        XCTAssertEqual(GroupText.memberState(g.members[1]), "failed")
        XCTAssertNil(GroupText.lane(g))
        g.lane = GroupLaneState(landing: "api", queue: ["ui"])
        XCTAssertEqual(GroupText.lane(g), "landing API · queue: UI")
        g.items[1].state = "dropped"
        g.items[0].state = "landed"
        XCTAssertEqual(GroupText.counts(g), "1 of 1 landed · 1 started", "a dropped item is not counted")
        g.pause = GroupPause(usage: "2026-11-07T10:00:00Z", user: "2026-11-07T10:00:00Z")
        XCTAssertEqual(GroupText.pause(g), "Paused by you and by usage")
        XCTAssertNil(GroupText.waits(g))
    }

    // MARK: - Members

    func testMemberOpensItsSessionAndTheBoardMarksIt() async throws {
        let h = await hab(member: true)
        await h.loadGroup(Self.group())
        let f = await fleet([h])
        await eventually("card") { f.groups.all.count == 1 }
        let card = try XCTUnwrap(f.groups.all.first)
        XCTAssertEqual(f.groups.target(card, card.group.members[0]), SessionTarget(habID: "bench", session: "s2", pane: "%2"))
        XCTAssertNil(f.groups.target(card, card.group.members[1]), "a member that never started has no session")
        XCTAssertFalse(f.groups.open(card, card.group.members[1]))
        XCTAssertTrue(f.groups.open(card, card.group.members[0]))
        XCTAssertEqual(f.groups.openRequest?.session, "s2")

        await eventually("member mark") { f.board.rows.first { $0.target.session == "s2" }?.group == "Split the parser" }
        XCTAssertNil(f.board.rows.first { $0.target.session == "s1" }?.group)
        await f.setActive(false)
    }

    /// A member on another hab opens there, found by its hab's device id.
    func testMemberOnAnotherHab() async throws {
        let home = await hab()
        let peer = await hab("beta", member: true)
        var g = Self.group()
        g.members[1].session = "s2"
        g.members[1].device = "SHA256:fake-beta"
        g.members[1].state = "running"
        await home.loadGroup(g)
        let f = await fleet([home, peer])
        await eventually("card") { f.groups.all.count == 1 }
        let card = try XCTUnwrap(f.groups.all.first)
        XCTAssertEqual(f.groups.target(card, card.group.members[1]), SessionTarget(habID: "beta", session: "s2", pane: "%2"))
        await eventually("mark across habs") {
            f.board.rows.first { $0.target.habID == "beta" && $0.target.session == "s2" }?.group == "Split the parser"
        }
        var lost = card
        lost.group.members[1].device = "SHA256:not-here"
        XCTAssertNil(f.groups.target(lost, lost.group.members[1]), "a hab the app lacks opens nothing")
        await f.setActive(false)
    }

    // MARK: - Actions

    func testPauseResumeRetryAndClose() async throws {
        let h = await hab()
        await h.loadGroup(Self.group())
        let f = await fleet([h])
        await eventually("card") { f.groups.all.count == 1 }
        var card = try XCTUnwrap(f.groups.all.first)
        XCTAssertTrue(card.canPause)
        XCTAssertFalse(card.canResume)
        XCTAssertTrue(card.canRetry(card.group.members[1]), "a failed member starts again")
        XCTAssertFalse(card.canRetry(card.group.members[0]))

        await f.groups.pause(card)
        await eventually("paused") { f.groups.all.first?.group.state == "paused" }
        card = try XCTUnwrap(f.groups.all.first)
        XCTAssertFalse(card.canPause)
        XCTAssertTrue(card.canResume)
        await f.groups.resume(card)
        await eventually("running") { f.groups.all.first?.group.state == "running" }

        // A refusal shows on the card.
        card = try XCTUnwrap(f.groups.all.first)
        await f.groups.retry(card, card.group.members[0])
        XCTAssertEqual(f.groups.errors[card.id], "member claude-3fa9c2-lead-1 is running")
        await f.groups.retry(card, card.group.members[1])
        XCTAssertNil(f.groups.errors[card.id])
        await eventually("planned") { f.groups.all.first?.group.members[1].state == "planned" }

        // Close asks first; the yes closes it.
        card = try XCTUnwrap(f.groups.all.first)
        f.groups.askClose(card)
        let calls = await h.groupCalls
        XCTAssertFalse(calls.contains { $0.hasPrefix("group.close") }, "nothing closes before the yes")
        await f.groups.confirmClose()?.value
        XCTAssertNil(f.groups.closeAsk)
        await eventually("ended") { f.groups.all.first?.group.state == "ended" }
        let all = await h.groupCalls
        XCTAssertEqual(all, ["group.pause g1", "group.resume g1", "group.retry g1 claude-3fa9c2-lead-1",
                             "group.retry g1 codex-3fa9c2-lead-2", "group.close g1"])
        await f.setActive(false)
    }

    func testOfflineCardOffersNoAction() {
        let c = GroupCard(habID: "a", habLabel: "a", group: Self.group(), online: false)
        XCTAssertFalse(c.canPause || c.canClose || c.canRetry(c.group.members[1]))
    }

    // MARK: - The plan card

    func testPlanDraftEdits() {
        let orig = Self.plan()
        var d = GroupPlanDraft(orig)
        XCTAssertNil(d.edits(against: orig))
        d.setReview("api", on: false)
        XCTAssertEqual(d.edits(against: orig)?.items[0].review, "off")
        d.setReview("api", on: true)
        XCTAssertNil(d.edits(against: orig), "review off and on again is no change")

        d.setMember(1, host: "  ", harness: "claude", model: "opus", name: " ui-lead ")
        XCTAssertNil(d.plan.members[1].host, "an empty host is the home hab")
        XCTAssertEqual(d.plan.members[1].harness, "claude")
        XCTAssertEqual(d.plan.members[1].name, "ui-lead")
        d.setMember(1, harness: "")
        XCTAssertEqual(d.plan.members[1].harness, "claude", "a harness is never cleared")

        // Dropping an item drops its members, and the item after it no longer stacks.
        d.dropItem("api")
        XCTAssertEqual(d.plan.items.map(\.id), ["ui"])
        XCTAssertNil(d.plan.items[0].after)
        XCTAssertEqual(d.plan.members.map(\.item), ["ui"])
        XCTAssertNil(d.problem)

        d.dropMember(0)
        XCTAssertEqual(d.problem, "UI has no lead: drop the item or keep its lead")
        d.dropItem("ui")
        XCTAssertEqual(d.problem, "Keep at least one item")
    }

    /// An amend's plan is only the change: members for items already in the group, no item.
    func testAmendDraft() {
        let change = GroupPlan(items: [], members: [GroupPlanMember(harness: "codex", item: "api", role: .worker),
                                                    GroupPlanMember(harness: "claude", item: "api", role: .worker)],
                               project: "tide-tables", title: "Split the parser")
        var d = GroupPlanDraft(change, amend: true)
        XCTAssertNil(d.problem, "a change that only adds members goes")
        d.setMember(1, model: "opus")
        XCTAssertNil(d.problem)
        XCTAssertEqual(d.memberIDs, [0, 1])
        d.dropMember(0)
        XCTAssertEqual(d.memberIDs, [1], "a row keeps its id when one above it goes")
        XCTAssertEqual(d.plan.members.map(\.model), ["opus"])
        d.dropMember(0)
        XCTAssertEqual(d.problem, "Keep at least one item or member")
        var plan = Self.plan()
        plan.items[1].after = nil
        var full = GroupPlanDraft(plan)
        full.dropItem("api")
        XCTAssertEqual(full.memberIDs, [1], "an item's members leave with their ids")
    }

    func testAmendCardEditsGoOverTheHab() async throws {
        let h = await hab()
        await h.setCapability("confirm", true)
        let change = GroupPlan(items: [], members: [GroupPlanMember(harness: "codex", item: "api", role: .worker)],
                               project: "tide-tables", title: "Split the parser")
        await h.requestConfirmation(Confirmation(created: Fixture.stamp(Date()), expiresAt: Fixture.stamp(Date().addingTimeInterval(3600)),
                                                 group: ConfirmGroup(amend: true, group: "g1", plan: change, trust: ""),
                                                 id: "amend1", kind: .group, state: .open))
        let f = await fleet([h])
        let confirms = try XCTUnwrap(f.confirms)
        await eventually("amend card") { confirms.cards.contains { $0.confirmation.id == "amend1" } }
        let card = try XCTUnwrap(confirms.cards.first { $0.confirmation.id == "amend1" })
        var d = try XCTUnwrap(confirms.groupDraft(card))
        XCTAssertTrue(d.amend)
        d.setMember(0, model: "gpt-5")
        confirms.setGroupDraft(d, for: card)
        await confirms.confirm(card)
        let answers = await h.confirmAnswers
        XCTAssertEqual(answers.last?.edits?.plan?.members.first?.model, "gpt-5")
        await f.setActive(false)
    }

    /// Two members of one name on two habs are two rows.
    func testMemberRowIDs() {
        let a = GroupMember(device: "A", harness: "claude", host: "a", item: "api", name: "w", role: .worker, state: "running")
        var b = a
        b.device = "B"
        XCTAssertNotEqual(a.rowID, b.rowID)
    }

    /// Retry takes a planned member whose setup waited, as the hab does.
    func testRetryAWaitingMember() async throws {
        let h = await hab()
        var g = Self.group()
        g.members[1].state = "planned"
        g.members[1].reason = nil
        await h.loadGroup(g)
        let f = await fleet([h])
        await eventually("card") { f.groups.all.count == 1 }
        let card = try XCTUnwrap(f.groups.all.first)
        XCTAssertTrue(card.canRetry(card.group.members[1]), "the UI item waits with a reason")
        await f.groups.retry(card, card.group.members[1])
        XCTAssertNil(f.groups.errors[card.id])
        await f.setActive(false)
    }

    func testPlanCardOverTheHab() async throws {
        let h = await hab()
        await h.setCapability("confirm", true)
        let c = Confirmation(created: Fixture.stamp(Date()), expiresAt: Fixture.stamp(Date().addingTimeInterval(3600)), from: "coord",
                             group: ConfirmGroup(group: "g1", plan: Self.plan(), trust: "Trust for the new worktrees, for claude."),
                             id: "plan1", kind: .group, state: .open)
        await h.requestConfirmation(c)
        let f = await fleet([h])
        let confirms = try XCTUnwrap(f.confirms)
        await eventually("plan card") { confirms.cards.contains { $0.confirmation.id == "plan1" } }
        let card = try XCTUnwrap(confirms.cards.first { $0.confirmation.id == "plan1" })
        XCTAssertNil(confirms.draft(card), "a plan card has no start draft")
        var d = try XCTUnwrap(confirms.groupDraft(card))
        XCTAssertFalse(confirms.isEdited(card))

        // A draft with a problem does not go.
        d.dropMember(0)
        confirms.setGroupDraft(d, for: card)
        XCTAssertTrue(confirms.isEdited(card))
        await confirms.confirm(card)
        XCTAssertEqual(confirms.phase(card), .failed("API has no lead: drop the item or keep its lead"))
        var answers = await h.confirmAnswers
        XCTAssertTrue(answers.isEmpty)

        // Drop the UI item and turn review off for API: the whole edited plan goes.
        d = GroupPlanDraft(Self.plan())
        d.dropItem("ui")
        d.setReview("api", on: false)
        confirms.setGroupDraft(d, for: card)
        await confirms.confirm(card)
        answers = await h.confirmAnswers
        XCTAssertEqual(answers.last?.choice, .confirm)
        XCTAssertEqual(answers.last?.edits?.plan, d.plan)
        XCTAssertNil(answers.last?.edits?.host)
        await eventually("closed") { confirms.cards.first { $0.confirmation.id == "plan1" }?.confirmation.state == .done }
        let done = try XCTUnwrap(confirms.cards.first { $0.confirmation.id == "plan1" })
        XCTAssertEqual(ConfirmCenter.outcome(done.confirmation), "Started group Split the parser on bench")
        await f.setActive(false)
    }
}
