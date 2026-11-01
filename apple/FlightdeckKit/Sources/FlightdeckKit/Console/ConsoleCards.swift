import Foundation
import HabAPI

/// One command card in the console (app.md 6.1): the command the hab found
/// by its prompt marks, and the output once the owner opened it.
public struct ConsoleCard: Identifiable, Equatable, Sendable {
    public var command: Command
    /// The output's plain lines; nil until the card was opened and read.
    public var output: [String]?
    /// True when the hab's ring no longer held the start of the output, or
    /// the card read only the output's last part.
    public var outputCut = false

    public var id: UInt64 { command.id }

    public init(command: Command, output: [String]? = nil) {
        self.command = command
        self.output = output
    }

    public var started: Date? { command.startedAt.flatMap(ConsoleTime.parse) }
    public var ended: Date? { command.endedAt.flatMap(ConsoleTime.parse) }

    /// Seconds from start to end; nil while it runs or when a time is unknown.
    public var duration: TimeInterval? {
        guard let s = started, let e = ended else { return nil }
        return max(0, e.timeIntervalSince(s))
    }

    /// The command line, or a stand-in when the shell sent none (bash
    /// without more setup).
    public var title: String {
        if let t = command.text, !t.isEmpty { return command.textTruncated == true ? t + "…" : t }
        return "Command"
    }

    /// The status line: "Exit 0", "Exit 1", "Running", "Ended" when no
    /// status came (Ctrl+C mid-output, a crashed shell).
    public var status: String {
        switch command.state {
        case .running: return "Running"
        case .done: return command.exit.map { "Exit \($0)" } ?? "Done"
        default: return "Ended"
        }
    }

    public var failed: Bool {
        if let x = command.exit { return x != 0 }
        return command.state == .unknown
    }
}

/// The console's card list: commands by id, oldest first, from
/// `command.list` pages and `command.updated` events.
public struct ConsoleCardList: Equatable, Sendable {
    public private(set) var cards: [ConsoleCard] = []
    /// True when the hab holds older commands than the first card.
    public var more = false

    public init() {}

    /// Merges a page or an event's commands. A command already held is
    /// replaced (its output, when read, is kept until it ends again);
    /// a new one goes in its place by id.
    public mutating func merge(_ commands: [Command]) {
        for c in commands {
            if let i = cards.firstIndex(where: { $0.id == c.id }) {
                let old = cards[i]
                var card = ConsoleCard(command: c, output: old.output)
                card.outputCut = old.outputCut
                if old.command.state != c.state || old.command.end != c.end {
                    // The command went on or ended: what was read is stale.
                    card.output = nil
                    card.outputCut = false
                }
                cards[i] = card
            } else {
                let i = cards.firstIndex(where: { $0.id > c.id }) ?? cards.count
                cards.insert(ConsoleCard(command: c), at: i)
            }
        }
        // The hab keeps at most 1000 commands per pane; so does the list.
        if cards.count > 1000 { cards.removeFirst(cards.count - 1000) }
    }

    /// Replaces the list with a fresh newest page, keeping what was read
    /// for commands that did not change.
    public mutating func replace(_ page: CommandList) {
        var next = ConsoleCardList()
        next.cards = cards.filter { held in page.commands.contains { $0.id == held.id } }
        next.merge(page.commands)
        next.cards.removeAll { held in !page.commands.contains { $0.id == held.id } }
        next.more = page.more
        self = next
    }

    public mutating func setOutput(_ id: UInt64, lines: [String], cut: Bool) {
        guard let i = cards.firstIndex(where: { $0.id == id }) else { return }
        cards[i].output = lines
        cards[i].outputCut = cut
    }

    public func card(_ id: UInt64) -> ConsoleCard? { cards.first { $0.id == id } }
}

enum ConsoleTime {
    static func parse(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }

    /// "0.12s", "2.0s", "3m 12s", "1h 4m" (app.md 15: "Exit 0 · 3m 12s").
    static func duration(_ t: TimeInterval) -> String {
        if t < 10 { return String(format: "%.1fs", (t * 10).rounded(.down) / 10) }
        if t < 60 { return "\(Int(t))s" }
        let s = Int(t)
        if s < 3600 { return "\(s / 60)m \(s % 60)s" }
        return "\(s / 3600)h \((s % 3600) / 60)m"
    }
}

/// Plain text of a card list, for snapshot tests and for reading a console
/// without the view.
public enum ConsoleRender {
    /// One block per card: the command line and its folder, the status and
    /// duration, then the output lines (when read), indented.
    public static func text(_ list: ConsoleCardList) -> String {
        var out: [String] = []
        if list.more { out.append("(older commands)") }
        for c in list.cards {
            var head = "$ \(c.title)"
            if let d = c.command.cwd { head += "  [\(d)]" }
            out.append(head)
            var status = c.status
            if let d = c.duration { status += " · " + ConsoleTime.duration(d) }
            out.append("  " + status)
            if let lines = c.output {
                if c.outputCut { out.append("  | …") }
                if lines.isEmpty { out.append("  (no output)") }
                for l in lines { out.append("  | " + l) }
            }
        }
        return out.joined(separator: "\n") + "\n"
    }
}
