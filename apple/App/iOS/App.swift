import FlightdeckKit
import SwiftUI

@main
struct FlightdeckApp: App {
    @State private var fleet = AppFleet.make()

    var body: some Scene {
        WindowGroup {
            FlightdeckRoot(fleet: fleet)
        }
    }
}
