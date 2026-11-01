import SwiftUI
import UIKit

@main
struct ParakeetSpikeApp: App {
    @StateObject private var model = SpikeModel()
    @Environment(\.scenePhase) private var scenePhase

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(model)
                .onReceive(NotificationCenter.default.publisher(for: UIApplication.didReceiveMemoryWarningNotification)) { _ in
                    model.handleMemoryWarning()
                }
                .task { await model.runAutomaticTestIfRequested() }
        }
        .onChange(of: scenePhase) { _, phase in
            switch phase {
            case .background: model.handleBackground()
            case .active: model.handleForeground()
            default: break
            }
        }
    }
}

/// A launch screen in front of the spike screen, so leaving the spike screen
/// (and the model unload that goes with it) can be tested.
struct RootView: View {
    @EnvironmentObject var model: SpikeModel
    @State private var path: [String] = ProcessInfo.processInfo.environment["SPIKE_AUDIO_FILE"] == nil ? [] : ["spike"]

    var body: some View {
        NavigationStack(path: $path) {
            List {
                Section {
                    NavigationLink("Open the spike screen", value: "spike")
                }
                Section("Memory") {
                    MemoryRows()
                }
                Section {
                    Button("Copy results as Markdown") { model.copyReport() }
                }
            }
            .navigationTitle("Parakeet Spike")
            .navigationDestination(for: String.self) { _ in SpikeView() }
        }
    }
}

struct MemoryRows: View {
    @EnvironmentObject var model: SpikeModel

    var body: some View {
        LabeledContent("Footprint", value: Memory.megabytes(model.footprint))
        LabeledContent("Peak since launch", value: Memory.megabytes(model.peakFootprint))
        if let available = model.available {
            LabeledContent("Available to app", value: Memory.megabytes(available))
        }
        LabeledContent("Memory warnings", value: "\(model.memoryWarnings)")
    }
}
