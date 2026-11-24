import FlightdeckShared
import SwiftUI

/// Takes the routes App Intents post (app.md 3.19) in a board window: New session, Capcom and
/// the approval sheet. Open Session goes through the window's link handling instead.
struct AppRouteHost: ViewModifier {
    let fleet: Fleet
    let commands: WindowCommands

    func body(content: Content) -> some View {
        content
            .onChange(of: fleet.routes.serial, initial: true) { _, _ in attempt() }
            .onChange(of: commands.goTargets) { _, _ in attempt() }
            .onChange(of: fleet.board.onlineCount) { _, _ in attempt() }
            .onChange(of: fleet.board.rows) { _, _ in attempt() }
    }

    private func attempt() {
        // The window registers its commands as it appears; until then it takes nothing.
        guard !commands.goTargets.isEmpty else { return }
        let route = fleet.routes.take { r in
            switch r {
            case .session: return false
            case .newSession, .capcom: return true
            case .approvals(let hab, let session):
                // Wait for the request to reach the board; with the board current and nothing
                // waiting, there is nothing to open.
                return fleet.waitingRequest(hab: hab, session: session) != nil || fleet.boardIsCurrent
            }
        }
        switch route {
        case .newSession?:
            commands.newSession()
        case .capcom(let message)?:
            commands.go(.capcom)
            if let message, !message.isEmpty {
                let entry = QuickEntry(capcom: fleet.capcom)
                entry.text = message
                Task { _ = await entry.send() }
            }
        case .approvals(let hab, let session)?:
            guard let r = fleet.waitingRequest(hab: hab, session: session) else { return }
            fleet.approvals?.open(PushMeta(kind: r.kind == .question ? "question" : "permission", hab: r.host ?? hab ?? "",
                                           session: r.session, pane: r.pane, request: r.requestID))
        case .session?, nil:
            break
        }
    }
}
