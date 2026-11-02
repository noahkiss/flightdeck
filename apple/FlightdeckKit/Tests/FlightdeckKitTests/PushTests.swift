import FlightdeckShared
import Foundation
import HabAPI
import UserNotifications
import XCTest
@testable import FlightdeckKit

/// Empty UserDefaults for a test, so settings and the device id never leak.
/// The n-th call in a test gets the suite `flightdeck.tests.<test>.<n>`,
/// emptied first. A suite name used once leaves a preferences file behind
/// even after `removePersistentDomain`, so the names stay the same from run
/// to run (one file per test that uses them), and no two tests share one:
/// an object a test left running cannot write into the next test's
/// defaults. Two processes running the same test at once (`--parallel`)
/// would share it; `swift test` here runs one at a time.
extension XCTestCase {
    func emptyDefaults() -> UserDefaults {
        TestDefaultsSlots.next(for: name)
    }
}

private enum TestDefaultsSlots {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var test: String?
    nonisolated(unsafe) private static var used = 0

    static func next(for current: String) -> UserDefaults {
        lock.lock()
        if current != test {
            test = current
            used = 0
        }
        let test = String(current.unicodeScalars.map { CharacterSet.alphanumerics.contains($0) ? Character($0) : "-" })
        let name = "flightdeck.tests.\(test).\(used)"
        used += 1
        lock.unlock()
        let d = UserDefaults(suiteName: name)!
        d.removePersistentDomain(forName: name)
        return d
    }
}

/// A hab with one Claude session blocked on a permission request.
func blockedHab(_ host: String = "testhost", push: Bool = true, requests: [(pane: String, id: String, at: Date)] = [("%1", "req-1", Date())], expires: Date? = nil, tool: String = "Bash", input: JSONValue? = nil) async -> FakeHab {
    var caps = ["agent-state", "data-streams", "snapshot-vt", "transcripts"]
    if push { caps.append("push") }
    let hab = FakeHab(hostID: host, capabilities: caps)
    var sessions: [Session] = []
    var panes: [Pane] = []
    var agents: [AgentState] = []
    for (i, r) in requests.enumerated() {
        let sid = "s\(i + 1)"
        sessions.append(Fixture.session(sid, name: "claude-\(i + 1)", pane: r.pane, project: "p1", model: "opus"))
        panes.append(Fixture.pane(r.pane, session: sid))
        let req = PermissionRequest(command: tool == "Bash" ? "make test" : nil, expiresAt: expires.map(Fixture.stamp), id: r.id, input: input, title: tool == "Bash" ? "Run make test?" : nil, tool: tool)
        agents.append(Fixture.agent(pane: r.pane, harness: "claude", state: .blocked, kind: .permission, request: req, at: r.at))
    }
    await hab.load(projects: [Fixture.project("p1", name: "tide-tables", sessions: sessions.map(\.id))], sessions: sessions, panes: panes, agents: agents)
    return hab
}

// MARK: - Registration

