import FlightdeckKit
import UIKit
import UserNotifications

/// Gets the APNs device token and hands it to the push registrar (app.md
/// 8.1). The notification delegate itself is `HabActions`, installed when the
/// fleet is built, so a background action finds it at launch.
final class PushDelegate: NSObject, UIApplicationDelegate {
    @MainActor private weak var push: PushRegistrar?

    /// Asks for notification permission, then for a device token. Runs on
    /// every start: iOS returns the stored answer and the current token.
    @MainActor
    func start(_ push: PushRegistrar?) {
        guard let push else { return }
        self.push = push
        Task { @MainActor in
            let center = UNUserNotificationCenter.current()
            do {
                let ok = try await center.requestAuthorization(options: [.alert, .sound, .badge])
                push.setPermission(ok ? .granted : .denied)
            } catch {
                push.setPermission(.denied)
            }
            // A token works for Live Activity pushes and silent updates even
            // when alerts are off, so ask for it either way.
            UIApplication.shared.registerForRemoteNotifications()
        }
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        MainActor.assumeIsolated { push?.setDeviceToken(deviceToken) }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        MainActor.assumeIsolated { push?.setTokenError(error.localizedDescription) }
    }
}
