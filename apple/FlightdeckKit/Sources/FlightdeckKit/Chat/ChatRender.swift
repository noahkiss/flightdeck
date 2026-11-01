import Foundation
import HabAPI

/// One block of an agent's Markdown, split by lines. Inline Markdown
/// (emphasis, code spans, links) stays in `text`; SwiftUI renders it with
/// `AttributedString(markdown:)`.
public enum MarkdownBlock: Hashable, Sendable {
    case paragraph(String)
    case heading(level: Int, text: String)
    case code(language: String?, text: String)
    /// A list item: its marker ("•" or "1.") and indent level.
    case item(marker: String, level: Int, text: String)
    case quote(String)
    case rule

    /// Splits Markdown into blocks. Unclosed code fences run to the end.
    public static func parse(_ md: String) -> [MarkdownBlock] {
        var out: [MarkdownBlock] = []
        var para: [String] = []
        var code: [String]?
        var lang: String?
        func flush() {
            if !para.isEmpty {
                out.append(.paragraph(para.joined(separator: "\n")))
                para = []
            }
        }
        for raw in md.components(separatedBy: "\n") {
            let line = raw.hasSuffix("\r") ? String(raw.dropLast()) : raw
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if code != nil {
                if trimmed.hasPrefix("```") || trimmed.hasPrefix("~~~") {
                    out.append(.code(language: lang, text: code!.joined(separator: "\n")))
                    code = nil
                } else {
                    code!.append(line)
                }
                continue
            }
            if trimmed.hasPrefix("```") || trimmed.hasPrefix("~~~") {
                flush()
                let l = trimmed.dropFirst(3).trimmingCharacters(in: .whitespaces)
                lang = l.isEmpty ? nil : l
                code = []
                continue
            }
            if trimmed.isEmpty {
                flush()
                continue
            }
            if let h = heading(trimmed) {
                flush()
                out.append(h)
                continue
            }
            if trimmed == "---" || trimmed == "***" || trimmed == "___" {
                flush()
                out.append(.rule)
                continue
            }
            if trimmed.hasPrefix(">") {
                flush()
                let t = trimmed.dropFirst().trimmingCharacters(in: .whitespaces)
                if case .quote(let prev)? = out.last {
                    out[out.count - 1] = .quote(prev + "\n" + t)
                } else {
                    out.append(.quote(t))
                }
                continue
            }
            if let i = item(line) {
                flush()
                out.append(i)
                continue
            }
            para.append(trimmed)
        }
        if let c = code { out.append(.code(language: lang, text: c.joined(separator: "\n"))) }
        flush()
        return out
    }

    private static func heading(_ t: String) -> MarkdownBlock? {
        var n = 0
        for ch in t {
            if ch == "#" { n += 1 } else { break }
        }
        guard (1...6).contains(n), t.count > n, t[t.index(t.startIndex, offsetBy: n)] == " " else { return nil }
        return .heading(level: n, text: String(t.dropFirst(n + 1)).trimmingCharacters(in: .whitespaces))
    }

    private static func item(_ line: String) -> MarkdownBlock? {
        let indent = line.prefix { $0 == " " }.count
        let t = line.dropFirst(indent)
        let level = indent / 2
        for b in ["- ", "* ", "+ "] where t.hasPrefix(b) {
            var rest = String(t.dropFirst(2))
            var marker = "•"
            if rest.hasPrefix("[ ] ") { marker = "☐"; rest = String(rest.dropFirst(4)) }
            if rest.hasPrefix("[x] ") || rest.hasPrefix("[X] ") { marker = "☑"; rest = String(rest.dropFirst(4)) }
            return .item(marker: marker, level: level, text: rest)
        }
        let digits = t.prefix { $0.isNumber }
        if !digits.isEmpty, digits.count <= 3 {
            let after = t.dropFirst(digits.count)
            if after.hasPrefix(". ") || after.hasPrefix(") ") {
                return .item(marker: digits + ".", level: level, text: String(after.dropFirst(2)))
            }
        }
        return nil
    }
}

/// A line of an edit's diff.
public struct DiffLine: Hashable, Sendable {
    public enum Kind: Hashable, Sendable { case removed, added }
    public var kind: Kind
    public var text: String
}

/// A tool call card (app.md 4.1): a header, and a body that is folded by default.
public struct ToolCard: Hashable, Sendable {
    public enum Detail: Hashable, Sendable {
        /// A shell command, shown exactly.
        case command(String)
        /// An edit, as removed and added lines.
        case diff([DiffLine])
        /// Any other input, as formatted JSON.
        case input(String)
        case none
    }

    public var name: String
    public var target: String?
    public var status: ToolStatus
    /// Reads and searches fold to one line.
    public var oneLine: Bool
    public var detail: Detail
    public var output: String?
    public var outputTruncated: Bool
    /// A subagent's own messages, folded inside its card.
    public var children: [ChatItem]
}

