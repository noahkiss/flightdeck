import FlightdeckShared
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// ActivityKit as a recorder: what the tracker started, updated and ended.
@MainActor
final class FakeActivities: ActivityHost {
    var enabled = true
    var live: [String] = []
    var started: [(SessionActivityAttributes, SessionActivityAttributes.ContentState)] = []
    var updates: [(String, SessionActivityAttributes.ContentState)] = []
    var ended: [String] = []
    /// Every event in order, to check that other activities end first.
    var log: [String] = []
    var token = Data([0xC3, 0x01])

    func running() -> [String] { live }

    func start(_ a: SessionActivityAttributes, _ s: SessionActivityAttributes.ContentState) throws -> AsyncStream<Data> {
        started.append((a, s))
        live.append(a.session)
        log.append("start \(a.session)")
        let t = token
        return AsyncStream { c in
            c.yield(t)
            c.finish()
        }
    }

    func update(session: String, _ s: SessionActivityAttributes.ContentState) async {
        updates.append((session, s))
    }

    func end(session: String) async {
        ended.append(session)
        live.removeAll { $0 == session }
        log.append("end \(session)")
    }
}

@MainActor
final class LiveActivityFixTests: XCTestCase {
    /// A fleet online with one blocked session, push registered, and a tracker
    /// whose calls go to the fake hab on a one-shot connection.
    private func rig() async -> (FakeHab, Fleet, LiveActivityTracker, FakeActivities) {
        let hab = await blockedHab()
        let fleet = Fleet()
        let reg = PushRegistrar(defaults: emptyDefaults(), environment: .development)
        fleet.push = reg
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "desk")
        let host = FakeActivities()
        let caller = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone")
        let tracker = LiveActivityTracker(board: fleet.board, push: reg, caller: caller, host: host)
        fleet.activities = tracker
        reg.setDeviceToken(Data([1]))
        await fleet.setActive(true)
        await eventually("registered") { reg.status["h"] == .registered }
        await eventually("tracked") { tracker.pick.tracked?.session == "s1" }
        return (hab, fleet, tracker, host)
    }

    /// The scene turning inactive starts the activity while the app is still
    /// in front; going to the background alone starts nothing.
    func testStartsOnInactiveNotBackground() async {
        let (hab, fleet, _, host) = await rig()
        fleet.scene(active: false)
        // setActive queues behind the scene's own call: this returns once the
        // background transition has run.
        await fleet.setActive(false)
        XCTAssertTrue(host.started.isEmpty, "no start from the background")
        await fleet.setActive(true)
        // The new connection registers again; wait for the hab to see it,
        // since the status still says registered from the last connection.
        await eventually("registered again") { await hab.pushRegisters.count == 2 }
        fleet.scene(active: nil)
        await eventually("started") { host.started.count == 1 }
        XCTAssertEqual(host.started.first?.0.session, "s1")
        await fleet.setActive(false)
    }

    /// The scene can turn inactive while a new connection's repeat
    /// `push.register` is still open: the hab already holds the device, so
    /// the activity starts before that call answers.
    func testStartsWhileANewConnectionRegistersAgain() async {
        let (hab, fleet, _, host) = await rig()
        let gate = Gate()
        // A failed check must not leave the hab connection held.
        defer { Task { await gate.open() } }
        await hab.setPushRegisterHook { _ in await gate.wait() }
        await fleet.setActive(false)
        await fleet.setActive(true)
        await eventually("repeat register held") { await hab.pushRegisters.count == 2 }
        XCTAssertEqual(fleet.push?.status["h"], .registering)
        fleet.scene(active: nil)
        await eventually("started") { host.started.count == 1 }
        XCTAssertEqual(fleet.push?.status["h"], .registering, "started before the hab answered")
        await gate.open()
        await fleet.setActive(false)
    }

    /// A new device token no hab holds yet does not count as registered.
    func testNoStartWhileANewTokenRegisters() async {
        let (hab, fleet, tracker, host) = await rig()
        let gate = Gate()
        // A failed check must not leave the hab connection held.
        defer { Task { await gate.open() } }
        await hab.setPushRegisterHook { _ in await gate.wait() }
        fleet.push!.setDeviceToken(Data([2]))
        XCTAssertEqual(fleet.push?.status["h"], .registering)
        await tracker.start()
        XCTAssertTrue(host.started.isEmpty)
        await gate.open()
        await eventually("registered") { fleet.push?.status["h"] == .registered }
        await tracker.start()
        XCTAssertEqual(host.started.count, 1)
        await fleet.setActive(false)
    }

    /// A first registration still in flight is not enough to start one.
    func testNoStartBeforeFirstRegistration() async {
        let hab = await blockedHab()
        let fleet = Fleet()
        let reg = PushRegistrar(defaults: emptyDefaults(), environment: .development)
        fleet.push = reg
        let conn = HabConnection(id: "h", transport: hab.transport(), timing: fastTiming())
        fleet.add(conn, label: "desk")
        let host = FakeActivities()
        let tracker = LiveActivityTracker(board: fleet.board, push: reg, caller: ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone"), host: host)
        fleet.activities = tracker
        await fleet.setActive(true)
        await eventually("tracked") { tracker.pick.tracked?.session == "s1" }
        await eventually("waiting for a token") { reg.status["h"] == .waiting }
        await tracker.start()
        reg.setDeviceToken(Data([1]))
        XCTAssertEqual(reg.status["h"], .registering)
        await tracker.start()
        XCTAssertTrue(host.started.isEmpty)
        await fleet.setActive(false)
    }

    /// A blocked session's first content has its request with Allow and Deny,
    /// and the token goes to the hab.
    func testInitialContentHasButtonsAndTokenRegisters() async {
        let (hab, fleet, tracker, host) = await rig()
        await tracker.start()
        let s = host.started.first?.1
        XCTAssertEqual(s?.buttons, true)
        XCTAssertEqual(s?.request, "req-1")
        XCTAssertEqual(s?.pane, "%1")
        XCTAssertEqual(s?.title, "Run make test?")
        await eventually("token registered") { await hab.activityRegisters.first?.token == "c301" }
        await fleet.setActive(false)
    }

    /// One activity at a time: any other ends before the new one starts.
    func testEndsOtherActivitiesFirst() async {
        let (_, fleet, tracker, host) = await rig()
        host.live = ["old-session"]
        await tracker.start()
        XCTAssertEqual(host.log, ["end old-session", "start s1"])
        await fleet.setActive(false)
    }

    /// An answer that landed (or was refused) drops the buttons at once; a
    /// failure keeps them.
    func testAnswerDropsButtonsLocally() async {
        let (_, fleet, tracker, host) = await rig()
        await tracker.start()
        await tracker.answered(pane: "%1", request: "req-1", outcome: .unreachable("x"))
        XCTAssertTrue(host.updates.isEmpty)
        await tracker.answered(pane: "%1", request: "req-1", outcome: .answered)
        XCTAssertEqual(host.updates.last?.0, "s1")
        XCTAssertEqual(host.updates.last?.1.buttons, false)
        XCTAssertNil(host.updates.last?.1.request)
        await fleet.setActive(false)
    }

    /// Opening the session ends its activity and drops the token on the hab.
    func testOpenedEndsAndUnregisters() async {
        let (hab, fleet, tracker, host) = await rig()
        await tracker.start()
        await eventually("token registered") { await hab.activityRegisters.count == 1 }
        await tracker.opened(SessionTarget(habID: "h", session: "s1", pane: "%1"))
        XCTAssertEqual(host.ended, ["s1"])
        await eventually("unregistered") { await hab.unregisters.first?.token == "c301" }
        await fleet.setActive(false)
    }
}

