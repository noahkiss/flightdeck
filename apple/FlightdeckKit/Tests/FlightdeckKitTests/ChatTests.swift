import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

@MainActor
final class ChatTests: XCTestCase {
    private func msg(_ n: Int, _ kind: ChatKind = .agent, text: String? = nil, session: String = "s1") -> ChatMessage {
        ChatMessage(at: "2026-01-01T10:00:00Z", id: "m\(n)", kind: kind, session: session, text: text ?? "message \(n)")
    }

    /// A fleet with one fake hab whose session s1 (pane %1) runs claude in `state`.
    private func setup(state: AgentStatus = .idle, messages: Int = 0) async -> (Fleet, FakeHab, ChatModel) {
        let hab = await sampleHab()
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: state))
        await hab.setChat(session: "s1", (1...max(1, messages)).prefix(messages).map { msg($0) })
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.first?.state.status == .online }
        let model = fleet.chat(for: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        model.pageSize = 10
        model.readBackWindow = .milliseconds(300)
        return (fleet, hab, model)
    }

    func testTailOlderPagesLiveAppendAndChangeInPlace() async {
        let (fleet, hab, model) = await setup(messages: 25)
        model.start()
        await eventually("tail") { model.load == .ready }
        XCTAssertEqual(model.messages.map(\.id), (16...25).map { "m\($0)" })
        XCTAssertTrue(model.hasOlder)
        XCTAssertEqual(model.context.hostLabel, "bench")
        XCTAssertEqual(model.context.harness, "claude")

        await model.loadOlder()
        XCTAssertEqual(model.messages.first?.id, "m6")
        await model.loadOlder()
        XCTAssertEqual(model.messages.first?.id, "m1")
        XCTAssertFalse(model.hasOlder)
        XCTAssertEqual(model.messages.count, 25)

        await hab.putChat(msg(26, .tool))
        await eventually("appended") { model.messages.last?.id == "m26" }
        var changed = msg(26, .tool)
        changed.tool = ChatTool(name: "Bash", status: .done, target: "ls")
        await hab.putChat(changed)
        await eventually("changed in place") { model.messages.last?.tool?.status == .done }
        XCTAssertEqual(model.messages.count, 26)
        // Another session's messages stay out.
        await hab.putChat(msg(99, session: "s2"))
        await hab.putChat(msg(27))
        await eventually("next") { model.messages.last?.id == "m27" }
        XCTAssertFalse(model.messages.contains { $0.id == "m99" })
        await fleet.setActive(false)
    }

    func testWaitingThenFirstMessageArrives() async {
        let (fleet, hab, model) = await setup(messages: 0)
        model.start()
        await eventually("waiting") { model.load == .waiting }
        await hab.putChat(msg(1, .user, text: "hello"))
        await eventually("ready") { model.load == .ready && model.messages.count == 1 }
        await fleet.setActive(false)
    }

    func testNoReaderIsUnavailable() async {
        let (fleet, _, _) = await setup()
        let model = fleet.chat(for: SessionTarget(habID: "h", session: "s2", pane: "%2"))
        model.start()
        await eventually("unavailable") { if case .unavailable = model.load { return true }; return false }
        XCTAssertEqual(model.gate, .noAgent)
        await fleet.setActive(false)
    }

    func testResetReloadsTail() async {
        let (fleet, hab, model) = await setup(messages: 3)
        model.start()
        await eventually("tail") { model.messages.count == 3 }
        await hab.resetChat(session: "s1", [msg(7), msg(8)])
        await eventually("reloaded") { model.messages.map(\.id) == ["m7", "m8"] }
        await fleet.setActive(false)
    }

    func testReconnectCatchesUpFromCursor() async {
        let (fleet, hab, model) = await setup(messages: 3)
        model.start()
        await eventually("tail") { model.messages.count == 3 }
        let pagesBefore = await hab.chatPages
        let conn = fleet.connections[0]
        (conn.transport as! FakeTransport).setRefusing(true)
        await hab.dropConnections()
        await eventually("not online") { !model.context.online }
        XCTAssertEqual(model.gate, .offline)
        await hab.putChat(msg(4))
        var m2 = msg(2)
        m2.text = "changed while away"
        await hab.putChat(m2)
        (conn.transport as! FakeTransport).setRefusing(false)
        await conn.retryNow()
        await eventually("caught up") { model.messages.count == 4 && model.messages[1].text == "changed while away" }
        let pagesAfter = await hab.chatPages
        XCTAssertEqual(pagesAfter, pagesBefore + 1, "one after-cursor page, no tail reload")
        await fleet.setActive(false)
    }

    func testSendFallsBackToPaneInputWithBracketedPasteThenEnter() async {
        let (fleet, hab, model) = await setup(state: .idle)
        model.start()
        model.send("  two\nlines  ")
        await eventually("typed") { await hab.inputs.count == 2 }
        let inputs = await hab.inputs
        XCTAssertEqual(inputs.map(\.pane), ["%1", "%1"])
        XCTAssertEqual(inputs[0].data, Data("\u{1b}[200~two\nlines\u{1b}[201~".utf8))
        XCTAssertEqual(inputs[1].data, Data("\r".utf8))
        XCTAssertEqual(model.outgoing.first?.status, .sent)
        // The transcript's user record clears the outgoing message.
        await hab.putChat(msg(1, .user, text: "two\nlines"))
        await eventually("cleared") { model.outgoing.isEmpty }
        await fleet.setActive(false)
    }

    func testSendUsesSessionSendWhenBuilt() async {
        let (fleet, hab, model) = await setup(state: .done)
        await hab.setSend(works: true)
        model.start()
        model.send("hi")
        await eventually("sent") { await hab.sends.count == 1 }
        let sends = await hab.sends
        XCTAssertEqual(sends[0].text, "hi")
        XCTAssertEqual(sends[0].origin, .typed)
        let inputs = await hab.inputs
        XCTAssertTrue(inputs.isEmpty)
        await fleet.setActive(false)
    }

    func testWorkingQueuesUntilTurnEndsAndCancel() async {
        let (fleet, hab, model) = await setup(state: .working)
        model.start()
        XCTAssertEqual(model.gate, .afterTurn)
        model.send("first")
        model.send("second")
        XCTAssertEqual(model.outgoing.map(\.status), [.queued, .queued])
        model.cancel(model.outgoing[1].id)
        XCTAssertEqual(model.outgoing.count, 1)
        try? await Task.sleep(for: .milliseconds(100))
        let none = await hab.inputs
        XCTAssertTrue(none.isEmpty)
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle))
        await eventually("delivered after the turn") { await hab.inputs.count == 2 }
        await fleet.setActive(false)
    }

    func testNotDeliveredAfterReadBackWindowAndManualRetry() async {
        let (fleet, hab, model) = await setup(state: .idle)
        model.start()
        model.send("anyone there")
        await eventually("not delivered") { model.outgoing.first?.status == .notDelivered }
        try? await Task.sleep(for: .milliseconds(200))
        let n = await hab.inputs.count
        XCTAssertEqual(n, 2, "no retry on its own")
        model.retry(model.outgoing[0].id)
        await eventually("retried") { await hab.inputs.count == 4 }
        // A working signal confirms delivery.
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .working))
        await eventually("delivered") { model.outgoing.first?.status == .delivered }
        await fleet.setActive(false)
    }

    func testBlockedTurnsSendOff() async {
        let (fleet, _, model) = await setup(state: .blocked)
        XCTAssertEqual(model.gate, .blocked)
        model.send("ignored")
        XCTAssertTrue(model.outgoing.isEmpty)
        await fleet.setActive(false)
    }

    func testStopFallsBackToEscape() async {
        let (fleet, hab, model) = await setup(state: .working)
        await model.interrupt()
        let inputs = await hab.inputs
        XCTAssertEqual(inputs.last?.text, "\u{1b}")
        await fleet.setActive(false)
    }

    func testMarkdownBlocks() {
        let md = "# Title\n\nSome *text*\nmore.\n\n- one\n  - nested\n1. first\n- [x] done\n\n```swift\nlet x = 1\n```\n> quoted\n---"
        XCTAssertEqual(MarkdownBlock.parse(md), [
            .heading(level: 1, text: "Title"),
            .paragraph("Some *text*\nmore."),
            .item(marker: "•", level: 0, text: "one"),
            .item(marker: "•", level: 1, text: "nested"),
            .item(marker: "1.", level: 0, text: "first"),
            .item(marker: "☑", level: 0, text: "done"),
            .code(language: "swift", text: "let x = 1"),
            .quote("quoted"),
            .rule,
        ])
    }

    func testDemoServesChatAndAnswersSend() async {
        let fleet = Fleet()
        await DemoFleet.populate(fleet, script: false)
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.allSatisfy { $0.state.status == .online } }
        let model = fleet.chat(for: SessionTarget(habID: "demo-workbench", session: "s3", pane: "%3"))
        model.start()
        await eventually("demo chat") { model.messages.count == 6 }
        model.send("Thanks")
        await eventually("demo reply", timeout: 8) { model.messages.last?.kind == .agent && model.messages.count == 8 }
        XCTAssertTrue(model.outgoing.isEmpty)
        await fleet.setActive(false)
    }
}