@MainActor
final class PushRegistrationTests: XCTestCase {
    private func rig(push: Bool = true) async -> (FakeHab, Fleet, PushRegistrar) {
        let hab = await blockedHab(push: push)
        let fleet = Fleet()
        let reg = PushRegistrar(defaults: emptyDefaults(), environment: .development)
        fleet.push = reg
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming(), clientLabel: "phone"), label: "desk")
        return (hab, fleet, reg)
    }

    /// The token goes to the hab on connect, with this build's environment,
    /// the device id and the default kinds, round among them; done is opt-in.
    func testRegistersOnConnect() async {
        let (hab, fleet, reg) = await rig()
        reg.setDeviceToken(Data([0xAB, 0x01, 0xFF]))
        await fleet.setActive(true)
        await eventually("registered") { reg.status["h"] == .registered }
        let calls = await hab.pushRegisters
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(calls.first?.token, "ab01ff")
        XCTAssertEqual(calls.first?.environment, .development)
        XCTAssertEqual(calls.first?.deviceId, reg.deviceID)
        XCTAssertEqual(calls.first?.kinds, [.permission, .question, .auth, .error, .round])
        XCTAssertNil(calls.first?.hideCommands)
        await fleet.setActive(false)
    }

    /// A hab built before the `round` kind refuses a registration naming it as `invalid`
    /// (internal/control/push.go). The app registers there again without it, so that hab's
    /// other pushes still come.
    func testHabThatRefusesTheRoundKindGetsTheOtherKinds() async {
        let (hab, fleet, reg) = await rig()
        await hab.setUnknownPushKinds([.round])
        reg.setDeviceToken(Data([0xAB]))
        await fleet.setActive(true)
        await eventually("registered") { reg.status["h"] == .registered }
        XCTAssertTrue(reg.canPush("h"))
        let refused = await hab.refusedPushRegisters
        XCTAssertEqual(refused.map(\.kinds), [[.permission, .question, .auth, .error, .round]])
        let calls = await hab.pushRegisters
        XCTAssertEqual(calls.map(\.kinds), [[.permission, .question, .auth, .error]])
        // A setting change asks for round again, and falls back again.
        reg.doneBanners = true
        await eventually("done kind") { await hab.pushRegisters.last?.kinds == [.permission, .question, .auth, .error, .done] }
        await reg.settle()
        XCTAssertEqual(reg.status["h"], .registered)
        await fleet.setActive(false)
    }

    /// A hab online before the token waits; the token then registers there.
    /// A new token, a reconnect and a setting change each register again.
    func testRegistersAgainOnTokenReconnectAndSettings() async {
        let (hab, fleet, reg) = await rig()
        await fleet.setActive(true)
        await eventually("waiting") { reg.status["h"] == .waiting }
        var n = await hab.pushRegisters.count
        XCTAssertEqual(n, 0, "no token, no call")

        reg.setDeviceToken(Data([1, 2]))
        await eventually("first") { await hab.pushRegisters.count == 1 }
        reg.setDeviceToken(Data([1, 2]))
        await reg.settle()
        n = await hab.pushRegisters.count
        XCTAssertEqual(n, 1, "the same token again sends nothing")

        reg.setDeviceToken(Data([3, 4]))
        await eventually("new token") { await hab.pushRegisters.last?.token == "0304" }

        await hab.dropConnections()
        await eventually("reconnect registers") { await hab.pushRegisters.count == 3 }

        reg.doneBanners = true
        await eventually("done kind") { await hab.pushRegisters.last?.kinds.contains(.done) == true }
        reg.showCommands = false
        await eventually("hide commands") { await hab.pushRegisters.last?.hideCommands == true }
        await fleet.setActive(false)
    }

    /// A hab whose welcome has no push gets no call and shows "not set up".
    func testHabWithoutPushIsNotSetUp() async {
        let (hab, fleet, reg) = await rig(push: false)
        reg.setDeviceToken(Data([9]))
        await fleet.setActive(true)
        await eventually("not set up") { reg.status["h"] == .notSetUp }
        let n = await hab.pushRegisters.count
        XCTAssertEqual(n, 0)
        XCTAssertEqual(reg.status["h"]?.text, "Push is not set up on this hab")
        await fleet.setActive(false)
    }

    /// The device id survives a restart; Debug test builds are development.
    func testDeviceIDPersistsAndEnvironment() {
        let d = emptyDefaults()
        let a = PushRegistrar(defaults: d)
        let b = PushRegistrar(defaults: d)
        XCTAssertEqual(a.deviceID, b.deviceID)
        XCTAssertFalse(a.deviceID.isEmpty)
        XCTAssertEqual(PushRegistrar.buildEnvironment, .development)
    }

    /// The hab book records the host id of each hab that comes online.
    func testHabBookRecordsHost() async throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("fd-book-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: dir) }
        let url = dir.appendingPathComponent("hab-book.json")
        let (_, fleet, _) = await rig()
        fleet.book = HabBook(url: url)
        await fleet.setActive(true)
        await eventually("recorded") { fleet.book?.entry(host: "testhost") != nil }
        XCTAssertEqual(fleet.book?.entry(host: "testhost"), HabBook.Entry(connection: "h", label: "desk", device: "SHA256:fake-testhost"))
        XCTAssertEqual(HabBook(url: url).entry(host: "testhost")?.connection, "h", "read back from the file")
        let mode = try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? Int
        XCTAssertEqual(mode, 0o600)
        await fleet.setActive(false)
    }
}

