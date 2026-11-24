import FlightdeckShared
import Foundation
import HabAPI
import UserNotifications
import XCTest
@testable import FlightdeckKit

/// Notification levels per kind and the foreground banner (app.md 8.9).
@MainActor
final class NotifyLevelTests: XCTestCase {
    func testKindsAndChoices() {
        XCTAssertEqual(NotifyKind.of(pushKind: "permission"), .approvals)
        XCTAssertEqual(NotifyKind.of(pushKind: "trust"), .approvals)
        XCTAssertEqual(NotifyKind.of(pushKind: "question"), .questions)
        XCTAssertEqual(NotifyKind.of(pushKind: "done"), .finished)
        for k in ["auth", "error", "notice", "round", "capcom", "answered"] {
            XCTAssertNil(NotifyKind.of(pushKind: k), k)
        }
        XCTAssertFalse(NotifyKind.approvals.choices.contains(.off), "an approval is never dropped silently")
        XCTAssertEqual(NotifyKind.questions.choices, NotifyLevel.allCases)
        XCTAssertEqual(NotifyKind.approvals.defaultLevel, .timeSensitive)
        XCTAssertEqual(NotifyKind.questions.defaultLevel, .timeSensitive)
        XCTAssertEqual(NotifyKind.finished.defaultLevel, .off)
    }

    func testLevelMapping() {
        XCTAssertNil(NotifyLevel.off.interruption)
        XCTAssertEqual(NotifyLevel.quiet.interruption, .passive)
        XCTAssertEqual(NotifyLevel.normal.interruption, .active)
        XCTAssertEqual(NotifyLevel.timeSensitive.interruption, .timeSensitive)
        XCTAssertEqual(NotifyLevel.quiet.foreground, [.list])
        XCTAssertEqual(NotifyLevel.normal.foreground, [.banner, .list, .sound])
        XCTAssertEqual(NotifyLevel.timeSensitive.rawValue, "time-sensitive", "the hab's word")
    }

    func testStoreRefusesOffForApprovalsAndMigratesTheDoneSwitch() {
        let d = emptyDefaults()
        let levels = NotifyLevels(defaults: d)
        levels.set(.off, for: .approvals)
        XCTAssertEqual(levels.level(.approvals), .timeSensitive)
        d.set("off", forKey: NotifyLevels.key(.approvals))
        XCTAssertEqual(levels.level(.approvals), .timeSensitive, "a stored off for approvals reads as the default")
        levels.set(.quiet, for: .approvals)
        XCTAssertEqual(levels.level(.approvals), .quiet)

        let old = emptyDefaults()
        old.set(true, forKey: NotifyLevels.legacyDoneKey)
        levels.migrate(from: old)
        XCTAssertEqual(levels.level(.finished), .quiet, "the old switch on is a quiet finished push, as the hab sent it")
        XCTAssertNil(old.object(forKey: NotifyLevels.legacyDoneKey))

        let d2 = emptyDefaults()
        let l2 = NotifyLevels(defaults: d2)
        l2.set(.normal, for: .finished)
        let old2 = emptyDefaults()
        old2.set(false, forKey: NotifyLevels.legacyDoneKey)
        l2.migrate(from: old2)
        XCTAssertEqual(l2.level(.finished), .normal, "a level already picked stays")
    }

    func testServiceExtensionStep() {
        let levels = NotifyLevels(defaults: emptyDefaults())
        levels.set(.quiet, for: .approvals)
        levels.set(.normal, for: .finished)
        let c = UNMutableNotificationContent()
        c.interruptionLevel = .timeSensitive
        c.sound = .default
        levels.apply(to: c, meta: PushMeta(kind: "permission", hab: "h"))
        XCTAssertEqual(c.interruptionLevel, .passive)
        XCTAssertNil(c.sound)

        let done = UNMutableNotificationContent()
        done.interruptionLevel = .passive
        levels.apply(to: done, meta: PushMeta(kind: "done", hab: "h"))
        XCTAssertEqual(done.interruptionLevel, .active)
        XCTAssertNotNil(done.sound)

        let error = UNMutableNotificationContent()
        error.interruptionLevel = .active
        levels.apply(to: error, meta: PushMeta(kind: "error", hab: "h"))
        XCTAssertEqual(error.interruptionLevel, .active, "no level covers errors: as the hab sent it")
        levels.apply(to: error, meta: nil)
        XCTAssertEqual(error.interruptionLevel, .active)
    }

