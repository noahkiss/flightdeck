import FlightdeckShared
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Live checks against a real gff-hab. Skipped unless the environment names one:
///
/// - `FLIGHTDECK_LIVE_SOCKET`: a hab's socket (`gff-hab serve -socket <path>`).
/// - `FLIGHTDECK_LIVE_CHAT_SESSION` with `FLIGHTDECK_LIVE_SOCKET`: a hab session that runs
///   Claude Code with the hab's hooks. The test sends `FLIGHTDECK_LIVE_CHAT_TEXT` (default: a
///   one-word request) through the chat model and waits for the reply in the chat.
/// - `FLIGHTDECK_LIVE_SSH_HOST`: an ssh destination that runs `gff-hab connect`.
///   `FLIGHTDECK_LIVE_SSH_COMMAND` (words split on spaces) and
///   `FLIGHTDECK_LIVE_SSH_OPTIONS` (the same) adjust the command and options.
final class LiveTests: XCTestCase {
    private var env: [String: String] { ProcessInfo.processInfo.environment }

    func testLiveUnixSocket() async throws {
        guard let path = env["FLIGHTDECK_LIVE_SOCKET"], !path.isEmpty else {
            throw XCTSkip("FLIGHTDECK_LIVE_SOCKET not set")
        }
        try await check(UnixSocketTransport(path: path))
    }

