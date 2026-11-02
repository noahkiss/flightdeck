import HabAPI
import XCTest
@testable import FlightdeckKit

/// The hand-off card and the proposal line, over hand-written tool calls in the shapes
/// `cmd/gff-hab/handoff.go` answers with.
final class CapcomCardTests: XCTestCase {
    func handOff(_ status: ToolStatus, _ output: String?, name: String = "mcp__flightdeck__hand_off",
                 input: [String: JSONValue] = ["project": .string("orbit-api"), "message": .string("Raise the timeout.")]) -> ChatTool {
        ChatTool(input: .object(input), name: name, output: output, status: status)
    }

    func testDelivered() throws {
        let out = """
        Delivered: box-b/api-claude (claude, idle, on box-b) has the message now.
        Why this agent: idle, so it takes the message now; the most recently active of 2 agents that are.
        Also on orbit-api: box-a/api-codex (codex, working).
        Also on orbit-api: lab/api-open (opencode, idle).
        Message m-1, state delivered. A reply reaches you as an agent message.

        """
        let c = try XCTUnwrap(HandOffCard.from(handOff(.done, out)))
        XCTAssertEqual(c.delivery, .delivered)
        XCTAssertEqual(c.target, HandOffAgent(address: "box-b/api-claude", harness: "claude", state: "idle", host: "box-b"))
        XCTAssertEqual(c.line, "Delivered: box-b/api-claude (claude, idle, on box-b) has the message now.")
        XCTAssertEqual(c.why, "idle, so it takes the message now; the most recently active of 2 agents that are")
        XCTAssertEqual(c.others.map(\.address), ["box-a/api-codex", "lab/api-open"])
        XCTAssertEqual(c.others.first?.state, "working")
        XCTAssertNil(c.others.first?.host)
        XCTAssertEqual(c.messageID, "m-1")
        XCTAssertEqual(c.channelState, "delivered")
        XCTAssertEqual(c.project, "orbit-api")
        XCTAssertEqual(c.message, "Raise the timeout.")
        XCTAssertNil(c.answer)
    }

    func testQueuedHeldAndFailed() throws {
        let queued = try XCTUnwrap(HandOffCard.from(handOff(.done, "Queued: api-claude (claude, blocked, on hab-1) is blocked on a permission request; it gets the message once that is answered. Point the user at the open approval in Flightdeck.\nWhy this agent: the agent the user named.\nMessage m-2, state handed. A reply reaches you as an agent message.\n")))
        XCTAssertEqual(queued.delivery, .queued)
        XCTAssertEqual(queued.target?.address, "api-claude")
        XCTAssertEqual(queued.target?.host, "hab-1")
        XCTAssertEqual(queued.channelState, "handed")
        XCTAssertEqual(queued.deliveryWord, "Queued")

        let held = try XCTUnwrap(HandOffCard.from(handOff(.done, "Held: lab/api-claude (claude, idle, on lab) is offline. The message waits in this hab's outbox and goes when the host is back, for up to 7 days.\nWhy this agent: every agent on the project is on an offline host; the most recently active.\nMessage m-3, state queued. A reply reaches you as an agent message.\n")))
        XCTAssertEqual(held.delivery, .held)
        XCTAssertEqual(held.target?.host, "lab")

        let failed = try XCTUnwrap(HandOffCard.from(handOff(.done, "Not delivered (not_delivered): the pane closed. box-b/api-claude (claude, idle, on box-b) does not have the message.\nWhy this agent: the only agent running on the project.\nMessage m-4, state not_delivered. A reply reaches you as an agent message.\n")))
        XCTAssertEqual(failed.delivery, .failed)
        XCTAssertEqual(failed.target?.address, "box-b/api-claude")
        XCTAssertEqual(failed.why, "the only agent running on the project")
        XCTAssertEqual(failed.deliveryWord, "Not delivered")
    }

    func testFailedReasonWithParenthesesKeepsTheTarget() throws {
        let c = try XCTUnwrap(HandOffCard.from(handOff(.done, "Not delivered (not_delivered): pane of x (a, b, on c) closed. box-b/api-claude (claude, idle, on box-b) does not have the message.\nMessage m-5, state not_delivered. A reply reaches you as an agent message.\n")))
        XCTAssertEqual(c.delivery, .failed)
        XCTAssertEqual(c.target, HandOffAgent(address: "box-b/api-claude", harness: "claude", state: "idle", host: "box-b"))
    }

    func testProposalIDNotTakenFromTheTitle() throws {
        let t = ChatTool(input: .object(["type": .string("task"), "title": .string("Rename as proposal X. later"), "quote": .string("q")]),
                         name: "mcp__flightdeck__file_record",
                         output: "Proposed: the task \"Rename as proposal X. later\" waits for the user's review as proposal p-9. Nothing is in Logbook until the user approves it. Tell the user in one line; do not file it again.\n",
                         status: .done)
        XCTAssertEqual(ProposalLine.from(t)?.proposal, "p-9")
    }

