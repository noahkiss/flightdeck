import GhosttyTerminal
import HabAPI
import SwiftUI

/// The terminal view for one pane (app.md 5): libghostty draws the pane at its
/// PTY size, scaled to fit. Typing here takes the size after the hab's
/// debounce; viewing never changes it. A self-contained view, so the session
/// view can host it beside the chat.
public struct TerminalScreen: View {
    @State private var model: TerminalModel
    @Environment(\.displayScale) private var displayScale
    @Environment(\.dismiss) private var dismiss

    public init(pane: String, connection: HabConnection) {
        _model = State(initialValue: TerminalModel(pane: pane, connection: connection))
    }

    public init(model: TerminalModel) {
        _model = State(initialValue: model)
    }

    public var body: some View {
        GeometryReader { geo in
            let s = model.sizing
            TerminalSurfaceView(context: model.view)
                .frame(width: s.surface.width, height: s.surface.height)
                .scaleEffect(s.scale, anchor: .topLeading)
                .frame(width: geo.size.width, height: geo.size.height, alignment: .topLeading)
                .clipped()
                .onChange(of: geo.size, initial: true) { _, size in
                    model.containerChanged(size)
                }
        }
        .background(Color.black)
        .overlay(alignment: .center) { statusOverlay }
        .onReceive(model.view.$surfaceSize) { m in
            model.surfaceChanged(m, displayScale: displayScale)
        }
        .onAppear {
            model.start()
            #if os(macOS)
            // Focus only routes keys here; it sends nothing until a key is typed.
            model.view.requestFocus()
            #endif
        }
        .onDisappear { model.stop() }
        .toolbar {
            ToolbarItemGroup {
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
