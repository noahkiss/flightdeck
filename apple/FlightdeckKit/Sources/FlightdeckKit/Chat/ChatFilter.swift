import Foundation
import HabAPI
import Observation

/// A kind of chat content the owner can hide ("Show in chat", app.md 4.11). Replies,
/// questions, approvals and errors have no kind: they always show.
public enum ChatContentKind: String, CaseIterable, Codable, Sendable, Identifiable {
    case plans
    case connectors
    case agents
    case commands
    case edits
    case reads
    case thinking

    public var id: String { rawValue }

    /// The switch's title.
    public var title: String {
        switch self {
        case .plans: return "Plans and progress"
        case .connectors: return "MCP and connector calls"
        case .agents: return "Messages from other agents"
        case .commands: return "Commands"
        case .edits: return "Edits and diffs"
        case .reads: return "File reads and searches"
        case .thinking: return "Thinking"
        }
    }

    /// The switch's second line, where one helps.
    public var subtitle: String? {
        switch self {
        case .connectors: return "Linear, GitHub, your own servers"
        case .agents: return "Channels and subagents"
        case .commands: return "Shell commands and other tools"
        case .thinking: return "No harness sends it to Flightdeck yet"
        default: return nil
        }
    }

    public var symbol: String {
        switch self {
        case .plans: return "checklist"
        case .connectors: return "point.3.connected.trianglepath.dotted"
        case .agents: return "person.2"
        case .commands: return "terminal"
        case .edits: return "pencil"
        case .reads: return "doc.text.magnifyingglass"
        case .thinking: return "brain"
        }
    }

    /// The fold line's noun: "3 reads", "1 command".
    func count(_ n: Int) -> String {
        let (one, many): (String, String)
        switch self {
        case .plans: (one, many) = ("plan update", "plan updates")
        case .connectors: (one, many) = ("connector call", "connector calls")
        case .agents: (one, many) = ("agent message", "agent messages")
        case .commands: (one, many) = ("command", "commands")
        case .edits: (one, many) = ("edit", "edits")
        case .reads: (one, many) = ("read", "reads")
        case .thinking: (one, many) = ("thought", "thoughts")
        }
        return "\(n) \(n == 1 ? one : many)"
    }
}

/// Which kind each transcript item is (app.md 4.11, the mapping table).
public enum ChatKindMap {
    /// What a tool does, for its sentence and its kind.
    enum Family {
        case edit, write, read, view, grep, glob, list, shell, fetch, webSearch, plan, subagent, imageGen, mcp, other
    }

    static let families: [String: Family] = {
        var m: [String: Family] = [:]
        for n in ["Edit", "MultiEdit", "NotebookEdit", "edit", "patch", "multiedit", "apply_patch"] { m[n] = .edit }
        for n in ["Write", "write"] { m[n] = .write }
        for n in ["Read", "read", "NotebookRead"] { m[n] = .read }
        for n in ["view_image"] { m[n] = .view }
        for n in ["Grep", "grep", "codesearch"] { m[n] = .grep }
        for n in ["Glob", "glob"] { m[n] = .glob }
        for n in ["LS", "list"] { m[n] = .list }
        for n in ["Bash", "BashOutput", "KillShell", "KillBash", "bash", "shell", "exec", "exec_command", "local_shell"] { m[n] = .shell }
        for n in ["WebFetch", "webfetch"] { m[n] = .fetch }
        for n in ["WebSearch", "websearch", "web_search"] { m[n] = .webSearch }
        for n in ["TodoWrite", "TodoRead", "todowrite", "todoread", "update_plan", "ExitPlanMode",
                  "TaskCreate", "TaskUpdate", "TaskList", "TaskGet"] { m[n] = .plan }
        for n in ["Task", "Agent", "task"] { m[n] = .subagent }
        for n in ["image_gen"] { m[n] = .imageGen }
        for n in ["ListMcpResourcesTool", "ReadMcpResourceTool"] { m[n] = .mcp }
        return m
    }()