// MARK: - Notification actions

final class NotificationAnswerTests: XCTestCase {
    let meta = PushMeta(kind: "permission", hab: "testhost", session: "s1", pane: "%1", state: "blocked", request: "req-1", expiresAt: "2026-11-01T12:05:00Z")

    func testActionsMapToApprovalAnswer() {
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.approveAction, meta: meta),
                       .answer(host: "testhost", ApprovalAnswerParams(choice: .allow, pane: "%1", request: "req-1", via: .banner)))
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.denyAction, meta: meta),
                       .answer(host: "testhost", ApprovalAnswerParams(choice: .deny, pane: "%1", request: "req-1", via: .banner)))
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.replyAction, meta: meta, text: "  Clerk \n"),
                       .answer(host: "testhost", ApprovalAnswerParams(choice: .answer, pane: "%1", request: "req-1", text: "Clerk", via: .banner)))
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.openAction, meta: meta), .open(meta))
        XCTAssertEqual(NotificationAnswer.intent(action: NotificationAnswer.defaultAction, meta: meta), .open(meta))
        XCTAssertEqual(NotificationAnswer.intent(action: "com.apple.UNNotificationDismissActionIdentifier", meta: meta), .none)
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.approveAction, meta: nil), .none)
    }

    /// With no request id or pane an answer cannot name the request: the tap
    /// opens the app instead. An empty reply opens it too.
    func testNoRequestIDOpensInstead() {
        var m = meta
        m.request = nil
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.approveAction, meta: m), .open(m))
        var p = meta
        p.pane = nil
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.denyAction, meta: p), .open(p))
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.replyAction, meta: meta, text: " "), .open(meta))
    }

    /// The push's `flightdeck` object parses from userInfo as APNs delivers it.
    func testPushMetaFromUserInfo() throws {
        let json = #"{"aps":{"alert":{"title":"Run make test?"},"category":"approval"},"flightdeck":{"v":1,"kind":"permission","hab":"testhost","session":"s1","pane":"%1","state":"blocked","request":"req-1","expires_at":"2026-11-01T12:05:00Z"}}"#
        let info = try JSONSerialization.jsonObject(with: Data(json.utf8)) as! [AnyHashable: Any]
        XCTAssertEqual(PushMeta(userInfo: info), meta)
        XCTAssertNil(PushMeta(userInfo: ["aps": [:]]))
        XCTAssertEqual(PushMeta(userInfo: [PushMeta.userInfoKey: meta.userInfoValue]), meta, "a local notification round-trips")
    }

    /// The categories the hab names, with Approve and Deny behind unlock.
    func testCategories() {
        let cats = Dictionary(uniqueKeysWithValues: HabActions.categories().map { ($0.identifier, $0) })
        for id in [PushCategory.approval, PushCategory.approvalDeny, PushCategory.open, PushCategory.question, HabActions.pingCategory] {
            XCTAssertNotNil(cats[id], id)
        }
        XCTAssertEqual(cats[PushCategory.approval]?.actions.map(\.identifier), [PushCategory.denyAction, PushCategory.approveAction])
        XCTAssertEqual(cats[PushCategory.approvalDeny]?.actions.map(\.identifier), [PushCategory.denyAction, PushCategory.openAction])
        XCTAssertEqual(cats[PushCategory.open]?.actions.map(\.identifier), [PushCategory.openAction])
        for a in cats[PushCategory.approval]!.actions {
            XCTAssertTrue(a.options.contains(.authenticationRequired), a.identifier)
        }
        XCTAssertTrue(cats[PushCategory.question]?.actions.first is UNTextInputNotificationAction)
    }

    /// The service extension's expiry text, in the device's time zone.
    func testExpiryText() {
        let now = PushMeta.parseDate("2026-11-01T12:00:00Z")!
        let utc = TimeZone(identifier: "UTC")!
        let loc = Locale(identifier: "en_GB")
        XCTAssertEqual(ExpiryText.append(to: "make test", meta: meta, now: now, locale: loc, timeZone: utc), "make test · Expires 12:05")
        XCTAssertEqual(ExpiryText.append(to: "make test", meta: meta, now: now, locale: loc, timeZone: TimeZone(identifier: "Europe/Berlin")!), "make test · Expires 13:05")
        let past = PushMeta.parseDate("2026-11-01T12:06:00Z")!
        XCTAssertEqual(ExpiryText.append(to: "make test", meta: meta, now: past, locale: loc, timeZone: utc), "make test", "a past expiry adds nothing")
        var notice = meta
        notice.kind = "answered"
        XCTAssertEqual(ExpiryText.append(to: "Allowed: make", meta: notice, now: now, locale: loc, timeZone: utc), "Allowed: make")
        XCTAssertEqual(ExpiryText.append(to: "x", meta: nil, now: now), "x")
    }
}