    func testForegroundOptions() {
        let levels = NotifyLevels(defaults: emptyDefaults())
        levels.set(.quiet, for: .questions)
        levels.set(.off, for: .finished)
        let permission = PushMeta(kind: "permission", hab: "h", session: "s1")
        XCTAssertEqual(NotifyPresentation.options(meta: permission, remote: true, onScreen: false, levels: levels), [.banner, .list, .sound])
        XCTAssertEqual(NotifyPresentation.options(meta: permission, remote: true, onScreen: true, levels: levels), [], "the session on screen gets no banner")
        XCTAssertEqual(NotifyPresentation.options(meta: permission, remote: false, onScreen: true, levels: levels), [.banner, .list, .sound],
                       "the app's own notices always show")
        XCTAssertEqual(NotifyPresentation.options(meta: PushMeta(kind: "question", hab: "h"), remote: true, onScreen: false, levels: levels), [.list])
        XCTAssertEqual(NotifyPresentation.options(meta: PushMeta(kind: "done", hab: "h"), remote: true, onScreen: false, levels: levels), [.list],
                       "a push sent before the hab heard off shows quiet")
        XCTAssertEqual(NotifyPresentation.options(meta: PushMeta(kind: "notice", hab: "h"), remote: true, onScreen: false, levels: levels), [.banner, .list, .sound])
        XCTAssertEqual(NotifyPresentation.options(meta: nil, remote: true, onScreen: false, levels: levels), [.banner, .list, .sound])
    }

    /// Off leaves the kind out of `push.register`; the hab then sends none.
    func testOffLeavesTheKindOut() {
        let d = emptyDefaults()
        let reg = PushRegistrar(defaults: d, levels: NotifyLevels(defaults: d), environment: .development)
        XCTAssertTrue(reg.kinds.contains(.question))
        XCTAssertFalse(reg.kinds.contains(.done))
        reg.setLevel(.off, for: .questions)
        reg.setLevel(.timeSensitive, for: .finished)
        XCTAssertFalse(reg.kinds.contains(.question))
        XCTAssertTrue(reg.kinds.contains(.done))
        XCTAssertTrue(reg.kinds.contains(.permission))
        reg.setLevel(.off, for: .approvals)
        XCTAssertEqual(reg.level(.approvals), .timeSensitive)
        let again = PushRegistrar(defaults: d, levels: NotifyLevels(defaults: d), environment: .development)
        XCTAssertEqual(again.level(.questions), .off, "kept across launches")
    }

    /// The foreground check names the hab by host id; focus keys by connection.
    func testOnScreenFollowsTheFocusReport() {
        let board = Board()
        board.register(id: "conn-1", label: "Studio", endpoint: "fake")
        board.update("conn-1", linkHabState(host: "studio-host", agents: [("%1", "s1", .blocked, .permission)]))
        let fleet = Fleet(board: board)
        let meta = PushMeta(kind: "permission", hab: "studio-host", session: "s1")
        let target = SessionTarget(habID: "conn-1", session: "s1", pane: "%1")
        fleet.focus.show(target)
        XCTAssertFalse(fleet.onScreen(meta), "not in an active window")
        fleet.focus.setActive(true)
        XCTAssertTrue(fleet.onScreen(meta))
        XCTAssertFalse(fleet.onScreen(PushMeta(kind: "permission", hab: "studio-host", session: "s2")))
        XCTAssertFalse(fleet.onScreen(PushMeta(kind: "permission", hab: "other-host", session: "s1")))
        XCTAssertFalse(fleet.onScreen(PushMeta(kind: "notice", hab: "studio-host")), "no session named")
        fleet.focus.hide(target)
        XCTAssertFalse(fleet.onScreen(meta))
        fleet.focus.setActive(false)
    }
}

