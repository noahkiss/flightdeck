import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Settings and levels (app.md 20): level matching, Custom, exceptions, the
/// queue, drop-and-resend, the new-hab check, and the center's level pick.
final class SettingsTests: XCTestCase {
    typealias R = SettingsRegistry

    func setting(_ id: String, _ v: JSONValue, rev: UInt64 = 1, source: SettingSource = .default) -> Setting {
        let d = R.def(id)?.levels?[.balanced] ?? v
        return Setting(default: d, id: id, revision: rev, source: source, value: v)
    }

    /// A hab at Balanced: every levelled hab-side row at its default.
    func balancedHab(_ id: String = "h1", label: String = "alpha", online: Bool = true, epoch: String = "e1") -> HabSettingsView {
        var s: [String: Setting] = [:]
        for d in R.levelledHabSide { s[d.id] = setting(d.id, d.levels![.balanced]!) }
        return HabSettingsView(habID: id, label: label, online: online, hasSettings: true, epoch: epoch, settings: s)
    }

    func app(_ level: SettingLevel) -> [String: JSONValue] {
        var out: [String: JSONValue] = [:]
        for d in R.levelled where d.store == .app { out[d.id] = d.levels![level]! }
        return out
    }

    // MARK: - Level matching

    /// confirm.start_elsewhere: on at Safe, off at Balanced and Bypass (app.md 20.3).
    func testStartElsewhereOffAtBalanced() {
        let levels = R.def(R.startElsewhere)?.levels
        XCTAssertEqual(levels?[.safe], .bool(true))
        XCTAssertEqual(levels?[.balanced], .bool(false))
        XCTAssertEqual(levels?[.bypass], .bool(false))
    }

    /// confirm.group_plan: on at Safe and Balanced, off at Bypass, kept on every hab (groups.md 3.2).
    func testGroupPlanLevels() throws {
        let d = try XCTUnwrap(R.def(R.groupPlan))
        XCTAssertEqual(d.levels?[.safe], .bool(true))
        XCTAssertEqual(d.levels?[.balanced], .bool(true))
        XCTAssertEqual(d.levels?[.bypass], .bool(false))
        XCTAssertEqual(d.store, .everyHab)
        XCTAssertTrue(R.levelledHabSide.contains { $0.id == R.groupPlan })
    }

    /// agent.read_panes: project at Safe, all at Balanced and Bypass, kept on every hab
    /// (hab-logic-api.md 3.11, app.md 20.3).
    func testAgentReadPanesLevels() throws {
        let d = try XCTUnwrap(R.def(R.agentReadPanes))
        XCTAssertEqual(d.levels?[.safe], .string("project"))
        XCTAssertEqual(d.levels?[.balanced], .string("all"))
        XCTAssertEqual(d.levels?[.bypass], .string("all"))
        XCTAssertEqual(d.store, .everyHab)
        XCTAssertTrue(R.levelledHabSide.contains { $0.id == R.agentReadPanes })
    }

    /// agent.drive_panes: off at Safe, agents at Balanced, all at Bypass; kept on every hab (hab-logic-api.md 3.12, app.md 20.3).
    func testAgentDrivePanesLevels() throws {
        let d = try XCTUnwrap(R.def(R.agentDrivePanes))
        XCTAssertEqual(d.levels?[.safe], .string("off"))
        XCTAssertEqual(d.levels?[.balanced], .string("agents"))
        XCTAssertEqual(d.levels?[.bypass], .string("all"))
        XCTAssertEqual(d.store, .everyHab)
        XCTAssertTrue(R.levelledHabSide.contains { $0.id == R.agentDrivePanes })
    }

