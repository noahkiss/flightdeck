import SwiftUI

/// The scene ids the app's windows use.
public enum FlightdeckScene {
    /// The board window (iPhone and iPad: the tab shell).
    public static let board = "board"
    /// A session in its own window (app.md 3.14).
    public static let session = "session"
}

extension FocusedValues {
    /// The focused window's commands (app.md 3.13).
    @Entry var windowCommands: WindowCommands?
    /// What the menus show for the focused window, as a value.
    @Entry var commandState: CommandState?
}

extension EnvironmentValues {
    /// The window's commands, for the views that fill them.
    @Entry var windowCommands: WindowCommands?
}

/// Publishes the window's commands and their state to the menus (app.md 3.13). The state is
/// computed here, in a view, so a change of the board or the session re-reads the menus.
struct PublishCommands: ViewModifier {
    let fleet: Fleet
    let commands: WindowCommands
    @Environment(\.supportsMultipleWindows) private var multipleWindows

    func body(content: Content) -> some View {
        content
            .environment(\.windowCommands, commands)
            .focusedSceneValue(\.windowCommands, commands)
            .focusedSceneValue(\.commandState, commands.state(board: fleet.board, actions: fleet.sessionActions, multipleWindows: multipleWindows))
    }
}

/// The app's menus (app.md 3.13): the Mac menu bar, the iPadOS menu bar and the iPad's
/// Cmd-hold overlay. Each command acts on the focused window.
public struct FlightdeckCommands: Commands {
    private let fleet: Fleet
    @FocusedValue(\.windowCommands) private var window
    @FocusedValue(\.commandState) private var focusedState
    @Environment(\.openWindow) private var openWindow

    public init(fleet: Fleet) {
        self.fleet = fleet
    }

    private var state: CommandState { focusedState ?? CommandState() }

    private func on(_ c: MenuCommand) -> Bool { !state.enabled(c) }

    /// The open session's row, as the board lists it now.
    private var row: BoardRow? { state.row }

    public var body: some Commands {
        CommandGroup(replacing: .newItem) {
            Button("New Session") { window?.newSession() }
                .keyboardShortcut("n")
                .disabled(on(.newSession))
            Button("New Window") { openWindow(id: FlightdeckScene.board) }
                .keyboardShortcut("n", modifiers: [.option, .command])
                // With no window focused (the Mac with every window closed) it still opens one.
                .disabled(!(focusedState?.multipleWindows ?? true))
            Button("Open in New Window") {
                if let row, let link = SessionLink.of(row, board: fleet.board) { window?.openWindow?(link) }
            }
            .keyboardShortcut("n", modifiers: [.shift, .command])
            .disabled(on(.openInNewWindow))
        }
        CommandGroup(before: .textEditing) {
            Button("Find Sessions") { window?.find() }
                .keyboardShortcut("f")
                .disabled(on(.find))
        }
        CommandMenu("Session") {
            Button("Show Chat") { window?.session?.setMode(.chat) }
                .keyboardShortcut("1", modifiers: [.option, .command])
                .disabled(on(.showChat))
            Button("Show Terminal") { window?.session?.setMode(.terminal) }
                .keyboardShortcut("2", modifiers: [.option, .command])
                .disabled(on(.showTerminal))
            Divider()
            Button("Voice Mode") { window?.session?.voice() }
                .keyboardShortcut("v", modifiers: [.option, .command])
                .disabled(on(.voice))
            Button("Stop") { window?.session?.stop() }
                .keyboardShortcut(".")
                .disabled(on(.stop))
            Button("Console") { window?.session?.console() }
                .keyboardShortcut("k", modifiers: [.option, .command])
                .disabled(on(.console))
            Divider()
            if state.enabled(.markUnread) {
                Button("Mark as Unread") { if let row { fleet.board.markUnseen(row) } }
                    .keyboardShortcut("u", modifiers: [.shift, .command])
            } else {
                Button("Mark as Read") { if let row { fleet.board.markSeen(row) } }
                    .keyboardShortcut("u", modifiers: [.shift, .command])
                    .disabled(on(.markRead))
            }
            Divider()
            Button("Rename…") { if let row { window?.ask?.askRename(row) } }
                .disabled(on(.rename))
            Button("Restart") {
                guard let row, let ask = window?.ask else { return }
                let center = fleet.sessionActions
                Task { await ask.run { await center.restart(row) } }
            }
            .disabled(on(.restart))
            Button("Copy Name") { if let row { SessionRowActions.copy(row.name ?? row.title) } }
                .disabled(on(.copyName))
            Button("Copy Link") {
                if let row, let link = SessionLink.of(row, board: fleet.board) { SessionRowActions.copy(link.url.absoluteString) }
            }
            .disabled(on(.copyLink))
            Divider()
            Button("Close…") { if let row { window?.ask?.askClose(row) } }
                .disabled(on(.close))
        }
        CommandMenu("Go") {
            Button("Sessions") { window?.go(.sessions) }
                .keyboardShortcut("1")
                .disabled(on(.goSessions))
            Button("Capcom") { window?.go(.capcom) }
                .keyboardShortcut("2")
                .disabled(on(.goCapcom))
            if state.goTargets.contains(.rounds) {
                Button("Rounds") { window?.go(.rounds) }
                    .keyboardShortcut("3")
            }
            if state.goTargets.contains(.settings) {
                Button("Settings") { window?.go(.settings) }
                    .keyboardShortcut("4")
            }
            Divider()
            Button("Next Needing You") { nextNeedingYou() }
                .keyboardShortcut("a", modifiers: [.option, .command])
                .disabled(on(.nextNeedingYou))
            Button("Mark All as Read") { fleet.board.markAllSeen() }
                .disabled(on(.markAllRead))
        }
    }

    private func nextNeedingYou() {
        guard let window, let r = NextNeedingYou.next(in: fleet.board.sections, after: window.openRowID) else { return }
        window.openRow(r)
    }
}
