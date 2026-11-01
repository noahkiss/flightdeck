import Foundation
import HabAPI

/// What voice mode reads aloud (app.md 13.2, voice.md "Turn out"): the
/// agent's final text of the turn, made speakable by fixed rules. Never a
/// model: the same text always gives the same speech.
public enum SpokenText {
    /// The line the agent is asked to end with. When the final text has it,
    /// only what follows it is read.
    public static let summaryMarker = "Spoken summary:"

    /// Appended to each prompt sent from voice mode (voice.md Decisions).
    public static let summaryRequest = "(Voice mode: end your reply with one line that starts with \"\(summaryMarker)\" and gives a two-sentence summary to read aloud, with no code, paths or Markdown.)"

    /// A sent prompt as the chat view shows it: the summary request is hidden.
    public static func hidingSummaryRequest(_ text: String) -> String {
        guard text.hasSuffix(summaryRequest) else { return text }
        return String(text.dropLast(summaryRequest.count)).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// The prompt as voice mode sends it.
    public static func prompt(_ spoken: String, askSummary: Bool) -> String {
        askSummary ? spoken + "\n\n" + summaryRequest : spoken
    }

    /// Spoken in place of what is not read.
    public static let codePlaceholder = "A code block, shown on screen."
    public static let diffPlaceholder = "A diff, shown on screen."
    public static let tablePlaceholder = "A table."
    public static let restInChat = "The rest is in the chat."

    /// About 45 seconds of speech at the system voice's default rate.
    public static let defaultCapWords = 110

    // MARK: - The turn's text

    /// The last top-level agent text after the last user message: the
    /// agent's final text of the turn. Tool calls, tool output and subagent
    /// messages are never read.
    public static func finalText(_ messages: [ChatMessage]) -> String? {
        finalMessage(messages).map { ($0.text ?? "").trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    /// The message `finalText` reads.
    public static func finalMessage(_ messages: [ChatMessage]) -> ChatMessage? {
        let start = (messages.lastIndex { $0.kind == .user && $0.sender == nil }).map { $0 + 1 } ?? 0
        guard start < messages.count else { return nil }
        for m in messages[start...].reversed() where m.kind == .agent && m.parent == nil {
            if !(m.text ?? "").trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return m }
        }
        return nil
    }

    /// What is read when a turn ends (app.md 13.2): the speakable final
    /// text, "Done." with none, and "The turn stopped" for an error.
    public static func reply(_ messages: [ChatMessage], stopped: Bool, capWords: Int = defaultCapWords) -> String {
        let text = finalText(messages)
        if stopped {
            // The hab sends no error text, so the agent's last words stand in:
            // their first sentence ("main.go" does not end one).
            let first = text.map { speakable($0, capWords: capWords) }.flatMap(firstSentence)
            return first.map { "The turn stopped. \(sentence($0))" } ?? "The turn stopped."
        }
        guard let text else { return "Done." }
        let s = speakable(text, capWords: capWords)
        return s.isEmpty ? "Done." : s
    }

    // MARK: - Speakable text

    /// Turns Markdown into plain speech: the summary line alone when there is
    /// one; code and diffs as one placeholder each, tables as "A table.";
    /// links as their text, bare links as their site, paths as their file
    /// name; Markdown marks dropped. Capped at `capWords`.
    public static func speakable(_ markdown: String, capWords: Int = defaultCapWords) -> String {
        let source = summary(in: markdown) ?? markdown
        var parts: [String] = []
        var lastPlaceholder: String?
        func add(_ s: String, placeholder: Bool = false) {
            if placeholder {
                if lastPlaceholder == s { return }
                lastPlaceholder = s
            } else {
                lastPlaceholder = nil
            }
            parts.append(s)
        }

        var fence: (char: Character, count: Int)?
        var inDiff = false
        var inTable = false
        var inIndented = false
        var prevBlank = true
        var prevList = false
        for raw in source.components(separatedBy: .newlines) {
            let line = raw.trimmingCharacters(in: .whitespaces)
            defer {
                if !line.isEmpty { prevList = line.range(of: "^([-*+]|\\d+[.)])\\s", options: .regularExpression) != nil }
                prevBlank = line.isEmpty
            }
            if let f = fence {
                // A fence closes only with as many of its marks, alone on the line.
                if line.count >= f.count, line.allSatisfy({ $0 == f.char }) { fence = nil }
                continue
            }
            if let open = fenceOpen(line) {
                let lang = line.drop(while: { $0 == open.char }).trimmingCharacters(in: .whitespaces).lowercased()
                let diff = lang.hasPrefix("diff") || lang.hasPrefix("patch")
                fence = open
                add(diff ? diffPlaceholder : codePlaceholder, placeholder: true)
                continue
            }
            // An indented code block: four spaces or a tab after a blank line, not in a list.
            let indented = raw.hasPrefix("    ") || raw.hasPrefix("\t")
            if indented, !line.isEmpty, inIndented || (prevBlank && !prevList) {
                inIndented = true
                add(codePlaceholder, placeholder: true)
                continue
            }
            if !line.isEmpty { inIndented = false }
            if !inDiff, isTableSeparator(line) {
                // The header row above it was read as prose: take it back.
                if lastPlaceholder == nil, !parts.isEmpty { parts.removeLast() }
                inTable = true
                add(tablePlaceholder, placeholder: true)
                continue
            }
            if isDiffLine(raw, continuing: inDiff) {
                inDiff = true
                add(diffPlaceholder, placeholder: true)
                continue
            }
            inDiff = false
            if line.hasPrefix("|") || (inTable && line.contains("|")) {
                inTable = true
                add(tablePlaceholder, placeholder: true)
                continue
            }
            inTable = false
            let spoken = speakLine(line)
            if !spoken.isEmpty { add(sentence(spoken)) }
        }
        let joined = parts.joined(separator: " ")
            .replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
            .trimmingCharacters(in: .whitespaces)
        return cap(joined, words: capWords)
    }

    /// A fence opening: three or more backticks or tildes.
    static func fenceOpen(_ line: String) -> (char: Character, count: Int)? {
        guard let c = line.first, c == "`" || c == "~" else { return nil }
        let n = line.prefix(while: { $0 == c }).count
        return n >= 3 ? (c, n) : nil
    }

    /// A table's separator row, with or without outer pipes: "---|:---:".
    static func isTableSeparator(_ line: String) -> Bool {
        line.contains("-") && line.contains("|")
            && line.range(of: "^\\|?\\s*:?-{3,}:?\\s*(\\|\\s*:?-{3,}:?\\s*)+\\|?$", options: .regularExpression) != nil
    }

    /// The first sentence: up to a stop followed by a space or the end.
    static func firstSentence(_ s: String) -> String? {
        let t = s.trimmingCharacters(in: .whitespaces)
        guard !t.isEmpty else { return nil }
        if let r = t.range(of: "[.!?](\\s|$)", options: .regularExpression) {
            return String(t[..<r.lowerBound]).trimmingCharacters(in: .whitespaces) + String(t[r.lowerBound])
        }
        return t
    }

    /// The text after the summary marker, when the agent wrote one.
    static func summary(in text: String) -> String? {
        let lines = text.components(separatedBy: .newlines)
        let key = summaryMarker.lowercased()
        for (i, raw) in lines.enumerated().reversed() {
            // Allow Markdown around the marker: "**Spoken summary:** ...".
            let plain = raw.replacingOccurrences(of: "[*_>#]", with: "", options: .regularExpression)
                .trimmingCharacters(in: .whitespaces)
            guard plain.lowercased().hasPrefix(key) else { continue }
            let rest = ([String(plain.dropFirst(key.count))] + lines[(i + 1)...])
                .joined(separator: "\n")
                .trimmingCharacters(in: .whitespacesAndNewlines)
            return rest.isEmpty ? nil : rest
        }
        return nil
    }

    /// A line of a diff outside a fence: a header, a hunk, or (after one) a
    /// changed line.
    static func isDiffLine(_ line: String, continuing: Bool) -> Bool {
        if line.hasPrefix("diff --git ") || line.hasPrefix("@@ ") || line.hasPrefix("+++ ") || line.hasPrefix("--- ") {
            return true
        }
        guard continuing, !line.trimmingCharacters(in: .whitespaces).isEmpty else { return false }
        return line.hasPrefix("+") || line.hasPrefix("-") || line.hasPrefix(" ") || line.hasPrefix("index ") || line.hasPrefix("\\")
    }

    /// One line of prose, with Markdown marks and unspeakable tokens replaced.
    static func speakLine(_ line: String) -> String {
        var s = line
        // Block marks: headings, quotes, rules, list markers, task boxes.
        if s.range(of: "^([-*_]\\s*){3,}$", options: .regularExpression) != nil { return "" }
        s = s.replacingOccurrences(of: "^#{1,6}\\s+", with: "", options: .regularExpression)
        s = s.replacingOccurrences(of: "^(>\\s*)+", with: "", options: .regularExpression)
        s = s.replacingOccurrences(of: "^([-*+]|\\d+[.)])\\s+", with: "", options: .regularExpression)
        s = s.replacingOccurrences(of: "^\\[[ xX]\\]\\s+", with: "", options: .regularExpression)
        // HTML tags.
        s = s.replacingOccurrences(of: "</?[A-Za-z][^>]*>", with: "", options: .regularExpression)
        // Images go; links keep their text.
        s = s.replacingOccurrences(of: "!\\[([^\\]]*)\\]\\([^)]*\\)", with: "", options: .regularExpression)
        s = s.replacingOccurrences(of: "\\[([^\\]]+)\\]\\([^)]*\\)", with: "$1", options: .regularExpression)
        // Inline code keeps its words.
        s = s.replacingOccurrences(of: "`+([^`]*)`+", with: "$1", options: .regularExpression)
        // Emphasis marks; an underscore inside a word (snake_case) stays.
        s = s.replacingOccurrences(of: "\\*{1,3}|~~", with: "", options: .regularExpression)
        s = s.replacingOccurrences(of: "(?<![A-Za-z0-9])_{1,3}|_{1,3}(?![A-Za-z0-9])", with: "", options: .regularExpression)
        // Token by token: links to their site, paths to their file name.
        let words = s.split(separator: " ", omittingEmptySubsequences: true).map { speakToken(String($0)) }
        return words.joined(separator: " ").trimmingCharacters(in: .whitespaces)
    }

    /// A bare link becomes its site; a path becomes its file name.
    static func speakToken(_ token: String) -> String {
        // Keep punctuation that ends a sentence or clause around the token.
        var core = Substring(token)
        var tail = ""
        while let c = core.last, ",.;:!?)\"'".contains(c) {
            tail = String(c) + tail
            core = core.dropLast()
        }
        var head = ""
        while let c = core.first, "(\"'".contains(c) {
            head.append(c)
            core = core.dropFirst()
        }
        let t = String(core)
        if t.hasPrefix("http://") || t.hasPrefix("https://"), let host = URL(string: t)?.host() {
            let site = host.hasPrefix("www.") ? String(host.dropFirst(4)) : host
            return head + site + tail
        }
        if isPath(t), let name = t.split(separator: "/").last {
            return head + String(name) + tail
        }
        return token
    }

    /// A token that reads as a file path: segments joined by slashes, and
    /// either rooted (`/`, `~/`, `./`) or ending in a file name with a dot.
    static func isPath(_ t: String) -> Bool {
        guard t.contains("/"), t.range(of: "^[~.]{0,2}/?([A-Za-z0-9_.@+-]+/)+[A-Za-z0-9_.@+-]+/?$", options: .regularExpression) != nil else {
            return false
        }
        let rooted = t.hasPrefix("/") || t.hasPrefix("~/") || t.hasPrefix("./") || t.hasPrefix("../")
        let last = t.split(separator: "/").last.map(String.init) ?? ""
        return rooted || last.dropFirst().contains(".")
    }

    /// Ends a line with a stop so the voice pauses between lines.
    static func sentence(_ s: String) -> String {
        guard let c = s.last else { return s }
        return ".!?:;".contains(c) ? s : s + "."
    }

    /// Cuts past `words` at the last sentence end in the second half of the
    /// allowance (else at the word), then says the rest is in the chat.
    static func cap(_ s: String, words: Int) -> String {
        let all = s.split(separator: " ", omittingEmptySubsequences: true)
        guard all.count > words, words > 0 else { return s }
        var kept = Array(all.prefix(words))
        if let end = kept.lastIndex(where: { w in w.last.map { ".!?".contains($0) } ?? false }), end >= words / 2 {
            kept = Array(kept.prefix(end + 1))
        } else if let last = kept.last, let c = last.last, !".!?".contains(c) {
            kept[kept.count - 1] = Substring(String(last) + ".")
        }
        return kept.joined(separator: " ") + " " + restInChat
    }
}
