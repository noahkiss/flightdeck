import HabAPI
import SwiftUI

/// The view that fills the terminal half of a session: the pane's terminal
/// view by default. `.terminalSlot { target in ... }` replaces it.
public struct TerminalSlot {
    public var make: @MainActor (SessionTarget) -> AnyView

    public init(make: @escaping @MainActor (SessionTarget) -> AnyView) {
        self.make = make
    }

    /// The terminal view of the target's pane, through the hab connection in
    /// the environment; the placeholder when there is no pane or connection.
    public static let terminal = TerminalSlot { target in AnyView(PaneTerminal(target: target)) }

    public static let placeholder = TerminalSlot { target in AnyView(TerminalPlaceholder(target: target)) }
}

/// The terminal view of one session's pane.
struct PaneTerminal: View {
    let target: SessionTarget
    @Environment(\.habConnections) private var habs

    var body: some View {
        if let pane = target.pane, let conn = habs.find(target.habID) {
            TerminalScreen(pane: pane, connection: conn)
        } else {
            TerminalPlaceholder(target: target)
        }
    }
}

private struct TerminalSlotKey: EnvironmentKey {
    static let defaultValue = TerminalSlot.terminal
}

extension EnvironmentValues {
    public var terminalSlot: TerminalSlot {
        get { self[TerminalSlotKey.self] }
        set { self[TerminalSlotKey.self] = newValue }
    }
}

extension View {
    /// Sets the view that fills a session's terminal half.
    public func terminalSlot<V: View>(@ViewBuilder _ make: @escaping @MainActor (SessionTarget) -> V) -> some View {
        environment(\.terminalSlot, TerminalSlot { AnyView(make($0)) })
    }
}

struct TerminalPlaceholder: View {
    let target: SessionTarget

    var body: some View {
        ContentUnavailableView {
            Label("Terminal", systemImage: "terminal")
        } description: {
            Text("This session has no pane to show.")
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(.background.secondary)
    }
}

/// One session: chat first on the phone with a switch to the terminal; chat
/// beside the terminal, about 50/50, on iPad and Mac (app.md 11).
public struct SessionView: View {
    @State private var model: ChatModel
    private let compact: Bool
    private let close: (() -> Void)?
    @State private var mode: Mode
    @State private var consoleShown = false
    @Environment(\.terminalSlot) private var terminal

    enum Mode: String, CaseIterable, Identifiable {
        case chat = "Chat"
        case terminal = "Terminal"
        var id: String { rawValue }
    }

    public init(model: ChatModel, compact: Bool, close: (() -> Void)? = nil) {
        _model = State(initialValue: model)
        self.compact = compact
        self.close = close
        // A session with no agent (a plain shell) opens in the terminal (app.md 11.1).
        _mode = State(initialValue: model.context.harness == nil && model.target.session != nil ? .terminal : .chat)
    }

    public var body: some View {
        Group {
            if compact {
                Group {
                    switch mode {
                    case .chat: ChatView(model: model) { mode = .terminal }
                    case .terminal: terminal.make(model.target)
                    }
                }
                .toolbar {
                    ToolbarItem(placement: .principal) {
                        ChatHeader(context: model.context)
                    }
                    if model.target.session != nil {
                        ToolbarItem(placement: .secondaryAction) { consoleButton }
                    }
                    ToolbarItem(placement: .primaryAction) {
                        Picker("View", selection: $mode) {
                            ForEach(Mode.allCases) { Text($0.rawValue).tag($0) }
                        }
                        .pickerStyle(.segmented)
                        .fixedSize()
                    }
                }
            } else {
                VStack(spacing: 0) {
                    HStack {
                        ChatHeader(context: model.context)
                        Spacer()
                        if model.target.session != nil {
                            consoleButton
                                .buttonStyle(.borderless)
                        }
                        if let close {
                            Button("Close", systemImage: "xmark", action: close)
                                .labelStyle(.iconOnly)
                                .buttonStyle(.borderless)
                                .keyboardShortcut(.cancelAction)
                        }
                    }
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    Divider()
                    GeometryReader { geo in
                        HStack(spacing: 0) {
                            ChatView(model: model)
                                .frame(width: geo.size.width / 2)
                            Divider()
                            terminal.make(model.target)
                                .frame(maxWidth: .infinity)
                        }
                    }
                }
            }
        }
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .sheet(isPresented: $consoleShown) {
            ConsoleSheet(target: model.target)
        }
    }

    /// Opens the session's console (app.md 6).
    private var consoleButton: some View {
        Button("Console", systemImage: "apple.terminal") { consoleShown = true }
            .help("Open the console")
    }
}
