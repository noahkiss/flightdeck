import Foundation
import HabAPI

/// What a round or group confirmation card shows (capcom.md 6.3, groups.md 3.2): its title,
/// its facts as label and value rows, and its long texts. The view draws it; the tests read it.
public enum ConfirmCardContent {
    /// One row of a card: a label and its value.
    public struct Fact: Hashable, Sendable {
        public var label: String
        public var value: String
        public var mono: Bool

        public init(_ label: String, _ value: String, mono: Bool = false) {
            self.label = label
            self.value = value
            self.mono = mono
        }
    }

    /// One plan item of a group card, with the members that work on it.
    public struct Item: Hashable, Sendable {
        public var id: String
        /// The item's title; empty for an item an amend's member joins but the plan does not list.
        public var title: String
        /// "After build-api", "Review off", "Touches api/, docs/".
        public var notes: [String]
        /// "lead · claude-3fa9c2-lead-1 · boxb · Claude Code, opus".
        public var members: [String]
    }

    /// The card's title: a question for an open card, a noun for a record.
    public static func title(_ c: Confirmation) -> String {
        let record = c.record == true
        switch c.kind {
        case .setup: return record ? "Project set-up" : "Set up a project?"
        case .round: return record ? "Round" : "Make a round?"
        case .group:
            if c.group?.amend == true { return record ? "Group change" : "Change the group?" }
            return record ? "Group start" : "Start a group?"
        default: return record ? "Session start" : "Start a session?"
        }
    }

    // MARK: - Round

    /// A round card's rows (capcom.md 6.3, row Round on another host).
    public static func roundFacts(_ r: ConfirmRound) -> [Fact] {
        let s = r.round
        var out: [Fact] = []
        add(&out, "Runner", r.host)
        add(&out, "Name", s.name)
        add(&out, "Mode", s.mode.rawValue)
        add(&out, "Trigger", trigger(r))
        if let p = nonEmpty(r.project) ?? nonEmpty(s.projectRemote) {
            add(&out, "Project", p, mono: r.project?.isEmpty != false)
        } else if let d = nonEmpty(s.projectDir) ?? nonEmpty(s.cwd) {
            add(&out, "Folder", d, mono: true)
        }
        add(&out, "Harness", s.harness.map { harnessName($0) })
        add(&out, "Model", s.model)
        if let cmd = s.command, !cmd.isEmpty {
            add(&out, "Command", cmd.joined(separator: " "), mono: true)
        }
        if r.runNow == true { add(&out, "Run now", "Yes") }
        return out
    }

    /// The trigger in plain words: the hab's, else the app's own wording of the spec.
    static func trigger(_ r: ConfirmRound) -> String {
        if let t = nonEmpty(r.trigger) { return t }
        let t = r.round.trigger
        if let e = nonEmpty(t.every) {
            return nonEmpty(t.zone).map { "every \(e) in \($0)" } ?? "every \(e)"
        }
        if t.kind == .event, let e = nonEmpty(t.event) { return "on \(e)" }
        return RoundText.trigger(t)
    }

    // MARK: - Group

    /// A group card's top rows: the task, the project, the lane.
    public static func groupFacts(_ g: ConfirmGroup) -> [Fact] {
        var out: [Fact] = []
        add(&out, "Task", g.plan.title)
        add(&out, "Project", g.plan.project)
        add(&out, "Target branch", g.plan.lane?.target)
        if g.plan.lane?.fixInPlace == true { add(&out, "Fix in place", "Yes") }
        return out
    }

    /// The plan's items in plan order, each with its members. A member whose item the plan
    /// does not list (an amend that adds a member to a running item) gets a row of its own,
    /// after the listed ones. `home` names the host of a member with no host.
    public static func groupItems(_ g: ConfirmGroup, home: String) -> [Item] {
        var out = g.plan.items.map { i -> Item in
            var notes: [String] = []
            if let a = nonEmpty(i.after) { notes.append("After \(a)") }
            if i.review == "off" { notes.append("Review off") }
            if let t = i.touches, !t.isEmpty { notes.append("Touches \(t.joined(separator: ", "))") }
            return Item(id: i.id, title: i.title, notes: notes, members: [])
        }
        for m in g.plan.members {
            let line = member(m, home: home)
            if let k = out.firstIndex(where: { $0.id == m.item }) {
                out[k].members.append(line)
            } else {
                out.append(Item(id: m.item, title: "", notes: [], members: [line]))
            }
        }
        return out
    }

    /// "lead · claude-3fa9c2-lead-1 · boxb · Claude Code, opus"; no name when the hab picks it.
    static func member(_ m: GroupPlanMember, home: String) -> String {
        var parts = [m.role.rawValue]
        if let n = nonEmpty(m.name) { parts.append(n) }
        parts.append(nonEmpty(m.host) ?? home)
        let h = harnessName(m.harness)
        parts.append(nonEmpty(m.model).map { "\(h), \($0)" } ?? h)
        return parts.joined(separator: " · ")
    }

    // MARK: - Helpers

    private static func add(_ out: inout [Fact], _ label: String, _ value: String?, mono: Bool = false) {
        if let v = nonEmpty(value) { out.append(Fact(label, v, mono: mono)) }
    }

    private static func nonEmpty(_ s: String?) -> String? {
        guard let s, !s.isEmpty else { return nil }
        return s
    }
}