// MARK: - Answers and the re-raise

@MainActor
final class ApprovalAnswerTests: XCTestCase {
    /// A one-shot answer over its own connection reaches the hab with via
    /// banner; a second answer to the same request is refused.
    func testOneShotAnswerAndSecondRefused() async {
        let hab = await blockedHab()
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone", deadline: .seconds(5))
        let p = ApprovalAnswerParams(choice: .allow, pane: "%1", request: "req-1", via: .banner)
        let first = await answerer.answer(host: "testhost", p)
        XCTAssertEqual(first, .answered)
        let second = await answerer.answer(host: "testhost", p)
        guard case .refused = second else { return XCTFail("second answer: \(second)") }
        let answers = await hab.answers
        XCTAssertEqual(answers.count, 2)
        XCTAssertEqual(answers.first?.via, .banner)
        let hello = await hab.hellos.last
        XCTAssertEqual(hello?.label, "phone", "the label the hab's withdrawal names")
    }

    /// An online live connection answers without a new connection.
    func testLiveConnectionAnswers() async {
        let hab = await blockedHab()
        let conn = HabConnection(id: "h", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let answerer = ApprovalAnswerer(live: { _ in conn }, oneShot: { _ in throw TransportError("must not dial") }, clientLabel: "phone")
        let o = await answerer.answer(host: "testhost", ApprovalAnswerParams(choice: .deny, pane: "%1", request: "req-1", via: .app))
        XCTAssertEqual(o, .answered)
        let hellos = await hab.hellos.count
        XCTAssertEqual(hellos, 1, "no second connection")
        await conn.stop()
    }

    private func center(answerer: ApprovalAnswerer, board: Board? = nil) -> (ApprovalCenter, Recorder) {
        let rec = Recorder()
        let c = ApprovalCenter(board: board ?? Board(), answerer: answerer, defaults: emptyDefaults())
        c.post = { t, b, m in rec.posts.append((t, b, m)) }
        // These tests answer as an unlocked device. The default reads the simulator, whose
        // protected data is sometimes unavailable on a CI runner.
        c.isUnlocked = { true }
        return (c, rec)
    }

    final class Recorder {
        var posts: [(String, String, PushMeta?)] = []
    }

    /// An unreachable hab: "Could not reach", the request kept for the
    /// foreground, and the sheet opens once the hab is online with it open.
    func testUnreachableReraisesInForeground() async {
        let hab = await blockedHab()
        let fleet = Fleet()
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in throw TransportError("no route") }, clientLabel: "phone")
        let (c, rec) = center(answerer: answerer, board: fleet.board)
        fleet.approvals = c
        let meta = PushMeta(kind: "permission", hab: "testhost", pane: "%1", request: "req-1")
        let o = await c.answer(host: "testhost", ApprovalAnswerParams(choice: .allow, pane: "%1", request: "req-1", via: .banner), meta: meta)
        XCTAssertEqual(o, .unreachable("no route"))
        XCTAssertEqual(rec.posts.count, 1)
        XCTAssertEqual(rec.posts.first?.0, "Could not reach testhost")
        XCTAssertEqual(rec.posts.first?.2, meta, "the notice opens the request")
        XCTAssertEqual(c.pending, [meta])
        XCTAssertFalse(c.presenting)

        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "desk")
        await fleet.setActive(true)
        await eventually("sheet raised") { c.presenting }
        XCTAssertEqual(c.sheet.current?.requestID, "req-1")
        XCTAssertEqual(c.pending, [], "raised once")
        await fleet.setActive(false)
    }

    /// The hab refused: "Already answered", nothing raised again. Past the
    /// expiry the notice says to answer in the terminal.
    func testRefusedAndExpired() async {
        let hab = await blockedHab()
        await hab.failNextAnswer(kind: .conflict, hint: "request req-1 is not open")
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone")
        let (c, rec) = center(answerer: answerer)
        let meta = PushMeta(kind: "permission", hab: "testhost", pane: "%1", request: "req-1")
        let o = await c.answer(host: "testhost", ApprovalAnswerParams(choice: .allow, pane: "%1", request: "req-1"), meta: meta)
        XCTAssertEqual(o, .refused("request req-1 is not open"))
        XCTAssertEqual(rec.posts.first?.0, "Already answered")
        XCTAssertNil(rec.posts.first?.2)
        XCTAssertEqual(c.pending, [])

        let old = AnswerOutcome.refused("x").followUp(hab: "desk", expiry: Date(timeIntervalSinceNow: -5))
        XCTAssertEqual(old?.title, "Expired: answer in the terminal")
        XCTAssertNil(AnswerOutcome.answered.followUp(hab: "desk", expiry: nil))
    }

    /// A hab that could not deliver (unavailable) raises it again too.
    func testUnavailableReraises() async {
        let hab = await blockedHab()
        await hab.failNextAnswer(kind: .unavailable, hint: "the hook ended")
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone")
        let (c, rec) = center(answerer: answerer)
        let meta = PushMeta(kind: "permission", hab: "testhost", pane: "%1", request: "req-1")
        let o = await c.answer(host: "testhost", ApprovalAnswerParams(choice: .allow, pane: "%1", request: "req-1"), meta: meta)
        XCTAssertEqual(o, .failed("the hook ended"))
        XCTAssertEqual(rec.posts.first?.0, "Could not answer on testhost")
        XCTAssertEqual(c.pending, [meta])
    }
}

