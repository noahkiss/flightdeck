import GhosttyTerminal
import HabAPI
import SwiftUI

/// The terminal view for one pane (app.md 5): libghostty draws the pane at its
/// PTY size, scaled to fit. Typing here takes the size after the hab's
/// debounce; viewing never changes it. A self-contained view, so the session
/// view can host it beside the chat.
public struct TerminalScreen: View {
    @State private var model: TerminalModel
    /// For dictation into the pane; nil hides it.
    private let connection: HabConnection?
    @Environment(\.displayScale) private var displayScale
    @Environment(\.dismiss) private var dismiss
    @Environment(\.terminals) private var terminals
    #if os(iOS)
    @Environment(\.horizontalSizeClass) private var sizeClass
    #endif

    /// The pane runs a round's live run: focus reports, mouse motion and the wheel stay
    /// here, so watching never takes the run over (app.md 21.4).
    private let liveRun: Bool

    public init(pane: String, connection: HabConnection, liveRun: Bool = false) {
        _model = State(initialValue: TerminalModel(pane: pane, connection: connection))
        self.connection = connection
        self.liveRun = liveRun
    }

    public init(model: TerminalModel, connection: HabConnection? = nil) {
        _model = State(initialValue: model)
        self.connection = connection
        liveRun = false
    }

    public var body: some View {
        GeometryReader { geo in
            let s = model.sizing
            TerminalSurfaceView(context: model.view)
                // One dark scheme, so the space around a scaled pane matches it.
                .environment(\.colorScheme, .dark)
                .frame(width: s.surface.width, height: s.surface.height)
                .scaleEffect(s.scale, anchor: .topLeading)
                .frame(width: geo.size.width, height: geo.size.height, alignment: .topLeading)
                .clipped()
                .onChange(of: geo.size, initial: true) { _, size in
                    model.containerChanged(size)
                }
        }
        .background(Color(red: 0x1e / 255, green: 0x1e / 255, blue: 0x1e / 255))
        .overlay(alignment: .center) { statusOverlay }
        .overlay(alignment: .bottom) { inputNotice }
        .onChange(of: liveRun, initial: true) { _, on in model.liveRun = on }
        .onReceive(model.view.$surfaceSize) { m in
            model.surfaceChanged(m, displayScale: displayScale)
        }
        .onAppear {
            terminals?.add(model)
            model.start()
            #if os(macOS)
            // Focus only routes keys here; it sends nothing until a key is typed.
            model.view.requestFocus()
            #endif
        }
        .onDisappear {
            terminals?.remove(model)
            model.stop()
        }
        .toolbar {
            if let connection {
                ToolbarItem(placement: .automatic) {
                    PaneDictationButton(pane: model.pane, connection: connection)
                }
            }
            // On a narrow screen Fit and Redraw go in the More menu, so the
            // session's Chat | Terminal switch keeps its room.
            ToolbarItemGroup(placement: narrow ? .secondaryAction : .automatic) {
                Button {
                    model.fitToScreen()
                } label: {
                    Label("Fit to this screen", systemImage: "arrow.up.left.and.arrow.down.right")
                }
                .help("Fit to this screen")
                Button {
                    model.redraw()
                } label: {
                    Label("Redraw", systemImage: "arrow.clockwise")
                }
                .help("Redraw")
            }
        }
    }

    private var narrow: Bool {
        #if os(iOS)
        return sizeClass == .compact
        #else
        return false
        #endif
    }

    @ViewBuilder private var inputNotice: some View {
        if let notice = model.inputNotice {
            HStack(spacing: 8) {
                Label(notice, systemImage: "exclamationmark.triangle")
                    .font(.callout)
                Button {
                    model.dismissInputNotice()
                } label: {
                    Label("Dismiss", systemImage: "xmark")
                        .labelStyle(.iconOnly)
                }
                .buttonStyle(.borderless)
                .help("Dismiss")
            }
            .padding(10)
            .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 8))
            .padding(12)
        }
    }

    @ViewBuilder private var statusOverlay: some View {
        switch model.status {
        case .live:
            EmptyView()
        case .connecting:
            ProgressView()
                .controlSize(.small)
                .padding(10)
                .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 8))
        case .retrying:
            Label("Reconnecting", systemImage: "wifi.exclamationmark")
                .font(.callout)
                .padding(10)
                .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 8))
        case .exited(let x):
            VStack(spacing: 10) {
                Text(Self.exitText(x))
                    .font(.headline)
                Button("Close") { dismiss() }
            }
            .padding(16)
            .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 10))
        case .gone:
            Text("This pane is gone")
                .font(.headline)
                .padding(16)
                .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 10))
        case .closed:
            EmptyView()
        }
    }

    static func exitText(_ x: ExitedFrame) -> String {
        if let s = x.signal { return "Exited on \(s)" }
        if let c = x.code { return "Exited with status \(c)" }
        return "Exited"
    }
}

/// Finds the connection for a hab id, so a view opened from a board row can
/// reach that hab.
public struct HabConnectionLookup: Sendable {
    public var find: @MainActor @Sendable (String) -> HabConnection?
    public init(find: @escaping @MainActor @Sendable (String) -> HabConnection?) { self.find = find }
    public static let none = HabConnectionLookup { _ in nil }
}

extension EnvironmentValues {
    @Entry public var habConnections: HabConnectionLookup = .none
}
