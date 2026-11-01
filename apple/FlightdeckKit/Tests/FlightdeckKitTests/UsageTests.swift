import FlightdeckShared
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

// Usage (app.md 14, 15): window math, the merge over habs, warnings,
// "alert me when free", the widget snapshot and the hab feed.

private let t0 = Date(timeIntervalSince1970: 1_800_000_000)

private func win(_ name: String, _ pct: Int, resetsIn: TimeInterval?, readAgo: TimeInterval = 0, now: Date = t0) -> UsageWindowReading {
    UsageWindowReading(name: name, usedPct: pct, resetsAt: resetsIn.map { now.addingTimeInterval($0) }, readAt: now.addingTimeInterval(-readAgo))
}

private func acct(_ key: String, hab: String = "h1", harness: String = "claude", _ windows: [UsageWindowReading], received: Date = t0) -> UsageAccountReading {
    UsageAccountReading(key: key, label: "Max", harness: harness, habID: hab, habLabel: hab, windows: windows, receivedAt: received)
}

private func apiAccount(_ key: String, _ windows: [(String, Int, Date?, Date)]) -> UsageAccount {
    UsageAccount(harness: "claude", key: key, label: "Max 5x", windows: windows.map {
        UsageWindow(name: $0.0, readAt: Fixture.stamp($0.3), resetsAt: $0.2.map(Fixture.stamp), usedPct: $0.1)
    })
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

final class RecordingNotifier: UsageNotifier {
    struct Post: Equatable {
        var id: String
        var title: String
        var at: Date?
    }

    var posts: [Post] = []
    var cancels: [String] = []

    func post(id: String, title: String, body: String, account: String, at: Date?) {
        posts.append(Post(id: id, title: title, at: at))
    }

    func cancel(id: String) {
        cancels.append(id)
    }
}

// MARK: - Window math

final class UsageMathTests: XCTestCase {
    func testLengthFromName() {
        XCTAssertEqual(UsageMath.length(of: "5h"), 5 * 3600)
        XCTAssertEqual(UsageMath.length(of: "7d"), 7 * 86400)
        XCTAssertEqual(UsageMath.length(of: "7d opus"), 7 * 86400)
        XCTAssertEqual(UsageMath.length(of: "model-y 7d"), 7 * 86400)
        XCTAssertEqual(UsageMath.length(of: "spend monthly"), 30 * 86400)
        XCTAssertEqual(UsageMath.length(of: "30m"), 1800)
        XCTAssertNil(UsageMath.length(of: "model-y"))
        XCTAssertNil(UsageMath.length(of: "spend"))
    }

    func testPaceIsTheShareOfTheWindowGone() throws {
        // 5h window, resets in 1h: 80 % of it is gone.
        let w = win("5h", 50, resetsIn: 3600)
        XCTAssertEqual(try XCTUnwrap(UsageMath.pace(w, now: t0)), 0.8, accuracy: 0.0001)
        XCTAssertFalse(UsageMath.overPace(w, now: t0))
        XCTAssertTrue(UsageMath.overPace(win("5h", 90, resetsIn: 3600), now: t0))
        // No length or no reset time: no pace.
        XCTAssertNil(UsageMath.pace(win("model-y", 50, resetsIn: 3600), now: t0))
        XCTAssertNil(UsageMath.pace(win("5h", 50, resetsIn: nil), now: t0))
    }

    func testResetPassedEmptiesTheWindow() {
        let w = win("5h", 100, resetsIn: -60, readAgo: 3600)
        XCTAssertTrue(UsageMath.resetPassed(w, now: t0))
        XCTAssertEqual(UsageMath.effectivePct(w, now: t0), 0)
        XCTAssertNil(UsageMath.pace(w, now: t0))
        // A reading after the reset time is a new reading, not a passed reset.
        let fresh = UsageWindowReading(name: "5h", usedPct: 3, resetsAt: t0.addingTimeInterval(-60), readAt: t0)
        XCTAssertFalse(UsageMath.resetPassed(fresh, now: t0))
    }

    func testAboveOneHundredShowsAsFull() {
        let w = win("spend monthly", 130, resetsIn: 86400)
        XCTAssertEqual(UsageMath.shownPct(w), 100)
        XCTAssertTrue(UsageMath.isFull(w))
        XCTAssertEqual(UsageText.short(w, now: t0), "Limit reached")
    }

    func testStaleAfterAnHourOrTheWindow() {
        XCTAssertFalse(UsageMath.isStale(win("7d", 10, resetsIn: 86400, readAgo: 3000), now: t0))
        XCTAssertTrue(UsageMath.isStale(win("7d", 10, resetsIn: 86400, readAgo: 3700), now: t0))
        // A 30-minute window is stale after 30 minutes.
        XCTAssertTrue(UsageMath.isStale(win("30m", 10, resetsIn: 600, readAgo: 1900), now: t0))
    }

    func testTightestAndOrder() {
        let a = acct("a", [win("5h", 40, resetsIn: 3600), win("7d", 70, resetsIn: 86400)])
        let b = acct("b", [win("5h", 100, resetsIn: -10, readAgo: 7200), win("7d", 20, resetsIn: 86400)])
        XCTAssertEqual(UsageMath.tightest(a, now: t0)?.name, "7d")
        // b's 5h reset passed, so it counts as empty: 7d at 20 is its tightest.
        XCTAssertEqual(UsageMath.tightest(b, now: t0)?.name, "7d")
        XCTAssertEqual(UsageMath.byTightest([b, a], now: t0).map(\.key), ["a", "b"])
    }

    func testTexts() {
        XCTAssertEqual(UsageText.duration(118 * 60), "1h 58m")
        XCTAssertEqual(UsageText.duration(4 * 86400 + 23 * 3600 + 59), "4d 23h")
        XCTAssertEqual(UsageText.duration(12 * 60), "12m")
        XCTAssertEqual(UsageText.duration(20), "now")
        let full = acct("a", [win("5h", 100, resetsIn: 118 * 60), win("7d", 90, resetsIn: 3600)])
        XCTAssertEqual(UsageText.status(full, now: t0), "5h limit reached · resets in 1h 58m")
        let over = acct("a", [win("5h", 10, resetsIn: 4 * 3600), win("7d", 90, resetsIn: 86400)])
        XCTAssertEqual(UsageText.status(over, now: t0), "Over pace on 7d")
        let on = acct("a", [win("5h", 10, resetsIn: 4 * 3600)])
        XCTAssertEqual(UsageText.status(on, now: t0), "On pace")
        XCTAssertNil(UsageText.status(acct("a", [win("model-y", 10, resetsIn: nil)]), now: t0))
    }
}

// MARK: - Merge over habs

@MainActor
final class UsageBookTests: XCTestCase {
    func testNewestReadingWinsAcrossHabs() {
        let book = UsageBook(now: { t0 })
        let older = apiAccount("k", [("5h", 40, t0.addingTimeInterval(3600), t0.addingTimeInterval(-300))])
        let newer = apiAccount("k", [("5h", 55, t0.addingTimeInterval(3600), t0.addingTimeInterval(-60))])
        book.update(habID: "h2", habLabel: "attic", online: true, readings: [newer])
        book.update(habID: "h1", habLabel: "workbench", online: true, readings: [older])
        XCTAssertEqual(book.accounts.count, 1)
        XCTAssertEqual(book.accounts["k"]?.windows.first?.usedPct, 55)
        XCTAssertEqual(book.accounts["k"]?.habLabel, "attic")
        // A newer reading from the other hab takes over, with its hab.
        let newest = apiAccount("k", [("5h", 60, t0.addingTimeInterval(3600), t0)])
        book.update(habID: "h1", habLabel: "workbench", online: true, readings: [newest])
        XCTAssertEqual(book.accounts["k"]?.windows.first?.usedPct, 60)
        XCTAssertEqual(book.accounts["k"]?.habID, "h1")
    }

    func testSeparateKeysStaySeparateAndOfflineKeepsReadings() {
        let book = UsageBook(now: { t0 })
        book.update(habID: "h1", habLabel: "workbench", online: true, readings: [apiAccount("a", [("5h", 10, nil, t0)])])
        book.update(habID: "h2", habLabel: "attic", online: true, readings: [apiAccount("b", [("5h", 20, nil, t0)])])
        XCTAssertEqual(Set(book.accounts.keys), ["a", "b"])
        book.update(habID: "h1", habLabel: "workbench", online: false, readings: [])
        XCTAssertEqual(Set(book.accounts.keys), ["a", "b"])
        XCTAssertFalse(book.isOnline(book.accounts["a"]!))
        XCTAssertTrue(book.isOnline(book.accounts["b"]!))
        book.forget(habID: "h2")
        XCTAssertEqual(Set(book.accounts.keys), ["a"])
    }

    func testSameReadingAgainIsNoChange() {
        let book = UsageBook(now: { t0 })
        var changes = 0
        book.onChange = { _ in changes += 1 }
        let r = apiAccount("a", [("5h", 10, nil, t0)])
        book.update(habID: "h1", habLabel: "w", online: true, readings: [r])
        book.update(habID: "h1", habLabel: "w", online: true, readings: [r])
        XCTAssertEqual(changes, 1)
    }

    func testTimesParse() throws {
        let book = UsageBook(now: { t0 })
        let a = UsageAccount(harness: "codex", key: "c", label: nil, windows: [
            UsageWindow(name: "7d", readAt: "2027-01-15T08:00:00Z", resetsAt: "2027-01-20T08:00:00.250Z", usedPct: 5),
            UsageWindow(name: "5h", readAt: "not a time", resetsAt: nil, usedPct: 1),
        ])
        book.update(habID: "h", habLabel: "h", online: true, readings: [a])
        let w = try XCTUnwrap(book.accounts["c"]?.windows)
        XCTAssertEqual(w[0].readAt, ISO8601DateFormatter().date(from: "2027-01-15T08:00:00Z"))
        XCTAssertNotNil(w[0].resetsAt)
        XCTAssertEqual(w[1].readAt, t0)
    }
}

// MARK: - Warnings

@MainActor
final class UsageWarningTests: XCTestCase {
    func testThresholdFiresOncePerWindow() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        alerts.check([acct("a", [win("5h", 70, resetsIn: 3600)])])
        XCTAssertTrue(n.posts.isEmpty)
        alerts.check([acct("a", [win("5h", 81, resetsIn: 3600)])])
        XCTAssertEqual(n.posts.map(\.title), ["Claude 5h at 81%"])
        alerts.check([acct("a", [win("5h", 85, resetsIn: 3590)])])
        XCTAssertEqual(n.posts.count, 1, "the same window warns once at 80")
        alerts.check([acct("a", [win("5h", 96, resetsIn: 3500)])])
        XCTAssertEqual(n.posts.map(\.title), ["Claude 5h at 81%", "Claude 5h at 96%"])
        // The next window (reset five hours later) warns again.
        alerts.check([acct("a", [win("5h", 82, resetsIn: 3600 + 5 * 3600)])])
        XCTAssertEqual(n.posts.count, 3)
    }

    func testJumpPastTwoThresholdsPostsOnce() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        alerts.check([acct("a", [win("7d", 97, resetsIn: 86400)])])
        XCTAssertEqual(n.posts.count, 1)
        alerts.check([acct("a", [win("7d", 82, resetsIn: 86400)])])
        XCTAssertEqual(n.posts.count, 1, "80 counted as warned with the jump")
    }

    func testStaleOrResetReadingDoesNotWarn() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        alerts.check([acct("a", [win("7d", 90, resetsIn: 86400, readAgo: 7200)])])
        alerts.check([acct("a", [win("5h", 100, resetsIn: -60, readAgo: 600)])])
        XCTAssertTrue(n.posts.isEmpty)
    }

    func testOffAndThresholdSettingsPersist() {
        let d = freshDefaults()
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: d, now: { t0 })
        alerts.thresholds = [90, 50, 90, 150]
        XCTAssertEqual(alerts.thresholds, [50, 90])
        alerts.warningsOn = false
        alerts.check([acct("a", [win("5h", 99, resetsIn: 3600)])])
        XCTAssertTrue(n.posts.isEmpty)
        let again = UsageAlerts(notifier: n, defaults: d, now: { t0 })
        XCTAssertFalse(again.warningsOn)
        XCTAssertEqual(again.thresholds, [50, 90])
    }

    func testFiredMarksSurviveARestart() {
        let d = freshDefaults()
        let n = RecordingNotifier()
        UsageAlerts(notifier: n, defaults: d, now: { t0 }).check([acct("a", [win("5h", 85, resetsIn: 3600)])])
        // Codex rounds reset times: two minutes off is the same window.
        UsageAlerts(notifier: n, defaults: d, now: { t0 }).check([acct("a", [win("5h", 86, resetsIn: 3720)])])
        XCTAssertEqual(n.posts.count, 1)
    }
}

