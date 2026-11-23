import Foundation

/// The long-press menu of a chat row (app.md 4.12): what it offers, and the text each action
/// takes. It works on a reply while it streams: each action reads the row's text when tapped.
public enum MessageMenu {
    public enum Action: Hashable, Sendable {
        case copy
        case selectText
        case share
        case readAloud
        case copyLastCode
        /// A tool card: the exact shell command.
        case copyCommand
        /// A tool card: its output.
        case copyOutput
    }

    /// The text a row's menu works on.
    public struct Content: Equatable, Sendable {
        /// What Copy, Share and Select Text take: a reply's Markdown as written, a message's text.
        public var text: String
        /// Read Aloud's words: Markdown marks dropped, code as a placeholder.
        public var speech: String
        /// A tool card's command and output.
        public var command: String?
        public var output: String?

        public init(text: String, speech: String, command: String? = nil, output: String? = nil) {
            self.text = text
            self.speech = speech
            self.command = command
            self.output = output
        }
    }

    /// Read Aloud reads a whole reply, not the short form voice mode speaks.
    static let readAloudWords = 2000

    /// A row's content; nil for a row with no menu (an event line, a plan, a fold).
    public static func content(_ item: ChatItem) -> Content? {
        switch item {
        case .user(_, let text, _, _), .capcomUser(_, _, let text, _):
            return text.isEmpty ? nil : Content(text: text, speech: text)
        case .agent(_, let text, _, _):
            return message(text)
        case .tool(_, let card):
            let command: String? = if case .command(let c) = card.detail { c } else { nil }
            let output = card.output.flatMap { $0.isEmpty ? nil : $0 }
            guard command != nil || output != nil else { return nil }
            return Content(text: command ?? output ?? "", speech: "", command: command, output: output)
        default:
            return nil
        }
    }

    /// A Capcom thread row's content: the owner's and Capcom's messages, a report's text.
    public static func content(_ row: CapcomRow) -> Content? {
        switch row {
        case .user(_, let text, _, _, _):
            return text.isEmpty ? nil : Content(text: text, speech: text)
        case .capcom(_, let text, _, _), .quiet(_, _, let text):
            return message(text)
        default:
            return nil
        }
    }

    /// A message in Markdown: a reply, or a Capcom message.
    public static func message(_ markdown: String) -> Content? {
        guard !markdown.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return nil }
        return Content(text: markdown, speech: SpokenText.speakable(markdown, capWords: readAloudWords))
    }

    /// The actions a row's menu offers, in order.
    public static func actions(_ c: Content) -> [Action] {
        if c.command != nil || c.output != nil {
            return (c.command != nil ? [.copyCommand] : []) + (c.output != nil ? [.copyOutput] : [])
        }
        var out: [Action] = [.copy, .selectText, .share]
        if !c.speech.isEmpty { out.append(.readAloud) }
        if ChatMarkdown.lastCodeBlock(c.text) != nil { out.append(.copyLastCode) }
        return out
    }

    /// What a copy action puts on the clipboard; nil for an action that copies nothing.
    public static func copied(_ action: Action, _ c: Content) -> String? {
        switch action {
        case .copy: return c.text
        case .copyLastCode: return ChatMarkdown.lastCodeBlock(c.text)
        case .copyCommand: return c.command
        case .copyOutput: return c.output
        case .selectText, .share, .readAloud: return nil
        }
    }
}
