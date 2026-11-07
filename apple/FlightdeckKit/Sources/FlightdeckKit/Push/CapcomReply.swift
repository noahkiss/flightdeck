import Foundation
import HabAPI

/// The Reply of a Capcom message's notification (capcom-service.md 7.6): the text goes to the
/// Capcom session with `session.send`, on the live connection or a one-shot one, as an
/// answer to an approval does from the lock screen (app.md 8.7, 18.7). It never retries on
/// its own. On failure the notification says "Not sent" and the text waits in the composer.
public enum CapcomReply {
    /// Sends `text`; true when the hab took it. `post` shows a local notification.
    @MainActor
    public static func send(host: String, session: String, text: String, answerer: ApprovalAnswerer, center: CapcomCenter?,
                            post: @MainActor (_ title: String, _ body: String) async -> Void) async -> Bool {
        let p = SessionSendParams(idempotencyKey: UUID().uuidString, origin: .typed, session: session, text: text)
        do {
            _ = try await answerer.call(host: host, HabMethods.sessionSend, p)
            return true
        } catch {
            if let center {
                center.unsent = center.unsent.map { $0 + "\n" + text } ?? text
            }
            await post("Not sent", "Your reply to Capcom waits in Flightdeck: \(text)")
            return false
        }
    }
}
