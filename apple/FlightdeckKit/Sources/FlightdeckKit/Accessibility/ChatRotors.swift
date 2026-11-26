import Foundation
import HabAPI

/// One stop on a VoiceOver rotor in the chat: the row's scroll id and what VoiceOver reads.
public struct ChatRotorEntry: Identifiable, Hashable, Sendable {
    public let id: String
    public let label: String
}

/// The chat's VoiceOver rotors (app.md 22.1): turns, tool calls and requests, built from the
/// rows the transcript or the Capcom thread draws. An entry's id is the row's `.id`, so VoiceOver can scroll a lazy
/// row it has not drawn yet into view.
public struct ChatRotors: Equatable, Sendable {
    /// The owner's messages: each starts a turn.
    public var turns: [ChatRotorEntry] = []
    /// Tool calls, and folded lines that hide tool calls.
    public var tools: [ChatRotorEntry] = []
    /// What an agent asked the owner: permission prompts, questions and plans to approve.
    public var requests: [ChatRotorEntry] = []

    /// Tools that ask the owner something: a question, a plan to approve.
    static let requestTools: Set<String> = ["AskUserQuestion", "ExitPlanMode"]

    /// The longest label, in characters, before it is cut with an ellipsis.
    static let labelMax = 80

    public static func of(_ rows: [ChatRow]) -> ChatRotors {
        var r = ChatRotors()
        for row in rows {
            switch row {
            case .item(let item):
                switch item {
                case .user(let id, let text, let sender, let att):
                    r.turns.append(ChatRotorEntry(id: id, label: turnLabel(text, sender: sender, attachments: att.count)))
                case .capcomUser(let id, let context, let text, let att):
                    let body = text.isEmpty ? context.map { $0.title.isEmpty ? $0.ref : $0.title }.joined(separator: ", ") : text
                    r.turns.append(ChatRotorEntry(id: id, label: turnLabel(body, sender: nil, attachments: att.count)))
                case .tool(let id, let card):
                    let entry = ChatRotorEntry(id: id, label: cut(card.sentence.text))
                    r.tools.append(entry)
                    if requestTools.contains(card.name) { r.requests.append(entry) }
                case .event(let id, let event, let text) where event == .permission:
                    r.requests.append(ChatRotorEntry(id: id, label: cut(text.isEmpty ? "Permission" : text)))
                default:
                    break
                }
            case .folded(let id, let items, let summary):
                // A folded line has a view; the rows inside it do not until it opens.
                if items.contains(where: { if case .tool = $0 { true } else { false } }) {
                    r.tools.append(ChatRotorEntry(id: id, label: summary))
                }
            }
        }
        return r
    }

    /// Capcom cards that ask the owner something: an approval, a confirmation, a grant, and a
    /// calendar or reminder card that offers choices (Add). A memory card's undo asks nothing.
    static func asks(_ card: CapcomCard?) -> Bool {
        switch card?.type {
        case "approval", "confirmation", "grant": return true
        case "calendar", "reminder": return !(card?.choices.isEmpty ?? true)
        default: return false
        }
    }

    /// The Capcom thread's rotors (app.md 22.1): the owner's messages, each turn's line of tool
    /// calls, and the cards that ask the owner. An entry's id is the row's id, as in a chat.
    public static func of(_ rows: [CapcomRow]) -> ChatRotors {
        var r = ChatRotors()
        for row in rows {
            switch row {
            case .user(let id, let text, let att, let context, _):
                let body = text.isEmpty ? context.map { $0.title.isEmpty ? $0.ref : $0.title }.joined(separator: ", ") : text
                r.turns.append(ChatRotorEntry(id: id, label: turnLabel(body, sender: nil, attachments: att.count)))
            case .tools(let id, let line, _):
                r.tools.append(ChatRotorEntry(id: id, label: cut(line)))
            case .card(let it) where asks(it.card):
                let text = ToolSentence.firstLine(it.text ?? "").trimmingCharacters(in: .whitespaces)
                let label = text.isEmpty ? (it.card?.grant?.name).flatMap { $0.isEmpty ? nil : $0 } ?? "Capcom asks" : text
                r.requests.append(ChatRotorEntry(id: it.id, label: cut(label)))
            default:
                break
            }
        }
        return r
    }

    /// "You: Fix the failing test", or "From reviewer: …" for a message another agent sent.
    static func turnLabel(_ text: String, sender: String?, attachments: Int) -> String {
        let line = ToolSentence.firstLine(text).trimmingCharacters(in: .whitespaces)
        let body = line.isEmpty ? inflected("^[\(attachments) attachment](inflect: true)") : cut(line)
        if let sender { return "From \(sender): \(body)" }
        return "You: \(body)"
    }

    static func cut(_ s: String) -> String {
        s.count > labelMax ? String(s.prefix(labelMax - 1)) + "…" : s
    }
}
