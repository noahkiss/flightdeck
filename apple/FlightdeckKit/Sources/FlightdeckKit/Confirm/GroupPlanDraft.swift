import Foundation
import HabAPI

/// The owner's changes on a group's plan card (groups.md 3.2): hosts, harnesses, models and
/// names; an item or a member dropped; review turned off for an item. The hab takes the whole
/// edited plan and checks it again (3.1); this only keeps the edits the card allows.
public struct GroupPlanDraft: Equatable, Sendable {
    public private(set) var plan: GroupPlan

    public init(_ plan: GroupPlan) { self.plan = plan }

    /// Sets one member's field. An empty host means the home hab; an empty model or name lets
    /// the hab choose.
    public mutating func setMember(_ i: Int, host: String? = nil, harness: String? = nil, model: String? = nil, name: String? = nil) {
        guard plan.members.indices.contains(i) else { return }
        func clean(_ s: String) -> String? {
            let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
            return t.isEmpty ? nil : t
        }
        if let host { plan.members[i].host = clean(host) }
        if let harness, let h = clean(harness) { plan.members[i].harness = h }
        if let model { plan.members[i].model = clean(model) }
        if let name { plan.members[i].name = clean(name) }
    }

    public mutating func dropMember(_ i: Int) {
        guard plan.members.indices.contains(i) else { return }
        plan.members.remove(at: i)
    }

    /// Drops an item and its members. An item that stacked on it no longer stacks.
    public mutating func dropItem(_ id: String) {
        plan.items.removeAll { $0.id == id }
        plan.members.removeAll { $0.item == id }
        for i in plan.items.indices where plan.items[i].after == id { plan.items[i].after = nil }
    }

    /// Review is on unless the plan says off (groups.md 6.1). The card only turns it off, or
    /// back on after turning it off.
    public mutating func setReview(_ id: String, on: Bool) {
        guard let i = plan.items.firstIndex(where: { $0.id == id }) else { return }
        plan.items[i].review = on ? "on" : "off"
    }

    public static func reviewOn(_ it: GroupPlanItem) -> Bool { it.review != "off" }

    /// The edited plan, as `confirm.answer` takes it; nil when nothing changed. A review
    /// turned off and on again compares by its meaning, not its word.
    public func edits(against orig: GroupPlan) -> GroupPlan? {
        func norm(_ p: GroupPlan) -> GroupPlan {
            var p = p
            for i in p.items.indices { p.items[i].review = Self.reviewOn(p.items[i]) ? nil : "off" }
            return p
        }
        return norm(plan) == norm(orig) ? nil : plan
    }

    /// Why Confirm is off for this draft; nil when it can go. The hab checks the rest.
    public var problem: String? {
        if plan.items.isEmpty { return "Keep at least one item" }
        for it in plan.items {
            let leads = plan.members.filter { $0.item == it.id && $0.role == .lead }.count
            if leads == 0 { return "\(it.title) has no lead: drop the item or keep its lead" }
        }
        return nil
    }
}
