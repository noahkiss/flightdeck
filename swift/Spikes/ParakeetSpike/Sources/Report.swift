import Foundation

/// Plain-text Markdown export of every measurement in this app run.
enum Report {
    static func seconds(_ value: Double?) -> String {
        value.map { String(format: "%.2f s", $0) } ?? "-"
    }

    static func factor(_ result: EngineResult?) -> String {
        result.map { String(format: "%.3f", $0.realTimeFactor) } ?? "-"
    }

    static func percent(_ score: WordErrorRate.Score?) -> String {
        score.map { String(format: "%.0f%%", $0.rate * 100) } ?? "-"
    }

    static func cell(_ text: String) -> String {
        text.replacingOccurrences(of: "|", with: "\\|").replacingOccurrences(of: "\n", with: " ")
    }

    static func runLine(_ run: SpikeModel.Run) -> String {
        let p = run.parakeet.map { "\($0.text) [\(seconds($0.seconds)), rtf \(factor($0))]" } ?? run.parakeetError ?? "-"
        let a = run.apple.map { "\($0.text) [\(seconds($0.seconds)), rtf \(factor($0))]" } ?? run.appleError ?? "-"
        return "\(run.label) audio \(seconds(run.audioSeconds)) | parakeet: \(p) | apple: \(a)"
    }

    /// Mean of per-phrase scores, weighted by reference words (total errors / total words).
    static func pooled(_ scores: [WordErrorRate.Score]) -> WordErrorRate.Score? {
        guard !scores.isEmpty else { return nil }
        return WordErrorRate.Score(errors: scores.map(\.errors).reduce(0, +),
                                   referenceWords: scores.map(\.referenceWords).reduce(0, +))
    }

    @MainActor
    static func markdown(model: SpikeModel) -> String {
        var out: [String] = []
        let iso = ISO8601DateFormatter()
        out.append("# Parakeet spike results")
        out.append("")
        out.append("- Device: \(SpikeModel.deviceLine)")
        out.append("- Exported: \(iso.string(from: Date())), app up \(Int(Date().timeIntervalSince(model.launchedAt))) s")
        out.append("- Parakeet model: \(ParakeetEngine.modelRepo) (FluidAudio 0.17.7)")
        out.append("- Model download + compile: \(seconds(model.downloadSeconds))")
        out.append("- Model loads: \(model.loadSeconds.isEmpty ? "-" : model.loadSeconds.map { seconds($0) }.joined(separator: ", "))")
        out.append("- Memory now: \(Memory.megabytes(model.footprint)), peak since launch: \(Memory.megabytes(model.peakFootprint))")
        out.append("- Memory warnings: \(model.memoryWarnings)")
        out.append("- Apple engine: \(model.appleStatus)")
        out.append("")

        out.append("## Transcriptions")
        out.append("")
        out.append("| Clip | Audio | Engine | Time | RTF | Text |")
        out.append("|---|---|---|---|---|---|")
        for run in model.runs {
            out.append("| \(cell(run.label)) | \(seconds(run.audioSeconds)) | Parakeet | \(seconds(run.parakeet?.seconds)) | \(factor(run.parakeet)) | \(cell(run.parakeet?.text ?? run.parakeetError ?? "-")) |")
            out.append("| \(cell(run.label)) | \(seconds(run.audioSeconds)) | Apple | \(seconds(run.apple?.seconds)) | \(factor(run.apple)) | \(cell(run.apple?.text ?? run.appleError ?? "-")) |")
        }
        out.append("")

        if !model.phraseResults.isEmpty {
            out.append("## Phrase test")
            out.append("")
            out.append("| # | Reference | Parakeet WER | Apple WER | Parakeet text | Apple text |")
            out.append("|---|---|---|---|---|---|")
            for r in model.phraseResults {
                out.append("| \(r.index + 1) | \(cell(r.reference)) | \(percent(r.parakeetScore)) | \(percent(r.appleScore)) | \(cell(r.run.parakeet?.text ?? "-")) | \(cell(r.run.apple?.text ?? "-")) |")
            }
            let p = pooled(model.phraseResults.compactMap(\.parakeetScore))
            let a = pooled(model.phraseResults.compactMap(\.appleScore))
            out.append("| all | \(model.phraseResults.count) phrases | \(percent(p)) | \(percent(a)) | | |")
            out.append("")
        }

        out.append("## Events")
        out.append("")
        let time = DateFormatter()
        time.dateFormat = "HH:mm:ss"
        for event in model.events {
            out.append("- \(time.string(from: event.time)) \(event.text)")
        }
        return out.joined(separator: "\n")
    }
}
