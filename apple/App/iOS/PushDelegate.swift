import FlightdeckKit
import UIKit
import UserNotifications

/// Gets the APNs device token and hands it to the push registrar (app.md
/// 8.1). The notification delegate itself is `HabActions`, installed when the
/// fleet is built, so a background action finds it at launch.
final class PushDelegate: NSObject, UIApplicationDelegate {
    @MainActor private weak var push: PushRegistrar?

    /// Asks for a device token and reads the stored notification answer. Runs on every
    /// start. The permission prompt waits for `askPermission` (app.md 3.9).
    @MainActor
    func start(_ push: PushRegistrar?) {
        guard let push else { return }
        self.push = push
        // The token needs no permission, and Live Activity pushes work with
        // alerts off, so ask for it first rather than after the prompt.
        UIApplication.shared.registerForRemoteNotifications()
        Task { @MainActor in
            switch await UNUserNotificationCenter.current().notificationSettings().authorizationStatus {
            case .authorized, .provisional, .ephemeral: push.setPermission(.granted)
            case .denied: push.setPermission(.denied)
            default: break
            }
        }
    }

    /// Asks for notification permission: once a hab is online, never on the first frame
    /// (app.md 3.9). iOS shows the prompt once and then returns the stored answer.
    @MainActor
    func askPermission() {
        guard let push else { return }
        Task { @MainActor in
            do {
                let ok = try await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge])
                push.setPermission(ok ? .granted : .denied)
            } catch {
                push.setPermission(.denied)
            }
        }
    }

    /// Sets the app icon badge (app.md 7.9). Returns false without permission
    /// for badges, so the next count tries again.
    static func setBadge(_ count: Int) async -> Bool {
        let center = UNUserNotificationCenter.current()
        let s = await center.notificationSettings()
        guard s.badgeSetting == .enabled else { return false }
        switch s.authorizationStatus {
        case .authorized, .provisional, .ephemeral: break
        default: return false
        }
        do {
            try await center.setBadgeCount(count)
            return true
        } catch {
            return false
        }
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        MainActor.assumeIsolated { push?.setDeviceToken(deviceToken) }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        MainActor.assumeIsolated { push?.setTokenError(error.localizedDescription) }
    }
}
