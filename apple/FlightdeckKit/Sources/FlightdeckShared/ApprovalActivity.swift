#if os(iOS)
import ActivityKit
import AppIntents
import Foundation

/// The Live Activity of one session (app.md 9). The hab that owns the session
/// updates it by push; the content state keys are the hab's contract
/// (hab-logic-api.md 3.9 Push, Live Activity).
public struct SessionActivityAttributes: ActivityAttributes {
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

/// Allow or Deny from the Live Activity. A `LiveActivityIntent` runs in the
/// app's process, so the app sets `handler` at launch; the widget extension
/// only names the intent on its buttons.
public struct ActivityAnswerIntent: LiveActivityIntent {
    public static let title: LocalizedStringResource = "Answer a request"
    public static let openAppWhenRun = false
    public static let isDiscoverable = false

    @Parameter(title: "Hab") public var hab: String
    @Parameter(title: "Pane") public var pane: String
    @Parameter(title: "Request") public var request: String
    @Parameter(title: "Allow") public var allow: Bool

    /// Set by the app at launch: answers the request and reports why it could not.
    nonisolated(unsafe) public static var handler: (@Sendable (_ hab: String, _ pane: String, _ request: String, _ allow: Bool) async -> Void)?

    public init() {}

    public init(hab: String, pane: String, request: String, allow: Bool) {
        self.hab = hab
        self.pane = pane
        self.request = request
        self.allow = allow
    }

    public func perform() async throws -> some IntentResult {
        await Self.handler?(hab, pane, request, allow)
        return .result()
    }
}
#endif
