import Foundation
import Synchronization

/// Where an App Intent takes the app (app.md 3.19): Shortcuts, Siri, Spotlight, a control or a
/// widget button. The intents are compiled into the apps and the widget extension; each runs
/// in the app's process and hands its route to the handler the app set at launch.
public enum AppRoute: Equatable, Sendable {
    /// Open a session, by the hab's host id and the session id.
    case session(hab: String, session: String)
    /// The New session form.
    case newSession
    /// The Capcom view; with a message, it goes to Capcom's standing conversation.
    case capcom(message: String?)
    /// The approval sheet on the oldest request that waits, in one session when one is named.
    case approvals(hab: String?, session: String?)
}

/// The app's route handler, set at launch (`AppRouteBridge.handler`).
public enum AppRouteBridge {
    public typealias Handler = @Sendable (AppRoute) async -> Void

    private static let slot = Mutex<Handler?>(nil)

    public static var handler: Handler? {
        get { slot.withLock { $0 } }
        set { slot.withLock { $0 = newValue } }
    }

    /// Hands `route` to the app; nothing happens in a process with no handler.
    public static func post(_ route: AppRoute) async {
        await handler?(route)
    }
}