// MARK: - Alert me when free

@MainActor
final class UsageFreeAlertTests: XCTestCase {
    func testSchedulesAtResetAndFollowsAMovedReset() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        let full = win("5h", 100, resetsIn: 3600)
        let a = acct("a", [full])
        XCTAssertTrue(alerts.canAlertWhenFree(full))
        XCTAssertFalse(alerts.canAlertWhenFree(win("5h", 99, resetsIn: 3600)))
        XCTAssertFalse(alerts.canAlertWhenFree(win("5h", 100, resetsIn: nil)))
        alerts.alertWhenFree(a, full)
        XCTAssertEqual(n.posts, [.init(id: UsageAlerts.freeID("a", "5h"), title: "Claude 5h window has reset", at: t0.addingTimeInterval(3600))])
        XCTAssertTrue(alerts.isWaiting(account: "a", window: "5h"))
        // A newer reading moves the reset: the notification moves with it.
        alerts.check([acct("a", [win("5h", 100, resetsIn: 5400)])])
        XCTAssertEqual(n.posts.last?.at, t0.addingTimeInterval(5400))
        XCTAssertEqual(n.posts.last?.id, UsageAlerts.freeID("a", "5h"))
    }

    func testRoomBeforeTheResetPostsNow() {
        let n = RecordingNotifier()
        let d = freshDefaults()
        let alerts = UsageAlerts(notifier: n, defaults: d, now: { t0 })
        alerts.warningsOn = false
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        alerts.check([acct("a", [win("5h", 40, resetsIn: 3600)])])
        XCTAssertEqual(n.posts.count, 2)
        XCTAssertNil(n.posts.last?.at, "posted at once")
        XCTAssertEqual(n.posts.last?.id, UsageAlerts.freeID("a", "5h"), "replaces the scheduled one")
        XCTAssertFalse(alerts.isWaiting(account: "a", window: "5h"))
        // The request is gone for good, also after a restart.
        XCTAssertTrue(UsageAlerts(notifier: n, defaults: d, now: { t0 }).freeRequests.isEmpty)
    }

    func testAPassedResetEndsQuietly() {
        let n = RecordingNotifier()
        let clock = Clock(t0)
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { clock.now })
        alerts.warningsOn = false
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        clock.now = t0.addingTimeInterval(3700)
        alerts.check([acct("a", [win("5h", 2, resetsIn: 5 * 3600, now: clock.now)])])
        XCTAssertEqual(n.posts.count, 1, "the scheduled notification already fired")
        XCTAssertTrue(alerts.freeRequests.isEmpty)
    }

    func testCancel() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        alerts.cancelFree(account: "a", window: "5h")
        XCTAssertEqual(n.cancels, [UsageAlerts.freeID("a", "5h")])
        XCTAssertTrue(alerts.freeRequests.isEmpty)
    }
}