    /// capcom.event_actions: ask at Safe, allow at Balanced, all at Bypass (from A10), and a level
    /// pick writes it (capcom-service.md 9.4, 9.6, app.md 20.3).
    @MainActor
    func testCapcomEventActionsLevels() async throws {
        let d = try XCTUnwrap(R.def(R.capcomEventActions))
        XCTAssertEqual(d.levels?[.safe], .string("ask"))
        XCTAssertEqual(d.levels?[.balanced], .string("allow"))
        XCTAssertEqual(d.levels?[.bypass], .string("all"))
        XCTAssertEqual(d.store, .everyHab)
        var sent: [SettingWrite] = []
        let (c, _) = try center("SettingsTests.eventactions") { _, p in
            sent += p.values
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: $0.revision + 1, source: .client) })
        }
        let h = balancedHab()
        c.update(habID: h.habID, label: h.label, state: state(h))
        await c.pick(.safe)
        XCTAssertTrue(sent.contains(SettingWrite(id: R.capcomEventActions, revision: 1, value: .string("ask"))))
        sent = []
        await c.pick(.bypass)
        XCTAssertTrue(sent.contains { $0.id == R.capcomEventActions && $0.value == .string("all") }, "Bypass writes all")
    }

    /// A Capcom job's action card names the job, the word and the call (capcom-service.md 9.8).
    func testActionCardFacts() {
        let a = ConfirmAction(job: "host-b/mail", method: "channel.send", params: "{\"to\":\"web\"}", summary: "message web: fix it", word: .handOff)
        let facts = ConfirmCardContent.actionFacts(a)
        XCTAssertEqual(facts.map(\.label), ["Job", "Action", "Call"])
        XCTAssertEqual(facts.map(\.value), ["host-b/mail", "hand_off", "channel.send"])
    }

    /// The four group caps are preferences kept on every hab, with the hab's ranges (groups.md 15.3).
    func testGroupPreferenceRows() throws {
        let want: [(String, SettingKind)] = [
            (R.groupConfirmMaxAge, .integer(1...1440, unit: "min")),
            (R.groupMaxLeads, .integer(1...10, unit: "")),
            (R.groupMaxWorkers, .integer(0...5, unit: "")),
            (R.groupCloseMembers, .bool),
        ]
        for (id, kind) in want {
            let d = try XCTUnwrap(R.def(id), id)
            XCTAssertFalse(d.isLevelled, id)
            XCTAssertEqual(d.store, .everyHab, id)
            XCTAssertEqual(d.kind, kind, id)
            XCTAssertEqual(d.area, .agents, id)
            XCTAssertTrue(d.built, id)
        }
    }

    /// The minutes picker offers only values inside the row's range, and keeps a value set elsewhere.
    func testMinuteChoices() {
        XCTAssertEqual(ValueControl.minuteChoices(1...1440, current: 60), [15, 30, 60, 120, 240, 480, 1440])
        XCTAssertEqual(ValueControl.minuteChoices(1...1440, current: 5), [5, 15, 30, 60, 120, 240, 480, 1440])
        XCTAssertEqual(ValueControl.minuteChoices(0...1440, current: -1), ValueControl.minutes)
    }

    /// Picking a level never touches a group cap: they are preferences.
    @MainActor
    func testPickLeavesGroupCaps() async throws {
        var sent: [SettingWrite] = []
        let (c, _) = try center("SettingsTests.groupcaps") { _, p in
            sent += p.values
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: $0.revision + 1, source: .client) })
        }
        var h = balancedHab()
        h.settings[R.groupMaxLeads] = setting(R.groupMaxLeads, .int(3))
        c.update(habID: h.habID, label: h.label, state: state(h))
        await c.pick(.bypass)
        XCTAssertTrue(sent.contains(SettingWrite(id: R.groupPlan, revision: 1, value: .bool(false))))
        XCTAssertFalse(sent.contains { $0.id == R.groupMaxLeads })
        sent = []
        await c.setEveryHab(R.groupMaxLeads, .int(5))
        XCTAssertEqual(sent, [SettingWrite(id: R.groupMaxLeads, revision: 1, value: .int(5))])
    }

    func testEachColumnMatchesItsLevel() {
        XCTAssertEqual(LevelMatcher.match(app: app(.balanced), habs: [balancedHab()]).level, .balanced)
        var h = balancedHab()
        for d in R.levelledHabSide { h.settings[d.id]?.value = d.levels![.safe]! }
        XCTAssertEqual(LevelMatcher.match(app: app(.safe), habs: [h]).level, .safe)
        XCTAssertEqual(LevelMatcher.match(app: app(.safe), habs: [h]).title, "Safe")
    }

    func testOneChangedRowIsCustomAndBackIsTheLevel() {
        var a = app(.balanced)
        a[R.voiceYesAllows] = .bool(false)
        XCTAssertNil(LevelMatcher.match(app: a, habs: [balancedHab()]).level)
        XCTAssertEqual(LevelMatcher.match(app: a, habs: [balancedHab()]).title, "Custom")
        a[R.voiceYesAllows] = .bool(true)
        XCTAssertEqual(LevelMatcher.match(app: a, habs: [balancedHab()]).level, .balanced)
    }

    /// A setting the device does not show (Face ID on the Mac) is left out.
    func testRowNotShownIsLeftOut() {
        var a = app(.balanced)
        a[R.sshRequireFaceID] = nil
        XCTAssertEqual(LevelMatcher.match(app: a, habs: [balancedHab()]).level, .balanced)
    }

    /// A hab-side write from another device flips the picker to Custom.
    func testOnlineHabValueBreaksTheMatch() {
        var h = balancedHab()
        h.settings[R.logbookApproval]?.value = .string("agents")
        h.settings[R.logbookApproval]?.source = .client
        XCTAssertNil(LevelMatcher.match(app: app(.balanced), habs: [h, balancedHab("h2", label: "beta")]).level)
    }

    /// A locked row and an offline hab do not break the match; each that
    /// differs is an exception.
    func testLockedAndOfflineRowsAreExceptions() {
        var safe = balancedHab()
        for d in R.levelledHabSide { safe.settings[d.id]?.value = d.levels![.safe]! }
        var locked = safe
        locked.habID = "h2"
        locked.label = "beta"
        locked.settings[R.startElsewhere] = setting(R.startElsewhere, .bool(false), rev: 2, source: .config)
        var offline = balancedHab("h3", label: "gamma", online: false)
        offline.settings[R.startElsewhere]?.value = .bool(true)
        offline.settings[R.capcomEventActions]?.value = .string("ask")
        offline.settings[R.agentReadPanes]?.value = .string("project")
        offline.settings[R.agentDrivePanes]?.value = .string("off")
        let st = LevelMatcher.match(app: app(.safe), habs: [safe, locked, offline])
        XCTAssertEqual(st.level, .safe)
        XCTAssertEqual(st.exceptions.map(\.habLabel), ["beta", "gamma"])
        XCTAssertEqual(st.exceptions.map(\.reason), [.locked, .offline])
        XCTAssertEqual(st.exceptions.first?.settingID, R.startElsewhere)
        XCTAssertEqual(st.title, "Safe, 2 exceptions")
    }

    func testUnknownSourceReadsAsLocked() {
        XCTAssertTrue(setting(R.startElsewhere, .bool(true), source: SettingSource(rawValue: "policy")).isLocked)
        XCTAssertFalse(setting(R.startElsewhere, .bool(true), source: .client).isLocked)
    }

    /// A hab without `settings` is left out; an offline hab's queued value counts.
    func testHabWithoutSettingsAndQueuedValue() {
        let bare = HabSettingsView(habID: "old", label: "old", online: true, hasSettings: false)
        XCTAssertEqual(LevelMatcher.match(app: app(.balanced), habs: [balancedHab(), bare]).level, .balanced)
        let off = balancedHab("h2", label: "beta", online: false)
        let q = QueuedSettingWrite(habID: "h2", settingID: R.logbookApproval, value: .string("agents"), revision: 1, epoch: "e1", seen: .string("off"))
        let st = LevelMatcher.match(app: app(.balanced), habs: [balancedHab(), off], queued: [q])
        XCTAssertEqual(st.level, .balanced)
        XCTAssertEqual(st.exceptions.count, 1, "the queued value differs on the offline hab")
    }

    /// Two columns that the rows share: the picked level shows.
    func testPreferredLevelWinsATie() {
        let lockedEverywhere = HabSettingsView(habID: "h", label: "h", online: true, hasSettings: true, settings: [
            R.startElsewhere: setting(R.startElsewhere, .bool(true), source: .config),
        ])
        XCTAssertEqual(LevelMatcher.match(defs: [R.def(R.startElsewhere)!], app: [:], habs: [lockedEverywhere], prefer: .bypass).level, .bypass)
        XCTAssertEqual(LevelMatcher.match(defs: [R.def(R.startElsewhere)!], app: [:], habs: [lockedEverywhere]).level, .safe)
    }

    func testRowStatusDiffersAndLocked() {
        var b = balancedHab("h2", label: "beta")
        b.settings[R.approvalMaxAge] = setting(R.approvalMaxAge, .int(5), rev: 2, source: .config)
        let row = LevelMatcher.row(R.approvalMaxAge, habs: [balancedHab(), b])
        XCTAssertEqual(row.value, .int(60))
        XCTAssertEqual(row.differsOn, ["beta"])
        XCTAssertEqual(row.lockedOn, ["beta"])
        XCTAssertTrue(LevelMatcher.row("newer.setting", habs: [balancedHab()]).unavailable)
    }

    // MARK: - Queue

    func testQueueReplacesAndSurvivesARelaunch() throws {
        let d = try XCTUnwrap(UserDefaults(suiteName: "SettingsTests.queue"))
        d.removePersistentDomain(forName: "SettingsTests.queue")
        var q = SettingsQueue()
        q.put(QueuedSettingWrite(habID: "h", settingID: R.startElsewhere, value: .bool(false), revision: 1, epoch: "e", seen: .bool(true)))
        q.put(QueuedSettingWrite(habID: "h", settingID: R.startElsewhere, value: .bool(true), revision: 1, epoch: "e", seen: .bool(true)))
        q.put(QueuedSettingWrite(habID: "g", settingID: R.startElsewhere, value: .bool(false), revision: 3, epoch: "x", seen: nil))
        XCTAssertEqual(q.items.count, 2)
        XCTAssertEqual(q.items(for: "h").first?.value, .bool(true))
        q.save(d)
        XCTAssertEqual(SettingsQueue.load(d), q)
    }

    // MARK: - Connect plan

    func testSameEpochSendsWithItsRevision() {
        var h = balancedHab()
        h.settings[R.startElsewhere]?.revision = 4
        let q = QueuedSettingWrite(habID: "h1", settingID: R.startElsewhere, value: .bool(true), revision: 3, epoch: "e1", seen: .bool(false))
        let p = SettingsWrites.connectPlan(hab: h, queued: [q], picked: nil)
        XCTAssertEqual(p.send, [SettingWrite(id: R.startElsewhere, revision: 3, value: .bool(true))], "the hab answers conflict if another device wrote since")
        XCTAssertEqual(p.settled, [R.startElsewhere])
    }

    func testNewEpochComparesTheSnapshotValue() {
        var h = balancedHab(epoch: "e2")
        h.settings[R.startElsewhere]?.revision = 1
        let kept = QueuedSettingWrite(habID: "h1", settingID: R.startElsewhere, value: .bool(true), revision: 9, epoch: "e1", seen: .bool(false))
        XCTAssertEqual(SettingsWrites.connectPlan(hab: h, queued: [kept], picked: nil).send,
                       [SettingWrite(id: R.startElsewhere, revision: 1, value: .bool(true))])
        let lost = QueuedSettingWrite(habID: "h1", settingID: R.startElsewhere, value: .bool(true), revision: 9, epoch: "e1", seen: .bool(true))
        h.settings[R.startElsewhere]?.value = .bool(false)
        h.settings[R.startElsewhere]?.source = .client
        let p = SettingsWrites.connectPlan(hab: h, queued: [lost], picked: nil)
        XCTAssertTrue(p.send.isEmpty)
        XCTAssertEqual(p.dropped, [DroppedSetting(settingID: R.startElsewhere, habID: "h1", reason: .changedElsewhere)])
    }

    func testQueuedWriteToALockedRowDrops() {
        var h = balancedHab()
        h.settings[R.logbookApproval] = setting(R.logbookApproval, .string("off"), rev: 2, source: .config)
        let q = QueuedSettingWrite(habID: "h1", settingID: R.logbookApproval, value: .string("agents"), revision: 1, epoch: "e1", seen: .string("off"))
        XCTAssertEqual(SettingsWrites.connectPlan(hab: h, queued: [q], picked: nil).dropped.map(\.reason), [.locked])
    }

    /// The new-hab check: a row still at `default` takes the picked level;
    /// a client-written or locked row stands; a queued row is left to the queue.
    func testNewHabCheck() {
        var h = balancedHab()
        h.settings[R.approvalMaxAge] = setting(R.approvalMaxAge, .int(30), rev: 3, source: .client)
        h.settings[R.startElsewhere] = setting(R.startElsewhere, .bool(true), rev: 2, source: .config)
        h.settings[R.capcomEventActions] = setting(R.capcomEventActions, .string("allow"), rev: 2, source: .client)
        h.settings[R.agentReadPanes] = setting(R.agentReadPanes, .string("all"), rev: 2, source: .client)
        h.settings[R.agentDrivePanes] = setting(R.agentDrivePanes, .string("project"), rev: 2, source: .client)
        let q = QueuedSettingWrite(habID: "h1", settingID: R.logbookApproval, value: .string("off"), revision: 1, epoch: "e1", seen: .string("off"))
        XCTAssertTrue(SettingsWrites.connectPlan(hab: h, queued: [q], picked: .safe).send.isEmpty, "logbook is queued; the others stand")
        let p = SettingsWrites.connectPlan(hab: h, queued: [], picked: .safe)
        XCTAssertEqual(p.send, [SettingWrite(id: R.logbookApproval, revision: 1, value: .string("agents"))])
        XCTAssertTrue(SettingsWrites.connectPlan(hab: balancedHab(), queued: [], picked: .balanced).send.isEmpty)
        XCTAssertTrue(SettingsWrites.connectPlan(hab: balancedHab(), queued: [], picked: nil).send.isEmpty)
        let bare = HabSettingsView(habID: "old", label: "old", online: true, hasSettings: false)
        XCTAssertTrue(SettingsWrites.connectPlan(hab: bare, queued: [], picked: .safe).send.isEmpty)
    }

    // MARK: - Drop and resend once

    func refusal(_ kind: ErrorKind, _ id: String) -> RPCError {
        RPCError(code: -32000, message: kind.rawValue, data: ErrorData(hint: kind.rawValue, id: id, kind: kind, seq: 1))
    }

    func rows(_ ids: String...) -> [SettingWrite] { ids.map { SettingWrite(id: $0, revision: 1, value: .bool(true)) } }

    func testDropAndResendOnce() async {
        var calls: [[String]] = []
        let out = await SettingsWrites.send(rows("a", "b", "c"), habID: "h") { p in
            calls.append(p.values.map(\.id))
            if calls.count == 1 { throw self.refusal(.conflict, "b") }
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: 2, source: .client) })
        }
        XCTAssertEqual(calls, [["a", "b", "c"], ["a", "c"]], "the rest goes once more, revisions unchanged")
        XCTAssertEqual(out.written.map(\.id), ["a", "c"])
        XCTAssertEqual(out.dropped, [DroppedSetting(settingID: "b", habID: "h", reason: .changedElsewhere)])
    }

    func testSecondRefusalEndsTheTry() async {
        var n = 0
        let out = await SettingsWrites.send(rows("a", "b", "c"), habID: "h") { _ in
            n += 1
            throw n == 1 ? self.refusal(.locked, "a") : self.refusal(.notFound, "c")
        }
        XCTAssertEqual(n, 2)
        XCTAssertTrue(out.written.isEmpty)
        XCTAssertEqual(out.dropped.map(\.reason), [.locked, .notOnHab, .notTried])
        XCTAssertEqual(out.dropped.map(\.settingID), ["a", "c", "b"])
    }

    func testTransportFailureLeavesRowsUnreached() async {
        let out = await SettingsWrites.send(rows("a"), habID: "h") { _ in throw TransportError("gone") }
        XCTAssertEqual(out.unreached.map(\.id), ["a"])
    }

    func testNotice() {
        XCTAssertEqual(SettingsWrites.levelNotice(.safe, changed: 3, total: 3, sentences: []), "Safe: 3 settings changed")
        let s = SettingsWrites.sentence(DroppedSetting(settingID: R.logbookApproval, habID: "h", reason: .changedElsewhere), habLabel: "alpha")
        XCTAssertEqual(SettingsWrites.levelNotice(.safe, changed: 2, total: 3, sentences: [s]),
                       "Safe: 2 of 3 changed. Require approval for memories on alpha was changed from another device.")
    }

    // MARK: - Center

    @MainActor
    func center(_ suite: String, setter: @escaping (String, SettingsSetParams) async throws -> SettingsResult) throws -> (SettingsCenter, UserDefaults) {
        let d = try XCTUnwrap(UserDefaults(suiteName: suite))
        d.removePersistentDomain(forName: suite)
        return (SettingsCenter(defaults: d, setter: setter), d)
    }

    func state(_ v: HabSettingsView) -> HabState {
        var s = HabState()
        s.status = v.online ? .online : .offline
        s.welcome = Welcome(build: "test", capabilities: v.hasSettings ? ["settings"] : [], clientId: "c", epoch: v.epoch ?? "e",
                            host: HostInfo(id: v.label), reconnectToken: "t", revision: 1, seq: 0)
        s.settings = v.settings
        return s
    }

    /// A level pick writes each online hab at once, queues the offline one,
    /// applies the app-side rows, keeps a refused Face ID row, and names it.
    @MainActor
    func testPickWritesQueuesAndReports() async throws {
        var sent: [String: [SettingWrite]] = [:]
        let (c, d) = try center("SettingsTests.pick") { hab, p in
            sent[hab, default: []] += p.values
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: $0.revision + 1, source: .client) })
        }
        var yes = true
        var faceID = false
        c.ports[R.voiceYesAllows] = AppSettingPort(get: { .bool(yes) }, set: { if case .bool(let b) = $0 { yes = b } })
        c.ports[R.sshRequireFaceID] = AppSettingPort(get: { .bool(faceID) }, set: { _ in throw TransportError("refused") })
        let on = balancedHab()
        let off = balancedHab("h2", label: "beta", online: false)
        c.update(habID: on.habID, label: on.label, state: state(on))
        c.update(habID: off.habID, label: off.label, state: state(off))
        await c.pick(.safe)
        XCTAssertEqual(sent["h1"], [SettingWrite(id: R.startElsewhere, revision: 1, value: .bool(true)),
                                     SettingWrite(id: R.logbookApproval, revision: 1, value: .string("agents")),
                                     SettingWrite(id: R.capcomEventActions, revision: 1, value: .string("ask")),
                                     SettingWrite(id: R.agentReadPanes, revision: 1, value: .string("project")),
                                     SettingWrite(id: R.agentDrivePanes, revision: 1, value: .string("off"))])
        XCTAssertNil(sent["h2"])
        XCTAssertEqual(c.queue.items.map(\.habID), ["h2", "h2", "h2", "h2", "h2"])
        XCTAssertFalse(yes)
        XCTAssertFalse(faceID)
        // Rows count once each, however many habs take them: the five hab-side rows, "Yes"
        // counts as Allow, and on iOS the refused Face ID row (macOS does not show it).
        #if os(iOS)
        XCTAssertEqual(c.notice, "Safe: 6 of 7 changed. Require Face ID for SSH keys did not change: authentication was refused. "
                       + "beta gets the change when it connects.")
        #else
        XCTAssertEqual(c.notice, "Safe: 6 settings changed. beta gets the change when it connects.")
        #endif
        XCTAssertEqual(SettingsQueue.load(d).items.count, 5, "the queue is saved")
        XCTAssertEqual(d.string(forKey: SettingsCenter.levelKey), "safe")

        // The offline hab connects in the same epoch: its queued write goes, and the queue empties.
        c.update(habID: "h2", label: "beta", state: state(balancedHab("h2", label: "beta")))
        await c.connectionOnline(habID: "h2")
        XCTAssertEqual(sent["h2"], [SettingWrite(id: R.startElsewhere, revision: 1, value: .bool(true)),
                                     SettingWrite(id: R.logbookApproval, revision: 1, value: .string("agents")),
                                     SettingWrite(id: R.capcomEventActions, revision: 1, value: .string("ask")),
                                     SettingWrite(id: R.agentReadPanes, revision: 1, value: .string("project")),
                                     SettingWrite(id: R.agentDrivePanes, revision: 1, value: .string("off"))])
        XCTAssertTrue(c.queue.items.isEmpty)
    }

    /// The new-hab check runs on every connect with the last picked level.
    @MainActor
    func testConnectAppliesThePickedLevelToDefaults() async throws {
        var sent: [SettingWrite] = []
        let d = try XCTUnwrap(UserDefaults(suiteName: "SettingsTests.newhab"))
        d.removePersistentDomain(forName: "SettingsTests.newhab")
        d.set("bypass", forKey: SettingsCenter.levelKey)
        let c2 = SettingsCenter(defaults: d) { _, p in sent += p.values; return SettingsResult(seq: 1, settings: []) }
        let h = balancedHab()
        c2.update(habID: h.habID, label: h.label, state: state(h))
        await c2.connectionOnline(habID: h.habID)
        XCTAssertEqual(Set(sent.map(\.id)), [R.approvalMaxAge, R.groupPlan, R.agentDrivePanes, R.capcomEventActions],
                       "Bypass turns the plan card off too, lets agents drive every pane, and lets event turns act (A10)")
    }

    /// A hab whose capcom.event_actions a client already wrote as allow keeps it at Bypass until
    /// the user picks a level again, and the row shows it differs (capcom-service.md 18 A10).
    @MainActor
    func testEventActionsWrittenAllowKept() async throws {
        var sent: [SettingWrite] = []
        let d = try XCTUnwrap(UserDefaults(suiteName: "SettingsTests.keepallow"))
        d.removePersistentDomain(forName: "SettingsTests.keepallow")
        d.set("bypass", forKey: SettingsCenter.levelKey)
        let c2 = SettingsCenter(defaults: d) { _, p in sent += p.values; return SettingsResult(seq: 1, settings: []) }
        var h = balancedHab()
        h.settings[R.capcomEventActions] = setting(R.capcomEventActions, .string("allow"), rev: 3, source: .client)
        c2.update(habID: h.habID, label: h.label, state: state(h))
        await c2.connectionOnline(habID: h.habID)
        XCTAssertFalse(sent.contains { $0.id == R.capcomEventActions }, "a client's allow stays")
        var b = balancedHab("h2", label: "beta")
        b.settings[R.capcomEventActions] = setting(R.capcomEventActions, .string("all"), rev: 2, source: .client)
        let row = LevelMatcher.row(R.capcomEventActions, habs: [b, h])
        XCTAssertEqual(row.differsOn, ["alpha"])
    }

    /// A refused queued write on connect is dropped and named in the notice.
    @MainActor
    func testConnectDropAndNotice() async throws {
        let d = try XCTUnwrap(UserDefaults(suiteName: "SettingsTests.drop"))
        d.removePersistentDomain(forName: "SettingsTests.drop")
        var q = SettingsQueue()
        q.put(QueuedSettingWrite(habID: "h1", settingID: R.startElsewhere, value: .bool(true), revision: 1, epoch: "e1", seen: .bool(false)))
        q.save(d)
        let c2 = SettingsCenter(defaults: d) { _, _ in throw self.refusal(.conflict, R.startElsewhere) }
        XCTAssertEqual(c2.queue.items.count, 1, "the queue survives a relaunch")
        let h = balancedHab()
        c2.update(habID: h.habID, label: h.label, state: state(h))
        await c2.connectionOnline(habID: h.habID)
        XCTAssertTrue(c2.queue.items.isEmpty)
        XCTAssertEqual(c2.notice, "Confirm before an agent starts work on another host on alpha was changed from another device.")
    }

    // MARK: - Review fixes

    /// S4: a same-epoch queued write whose value is in place at a newer revision (the answer was
    /// lost) is cleared with nothing sent.
    func testSameEpochValueInPlaceSendsNothing() {
        var h = balancedHab()
        h.settings[R.startElsewhere] = setting(R.startElsewhere, .bool(false), rev: 4, source: .client)
        let q = QueuedSettingWrite(habID: "h1", settingID: R.startElsewhere, value: .bool(false), revision: 3, epoch: "e1", seen: .bool(true))
        let p = SettingsWrites.connectPlan(hab: h, queued: [q], picked: nil)
        XCTAssertTrue(p.send.isEmpty)
        XCTAssertTrue(p.dropped.isEmpty)
        XCTAssertEqual(p.settled, [R.startElsewhere])
    }

    /// N3: with no level picked, the matching level with the fewest exceptions wins.
    func testFewestExceptionsWinsWithNoPick() {
        var h = balancedHab()
        for d in R.levelledHabSide {
            h.settings[d.id] = setting(d.id, d.levels![.bypass]!, rev: 2, source: .config)
        }
        var a = app(.bypass)
        a[R.sshRequireFaceID] = nil
        let st = LevelMatcher.match(app: a, habs: [h])
        XCTAssertEqual(st.level, .bypass)
        XCTAssertEqual(st.title, "Bypass permissions")
        // A picked level that does not match falls back to the same rule.
        XCTAssertEqual(LevelMatcher.match(app: a, habs: [h], prefer: .safe).level, .bypass)
        // A picked level that matches wins, exceptions and all.
        XCTAssertEqual(LevelMatcher.match(app: a, habs: [h], prefer: .balanced).level, .balanced)
    }

    func testInvalidSentence() {
        XCTAssertEqual(SettingsWrites.sentence(DroppedSetting(settingID: R.approvalMaxAge, habID: "h", reason: .invalid), habLabel: "alpha"),
                       "Remote answers expire after on alpha was refused: not a value this hab takes.")
    }

    /// S1: during the send, the queued write is still in the saved queue; it leaves after the
    /// hab answers.
    @MainActor
    func testQueuedWriteLeavesTheSavedQueueAfterTheAnswer() async throws {
        let d = try XCTUnwrap(UserDefaults(suiteName: "SettingsTests.s1"))
        d.removePersistentDomain(forName: "SettingsTests.s1")
        var q = SettingsQueue()
        q.put(QueuedSettingWrite(habID: "h1", settingID: R.startElsewhere, value: .bool(true), revision: 1, epoch: "e1", seen: .bool(false)))
        q.save(d)
        var savedDuringSend = -1
        let c = SettingsCenter(defaults: d) { _, p in
            savedDuringSend = SettingsQueue.load(d).items.count
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: $0.revision + 1, source: .client) })
        }
        let h = balancedHab()
        c.update(habID: h.habID, label: h.label, state: state(h))
        await c.connectionOnline(habID: h.habID)
        XCTAssertEqual(savedDuringSend, 1, "a kill during the send keeps the write")
        XCTAssertTrue(SettingsQueue.load(d).items.isEmpty)

        // An unreached send keeps it queued, as it was.
        q.save(d)
        let c2 = SettingsCenter(defaults: d) { _, _ in throw TransportError("gone") }
        c2.update(habID: h.habID, label: h.label, state: state(h))
        await c2.connectionOnline(habID: h.habID)
        XCTAssertEqual(SettingsQueue.load(d).items, q.items)
    }

    /// S2: a connect that arrives during a flush runs after it, with the current view.
    @MainActor
    func testConnectDuringAFlushIsDeferred() async throws {
        let d = try XCTUnwrap(UserDefaults(suiteName: "SettingsTests.s2"))
        d.removePersistentDomain(forName: "SettingsTests.s2")
        d.set("safe", forKey: SettingsCenter.levelKey)
        var calls = 0
        var release: CheckedContinuation<Void, Never>?
        let c = SettingsCenter(defaults: d) { _, p in
            calls += 1
            if calls == 1 {
                await withCheckedContinuation { release = $0 }
                throw TransportError("the hab restarted")
            }
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: $0.revision + 1, source: .client) })
        }
        let h = balancedHab()
        c.update(habID: h.habID, label: h.label, state: state(h))
        let first = Task { await c.connectionOnline(habID: h.habID) }
        while release == nil { await Task.yield() }
        let second = Task { await c.connectionOnline(habID: h.habID) }
        for _ in 0..<20 { await Task.yield() }
        XCTAssertEqual(calls, 1, "the second connect waits for the first")
        release?.resume()
        await first.value
        await second.value
        XCTAssertEqual(calls, 2, "the second connect ran its new-hab check")
        XCTAssertEqual(c.hab("h1")?.settings[R.logbookApproval]?.value, .string("agents"))
    }

    /// S3: two fast writes to one row send increasing revisions.
    @MainActor
    func testFastWritesUseFreshRevisions() async throws {
        let d = try XCTUnwrap(UserDefaults(suiteName: "SettingsTests.s3"))
        d.removePersistentDomain(forName: "SettingsTests.s3")
        var revisions: [UInt64] = []
        let c = SettingsCenter(defaults: d) { _, p in
            revisions += p.values.map(\.revision)
            try await Task.sleep(for: .milliseconds(20))
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: $0.revision + 1, source: .client) })
        }
        var h = balancedHab()
        h.settings[R.permissionWait] = setting(R.permissionWait, .int(0))
        c.update(habID: h.habID, label: h.label, state: state(h))
        async let a: Void = c.setOneHab(R.permissionWait, habID: "h1", .int(30))
        async let b: Void = c.setOneHab(R.permissionWait, habID: "h1", .int(31))
        _ = await (a, b)
        XCTAssertEqual(revisions, [1, 2])
        XCTAssertNil(c.notice)
        // A stale state from before the answers does not roll the row back.
        c.update(habID: h.habID, label: h.label, state: state(h))
        XCTAssertEqual(c.hab("h1")?.settings[R.permissionWait]?.revision, 3)
    }

    // MARK: - Quiet hours

    /// `capcom.quiet_hours` (app.md 20.4): `off` and HH:MM-HH:MM read and write as the hab
    /// takes them, a value the app cannot read is nil (the row shows it and never writes), the
    /// time pickers round-trip whole minutes, and the off switch writes the home hab only.
    @MainActor
    func testQuietHours() async throws {
        XCTAssertEqual(QuietHours.parse("off"), .off)
        XCTAssertEqual(QuietHours.parse("22:00-07:00"), .range(start: 1320, end: 420))
        XCTAssertEqual(QuietHours.parse("07:05-07:05"), .range(start: 425, end: 425))
        for bad in ["", "Off", "10pm-7am", "24:00-07:00", "22:60-07:00", "2:00-07:00", "22:00", "22:00-07:00-08:00", "２２:00-07:00"] {
            XCTAssertNil(QuietHours.parse(bad), bad)
        }
        XCTAssertEqual(QuietHours.off.value, "off")
        XCTAssertEqual(QuietHours.range(start: 1320, end: 420).value, "22:00-07:00")
        XCTAssertEqual(QuietHours.range(start: 5, end: 1439).value, "00:05-23:59")
        XCTAssertEqual(QuietHours.standard.value, "22:00-07:00")
        XCTAssertEqual(QuietHours.range(start: 1320, end: 420).with(end: 450).value, "22:00-07:30")
        XCTAssertEqual(QuietHours.off.with(start: 60), .off)
        for m in [0, 59, 420, 1320, 1439] {
            XCTAssertEqual(QuietHours.minutes(QuietHours.date(m)), m)
        }
        XCTAssertEqual(R.def(QuietHours.id)?.kind, .page, "searchable, kept on the Capcom host page")

        var sent: [String: [SettingWrite]] = [:]
        let (c, _) = try center("SettingsTests.quiet") { hab, p in
            sent[hab, default: []] += p.values
            return SettingsResult(seq: 1, settings: p.values.map { self.setting($0.id, $0.value, rev: $0.revision + 1, source: .client) })
        }
        var home = balancedHab()
        home.settings[QuietHours.id] = setting(QuietHours.id, .string("22:00-07:00"))
        var other = balancedHab("h2", label: "beta")
        other.settings[QuietHours.id] = setting(QuietHours.id, .string("22:00-07:00"))
        c.update(habID: home.habID, label: home.label, state: state(home))
        c.update(habID: other.habID, label: other.label, state: state(other))
        await c.setOneHab(QuietHours.id, habID: "h1", .string(QuietHours.off.value))
        XCTAssertEqual(sent["h1"], [SettingWrite(id: QuietHours.id, revision: 1, value: .string("off"))])
        XCTAssertNil(sent["h2"], "quiet hours go to the home hab only")
        XCTAssertEqual(c.hab("h1")?.settings[QuietHours.id]?.value, .string("off"))
        // A value the hab's config locks is never written.
        var locked = balancedHab("h3", label: "gamma")
        locked.settings[QuietHours.id] = setting(QuietHours.id, .string("23:00-06:00"), source: .config)
        c.update(habID: locked.habID, label: locked.label, state: state(locked))
        await c.setOneHab(QuietHours.id, habID: "h3", .string("off"))
        XCTAssertNil(sent["h3"])
    }

    // MARK: - Voice

    func testYesAllows() {
        XCTAssertEqual(VoiceGrammar.answer("Yes."), .allow)
        XCTAssertNil(VoiceGrammar.answer("yes", yesAllows: false))
        XCTAssertEqual(VoiceGrammar.answer("allow it", yesAllows: false), .allow)
        XCTAssertEqual(VoiceGrammar.answer("no", yesAllows: false), .deny)
    }

    /// Voice settings saved by an older app, with no yesAllows, keep every value.
    func testVoiceSettingsReadOlderData() throws {
        let old = #"{"habEngine":false,"parakeetEngine":true,"appleEngine":true,"askSummary":false,"spokenCommands":true,"confirmBeforeSend":true,"bargeIn":false,"noisyRoom":true,"silence":2.5}"#
        let v = try JSONDecoder().decode(VoiceSettings.self, from: Data(old.utf8))
        XCTAssertFalse(v.habEngine)
        XCTAssertEqual(v.silence, 2.5)
        XCTAssertTrue(v.yesAllows)
    }
}
