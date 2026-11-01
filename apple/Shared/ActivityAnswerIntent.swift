import AppIntents
import FlightdeckShared

/// Allow or Deny from the Live Activity (app.md 9.1). Compiled into the iOS
/// app and the widget extension: the widget names it on its buttons, and iOS
/// runs `perform` in the app's process, which hands it to the handler the app
/// set at launch (`ActivityAnswerBridge`).
struct ActivityAnswerIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Answer a request"
    static let isDiscoverable = false

    @Parameter(title: "Hab") var hab: String
    @Parameter(title: "Pane") var pane: String
    @Parameter(title: "Request") var request: String
    @Parameter(title: "Allow") var allow: Bool

    init() {}

    init(hab: String, pane: String, request: String, allow: Bool) {
        self.hab = hab
        self.pane = pane
        self.request = request
        self.allow = allow
    }

    func perform() async throws -> some IntentResult {
        await ActivityAnswerBridge.handler?(hab, pane, request, allow)
        return .result()
    }
}
