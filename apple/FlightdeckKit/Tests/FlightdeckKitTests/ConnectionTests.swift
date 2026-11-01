import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

final class ConnectionTests: XCTestCase {
    func testConnectHelloSubscribeSnapshot() async {
        let hab = await sampleHab()
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let st = await conn.state
        XCTAssertEqual(st.store.host, "testhost")
        XCTAssertEqual(st.store.sessions.count, 2)
        XCTAssertEqual(st.store.agents["%1"]?.state, .working)
        XCTAssertEqual(st.welcome?.revision, HabAPI.revision)
        let hellos = await hab.hellos
        XCTAssertEqual(hellos.count, 1)
        XCTAssertNil(hellos[0].reconnectToken)
        XCTAssertEqual(hellos[0].client.kind, .app)
        let subs = await hab.subscribes
        XCTAssertEqual(subs, 1)
        await conn.stop()
    }

    func testEventsAfterSnapshotApply() async {
        let hab = await sampleHab()
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission,
                                    request: Fixture.request(title: "Run tests?", tool: "Bash", command: "make test")))
        await eventually("blocked") { await conn.state.store.agents["%1"]?.state == .blocked }
        await conn.stop()
    }

    /// Reconnect resumes at the last seq: the token goes back, the client
    /// subscribes again, the snapshot holds what changed in the gap, and events
    /// after it apply by the new connection's seq.
    func testReconnectSendsTokenResubscribesAndKeepsClientID() async {
        let hab = await sampleHab()
        let transport = hab.transport()
        let conn = HabConnection(id: "a", transport: transport, timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let first = await conn.state.welcome!

        // Drop the link and change state while the app cannot connect.
        transport.setRefusing(true)
        await hab.dropConnections()
        await eventually("connecting") { await conn.state.status != .online }
        var s1 = await hab.session("s1")!
        s1.title = "Changed in the gap"
        await hab.put(s1)
        transport.setRefusing(false)
        await conn.retryNow()

        await eventually("online again") { let s = await conn.state; return s.status == .online && s.snapshots == 2 }
        let hellos = await hab.hellos
        XCTAssertEqual(hellos.count, 2)
        XCTAssertEqual(hellos[1].reconnectToken, first.reconnectToken)
        let st = await conn.state
        XCTAssertTrue(st.resumed)
        XCTAssertEqual(st.welcome?.clientId, first.clientId)
        XCTAssertNotEqual(st.welcome?.reconnectToken, first.reconnectToken)
        XCTAssertEqual(st.epochChanges, 0)
        XCTAssertEqual(st.store.sessions["s1"]?.title, "Changed in the gap")
        let subs = await hab.subscribes
        XCTAssertEqual(subs, 2)

        // Seq restarts at 1 on the new connection; events still apply.
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .done))
        await eventually("done") { await conn.state.store.agents["%1"]?.state == .done }
        await conn.stop()
    }

    /// The first retry after a lost connection goes at once.
    func testFirstRetryIsImmediate() async {
        let hab = await sampleHab()
        var t = fastTiming()
        t.minBackoff = .seconds(5)
        t.maxBackoff = .seconds(5)
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: t)
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        try? await Task.sleep(for: .milliseconds(300)) // past stableAfter
        await hab.dropConnections()
        // With a 5 s backoff, only an immediate retry is back within 2 s.
        await eventually("back online", timeout: 2) { let s = await conn.state; return s.status == .online && s.snapshots == 2 }
        await conn.stop()
    }

    func testNewEpochReplacesEveryObject() async {
        let hab = await sampleHab()
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        // Raise s1's revision on the client side well above 1.
        for i in 0..<5 {
            var s = await hab.session("s1")!
            s.title = "t\(i)"
            await hab.put(s)
        }
        await eventually("rev 6") { await conn.state.store.sessions["s1"]?.revision == 6 }
        let firstClient = await conn.state.welcome!.clientId

        // The hab restarts with lost revisions and a session removed.
        await hab.deleteSession("s2")
        await hab.restart(lowerRevisions: true)
        await eventually("new epoch online") { let s = await conn.state; return s.status == .online && s.epochChanges == 1 }
        let st = await conn.state
        XCTAssertFalse(st.resumed)
        XCTAssertNotEqual(st.welcome?.clientId, firstClient)
        XCTAssertEqual(st.store.sessions["s1"]?.revision, 1, "a new epoch takes lower revisions")
        XCTAssertNil(st.store.sessions["s2"])

        // Revision 2 is not above the old 6, but it is above what this epoch holds.
        var s = await hab.session("s1")!
        s.title = "after restart"
        await hab.put(s)
        await eventually("applied") { await conn.state.store.sessions["s1"]?.title == "after restart" }
        await conn.stop()
    }

    func testStaleRevisionIgnoredWithinEpoch() async {
        let hab = await sampleHab()
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        var s = await hab.session("s1")!
        s.title = "new"
        await hab.put(s) // revision 2
        await eventually("rev 2") { await conn.state.store.sessions["s1"]?.revision == 2 }
        var stale = s
        stale.title = "stale"
        stale.revision = 2
        await hab.emit(type: "session.updated", id: "s1", revision: 2, data: stale)
        var newer = s
        newer.title = "newer"
        newer.revision = 3
        await hab.emit(type: "session.updated", id: "s1", revision: 3, data: newer)
        await eventually("rev 3") { await conn.state.store.sessions["s1"]?.revision == 3 }
        let title = await conn.state.store.sessions["s1"]?.title
        XCTAssertEqual(title, "newer")
        await conn.stop()
    }

    func testSeqGapReconnects() async {
        let hab = await sampleHab()
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        await hab.skipSeq()
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle))
        await eventually("resubscribed") { await hab.subscribes == 2 }
        await eventually("online") { await conn.state.status == .online }
        let st = await conn.state.store.agents["%1"]?.state
        XCTAssertEqual(st, .idle, "the fresh snapshot carries the change")
        await conn.stop()
    }

    func testHeartbeatAheadOfSeqIsAGap() async {
        let hab = await sampleHab()
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        await hab.skipSeq()
        await hab.sendHeartbeat()
        await eventually("resubscribed") { await hab.subscribes == 2 }
        await conn.stop()
    }

    func testPingsWhenIdleAndDeadAfterSilence() async {
        let hab = await sampleHab()
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        await eventually("pinged") { await hab.pings >= 2 }
        // The hab stops answering: nothing arrives, so the app calls it dead and reconnects.
        await hab.setSilent(true)
        await eventually("not online") { await conn.state.status != .online }
        let err = await conn.state.lastError ?? ""
        XCTAssertTrue(err.contains("dead") || err.contains("timed out"), err)
        await hab.setSilent(false)
        await eventually("online again") { await conn.state.status == .online }
        await conn.stop()
    }

    func testHeartbeatsKeepASilentClientAlive() async {
        // Only heartbeats arrive (pings never answered would also be fine):
        // the connection must stay up past deadAfter.
        let hab = await sampleHab()
        await hab.setHeartbeat(.milliseconds(100))
        var t = fastTiming()
        t.pingInterval = .seconds(60)
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: t)
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        try? await Task.sleep(for: .milliseconds(1200))
        let st = await conn.state
        XCTAssertEqual(st.status, .online)
        XCTAssertEqual(st.snapshots, 1)
        await conn.stop()
    }

    func testOfflineAfterThreeFailuresThenRecovers() async {
        let hab = await sampleHab()
        let transport = hab.transport()
        transport.setRefusing(true)
        let conn = HabConnection(id: "a", transport: transport, timing: fastTiming())
        await conn.start()
        await eventually("offline") { await conn.state.status == .offline }
        XCTAssertGreaterThanOrEqual(transport.openAttempts, 3)
        let lost = await conn.state.lostAt
        XCTAssertNotNil(lost)
        transport.setRefusing(false)
        await eventually("online") { await conn.state.status == .online }
        await conn.stop()
    }

    func testUpdateNeededWhenHabIsNewer() async {
        let hab = await sampleHab()
        await hab.setRevisions((HabAPI.revision + 1)...(HabAPI.revision + 2))
        let transport = hab.transport()
        let conn = HabConnection(id: "a", transport: transport, timing: fastTiming())
        await conn.start()
        await eventually("update needed") { await conn.state.status == .updateNeeded(.app) }
        let attempts = transport.openAttempts
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(transport.openAttempts, attempts, "no retry after a revision mismatch")
        await conn.stop()
    }

    func testUpdateNeededWhenHabIsOlder() async {
        let hab = await sampleHab()
        await hab.setRevisions(0...0)
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("update needed") { await conn.state.status == .updateNeeded(.hab) }
        await conn.stop()
    }

    func testStopClosesTheStream() async {
        let hab = await sampleHab()
        let transport = hab.transport()
        let conn = HabConnection(id: "a", transport: transport, timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        XCTAssertEqual(transport.openStreams, 1)
        await conn.stop()
        XCTAssertEqual(transport.openStreams, 0)
        let status = await conn.state.status
        XCTAssertEqual(status, .stopped)
        await eventually("hab sees the close") { await hab.connectionCount == 0 }
        // Rows stay after a stop.
        let n = await conn.state.store.sessions.count
        XCTAssertEqual(n, 2)
    }

    /// A hab that drops every connection right after the snapshot is not
    /// hammered: short sessions count as failures and back off.
    func testShortSessionsBackOff() async {
        let hab = await sampleHab()
        let transport = hab.transport()
        var t = fastTiming()
        t.stableAfter = .seconds(5)
        t.minBackoff = .milliseconds(200)
        t.maxBackoff = .milliseconds(200)
        let conn = HabConnection(id: "a", transport: transport, timing: t)
        await conn.start()
        let dropper = Task {
            while !Task.isCancelled {
                if await hab.subscribes > 0 { await hab.dropConnections() }
                try? await Task.sleep(for: .milliseconds(5))
            }
        }
        try? await Task.sleep(for: .milliseconds(1000))
        dropper.cancel()
        // Back-to-back immediate retries would make dozens of attempts in 1 s;
        // backing off at 100-200 ms allows about ten.
        XCTAssertLessThanOrEqual(transport.openAttempts, 12, "attempts: \(transport.openAttempts)")
        await conn.stop()
    }

    func testBackoffBounds() {
        let lo = Duration.milliseconds(500), hi = Duration.seconds(30)
        for attempt in 1...20 {
            let d = HabConnection.backoff(attempt: attempt, lo: lo, hi: hi)
            var full = lo
            for _ in 1..<attempt where full < hi { full = full * 2 }
            if full > hi { full = hi }
            XCTAssertGreaterThanOrEqual(d, full / 2, "attempt \(attempt)")
            XCTAssertLessThanOrEqual(d, full, "attempt \(attempt)")
        }
        XCTAssertLessThanOrEqual(HabConnection.backoff(attempt: 50, lo: lo, hi: hi), hi)
    }
}

