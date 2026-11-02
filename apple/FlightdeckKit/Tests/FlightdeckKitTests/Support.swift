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