// MARK: - Widget snapshot

@MainActor
final class UsageSnapshotTests: XCTestCase {
    func testRoundTripAndVersion() throws {
        let s = UsageSnapshot(writtenAt: t0, accounts: [acct("a", [win("5h", 42, resetsIn: 3600)])])
        let data = try s.encoded()
        XCTAssertEqual(UsageSnapshot.decode(data), s)
        let json = try XCTUnwrap(String(data: data, encoding: .utf8))
        XCTAssertTrue(json.contains("\"usedPct\":42"), json)
        XCTAssertTrue(json.contains("\"writtenAt\":\"2027-01-15T08:00:00Z\""), json)
        var other = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        other["version"] = 99
        XCTAssertNil(UsageSnapshot.decode(try JSONSerialization.data(withJSONObject: other)))
        XCTAssertNil(UsageSnapshot.decode(Data("junk".utf8)))
        XCTAssertNil(UsageSnapshot.decode(nil))
        // Saving the same readings again writes nothing, so widgets reload only on change.
        let d = freshDefaults()
        XCTAssertTrue(s.save(d))
        XCTAssertFalse(UsageSnapshot(writtenAt: t0.addingTimeInterval(60), accounts: s.accounts).save(d))
        XCTAssertEqual(UsageSnapshot.load(d), s)
    }

