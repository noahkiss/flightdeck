import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Fast timings for tests: no test waits on the real 15 s and 45 s.
///
/// The liveness window and the call timeout stay long. A loaded CI runner can stall a test
/// process for seconds; a short window then calls a healthy fake hab dead, and the call in
/// flight fails with "not online". Only a test of liveness itself uses `livenessTiming()`.
func fastTiming() -> HabTiming {
    var t = HabTiming()
    t.pingInterval = .milliseconds(100)
    t.deadAfter = .seconds(30)
    t.minBackoff = .milliseconds(20)
    t.maxBackoff = .milliseconds(80)
    t.helloTimeout = .seconds(2)
    t.callTimeout = .seconds(30)
    t.tick = .milliseconds(25)
    t.stableAfter = .milliseconds(150)
    return t
}

/// `fastTiming()` with a short liveness window and call timeout, for the tests of a hab that
/// stops answering.
func livenessTiming() -> HabTiming {
    var t = fastTiming()
    t.deadAfter = .milliseconds(600)
    t.callTimeout = .seconds(2)
    return t
}

/// Polls `cond` until it holds or `timeout` passes.
func eventually(_ what: String, timeout: TimeInterval = 5, file: StaticString = #filePath, line: UInt = #line, _ cond: () async -> Bool) async {
    let end = Date().addingTimeInterval(timeout)
    while Date() < end {
        if await cond() { return }
        try? await Task.sleep(for: .milliseconds(10))
    }
    XCTFail("timed out waiting for: \(what)", file: file, line: line)
}

/// A hab with one project, two sessions and their panes and agents.
func sampleHab(_ host: String = "testhost") async -> FakeHab {
    let hab = FakeHab(hostID: host)
    await hab.load(
        projects: [Fixture.project("p1", name: "tide-tables", sessions: ["s1", "s2"])],
        sessions: [
            Fixture.session("s1", name: "claude", pane: "%1", project: "p1"),
            Fixture.session("s2", name: "shell", pane: "%2", project: "p1"),
        ],
        panes: [Fixture.pane("%1", session: "s1"), Fixture.pane("%2", session: "s2")],
        agents: [Fixture.agent(pane: "%1", harness: "claude", state: .working)]
    )
    return hab
}

extension HabConnection {
    func currentStore() -> HabStore { state.store }
}

/// A slow uplink in front of a fake transport: the app's writes on the control stream leave
/// at `bytesPerSecond`, in pieces of at most 16 KiB, on one serial queue, as FDStream
/// writes. A piece leaves no earlier than the rate allows, so a write of n bytes takes at
/// least n / rate seconds. While stalled, no piece leaves at all; a stalled write ends only
/// when the stream closes. Bytes from the hab arrive at once.
final class SlowUplink: Transport, @unchecked Sendable {
    let inner: FakeTransport
    let bytesPerSecond: Double
    let slice = 16 << 10
    let gate = NSCondition()
    private var stalled = false

    init(_ inner: FakeTransport, bytesPerSecond: Double = 1_000_000) {
        self.inner = inner
        self.bytesPerSecond = bytesPerSecond
    }

    var endpoint: String { inner.endpoint }

    /// Stops or restarts every write on every stream.
    func setStalled(_ on: Bool) {
        gate.lock()
        stalled = on
        gate.broadcast()
        gate.unlock()
    }

    /// Waits while stalled; false once `closed` says the stream is gone.
    fileprivate func mayWrite(_ closed: () -> Bool) -> Bool {
        gate.lock()
        defer { gate.unlock() }
        while stalled && !closed() { gate.wait() }
        return !closed()
    }

    func open(_ kind: StreamKind) async throws -> ByteStream {
        let s = try await inner.open(kind)
        guard kind == .control, let f = s as? FakeStream else { return s }
        return SlowStream(f, uplink: self)
    }
}

private final class SlowStream: ByteStream, @unchecked Sendable {
    private let inner: FakeStream
    private let uplink: SlowUplink
    private let writes = DispatchQueue(label: "test.slowuplink.write")
    /// Read and written under `uplink.gate`.
    private var closed = false

    init(_ inner: FakeStream, uplink: SlowUplink) {
        self.inner = inner
        self.uplink = uplink
    }

    func read() async throws -> Data? { try await inner.read() }

    func write(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
            writes.async { [self] in
                var off = 0
                let start = Date()
                while off < data.count {
                    guard uplink.mayWrite({ closed }) else {
                        c.resume(throwing: TransportError("slow uplink: stream closed"))
                        return
                    }
                    let end = min(off + uplink.slice, data.count)
                    // The bandwidth itself, not a wait for another task: the piece leaves when
                    // the rate allows. A late wake-up shortens the next wait, never adds up.
                    Thread.sleep(until: start.addingTimeInterval(Double(end) / uplink.bytesPerSecond))
                    do {
                        try inner.send(data.subdata(in: off..<end))
                    } catch {
                        c.resume(throwing: error)
                        return
                    }
                    off = end
                }
                c.resume()
            }
        }
    }

    func close() {
        uplink.gate.lock()
        closed = true
        uplink.gate.broadcast()
        uplink.gate.unlock()
        inner.close()
    }
}