// MARK: - Focus cadence

@MainActor
final class FocusReporterTests: XCTestCase {
    final class Calls {
        var list: [(String, SessionFocusParams)] = []
        func of(_ s: String) -> [Bool] { list.filter { $0.1.session == s }.map(\.1.active) }
    }

    private func reporter(_ interval: Duration = .milliseconds(60)) -> (FocusReporter, Calls) {
        let calls = Calls()
        let r = FocusReporter(interval: interval) { hab, p in calls.list.append((hab, p)) }
        return (r, calls)
    }

    let s1 = SessionTarget(habID: "h", session: "s1", pane: "%1")
    let s2 = SessionTarget(habID: "h", session: "s2", pane: "%2")

    /// Nothing until the window is active; then true at once and again every
    /// interval; false when the view goes away, and the repeats stop.
    func testActiveRepeatsAndStops() async {
        let (r, calls) = reporter()
        r.show(s1)
        await r.flush()
        XCTAssertEqual(calls.of("s1"), [], "not active yet")
        r.setActive(true)
        await r.flush()
        XCTAssertEqual(calls.of("s1"), [true])
        await eventually("repeats") { calls.of("s1").count >= 3 }
        XCTAssertTrue(calls.of("s1").allSatisfy { $0 })
        r.hide(s1)
        await r.flush()
        XCTAssertEqual(calls.of("s1").last, false)
        let n = calls.list.count
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(calls.list.count, n, "no repeats after the view left")
    }

    /// Going to the background sends false for every session on screen;
    /// coming back sends true again.
    func testBackgroundSendsFalse() async {
        let (r, calls) = reporter(.seconds(60))
        r.setActive(true)
        r.show(s1)
        r.show(s2)
        r.setActive(false)
        await r.flush()
        XCTAssertEqual(calls.of("s1"), [true, false])
        XCTAssertEqual(calls.of("s2"), [true, false])
        r.setActive(true)
        await r.flush()
        XCTAssertEqual(calls.of("s1"), [true, false, true])
    }