    func testCenterWritesTheSnapshotOnChangeOnly() throws {
        let d = freshDefaults()
        let center = UsageCenter(notifier: RecordingNotifier(), defaults: d, now: { t0 })
        var reloads = 0
        center.reloadWidgets = { reloads += 1 }
        var state = HabState()
        state.status = .online
        state.usage = ["k": apiAccount("k", [("5h", 30, t0.addingTimeInterval(3600), t0)])]
        center.update(habID: "h1", label: "workbench", state: state)
        center.update(habID: "h1", label: "workbench", state: state)
        XCTAssertEqual(reloads, 1)
        let snap = try XCTUnwrap(UsageSnapshot.load(d))
        XCTAssertEqual(snap.accounts.map(\.key), ["k"])
        XCTAssertEqual(snap.accounts.first?.habLabel, "workbench")
        // The next start begins from the snapshot: last known values.
        let next = UsageCenter(notifier: RecordingNotifier(), defaults: d, now: { t0 })
        XCTAssertEqual(next.book.accounts["k"]?.windows.first?.usedPct, 30)
        XCTAssertFalse(next.book.isOnline(next.book.accounts["k"]!))
    }

    func testLink() {
        XCTAssertEqual(UsageLink.url(account: "k").absoluteString, "flightdeck://usage?account=k")
        XCTAssertEqual(UsageLink.account(UsageLink.url(account: "k")), .some("k"))
        XCTAssertEqual(UsageLink.account(UsageLink.url()), .some(nil))
        XCTAssertTrue(UsageLink.account(URL(string: "flightdeck://request?hab=x")!) == nil)
    }
}