/// One row of the chat view, built from the hab's chat messages.
public indirect enum ChatItem: Hashable, Sendable, Identifiable {
    case user(id: String, text: String, sender: String?, attachments: [String])
    case agent(id: String, blocks: [MarkdownBlock], sender: String?)
    case tool(id: String, card: ToolCard)
    case event(id: String, event: ChatEvent, text: String)
    /// A plan checklist. Only the latest plan shows its steps; earlier ones fold.
    case plan(id: String, steps: [PlanStep], latest: Bool)
    /// A message kind this app does not know.
    case unsupported(id: String, kind: String)

    public var id: String {
        switch self {
        case .user(let id, _, _, _), .agent(let id, _, _), .tool(let id, _), .event(let id, _, _),
             .plan(let id, _, _), .unsupported(let id, _):
            return id
        }
    }
}

/// Builds chat items from messages, and a plain-text rendering of them for
/// snapshot tests.
public enum ChatRender {
    /// Tool names whose card folds to one line.
    static let oneLineTools: Set<String> = ["Read", "Grep", "Glob", "LS", "WebFetch", "WebSearch", "NotebookRead",
                                            "read", "grep", "glob", "list", "webfetch", "websearch",
                                            "web_search", "view_image"]

    /// Rows already built, by message id, with the message they came from.
    public struct Cache {
        var rows: [String: (message: ChatMessage, item: ChatItem)] = [:]
        public init() {}
    }

    public static func items(_ messages: [ChatMessage]) -> [ChatItem] {
        var cache = Cache()
        return items(messages, cache: &cache)
    }

    /// Builds rows, reusing the cached row of each unchanged message. Plans and
    /// tool calls with nested messages are rebuilt each time: they depend on
    /// other messages.
    public static func items(_ messages: [ChatMessage], cache: inout Cache) -> [ChatItem] {
        let lastPlan = messages.last { $0.kind == .plan && $0.parent == nil }?.id
        var children: [String: [ChatMessage]] = [:]
        let toolIDs = Set(messages.filter { $0.kind == .tool }.map(\.id))
        var top: [ChatMessage] = []
        for m in messages {
            if let p = m.parent, toolIDs.contains(p), p != m.id {
                children[p, default: []].append(m)
            } else {
                top.append(m)
            }
        }
        var next = Cache()
        func build(_ m: ChatMessage, depth: Int) -> ChatItem {
            let kids = children[m.id] ?? []
            let cacheable = m.kind != .plan && kids.isEmpty
            if cacheable, let hit = cache.rows[m.id], hit.message == m {
                next.rows[m.id] = hit
                return hit.item
            }
            let item: ChatItem
            switch m.kind {
            case .user:
                item = .user(id: m.id, text: SpokenText.hidingSummaryRequest(m.text ?? ""), sender: m.sender, attachments: m.attachments ?? [])
            case .agent:
                item = .agent(id: m.id, blocks: MarkdownBlock.parse(m.text ?? ""), sender: m.sender)
            case .tool:
                let built = depth < 4 ? kids.map { build($0, depth: depth + 1) } : []
                item = .tool(id: m.id, card: card(m.tool, children: built))
            case .event:
                let e = m.event ?? .unknown
                item = .event(id: m.id, event: e, text: eventText(e, m.text))
            case .plan:
                item = .plan(id: m.id, steps: m.plan ?? [], latest: m.parent != nil || m.id == lastPlan)
            default:
                item = .unsupported(id: m.id, kind: m.kind.rawValue)
            }
            if cacheable { next.rows[m.id] = (m, item) }
            return item
        }
        let out = top.map { build($0, depth: 0) }
        cache = next
        return out
    }

    static func card(_ t: ChatTool?, children: [ChatItem]) -> ToolCard {
        guard let t else {
            return ToolCard(name: "tool", target: nil, status: .running, oneLine: true, detail: .none, output: nil, outputTruncated: false, children: children)
        }
        return ToolCard(
            name: t.name,
            target: t.target,
            status: t.status,
            oneLine: oneLineTools.contains(t.name) && children.isEmpty,
            detail: detail(t),
            output: t.output,
            outputTruncated: t.outputTruncated ?? false,
            children: children)
    }

