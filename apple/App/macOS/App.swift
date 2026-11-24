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
    /// The needs-you menu bar extra shows (app.md 3.17).
    @AppStorage(MenuBarSetting.key) private var menuBarShown = true

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
        .commands {
            FlightdeckCommands(fleet: fleet)
            // View › Show Toolbar and Customize Toolbar… (app.md 3.13).
            ToolbarCommands()
        }
        // A session in its own window (app.md 3.14); it takes no links.
        WindowGroup(id: FlightdeckScene.session, for: SessionLink.self) { $link in
            SessionWindowRoot(fleet: fleet, link: link)
                // A launch may restore only session windows: the quit flush needs the fleet too.
                .task { appDelegate.fleet = fleet }
        }
        .handlesExternalEvents(matching: [])
        // The Settings window, ⌘, (app.md 3.8).
        Settings {
            FlightdeckSettings(fleet: fleet)
        }
        // Needs you in the menu bar (app.md 3.17); Settings turns it off.
        MenuBarExtra(isInserted: $menuBarShown) {
            NeedsYouMenu(fleet: fleet)
        } label: {
            NeedsYouMenuBarLabel(board: fleet.board)
        }
        .menuBarExtraStyle(.menu)
    }
}

/// Writes what was typed before the app quits (app.md 4.2): the draft store saves after a
/// pause, which a quit would cut short. A logout or force quit may skip the quit call, so the
/// app also writes when it goes to the background.
final class AppDelegate: NSObject, NSApplicationDelegate {
    var fleet: Fleet?

    func applicationWillResignActive(_ notification: Notification) {
        fleet?.drafts.flush()
    }

    func applicationWillTerminate(_ notification: Notification) {
        fleet?.drafts.flush()
    }
}
