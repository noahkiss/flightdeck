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

    // MARK: - Review fixes

    /// Must-fix 1: a new native session (/clear, a new agent, a resume) takes the new tail.
    func testNewNativeSessionLoadsNewTail() async {
        let (fleet, hab, model) = await setup(messages: 0)
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle, session: "native-1"))
        await hab.setChat(session: "s1", [msg(1), msg(2)])
        model.start()
        await eventually("first tail") { model.messages.map(\.id) == ["m1", "m2"] }
        // The hab now reads another file for this session; no event says so.
        await hab.setChat(session: "s1", [msg(10)])
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle, session: "native-2"))
        await eventually("new tail") { model.messages.map(\.id) == ["m10"] }
        await fleet.setActive(false)
    }

    /// Must-fix 2: the list keeps its place on a prepend, and follows new rows
    /// only from the bottom.
    func testScrollFollow() {
        typealias E = ScrollFollow.Ends
        let tail = E(first: "m16", last: "m25")
        XCTAssertEqual(ScrollFollow.action(from: E(first: nil, last: nil), to: tail, atBottom: true), .toBottom, "first load")
        XCTAssertEqual(ScrollFollow.action(from: tail, to: E(first: "m6", last: "m25"), atBottom: false), .keep("m16"), "older page")
        XCTAssertEqual(ScrollFollow.action(from: tail, to: E(first: "m6", last: "m25"), atBottom: true), .keep("m16"), "older page at the bottom")
        XCTAssertEqual(ScrollFollow.action(from: tail, to: E(first: "m16", last: "m26"), atBottom: false), .none, "reading history")
        XCTAssertEqual(ScrollFollow.action(from: tail, to: E(first: "m16", last: "m26"), atBottom: true), .toBottom, "following")
        XCTAssertEqual(ScrollFollow.action(from: tail, to: tail, atBottom: true), .none, "a change in place")
        XCTAssertEqual(ScrollFollow.action(from: tail, to: E(first: "x1", last: "x9"), atBottom: false), .toBottom, "a new conversation")
    }

    /// The seq rule during a tail load: an event older than the page is dropped,
    /// a newer one applies over it.
    func testTailLoadSeqRule() async {
        let (fleet, hab, model) = await setup(messages: 2)
        var stale = msg(1)
        stale.text = "stale"
        var fresh = msg(2)
        fresh.text = "newer than the page"
        let (staleMsg, freshMsg) = (stale, fresh)
        let current = [msg(1, text: "current"), msg(2)]
        await hab.setChatPageHook { [weak hab] p, phase in
            guard let hab, p.before == nil, p.after == nil else { return }
            switch phase {
            case .before:
                // Sent before the page is read, then superseded without an event.
                await hab.putChat(staleMsg)
                await hab.setChat(session: "s1", current)
            case .after:
                await hab.putChat(freshMsg)
            }
        }
        model.start()
        await eventually("tail") { model.load == .ready && model.messages.count == 2 }
        await eventually("the newer event") { model.messages[1].text == "newer than the page" }
        XCTAssertEqual(model.messages[0].text, "current", "the event older than the page is dropped")
        await fleet.setActive(false)
    }

    /// Should-fix 3: catch-up pages follow the seq rule too.
    func testCatchUpSeqRule() async {
        let (fleet, hab, model) = await setup(messages: 2)
        model.start()
        await eventually("tail") { model.messages.count == 2 }
        let conn = fleet.connections[0]
        (conn.transport as! FakeTransport).setRefusing(true)
        await hab.dropConnections()
        await eventually("not online") { !model.context.online }
        var running = msg(3, .tool)
        running.tool = ChatTool(name: "Bash", status: .running, target: "make")
        await hab.putChat(running)
        var done = running
        done.tool?.status = .done
        let doneMsg = done
        await hab.setChatPageHook { [weak hab] p, phase in
            guard let hab, p.after != nil, phase == .after else { return }
            // The call finished after the page was read; its event reaches the app first.
            await hab.putChat(doneMsg)
        }
        (conn.transport as! FakeTransport).setRefusing(false)
        await conn.retryNow()
        await eventually("caught up") { model.messages.count == 3 }
        try? await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(model.messages[2].tool?.status, .done, "the page does not overwrite the newer event")
        await fleet.setActive(false)
    }

    /// Should-fix 4: an older page that returns after a reset is dropped.
    func testOlderPageAfterResetIsDropped() async {
        let (fleet, hab, model) = await setup(messages: 25)
        model.start()
        await eventually("tail") { model.messages.count == 10 }
        let replaced = [msg(100), msg(101)]
        await hab.setChatPageHook { [weak hab] p, phase in
            guard let hab, p.before != nil, phase == .after else { return }
            await hab.resetChat(session: "s1", replaced)
        }
        await model.loadOlder()
        await eventually("new tail") { model.messages.map(\.id) == ["m100", "m101"] }
        try? await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(model.messages.map(\.id), ["m100", "m101"])
        await fleet.setActive(false)
    }

    /// Should-fix 8: two views share a model; one leaving keeps it live.
    func testViewersAreCounted() async {
        let (fleet, hab, model) = await setup(messages: 1)
        model.start()
        model.start()
        await eventually("tail") { model.messages.count == 1 }
        model.stop()
        await hab.putChat(msg(2))
        await eventually("still live") { model.messages.count == 2 }
        model.stop()
        model.stop()
        await fleet.setActive(false)
    }

    /// Should-fix 9: a user record that arrives through a tail reload clears its outgoing message.
    func testTailReloadClearsOutgoing() async {
        let (fleet, hab, model) = await setup(state: .idle, messages: 1)
        model.readBackWindow = .seconds(5)
        model.start()
        await eventually("tail") { model.messages.count == 1 }
        model.send("hello there")
        await eventually("sent") { model.outgoing.first?.status == .sent }
        await hab.resetChat(session: "s1", [msg(1), msg(2, .user, text: "hello there")])
        await eventually("cleared") { model.outgoing.isEmpty }
        await fleet.setActive(false)
    }

    func testTypedInputStripsEscape() {
        let w = ChatModel.typedInput("a\u{1b}[201~b")
        XCTAssertEqual(w[0], Data("\u{1b}[200~a[201~b\u{1b}[201~".utf8))
    }

    func testRowCacheReusesUnchangedRows() {
        var cache = ChatRender.Cache()
        let ms = [msg(1, text: "# Hi"), msg(2, .user, text: "yo")]
        let first = ChatRender.items(ms, cache: &cache)
        XCTAssertEqual(cache.rows.count, 2)
        var changed = ms
        changed[0].text = "# Bye"
        let second = ChatRender.items(changed, cache: &cache)
        XCTAssertNotEqual(first[0], second[0])
        XCTAssertEqual(first[1], second[1])
        XCTAssertEqual(ChatRender.items(changed), second)
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