    /// Moving from one session to another: false for the old, true for the new.
    /// The same session in two windows stays focused until both close.
    func testSwitchAndTwoWindows() async {
        let (r, calls) = reporter(.seconds(60))
        r.setActive(true)
        r.show(s1)
        r.show(s1)
        r.hide(s1)
        await r.flush()
        XCTAssertEqual(calls.of("s1"), [true], "still shown in the other window")
        r.hide(s1)
        r.show(s2)
        await r.flush()
        XCTAssertEqual(calls.of("s1"), [true, false])
        XCTAssertEqual(calls.of("s2"), [true])
    }

    /// An unfiled pane has no session: nothing to report. A reconnect repeats
    /// the focus to the hab's new client.
    func testUnfiledAndReconnect() async {
        let (r, calls) = reporter(.seconds(60))
        r.setActive(true)
        r.show(SessionTarget(habID: "h", session: nil, pane: "%9"))
        await r.flush()
        XCTAssertTrue(calls.list.isEmpty)
        r.show(s1)
        r.connectionOnline(habID: "h")
        r.connectionOnline(habID: "other")
        await r.flush()
        XCTAssertEqual(calls.of("s1"), [true, true])
    }

    /// Through the fleet: a session view on screen reports to its hab, and
    /// going to the background reports false before the connection closes.
    func testFleetReportsAndClearsOnBackground() async {
        let hab = await blockedHab()
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: nil)
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 1 }
        fleet.focus.show(s1)
        await eventually("true") { await hab.focusReports.last == SessionFocusParams(active: true, session: "s1") }
        await fleet.setActive(false)
        let last = await hab.focusReports.last
        XCTAssertEqual(last, SessionFocusParams(active: false, session: "s1"))
    }
}

// MARK: - The approval sheet