    static func family(_ name: String) -> Family {
        if let f = families[name] { return f }
        if name.hasPrefix("mcp__") { return .mcp }
        if name.hasPrefix("agent_") { return .subagent }
        return .other
    }

    /// A tool's kind by its name. Any tool not in the table is a command (the fallback kind).
    public static func kind(tool name: String) -> ChatContentKind {
        switch family(name) {
        case .edit, .write, .imageGen: return .edits
        case .read, .view, .grep, .glob, .list, .fetch, .webSearch: return .reads
        case .shell, .other: return .commands
        case .plan: return .plans
        case .subagent: return .agents
        case .mcp: return .connectors
        }
    }

    /// A row's kind; nil for a row that always shows: the owner's messages, replies, events,
    /// Capcom's cards, unsupported rows, and a tool call that failed or was denied.
    public static func kind(_ item: ChatItem) -> ChatContentKind? {
        switch item {
        case .user(_, _, let sender, _):
            return sender == nil ? nil : .agents
        case .agent(_, _, let sender):
            return sender == nil ? nil : .agents
        case .tool(_, let card):
            if card.status == .error || card.status == .denied { return nil }
            return card.kind
        case .plan:
            return .plans
        case .capcomUser, .event, .handOff, .proposal, .unsupported:
            return nil
        }
    }
}

/// One row the chat list shows: a chat item, or hidden items folded into one line.
public enum ChatRow: Hashable, Sendable, Identifiable {
    case item(ChatItem)
    case folded(id: String, items: [ChatItem], summary: String)

    public var id: String {
        switch self {
        case .item(let i): return i.id
        case .folded(let id, _, _): return id
        }
    }
}

/// Folds hidden rows (app.md 4.11): rows that follow each other and are hidden become one line.
public enum ChatFold {
    public static func rows(_ items: [ChatItem], hidden: Set<ChatContentKind>) -> [ChatRow] {
        guard !hidden.isEmpty else { return items.map(ChatRow.item) }
        var out: [ChatRow] = []
        var run: [ChatItem] = []
        func flush() {
            guard let first = run.first else { return }
            out.append(.folded(id: "fold-" + first.id, items: run, summary: summary(run)))
            run = []
        }
        for i in items {
            if let k = ChatKindMap.kind(i), hidden.contains(k) {
                run.append(i)
            } else {
                flush()
                out.append(.item(i))
            }
        }
        flush()
        return out
    }

    /// "6 steps hidden: 3 reads, 2 edits, 1 command": largest count first, ties in the sheet's order.
    public static func summary(_ items: [ChatItem]) -> String {
        var counts: [ChatContentKind: Int] = [:]
        for i in items {
            if let k = ChatKindMap.kind(i) { counts[k, default: 0] += 1 }
        }
        let order = ChatContentKind.allCases
        let parts = counts.sorted { a, b in
            a.value != b.value ? a.value > b.value : order.firstIndex(of: a.key)! < order.firstIndex(of: b.key)!
        }.map { $0.key.count($0.value) }
        let n = items.count
        return "\(n) \(n == 1 ? "step" : "steps") hidden: " + parts.joined(separator: ", ")
    }
}

/// What the owner chose to show (app.md 4.11): the kinds hidden in every session, and
/// per-session overrides keyed by hab and session id.
public struct ChatShowSettings: Codable, Equatable, Sendable {
    public var hidden: Set<ChatContentKind> = []
    public var sessions: [String: Set<ChatContentKind>] = [:]

    public init(hidden: Set<ChatContentKind> = [], sessions: [String: Set<ChatContentKind>] = [:]) {
        self.hidden = hidden
        self.sessions = sessions
    }

