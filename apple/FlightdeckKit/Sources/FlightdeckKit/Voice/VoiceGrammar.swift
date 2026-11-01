import Foundation
import HabAPI

/// The fixed words voice mode acts on (app.md 13.1, 13.3). Never a model: a
/// command matches only when the whole utterance is the command.
public enum VoiceGrammar {
    /// A spoken command, matched on the whole utterance.
    public enum Command: Equatable, Sendable {
        case stop
        case repeatReply
        case allow
        case deny
        case endVoice
    }

    /// An answer to a permission request.
    public enum Answer: Equatable, Sendable {
        case allow
        case deny
    }

    /// Lowercase words with punctuation dropped: "Allow." and " allow!" match "allow".
    public static func normalize(_ s: String) -> String {
        let kept = s.lowercased().unicodeScalars.map { CharacterSet.letters.contains($0) || CharacterSet.decimalDigits.contains($0) ? Character($0) : " " }
        return String(kept).split(separator: " ").joined(separator: " ")
    }

    /// The command the whole utterance names, or nil: the text is a message.
    public static func command(_ utterance: String) -> Command? {
        switch normalize(utterance) {
        case "stop": return .stop
        case "repeat", "say that again": return .repeatReply
        case "allow", "approve": return .allow
        case "deny": return .deny
        case "end voice", "end voice mode": return .endVoice
        default: return nil
        }
    }

    /// The answer to "Allow or deny?", or nil when it is not a clear match.
    /// "yes" is not an Allow: a stray yes must never run a command. "no" is a
    /// Deny, which is safe.
    public static func answer(_ utterance: String) -> Answer? {
        switch normalize(utterance) {
        case "allow", "approve", "allow it", "approve it": return .allow
        case "deny", "no", "deny it": return .deny
        default: return nil
        }
    }

    // MARK: - Reading a request

    /// The longest command voice may Allow, as the banner's body budget (app.md 16.2).
    public static let allowMaxCharacters = 100
    public static let allowMaxWords = 16

    /// Whether a voice Allow may answer this request (app.md 13.3): a
    /// permission with an id, not expired, and a command short enough to
    /// read back whole. Anything else can be denied by voice, allowed by tap.
    public static func voiceMayAllow(_ r: PermissionRequest?, now: Date = Date()) -> Bool {
        guard let r, r.id != nil else { return false }
        if let e = r.expiresAt.flatMap(BoardRows.parseStamp), e <= now { return false }
        guard let c = r.command?.trimmingCharacters(in: .whitespacesAndNewlines), !c.isEmpty else {
            if r.command != nil { return false }
            // A file edit: the files are the request; read them all or tap.
            let files = r.files ?? []
            return !files.isEmpty && files.count <= 3
        }
        // A command substitution cannot be read back: allow it on screen only.
        if c.contains("$(") || c.contains("`") { return false }
        let words = c.split(whereSeparator: { $0.isWhitespace }).count
        return c.count <= allowMaxCharacters && words <= allowMaxWords
    }

    /// What voice mode says for a permission request: its title, then the
    /// command in a speakable form, then the question.
    public static func readBack(_ r: PermissionRequest?, mayAllow: Bool) -> String {
        let title = r.flatMap { req -> String? in
            if let t = req.title, !t.isEmpty { return t }
            if let d = req.description, !d.isEmpty { return d }
            return req.tool.map { "Allow \($0)?" }
        } ?? "The agent asks for permission."
        var parts = [SpokenText.sentence(title)]
        if let c = r?.command, !c.isEmpty {
            parts.append(SpokenText.sentence(speakCommand(c)))
        } else if let files = r?.files, !files.isEmpty {
            parts.append(SpokenText.sentence(files.map(speakPath).joined(separator: ", ")))
        }
        parts.append(mayAllow ? "Allow or deny?" : "This one can be allowed only on screen. Say deny, or tap.")
        return parts.joined(separator: " ")
    }

    /// A path read whole: "~/work/build" is "home slash work slash build".
    static func speakPath(_ p: String) -> String {
        var t = p
        if t.hasPrefix("~/") { t = "home/" + t.dropFirst(2) } else if t == "~" { t = "home" }
        return t.replacingOccurrences(of: "/", with: " slash ")
            .replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
            .trimmingCharacters(in: .whitespaces)
    }

    /// A command as it is read aloud: "wrangler deploy --env prod" becomes
    /// "wrangler deploy, dash dash env prod".
    public static func speakCommand(_ c: String) -> String {
        var out: [String] = []
        for token in c.split(whereSeparator: { $0.isWhitespace }) {
            var t = String(token)
            if t.hasPrefix("--") {
                t = "dash dash " + t.dropFirst(2)
                out.append(",")
            } else if t.hasPrefix("-"), t.count > 1 {
                t = "dash " + t.dropFirst()
                out.append(",")
            } else {
                t = t.replacingOccurrences(of: "&&", with: " and then ")
                    .replacingOccurrences(of: "||", with: " or else ")
                    .replacingOccurrences(of: "|", with: " pipe ")
                    .replacingOccurrences(of: ";", with: " then ")
                    .replacingOccurrences(of: ">", with: " into ")
                // A path is read whole: the owner hears exactly what runs.
                if t.contains("/") { t = speakPath(t) }
            }
            out.append(t)
        }
        return out.joined(separator: " ")
            .replacingOccurrences(of: " ,", with: ",")
            .replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
            .trimmingCharacters(in: .whitespaces)
    }
}
