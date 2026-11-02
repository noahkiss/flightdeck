import Foundation
import FlightdeckShared
import HabAPI
import os
import UserNotifications

/// Notification actions that reach a hab from the background: iOS wakes the
/// app, the action connects over the app's transport, sends one call and
/// disconnects (task-iv89iwd8). It answers approvals from push banners
/// (Approve, Deny, Reply; task-oe7ioczy) and keeps the "Ping hab" test action.
///
/// Approve and Deny ask for unlock on the phone's lock screen: an Allow can
/// run a command (app.md 7.6). The device key signs once the device has been
/// unlocked after it starts, so a Watch answer signs while the phone is
/// locked (transport.md 7.2). A failure posts a notice and raises the request again in the
/// foreground; nothing retries on its own (app.md 18.7).
public final class HabActions: NSObject, UNUserNotificationCenterDelegate, @unchecked Sendable {
    public static let pingCategory = "flightdeck.hab-ping"
    public static let pingAction = "flightdeck.hab-ping.send"
    static let entryKey = "hab-entry"

    private let keys: DeviceKeys?
    private let listURL: URL?
    private let log = Logger(subsystem: "com.flightdeck.app", category: "actions")
    /// Answers and the foreground prompt; set once the fleet is built.
    @MainActor public weak var approvals: ApprovalCenter?
    /// Opens the usage sheet from a usage notification (app.md 14.4).
    @MainActor public weak var usage: UsageBook?
    /// Opens Capcom from "Ask an agent" (capcom.md 8.2).
    @MainActor public weak var capcom: CapcomCenter?
    /// Opens the session a notification names, such as a round's run session (rounds.md 10.2).
    @MainActor public weak var rounds: RoundCenter?

    public init(keys: DeviceKeys?, listURL: URL?) {
        self.keys = keys
        self.listURL = listURL
    }

    /// Every category the app registers: the ping test and the approval set
    /// the hab names in its pushes (app.md 8.6, 16.2).
    public static func categories() -> Set<UNNotificationCategory> {
        let ping = UNNotificationAction(identifier: pingAction, title: "Ping hab", options: [.authenticationRequired])
        let approve = UNNotificationAction(identifier: PushCategory.approveAction, title: "Approve", options: [.authenticationRequired])
        let deny = UNNotificationAction(identifier: PushCategory.denyAction, title: "Deny", options: [.authenticationRequired, .destructive])
        let open = UNNotificationAction(identifier: PushCategory.openAction, title: openTitle, options: [.foreground])
        let reply = UNTextInputNotificationAction(identifier: PushCategory.replyAction, title: "Reply", options: [.authenticationRequired], textInputButtonTitle: "Send", textInputPlaceholder: "Answer")
        // Ask opens the app at Capcom; it never answers (capcom.md 8.5).
        let ask = UNNotificationAction(identifier: PushCategory.askAction, title: "Ask an agent", options: [.foreground])
        return [
            UNNotificationCategory(identifier: pingCategory, actions: [ping], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.approval, actions: [deny, approve], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.approvalDeny, actions: [deny, open], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.open, actions: [open], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.question, actions: [reply, open], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.approvalAsk, actions: [deny, approve, ask], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.approvalDenyAsk, actions: [deny, open, ask], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.openAsk, actions: [open, ask], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.ask, actions: [ask], intentIdentifiers: []),
        ]
    }

    /// "Open on iPhone" reads right on a mirrored Watch banner (app.md 16.2).
    static var openTitle: String {
        #if os(iOS)
        return "Open on iPhone"
        #else
        return "Open"
        #endif
    }

    /// Registers the categories and becomes the notification delegate. Call
    /// at launch: iOS delivers a background action to the delegate it finds then.
    public func install(on center: UNUserNotificationCenter = .current()) {
        center.setNotificationCategories(Self.categories())
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

    // On the main actor: iOS calls its completion handler on the thread where this returns,
    // and UIKit aborts when that is not the main thread (a background notification action
    // crashed the app after Ping hab answered).
    @MainActor
    public func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .list, .sound]
    }

    @MainActor
    public func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        let content = response.notification.request.content
        if response.actionIdentifier == Self.pingAction {
            await ping(content)
            return
        }
        if let account = content.userInfo[SystemUsageNotifier.accountKey] as? String {
            await usage?.present(account: account)
            return
        }
        let text = (response as? UNTextInputNotificationResponse)?.userText
        let meta = PushMeta(userInfo: content.userInfo)
        switch NotificationAnswer.intent(action: response.actionIdentifier, meta: meta, text: text) {
        case .answer(let host, let params):
            guard let approvals = await approvals else {
                await notify("Flightdeck", "Open Flightdeck to answer.")
                return
            }
            _ = await approvals.answer(host: host, params, meta: meta)
        case .open(let m):
            await approvals?.open(m)
            await rounds?.open(m)
        case .ask(let m):
            await capcom?.ask(m)
        case .none:
            break
        }
    }

    /// Posts a local notification; with `meta` its tap opens the request.
    public static func post(_ title: String, _ body: String, meta: PushMeta?) async {
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = body
        if let meta {
            c.userInfo = [PushMeta.userInfoKey: meta.userInfoValue]
            c.categoryIdentifier = PushCategory.open
            if let s = meta.session { c.threadIdentifier = s }
        }
        try? await UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: "flightdeck.result." + UUID().uuidString, content: c, trigger: nil))
    }

    /// Removes delivered banners of requests that closed (app.md 18.6, B
    /// where iOS allows: while the app runs).
    public static func withdraw(requests: Set<String>) async {
        let center = UNUserNotificationCenter.current()
        let delivered = await center.deliveredNotifications()
        let ids = delivered.filter { n in
            guard let m = PushMeta(userInfo: n.request.content.userInfo), let r = m.request else { return false }
            return m.kind == "permission" && requests.contains(r)
        }.map(\.request.identifier)
        if !ids.isEmpty { center.removeDeliveredNotifications(withIdentifiers: ids) }
    }

    private func ping(_ content: UNNotificationContent) async {
        guard let id = content.userInfo[Self.entryKey] as? String, let keys else { return }
        guard let entry = (try? SSHHabList.load(from: listURL))?.habs.first(where: { $0.id == id }) else {
            await notify("Flightdeck", "That hab is no longer in the app.")
            return
        }
        let name = entry.label ?? entry.destination
        if (try? keys.key(entry.keyID))?.requiresBiometry == true {
            // No Face ID prompt in the background: fail fast, never hang.
            await notify(name, "Open Flightdeck to retry: the SSH key needs Face ID.")
            return
        }
        do {
            let r = try await SSHSetup.perform(entry: entry, keys: keys, HabMethods.habPing, PingParams(), clientLabel: "action")
            let ms = r.elapsed.components.seconds * 1000 + r.elapsed.components.attoseconds / 1_000_000_000_000_000
            log.notice("ping action: \(name, privacy: .private) answered in \(ms) ms")
            await notify(name, "The hab answered in \(ms) ms.")
        } catch {
            log.notice("ping action: \(name, privacy: .private) failed: \(String(describing: error), privacy: .private)")
            await notify(name, "Could not reach the hab: \(error)")
        }
    }

    private func notify(_ title: String, _ body: String) async {
        await Self.post(title, body, meta: nil)
    }
}
