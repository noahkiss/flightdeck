import Foundation
import SwiftUI

/// The third-party notices the app ships: `Acknowledgements.txt` in the app
/// bundle, written by `make notices` (docs/specs/ci-release.md 10.5).
struct AcknowledgementsView: View {
    static let title = "Acknowledgements"
    var bundle: Bundle = .main

    var body: some View {
        let parts = Self.sections(in: bundle)
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 16) {
                if parts.isEmpty {
                    Text("This build has no acknowledgements file.")
                        .foregroundStyle(.secondary)
                }
                ForEach(parts.indices, id: \.self) { i in
                    Text(parts[i])
                        .font(.system(.caption, design: .monospaced))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .padding()
        }
    }

    static func sections(in bundle: Bundle) -> [String] {
        guard let url = bundle.url(forResource: "Acknowledgements", withExtension: "txt"),
              let text = try? String(contentsOf: url, encoding: .utf8) else { return [] }
        return sections(of: text)
    }

    /// Splits the file at each entry rule and each licence file, so a long
    /// file renders as many small texts instead of one.
    static func sections(of text: String) -> [String] {
        let rule = "\n" + String(repeating: "=", count: 78) + "\n"
        return text.components(separatedBy: rule).flatMap { entry in
            entry.components(separatedBy: "\n--- ").enumerated().map { i, part in
                i == 0 ? part : "--- " + part
            }
        }
        .map { $0.trimmingCharacters(in: .newlines) }
        .filter { !$0.isEmpty }
    }
}