@MainActor
final class ApprovalSheetTests: XCTestCase {
    private func rig(_ hab: FakeHab, now: @escaping () -> Date = Date.init) async -> (Fleet, ApprovalSheetModel) {
        let fleet = Fleet()
        let conn = HabConnection(id: "h", transport: hab.transport(), timing: fastTiming())
        fleet.add(conn, label: "desk")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 1 }
        let m = ApprovalSheetModel(board: fleet.board, now: now) { _, p in
            do {
                _ = try await conn.call(HabMethods.approvalAnswer, p)
                return .answered
            } catch {
                return AnswerOutcome.classify(error)
            }
        }
        return (fleet, m)
    }

    /// Oldest first with "1 of 2"; Allow answers by id from the app and moves
    /// to the next; the last answer finishes the sheet.
    func testOldestFirstAnswerMovesOn() async {
        let t = Date()
        let hab = await blockedHab(requests: [("%1", "req-new", t), ("%2", "req-old", t.addingTimeInterval(-60))])
        let (fleet, m) = await rig(hab)
        XCTAssertTrue(m.show())
        XCTAssertEqual(m.current?.requestID, "req-old")
        XCTAssertEqual(m.position.index, 1)
        XCTAssertEqual(m.position.count, 2)
        XCTAssertEqual(m.current?.title, "Run make test?")
        XCTAssertEqual(m.current?.habLabel, "desk")
        XCTAssertEqual(m.current?.project, "tide-tables")
        XCTAssertEqual(m.phase, .open)
        XCTAssertTrue(m.canAnswer)
        await m.allow()
        let a = await hab.answers.last
        XCTAssertEqual(a, ApprovalAnswerParams(choice: .allow, pane: "%2", request: "req-old", via: .app))
        XCTAssertEqual(m.current?.requestID, "req-new")
        await m.deny()
        let d = await hab.answers.last
        XCTAssertEqual(d?.choice, .deny)
        XCTAssertTrue(m.finished)
        await fleet.setActive(false)
    }

    /// An answer on another device closes the request on the hab: the sheet's
    /// buttons turn off and it says so (app.md 7.2, 7.5).
    func testAnsweredElsewhereTurnsButtonsOff() async {
        let hab = await blockedHab()
        let (fleet, m) = await rig(hab)
        XCTAssertTrue(m.show(habID: "h", pane: "%1"))
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .working))
        await eventually("answered elsewhere") { m.phase == .answeredElsewhere }
        XCTAssertFalse(m.canAnswer)
        XCTAssertNotNil(m.current, "the sheet keeps the request on screen")
        await m.allow()
        let n = await hab.answers.count
        XCTAssertEqual(n, 0, "a closed request sends nothing")
        m.next()
        XCTAssertTrue(m.finished)
        await fleet.setActive(false)
    }

    /// Past expires_at the buttons turn off; the countdown runs before that.
    func testExpiry() async {
        let start = Date()
        var clock = start
        let hab = await blockedHab(expires: start.addingTimeInterval(90))
        let (fleet, m) = await rig(hab, now: { clock })
        m.show()
        XCTAssertEqual(m.remaining ?? 0, 90, accuracy: 1)
        XCTAssertTrue(m.canAnswer)
        clock = start.addingTimeInterval(91)
        XCTAssertEqual(m.phase, .expired)
        XCTAssertFalse(m.canAnswer)
        XCTAssertEqual(ApprovalSheet.countdown(252), "4m 12s")
        await fleet.setActive(false)
    }

    /// A failed answer keeps the buttons on with the reason; the hab got it once.
    func testFailureKeepsButtons() async {
        let hab = await blockedHab()
        await hab.failNextAnswer(kind: .unavailable, hint: "the hook ended")
        let (fleet, m) = await rig(hab)
        m.show()
        await m.allow()
        XCTAssertEqual(m.phase, .failed("the hook ended"))
        XCTAssertTrue(m.canAnswer)
        await m.allow()
        XCTAssertTrue(m.finished, "the retry the owner pressed went through")
        await fleet.setActive(false)
    }

    /// Claude's plan approval shows the plan; Approve plan is an allow.
    func testPlanApproval() async {
        let hab = await blockedHab(tool: "ExitPlanMode", input: .object(["plan": .string("1. Read\n2. Fix")]))
        let (fleet, m) = await rig(hab)
        m.show()
        XCTAssertEqual(m.current?.form, .plan("1. Read\n2. Fix"))
        XCTAssertEqual(m.current?.title, "Approve the plan?")
        XCTAssertEqual(m.current?.kindText, "plan")
        await m.allow()
        let a = await hab.answers.last
        XCTAssertEqual(a?.choice, .allow)
        await fleet.setActive(false)
    }

    /// AskUserQuestion shows its options; the answer is the chosen labels.
    /// The hab does not take answers yet, which the sheet reports.
    func testQuestionsAsStructuredAnswers() async {
        let q: JSONValue = .object(["questions": .array([
            .object(["question": .string("Which auth library?"), "header": .string("Auth"), "multiSelect": .bool(false),
                     "options": .array([.object(["label": .string("Clerk"), "description": .string("Hosted")]), .object(["label": .string("Auth.js")])])]),
        ])])
        let hab = await blockedHab(tool: "AskUserQuestion", input: q)
        let (fleet, m) = await rig(hab)
        m.show()
        guard case .questions(let qs)? = m.current?.form else { return XCTFail("form \(String(describing: m.current?.form))") }
        XCTAssertEqual(qs.first?.options.map(\.label), ["Clerk", "Auth.js"])
        XCTAssertEqual(qs.first?.options.first?.description, "Hosted")
        XCTAssertEqual(m.current?.title, "Which auth library?")
        await m.reply(RequestForm.answerText(qs, [["Clerk"]]))
        let a = await hab.answers.last
        XCTAssertEqual(a, ApprovalAnswerParams(choice: .answer, pane: "%1", request: "req-1", text: "Clerk", via: .app))
        guard case .failed = m.phase else { return XCTFail("phase \(m.phase)") }
        let two = [qs[0], RequestForm.Question(header: nil, question: "Tests?", options: [], multiSelect: true)]
        XCTAssertEqual(RequestForm.answerText(two, [["Clerk"], ["Unit", "UI"]]), "Auth: Clerk\nTests?: Unit, UI")
        await fleet.setActive(false)
    }

    /// A request with no id (from screen text) offers no answer.
    func testNoRequestIDNotAnswerable() async {
        let hab = await blockedHab(requests: [("%1", "", Date())])
        let (fleet, m) = await rig(hab)
        m.show()
        XCTAssertFalse(m.canAnswer)
        await fleet.setActive(false)
    }
}

