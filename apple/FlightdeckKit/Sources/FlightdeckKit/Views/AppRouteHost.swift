import FlightdeckShared
import SwiftUI
#if os(iOS)
import UIKit
#else
import AppKit
#endif

/// Takes the routes App Intents post (app.md 3.19) in a board window: New session, Capcom and
/// the approval sheet. Open Session goes through the window's link handling instead.
struct AppRouteHost: ViewModifier {
    let fleet: Fleet
    let commands: WindowCommands
    /// An Ask Capcom message that did not go.
    @State private var unsent: Unsent?

    struct Unsent: Identifiable {
        let id = UUID()
        var message: String
        var why: String
    }

    func body(content: Content) -> some View {
        content
            .alert("Not sent to Capcom", isPresented: Binding(get: { unsent != nil }, set: { if !$0 { unsent = nil } }), presenting: unsent) { u in
                Button("Copy Message") { Self.copy(u.message) }
                Button("OK", role: .cancel) {}
            } message: { u in
                Text("\(u.why)\n\n\(u.message)")
            }
            .onChange(of: fleet.routes.serial, initial: true) { _, _ in attempt() }
            .onChange(of: commands.goTargets) { _, _ in attempt() }
            .onChange(of: fleet.board.onlineCount) { _, _ in attempt() }
            .onChange(of: fleet.board.rows) { _, _ in attempt() }
    }

    private static func copy(_ s: String) {
        #if os(iOS)
        UIPasteboard.general.string = s
        #else
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(s, forType: .string)
        #endif
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
                Task {
                    // A message that did not go is never dropped: the alert keeps it to copy.
                    if !(await entry.sendWhenReady()) {
                        unsent = Unsent(message: message, why: entry.failure ?? "Capcom did not take the message.")
                    }
                }
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
