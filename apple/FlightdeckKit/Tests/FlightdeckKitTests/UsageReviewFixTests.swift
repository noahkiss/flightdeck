import FlightdeckShared
import Foundation
import HabAPI
import UserNotifications
import XCTest
@testable import FlightdeckKit

// Usage edge cases from review: ties between habs, false room alerts,
// notification order, account age, demo isolation, the list and event race.

private let t0 = Date(timeIntervalSince1970: 1_800_000_000)

private func win(_ name: String, _ pct: Int, resetsIn: TimeInterval?, readAgo: TimeInterval = 0) -> UsageWindowReading {
    UsageWindowReading(name: name, usedPct: pct, resetsAt: resetsIn.map { t0.addingTimeInterval($0) }, readAt: t0.addingTimeInterval(-readAgo))
}

private func acct(_ key: String, _ windows: [UsageWindowReading]) -> UsageAccountReading {
    UsageAccountReading(key: key, label: "Max", harness: "claude", habID: "h1", habLabel: "h1", windows: windows, receivedAt: t0)
}

private func apiAccount(_ key: String, _ name: String, _ pct: Int, resetsAt: Date?, readAt: Date) -> UsageAccount {
    UsageAccount(harness: "claude", key: key, label: "Max 5x", windows: [
        UsageWindow(name: name, readAt: Fixture.stamp(readAt), resetsAt: resetsAt.map(Fixture.stamp), usedPct: pct),
    ])
}

/// One suite, emptied for each test, so test runs leave one preferences file.
private func freshDefaults() -> UserDefaults {
    let name = "flightdeck.tests.usage"
    let d = UserDefaults(suiteName: name)!
    d.removePersistentDomain(forName: name)
    return d
}

private final class Clock: @unchecked Sendable {
    var now: Date
    init(_ d: Date) { now = d }
}

@MainActor
final class UsageReviewFixTests: XCTestCase {
    /// One hab reached two ways: the same reading from both ids does not flip the account.
    func testSameReadingFromTwoHabsStaysPut() {
        let book = UsageBook(now: { t0 })
        var changes = 0
        book.onChange = { _ in changes += 1 }
        let r = apiAccount("k", "5h", 40, resetsAt: t0.addingTimeInterval(3600), readAt: t0)
        for _ in 0..<4 {
            book.update(habID: "a", habLabel: "desk", online: true, readings: [r])
            book.update(habID: "b", habLabel: "desk via ssh", online: true, readings: [r])
        }
        XCTAssertEqual(book.accounts["k"]?.habID, "a")
        // The first reading, then b coming online; nothing after that.
        XCTAssertEqual(changes, 2)
    }

    func testTieMovesOnlyToAnOnlineHab() {
        let book = UsageBook(now: { t0 })
        let r = apiAccount("k", "5h", 40, resetsAt: t0.addingTimeInterval(3600), readAt: t0)
        book.update(habID: "a", habLabel: "a", online: true, readings: [r])
        book.update(habID: "b", habLabel: "b", online: true, readings: [r])
        // b drops: its offline update does not take the account from online a.
        book.update(habID: "b", habLabel: "b", online: false, readings: [r])
        XCTAssertEqual(book.accounts["k"]?.habID, "a")
        // a drops, then b is back: the account moves to the online hab.
        book.update(habID: "a", habLabel: "a", online: false, readings: [r])
        book.update(habID: "b", habLabel: "b", online: true, readings: [r])
        XCTAssertEqual(book.accounts["k"]?.habID, "b")
        XCTAssertTrue(book.isOnline(book.accounts["k"]!))
    }

