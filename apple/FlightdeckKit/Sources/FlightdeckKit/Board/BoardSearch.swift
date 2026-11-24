import Foundation

/// The host scope of the board's search (app.md 3.6): every hab, or one.
public enum BoardSearchScope: Hashable, Sendable {
    case all
    case hab(String)
}

/// The board's search (app.md 3.6): matches the names the app already holds, so it works
/// offline. Every word typed must match somewhere in the row, ignoring case and accents.
public enum BoardSearch {
    /// The query's words, folded for matching; none for an empty query.
    public static func words(_ query: String) -> [String] {
        fold(query).split(whereSeparator: \.isWhitespace).map(String.init)
    }

    static func fold(_ s: String) -> String {
        s.folding(options: [.caseInsensitive, .diacriticInsensitive, .widthInsensitive], locale: nil)
    }

    /// The text a row matches on: titles and names, project, hab, harness by id and by name,
    /// model, and the context line.
    static func haystack(_ row: BoardRow) -> String {
        var parts = [row.title, row.host]
        if let n = row.name { parts.append(n) }
        if let p = row.project { parts.append(p) }
        if let h = row.harness { parts += [h, OpenRequest.harnessName(h)] }
        if let m = row.model { parts.append(m) }
        if let c = row.context { parts.append(c) }
        if let r = row.round { parts.append(r) }
        if let g = row.group { parts.append(g) }
        return fold(parts.joined(separator: "\n"))
    }

    static func matches(_ text: String, _ words: [String]) -> Bool {
        let t = fold(text)
        return words.allSatisfy { t.contains($0) }
    }

    /// Whether a row passes the query and the scope.
    public static func matches(_ row: BoardRow, words: [String], scope: BoardSearchScope) -> Bool {
        if case .hab(let id) = scope, row.target.habID != id { return false }
        guard !words.isEmpty else { return true }
        let hay = haystack(row)
        return words.allSatisfy { hay.contains($0) }
    }

    /// Whether the search filters anything.
    public static func isActive(query: String, scope: BoardSearchScope) -> Bool {
        !words(query).isEmpty || scope != .all
    }

    /// The board's sections with only matching rows; an emptied section goes (the Mac board).
    public static func filter(_ sections: [BoardSection], query: String, scope: BoardSearchScope) -> [BoardSection] {
        let w = words(query)
        guard !w.isEmpty || scope != .all else { return sections }
        return sections.compactMap { s in
            var s = s
            s.rows = s.rows.filter { matches($0, words: w, scope: scope) }
            return s.rows.isEmpty ? nil : s
        }
    }

    /// The Sessions list with only matching items; an emptied section goes, and the counts
    /// follow (iPhone and iPad).
    public static func filter(_ sections: [SessionsSection], query: String, scope: BoardSearchScope) -> [SessionsSection] {
        let w = words(query)
        guard !w.isEmpty || scope != .all else { return sections }
        return sections.compactMap { s in
            var s = s
            s.items = s.items.filter { matches($0, words: w, scope: scope) }
            return s.items.isEmpty ? nil : s
        }
    }

    static func matches(_ item: SessionItem, words: [String], scope: BoardSearchScope) -> Bool {
        switch item {
        case .permission(let r), .question(let r), .session(let r):
            return matches(r, words: words, scope: scope)
        case .confirm(let c):
            if case .hab(let id) = scope, c.habID != id { return false }
            return matches(ConfirmCardContent.title(c.confirmation) + "\n" + c.habLabel, words)
        case .group(let g):
            if case .hab(let id) = scope, !g.memberTargets.contains(where: { $0.habID == id }) && !g.id.hasPrefix(id + "/") {
                return false
            }
            return matches(g.title + "\n" + g.habLabel, words)
        }
    }

    /// The scope with a hab that left the app falls back to All.
    public static func fit(_ scope: BoardSearchScope, habs: [String]) -> BoardSearchScope {
        if case .hab(let id) = scope, !habs.contains(id) { return .all }
        return scope
    }
}
