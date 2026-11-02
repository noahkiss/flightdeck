import Foundation
import HabAPI

// Capcom's action tools in the chat view (capcom.md 3.2, 3.4, 6.3; app.md 4.8). The hab's
// `hand_off` and `file_record` tools answer in text only (`cmd/gff-hab/handoff.go`), so the
// card reads the call's arguments and the lead words of the answer. An answer it cannot read
// shows as plain text on the card, never as an error.

/// Which agent kit tool a tool call is, by its name in each harness: `mcp__flightdeck__<tool>`
/// in Claude Code and Codex, `flightdeck_<tool>` in opencode.
enum KitTool {
    static let server = "flightdeck"

    static func matches(_ name: String, _ tool: String) -> Bool {
        name == "mcp__\(server)__\(tool)" || name == "\(server)_\(tool)"
    }
}

/// An agent named in a hand-off answer.
public struct HandOffAgent: Hashable, Sendable {
    /// The channel address: `label/name` on a peer, the name on the hab itself.
    public var address: String
    public var harness: String
    public var state: String
    /// The host's label; nil for the other candidates, whose answer line names no host.
    public var host: String?
}

/// A `hand_off` tool call as a hand-off card: a record of what was sent, with no buttons.
public struct HandOffCard: Hashable, Sendable {
    public enum Delivery: String, Hashable, Sendable {
        /// The call is still running.
        case sending
        /// The agent has the message.
        case delivered
        /// The message waits for the agent to be idle, or for a permission answer.
        case queued
        /// The agent's host is offline; the message waits in the outbox.
        case held
        /// The hab gave up on the message (`not_delivered`, expired).
        case failed
        /// The tool sent nothing: no agent, an ambiguous name, the caller itself.
        case notSent
        /// The tool call failed or was refused.
        case error
    }

    /// The arguments: the project and the exact text sent.
    public var project: String?
    public var message: String
    /// The agent or host the caller asked for, when it named one.
    public var named: String?
    public var host: String?

    public var delivery: Delivery
    /// Where it went, when the answer names it.
    public var target: HandOffAgent?
    /// The answer's first line: how delivery went, in the hab's words.
    public var line: String?
    /// Why this agent was picked.
    public var why: String?
    /// The other running agents on the project.
    public var others: [HandOffAgent]
    /// The channel message id and its state (`handed`, `queued`, `delivered`, ...).
    public var messageID: String?
    public var channelState: String?
    /// The whole answer, shown as is when the tool sent nothing or failed.
    public var answer: String?

