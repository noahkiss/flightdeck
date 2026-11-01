import Foundation
import HabAPI
import os
import UserNotifications

/// Notification actions that reach a hab from the background: iOS wakes the
/// app, the action connects over the SSH carrier, sends one call and
/// disconnects (task-iv89iwd8). A local notification carries the action for
/// now; push replaces it later (task-oe7ioczy). `approval.answer` will use the
/// same path through `SSHSetup.perform`.
///
/// The action asks for unlock: the device key works only while the device is
/// unlocked (transport.md 7.2). A failure posts a "could not reach" notice and
/// never retries (app.md 18.7).
public final class HabActions: NSObject, UNUserNotificationCenterDelegate, @unchecked Sendable {
    public static let pingCategory = "flightdeck.hab-ping"
    public static let pingAction = "flightdeck.hab-ping.send"
    static let entryKey = "hab-entry"

    private let keys: DeviceKeys
    private let listURL: URL?
    private let log = Logger(subsystem: "com.flightdeck.app", category: "actions")

    public init(keys: DeviceKeys, listURL: URL?) {
        self.keys = keys
        self.listURL = listURL
    }

    /// Registers the categories and becomes the notification delegate. Call
    /// at launch: iOS delivers a background action to the delegate it finds then.
    public func install(on center: UNUserNotificationCenter = .current()) {
        let ping = UNNotificationAction(identifier: Self.pingAction, title: "Ping hab", options: [.authenticationRequired])
        center.setNotificationCategories([UNNotificationCategory(identifier: Self.pingCategory, actions: [ping], intentIdentifiers: [])])
        center.delegate = self
    }

    /// Posts a notification with the ping action, `after` from now.
    public static func scheduleTest(entry: SSHHabEntry, after: TimeInterval = 5) async throws {
        let center = UNUserNotificationCenter.current()
        guard try await center.requestAuthorization(options: [.alert, .sound]) else {
            throw TransportError("notifications are off for Flightdeck")
        }
        let c = UNMutableNotificationContent()
        c.title = entry.label ?? entry.destination
        c.body = "Press and hold, then Ping hab."
        c.categoryIdentifier = pingCategory
        c.userInfo = [entryKey: entry.id]
        let req = UNNotificationRequest(identifier: "flightdeck.ping." + UUID().uuidString, content: c, trigger: UNTimeIntervalNotificationTrigger(timeInterval: max(1, after), repeats: false))
        try await center.add(req)
    }

    // MARK: - Delegate

    public func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .list]
    }

    public func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        guard response.actionIdentifier == Self.pingAction,
              let id = response.notification.request.content.userInfo[Self.entryKey] as? String
        else { return }
        guard let entry = (try? SSHHabList.load(from: listURL))?.habs.first(where: { $0.id == id }) else {
            await notify("Flightdeck", "That hab is no longer in the app.")
            return
        }
        let name = entry.label ?? entry.destination
        do {
            let r = try await SSHSetup.perform(entry: entry, keys: keys, HabMethods.habPing, PingParams(), clientLabel: "action")
            let ms = r.elapsed.components.seconds * 1000 + r.elapsed.components.attoseconds / 1_000_000_000_000_000
            log.notice("ping action: \(name, privacy: .public) answered in \(ms) ms")
            await notify(name, "The hab answered in \(ms) ms.")
        } catch {
            log.notice("ping action: \(name, privacy: .public) failed: \(String(describing: error), privacy: .public)")
            await notify(name, "Could not reach the hab: \(error)")
        }
    }

    private func notify(_ title: String, _ body: String) async {
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = body
        try? await UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: "flightdeck.result." + UUID().uuidString, content: c, trigger: nil))
    }
}
