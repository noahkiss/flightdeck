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
    private let defaults: UserDefaults
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

/// Local notifications through the system. A tap opens the usage sheet at
/// the account (`HabActions` routes it).
public final class SystemUsageNotifier: UsageNotifier {
    public static let accountKey = "flightdeck.usage.account"
    public static let threadID = "flightdeck.usage"

    public init() {}

    public func post(id: String, title: String, body: String, account: String, at: Date?) {
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = body
        c.threadIdentifier = Self.threadID
        c.userInfo = [Self.accountKey: account]
        var trigger: UNNotificationTrigger?
        if let at, at.timeIntervalSinceNow > 1 {
            // A calendar date, not an interval: it holds across sleep and clock changes.
            let parts = Calendar.current.dateComponents([.year, .month, .day, .hour, .minute, .second], from: at)
            trigger = UNCalendarNotificationTrigger(dateMatching: parts, repeats: false)
        }
        let req = UNNotificationRequest(identifier: id, content: c, trigger: trigger)
        Task {
            let center = UNUserNotificationCenter.current()
            // Ask once; iOS returns the stored answer after the first time.
            _ = try? await center.requestAuthorization(options: [.alert, .sound])
            try? await center.add(req)
        }
    }

    public func cancel(id: String) {
        UNUserNotificationCenter.current().removePendingNotificationRequests(withIdentifiers: [id])
    }
}
