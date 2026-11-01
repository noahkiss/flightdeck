import SwiftUI

struct SpikeView: View {
    enum Mode: String, CaseIterable, Identifiable {
        case free = "Free clips"
        case phrases = "Phrase test"
        var id: String { rawValue }
    }

    @EnvironmentObject var model: SpikeModel
    @State private var mode: Mode = .free

    var body: some View {
        List {
            modelSection
            Section("Memory") { MemoryRows() }
            Section {
                Picker("Mode", selection: $mode) {
                    ForEach(Mode.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
            }
            if mode == .phrases { phraseSection }
            Section { talkButton }
            if mode == .free { runsSection } else { phraseSummarySection }
            Section {
                Button("Copy results as Markdown") { model.copyReport() }
            }
            eventsSection
        }
        .navigationTitle("Parakeet vs Apple")
        .navigationBarTitleDisplayMode(.inline)
        .task { await model.prepareApple() }
        .onDisappear { Task { await model.unload(reason: "left the screen") } }
    }

    private var modelSection: some View {
        Section("Parakeet TDT 0.6B v2") {
            LabeledContent("Status", value: model.modelStatus)
            if let fraction = model.downloadFraction {
                ProgressView(value: fraction) { Text(String(format: "%.0f%%", fraction * 100)) }
            }
            LabeledContent("Download + compile", value: Report.seconds(model.downloadSeconds))
            LabeledContent("Last load", value: Report.seconds(model.loadSeconds.last))
            if model.isModelLoaded {
                Button("Unload model") { Task { await model.unload(reason: "button") } }
                    .disabled(model.isBusy)
            } else {
                Button(ParakeetEngine.isDownloaded() ? "Load model" : "Download and load model") {
                    Task { await model.downloadAndLoad() }
                }
                .disabled(model.isBusy)
            }
            Toggle("Unload when the app goes to the background", isOn: $model.unloadInBackground)
            LabeledContent("Apple", value: model.appleStatus)
        }
    }

    private var phraseSection: some View {
        Section("Read this phrase (\(model.phraseIndex + 1) of \(Phrases.all.count))") {
            Text(Phrases.all[model.phraseIndex])
                .font(.title3.monospaced())
                .padding(.vertical, 4)
            HStack {
                Button("Previous") { model.phraseIndex = max(0, model.phraseIndex - 1) }
                    .disabled(model.phraseIndex == 0)
                Spacer()
                Button("Next") { model.phraseIndex = min(Phrases.all.count - 1, model.phraseIndex + 1) }
                    .disabled(model.phraseIndex == Phrases.all.count - 1)
            }
            .buttonStyle(.borderless)
        }
    }

    private var talkButton: some View {
        let label = model.isRecording ? "Recording, release to transcribe" : (model.isBusy ? "Working" : "Hold to talk")
        return Text(label)
            .font(.headline)
            .foregroundStyle(.white)
            .frame(maxWidth: .infinity, minHeight: 64)
            .background(model.isRecording ? Color.red : (model.isBusy ? Color.gray : Color.accentColor),
                        in: RoundedRectangle(cornerRadius: 12))
            .contentShape(Rectangle())
            .gesture(
                DragGesture(minimumDistance: 0)
                    .onChanged { _ in if !model.isRecording { model.startRecording() } }
                    .onEnded { _ in model.stopRecording(phrase: mode == .phrases ? model.phraseIndex : nil) }
            )
            .listRowInsets(EdgeInsets())
            .accessibilityAddTraits(.isButton)
    }

    private var runsSection: some View {
        Section("Transcriptions") {
            if model.runs.isEmpty { Text("No clips yet").foregroundStyle(.secondary) }
            ForEach(model.runs.reversed()) { run in
                VStack(alignment: .leading, spacing: 6) {
                    Text("\(run.label), audio \(Report.seconds(run.audioSeconds))").font(.caption).foregroundStyle(.secondary)
                    EngineRow(name: "Parakeet", result: run.parakeet, error: run.parakeetError)
                    EngineRow(name: "Apple", result: run.apple, error: run.appleError)
                }
            }
        }
    }

    private var phraseSummarySection: some View {
        Section("Phrase results") {
            if model.phraseResults.isEmpty { Text("No phrases read yet").foregroundStyle(.secondary) }
            ForEach(model.phraseResults) { r in
                VStack(alignment: .leading, spacing: 4) {
                    Text("\(r.index + 1). \(r.reference)").font(.caption)
                    HStack {
                        Text("Parakeet \(Report.percent(r.parakeetScore))")
                        Spacer()
                        Text("Apple \(Report.percent(r.appleScore))")
                    }
                    .font(.caption.monospacedDigit())
                    Text(r.run.parakeet?.text ?? r.run.parakeetError ?? "-").font(.caption2).foregroundStyle(.secondary)
                    Text(r.run.apple?.text ?? r.run.appleError ?? "-").font(.caption2).foregroundStyle(.secondary)
                }
            }
            if !model.phraseResults.isEmpty {
                let p = Report.pooled(model.phraseResults.compactMap(\.parakeetScore))
                let a = Report.pooled(model.phraseResults.compactMap(\.appleScore))
                HStack {
                    Text("All: Parakeet \(Report.percent(p))")
                    Spacer()
                    Text("Apple \(Report.percent(a))")
                }
                .font(.headline.monospacedDigit())
            }
        }
    }

    private var eventsSection: some View {
        Section("Events") {
            ForEach(model.events.reversed()) { event in
                Text("\(event.time.formatted(date: .omitted, time: .standard))  \(event.text)")
                    .font(.caption2.monospaced())
            }
        }
    }
}

struct EngineRow: View {
    let name: String
    let result: EngineResult?
    let error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack {
                Text(name).font(.subheadline.bold())
                Spacer()
                if let result {
                    Text("\(Report.seconds(result.seconds)), RTF \(Report.factor(result))")
                        .font(.caption.monospacedDigit())
                }
            }
            Text(result?.text ?? error ?? "-")
                .font(.callout)
                .foregroundStyle(result == nil ? .secondary : .primary)
        }
    }
}