    /// Reads tolerate a kind this build does not know: it is dropped.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        func kinds(_ raw: [String]) -> Set<ChatContentKind> { Set(raw.compactMap(ChatContentKind.init(rawValue:))) }
        hidden = kinds((try? c.decode([String].self, forKey: .hidden)) ?? [])
        let s = (try? c.decode([String: [String]].self, forKey: .sessions)) ?? [:]
        sessions = s.mapValues(kinds)
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(hidden.map(\.rawValue).sorted(), forKey: .hidden)
        try c.encode(sessions.mapValues { $0.map(\.rawValue).sorted() }, forKey: .sessions)
    }

    enum CodingKeys: String, CodingKey { case hidden, sessions }
}

/// The scope of the Show in chat switches.
public enum ChatShowScope: String, CaseIterable, Identifiable, Sendable {
    case every
    case session
    public var id: String { rawValue }
    public var title: String { self == .every ? "Every session" : "Only this one" }
}

/// The device's Show in chat choices, kept in `UserDefaults` (app.md 4.11).
@MainActor
@Observable
public final class ChatShowStore {
    public private(set) var settings: ChatShowSettings
    @ObservationIgnored private let defaults: UserDefaults

    public static let key = "flightdeck.chat.show"

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        settings = Self.read(defaults)
    }

    static func read(_ d: UserDefaults) -> ChatShowSettings {
        // A JSON string (a launch argument can set it) or data.
        if let s = d.string(forKey: key), let data = s.data(using: .utf8),
           let v = try? JSONDecoder().decode(ChatShowSettings.self, from: data) { return v }
        if let data = d.data(forKey: key), let v = try? JSONDecoder().decode(ChatShowSettings.self, from: data) { return v }
        // A launch argument in property list form: `-flightdeck.chat.show '{hidden=(reads);}'`.
        if let dict = d.dictionary(forKey: key), JSONSerialization.isValidJSONObject(dict),
           let data = try? JSONSerialization.data(withJSONObject: dict),
           let v = try? JSONDecoder().decode(ChatShowSettings.self, from: data) { return v }
        return ChatShowSettings()
    }

    private func save() {
        guard let data = try? JSONEncoder().encode(settings), let s = String(data: data, encoding: .utf8) else { return }
        defaults.set(s, forKey: Self.key)
    }

    /// The key of one session: its hab and its session id (the pane without one).
    public nonisolated static func sessionKey(_ t: SessionTarget) -> String {
        "\(t.habID)/\(t.session ?? t.pane ?? "")"
    }

    public func hidden(for t: SessionTarget) -> Set<ChatContentKind> {
        settings.sessions[Self.sessionKey(t)] ?? settings.hidden
    }

    public func scope(for t: SessionTarget) -> ChatShowScope {
        settings.sessions[Self.sessionKey(t)] == nil ? .every : .session
    }

    /// Shows or hides a kind in the given scope.
    public func set(_ kind: ChatContentKind, shown: Bool, scope: ChatShowScope, for t: SessionTarget) {
        var set = scope == .every ? settings.hidden : hidden(for: t)
        if shown { set.remove(kind) } else { set.insert(kind) }
        if scope == .every {
            settings.hidden = set
        } else {
            settings.sessions[Self.sessionKey(t)] = set
        }
        save()
    }

    /// "Only this one" starts from what the session shows now; "Every session" drops its override.
    public func setScope(_ scope: ChatShowScope, for t: SessionTarget) {
        let k = Self.sessionKey(t)
        switch scope {
        case .every: settings.sessions[k] = nil
        case .session: if settings.sessions[k] == nil { settings.sessions[k] = settings.hidden }
        }
        save()
    }

    /// Drops the overrides of sessions an online hab no longer lists. `sessions` is every
    /// session id the hab lists now.
    public func prune(habID: String, sessions: Set<String>, panes: Set<String> = []) {
        let prefix = habID + "/"
        let gone = settings.sessions.keys.filter { k in
            guard k.hasPrefix(prefix) else { return false }
            let id = String(k.dropFirst(prefix.count))
            return !sessions.contains(id) && !panes.contains(id)
        }
        guard !gone.isEmpty else { return }
        for k in gone { settings.sessions[k] = nil }
        save()
    }
}