/// The seq rule with events that arrive before the subscribe response.
final class SeqRuleTests: XCTestCase {
    func testEventsAtOrBelowResponseSeqAreDropped() async throws {
        // A hand-written server: on subscribe it sends events 1 and 2, then the
        // response with seq 1, then event 3. Event 1 is the only change to s1 and
        // is already in the snapshot, so only the seq rule can drop it: its
        // revision (9) is above the snapshot's (2), so the revision rule would let
        // it through. Events 2 and 3 touch s2, so they cannot mask it.
        let transport = FakeTransport { server, _ in
            Task {
                var lines = LineBuffer()
                while let chunk = try? await server.read() {
                    for line in (try? lines.append(chunk)) ?? [] {
                        let obj = (try? JSONSerialization.jsonObject(with: line)) as? [String: Any] ?? [:]
                        let id = obj["id"] as? Int ?? 0
                        let method = obj["method"] as? String ?? ""
                        func send(_ s: String) async { try? await server.write(Data((s + "\n").utf8)) }
                        func event(_ seq: Int, _ sid: String, _ title: String, rev: Int) -> String {
                            #"{"jsonrpc":"2.0","method":"event","params":{"seq":\#(seq),"type":"session.updated","id":"\#(sid)","revision":\#(rev),"data":{"id":"\#(sid)","name":"x","revision":\#(rev),"state":"running","title":"\#(title)"}}}"#
                        }
                        if method == "hab.hello" {
                            await send(#"{"jsonrpc":"2.0","id":\#(id),"result":{"build":"t","capabilities":[],"client_id":"c1","epoch":"e1","host":{"id":"h"},"reconnect_token":"tok","revision":1,"seq":0}}"#)
                        } else if method == "events.subscribe" {
                            await send(event(1, "s1", "from event 1", rev: 9))
                            await send(event(2, "s2", "from event 2", rev: 2))
                            await send(#"{"jsonrpc":"2.0","id":\#(id),"result":{"host":{"id":"h"},"panes":[],"projects":[],"seq":1,"sessions":[{"id":"s1","name":"x","revision":2,"state":"running","title":"snapshot"},{"id":"s2","name":"y","revision":1,"state":"running","title":"s2 snapshot"}]}}"#)
                            await send(event(3, "s2", "from event 3", rev: 3))
                        }
                    }
                }
            }
        }
        let conn = HabConnection(id: "a", transport: transport, timing: fastTiming())
        await conn.start()
        await eventually("event 3") { await conn.state.store.sessions["s2"]?.title == "from event 3" }
        let s1 = await conn.state.store.sessions["s1"]
        XCTAssertEqual(s1?.title, "snapshot", "event 1 has seq 1, not above the response seq")
        XCTAssertEqual(s1?.revision, 2)
        await conn.stop()
    }

    func testBufferedEventAboveSeqIsApplied() async {
        // Events 1 and 2 before a response with seq 1: event 2 applies.
        let transport = FakeTransport { server, _ in
            Task {
                var lines = LineBuffer()
                while let chunk = try? await server.read() {
                    for line in (try? lines.append(chunk)) ?? [] {
                        let obj = (try? JSONSerialization.jsonObject(with: line)) as? [String: Any] ?? [:]
                        let id = obj["id"] as? Int ?? 0
                        let method = obj["method"] as? String ?? ""
                        func send(_ s: String) async { try? await server.write(Data((s + "\n").utf8)) }
                        if method == "hab.hello" {
                            await send(#"{"jsonrpc":"2.0","id":\#(id),"result":{"build":"t","capabilities":[],"client_id":"c1","epoch":"e1","host":{"id":"h"},"reconnect_token":"tok","revision":1,"seq":0}}"#)
                        } else if method == "events.subscribe" {
                            await send(#"{"jsonrpc":"2.0","method":"event","params":{"seq":1,"type":"session.updated","id":"s1","revision":2,"data":{"id":"s1","name":"x","revision":2,"state":"running","title":"one"}}}"#)
                            await send(#"{"jsonrpc":"2.0","method":"event","params":{"seq":2,"type":"session.updated","id":"s1","revision":3,"data":{"id":"s1","name":"x","revision":3,"state":"running","title":"two"}}}"#)
                            await send(#"{"jsonrpc":"2.0","id":\#(id),"result":{"host":{"id":"h"},"panes":[],"projects":[],"seq":1,"sessions":[{"id":"s1","name":"x","revision":2,"state":"running","title":"one"}]}}"#)
                        }
                    }
                }
            }
        }
        let conn = HabConnection(id: "a", transport: transport, timing: fastTiming())
        await conn.start()
        await eventually("event 2") { await conn.state.store.sessions["s1"]?.title == "two" }
        await conn.stop()
    }
}
