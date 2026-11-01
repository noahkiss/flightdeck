import FlightdeckKit
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
                    if fleet.usage?.open(url: url) == true { return }
                    if fleet.approvals?.open(url: url) != true { fleet.ssh?.open(url: url) }
                }
                .task {
                    // The device key signs only while unlocked: a Live Activity
                    // answer on a locked phone says "Unlock to answer".
                    fleet.approvals?.isUnlocked = { UIApplication.shared.isProtectedDataAvailable }
                    pushDelegate.start(fleet.push)
                }
        }
        .onChange(of: phase, initial: true) { _, p in
            fleet.apply(p)
        }
    }
}
