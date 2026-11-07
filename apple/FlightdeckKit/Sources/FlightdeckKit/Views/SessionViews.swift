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
    @Environment(\.rounds) private var rounds

    var body: some View {
        if let pane = target.pane, let conn = habs.find(target.habID) {
            TerminalScreen(pane: pane, connection: conn, liveRun: rounds?.liveRun(habID: target.habID, pane: pane) ?? false)
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
    /// The content side panel (Mac, iPad) or sheet (iPhone), app.md 4.7.
    @State private var showFiles = Self.filesAtLaunch
    @State private var voiceShown = false
    /// Voice from the More menu found the microphone off.
    @State private var voiceDenied = false
    @Environment(\.voiceKit) private var voiceKit
    /// Raised by voice mode's Keyboard control: focus the composer.
    @State private var focusComposer = 0
    /// The composer's draft, here so the Capcom toolbar's New conversation can carry it.
    @State private var draft = ""
    @Environment(\.terminalSlot) private var terminal
    /// The Capcom view this session view sits in, if any.
    @Environment(\.capcomView) private var capcomView

    enum Mode: String, CaseIterable, Identifiable {
        case chat = "Chat"
        case terminal = "Terminal"
        var id: String { rawValue }
    }

    /// Debug builds only: `-FlightdeckShowFiles YES` opens the files panel or sheet at once.
    private static var filesAtLaunch: Bool {
        #if DEBUG
        return UserDefaults.standard.bool(forKey: "FlightdeckShowFiles")
        #else
        return false
        #endif
    }

    public init(model: ChatModel, compact: Bool, close: (() -> Void)? = nil) {
        _model = State(initialValue: model)
        self.compact = compact
        self.close = close
        // A session with no agent (a plain shell) opens in the terminal (app.md 11.1).
        _mode = State(initialValue: model.context.harness == nil && model.target.session != nil ? .terminal : .chat)
        #if DEBUG
        // Debug builds only: `-FlightdeckSessionMode Terminal` opens sessions in the terminal.
        if let m = UserDefaults.standard.string(forKey: "FlightdeckSessionMode").flatMap(Mode.init(rawValue:)) {
            _mode = State(initialValue: m)
        }
        #endif
    }

    public var body: some View {
        Group {
            if compact {
                Group {
                    switch model.context.isCapcomService ? .chat : mode {
                    case .chat: ChatView(model: model, openTerminal: model.context.isCapcomService ? nil : { mode = .terminal }, openVoice: { voiceShown = true }, focusComposer: focusComposer, draft: $draft)
                    case .terminal: terminal.make(model.target)
                    }
                }
                // The Chat | Terminal switch holds the middle of the bar, so the
                // terminal's own controls never push it off (app.md 11).
                .navigationTitle(model.context.title)
                .toolbar {
                    // The Capcom service has no pane: no Chat and Terminal switch (capcom-service.md 7.6).
                    if !model.context.isCapcomService {
                        ToolbarItem(placement: .principal) {
                            Picker("View", selection: $mode) {
                                ForEach(Mode.allCases) { Text($0.rawValue).tag($0) }
                            }
                            .pickerStyle(.segmented)
                            .fixedSize()
                        }
                    }
                    if model.target.session != nil, !model.context.isConsole, !model.context.isCapcomService {
                        ToolbarItem(placement: .secondaryAction) { consoleButton }
                    }
                    ToolbarItem(placement: .secondaryAction) {
                        Button("Files", systemImage: "doc.on.doc") { showFiles = true }
                    }
                    // A phone's bar holds the back button, the switch, one control and the
                    // More menu. More would cover the switch, so New conversation goes in
                    // the More menu, and in the terminal, where the pane's dictation takes
                    // the control's place, so does Voice.
                    if model.context.isCapcom {
                        ToolbarItem(placement: .secondaryAction) { newConversationButton }
                    }
                    if mode == .chat {
                        ToolbarItem(placement: .primaryAction) {
                            VoiceButton(model: model, shown: $voiceShown)
                        }
                    } else if voiceKit != nil {
                        ToolbarItem(placement: .secondaryAction) { voiceMenuButton }
                    }
                }
                .alert("Microphone access is off", isPresented: $voiceDenied) {
                    Button("Open Settings") { openPrivacySettings() }
                    Button("Cancel", role: .cancel) {}
                } message: {
                    Text("Turn on microphone access for Flightdeck to use voice mode and dictation.")
                }
                .sheet(isPresented: $showFiles) {
                    ContentPanel(model: model.content)
                        .presentationDetents([.medium, .large])
                }
            } else {
                VStack(spacing: 0) {
                    HStack {
                        ChatHeader(context: model.context)
                        Spacer()
                        if model.context.isCapcom {
                            if !model.context.isCapcomService {
                                Picker("View", selection: $mode) {
                                    ForEach(Mode.allCases) { Text($0.rawValue).tag($0) }
                                }
                                .pickerStyle(.segmented)
                                .labelsHidden()
                                .fixedSize()
                            }
                            newConversationButton
                                .labelStyle(.iconOnly)
                                .buttonStyle(.borderless)
                        }
                        VoiceButton(model: model, shown: $voiceShown)
                            .buttonStyle(.borderless)
                        if model.target.session != nil, !model.context.isConsole, !model.context.isCapcomService {
                            consoleButton
                                .buttonStyle(.borderless)
                        }
                        Button(showFiles ? "Hide files" : "Show files", systemImage: "sidebar.right") {
                            withAnimation(.easeOut(duration: 0.15)) { showFiles.toggle() }
                        }
                        .labelStyle(.iconOnly)
                        .buttonStyle(.borderless)
                        .help(showFiles ? "Hide files" : "Show files")
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
                    HStack(spacing: 0) {
                        if model.context.isCapcom {
                            // The Capcom view (app.md 4.10): the conversation fills the width;
                            // the terminal is one switch away.
                            switch model.context.isCapcomService ? .chat : mode {
                            case .chat: ChatView(model: model, openVoice: { voiceShown = true }, focusComposer: focusComposer, draft: $draft)
                            case .terminal: terminal.make(model.target)
                            }
                        } else {
                            GeometryReader { geo in
                                HStack(spacing: 0) {
                                    ChatView(model: model, openVoice: { voiceShown = true }, focusComposer: focusComposer, draft: $draft)
                                        .frame(width: geo.size.width / 2)
                                    Divider()
                                    terminal.make(model.target)
                                        .frame(maxWidth: .infinity)
                                }
                            }
                        }
                        if showFiles {
                            Divider()
                            ContentPanel(model: model.content)
                                .frame(width: 320)
                                .transition(.move(edge: .trailing))
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
        .voiceMode($voiceShown, chat: model) {
            mode = .chat
            focusComposer += 1
        }
    }

    /// "New conversation" (capcom.md 8.2): the chips that can move and the draft go to a fresh
    /// Capcom session, which opens in this one's place.
    private var newConversationButton: some View {
        Button("New conversation", systemImage: "square.and.pencil") {
            guard let center = model.capcom else { return }
            let text = draft
            Task {
                if await center.newConversation(from: model, draft: text, view: capcomView) { draft = "" }
            }
        }
        .disabled(model.capcom == nil || model.capcomBusy || !model.context.online)
        .help("Start a new Capcom conversation")
    }

    /// Voice as a More menu item (a phone in the terminal). An alert on a menu item never
    /// shows, so the session view raises the microphone one.
    private var voiceMenuButton: some View {
        Button("Voice", systemImage: "waveform") {
            if VoiceAudio.permissionDenied {
                voiceDenied = true
            } else {
                voiceShown = true
            }
        }
        .disabled(!VoiceButton.allowed(model))
    }

    /// Opens the session's console (app.md 6).
    private var consoleButton: some View {
        Button("Console", systemImage: "apple.terminal") { consoleShown = true }
            .help("Open the console")
    }
}
