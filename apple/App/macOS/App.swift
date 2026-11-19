import AppKit
import FlightdeckKit
import SwiftUI

@main
struct FlightdeckApp: App {
    /// app.log opens first, so the fleet's first lines reach it (diagnostics.md 4.10).
    @State private var fleet: Fleet = {
        AppLogFile.open()
        return AppFleet.make()
    }()

    var body: some Scene {
        WindowGroup {
            FlightdeckRoot(fleet: fleet)
                .task {
                    // The Dock tile badge (app.md 7.9); empty at 0.
                    if fleet.badge == nil {
                        fleet.badge = AppBadge { n in
                            NSApp.dockTile.badgeLabel = AppBadge.label(n)
                            return true
                        }
                    }
                }
        }
    }
}
