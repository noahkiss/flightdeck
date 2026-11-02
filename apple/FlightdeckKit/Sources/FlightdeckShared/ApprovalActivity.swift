#if os(iOS)
import ActivityKit
#endif
import Foundation

/// The Live Activity of one session (app.md 9). The hab that owns the session
/// updates it by push; the content state keys are the hab's contract
/// (hab-logic-api.md 3.9 Push, Live Activity). Defined on every platform so
/// the app's logic is testable on the Mac; only iOS makes it an activity.
public struct SessionActivityAttributes: Codable, Hashable, Sendable {
    public struct ContentState: Codable, Hashable, Sendable {
        /// `working`, `blocked`, `idle`, `done`, `error`, `exited`.
        public var state: String
        public var kind: String?
        public var title: String
        public var detail: String?
        /// The open request's id; set with `buttons`.
        public var request: String?
        public var pane: String?
        /// Allow and Deny show only while this is true.
        public var buttons: Bool
        /// RFC 3339, when the agent entered its state.
        public var since: String
        /// Other panes on this hab that wait for the owner.
        public var others: Int
        public var hab: String
        public var harness: String?

        public init(state: String, kind: String? = nil, title: String, detail: String? = nil, request: String? = nil, pane: String? = nil, buttons: Bool = false, since: String, others: Int = 0, hab: String, harness: String? = nil) {
            self.state = state
            self.kind = kind
            self.title = title
            self.detail = detail
            self.request = request
            self.pane = pane
            self.buttons = buttons
            self.since = since
            self.others = others
            self.hab = hab
            self.harness = harness
        }

        public var sinceDate: Date? { PushMeta.parseDate(since) }
    }

    /// The session's id on its hab.
    public var session: String
    public var name: String
    public var project: String?
    /// The hab's host id.
    public var hab: String
    /// The hab's label, as the board shows it.
    public var habLabel: String

    public init(session: String, name: String, project: String?, hab: String, habLabel: String) {
        self.session = session
        self.name = name
        self.project = project
        self.hab = hab
        self.habLabel = habLabel
    }
}

#if os(iOS)
extension SessionActivityAttributes: ActivityAttributes {}
#endif

/// Where the Live Activity's Allow and Deny land. The intents themselves
/// (apple/Shared) are compiled into the app and the widget, because App
/// Intents reads intent metadata from each target's own sources. A
/// `LiveActivityIntent` runs in the app's process, where the app sets this
/// handler at launch.
public enum ActivityAnswerBridge {
    nonisolated(unsafe) public static var handler: (@Sendable (_ hab: String, _ pane: String, _ request: String, _ allow: Bool) async -> Void)?
}

/// Settings the app shares with its widget through the App Group.
public enum SharedSettings {
    /// The App Group, as Info.plist names it (`FlightdeckAppGroup`).
    public static var groupID: String? {
        Bundle.main.object(forInfoDictionaryKey: "FlightdeckAppGroup") as? String
    }

    /// The App Group's defaults; standard defaults where there is none (tests).
    public static var defaults: UserDefaults {
        groupID.flatMap { UserDefaults(suiteName: $0) } ?? .standard
    }
}
