#if os(macOS)
import AppKit
import SwiftUI

/// The menu bar extra's settings key: shown or not (app.md 3.17).
public enum MenuBarSetting {
    public static let key = "flightdeck.menuBar.shown"
}

/// The menu bar icon (app.md 3.17): a bell, filled with the count while something needs the
/// owner.
public struct NeedsYouMenuBarLabel: View {
    private let board: Board

    public init(board: Board) {
        self.board = board
    }

    public var body: some View {
        let content = MenuBarNeeds.content(board)
        if let n = MenuBarNeeds.count(content) {
            HStack(spacing: 2) {
                Image(systemName: MenuBarNeeds.symbol(content))
                Text("\(n)").monospacedDigit()
            }
            .accessibilityLabel("Flightdeck: \(n) need you")
        } else {
            Image(systemName: MenuBarNeeds.symbol(content))
                .accessibilityLabel("Flightdeck")
        }
    }
}

/// The menu under the icon (app.md 3.17): the sessions that need the owner, then Ask Capcom,
/// Open Flightdeck and Mark All as Read.
public struct NeedsYouMenu: View {
    private let fleet: Fleet
    private let askCapcom: (() -> Void)?
    @Environment(\.openWindow) private var openWindow

    /// `askCapcom` opens the quick entry panel (app.md 3.18); nil leaves it out.
    public init(fleet: Fleet, askCapcom: (() -> Void)? = nil) {
        self.fleet = fleet
        self.askCapcom = askCapcom
    }

    public var body: some View {
        let content = MenuBarNeeds.content(fleet.board)
        switch content {
        case .noHabs:
            Text("No paired computer")
        case .notConnected:
            Text("Not connected")
            Text("Flightdeck connects while one of its windows is open.")
        case .needs(let items, let confirmations):
            if items.isEmpty && confirmations == 0 {
                Text("Nothing needs you")
            }
            if !items.isEmpty {
                Section("Needs You") {
                    ForEach(items) { item in
                        Button {
                            open(item.row)
                        } label: {
                            Label {
                                Text(item.title)
                                Text(item.detail)
                            } icon: {
                                Image(systemName: item.symbol)
                            }
                        }
                    }
                }
            }
            if confirmations > 0 {
                Button(MenuBarNeeds.confirmationsLine(confirmations), systemImage: "checklist") { openBoard() }
            }
        }
        Divider()
        if let askCapcom {
            Button("Ask Capcom…", systemImage: "dot.radiowaves.left.and.right", action: askCapcom)
        }
        Button("Open Flightdeck", systemImage: "macwindow") { openBoard() }
        if fleet.board.unreadCount > 0 {
            Button("Mark All as Read") { fleet.board.markAllSeen() }
        }
    }

    /// Opens the session in its own window (3.14), by its link: one window per session, so a
    /// second pick brings it forward.
    private func open(_ row: BoardRow) {
        guard let link = SessionLink.of(row, board: fleet.board) else { return }
        NSApp.activate()
        openWindow(value: link)
    }

    /// Brings Flightdeck forward, with a board window: a new one only when none is open.
    private func openBoard() {
        NSApp.activate()
        if fleet.boardWindows == 0 { openWindow(id: FlightdeckScene.board) }
    }
}
#endif
