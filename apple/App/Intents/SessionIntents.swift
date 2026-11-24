import AppIntents
import CoreSpotlight
import FlightdeckKit
import FlightdeckShared
import Foundation

// Shortcuts, Siri and Spotlight (app.md 3.19). Both apps compile this folder. The intents hand
// a route to the fleet; the queries read the session directory, which the app keeps on the
// device, so they answer while the connections are down.

/// The fleet the intents and queries read, set at launch.
@MainActor
enum IntentHost {
    static var fleet: Fleet?

    /// Sets the fleet, routes intents to it, and keeps Spotlight in step with its sessions.
    static func install(_ fleet: Fleet) {
        self.fleet = fleet
        AppRouteBridge.handler = { route in
            await MainActor.run { IntentHost.fleet?.routes.post(route) }
        }
        guard let directory = fleet.directory else { return }
        directory.onChange = { upsert, delete in
            SessionSpotlight.update(upsert: upsert, delete: delete)
            // "Open <session> in Flightdeck" knows the sessions of the last update.
            FlightdeckShortcuts.updateAppShortcutParameters()
        }
        // The index can be lost (a restore, a reset): write the whole list once per launch.
        SessionSpotlight.update(upsert: directory.entries, delete: [])
        FlightdeckShortcuts.updateAppShortcutParameters()
    }
}

/// A session, by the hab's host id and the session id. Only its name and its computer's name
/// reach Shortcuts and Spotlight.
struct SessionEntity: AppEntity, IndexedEntity {
    static let typeDisplayRepresentation = TypeDisplayRepresentation(name: "Session", numericFormat: "\(placeholder: .int) sessions")
    static let defaultQuery = SessionQuery()

    /// The session link: `flightdeck://session?hab=&session=`.
    var id: String
    @Property(title: "Name")
    var name: String
    @Property(title: "Computer")
    var computer: String

    init(_ e: SessionEntry) {
        id = e.id
        name = e.name
        computer = e.habLabel
    }

    var displayRepresentation: DisplayRepresentation {
        DisplayRepresentation(title: "\(name)", subtitle: "\(computer)", image: .init(systemName: "bubble.left.and.text.bubble.right"))
    }

    var link: SessionLink? { URL(string: id).flatMap(SessionLink.init(url:)) }
}

struct SessionQuery: EntityStringQuery {
    func entities(for identifiers: [SessionEntity.ID]) async throws -> [SessionEntity] {
        await MainActor.run { IntentHost.fleet?.directory?.entries(for: identifiers).map(SessionEntity.init) ?? [] }
    }

    func entities(matching string: String) async throws -> [SessionEntity] {
        await MainActor.run { IntentHost.fleet?.directory?.matching(string).map(SessionEntity.init) ?? [] }
    }

    func suggestedEntities() async throws -> [SessionEntity] {
        await MainActor.run { IntentHost.fleet?.directory?.entries.map(SessionEntity.init) ?? [] }
    }
}

/// Spotlight's copy of the session list: names and ids only (app.md 3.19).
enum SessionSpotlight {
    @MainActor
    static func update(upsert: [SessionEntry], delete: [String]) {
        let add = upsert.map(SessionEntity.init)
        Task.detached {
            let index = CSSearchableIndex.default()
            if !delete.isEmpty { try? await index.deleteAppEntities(identifiedBy: delete, ofType: SessionEntity.self) }
            if !add.isEmpty { try? await index.indexAppEntities(add) }
        }
    }
}

/// Opens a session; Spotlight runs it for a tapped session.
struct OpenSessionIntent: OpenIntent {
    static let title: LocalizedStringResource = "Open Session"
    static let description = IntentDescription("Opens a session in Flightdeck.")
    static var supportedModes: IntentModes { .foreground }

    @Parameter(title: "Session")
    var target: SessionEntity

    init() {}

    init(target: SessionEntity) {
        self.target = target
    }

    func perform() async throws -> some IntentResult {
        guard let link = target.link else { return .result() }
        await AppRouteBridge.post(.session(hab: link.hab, session: link.session))
        return .result()
    }
}

/// The New session form.
struct NewSessionIntent: AppIntent {
    static let title: LocalizedStringResource = "New Session"
    static let description = IntentDescription("Opens the New session form in Flightdeck.")
    static var supportedModes: IntentModes { .foreground }

    func perform() async throws -> some IntentResult {
        await AppRouteBridge.post(.newSession)
        return .result()
    }
}

/// The approval sheet on the oldest request that waits. Allow stays in the sheet, where the
/// command shows and Allow asks for Face ID (app.md 7.6); the intent never answers.
struct ApprovePendingIntent: AppIntent {
    static let title: LocalizedStringResource = "Approve Pending Request"
    static let description = IntentDescription("Opens the oldest request that waits for you, ready to approve.")
    static var supportedModes: IntentModes { [.background, .foreground(.dynamic)] }

    @Parameter(title: "Session")
    var session: SessionEntity?

    static var parameterSummary: some ParameterSummary {
        Summary("Approve the pending request in \(\.$session)")
    }

    init() {}

    func perform() async throws -> some IntentResult & ProvidesDialog {
        let link = session?.link
        let nothing = await MainActor.run {
            guard let fleet = IntentHost.fleet else { return false }
            return fleet.boardIsCurrent && fleet.waitingRequest(hab: link?.hab, session: link?.session) == nil
        }
        if nothing { return .result(dialog: "Nothing waits for you.") }
        guard systemContext.currentMode.canContinueInForeground else {
            throw needsToContinueInForegroundError("Open Flightdeck to see the request.", alwaysConfirm: false)
        }
        try await continueInForeground(nil, alwaysConfirm: false)
        await AppRouteBridge.post(.approvals(hab: link?.hab, session: link?.session))
        return .result(dialog: "Opening the request.")
    }
}

/// The phrases Siri and Spotlight offer. Each names the app, or the system drops it.
struct FlightdeckShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(intent: AskCapcomIntent(), phrases: [
            "Ask \(.applicationName)",
            "Talk to Capcom in \(.applicationName)",
            "Ask Capcom in \(.applicationName)",
        ], shortTitle: "Ask Capcom", systemImageName: "dot.radiowaves.left.and.right")
        AppShortcut(intent: OpenSessionIntent(), phrases: [
            "Open \(\.$target) in \(.applicationName)",
            "Open a session in \(.applicationName)",
        ], shortTitle: "Open Session", systemImageName: "bubble.left.and.text.bubble.right")
        AppShortcut(intent: NewSessionIntent(), phrases: [
            "New session in \(.applicationName)",
            "Start a session in \(.applicationName)",
        ], shortTitle: "New Session", systemImageName: "plus.bubble")
        AppShortcut(intent: ApprovePendingIntent(), phrases: [
            "Approve pending request in \(.applicationName)",
            "What needs me in \(.applicationName)",
        ], shortTitle: "Approve Pending", systemImageName: "hand.raised")
    }
}