@MainActor
final class ApprovalFixTests: XCTestCase {
    /// A Live Activity answer on a locked device sends nothing: "Unlock to
    /// answer", and the request waits for the foreground.
    func testLockedActivityAnswerRefused() async {
        let hab = await blockedHab()
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone")
        let c = ApprovalCenter(board: Board(), answerer: answerer, defaults: emptyDefaults())
        var posts: [String] = []
        c.post = { t, _, _ in posts.append(t) }
        c.isUnlocked = { false }
        let o = await c.answerFromActivity(host: "testhost", pane: "%1", request: "req-1", allow: true)
        XCTAssertNil(o)
        XCTAssertEqual(posts, ["Unlock to answer"])
        XCTAssertEqual(c.pending.first?.request, "req-1")
        let n = await hab.answers.count
        XCTAssertEqual(n, 0)
        c.isUnlocked = { true }
        let o2 = await c.answerFromActivity(host: "testhost", pane: "%1", request: "req-1", allow: true)
        XCTAssertEqual(o2, .answered)
    }

    /// A tap on a banner whose request is gone never opens another request
    /// under Allow: the sheet shows that request as answered elsewhere.
    func testStaleTapOpensPlaceholder() async {
        let hab = await blockedHab(requests: [("%1", "req-2", Date()), ("%2", "req-3", Date())])
        let fleet = Fleet()
        let conn = HabConnection(id: "h", transport: hab.transport(), timing: fastTiming())
        fleet.add(conn, label: "desk")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 1 }
        let m = ApprovalSheetModel(board: fleet.board) { _, _ in .answered }
        XCTAssertTrue(m.show(host: "testhost", pane: "%1", request: "req-1"))
        XCTAssertEqual(m.current?.requestID, "req-1")
        XCTAssertEqual(m.phase, .answeredElsewhere)
        XCTAssertFalse(m.canAnswer)
        // A hab that is not online yet: nothing to show, so the center keeps it pending.
        XCTAssertFalse(m.show(host: "otherhost", pane: "%1", request: "req-9"))
        await fleet.setActive(false)
    }

    /// A request that closes on the board withdraws its delivered banners.
    func testClosedRequestWithdrawsBanners() async {
        let hab = await blockedHab()
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "desk")
        let c = ApprovalCenter(board: fleet.board, answerer: ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in throw TransportError("x") }, clientLabel: "phone"), defaults: emptyDefaults())
        var withdrawn: [Set<String>] = []
        c.withdraw = { ids in withdrawn.append(ids) }
        fleet.approvals = c
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 1 }
        XCTAssertTrue(withdrawn.isEmpty)
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .working))
        await eventually("withdrawn") { withdrawn == [["req-1"]] }
        await fleet.setActive(false)
    }

    /// A live connection that drops under the call tries once on a fresh one.
    /// When that try is refused, the first one most likely landed: "Answered".
    func testDroppedLiveRetriesOnce() async {
        let silent = await blockedHab()
        let conn = HabConnection(id: "h", transport: silent.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        await silent.setSilent(true)
        let other = await blockedHab()
        let answerer = ApprovalAnswerer(live: { _ in conn }, oneShot: { _ in other.transport() }, clientLabel: "phone", deadline: .seconds(5))
        let p = ApprovalAnswerParams(choice: .allow, pane: "%1", request: "req-1", via: .banner)
        let o = await answerer.answer(host: "testhost", p)
        XCTAssertEqual(o, .answered, "the fresh try answered")
        let n = await other.answers.count
        XCTAssertEqual(n, 1)

        await other.failNextAnswer(kind: .conflict, hint: "not open")
        await silent.setSilent(false)
        await eventually("online again") { await conn.state.status == .online }
        await silent.setSilent(true)
        let o2 = await answerer.answer(host: "testhost", p)
        XCTAssertEqual(o2, .probablyAnswered("not open"))
        XCTAssertEqual(o2.followUp(hab: "desk", expiry: nil)?.title, "Answered")
        await conn.stop()
    }

    /// Forgetting a connection rewrites the hab book file.
    func testHabBookForgetWrites() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("fd-book-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: dir) }
        let url = dir.appendingPathComponent("hab-book.json")
        let b = HabBook(url: url)
        b.record(host: "a", connection: "c1", label: "A")
        b.record(host: "b", connection: "c2", label: "B")
        b.forget(connection: "c1")
        let back = HabBook(url: url)
        XCTAssertNil(back.entry(host: "a"))
        XCTAssertEqual(back.entry(host: "b")?.connection, "c2")
    }

    /// A plan is Markdown, never a localization key: format specifiers stay.
    func testPlanTextIsNotALocalizationKey() {
        XCTAssertEqual(String(RequestForm.planText("Use %@ and **bold** 100%").characters), "Use %@ and bold 100%")
    }

    /// Log lines put only the outcome's case in public.
    func testOutcomeCaseNameHidesDetail() {
        let o = AnswerOutcome.unreachable("dial build-box.example:22 failed")
        XCTAssertEqual(o.caseName, "unreachable")
        XCTAssertFalse(o.caseName.contains("example"))
        XCTAssertEqual(o.detail, "dial build-box.example:22 failed")
    }
}

/// Holds callers until opened.
actor Gate {
    private var isOpen = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func wait() async {
        if isOpen { return }
        await withCheckedContinuation { waiters.append($0) }
    }

    func open() {
        isOpen = true
        for w in waiters { w.resume() }
        waiters = []
    }
}
