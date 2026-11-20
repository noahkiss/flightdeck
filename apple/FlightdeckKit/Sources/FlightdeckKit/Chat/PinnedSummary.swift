import Foundation
import HabAPI

/// The one-line banners at the top of a chat (app.md 4.11): a confirmation or a group the
/// session coordinates, in a few words. A tap opens the full card in a sheet.
public enum PinnedSummary {
    /// A banner's text: the bold part, then the quieter part.
    public struct Line: Hashable, Sendable {
        public var title: String
        public var detail: String?

        public var text: String { detail.map { "\(title) · \($0)" } ?? title }
    }

    /// "Start a session? · from claude-2 · 56 min left". A closed card names its outcome.
    public static func confirm(_ c: Confirmation, remaining: TimeInterval?) -> Line {
        var parts: [String] = []
        if c.isClosed {
            parts.append(ConfirmCenter.outcome(c))
        } else {
            if let f = c.from, !f.isEmpty { parts.append("from \(f)") }
            if let r = remaining { parts.append(left(r)) }
        }
        return Line(title: ConfirmCardContent.title(c), detail: parts.isEmpty ? nil : parts.joined(separator: " · "))
    }

    /// "Group: Split the parser · 1 working, 1 waiting". A paused group says Paused; an ended
    /// or dropped one says so.
    public static func group(_ g: GroupRecord) -> Line {
        let title = "Group: " + (g.title.isEmpty ? g.id : g.title)
        switch g.state {
        case "paused": return Line(title: title, detail: "Paused")
        case "ended": return Line(title: title, detail: "Ended")
        case "dropped": return Line(title: title, detail: "Dropped")
        case "confirming": return Line(title: title, detail: "Waiting for you to confirm")
        default: break
        }
        let working = g.members.filter { GroupText.memberState($0) == "working" }.count
        let blocked = g.members.filter { GroupText.memberState($0) == "blocked" }.count
        let queued = g.items.filter { $0.state == "queued" }.count
        let failed = g.members.filter { $0.state == "failed" }.count
        let landed = g.items.filter { $0.state == "landed" }.count
        var parts: [String] = []
        if working > 0 { parts.append("\(working) working") }
        if queued + blocked > 0 { parts.append("\(queued + blocked) waiting") }
        if failed > 0 { parts.append("\(failed) failed") }
        if landed > 0 { parts.append("\(landed) landed") }
        if parts.isEmpty { parts.append(g.state == "ending" ? "Ending" : "Starting") }
        return Line(title: title, detail: parts.joined(separator: ", "))
    }

    /// "56 min left", "2 h 5 min left", "40 s left".
    public static func left(_ t: TimeInterval) -> String {
        let s = Int(t.rounded(.up))
        if s >= 3600 {
            let m = (s % 3600) / 60
            return m == 0 ? "\(s / 3600) h left" : "\(s / 3600) h \(m) min left"
        }
        if s >= 60 { return "\(s / 60) min left" }
        return "\(s) s left"
    }
}
