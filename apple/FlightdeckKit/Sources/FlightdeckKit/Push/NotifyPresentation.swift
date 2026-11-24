import FlightdeckShared
import Foundation
import UserNotifications

/// What a notification shows while the app is in front (app.md 8.9): nothing for a push about
/// the session on screen, the owner's level for the kinds it covers, and a banner for the rest.
public enum NotifyPresentation {
    /// A banner, in the list, with sound: what every notification showed before levels.
    public static let full: UNNotificationPresentationOptions = [.banner, .list, .sound]

    /// - Parameters:
    ///   - remote: the notification came from a hab's push. The app's own notices (a failed
    ///     answer, a usage alert, the ping test) always show.
    ///   - onScreen: the push names a session the app reports in focus now.
    public static func options(meta: PushMeta?, remote: Bool, onScreen: Bool, levels: NotifyLevels) -> UNNotificationPresentationOptions {
        guard remote else { return full }
        // The hab holds banners for a session in focus (8.6); this is the app's backstop for a
        // push that crossed the focus report.
        if onScreen { return [] }
        guard let l = levels.level(for: meta) else { return full }
        // Off can meet a push sent before the hab heard the new kinds: it shows quiet, as in the
        // notification service extension.
        return (l == .off ? NotifyLevel.quiet : l).foreground
    }
}

/// The labels of the level rows (app.md 8.9).
public enum NotifyLevelText {
    public static func title(_ kind: NotifyKind) -> String {
        switch kind {
        case .approvals: return "Approvals"
        case .questions: return "Questions"
        case .finished: return "When a turn finishes"
        }
    }
}

extension Fleet {
    /// The push names a session a view shows in an active window, as `session.focus` last told
    /// its hab (app.md 8.6). The push names the hab by host id; the focus by connection.
    public func onScreen(_ meta: PushMeta?) -> Bool {
        guard let meta, let session = meta.session, !session.isEmpty else { return false }
        let habs = board.habs.filter { $0.id == meta.hab || SessionLink.knownHostID($0) == meta.hab }
        return habs.contains { focus.reports(habID: $0.id, session: session) }
    }
}
