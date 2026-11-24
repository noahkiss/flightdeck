import AppIntents
import FlightdeckShared

// Intents a control or widget names (app.md 3.19, 15.4). Compiled into both apps and the widget
// extension: App Intents reads intent metadata per target. Each opens the app and runs there,
// where the app set `AppRouteBridge.handler` at launch.

/// Ask Capcom: the Capcom view, and with a message, that message sent to Capcom's standing
/// conversation. The "Talk to Capcom" control runs it with no message.
struct AskCapcomIntent: AppIntent {
    static let title: LocalizedStringResource = "Ask Capcom"
    static let description = IntentDescription("Opens Capcom, the conversation that needs no project. With a message, sends it to Capcom.")
    static var supportedModes: IntentModes { .foreground }

    @Parameter(title: "Message", inputOptions: String.IntentInputOptions(multiline: true))
    var message: String?

    static var parameterSummary: some ParameterSummary {
        Summary("Ask Capcom \(\.$message)")
    }

    init() {}

    init(message: String?) {
        self.message = message
    }

    func perform() async throws -> some IntentResult {
        await AppRouteBridge.post(.capcom(message: message))
        return .result()
    }
}
