import SwiftUI

/// Debug builds only: `-FlightdeckOpenPane <pane>` opens that pane's terminal
/// on the first hab at launch, so a build can be checked with no clicks. In
/// demo mode a pane id such as `%5` works. `-FlightdeckOpenConsole <session>`
/// opens that session's console the same way.
struct OpenPaneOnLaunch: ViewModifier {
    let fleet: Fleet
    @State private var target: Target?
    @State private var console: ConsoleTarget?

    struct ConsoleTarget: Identifiable {
        var target: SessionTarget
        var id: String { target.session ?? "" }
    }

    struct Target: Identifiable {
        var pane: String
        var connection: HabConnection
        var id: String { pane }
    }

    func body(content: Content) -> some View {
        #if DEBUG
        content
            .task {
                let pane = UserDefaults.standard.string(forKey: "FlightdeckOpenPane") ?? ""
                let session = UserDefaults.standard.string(forKey: "FlightdeckOpenConsole") ?? ""
                guard !pane.isEmpty || !session.isEmpty else { return }
                // Demo habs are added a moment after launch.
                for _ in 0..<50 {
                    if let c = fleet.connections.first {
                        if !pane.isEmpty {
                            target = Target(pane: pane, connection: c)
                            return
                        }
                        // The console needs the hab's sessions: wait until it is online.
                        for _ in 0..<100 where await c.state.status != .online {
                            try? await Task.sleep(for: .milliseconds(100))
                        }
                        console = ConsoleTarget(target: SessionTarget(habID: c.id, session: session, pane: nil))
                        return
                    }
                    try? await Task.sleep(for: .milliseconds(100))
                }
            }
            .sheet(item: $console) { t in
                ConsoleSheet(target: t.target)
                    .environment(\.habConnections, fleet.lookup)
                    .environment(\.terminals, fleet.terminals)
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
