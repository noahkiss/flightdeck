import AppKit
import FlightdeckKit
import SwiftUI

@main
struct FlightdeckApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    /// app.log opens first, so the fleet's first lines reach it (diagnostics.md 4.10).
    @State private var fleet: Fleet = {
        AppLogFile.open()
        return AppFleet.make()
    }()

    var body: some Scene {
        WindowGroup(id: FlightdeckScene.board) {
            FlightdeckRoot(fleet: fleet)
                // Links and Handoff go to an open board window, not a new one (app.md 3.15).
                .handlesExternalEvents(preferring: ["*"], allowing: ["*"])
                .task {
                    appDelegate.fleet = fleet
                    // The Dock tile badge (app.md 7.9); empty at 0.
                    if fleet.badge == nil {
                        fleet.badge = AppBadge { n in
                            NSApp.dockTile.badgeLabel = AppBadge.label(n)
                            return true
                        }
                    }
                }
        }
        .commands { FlightdeckCommands(fleet: fleet) }
        // A session in its own window (app.md 3.14); it takes no links.
        WindowGroup(id: FlightdeckScene.session, for: SessionLink.self) { $link in
            SessionWindowRoot(fleet: fleet, link: link)
        }
        .handlesExternalEvents(matching: [])
        // The Settings window, ⌘, (app.md 3.8).
        Settings {
            FlightdeckSettings(fleet: fleet)
        }
    }
}

/// Writes what was typed before the app quits (app.md 4.2): the draft store saves after a
/// pause, which a quit would cut short.
final class AppDelegate: NSObject, NSApplicationDelegate {
    var fleet: Fleet?

    func applicationWillTerminate(_ notification: Notification) {
        fleet?.drafts.flush()
    }
}