    func testNothingSentShowsTheAnswer() throws {
        let out = "No agent named api-x is running on orbit-api: nothing was sent. Running there:\n  box-a/api-codex: codex, working, since 2026-02-01T09:00:00Z\n"
        let c = try XCTUnwrap(HandOffCard.from(handOff(.done, out, input: ["project": .string("orbit-api"), "agent": .string("api-x"), "message": .string("hi")])))
        XCTAssertEqual(c.delivery, .notSent)
        XCTAssertEqual(c.answer, out.trimmingCharacters(in: .whitespacesAndNewlines))
        XCTAssertNil(c.target)
        XCTAssertEqual(c.named, "api-x")
        XCTAssertTrue(c.others.isEmpty)
    }

    func testErrorRunningAndDenied() throws {
        let err = try XCTUnwrap(HandOffCard.from(handOff(.error, "hand_off needs message: the request to send")))
        XCTAssertEqual(err.delivery, .error)
        XCTAssertEqual(err.answer, "hand_off needs message: the request to send")

        let running = try XCTUnwrap(HandOffCard.from(handOff(.running, nil)))
        XCTAssertEqual(running.delivery, .sending)
        XCTAssertNil(running.answer)
        XCTAssertEqual(running.message, "Raise the timeout.")

        let denied = try XCTUnwrap(HandOffCard.from(handOff(.denied, nil)))
        XCTAssertEqual(denied.delivery, .error)
        XCTAssertEqual(denied.answer, "The tool call was denied.")
    }

    func testNamesInEachHarness() {
        XCTAssertNotNil(HandOffCard.from(handOff(.running, nil, name: "flightdeck_hand_off")))
        XCTAssertNil(HandOffCard.from(handOff(.running, nil, name: "hand_off")))
        XCTAssertNil(HandOffCard.from(handOff(.running, nil, name: "mcp__other__hand_off")))
        XCTAssertNil(ProposalLine.from(handOff(.running, nil)))
    }

    func record(_ status: ToolStatus, _ output: String?) -> ChatTool {
        ChatTool(input: .object(["type": .string("task"), "title": .string(" Make the timeout a setting "), "project": .string("orbit-api"),
                                 "quote": .string("the timeout should be a setting"), "body": .string("A constant today.")]),
                 name: "mcp__flightdeck__file_record", output: output, status: status)
    }

    func testProposalLine() throws {
        let p = try XCTUnwrap(ProposalLine.from(record(.done, "Proposed: the task \"Make the timeout a setting\" waits for the user's review as proposal p-7. Nothing is in Logbook until the user approves it. Tell the user in one line; do not file it again.\n")))
        XCTAssertEqual(p.state, .proposed)
        XCTAssertEqual(p.proposal, "p-7")
        XCTAssertEqual(p.title, "Make the timeout a setting")
        XCTAssertEqual(p.type, "task")
        XCTAssertEqual(p.project, "orbit-api")
        XCTAssertEqual(p.quote, "the timeout should be a setting")
        XCTAssertEqual(p.stateText, "Waits for review")
        XCTAssertNil(p.answer)

        let filed = try XCTUnwrap(ProposalLine.from(record(.done, "The hab filed the task \"Make the timeout a setting\" at once (state applied), not as a proposal: r-1. Tell the user it is in Logbook already.\n")))
        XCTAssertEqual(filed.state, .filed)
        XCTAssertNotNil(filed.answer)

        let missing = try XCTUnwrap(ProposalLine.from(record(.error, "Logbook is not on this host yet, so the record was not filed and nothing was written. Tell the user, and keep the draft in this conversation")))
        XCTAssertEqual(missing.state, .notFiled)
        XCTAssertTrue(missing.answer?.hasPrefix("Logbook is not on this host yet") ?? false)

        XCTAssertEqual(ProposalLine.from(record(.running, nil))?.state, .filing)
    }

    func testRowsFromMessages() {
        let h = ChatMessage(at: "t", id: "h", kind: .tool, session: "s", tool: handOff(.running, nil))
        let f = ChatMessage(at: "t", id: "f", kind: .tool, session: "s", tool: record(.running, nil))
        let other = ChatMessage(at: "t", id: "o", kind: .tool, session: "s", tool: ChatTool(name: "mcp__flightdeck__projects", status: .done))
        let items = ChatRender.items([h, f, other])
        guard case .handOff("h", _) = items[0], case .proposal("f", _) = items[1], case .tool("o", _) = items[2] else {
            return XCTFail("rows: \(items)")
        }
    }
}