    func testFullWindowWithNoResetTimeKeepsTheRequest() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        alerts.warningsOn = false
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        alerts.check([acct("a", [win("5h", 100, resetsIn: nil)])])
        XCTAssertEqual(n.posts.count, 1, "no false room alert")
        XCTAssertTrue(alerts.isWaiting(account: "a", window: "5h"))
    }

    func testMissingWindowKeepsTheRequest() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        alerts.warningsOn = false
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        alerts.check([acct("a", [win("7d", 20, resetsIn: 86400)])])
        XCTAssertEqual(n.posts.count, 1)
        XCTAssertTrue(alerts.isWaiting(account: "a", window: "5h"))
    }

    func testWaitingEndsAtTheResetTime() {
        let clock = Clock(t0)
        let alerts = UsageAlerts(notifier: RecordingNotifier(), defaults: freshDefaults(), now: { clock.now })
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        XCTAssertTrue(alerts.isWaiting(account: "a", window: "5h"))
        clock.now = t0.addingTimeInterval(3601)
        XCTAssertFalse(alerts.isWaiting(account: "a", window: "5h"))
    }

    func testNoResetWindowWarnsAgainAfterFalling() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        alerts.check([acct("a", [win("spend monthly", 85, resetsIn: nil)])])
        alerts.check([acct("a", [win("spend monthly", 86, resetsIn: nil)])])
        XCTAssertEqual(n.posts.count, 1)
        alerts.check([acct("a", [win("spend monthly", 10, resetsIn: nil)])])
        alerts.check([acct("a", [win("spend monthly", 84, resetsIn: nil)])])
        XCTAssertEqual(n.posts.count, 2, "the next climb past 80 warns again")
    }

    func testAccountAgeFollowsItsNewestWindow() {
        let a = acct("a", [win("5h", 10, resetsIn: 3600, readAgo: 60), win("7d opus", 5, resetsIn: 86400, readAgo: 7200)])
        XCTAssertFalse(UsageMath.isStale(a, now: t0))
        XCTAssertTrue(UsageMath.isStale(a.windows[1], now: t0), "the old row greys alone")
        XCTAssertTrue(UsageMath.isStale(acct("b", [win("5h", 10, resetsIn: 3600, readAgo: 4000)]), now: t0))
    }

    func testDemoUsageIsSeparate() {
        defer { UserDefaults(suiteName: AppFleet.demoUsageSuite)?.removePersistentDomain(forName: AppFleet.demoUsageSuite) }
        let demo = AppFleet.usageCenter(demo: true)
        XCTAssertTrue(demo.notifier is NoopUsageNotifier)
        XCTAssertFalse(demo.defaults === SharedSettings.defaults)
        let shared = SharedSettings.defaults.data(forKey: UsageSnapshot.defaultsKey)
        let key = "k-demo-" + UUID().uuidString
        demo.book.update(habID: "demo-x", habLabel: "x", online: true, readings: [apiAccount(key, "5h", 90, resetsAt: nil, readAt: Date())])
        XCTAssertNotNil(UsageSnapshot.load(demo.defaults)?.accounts.first { $0.key == key })
        XCTAssertEqual(SharedSettings.defaults.data(forKey: UsageSnapshot.defaultsKey), shared, "the shared snapshot is untouched")
        XCTAssertTrue(AppFleet.usageCenter(demo: false).notifier is SystemUsageNotifier)
        // The next demo start begins empty.
        XCTAssertTrue(AppFleet.usageCenter(demo: true).book.accounts.isEmpty)
    }

    /// Posts and cancels reach the center in call order, even when an add is slow.
    func testNotifierKeepsCallOrder() async {
        let sink = RecordingSink()
        let n = SystemUsageNotifier(sink: sink)
        n.post(id: "x", title: "t", body: "b", account: "a", at: Date().addingTimeInterval(3600))
        n.cancel(id: "x")
        n.post(id: "y", title: "t", body: "b", account: "a", at: nil)
        await eventually("three calls") { await sink.ops.count == 3 }
        let ops = await sink.ops
        XCTAssertEqual(ops, ["add x", "remove x", "add y"])
        let auths = await sink.authorizations
        XCTAssertEqual(auths, 1)
    }

    /// A `usage.list` answer read before a `usage.updated` went out does not undo the event.
    func testListAnswerOlderThanAnEventDoesNotUndoIt() async {
        let hab = await sampleHab()
        await hab.setUsage([apiAccount("k", "5h", 10, resetsAt: t0, readAt: t0)])
        let newer = apiAccount("k", "5h", 64, resetsAt: t0, readAt: t0)
        await hab.setUsageListHook { await hab.putUsage(newer) }
        let conn = HabConnection(id: "h1", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("list answered") { await hab.usageLists == 1 }
        await eventually("the event applied") { await conn.state.usage["k"]?.windows.first?.usedPct == 64 }
        // The list answer went out after the event; give it time to arrive and be dropped.
        try? await Task.sleep(for: .milliseconds(150))
        let held = await conn.state.usage["k"]?.windows.first?.usedPct
        XCTAssertEqual(held, 64)
        await conn.stop()
    }
}