// MARK: - Live Activity choices and links

@MainActor
final class LiveActivityTests: XCTestCase {
    let a = SessionTarget(habID: "h", session: "s1", pane: "%1")
    let b = SessionTarget(habID: "h", session: "s2", pane: "%2")

    /// The later of "last sent a message to" and "last needed the owner" wins.
    func testTrackedSession() {
        var p = ActivityPick()
        XCTAssertNil(p.tracked)
        let t = Date()
        p.sent = (a, t)
        XCTAssertEqual(p.tracked, a)
        p.needed = (b, t.addingTimeInterval(5))
        XCTAssertEqual(p.tracked, b)
        p.sent = (a, t.addingTimeInterval(10))
        XCTAssertEqual(p.tracked, a)
    }

    /// An activity starts only for a working or blocked session on a hab with
    /// push registered, and never a second one.
    func testShouldStart() {
        var row = BoardRow(id: "h/s/s1", target: a, host: "desk", title: "fix", state: .working, rank: .working)
        XCTAssertTrue(ActivityPick.shouldStart(row: row, pushRegistered: true, running: false))
        XCTAssertFalse(ActivityPick.shouldStart(row: row, pushRegistered: false, running: false), "no push: nothing would update it")
        XCTAssertFalse(ActivityPick.shouldStart(row: row, pushRegistered: true, running: true))
        row.state = .idle
        XCTAssertFalse(ActivityPick.shouldStart(row: row, pushRegistered: true, running: false))
        row.state = .blocked
        row.offline = true
        XCTAssertFalse(ActivityPick.shouldStart(row: row, pushRegistered: true, running: false))
        XCTAssertFalse(ActivityPick.shouldStart(row: nil, pushRegistered: true, running: false))
    }

    /// A session that newly needs the owner becomes the tracked one; a message
    /// sent later takes it back.
    func testTrackerFollowsBoard() async {
        let hab = await blockedHab()
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "desk")
        let reg = PushRegistrar(defaults: emptyDefaults())
        let tracker = LiveActivityTracker(board: fleet.board, push: reg, caller: ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in throw TransportError("x") }, clientLabel: "phone"), host: nil)
        fleet.activities = tracker
        await fleet.setActive(true)
        await eventually("needed") { tracker.pick.tracked?.session == "s1" }
        tracker.noteSent(b, at: Date().addingTimeInterval(1))
        XCTAssertEqual(tracker.pick.tracked, b)
        await fleet.setActive(false)
    }

    /// The Live Activity's link opens the sheet at its request.
    func testRequestLinkOpensSheet() async {
        let url = RequestLink.url(hab: "testhost", pane: "%1", request: "req-1", session: "s1")
        XCTAssertEqual(RequestLink.meta(url), PushMeta(kind: "permission", hab: "testhost", session: "s1", pane: "%1", request: "req-1"))
        XCTAssertNil(RequestLink.meta(URL(string: "flightdeck://pair?x=1")!))
        let hab = await blockedHab()
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "desk")
        let c = ApprovalCenter(board: fleet.board, answerer: ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in throw TransportError("x") }, clientLabel: "phone"), defaults: emptyDefaults())
        fleet.approvals = c
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 1 }
        XCTAssertTrue(c.open(url: url))
        XCTAssertTrue(c.presenting)
        XCTAssertEqual(c.sheet.current?.requestID, "req-1")
        XCTAssertFalse(c.open(url: URL(string: "flightdeck://pair?x=1")!), "a pairing link is not ours")
        await fleet.setActive(false)
    }
}