    /// The card for a tool call, or nil when the call is not `hand_off`.
    public static func from(_ t: ChatTool) -> HandOffCard? {
        guard KitTool.matches(t.name, "hand_off") else { return nil }
        let args = t.input?.objectValue
        func arg(_ k: String) -> String? {
            guard let s = args?[k]?.stringValue?.trimmingCharacters(in: .whitespacesAndNewlines), !s.isEmpty else { return nil }
            return s
        }
        var c = HandOffCard(project: arg("project"), message: args?["message"]?.stringValue ?? "",
                            named: arg("agent"), host: arg("host"), delivery: .sending, others: [])
        let out = (t.output ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        switch t.status {
        case .running:
            return c
        case .error, .denied:
            c.delivery = .error
            c.answer = out.isEmpty ? (t.status == .denied ? "The tool call was denied." : "The tool call failed.") : out
            return c
        default:
            break
        }
        let lines = out.components(separatedBy: "\n")
        let first = lines.first ?? ""
        if first.hasPrefix("Delivered:") {
            c.delivery = .delivered
        } else if first.hasPrefix("Queued:") {
            c.delivery = .queued
        } else if first.hasPrefix("Held:") {
            c.delivery = .held
        } else if first.hasPrefix("Not delivered") {
            c.delivery = .failed
        } else {
            c.delivery = .notSent
            c.answer = out.isEmpty ? nil : out
            return c
        }
        c.line = first
        // "who" follows the lead word, or for Not delivered ends the line after the hab's reason,
        // which may hold parentheses of its own (cmd/gff-hab/handoff.go, TestHandOffAnswerContract).
        let who = c.delivery == .failed
            ? first.firstMatch(of: #/(\S+) \(([^,()]*), ([^,()]*), on ([^()]+)\) does not have the message\.$/#)
            : first.firstMatch(of: #/^(?:Delivered|Queued|Held): (\S+) \(([^,()]*), ([^,()]*), on ([^()]+)\)/#)
        if let m = who {
            c.target = HandOffAgent(address: String(m.1), harness: String(m.2), state: String(m.3), host: String(m.4))
        }
        for l in lines.dropFirst() {
            if let m = l.wholeMatch(of: #/Why this agent: (.*?)\.?/#) {
                c.why = String(m.1)
            } else if let m = l.wholeMatch(of: #/Also on .+: (\S+) \(([^,()]*), ([^,()]*)\)\./#) {
                c.others.append(HandOffAgent(address: String(m.1), harness: String(m.2), state: String(m.3)))
            } else if let m = l.firstMatch(of: #/^Message (\S+), state (\S+)\./#) {
                c.messageID = String(m.1)
                c.channelState = String(m.2)
            }
        }
        return c
    }

    /// The word the card's header shows for the delivery.
    public var deliveryWord: String {
        switch delivery {
        case .sending: return "Sending"
        case .delivered: return "Delivered"
        case .queued: return "Queued"
        case .held: return "Held"
        case .failed: return "Not delivered"
        case .notSent: return "Not sent"
        case .error: return "Failed"
        }
    }
}

/// A `file_record` tool call as a proposal line. Reviewing the proposal is an approval
/// (logbook.md 9), not part of this line.
public struct ProposalLine: Hashable, Sendable {
    public enum State: String, Hashable, Sendable {
        /// The call is still running.
        case filing
        /// A pending proposal waits for review.
        case proposed
        /// The hab filed the record at once, not as a proposal.
        case filed
        /// Nothing was filed: Logbook missing, a refused draft, a failed call.
        case notFiled
    }

    /// The record's type: task, finding, issue, plan or guide.
    public var type: String
    public var title: String
    public var project: String?
    public var body: String?
    public var quote: String?
    public var state: State
    /// The proposal id, when the hab answered with one.
    public var proposal: String?
    /// The whole answer, shown as is when nothing was filed or the answer is not read.
    public var answer: String?

    /// The line for a tool call, or nil when the call is not `file_record`.
    public static func from(_ t: ChatTool) -> ProposalLine? {
        guard KitTool.matches(t.name, "file_record") else { return nil }
        let args = t.input?.objectValue
        func arg(_ k: String) -> String? {
            guard let s = args?[k]?.stringValue, !s.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return nil }
            return s
        }
        var p = ProposalLine(type: arg("type") ?? "record", title: arg("title")?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "",
                             project: arg("project"), body: arg("body"), quote: arg("quote"), state: .filing)
        let out = (t.output ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        switch t.status {
        case .running:
            return p
        case .error, .denied:
            p.state = .notFiled
            p.answer = out.isEmpty ? (t.status == .denied ? "The tool call was denied." : "The tool call failed.") : out
            return p
        default:
            break
        }
        if out.hasPrefix("Proposed:") {
            p.state = .proposed
            p.proposal = out.firstMatch(of: #/review as proposal (\S+)\. Nothing is in Logbook/#).map { String($0.1) }
        } else if out.hasPrefix("The hab filed") {
            p.state = .filed
            p.answer = out
        } else {
            p.state = .notFiled
            p.answer = out.isEmpty ? nil : out
        }
        return p
    }

    /// The words after the title: what became of the record.
    public var stateText: String {
        switch state {
        case .filing: return "Filing"
        case .proposed: return "Waits for review"
        case .filed: return "Filed"
        case .notFiled: return "Not filed"
        }
    }
}