// MARK: - Second review round

@MainActor
final class UsageReviewRound2Tests: XCTestCase {
    /// Stepping one threshold as far as it goes never lands it on the other.
    func testSteppersStopShortOfTheNeighbour() {
        let alerts = UsageAlerts(notifier: RecordingNotifier(), defaults: freshDefaults(), now: { t0 })
        XCTAssertEqual(alerts.thresholds, [80, 95])
        XCTAssertEqual(UsageAlerts.stepRange(0, in: alerts.thresholds), 50...90)
        XCTAssertEqual(UsageAlerts.stepRange(1, in: alerts.thresholds), 85...100)
        alerts.thresholds = [UsageAlerts.stepRange(0, in: alerts.thresholds).upperBound, 95]
        XCTAssertEqual(alerts.thresholds, [90, 95])
        alerts.thresholds = [90, UsageAlerts.stepRange(1, in: alerts.thresholds).lowerBound]
        XCTAssertEqual(alerts.thresholds.count, 2, "two thresholds stay two")
        // A stored value off the grid stays inside its own range.
        XCTAssertTrue(UsageAlerts.stepRange(0, in: [92, 95]).contains(92))
    }

    /// The trigger names a UTC instant, so the night clocks go back is not ambiguous.
    func testTriggerComponentsAreUTC() throws {
        // 01:30 on a fall-back night in a zone with summer time, the second
        // (standard time) one: 06:30 UTC.
        let instant = try XCTUnwrap(ISO8601DateFormatter().date(from: "2026-11-01T06:30:00Z"))
        let parts = SystemUsageNotifier.triggerComponents(instant)
        XCTAssertEqual(parts.timeZone, TimeZone(identifier: "UTC"))
        XCTAssertEqual(parts.hour, 6)
        XCTAssertEqual(parts.minute, 30)
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = try XCTUnwrap(parts.timeZone)
        XCTAssertEqual(cal.date(from: parts), instant)
    }

    /// At launch, seeded readings of a hab no longer in the app go.
    func testLaunchPrunesSeededAccountsOfRemovedHabs() async {
        let d = freshDefaults()
        let seed = UsageSnapshot(writtenAt: t0, accounts: [
            UsageAccountReading(key: "kept", label: nil, harness: "claude", habID: "h1", habLabel: "h1", windows: [], receivedAt: t0),
            UsageAccountReading(key: "gone", label: nil, harness: "claude", habID: "removed", habLabel: "old", windows: [], receivedAt: t0),
        ])
        seed.save(d)
        let fleet = Fleet()
        fleet.usage = UsageCenter(notifier: RecordingNotifier(), defaults: d)
        XCTAssertEqual(fleet.usage?.book.accounts.count, 2)
        let hab = await sampleHab()
        fleet.add(HabConnection(id: "h1", transport: hab.transport(), timing: fastTiming()), label: nil)
        AppFleet.pruneUsage(fleet)
        XCTAssertEqual(Set(fleet.usage?.book.accounts.keys.map { $0 } ?? []), ["kept"])
        XCTAssertEqual(UsageSnapshot.load(d)?.accounts.map(\.key), ["kept"], "the widget loses it too")
    }

