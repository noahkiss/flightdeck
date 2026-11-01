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