    /// Push registration against a real gff-hab with push config (any APNs key
    /// works: nothing is sent until an agent blocks). The welcome carries
    /// `push`, `push.register` stores the token, and a second registration
    /// from the same device replaces it. Set `FLIGHTDECK_LIVE_PUSH=1`.
    @MainActor
    func testLivePushRegister() async throws {
        guard let path = env["FLIGHTDECK_LIVE_SOCKET"], !path.isEmpty, env["FLIGHTDECK_LIVE_PUSH"] == "1" else {
            throw XCTSkip("FLIGHTDECK_LIVE_SOCKET and FLIGHTDECK_LIVE_PUSH=1 not set")
        }
        let suite = "flightdeck.live.push"
        let d = UserDefaults(suiteName: suite)!
        d.removePersistentDomain(forName: suite)
        let fleet = Fleet()
        let reg = PushRegistrar(defaults: d, environment: .development)
        fleet.push = reg
        fleet.add(HabConnection(id: "live", transport: UnixSocketTransport(path: path), clientLabel: "live-test"), label: "live")
        reg.setDeviceToken(Data((0..<32).map { UInt8($0) }))
        await fleet.setActive(true)
        await eventually("registered", timeout: 15) { reg.status["live"] == .registered }
        reg.setDeviceToken(Data((0..<32).map { UInt8(255 - $0) }))
        await eventually("registered again", timeout: 15) { reg.status["live"] == .registered }
        await reg.settle()
        await fleet.setActive(false)
        // A banner answer on its own connection reaches the hab, which refuses
        // a request that is not open: the app reads it as a hab answer, not
        // as "could not reach".
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in UnixSocketTransport(path: path) }, clientLabel: "live-test")
        let o = await answerer.answer(host: "live", ApprovalAnswerParams(choice: .allow, pane: "00000000-0000-0000-0000-000000000000", request: "none", via: .banner))
        switch o {
        case .refused, .failed: break
        default: XCTFail("answer to a closed request: \(o)")
        }
    }

    /// Usage from a real gff-hab (app.md 14). Set `FLIGHTDECK_LIVE_USAGE` to a
    /// file path: the test creates it, and a script in a hab pane answers by
    /// posting a Claude status line with the 5h window full, then one with room
    /// (`gff-hab statusline claude`). The test sees the full window, asks to be
    /// alerted when free, and gets the alert from the `usage.updated` with room.
    /// `apple/scripts/usage-live-check.sh` sets all of this up and runs the test.
    @MainActor
    func testLiveUsage() async throws {
        guard let path = env["FLIGHTDECK_LIVE_SOCKET"], !path.isEmpty,
              let trigger = env["FLIGHTDECK_LIVE_USAGE"], !trigger.isEmpty else {
            throw XCTSkip("FLIGHTDECK_LIVE_SOCKET and FLIGHTDECK_LIVE_USAGE not set")
        }
        let suite = "flightdeck.live.usage"
        let d = UserDefaults(suiteName: suite)!
        d.removePersistentDomain(forName: suite)
        defer { d.removePersistentDomain(forName: suite) }
        let n = RecordingNotifier()
        let fleet = Fleet()
        let usage = UsageCenter(notifier: n, defaults: d)
        fleet.usage = usage
        fleet.add(HabConnection(id: "live", transport: UnixSocketTransport(path: path), clientLabel: "live-test"), label: "live")
        await fleet.setActive(true)
        await eventually("online", timeout: 20) { fleet.board.habs.first?.state.status == .online }
        FileManager.default.createFile(atPath: trigger, contents: Data())
        let full = { usage.book.accounts.values.first { a in a.windows.contains { $0.name == "5h" && UsageMath.isFull($0) } } }
        await eventually("a full 5h window", timeout: 20) { full() != nil }
        let a = try XCTUnwrap(full())
        let w = try XCTUnwrap(a.windows.first { $0.name == "5h" })
        XCTAssertTrue(n.posts.contains { $0.id == UsageAlerts.warnID(a.key, "5h") }, "a warning at the limit")
        XCTAssertTrue(usage.alerts.canAlertWhenFree(w))
        usage.alerts.alertWhenFree(a, w)
        XCTAssertEqual(n.posts.last?.at, w.resetsAt)
        await eventually("room again", timeout: 30) { n.posts.last.map { $0.id == UsageAlerts.freeID(a.key, "5h") && $0.at == nil } ?? false }
        XCTAssertFalse(usage.alerts.isWaiting(account: a.key, window: "5h"))
        XCTAssertNotNil(UsageSnapshot.load(d)?.accounts.first { $0.key == a.key })
        await fleet.setActive(false)
    }

    func testLiveSSH() async throws {
        guard let host = env["FLIGHTDECK_LIVE_SSH_HOST"], !host.isEmpty else {
            throw XCTSkip("FLIGHTDECK_LIVE_SSH_HOST not set")
        }
        let words = { (k: String) in (self.env[k] ?? "").split(separator: " ").map(String.init) }
        try await check(SSHTransport(host: host, remoteCommand: words("FLIGHTDECK_LIVE_SSH_COMMAND"), options: words("FLIGHTDECK_LIVE_SSH_OPTIONS")))
    }

    /// The chat view's path against a real hab: tail page, send through the
    /// composer's rules, the user record and the agent's reply as chat.message events.
    @MainActor
    func testLiveChatSendAndReply() async throws {
        guard let path = env["FLIGHTDECK_LIVE_SOCKET"], !path.isEmpty,
              let session = env["FLIGHTDECK_LIVE_CHAT_SESSION"], !session.isEmpty else {
            throw XCTSkip("FLIGHTDECK_LIVE_SOCKET and FLIGHTDECK_LIVE_CHAT_SESSION not set")
        }
        let text = env["FLIGHTDECK_LIVE_CHAT_TEXT"] ?? "Reply with the single word pong."
        let fleet = Fleet()
        fleet.add(HabConnection(id: "live", transport: UnixSocketTransport(path: path), clientLabel: "test"), label: nil)
        await fleet.setActive(true)
        await eventually("online", timeout: 20) { fleet.board.habs.first?.state.status == .online }
        let store = try XCTUnwrap(fleet.board.habs.first?.state.store)
        let pane = try XCTUnwrap(store.sessions[session]?.pane, "session \(session) has no pane")
        let model = fleet.chat(for: SessionTarget(habID: "live", session: session, pane: pane))
        model.readBackWindow = .seconds(20)
        model.start()
        await eventually("send allowed (agent idle or done)", timeout: 60) { model.gate == .now }
        await eventually("chat loaded", timeout: 30) { model.load == .ready || model.load == .waiting }
        let before = model.messages.count
        print("live chat: gate=\(model.gate) load=\(model.load) messages=\(before) agent=\(String(describing: model.context.agent?.state))")
        model.send(text)
        await eventually("the user record", timeout: 30) {
            model.messages.dropFirst(before).contains { $0.kind == .user && $0.text?.contains(text) == true }
        }
        await eventually("the agent's reply", timeout: 120) {
            guard let u = model.messages.lastIndex(where: { $0.kind == .user && $0.text?.contains(text) == true }) else { return false }
            return model.messages[(u + 1)...].contains { $0.kind == .agent && !($0.text ?? "").isEmpty }
        }
        XCTAssertTrue(model.outgoing.isEmpty, "the transcript record clears the outgoing message")
        for m in model.messages.dropFirst(before) {
            print("live chat: \(m.kind.rawValue) \(m.tool?.name ?? "") \((m.text ?? "").prefix(80))")
        }
        print("live chat: native session \(model.context.agent?.session ?? "-")")
        await fleet.setActive(false)
    }

    /// Hello, subscribe with scope host, a ping, then a clean stop with no stream left open.
    private func check(_ t: Transport) async throws {
        let before = OpenStreams.live
        var timing = HabTiming()
        timing.pingInterval = .milliseconds(300)
        let conn = HabConnection(id: "live", transport: t, timing: timing, clientLabel: "test")
        await conn.start()
        await eventually("online", timeout: 20) { await conn.state.status == .online }
        let st = await conn.state
        XCTAssertEqual(st.status, .online, st.lastError ?? "")
        let w = try XCTUnwrap(st.welcome)
        XCTAssertEqual(w.revision, HabAPI.revision)
        XCTAssertFalse(w.epoch.isEmpty)
        XCTAssertNotNil(st.store.host)
        let pong = try await conn.call(HabMethods.habPing, PingParams())
        XCTAssertGreaterThanOrEqual(pong.seq, 0)
        print("live: \(t.endpoint) host=\(st.store.host ?? "-") epoch=\(w.epoch) revision=\(w.revision) sessions=\(st.store.sessions.count) caps=\(w.capabilities)")
        await conn.stop()
        await eventually("closed") { OpenStreams.live == before }
    }
}