    /// A chosen account the snapshot lacks is reported, not swapped for another.
    func testWidgetSaysWhenTheChosenAccountIsMissing() {
        let a = acct("a", [win("5h", 90, resetsIn: 3600)])
        let b = acct("b", [win("5h", 10, resetsIn: 3600)])
        let chosen = UsageWidgetPick.accounts([a, b], selected: "b", now: t0)
        XCTAssertEqual(chosen.accounts.map(\.key), ["b", "a"])
        XCTAssertFalse(chosen.missing)
        XCTAssertEqual(UsageWidgetPick.accounts([a, b], selected: nil, now: t0).accounts.map(\.key), ["a", "b"])
        let gone = UsageWidgetPick.accounts([a, b], selected: "c", now: t0)
        XCTAssertTrue(gone.missing)
        XCTAssertTrue(gone.accounts.isEmpty)
    }

    /// An unreadable read time on the same reading again is no change.
    func testUnparsedReadTimeIsNoChange() {
        let clock = Clock(t0)
        let book = UsageBook(now: { clock.now })
        var changes = 0
        book.onChange = { _ in changes += 1 }
        let r = UsageAccount(harness: "claude", key: "k", label: nil, windows: [UsageWindow(name: "5h", readAt: "not a time", resetsAt: nil, usedPct: 20)])
        book.update(habID: "h", habLabel: "h", online: true, readings: [r])
        clock.now = t0.addingTimeInterval(30)
        book.update(habID: "h", habLabel: "h", online: true, readings: [r])
        XCTAssertEqual(changes, 1)
        // A changed value with an unreadable time is new, read now.
        let changed = UsageAccount(harness: "claude", key: "k", label: nil, windows: [UsageWindow(name: "5h", readAt: "not a time", resetsAt: nil, usedPct: 30)])
        book.update(habID: "h", habLabel: "h", online: true, readings: [changed])
        XCTAssertEqual(changes, 2)
        XCTAssertEqual(book.accounts["k"]?.windows.first?.readAt, clock.now)
    }

    /// Reset-time jitter under a minute leaves the scheduled notification alone.
    func testSmallResetJitterDoesNotReschedule() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        alerts.warningsOn = false
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        alerts.check([acct("a", [win("5h", 100, resetsIn: 3630)])])
        XCTAssertEqual(n.posts.count, 1)
        alerts.check([acct("a", [win("5h", 100, resetsIn: 3600 + 300)])])
        XCTAssertEqual(n.posts.count, 2, "a real move reschedules")
    }

    /// The hab holding a tie goes offline: the account moves at once to an
    /// online hab with the same reading.
    func testTieMovesWhenItsHabGoesOffline() {
        let book = UsageBook(now: { t0 })
        let r = UsageAccount(harness: "claude", key: "k", label: nil, windows: [
            UsageWindow(name: "5h", readAt: Fixture.stamp(t0), resetsAt: nil, usedPct: 40),
        ])
        book.update(habID: "a", habLabel: "a", online: true, readings: [r])
        book.update(habID: "b", habLabel: "b", online: true, readings: [r])
        XCTAssertEqual(book.accounts["k"]?.habID, "a")
        book.update(habID: "a", habLabel: "a", online: false, readings: [r])
        XCTAssertEqual(book.accounts["k"]?.habID, "b")
        XCTAssertTrue(book.isOnline(book.accounts["k"]!))
    }
}

actor RecordingSink: UsageNotificationSink {
    var ops: [String] = []
    var authorizations = 0

    func authorize() async {
        authorizations += 1
    }

    func add(_ request: UNNotificationRequest) async {
        // The first add is slow, as the first permission prompt is.
        if ops.isEmpty { try? await Task.sleep(for: .milliseconds(150)) }
        ops.append("add " + request.identifier)
    }

    func remove(_ ids: [String]) async {
        ops.append("remove " + ids.joined(separator: ","))
    }
}
