import Foundation
import HabAPI

/// One request that waits for the owner, as the approval sheet shows it
/// (app.md 7.2): a session in `blocked` on an online or offline hab.
public struct OpenRequest: Identifiable, Equatable, Sendable {
    public var habID: String
    public var habLabel: String
    /// The hab's host id, as pushes name it.
    public var host: String?
    public var pane: String
    public var session: String?
    public var sessionName: String
    public var project: String?
    public var model: String?
    public var harness: String
    public var contextPct: Int?
    public var kind: BlockedKind
    public var request: PermissionRequest?
    /// When the agent entered `blocked`, by the hab's clock.
    public var since: Date?
    public var offline: Bool

    public var id: String { "\(habID)/\(pane)/\(request?.id ?? kind.rawValue)" }
    public var requestID: String? { request?.id.flatMap { $0.isEmpty ? nil : $0 } }
    public var expiry: Date? { request?.expiresAt.flatMap(BoardRows.parseStamp) }

    public var target: SessionTarget { SessionTarget(habID: habID, session: session, pane: pane) }

    /// Whether Allow and Deny can answer it: a permission with a request id,
    /// from a structured source (harness-adapters.md Rule 2: never from
    /// screen text, which carries no id).
    public var answerable: Bool { kind == .permission && requestID != nil }

    /// What the request block shows.
    public var form: RequestForm { RequestForm(request) }

    /// The sheet's title (app.md 7.2): the harness's own title, else one built
    /// from the tool. The app never asks a model for it.
    public var title: String {
        switch kind {
        case .question: return "\(Self.harnessName(harness)) asks a question"
        case .auth: return "Sign-in needed"
        default: break
        }
        guard let r = request else { return "Approval needed" }
        switch form {
        case .plan: return "Approve the plan?"
        case .questions(let qs): return qs.count == 1 ? qs[0].question : "Answer \(qs.count) questions?"
        default: break
        }
        if let t = r.title, !t.isEmpty { return t }
        if r.command != nil { return "Run a command?" }
        if let f = r.files, !f.isEmpty { return f.count == 1 ? "Edit \((f[0] as NSString).lastPathComponent)?" : "Edit \(f.count) files?" }
        if let tool = r.tool { return "Allow \(tool)?" }
        return "Approval needed"
    }

    /// The header's request kind, in plain words.
    public var kindText: String {
        switch kind {
        case .permission: return form.isPlan ? "plan" : "permission"
        case .question: return "question"
        case .auth: return "sign-in"
        default: return kind.rawValue
        }
    }

    static func harnessName(_ id: String) -> String {
        ["claude": "Claude Code", "codex": "Codex", "opencode": "opencode", "pi": "pi"][id] ?? id
    }

    /// Every open request on the board, oldest first (app.md 7.8: a burst
    /// shows the oldest first).
    @MainActor
    public static func collect(_ habs: [Board.Hab]) -> [OpenRequest] {
        var out: [OpenRequest] = []
        for h in habs {
            if case .updateNeeded = h.state.status { continue }
            let store = h.state.store
            for a in store.agents.values where a.state == .blocked {
                // AgentState.session is the harness's own id; the hab's session holds the pane.
                let s = store.sessions.values.first { $0.pane == a.pane }
                out.append(OpenRequest(
                    habID: h.id, habLabel: h.label, host: h.state.welcome?.host.id ?? store.host,
                    pane: a.pane, session: s?.id, sessionName: s?.title ?? s?.name ?? a.pane,
                    project: s?.project.flatMap { store.projects[$0]?.name }, model: s?.model,
                    harness: a.harness, contextPct: s?.contextPct, kind: a.kind ?? .permission,
                    request: a.request, since: BoardRows.parseStamp(a.at), offline: h.state.status != .online
                ))
            }
        }
        return out.sorted { a, b in
            switch (a.since, b.since) {
            case let (x?, y?) where x != y: return x < y
            default: return a.id < b.id
            }
        }
    }
}

/// The request block of the sheet: a command, files, a plan, questions with
/// options, or the raw input of an unknown tool.
public enum RequestForm: Equatable, Sendable {
    case none
    case command(String)
    case files([String])
    /// Claude's ExitPlanMode: the plan to approve (landscape.md 4.4).
    case plan(String)
    /// Claude's AskUserQuestion: questions with their options (landscape.md 4.4).
    case questions([Question])
    case raw(String)

    public struct Question: Equatable, Sendable {
        public var header: String?
        public var question: String
        public var options: [Option]
        public var multiSelect: Bool
    }

    public struct Option: Equatable, Sendable {
        public var label: String
        public var description: String?
    }

    public var isPlan: Bool { if case .plan = self { return true } else { return false } }

    init(_ r: PermissionRequest?) {
        guard let r else { self = .none; return }
        if r.tool == "ExitPlanMode", case .object(let o)? = r.input, case .string(let plan)? = o["plan"] {
            self = .plan(plan)
            return
        }
        if r.tool == "AskUserQuestion", case .object(let o)? = r.input, case .array(let qs)? = o["questions"] {
            let parsed = qs.compactMap(Self.question)
            if !parsed.isEmpty, parsed.count == qs.count {
                self = .questions(parsed)
                return
            }
        }
        if let c = r.command, !c.isEmpty { self = .command(c); return }
        if let f = r.files, !f.isEmpty { self = .files(f); return }
        if let i = r.input {
            let enc = JSONEncoder()
            enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
            if let d = try? enc.encode(i), let s = String(data: d, encoding: .utf8) {
                self = .raw(s)
                return
            }
        }
        self = .none
    }

    private static func question(_ v: JSONValue) -> Question? {
        guard case .object(let q) = v, case .string(let text)? = q["question"], case .array(let opts)? = q["options"] else { return nil }
        var options: [Option] = []
        for o in opts {
            guard case .object(let m) = o, case .string(let label)? = m["label"] else { return nil }
            var desc: String?
            if case .string(let d)? = m["description"] { desc = d }
            options.append(Option(label: label, description: desc))
        }
        var header: String?
        if case .string(let h)? = q["header"] { header = h }
        var multi = false
        if case .bool(let b)? = q["multiSelect"] { multi = b }
        return Question(header: header, question: text, options: options, multiSelect: multi)
    }

    /// A plan as styled text: its Markdown when it parses, else the plain text.
    /// Never a localization key, so `%@` or a key-like line shows as written.
    public static func planText(_ plan: String) -> AttributedString {
        let opts = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        return (try? AttributedString(markdown: plan, options: opts)) ?? AttributedString(plan)
    }

    /// The `text` of an `answer` for chosen option labels, one list per
    /// question in order. One question: the labels joined with ", ". Several:
    /// one line per question, "<header or question>: <labels>".
    public static func answerText(_ questions: [Question], _ picks: [[String]]) -> String {
        if questions.count == 1 { return (picks.first ?? []).joined(separator: ", ") }
        return zip(questions, picks).map { q, p in "\(q.header ?? q.question): \(p.joined(separator: ", "))" }.joined(separator: "\n")
    }
}
