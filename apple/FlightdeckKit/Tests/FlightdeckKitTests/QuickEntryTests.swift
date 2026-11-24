import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Quick entry for Capcom (app.md 3.18): the send to the standing conversation and to a new
/// one, what the panel says before it can send, a refusal, and the recents.
@MainActor
final class QuickEntryTests: XCTestCase {
    private var suites: [String] = []

    override func tearDown() async throws {
        for s in suites { UserDefaults().removePersistentDomain(forName: s) }
        suites = []
    }

    private func defaults() -> UserDefaults {
        let name = "quickentry-\(UUID().uuidString)"
        suites.append(name)
        return emptySuite(name)
    }

    private func hab(_ host: String) async -> FakeHab {
        let h = await sampleHab(host)
        await h.setCapability("capcom", true)
        return h
    }

    /// A fleet over `habs`, started, with every hab online and the Capcom home read.
    private func fleet(_ habs: [FakeHab]) async -> Fleet {
        let f = Fleet()
        for h in habs {
            f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        await eventually("home read") {
            f.capcom.entry != .loading && f.capcom.reading.isEmpty && habs.allSatisfy { f.capcom.beliefs[$0.hostID] != nil }
        }
        return f
    }

    func testSendGoesToTheStandingConversation() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        XCTAssertEqual(q.readiness, .ready(habID: "bench", label: "bench"))
        XCTAssertEqual(q.line, "To Capcom on bench")
        XCTAssertFalse(q.canSend, "nothing typed")
        q.text = "  what is on today?  "
        XCTAssertTrue(q.canSend)
        let ok = await q.send()
        XCTAssertTrue(ok, q.failure ?? "")
        let opens = await a.capcomOpens
        XCTAssertEqual(opens, [CapcomOpenParams()], "the standing conversation: no context, not new")
        let sends = await a.sends
        XCTAssertEqual(sends.map(\.session), ["capcom-s1"])
        XCTAssertEqual(sends.map(\.text), ["what is on today?"])
        XCTAssertEqual(q.text, "", "the field empties")
        XCTAssertEqual(q.recents, ["what is on today?"])
        XCTAssertEqual(f.capcom.standing?.target.session, "capcom-s1", "the Capcom view shows it")
        XCTAssertNil(f.capcom.openRequest, "no board window opens")
        await f.setActive(false)
    }

    /// The Capcom service (capcom-service.md 7): a thread with no pane, which takes every message.
    func testSendGoesToTheServiceThread() async throws {
        let a = await hab("bench")
        await a.setCapcomService(true)
        let f = await fleet([a])
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        q.text = "remind me at six"
        let ok = await q.send()
        XCTAssertTrue(ok, q.failure ?? "")
        let sends = await a.sends
        XCTAssertEqual(sends.map(\.text), ["remind me at six"])
        let m = try XCTUnwrap(f.capcom.standing)
        XCTAssertTrue(m.context.isCapcomService)
        XCTAssertEqual(m.target.session, sends.first?.session)
        await f.setActive(false)
    }

    /// A pane-based Capcom mid-turn: the message stays in the panel, not queued in the app.
    func testBusyCapcomKeepsTheText() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        await f.capcom.openStanding()
        let m = try XCTUnwrap(f.capcom.standing)
        let pane = try XCTUnwrap(m.target.pane)
        await a.put(Fixture.agent(pane: pane, harness: "claude", state: .working))
        await eventually("working") { m.context.agentStatus == .working }
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        q.text = "later"
        let ok = await q.send()
        XCTAssertFalse(ok)
        XCTAssertEqual(q.failure, "Capcom is busy with a turn. Send again when it finishes.")
        XCTAssertEqual(q.text, "later")
        XCTAssertTrue(m.outgoing.isEmpty, "nothing waits in the app")
        let sends = await a.sends
        XCTAssertTrue(sends.isEmpty)
        await f.setActive(false)
    }

    func testSendToNewChatOpensANewConversation() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        q.text = "first"
        _ = await q.send()
        q.text = "second"
        let ok = await q.send(new: true)
        XCTAssertTrue(ok, q.failure ?? "")
        let opens = await a.capcomOpens
        XCTAssertEqual(opens.map(\.new), [nil, true])
        let sends = await a.sends
        XCTAssertEqual(sends.map(\.session), ["capcom-s1", "capcom-s2"])
        XCTAssertEqual(f.capcom.standing?.target.session, "capcom-s2", "the Capcom view follows the new one")
        XCTAssertEqual(q.recents, ["second", "first"])
        await f.setActive(false)
    }

    func testNoHomeSendsNothing() async throws {
        let a = await hab("bench")
        await a.setCapcomHome(assignment: 0, home: nil)
        let f = await fleet([a])
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        q.text = "hello"
        XCTAssertEqual(q.readiness, .unavailable("No Capcom host. Choose one in Settings."))
        XCTAssertFalse(q.canSend)
        let ok = await q.send()
        XCTAssertFalse(ok)
        let opens = await a.capcomOpens
        XCTAssertTrue(opens.isEmpty)
        XCTAssertEqual(q.text, "hello", "the text stays")
        await f.setActive(false)
    }

    func testHomeOfflineSaysSo() async throws {
        let a = await hab("bench")
        let b = await hab("attic")
        let bID = b.deviceID
        await a.setCapcomHome(assignment: 1, home: bID)
        await b.setCapcomHome(assignment: 1, home: bID)
        let f = await fleet([a, b])
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        XCTAssertEqual(q.line, "To Capcom on attic")
        await b.setSilent(true)
        await b.dropConnections()
        await eventually("attic offline") { f.capcom.entry == .offline(habID: "attic") }
        XCTAssertEqual(q.readiness, .unavailable("attic is offline"))
        q.text = "hello"
        XCTAssertFalse(q.canSend)
        await f.setActive(false)
    }

    func testNoPairedComputer() {
        let f = Fleet()
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        XCTAssertEqual(q.readiness, .unavailable("No paired computer."))
    }

    func testARefusalKeepsTheText() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        let q = QuickEntry(capcom: f.capcom, defaults: defaults())
        await a.failNextSend(kind: .unavailable, hint: "no adapter")
        q.text = "hello"
        let ok = await q.send()
        XCTAssertFalse(ok)
        XCTAssertEqual(q.failure, "the session can't take a message now", "the composer's sentence for it")
        XCTAssertEqual(q.text, "hello")
        XCTAssertTrue(q.recents.isEmpty)
        XCTAssertFalse(f.capcom.standing?.outgoing.contains { $0.text == "hello" } ?? true,
                       "the failed row leaves the Capcom view; the panel holds the text")
        // Sending again works.
        let again = await q.send()
        XCTAssertTrue(again, q.failure ?? "")
        XCTAssertNil(q.failure)
        await f.setActive(false)
    }

    func testRecentsKeepFiveNewestFirstAndPersist() async throws {
        let a = await hab("bench")
        let f = await fleet([a])
        let d = defaults()
        let q = QuickEntry(capcom: f.capcom, defaults: d)
        for t in ["one", "two", "three", "two", "four", "five", "six"] {
            q.text = t
            _ = await q.send()
        }
        XCTAssertEqual(q.recents, ["six", "five", "four", "two", "three"])
        XCTAssertEqual(QuickEntry(capcom: f.capcom, defaults: d).recents, q.recents, "kept on this Mac")
        q.pick("four")
        XCTAssertEqual(q.text, "four")
        q.clearRecents()
        XCTAssertTrue(q.recents.isEmpty)
        XCTAssertTrue(QuickEntry(capcom: f.capcom, defaults: d).recents.isEmpty)
        await f.setActive(false)
    }
}
