import SwiftUI

/// Debug builds only: `-FlightdeckOpenPane <pane>` opens that pane's terminal
/// on the first hab at launch, so a build can be checked with no clicks. In
/// demo mode a pane id such as `%5` works.
struct OpenPaneOnLaunch: ViewModifier {
    let fleet: Fleet
    @State private var target: Target?

    struct Target: Identifiable {
        var pane: String
        var connection: HabConnection
        var id: String { pane }
    }

    func body(content: Content) -> some View {
        #if DEBUG
        content
            .task {
                guard let pane = UserDefaults.standard.string(forKey: "FlightdeckOpenPane"), !pane.isEmpty else { return }
                // Demo habs are added a moment after launch.
                for _ in 0..<50 {
                    if let c = fleet.connections.first {
                        target = Target(pane: pane, connection: c)
                        return
                    }
                    try? await Task.sleep(for: .milliseconds(100))
                }
            }
            .sheet(item: $target) { t in
                NavigationStack {
                    TerminalScreen(pane: t.pane, connection: t.connection)
                        .navigationTitle(t.pane)
                }
                #if os(macOS)
                .frame(minWidth: 820, minHeight: 560)
                #endif
            }
        #else
        content
        #endif
    }
}

extension View {
    func openPaneOnLaunch(_ fleet: Fleet) -> some View {
        modifier(OpenPaneOnLaunch(fleet: fleet))
    }
}
