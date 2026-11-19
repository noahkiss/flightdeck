import FlightdeckKit
import FlightdeckParakeet
import SwiftUI

@main
struct FlightdeckApp: App {
    @UIApplicationDelegateAdaptor(PushDelegate.self) private var pushDelegate
    @State private var fleet = AppFleet.make()
    /// At the App level this phase combines every window: active while any is.
    @Environment(\.scenePhase) private var phase
    /// The push prompt was asked for in this run (app.md 3.9).
    @State private var pushAsked = false

    var body: some Scene {
        WindowGroup {
            FlightdeckRoot(fleet: fleet)
                .onOpenURL { url in
                    if url.isFileURL {
                        // Any file from the share sheet or Files goes to the send sheet
                        // (app.md 4.4); a key file goes to key import (app.md 10.4).
                        if !fleet.files.open(url) { fleet.ssh?.openKeyFile(url) }
                        return
                    }
                    if fleet.usage?.open(url: url) == true { return }
                    if fleet.approvals?.open(url: url) != true { fleet.ssh?.open(url: url) }
                }
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
        .onChange(of: phase, initial: true) { _, p in
            fleet.apply(p)
        }
    }
}
