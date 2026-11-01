import FlightdeckShared
import Foundation
import UserNotifications
#if canImport(WidgetKit)
import WidgetKit
#endif

/// Usage in the app: the merged book, the alerts it drives, and the widget
/// snapshot it writes to the App Group (app.md 14, 15.1).
@MainActor
public final class UsageCenter {
    public let book: UsageBook
    public let alerts: UsageAlerts
    let defaults: UserDefaults
    let notifier: UsageNotifier
    /// Asks the widgets to reload; tests count calls.
    var reloadWidgets: () -> Void = {
        #if canImport(WidgetKit)
        WidgetCenter.shared.reloadTimelines(ofKind: UsageSnapshot.widgetKind)
        #endif
    }

    /// `defaults` holds the snapshot and the alert settings: the App Group's
    /// defaults in the app.
    public init(notifier: UsageNotifier, defaults: UserDefaults = SharedSettings.defaults, now: @escaping () -> Date = Date.init) {
        self.defaults = defaults
        self.notifier = notifier
        let seed = UsageSnapshot.load(defaults)?.accounts ?? []
        book = UsageBook(seed: seed, now: now)
        alerts = UsageAlerts(notifier: notifier, defaults: defaults, now: now)
        book.onChange = { [weak self] accounts in self?.changed(accounts, now: now()) }
    }

    private func changed(_ accounts: [UsageAccountReading], now: Date) {
        alerts.check(accounts)
        let snap = UsageSnapshot(writtenAt: now, accounts: accounts.sorted { $0.key < $1.key })
        if snap.save(defaults) { reloadWidgets() }
    }

    /// Opens the usage sheet for a usage link from a widget; false for any other link.
    @discardableResult
    public func open(url: URL) -> Bool {
        guard let account = UsageLink.account(url) else { return false }
        book.present(account: account)
        return true
    }

    /// Feeds one hab's state.
    public func update(habID: String, label: String, state: HabState) {
        book.update(habID: habID, habLabel: label, online: state.status == .online, readings: Array(state.usage.values))
    }
}

/// Where `SystemUsageNotifier` sends its requests; the system center in
/// the app, a recorder in tests.
public protocol UsageNotificationSink: AnyObject, Sendable {
    func authorize() async
    func add(_ request: UNNotificationRequest) async
    func remove(_ ids: [String]) async
}

final class SystemNotificationSink: UsageNotificationSink, @unchecked Sendable {
    func authorize() async {
        // iOS asks once and returns the stored answer after that.
        _ = try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound])
    }

    func add(_ request: UNNotificationRequest) async {
        try? await UNUserNotificationCenter.current().add(request)
    }

    func remove(_ ids: [String]) async {
        UNUserNotificationCenter.current().removePendingNotificationRequests(withIdentifiers: ids)
    }
}

/// Local notifications through the system. A tap opens the usage sheet at
/// the account (`HabActions` routes it). Posts and cancels reach the center
/// one at a time, in call order, so a cancel never overtakes the post it
/// cancels and an older post never lands after a newer one with the same id.
public final class SystemUsageNotifier: UsageNotifier, @unchecked Sendable {
    public static let accountKey = "flightdeck.usage.account"
    public static let threadID = "flightdeck.usage"

    private enum Op {
        case add(UNNotificationRequest)
        case remove(String)
    }

    private let ops: AsyncStream<Op>.Continuation
    private let worker: Task<Void, Never>

    public convenience init() {
        self.init(sink: SystemNotificationSink())
    }

    public init(sink: UsageNotificationSink) {
        let (stream, ops) = AsyncStream.makeStream(of: Op.self)
        self.ops = ops
        worker = Task {
            var authorized = false
            for await op in stream {
                switch op {
                case .add(let r):
                    if !authorized {
                        await sink.authorize()
                        authorized = true
                    }
                    await sink.add(r)
                case .remove(let id):
                    await sink.remove([id])
                }
            }
        }
    }

    deinit {
        ops.finish()
    }

    public func post(id: String, title: String, body: String, account: String, at: Date?) {
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = body
        c.threadIdentifier = Self.threadID
        c.userInfo = [Self.accountKey: account]
        var trigger: UNNotificationTrigger?
        if let at, at.timeIntervalSinceNow > 1 {
            // A calendar date with its time zone, not an interval: it fires at
            // that instant across sleep and a time-zone change.
            var parts = Calendar.current.dateComponents([.year, .month, .day, .hour, .minute, .second], from: at)
            parts.timeZone = Calendar.current.timeZone
            trigger = UNCalendarNotificationTrigger(dateMatching: parts, repeats: false)
        }
        ops.yield(.add(UNNotificationRequest(identifier: id, content: c, trigger: trigger)))
    }

    public func cancel(id: String) {
        ops.yield(.remove(id))
    }
}

/// Demo mode: usage notifications go nowhere.
public final class NoopUsageNotifier: UsageNotifier {
    public init() {}
    public func post(id: String, title: String, body: String, account: String, at: Date?) {}
    public func cancel(id: String) {}
}
