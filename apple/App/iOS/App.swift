import FlightdeckKit
import SwiftUI

@main
struct FlightdeckApp: App {
    @State private var fleet = AppFleet.make()
    /// At the App level this phase combines every window: active while any is.
    @Environment(\.scenePhase) private var phase

    var body: some Scene {
        WindowGroup {
            FlightdeckRoot(fleet: fleet)
        }
        .onChange(of: phase, initial: true) { _, p in
            fleet.apply(p)
        }
    }
}
