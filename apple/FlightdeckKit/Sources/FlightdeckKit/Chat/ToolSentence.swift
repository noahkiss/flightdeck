import Foundation
import HabAPI

/// A tool call's header as one sentence (app.md 4.11): "Edited src/api/stations.ts +3 -1",
/// "Searched for "readings" · 6 matches in 3 files", "Asked Linear for issue TIDE-41".
public struct ToolSentence: Hashable, Sendable {
    /// The verb, with a named server when there is one: "Edited", "Asked Linear".
    public var verb: String
    /// What the call acted on, shown monospaced: a path, a command.
    public var subject: String?
    /// The rest of the sentence, in plain text: "for "readings" · 6 matches in 3 files".
    public var rest: String?
    /// An edit's added and removed line counts.
    public var added: Int?
    public var removed: Int?

    public init(verb: String, subject: String? = nil, rest: String? = nil, added: Int? = nil, removed: Int? = nil) {
        self.verb = verb
        self.subject = subject
        self.rest = rest
        self.added = added
        self.removed = removed
    }

    /// The sentence as plain text, for VoiceOver and the text snapshots.
    public var text: String {
        var parts = [verb]
        if let s = subject, !s.isEmpty { parts.append(s) }
        if let r = rest, !r.isEmpty { parts.append(r) }
        if let a = added { parts.append("+\(a)") }
        if let r = removed { parts.append("-\(r)") }
        return parts.joined(separator: " ")
    }

    // MARK: - Building

    /// The sentence for a tool call. `diff` is the call's diff when the card has one, for the counts.
    public static func of(_ t: ChatTool, diff: ParsedDiff?) -> ToolSentence {
        let running = t.status == .running
        func v(_ past: String, _ ing: String) -> String { running ? ing : past }
        let obj = t.input?.objectValue ?? [:]
        let target = t.target.flatMap { $0.isEmpty ? nil : $0 }
        let name = t.name
        switch ChatKindMap.family(name) {
        case .edit:
            let files = diff?.files ?? []
            let added = files.reduce(0) { $0 + $1.added }
            let removed = files.reduce(0) { $0 + $1.removed }
            let counts = diff == nil ? (nil, nil) : (Optional(added), Optional(removed))
            if files.count > 1 {
                return ToolSentence(verb: v("Edited", "Editing"), rest: "\(files.count) files", added: counts.0, removed: counts.1)
            }
            let path = files.first.flatMap { $0.newPath ?? $0.oldPath } ?? pathField(obj) ?? target
            return ToolSentence(verb: v("Edited", "Editing"), subject: path, added: counts.0, removed: counts.1)
        case .write:
            let path = pathField(obj) ?? target
            let n = diff?.files.first?.added
            return ToolSentence(verb: v("Wrote", "Writing"), subject: path, added: n)
        case .read:
            return ToolSentence(verb: v("Read", "Reading"), subject: pathField(obj) ?? target)
        case .view:
            return ToolSentence(verb: v("Viewed", "Viewing"), subject: target ?? pathField(obj))
        case .grep:
            let pattern = obj["pattern"]?.stringValue ?? target
            var rest = pattern.map { "for \"\($0)\"" } ?? ""
            if t.status == .done, let c = searchCount(t.output) { rest += (rest.isEmpty ? "" : " · ") + c }
            return ToolSentence(verb: v("Searched", "Searching"), rest: rest.isEmpty ? nil : rest)
        case .glob:
            let pattern = obj["pattern"]?.stringValue ?? target
            var rest = pattern.map { "matching \"\($0)\"" } ?? ""
            if t.status == .done, let n = fileCount(t.output) { rest += (rest.isEmpty ? "" : " · ") + plural(n, "file") }
            return ToolSentence(verb: v("Found files", "Finding files"), rest: rest.isEmpty ? nil : rest)
        case .list:
            return ToolSentence(verb: v("Listed", "Listing"), subject: pathField(obj) ?? target)
        case .shell:
            let cmd = obj["command"]?.stringValue.map(firstLine) ?? target
            return ToolSentence(verb: v("Ran", "Running"), subject: cmd)
        case .fetch:
            let url = obj["url"]?.stringValue ?? target
            return ToolSentence(verb: v("Fetched", "Fetching"), rest: url.map(shortURL))
        case .webSearch:
            let q = obj["query"]?.stringValue ?? target
            return ToolSentence(verb: v("Searched the web", "Searching the web"), rest: q.map { "for \"\($0)\"" })
        case .plan:
            return ToolSentence(verb: v("Updated the plan", "Updating the plan"))
        case .subagent:
            let what = (obj["description"]?.stringValue ?? obj["prompt"]?.stringValue).map(firstLine) ?? target
            switch name {
            case "agent_wait": return ToolSentence(verb: v("Waited for a subagent", "Waiting for a subagent"))
            case "agent_spawn": return ToolSentence(verb: v("Started a subagent", "Starting a subagent"), rest: what)
            default: return ToolSentence(verb: v("Asked a subagent", "Asking a subagent") + (what == nil ? "" : ":"), rest: what)
            }
        case .imageGen:
            return ToolSentence(verb: v("Made an image", "Making an image"), subject: target)
        case .mcp:
            let (server, tool) = mcpParts(name)
            let verb = v("Asked", "Asking") + " " + serverName(server)
            return ToolSentence(verb: verb, rest: mcpRest(tool: tool, input: obj, target: target))
        case .other:
            return ToolSentence(verb: v("Used", "Using") + " " + name, subject: target)
        }
    }

