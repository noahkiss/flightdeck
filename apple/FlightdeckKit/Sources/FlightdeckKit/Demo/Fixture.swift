import Foundation
import HabAPI

/// Builds hab objects for the fake hab, tests and demo mode. Every name it is
/// given must be invented: no real hosts, users, paths or projects.
public enum Fixture {
    public static func project(_ id: String, name: String, sessions: [String] = [], branch: String? = "main") -> Project {
        Project(branch: branch, id: id, name: name, remote: "https://example.com/demo/\(name).git", revision: 1, sessions: sessions)
    }

    public static func session(_ id: String, name: String, pane: String?, project: String?, model: String? = nil, title: String? = nil, contextPct: Int? = nil, exited: Bool = false) -> Session {
        Session(contextPct: contextPct, exit: exited ? ExitStatus(code: 0) : nil, id: id, model: model, name: name, pane: pane, project: project, revision: 1, state: exited ? .exited : .running, title: title)
    }

    public static func pane(_ id: String, session: String?) -> Pane {
        Pane(cols: 120, holderBuild: "fake", id: id, offset: 0, revision: 1, rows: 40, session: session)
    }

    public static func agent(pane: String, harness: String, state: AgentStatus, kind: BlockedKind? = nil, request: PermissionRequest? = nil, at: Date = Date(), revision: UInt64 = 1) -> AgentState {
        AgentState(at: Self.stamp(at), harness: harness, kind: kind, pane: pane, request: request, revision: revision, source: .hook, state: state)
    }

    public static func request(title: String, tool: String, command: String? = nil) -> PermissionRequest {
        PermissionRequest(command: command, title: title, tool: tool)
    }

    /// An RFC 3339 stamp, as the hab writes `AgentState.at`.
    public static func stamp(_ d: Date) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.string(from: d)
    }
}
