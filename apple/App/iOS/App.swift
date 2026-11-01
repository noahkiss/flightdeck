import FlightdeckKit
import FlightdeckParakeet
import SwiftUI

@main
struct FlightdeckApp: App {
    @UIApplicationDelegateAdaptor(PushDelegate.self) private var pushDelegate
    @State private var fleet = AppFleet.make()
    /// At the App level this phase combines every window: active while any is.
    @Environment(\.scenePhase) private var phase

    var body: some Scene {
        WindowGroup {
            FlightdeckRoot(fleet: fleet)
                .onOpenURL { url in
                    if url.isFileURL {
                        // A key file from the share sheet or Files (app.md 10.4).
                        fleet.ssh?.openKeyFile(url)
                        return
                    }
                    if fleet.usage?.open(url: url) == true { return }
                    if fleet.approvals?.open(url: url) != true { fleet.ssh?.open(url: url) }
                }
                .task {
                    // The device key signs only while unlocked: a Live Activity
                    // answer on a locked phone says "Unlock to answer".
                    fleet.approvals?.isUnlocked = { UIApplication.shared.isProtectedDataAvailable }
                    pushDelegate.start(fleet.push)
                    // On-device Parakeet for voice mode; loaded only while voice mode or dictation runs.
                    fleet.voice.parakeet.install(FluidParakeetRuntime())
                }
        }
        .onChange(of: phase, initial: true) { _, p in
            fleet.apply(p)
        }
    }
}