    // MARK: - Helpers

    static func pathField(_ o: [String: JSONValue]) -> String? {
        for k in ["file_path", "filePath", "notebook_path", "path"] {
            if let s = o[k]?.stringValue, !s.isEmpty { return s }
        }
        return nil
    }

    static func firstLine(_ s: String) -> String {
        s.split(separator: "\n", omittingEmptySubsequences: true).first.map(String.init) ?? s
    }

    static func plural(_ n: Int, _ word: String) -> String {
        n == 1 ? "1 \(word)" : "\(n) \(word)s"
    }

    static func shortURL(_ u: String) -> String {
        var s = u
        for p in ["https://", "http://"] where s.hasPrefix(p) { s = String(s.dropFirst(p.count)) }
        if s.hasPrefix("www.") { s = String(s.dropFirst(4)) }
        return s
    }

    /// A search's count from its output: "6 matches in 3 files" for `path:line:` lines, "in 3
    /// files" for bare paths, "no matches" for none; nil when the output says nothing usable.
    static func searchCount(_ output: String?) -> String? {
        guard let output else { return nil }
        let lines = output.split(separator: "\n").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
        if lines.isEmpty { return nil }
        if lines.count == 1, lines[0].lowercased().hasPrefix("no ") { return "no matches" }
        var matches = 0
        var files = Set<String>()
        var bare = 0
        for l in lines {
            if l.hasPrefix("Found ") && (l.hasSuffix(" files") || l.hasSuffix(" file")) { continue }
            let parts = l.split(separator: ":", maxSplits: 2, omittingEmptySubsequences: false)
            if parts.count >= 3, Int(parts[1]) != nil {
                matches += 1
                files.insert(String(parts[0]))
            } else {
                bare += 1
            }
        }
        if matches > 0 { return (matches == 1 ? "1 match" : "\(matches) matches") + " in " + plural(files.count, "file") }
        if bare > 0 { return "in " + plural(bare, "file") }
        return nil
    }

    /// The files a glob listed; nil without output.
    static func fileCount(_ output: String?) -> Int? {
        guard let output else { return nil }
        let lines = output.split(separator: "\n").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
        if lines.count == 1, lines[0].lowercased().hasPrefix("no ") { return 0 }
        return lines.isEmpty ? nil : lines.count
    }

    /// `mcp__<server>__<tool>`: the server and the tool.
    static func mcpParts(_ name: String) -> (server: String, tool: String) {
        var rest = name
        if rest.hasPrefix("mcp__") { rest = String(rest.dropFirst(5)) }
        if let r = rest.range(of: "__") {
            return (String(rest[..<r.lowerBound]), String(rest[r.upperBound...]))
        }
        return (rest, "")
    }

    static let knownServers = ["github": "GitHub", "gitlab": "GitLab", "linear": "Linear", "slack": "Slack",
                               "notion": "Notion", "jira": "Jira", "sentry": "Sentry", "figma": "Figma",
                               "google drive": "Google Drive", "gmail": "Gmail", "asana": "Asana"]

    /// A server's name as people say it: no `claude_ai_` prefix, spaces for underscores, a capital.
    public static func serverName(_ raw: String) -> String {
        var s = raw
        if s.hasPrefix("claude_ai_") { s = String(s.dropFirst("claude_ai_".count)) }
        s = s.replacingOccurrences(of: "_", with: " ").replacingOccurrences(of: "-", with: " ")
        s = s.trimmingCharacters(in: .whitespaces)
        if s.isEmpty { return "a server" }
        if let k = knownServers[s.lowercased()] { return k }
        if s == s.lowercased() { return s.prefix(1).uppercased() + s.dropFirst() }
        return s
    }

    /// The rest of an MCP sentence: "for issue TIDE-41", "to search "pytest fixtures"", "to list
    /// projects".
    static func mcpRest(tool: String, input: [String: JSONValue], target: String?) -> String? {
        let words = tool.replacingOccurrences(of: "-", with: "_").split(separator: "_").map(String.init)
            .flatMap(splitCamel).map { $0.lowercased() }
        if words.first == "get" || words.first == "read" || words.first == "fetch", words.count > 1 {
            let noun = words.dropFirst().joined(separator: " ")
            if let id = idValue(input) { return "for \(noun) \(id)" }
            if let t = target { return "for \(noun) \(t)" }
            return "for \(noun)"
        }
        for k in ["query", "q", "search", "text"] {
            if let q = input[k]?.stringValue, !q.isEmpty { return "to search \"\(q)\"" }
        }
        if words.isEmpty { return target.map { "about \($0)" } }
        return "to " + words.joined(separator: " ") + (target.map { " \($0)" } ?? "")
    }

