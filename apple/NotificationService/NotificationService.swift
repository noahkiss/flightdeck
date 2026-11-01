import FlightdeckShared
import UserNotifications

/// Adds the request's expiry to a permission banner as a local clock time,
/// " · Expires 14:05" (app.md 8.6, 16.1). The hab sends `expires_at` in the
/// payload's `flightdeck` object with `mutable-content`, and leaves the time
/// out of the body because only the phone knows its time zone and clock style.
/// Any other push passes through unchanged.
final class NotificationService: UNNotificationServiceExtension {
    private var handler: ((UNNotificationContent) -> Void)?
    private var original: UNNotificationContent?

    override func didReceive(_ request: UNNotificationRequest, withContentHandler contentHandler: @escaping (UNNotificationContent) -> Void) {
        handler = contentHandler
        original = request.content
        guard let content = request.content.mutableCopy() as? UNMutableNotificationContent else {
            contentHandler(request.content)
            return
        }
        let meta = PushMeta(userInfo: content.userInfo)
        content.body = ExpiryText.append(to: content.body, meta: meta)
        contentHandler(content)
        handler = nil
    }

    override func serviceExtensionTimeWillExpire() {
        if let handler, let original { handler(original) }
        handler = nil
    }
}