// MARK: - The hab feed

@MainActor
final class UsageFeedTests: XCTestCase {
    func testListAfterSnapshotThenEvents() async throws {
        let hab = await sampleHab()
        await hab.setUsage([apiAccount("k", [("5h", 10, t0, t0)])])
        let conn = HabConnection(id: "h1", transport: hab.transport(), timing: fastTiming())
        let fleet = Fleet()
        fleet.usage = UsageCenter(notifier: RecordingNotifier(), defaults: freshDefaults())
        fleet.add(conn, label: "workbench")
        await fleet.setActive(true)
        await eventually("list") { fleet.usage?.book.accounts["k"] != nil }
        XCTAssertEqual(fleet.usage?.book.accounts["k"]?.habLabel, "workbench")
        XCTAssertEqual(fleet.usage?.book.onlineHabs, ["h1"])
        await hab.putUsage(apiAccount("k", [("5h", 64, t0, t0.addingTimeInterval(1))]))
        await eventually("event") { fleet.usage?.book.accounts["k"]?.windows.first?.usedPct == 64 }
        await fleet.setActive(false)
        XCTAssertEqual(fleet.usage?.book.accounts["k"]?.windows.first?.usedPct, 64, "readings outlive the connection")
        XCTAssertEqual(fleet.usage?.book.onlineHabs, [])
    }

