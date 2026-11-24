import FlightdeckKit
import FlightdeckParakeet
import SwiftUI

@main
struct FlightdeckApp: App {
    @UIApplicationDelegateAdaptor(PushDelegate.self) private var pushDelegate
    /// app.log opens first, so the fleet's first lines reach it (diagnostics.md 4.10).
    @State private var fleet: Fleet = {
        AppLogFile.open()
        return AppFleet.make()
    }()
    /// At the App level this phase combines every window: active while any is.
    @Environment(\.scenePhase) private var phase
    /// The push prompt was asked for in this run (app.md 3.9).
    @State private var pushAsked = false

    var body: some Scene {
        // Links, files and Handoff open in the window that received them; the root shares
        // the handler with the Mac (app.md 3.15).
        WindowGroup(id: FlightdeckScene.board) {
            FlightdeckRoot(fleet: fleet)
                // An open board window takes a link, not a new scene.
                .handlesExternalEvents(preferring: ["*"], allowing: ["*"])
                .task {
                    pushDelegate.start(fleet.push)
                    if fleet.badge == nil {
                        fleet.badge = AppBadge { n in await PushDelegate.setBadge(n) }
                    }
                    // On-device Parakeet for voice mode; loaded only while voice mode or dictation runs.
                    fleet.voice.parakeet.install(FluidParakeetRuntime())
                }
                // Ask for push once a hab is online, not on the first frame (app.md 3.9).
                .onChange(of: fleet.board.onlineCount, initial: true) { _, online in
                    guard FirstRun.asksForPush(online: online, demo: fleet.isDemo, asked: pushAsked) else { return }
                    pushAsked = true
                    pushDelegate.askPermission()
                }
        }
        // The iPadOS menu bar and the Cmd-hold overlay (app.md 3.13).
        .commands { FlightdeckCommands(fleet: fleet) }
        .onChange(of: phase, initial: true) { _, p in
            fleet.apply(p)
        }
        // A session in its own window (app.md 3.14); it takes no links.
        WindowGroup(id: FlightdeckScene.session, for: SessionLink.self) { $link in
            SessionWindowRoot(fleet: fleet, link: link)
        }
        .handlesExternalEvents(matching: [])
    }
}
