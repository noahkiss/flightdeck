import Foundation
import HabAPI

/// Answers from the chat (app.md 4.11): what the composer does while a request is open, the
/// line after an answer, and the card's words when an answer fails.
public enum ChatAnswer {
    /// What Send does now.
    public enum ComposerMode: Equatable, Sendable {
        /// Sends a message, now or after the turn (the send gate decides).
        case message
        /// A permission or plan is open: the message waits until the agent is at its prompt.
        case queue
        /// A question with options is open: Send answers it with the text.
        case answer
        /// Send is off: no agent, offline, or a request only the terminal can answer.
        case off
    }

    public static func composerMode(gate: SendGate, agent: AgentState?, card: ApprovalCenter.CardState?) -> ComposerMode {
        switch gate {
        case .now, .afterTurn: return .message
        case .noAgent, .offline: return .off
        case .blocked:
            if let card, case .questions = card.request.form, card.canAnswer { return .answer }
            if agent?.kind == .permission, !(card.map(isQuestion) ?? false) { return .queue }
            return .off
        }
    }

    static func isQuestion(_ c: ApprovalCenter.CardState) -> Bool {
        if case .questions = c.request.form { return true }
        return false
    }

    /// The composer's placeholder.
    public static func placeholder(_ mode: ComposerMode, gate: SendGate, agent: AgentState?) -> String {
        switch mode {
        case .answer: return "Message, or answer above"
        case .queue: return "Message (sends after this turn)"
        case .message: return gate == .afterTurn ? "Message (sends after this turn)" : "Message"
        case .off:
            switch gate {
            case .offline: return "Offline"
            case .noAgent: return "No agent"
            default: return agent?.kind == .trust ? "Answer the folder prompt first" : "Answer in the terminal first"
            }
        }
    }

    /// The line after an answer: "Allowed · npm run migrate", "Answered · Harbor at night".
    public static func note(allow: Bool?, reply: String? = nil, request: OpenRequest) -> String {
        if let reply {
            let first = reply.split(separator: "\n").first.map(String.init) ?? reply
            return "Answered · " + clip(first)
        }
        let verb: String
        if request.form.isPlan {
            verb = allow == true ? "Plan approved" : "Kept planning"
            return verb
        }
        verb = allow == true ? "Allowed" : "Denied"
        return verb + " · " + clip(subject(request))
    }

    /// What the request was about, in a few words: the command's first line, the file, the tool.
    static func subject(_ r: OpenRequest) -> String {
        if let c = r.request?.command, !c.isEmpty { return c.split(separator: "\n").first.map(String.init) ?? c }
        if let f = r.request?.files, let first = f.first {
            return f.count == 1 ? (first as NSString).lastPathComponent : "\(f.count) files"
        }
        if let t = r.request?.tool, !t.isEmpty { return t }
        return r.title
    }

    static func clip(_ s: String, _ n: Int = 60) -> String {
        s.count <= n ? s : String(s.prefix(n - 1)) + "…"
    }

    /// The card's words for an answer that did not land; never the hab's hint.
    public static func failureText(_ r: OpenRequest, hint: String) -> String {
        // The hab's words for a question answer it does not take yet (harness-adapters.md,
        // Permission answers); only that phrase is read, never shown.
        if case .questions = r.form, hint.contains("not built") || hint.contains("answer it in the terminal") {
            return "Flightdeck can't answer this question yet. Answer it in the terminal."
        }
        if r.offline { return "\(r.habLabel) is offline. Try again when it's back." }
        return "The answer didn't go through. Try again, or answer in the terminal."
    }
}

/// The chat's empty and error states in plain words (app.md 4.11). The hab's hint stays in the
/// log, never on screen.
public struct ChatEmptyState: Equatable, Sendable {
    public var title: String
    public var line: String
    public var symbol: String
    /// The state offers Try again.
    public var retry: Bool

    public static func of(_ load: ChatLoad, context: ChatContext) -> ChatEmptyState? {
        let host = context.hostLabel.isEmpty ? "this hab" : context.hostLabel
        switch load {
        case .loading:
            return nil
        case .waiting:
            return ChatEmptyState(title: "Waiting for the first message", line: "The conversation shows here once the agent writes it.",
                                  symbol: "text.bubble", retry: false)
        case .unavailable:
            let who = context.harness.map(harnessName) ?? "This session"
            return ChatEmptyState(title: "No chat for this session",
                                  line: "\(who) on \(host) keeps no conversation Flightdeck can show. The terminal shows the session as it runs.",
                                  symbol: "text.bubble", retry: false)
        case .failed:
            return ChatEmptyState(title: "The chat didn't load", line: "Flightdeck couldn't read this conversation from \(host).",
                                  symbol: "exclamationmark.bubble", retry: true)
        case .ready:
            return ChatEmptyState(title: "No messages yet", line: "Send a message to start.", symbol: "text.bubble", retry: false)
        }
    }
}