/// Sessions for Shortcuts and Spotlight, routes, and the needs-you widget (app.md 3.19, 15.4).
@MainActor
final class AppIntentLogicTests: XCTestCase {
    /// One hab, "Studio": session "tide" blocked on a permission with request id "req-1" for the
    /// command `ls`, and session "harbour" idle.
    private func board(online: Bool = true) -> Board {
        var st = HabState()
        st.status = online ? .online : .offline
        let req = Fixture.request(title: "Run?", tool: "Bash", command: "ls", id: "req-1")
        st.store.reset(StructureResult(
            agents: [Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission, request: req),
                     Fixture.agent(pane: "%2", harness: "claude", state: .idle, kind: nil, request: nil)],
            host: HostInfo(id: "studio-host"), panes: [Fixture.pane("%1", session: "tide"), Fixture.pane("%2", session: "harbour")],
            projects: [], seq: 0,
            sessions: [Fixture.session("tide", name: "tide", pane: "%1", project: nil), Fixture.session("harbour", name: "harbour", pane: "%2", project: nil)]))
        let b = Board()
        b.register(id: "conn-1", label: "Studio", endpoint: "fake")
        b.update("conn-1", st)
        return b
    }

    func testDirectoryKeepsIdsAndNamesOnly() throws {
        let d = emptyDefaults()
        let dir = SessionDirectory(defaults: d)
        var changes: [([SessionEntry], [String])] = []
        dir.onChange = { changes.append(($0, $1)) }
        dir.update(board())
        XCTAssertEqual(dir.entries.map(\.session), ["harbour", "tide"], "sorted by name")
        XCTAssertEqual(dir.entries.map(\.name), ["harbour", "tide"], "the session's name, never the request's title")
        XCTAssertEqual(dir.entries.first?.hab, "studio-host", "the host id, not the connection id")
        XCTAssertEqual(dir.entries.first?.habLabel, "Studio")
        XCTAssertEqual(dir.entries.first?.id, SessionLink(hab: "studio-host", session: "harbour").url.absoluteString)
        XCTAssertEqual(changes.count, 1)
        XCTAssertEqual(changes[0].0.count, 2)

        // Stored: ids and names only, never a command.
        let raw = try XCTUnwrap(d.data(forKey: SessionDirectory.key))
        let text = String(decoding: raw, as: UTF8.self)
        XCTAssertFalse(text.contains("ls"), "no command")
        XCTAssertEqual(Set(try XCTUnwrap(JSONSerialization.jsonObject(with: raw) as? [[String: Any]]).first!.keys),
                       ["hab", "session", "name", "habLabel"])

        // The same board again changes nothing.
        dir.update(board())
        XCTAssertEqual(changes.count, 1)

        // A later launch reads the list back while every connection is down.
        let next = SessionDirectory(defaults: d)
        XCTAssertEqual(next.entries, dir.entries)
        // An offline hab keeps its sessions.
        next.update(board(online: false))
        XCTAssertEqual(next.entries.count, 2)
        // A hab no longer in the app loses them, and Spotlight hears which.
        var deleted: [String] = []
        next.onChange = { _, d in deleted = d }
        let empty = Board()
        next.update(empty)
        XCTAssertTrue(next.entries.isEmpty)
        XCTAssertEqual(deleted.count, 2)
    }

    func testDirectoryQuery() {
        let dir = SessionDirectory(defaults: nil)
        dir.update(board())
        XCTAssertEqual(dir.matching("TIDE").map(\.session), ["tide"], "case does not matter")
        XCTAssertEqual(dir.matching("stud").count, 2, "the computer's name matches too")
        XCTAssertEqual(dir.matching("  ").count, 2)
        XCTAssertEqual(dir.matching("ar").map(\.session), ["harbour"])
        let ids = dir.entries.map(\.id)
        XCTAssertEqual(dir.entries(for: [ids[1], "flightdeck://session?hab=x&session=y", ids[0]]).map(\.id), [ids[1], ids[0]],
                       "the order asked, unknown ids left out")
    }

    func testDemoReachesNoDirectory() {
        let fleet = Fleet(board: board())
        fleet.directory = SessionDirectory(defaults: nil)
        fleet.isDemo = true
        fleet.publishSessions()
        XCTAssertTrue(fleet.directory?.entries.isEmpty ?? false)
        fleet.isDemo = false
        fleet.publishSessions()
        XCTAssertEqual(fleet.directory?.entries.count, 2)
    }

    func testRoutesWaitForAWindowAndLapse() {
        let routes = AppRoutes()
        XCTAssertNil(routes.take { _ in true })
        let t0 = Date()
        routes.post(.newSession, now: t0)
        XCTAssertEqual(routes.serial, 1)
        XCTAssertNil(routes.take(now: t0) { if case .session = $0 { true } else { false } }, "another taker leaves it")
        XCTAssertEqual(routes.take(now: t0) { _ in true }, .newSession)
        XCTAssertNil(routes.pending)
        routes.post(.capcom(message: nil), now: t0)
        XCTAssertNil(routes.take(now: t0.addingTimeInterval(SessionRestore.limit + 1)) { _ in true }, "a route a minute old lapses")
        XCTAssertNil(routes.pending)
    }

    func testWaitingRequest() {
        let fleet = Fleet(board: board())
        XCTAssertEqual(fleet.waitingRequest(hab: nil, session: nil)?.session, "tide")
        XCTAssertEqual(fleet.waitingRequest(hab: "studio-host", session: "tide")?.pane, "%1")
        XCTAssertNil(fleet.waitingRequest(hab: "studio-host", session: "harbour"), "that session waits for nothing")
        XCTAssertNil(fleet.waitingRequest(hab: "other-host", session: nil))
        XCTAssertFalse(fleet.boardIsCurrent, "the fleet is not active: nothing proves the board current")
        XCTAssertNil(Fleet(board: board(online: false)).waitingRequest(hab: nil, session: nil), "an offline hab's request may be gone")
    }

    func testNeedsYouSnapshotCarriesNoCommand() throws {
        let snap = NeedsYouPublisher.snapshot(board(), now: Date(timeIntervalSince1970: 1_000))
        XCTAssertEqual(snap.items.count, 1)
        let item = try XCTUnwrap(snap.items.first)
        XCTAssertEqual(item.kind, .approval)
        XCTAssertEqual(item.title, "tide")
        XCTAssertEqual(item.hab, "studio-host")
        XCTAssertEqual(item.url.host, "request")
        XCTAssertEqual(RequestLink.meta(item.url)?.request, item.request)
        let d = emptyDefaults()
        XCTAssertTrue(snap.save(d))
        XCTAssertFalse(snap.save(d), "unchanged: no reload")
        let raw = String(decoding: try XCTUnwrap(d.data(forKey: NeedsYouSnapshot.defaultsKey)), as: UTF8.self)
        XCTAssertFalse(raw.contains("\"ls\""), "no command reaches the widget")

        var reloads = 0
        NeedsYouPublisher.drop(request: try XCTUnwrap(item.request), defaults: d) { reloads += 1 }
        XCTAssertEqual(reloads, 1)
        XCTAssertEqual(NeedsYouSnapshot.load(d)?.items.count, 0)
        NeedsYouPublisher.drop(request: "gone", defaults: d) { reloads += 1 }
        XCTAssertEqual(reloads, 1, "an unknown request changes nothing")
    }

    /// With two habs, the stop of one on the way to the background must not drop its items.
    func testNeedsYouWritesOnlyWhileTheAppIsInFront() async {
        let b = board()
        var st = HabState()
        st.status = .online
        let req = Fixture.request(title: "Edit?", tool: "Edit", id: "req-2")
        st.store.reset(StructureResult(
            agents: [Fixture.agent(pane: "%9", harness: "codex", state: .blocked, kind: .permission, request: req)],
            host: HostInfo(id: "bench-host"), panes: [Fixture.pane("%9", session: "reef")], projects: [], seq: 0,
            sessions: [Fixture.session("reef", name: "reef", pane: "%9", project: nil)]))
        b.register(id: "conn-2", label: "Bench", endpoint: "fake")
        b.update("conn-2", st)
        let d = emptyDefaults()
        let fleet = Fleet(board: b)
        fleet.needsYou = NeedsYouPublisher(defaults: d) {}
        fleet.publishSessions()
        XCTAssertNil(NeedsYouSnapshot.load(d), "not in front: nothing written")
        await fleet.setActive(true)
        fleet.publishSessions()
        XCTAssertEqual(Set(NeedsYouSnapshot.load(d)?.items.map(\.hab) ?? []), ["studio-host", "bench-host"])
        await fleet.setActive(false)
        st.status = .offline
        b.update("conn-2", st)
        fleet.publishSessions()
        XCTAssertEqual(NeedsYouSnapshot.load(d)?.items.count, 2, "a hab stopped on the way out keeps its items")
    }

    /// Ask Capcom with a message when it cannot go: the reason is set, so the app shows it with
    /// the text; nothing is dropped silently.
    func testAskCapcomFailureKeepsAReason() async {
        let entry = QuickEntry(capcom: Fleet().capcom, defaults: emptyDefaults())
        entry.wait = .milliseconds(100)
        entry.text = "check the build"
        let ok = await entry.sendWhenReady()
        XCTAssertFalse(ok)
        XCTAssertNotNil(entry.failure)
        XCTAssertEqual(entry.text, "check the build")
    }

    func testNeedsYouKeepsTheLastListWhileDisconnected() {
        let d = emptyDefaults()
        var reloads = 0
        let p = NeedsYouPublisher(defaults: d) { reloads += 1 }
        p.update(board())
        XCTAssertEqual(reloads, 1)
        p.update(board(online: false))
        XCTAssertEqual(reloads, 1)
        XCTAssertEqual(NeedsYouSnapshot.load(d)?.items.count, 1, "the background keeps the last list")
    }
}