    func testNoUsageCapabilityNoCall() async throws {
        let hab = await sampleHab()
        let conn = HabConnection(id: "h1", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        try? await Task.sleep(for: .milliseconds(100))
        let lists = await hab.usageLists
        XCTAssertEqual(lists, 0)
        let usage = await conn.state.usage
        XCTAssertTrue(usage.isEmpty)
        await conn.stop()
    }
}

// MARK: - Hab fields: window length and start, degraded, clock offset (app.md 14.5, 14.6)

/// A hand-written hab window with the newer fields.
private func habWindow(_ name: String, _ pct: Int, resets: Date?, read: Date, lengthS: Int? = nil, starts: Date? = nil) -> UsageWindow {
    UsageWindow(lengthS: lengthS, name: name, readAt: Fixture.stamp(read), resetsAt: resets.map(Fixture.stamp), startsAt: starts.map(Fixture.stamp), usedPct: pct)
}

private func habAccount(_ key: String, degraded: String? = nil, _ windows: [UsageWindow]) -> UsageAccount {
    UsageAccount(degraded: degraded, harness: "claude", key: key, label: "Max 5x", windows: windows)
}

final class UsageWindowLengthTests: XCTestCase {
    func testLengthFromHabBeatsName() {
        // The hab's length wins over a name that reads otherwise.
        var w = win("5h", 10, resetsIn: 3600)
        w.length = 2 * 3600
        XCTAssertEqual(UsageMath.length(of: w), 2 * 3600)
        // Start and reset with no length.
        var s = win("model-y", 10, resetsIn: 1800)
        s.startsAt = t0.addingTimeInterval(-1800)
        XCTAssertEqual(UsageMath.length(of: s), 3600)
        // Neither: the name is the fallback.
        XCTAssertEqual(UsageMath.length(of: win("7d", 10, resetsIn: 3600)), 7 * 86400)
        XCTAssertNil(UsageMath.length(of: win("model-y", 10, resetsIn: 3600)))
        // A start at or after the reset is no length.
        var bad = win("model-y", 10, resetsIn: 60)
        bad.startsAt = t0.addingTimeInterval(120)
        XCTAssertNil(UsageMath.length(of: bad))
    }

    func testPaceAndStaleUseHabLength() {
        // A model-name window gets a pace once the hab sends its length.
        var w = win("model-y", 50, resetsIn: 900)
        XCTAssertNil(UsageMath.pace(w, now: t0))
        w.length = 3600
        XCTAssertEqual(try XCTUnwrap(UsageMath.pace(w, now: t0)), 0.75, accuracy: 0.0001)
        XCTAssertFalse(UsageMath.overPace(w, now: t0))
        XCTAssertEqual(UsageText.status(acct("a", [w]), now: t0), "On pace")
        // Stale after the hab's length when shorter than an hour.
        var short = win("model-y", 10, resetsIn: 600, readAgo: 1900)
        XCTAssertFalse(UsageMath.isStale(short, now: t0))
        short.length = 1800
        XCTAssertTrue(UsageMath.isStale(short, now: t0))
        XCTAssertEqual(UsageMath.staleAt(short), t0.addingTimeInterval(-100))
    }

    @MainActor
    func testReadingTakesLengthStartAndDegraded() {
        let a = habAccount("k", degraded: "rate_limits: unknown shape", [
            habWindow("5h", 40, resets: t0.addingTimeInterval(3600), read: t0, lengthS: 18000, starts: t0.addingTimeInterval(-14400)),
            habWindow("model-y", 5, resets: nil, read: t0, lengthS: 0),
        ])
        let r = UsageBook.reading(a, habID: "h1", habLabel: "workbench", receivedAt: t0)
        XCTAssertEqual(r.windows[0].length, 18000)
        XCTAssertEqual(r.windows[0].startsAt, t0.addingTimeInterval(-14400))
        XCTAssertNil(r.windows[1].length, "a zero length is none")
        XCTAssertNil(r.windows[1].startsAt)
        XCTAssertEqual(r.degraded, "rate_limits: unknown shape")
        XCTAssertTrue(r.unavailable)
    }

