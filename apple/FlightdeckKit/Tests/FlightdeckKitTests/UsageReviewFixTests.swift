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
        try? await Task.sleep(for: .milliseconds(150))
        let held = await conn.state.usage["k"]?.windows.first?.usedPct
        XCTAssertEqual(held, 64)
        await conn.stop()
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
