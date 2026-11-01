import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Live checks against a real gff-hab. Skipped unless the environment names one:
///
/// - `FLIGHTDECK_LIVE_SOCKET`: a hab's socket (`gff-hab serve -socket <path>`).
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
