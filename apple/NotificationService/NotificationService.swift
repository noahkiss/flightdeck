import FlightdeckShared
import Intents
import UIKit
import UserNotifications

/// Adds the request's expiry to a permission banner as a local clock time,
/// " · Expires 14:05" (app.md 8.6, 16.1). The hab sends `expires_at` in the
/// payload's `flightdeck` object with `mutable-content`, and leaves the time
/// out of the body because only the phone knows its time zone and clock style.
///
/// A Capcom message (kind `capcom`, capcom-service.md 7.6) becomes a communication
/// notification: sender "Capcom" with its avatar. Without the Communication Notifications
/// capability iOS refuses the update, and the plain banner shows. Any other push passes
/// through unchanged.
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
        if meta?.kind == "capcom" {
            contentHandler(Self.communication(content, meta: meta))
            handler = nil
            return
        }
        content.body = ExpiryText.append(to: content.body, meta: meta)
        contentHandler(content)
        handler = nil
    }

    override func serviceExtensionTimeWillExpire() {
        if let handler, let original { handler(original) }
        handler = nil
    }

    /// The Capcom message as a message from Capcom; the content as it came when iOS refuses.
    static func communication(_ content: UNMutableNotificationContent, meta: PushMeta?) -> UNNotificationContent {
        let handle = INPersonHandle(value: "capcom", type: .unknown)
        let sender = INPerson(personHandle: handle, nameComponents: nil, displayName: "Capcom", image: avatar(),
                              contactIdentifier: nil, customIdentifier: "capcom")
        let intent = INSendMessageIntent(recipients: nil, outgoingMessageType: .outgoingMessageText, content: content.body,
                                         speakableGroupName: nil, conversationIdentifier: meta?.session ?? "capcom",
                                         serviceName: nil, sender: sender, attachments: nil)
        let interaction = INInteraction(intent: intent, response: nil)
        interaction.direction = .incoming
        interaction.donate(completion: nil)
        do {
            return try content.updating(from: intent)
        } catch {
            return content
        }
    }

    /// Capcom's avatar: its symbol on the accent colour.
    static func avatar() -> INImage? {
        let size = CGSize(width: 120, height: 120)
        let img = UIGraphicsImageRenderer(size: size).image { _ in
            UIColor.systemIndigo.setFill()
            UIBezierPath(ovalIn: CGRect(origin: .zero, size: size)).fill()
            let cfg = UIImage.SymbolConfiguration(pointSize: 56, weight: .semibold)
            if let s = UIImage(systemName: "dot.radiowaves.left.and.right", withConfiguration: cfg)?.withTintColor(.white, renderingMode: .alwaysOriginal) {
                s.draw(in: CGRect(x: (size.width - s.size.width) / 2, y: (size.height - s.size.height) / 2, width: s.size.width, height: s.size.height))
            }
        }
        return img.pngData().map { INImage(imageData: $0) }
    }
}
