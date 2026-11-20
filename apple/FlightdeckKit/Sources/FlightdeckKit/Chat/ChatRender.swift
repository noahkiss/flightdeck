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
    /// The reply's closing "Spoken summary:" line (app.md 13.7): a quiet
    /// caption the owner can open, so the answer does not read twice.
    case spokenSummary(String)

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

/// A tool call card (app.md 4.1, 4.11): a one-sentence header, and a body that is folded by default.
public struct ToolCard: Hashable, Sendable {
    public enum Detail: Hashable, Sendable {
        /// A shell command, shown exactly.
        case command(String)
        /// An edit, as a diff per file with line numbers where known.
        case diff(ParsedDiff)
        /// An input that is not an object, as text.
        case input(String)
        case none
    }

    public var name: String
    public var target: String?
    public var status: ToolStatus
    /// The header sentence: "Edited src/api/stations.ts +3 -1".
    public var sentence: ToolSentence
    /// The Show in chat kind (app.md 4.11).
    public var kind: ChatContentKind
    /// Reads and searches fold to one line.
    public var oneLine: Bool
    public var detail: Detail
    /// The input's other fields, labelled: what the detail does not already show.
    public var fields: [ToolField]
    public var output: String?
    public var outputTruncated: Bool
    /// A subagent's own messages, folded inside its card.
    public var children: [ChatItem]
}