    func testSnapshotWithoutNewFieldsStillReads() throws {
        // A snapshot an earlier build wrote: no length, start or degraded.
        let json = """
        {"accounts":[{"habID":"h1","habLabel":"h1","harness":"claude","key":"a","label":"Max","receivedAt":"2027-01-15T08:00:00Z",
        "windows":[{"name":"5h","readAt":"2027-01-15T08:00:00Z","resetsAt":"2027-01-15T09:00:00Z","usedPct":42}]}],
        "version":1,"writtenAt":"2027-01-15T08:00:00Z"}
        """
        let s = try XCTUnwrap(UsageSnapshot.decode(Data(json.utf8)))
        XCTAssertNil(s.accounts[0].degraded)
        XCTAssertNil(s.accounts[0].windows[0].length)
        XCTAssertNil(s.accounts[0].windows[0].startsAt)
        // And the new fields survive a round trip.
        var a = acct("a", [win("5h", 42, resetsIn: 3600)])
        a.degraded = "drift"
        a.windows[0].length = 18000
        a.windows[0].startsAt = t0
        let round = UsageSnapshot(writtenAt: t0, accounts: [a])
        XCTAssertEqual(UsageSnapshot.decode(try round.encoded()), round)
    }
}

@MainActor
final class UsageUnavailableTests: XCTestCase {
    func testStatusAndOrder() {
        var bad = acct("a", [win("5h", 99, resetsIn: 3600)])
        bad.degraded = "drift"
        XCTAssertEqual(UsageText.status(bad, now: t0), "Usage unavailable")
        let fine = acct("b", [win("5h", 10, resetsIn: 3600)])
        XCTAssertEqual(UsageMath.byTightest([bad, fine], now: t0).map(\.key), ["b", "a"], "no number to rank by")
    }

    func testNoWarningsFromUnavailableAccount() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        var a = acct("a", [win("5h", 97, resetsIn: 3600)])
        a.degraded = "drift"
        alerts.check([a])
        XCTAssertTrue(n.posts.isEmpty)
        // Readable again: the warning fires then.
        a.degraded = nil
        alerts.check([a])
        XCTAssertEqual(n.posts.count, 1)
    }

    func testFreeRequestWaitsThroughUnavailable() {
        let n = RecordingNotifier()
        let alerts = UsageAlerts(notifier: n, defaults: freshDefaults(), now: { t0 })
        let full = win("5h", 100, resetsIn: 3600)
        alerts.alertWhenFree(acct("a", [full]), full)
        var a = acct("a", [win("5h", 20, resetsIn: 3600)])
        a.degraded = "drift"
        alerts.check([a])
        XCTAssertEqual(alerts.freeRequests.count, 1, "an unavailable reading says nothing about room")
        XCTAssertEqual(n.posts.count, 1, "only the scheduled notification")
    }

    func testDegradedMarkIsAChange() {
        let book = UsageBook(now: { t0 })
        var changes = 0
        book.onChange = { _ in changes += 1 }
        let w = habWindow("5h", 40, resets: t0.addingTimeInterval(3600), read: t0)
        book.update(habID: "h1", habLabel: "h1", online: true, readings: [habAccount("k", [w])])
        book.update(habID: "h1", habLabel: "h1", online: true, readings: [habAccount("k", degraded: "drift", [w])])
        XCTAssertEqual(book.accounts["k"]?.degraded, "drift")
        book.update(habID: "h1", habLabel: "h1", online: true, readings: [habAccount("k", [w])])
        XCTAssertNil(book.accounts["k"]?.degraded)
        XCTAssertEqual(changes, 3)
    }
}

@MainActor
final class UsageClockOffsetTests: XCTestCase {
    func testOffsetAgainstRoundTripMiddle() throws {
        let sent = t0
        let received = t0.addingTimeInterval(0.4)
        let o = HabConnection.clockOffset(habTime: Fixture.stamp(t0.addingTimeInterval(120.2)), sent: sent, received: received)
        XCTAssertEqual(try XCTUnwrap(o), 120, accuracy: 0.001)
        XCTAssertNil(HabConnection.clockOffset(habTime: nil, sent: sent, received: received))
        XCTAssertNil(HabConnection.clockOffset(habTime: "yesterday", sent: sent, received: received))
        // Whole seconds parse too.
        let ten = try XCTUnwrap(UsageBook.parseTime("2027-01-15T08:00:10Z"))
        XCTAssertEqual(try XCTUnwrap(HabConnection.clockOffset(habTime: "2027-01-15T08:00:00Z", sent: ten, received: ten)), -10, accuracy: 0.001)
    }