    static func detail(_ t: ChatTool) -> ToolCard.Detail {
        guard let input = t.input else { return .none }
        let obj = input.objectValue
        if let cmd = obj?["command"]?.stringValue, ["Bash", "bash", "shell", "exec_command", "local_shell"].contains(t.name) || obj?.count ?? 0 <= 2 {
            return .command(cmd)
        }
        if let old = obj?["old_string"]?.stringValue ?? obj?["oldString"]?.stringValue,
           let new = obj?["new_string"]?.stringValue ?? obj?["newString"]?.stringValue {
            return .diff(old.components(separatedBy: "\n").map { DiffLine(kind: .removed, text: $0) }
                + new.components(separatedBy: "\n").map { DiffLine(kind: .added, text: $0) })
        }
        if let content = obj?["content"]?.stringValue, t.name == "Write" || t.name == "write" {
            return .diff(content.components(separatedBy: "\n").map { DiffLine(kind: .added, text: $0) })
        }
        // Codex apply_patch: each file holds a unified diff, or the content of a new file.
        if let files = obj, !files.isEmpty,
           files.values.allSatisfy({ $0.objectValue?["unified_diff"] != nil || $0.objectValue?["content"] != nil }) {
            var lines: [DiffLine] = []
            for (_, f) in files.sorted(by: { $0.key < $1.key }) {
                let o = f.objectValue
                if let d = o?["unified_diff"]?.stringValue {
                    for l in d.components(separatedBy: "\n") where !l.hasPrefix("---") && !l.hasPrefix("+++") {
                        if l.hasPrefix("-") { lines.append(DiffLine(kind: .removed, text: String(l.dropFirst()))) }
                        if l.hasPrefix("+") { lines.append(DiffLine(kind: .added, text: String(l.dropFirst()))) }
                    }
                } else if let c = o?["content"]?.stringValue {
                    lines += c.components(separatedBy: "\n").map { DiffLine(kind: .added, text: $0) }
                }
            }
            if !lines.isEmpty { return .diff(lines) }
        }
        return .input(input.prettyJSON)
    }

    static func eventText(_ e: ChatEvent, _ text: String?) -> String {
        if let t = text, !t.isEmpty { return t }
        switch e {
        case .compaction: return "Conversation compacted"
        case .interrupt: return "Interrupted"
        case .reset: return "The transcript was replaced"
        case .unknown: return "Unsupported message"
        default: return e.rawValue
        }
    }

    /// The status word a tool card shows.
    public static func statusWord(_ s: ToolStatus) -> String {
        switch s {
        case .running: return "running"
        case .done: return "done"
        case .error: return "error"
        case .denied: return "denied"
        default: return s.rawValue
        }
    }

    // MARK: - Text snapshot

    /// A stable plain-text rendering of the items: what each row shows, and
    /// what its folded body holds. Snapshot tests compare it.
    public static func text(_ items: [ChatItem]) -> String {
        var out: [String] = []
        for i in items { render(i, indent: "", into: &out) }
        return out.joined(separator: "\n") + "\n"
    }

    private static func render(_ item: ChatItem, indent: String, into out: inout [String]) {
        func add(_ s: String) {
            for l in s.components(separatedBy: "\n") { out.append(indent + l) }
        }
        func body(_ s: String) {
            for l in s.components(separatedBy: "\n") { out.append(indent + "    " + l) }
        }
        switch item {
        case .user(_, let text, let sender, let att):
            add("USER" + (sender.map { " from \($0)" } ?? "") + (att.isEmpty ? "" : " [\(att.count) attachments]"))
            body(text)
        case .agent(_, let blocks, let sender):
            add("AGENT" + (sender.map { " from \($0)" } ?? ""))
            for b in blocks {
                switch b {
                case .paragraph(let t): body(t)
                case .heading(let l, let t): body(String(repeating: "#", count: l) + " " + t)
                case .code(let lang, let t): body("```" + (lang ?? "")); body(t); body("```")
                case .item(let m, let l, let t): body(String(repeating: "  ", count: l) + m + " " + t)
                case .quote(let t): body("> " + t)
                case .rule: body("---")
                }
            }
        case .tool(_, let c):
            add("TOOL \(c.name) [\(statusWord(c.status))]" + (c.target.map { " \($0)" } ?? "") + (c.oneLine ? " (one line)" : "") + " (folded)")
            switch c.detail {
            case .command(let cmd): body("$ " + cmd.replacingOccurrences(of: "\n", with: "\n  "))
            case .diff(let lines): for l in lines { body((l.kind == .removed ? "- " : "+ ") + l.text) }
            case .input(let j): body("input: " + j)
            case .none: break
            }
            if let o = c.output, !o.isEmpty {
                body("output:" + (c.outputTruncated ? " (first part)" : ""))
                body(o.hasSuffix("\n") ? String(o.dropLast()) : o)
            }
            for k in c.children { render(k, indent: indent + "    | ", into: &out) }
        case .event(_, let e, let text):
            add("EVENT \(e.rawValue): " + text)
        case .plan(_, let steps, let latest):
            let done = steps.filter { $0.status == .done }.count
            add("PLAN \(done) of \(steps.count)" + (latest ? "" : " (earlier, folded)"))
            if latest {
                for s in steps {
                    let mark = s.status == .done ? "[x]" : s.status == .inProgress ? "[>]" : "[ ]"
                    body(mark + " " + s.text)
                }
            }
        case .unsupported(_, let kind):
            add("UNSUPPORTED \(kind)")
        }
    }
}

extension JSONValue {
    var objectValue: [String: JSONValue]? {
        if case .object(let o) = self { return o }
        return nil
    }

    var stringValue: String? {
        if case .string(let s) = self { return s }
        return nil
    }

    /// Sorted-key, indented JSON for a tool's input.
    var prettyJSON: String {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        guard let d = try? enc.encode(self), let s = String(data: d, encoding: .utf8) else { return "" }
        return s
    }
}