    static func idValue(_ o: [String: JSONValue]) -> String? {
        for (k, v) in o.sorted(by: { $0.key < $1.key }) {
            let lk = k.lowercased()
            guard lk == "id" || lk.hasSuffix("id") || lk.hasSuffix("_id") || lk == "key" || lk.hasSuffix("number") else { continue }
            switch v {
            case .string(let s) where !s.isEmpty: return s
            case .int(let n): return String(n)
            default: continue
            }
        }
        return nil
    }

    static func splitCamel(_ s: String) -> [String] {
        var out: [String] = []
        var cur = ""
        for ch in s {
            if ch.isUppercase, !cur.isEmpty, cur.last?.isLowercase == true {
                out.append(cur)
                cur = ""
            }
            cur.append(ch)
        }
        if !cur.isEmpty { out.append(cur) }
        return out
    }
}

/// One labelled row of a tool's input (app.md 4.11): the key in plain words, the value as text.
public struct ToolField: Hashable, Sendable {
    public var label: String
    public var value: String
    /// Monospaced: a path, a pattern, a command, JSON.
    public var code: Bool

    public init(label: String, value: String, code: Bool = false) {
        self.label = label
        self.value = value
        self.code = code
    }

    static let labels: [String: String] = [
        "file_path": "File", "filePath": "File", "notebook_path": "Notebook", "path": "Path",
        "old_string": "Old text", "oldString": "Old text", "new_string": "New text", "newString": "New text",
        "content": "Content", "command": "Command", "description": "Description", "pattern": "Pattern",
        "query": "Query", "url": "URL", "prompt": "Prompt", "glob": "Files", "output_mode": "Output",
        "-i": "Ignore case", "-n": "Line numbers", "-A": "Lines after", "-B": "Lines before", "-C": "Lines around",
        "replace_all": "Replace all", "replaceAll": "Replace all", "limit": "Limit", "offset": "Offset",
        "timeout": "Timeout", "subagent_type": "Agent type", "todos": "Steps", "type": "Type",
        "run_in_background": "In background", "head_limit": "First results", "multiline": "Multi-line",
        "allowed_domains": "Allowed sites", "blocked_domains": "Blocked sites",
    ]

    /// Keys whose value is code-like.
    static let codeKeys: Set<String> = ["file_path", "filePath", "notebook_path", "path", "pattern", "glob", "command", "url"]

    /// The key first in the list; the rest alphabetically.
    static let order = ["description", "file_path", "filePath", "path", "pattern", "query", "url", "command", "prompt"]

    /// The labelled rows of an input object, leaving out `skip`.
    public static func fields(_ input: [String: JSONValue], skip: Set<String> = []) -> [ToolField] {
        let keys = input.keys.filter { !skip.contains($0) }.sorted { a, b in
            let ia = order.firstIndex(of: a) ?? Int.max
            let ib = order.firstIndex(of: b) ?? Int.max
            return ia != ib ? ia < ib : a < b
        }
        return keys.compactMap { k in
            guard let v = input[k] else { return nil }
            let (text, json) = valueText(v)
            if text.isEmpty { return nil }
            return ToolField(label: label(k), value: text, code: json || codeKeys.contains(k))
        }
    }

    /// A key in plain words: a known label, else its words with a capital.
    public static func label(_ key: String) -> String {
        if let l = labels[key] { return l }
        let words = key.trimmingCharacters(in: CharacterSet(charactersIn: "-_"))
            .replacingOccurrences(of: "-", with: "_").split(separator: "_").map(String.init)
            .flatMap(ToolSentence.splitCamel).map { $0.lowercased() }
        let s = words.joined(separator: " ")
        return s.prefix(1).uppercased() + s.dropFirst()
    }

    /// A value as text; true when it is JSON (a nested value).
    static func valueText(_ v: JSONValue) -> (String, Bool) {
        switch v {
        case .null: return ("", false)
        case .bool(let b): return (b ? "Yes" : "No", false)
        case .int(let n): return (String(n), false)
        case .uint(let n): return (String(n), false)
        case .number(let d): return (d == d.rounded() && abs(d) < 1e15 ? String(Int64(d)) : String(d), false)
        case .string(let s): return (s, false)
        case .array, .object:
            let enc = JSONEncoder()
            enc.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
            let s = (try? enc.encode(v)).flatMap { String(data: $0, encoding: .utf8) } ?? ""
            return (s, true)
        }
    }
}
