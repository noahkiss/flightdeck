import Foundation
import HabAPI

/// A grant card of the Capcom thread (capcom-service.md 9.6, 9.9; app.md 21.5): Capcom set up a
/// job from the user's words, and the job needs action words the level does not give every
/// event turn. The card shows the job's prompt whole, its trigger and each word by its switch
/// title. Grant gives the words with one tap; Not now gives none.
public struct CapcomGrantCard: Equatable, Sendable {
    public nonisolated static let type = "grant"
    public nonisolated static let grant = "grant"
    public nonisolated static let notNow = "not_now"

    /// The job's name.
    public var name: String
    /// Each word Grant adds, by its switch title in Capcom jobs.
    public var words: [String]
    /// The prompt Capcom follows when the job's events come in, whole.
    public var prompt: String
    /// The trigger in plain words.
    public var trigger: String
    /// The hab's lead line when it shows the card again after the job changed; nil otherwise.
    public var lead: String?
    /// The answer, in words: "Granted", "Not now", "Expired"; nil while the card is open.
    public var answered: String?

    /// The card of a grant item; nil for any other item.
    public static func of(_ it: CapcomItem) -> CapcomGrantCard? {
        guard let card = it.card, card.type == type, let g = card.grant else { return nil }
        let titles = Dictionary(uniqueKeysWithValues: CapcomJobs.words.map { ($0.word, $0.title) })
        let lead = it.text?.components(separatedBy: "\n\n").first.flatMap { $0.hasPrefix("The job changed") ? $0 : nil }
        return CapcomGrantCard(name: g.name, words: g.actions.map { titles[$0] ?? $0.rawValue }, prompt: shown(g.prompt),
                               trigger: g.trigger, lead: lead, answered: it.answer.map { answeredText($0.choice) })
    }

    /// A prompt as the card shows it: runs of blank lines cut to one, so padding cannot put the
    /// instruction below the buttons (capcom-service.md 9.9). The hab cuts them too.
    static func shown(_ p: String) -> String {
        var out: [Substring] = []
        var blank = 0
        for line in p.split(separator: "\n", omittingEmptySubsequences: false) {
            if line.allSatisfy({ $0 == " " || $0 == "\t" }) {
                blank += 1
                if blank > 1 { continue }
                out.append("")
            } else {
                blank = 0
                out.append(line)
            }
        }
        return out.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Prompts longer than this show their length above them.
    public static let longPrompt = 500

    /// "1,234 characters" for a long prompt; nil for a short one.
    public var length: String? {
        prompt.count > Self.longPrompt ? "\(prompt.count.formatted()) characters" : nil
    }

    /// The card's title: "Let job builds act?".
    public var title: String { "Let job \(name) act?" }

    static func answeredText(_ choice: String) -> String {
        switch choice {
        case grant: return "Granted"
        case notNow: return "Not now"
        case "expired": return "Expired: the job changed or is gone"
        default: return choice
        }
    }
}
