import AppIntents
import FlightdeckShared

// Allow and Deny from the Live Activity (app.md 9.1, 7.6). Compiled into the
// iOS app and the widget extension: the widget names them on its buttons, and
// iOS runs `perform` in the app's process, which hands the answer to the
// handler the app set at launch (`ActivityAnswerBridge`). Allow asks for Face
// ID or the passcode; Deny needs no unlock (app.md 20.6: no setting skips it).

/// Allow, after Face ID or the passcode.
struct ActivityAllowIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Allow a request"
    static let isDiscoverable = false
    static let authenticationPolicy: IntentAuthenticationPolicy = .requiresAuthentication

    @Parameter(title: "Hab") var hab: String
    @Parameter(title: "Pane") var pane: String
    @Parameter(title: "Request") var request: String

    init() {}

    init(hab: String, pane: String, request: String) {
        self.hab = hab
        self.pane = pane
        self.request = request
    }

    func perform() async throws -> some IntentResult {
        await ActivityAnswerBridge.handler?(hab, pane, request, true)
        return .result()
    }
}

/// Deny: stopping a command needs no unlock.
struct ActivityDenyIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Deny a request"
    static let isDiscoverable = false
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed

    @Parameter(title: "Hab") var hab: String
    @Parameter(title: "Pane") var pane: String
    @Parameter(title: "Request") var request: String

    init() {}

    init(hab: String, pane: String, request: String) {
        self.hab = hab
        self.pane = pane
        self.request = request
    }

    func perform() async throws -> some IntentResult {
        await ActivityAnswerBridge.handler?(hab, pane, request, false)
        return .result()
    }
}