    func testHabTimesMoveOntoDeviceClock() throws {
        // The hab runs an hour ahead: its reset an hour and a half out is half an hour out here.
        let habNow = t0.addingTimeInterval(3600)
        let a = habAccount("k", [habWindow("5h", 50, resets: habNow.addingTimeInterval(1800), read: habNow, lengthS: 18000, starts: habNow.addingTimeInterval(-16200))])
        let r = UsageBook.reading(a, habID: "h1", habLabel: "h1", receivedAt: t0, clockOffset: 3600)
        let w = r.windows[0]
        XCTAssertEqual(w.resetsAt, t0.addingTimeInterval(1800))
        XCTAssertEqual(w.readAt, t0)
        XCTAssertEqual(w.startsAt, t0.addingTimeInterval(-16200))
        XCTAssertEqual(UsageText.resetsIn(w, now: t0), "30m")
        XCTAssertEqual(try XCTUnwrap(UsageMath.pace(w, now: t0)), 0.9, accuracy: 0.0001)
        XCTAssertFalse(UsageMath.isStale(w, now: t0), "read now, not an hour away")
        // With no offset, the same reading looks an hour later.
        XCTAssertEqual(UsageBook.reading(a, habID: "h1", habLabel: "h1", receivedAt: t0).windows[0].resetsAt, t0.addingTimeInterval(5400))
    }

    func testSmallOffsetIgnored() {
        let a = habAccount("k", [habWindow("5h", 50, resets: t0.addingTimeInterval(1800), read: t0)])
        XCTAssertEqual(UsageBook.reading(a, habID: "h1", habLabel: "h1", receivedAt: t0, clockOffset: 0.6).windows[0].resetsAt, t0.addingTimeInterval(1800))
        XCTAssertEqual(UsageBook.reading(a, habID: "h1", habLabel: "h1", receivedAt: t0, clockOffset: -1.5).windows[0].resetsAt, t0.addingTimeInterval(1801.5))
    }

    func testOffsetJitterIsNoChange() {
        // One hab reached two ways, its clock 10 s ahead, measured 10.0 and 10.6.
        let book = UsageBook(now: { t0 })
        var changes = 0
        book.onChange = { _ in changes += 1 }
        let a = habAccount("k", [habWindow("5h", 50, resets: t0.addingTimeInterval(1810), read: t0.addingTimeInterval(10))])
        book.update(habID: "h1", habLabel: "h1", online: true, readings: [a], clockOffset: 10)
        book.update(habID: "h2", habLabel: "h2", online: true, readings: [a], clockOffset: 10.6)
        XCTAssertEqual(book.accounts["k"]?.habID, "h1", "the same reading stays with the hab that has it")
        // A reconnect measures 11.2: the held times stay.
        book.update(habID: "h1", habLabel: "h1", online: true, readings: [a], clockOffset: 11.2)
        XCTAssertEqual(book.accounts["k"]?.windows[0].resetsAt, t0.addingTimeInterval(1800))
        // h1 goes offline: the tie moves to h2 at once.
        book.update(habID: "h1", habLabel: "h1", online: false, readings: [])
        XCTAssertEqual(book.accounts["k"]?.habID, "h2")
        XCTAssertEqual(changes, 3, "first reading, h2 online, h1 offline")
    }

    func testFeedAppliesHabClock() async throws {
        let hab = await sampleHab()
        await hab.setClockSkew(-7200)
        let now = Date()
        // The hab's clock is two hours behind: its times are two hours early.
        await hab.setUsage([habAccount("k", [habWindow("5h", 30, resets: now.addingTimeInterval(-7200 + 3600), read: now.addingTimeInterval(-7200), lengthS: 18000)])])
        let conn = HabConnection(id: "h1", transport: hab.transport(), timing: fastTiming())
        let fleet = Fleet()
        fleet.usage = UsageCenter(notifier: RecordingNotifier(), defaults: freshDefaults())
        fleet.add(conn, label: "workbench")
        await fleet.setActive(true)
        await eventually("list") { fleet.usage?.book.accounts["k"] != nil }
        let state = await conn.state
        let offset = try XCTUnwrap(state.clockOffset)
        XCTAssertEqual(offset, -7200, accuracy: 2)
        let w = try XCTUnwrap(fleet.usage?.book.accounts["k"]?.windows.first)
        XCTAssertEqual(try XCTUnwrap(w.resetsAt).timeIntervalSince(now), 3600, accuracy: 2)
        XCTAssertFalse(UsageMath.isStale(w, now: Date()), "read now on this device's clock")
        await fleet.setActive(false)
    }
}
