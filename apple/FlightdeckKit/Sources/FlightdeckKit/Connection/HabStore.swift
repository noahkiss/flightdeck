import Foundation
import HabAPI

/// One hab's objects, as its snapshot and events build them
/// (hab-logic-api.md 3.5).
///
/// - `reset` replaces every object with a snapshot's, whatever revisions were held.
/// - `apply` ignores an event whose revision is not higher than the one held.
///   Ephemeral events carry no revision and change nothing kept here.
public struct HabStore: Sendable, Equatable {
    public private(set) var host: String?
    public private(set) var projects: [String: Project] = [:]
    public private(set) var sessions: [String: Session] = [:]
    public private(set) var panes: [String: Pane] = [:]
    /// Agent state by pane id.
    public private(set) var agents: [String: AgentState] = [:]
    private var revs: [String: UInt64] = [:]

    public init() {}

    public init(_ snapshot: StructureResult) {
        reset(snapshot)
    }

    /// Replaces every object with the snapshot's.
    public mutating func reset(_ s: StructureResult) {
        self = HabStore()
        host = s.host.id
        for p in s.projects {
            projects[p.id] = p
            revs["project/" + p.id] = p.revision
        }
        for x in s.sessions {
            sessions[x.id] = x
            revs["session/" + x.id] = x.revision
        }
        for p in s.panes {
            panes[p.id] = p
            revs["pane/" + p.id] = p.revision
        }
        for a in s.agents ?? [] {
            agents[a.pane] = a
            revs["agent/" + a.pane] = a.revision
        }
    }

    /// Merges one event. Returns whether anything changed.
    @discardableResult
    public mutating func apply(_ e: Event) -> Bool {
        guard let kind = Self.kind(of: e.type) else { return false }
        let key = kind + "/" + e.id
        var rev = e.revision
        let decoded: HabEvent
        do {
            decoded = try HabEvent(e)
        } catch {
            return false
        }
        if case .agentState(let a) = decoded, rev == nil {
            // agent.state carries its per-pane revision in the data.
            rev = a.revision
        }
        if let rev, let held = revs[key], rev <= held {
            return false
        }
        switch decoded {
        case .projectUpdated(let p): projects[e.id] = p
        case .sessionUpdated(let s): sessions[e.id] = s
        case .paneUpdated(let p): panes[e.id] = p
        case .paneOwner(let o):
            guard var p = panes[e.id] else { return false }
            p.owner = o.owner
            p.cols = o.cols
            p.rows = o.rows
            if let rev { p.revision = rev }
            panes[e.id] = p
        case .agentState(let a): agents[e.id] = a
        case .projectDeleted: projects[e.id] = nil
        case .sessionDeleted: sessions[e.id] = nil
        case .paneDeleted:
            panes[e.id] = nil
            agents[e.id] = nil
        default: return false
        }
        if let rev { revs[key] = rev }
        return true
    }

    /// The object kind an event changes, for the events this store keeps.
    static func kind(of type: String) -> String? {
        switch type {
        case "project.updated", "project.deleted": return "project"
        case "session.updated", "session.deleted": return "session"
        case "pane.updated", "pane.deleted", "pane.owner": return "pane"
        case "agent.state": return "agent"
        default: return nil
        }
    }
}