/// One row of the chat view, built from the hab's chat messages.
public indirect enum ChatItem: Hashable, Sendable, Identifiable {
    case user(id: String, text: String, sender: String?, attachments: [String])
    /// A user message to Capcom that carried context blocks (capcom.md 8.4): the blocks as
    /// chips, then the user's own text (empty for context alone).
    case capcomUser(id: String, context: [CapcomContextBlock], text: String, attachments: [String])
    case agent(id: String, blocks: [MarkdownBlock], sender: String?)
    case tool(id: String, card: ToolCard)
    case event(id: String, event: ChatEvent, text: String)
    /// A plan checklist. Only the latest plan shows its steps; earlier ones fold.
    case plan(id: String, steps: [PlanStep], latest: Bool)
    /// Capcom's `hand_off` call, as a hand-off card (app.md 4.8).
    case handOff(id: String, card: HandOffCard)
    /// Capcom's `file_record` call, as a proposal line (app.md 4.8).
    case proposal(id: String, line: ProposalLine)
    /// A message kind this app does not know.
    case unsupported(id: String, kind: String)

    public var id: String {
        switch self {
        case .user(let id, _, _, _), .capcomUser(let id, _, _, _), .agent(let id, _, _), .tool(let id, _), .event(let id, _, _),
             .plan(let id, _, _), .handOff(let id, _), .proposal(let id, _), .unsupported(let id, _):
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
                if m.sender == nil, let c = CapcomContextText.split(m.text ?? "") {
                    item = .capcomUser(id: m.id, context: c.blocks, text: SpokenText.hidingSummaryRequest(c.text), attachments: m.attachments ?? [])
                } else {
                    item = .user(id: m.id, text: SpokenText.hidingSummaryRequest(m.text ?? ""), sender: m.sender, attachments: m.attachments ?? [])
                }
            case .agent:
                let split = SpokenText.splittingSummary(m.text ?? "")
                item = .agent(id: m.id, blocks: MarkdownBlock.parse(split.body) + (split.summary.map { [.spokenSummary($0)] } ?? []), sender: m.sender)
            case .tool:
                if kids.isEmpty, let t = m.tool, let h = HandOffCard.from(t) {
                    item = .handOff(id: m.id, card: h)
                } else if kids.isEmpty, let t = m.tool, let p = ProposalLine.from(t) {
                    item = .proposal(id: m.id, line: p)
                } else {
                    let built = depth < 4 ? kids.map { build($0, depth: depth + 1) } : []
                    item = .tool(id: m.id, card: card(m.tool, children: built))
                }
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
            return ToolCard(name: "tool", target: nil, status: .running, sentence: ToolSentence(verb: "Using a tool"), kind: .commands,
                            oneLine: true, detail: .none, fields: [], output: nil, outputTruncated: false, children: children)
        }
        let (d, skip) = detail(t)
        var diff: ParsedDiff?
        if case .diff(let p) = d { diff = p }
        let sentence = ToolSentence.of(t, diff: diff)
        var fields: [ToolField] = []
        if let obj = t.input?.objectValue, !isPatchInput(obj) {
            // A value the sentence already says (the path, the pattern) is not repeated.
            let said = [sentence.subject, sentence.rest].compactMap { $0 }
            fields = ToolField.fields(obj, skip: skip).filter { f in !said.contains { $0 == f.value || $0.contains("\"\(f.value)\"") || $0.hasSuffix(" \(f.value)") } }
        }
        return ToolCard(
            name: t.name,
            target: t.target,
            status: t.status,
            sentence: sentence,
            kind: ChatKindMap.kind(tool: t.name),
            oneLine: oneLineTools.contains(t.name) && children.isEmpty,
            detail: d,
            fields: fields,
            output: t.output,
            outputTruncated: t.outputTruncated ?? false,
            children: children)
    }

    /// Codex `apply_patch` input: one entry per file, each a unified diff or a new file's content.
    static func isPatchInput(_ obj: [String: JSONValue]) -> Bool {
        !obj.isEmpty && obj.values.allSatisfy { $0.objectValue?["unified_diff"] != nil || $0.objectValue?["content"] != nil }
    }

    static func lines(_ s: String) -> [String] {
        var l = s.components(separatedBy: "\n")
        if l.count > 1, l.last == "" { l.removeLast() }
        return l
    }

    /// The card's detail, and the input keys it already shows.
    static func detail(_ t: ChatTool) -> (ToolCard.Detail, Set<String>) {
        guard let input = t.input else { return (.none, []) }
        guard let obj = input.objectValue else {
            if case .null = input { return (.none, []) }
            return (.input(input.prettyJSON), [])
        }
        if let cmd = obj["command"]?.stringValue, ChatKindMap.family(t.name) == .shell || obj.count <= 2 {
            return (.command(cmd), ["command"])
        }
        let path = ToolSentence.pathField(obj)
        if let old = obj["old_string"]?.stringValue ?? obj["oldString"]?.stringValue,
           let new = obj["new_string"]?.stringValue ?? obj["newString"]?.stringValue {
            let start = editStart(new: new, output: t.output)
            var hunk = ParsedDiff.Hunk(header: "")
            for (i, l) in lines(old).enumerated() {
                hunk.lines.append(.init(kind: .removed, text: l, old: start.map { $0 + i }, new: nil))
            }
            for (i, l) in lines(new).enumerated() {
                hunk.lines.append(.init(kind: .added, text: l, old: nil, new: start.map { $0 + i }))
            }
            let f = ParsedDiff.File(oldPath: path, newPath: path, hunks: [hunk])
            return (.diff(ParsedDiff(files: [f])), ["old_string", "oldString", "new_string", "newString", "file_path", "filePath", "path"])
        }
        // Claude MultiEdit: several edits of one file, without line numbers.
        if case .array(let edits)? = obj["edits"], !edits.isEmpty {
            var hunks: [ParsedDiff.Hunk] = []
            for e in edits {
                guard let o = e.objectValue, let old = o["old_string"]?.stringValue, let new = o["new_string"]?.stringValue else { continue }
                var h = ParsedDiff.Hunk(header: "")
                h.lines = lines(old).map { .init(kind: .removed, text: $0, old: nil, new: nil) }
                    + lines(new).map { .init(kind: .added, text: $0, old: nil, new: nil) }
                hunks.append(h)
            }
            if !hunks.isEmpty {
                return (.diff(ParsedDiff(files: [ParsedDiff.File(oldPath: path, newPath: path, hunks: hunks)])), ["edits", "file_path", "filePath"])
            }
        }
        if let content = obj["content"]?.stringValue, ChatKindMap.family(t.name) == .write {
            var h = ParsedDiff.Hunk(header: "")
            h.lines = lines(content).enumerated().map { .init(kind: .added, text: $1, old: nil, new: $0 + 1) }
            return (.diff(ParsedDiff(files: [ParsedDiff.File(oldPath: nil, newPath: path, hunks: [h])])), ["content", "file_path", "filePath", "path"])
        }
        // Codex apply_patch: each file holds a unified diff, or the content of a new file.
        if isPatchInput(obj) {
            var files: [ParsedDiff.File] = []
            for (name, f) in obj.sorted(by: { $0.key < $1.key }) {
                let o = f.objectValue
                if let d = o?["unified_diff"]?.stringValue {
                    let move = o?["move_path"]?.stringValue
                    var parsed = DiffParser.parse("--- a/x\n+++ b/x\n" + d).files
                    for i in parsed.indices {
                        parsed[i].oldPath = name
                        parsed[i].newPath = move ?? name
                    }
                    files += parsed
                } else if let c = o?["content"]?.stringValue {
                    let deleted = o?["type"]?.stringValue == "delete"
                    var h = ParsedDiff.Hunk(header: "")
                    h.lines = lines(c).enumerated().map { i, l in
                        deleted ? .init(kind: .removed, text: l, old: i + 1, new: nil) : .init(kind: .added, text: l, old: nil, new: i + 1)
                    }
                    files.append(ParsedDiff.File(oldPath: deleted ? name : nil, newPath: deleted ? nil : name, hunks: [h]))
                }
            }
            if !files.isEmpty { return (.diff(ParsedDiff(files: files)), Set(obj.keys)) }
        }
        return (.none, [])
    }

    /// The new text's first line number, from a Claude `Edit` result's `cat -n` snippet: the
    /// snippet line that holds the first non-empty new line, less its place in the new text.
    static func editStart(new: String, output: String?) -> Int? {
        guard let output, !output.isEmpty else { return nil }
        let newLines = lines(new)
        guard let firstIdx = newLines.firstIndex(where: { !$0.trimmingCharacters(in: .whitespaces).isEmpty }) else { return nil }
        let want = newLines[firstIdx]
        for raw in output.components(separatedBy: "\n") {
            let t = raw.drop { $0 == " " }
            let digits = t.prefix { $0.isNumber }
            guard !digits.isEmpty, let n = Int(digits) else { continue }
            var rest = t.dropFirst(digits.count)
            if rest.hasPrefix("→") || rest.hasPrefix("\t") { rest = rest.dropFirst() } else { continue }
            if String(rest) == want { return max(1, n - firstIdx) }
        }
        return nil
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
        case .capcomUser(_, let context, let text, let att):
            add("USER" + (att.isEmpty ? "" : " [\(att.count) attachments]"))
            for c in context { body("[context \(c.kind) \(c.ref) on \(c.host)] \(c.title)") }
            if !text.isEmpty { body(text) }
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
                case .spokenSummary(let t): body("(spoken summary, folded) " + t)
                }
            }
        case .tool(_, let c):
            add("TOOL \(c.name) [\(statusWord(c.status))]" + (c.target.map { " \($0)" } ?? "") + (c.oneLine ? " (one line)" : "") + " (folded)")
            body("says: " + c.sentence.text + " [\(c.kind.rawValue)]")
            switch c.detail {
            case .command(let cmd): body("$ " + cmd.replacingOccurrences(of: "\n", with: "\n  "))
            case .diff(let d):
                for f in d.files {
                    body("file \(f.newPath ?? f.oldPath ?? "?") +\(f.added) -\(f.removed)")
                    for h in f.hunks {
                        if !h.header.isEmpty { body(h.header) }
                        for l in h.lines {
                            let n = (l.new ?? l.old).map { String($0) } ?? ""
                            let mark = l.kind == .removed ? "- " : l.kind == .added ? "+ " : "  "
                            body(String(repeating: " ", count: max(0, 4 - n.count)) + n + " " + mark + l.text)
                        }
                    }
                }
            case .input(let j): body("input: " + j)
            case .none: break
            }
            for f in c.fields { body("\(f.label): \(f.value)") }
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
        case .handOff(_, let c):
            var head = "HANDOFF [\(c.deliveryWord)]"
            if let t = c.target {
                head += " to \(t.address)" + (t.host.map { " on \($0)" } ?? "") + " (\(t.harness), \(t.state))"
            } else if let n = c.named ?? c.project {
                head += " for \(n)" + (c.host.map { " on \($0)" } ?? "")
            }
            add(head)
            if let l = c.line { body(l) }
            if let id = c.messageID { body("message \(id), \(c.channelState ?? "")") }
            if let w = c.why { body("why: " + w) }
            for o in c.others { body("also: \(o.address) (\(o.harness), \(o.state))") }
            if let a = c.answer { body("answer:"); body(a) }
            body("sent:")
            body(c.message)
        case .proposal(_, let p):
            add("PROPOSAL [\(p.stateText)] \(p.type): \(p.title)" + (p.project.map { " (\($0))" } ?? "") + (p.proposal.map { " \($0)" } ?? ""))
            if let a = p.answer { body(a) }
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
