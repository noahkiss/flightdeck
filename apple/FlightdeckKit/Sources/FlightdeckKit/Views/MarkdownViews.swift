import MarkdownView
import SwiftUI

// Markdown in the chat (app.md 4.6): MarkdownView renders an agent's reply, a Capcom message and a
// Markdown file with native SwiftUI views. This file is the only one that imports it; the rest of
// the app uses `MarkdownBody`. Our own pieces: the type scale, the code block, and images that
// load nothing.

/// An agent's Markdown, rendered natively.
struct MarkdownBody: View {
    let text: String

    var body: some View {
        MarkdownView(text)
            .markdownStyle()
    }
}

/// The reply in a sheet with continuous text selection across blocks (app.md 4.12, Select Text).
struct SelectableMarkdown: View {
    let text: String

    var body: some View {
        MarkdownText(text)
            .markdownStyle()
    }
}

extension View {
    /// The chat's type scale and renderers for MarkdownView and MarkdownText.
    fileprivate func markdownStyle() -> some View {
        self
            .font(.title3.bold(), for: .h1)
            .font(.headline, for: .h2)
            .font(.subheadline.bold(), for: .h3)
            .font(.subheadline.bold(), for: .h4)
            .font(.subheadline.bold(), for: .h5)
            .font(.subheadline.bold(), for: .h6)
            .font(.callout.monospaced(), for: .codeBlock)
            .markdownCodeBlockStyle(ChatCodeBlockStyle())
            // Agent text is not trusted: an image in it never loads by itself (app.md 4.6).
            .markdownElementRenderer(.image(LinkOnlyImage(), urlScheme: "http"))
            .markdownElementRenderer(.image(LinkOnlyImage(), urlScheme: "https"))
    }
}

/// A code block: no soft wrap, it scrolls sideways with a cue while more lies to the right
/// (app.md 4.11), under a header with its language and a Copy button (4.12).
struct ChatCodeBlockStyle: MarkdownCodeBlockStyle {
    func makeBody(configuration: Configuration) -> some View {
        CodeBlockView(code: configuration.code, language: configuration.language)
    }
}

/// One code block, with its Copy button.
struct CodeBlockView: View {
    let code: String
    let language: String?
    @State private var copied = 0

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 6) {
                if let language, !language.isEmpty {
                    Text(language)
                        .font(.caption.weight(.medium))
                        .foregroundStyle(.secondary)
                }
                Spacer(minLength: 0)
                Button {
                    copyToPasteboard(code)
                    copied += 1
                } label: {
                    Label(copied > 0 ? "Copied" : "Copy", systemImage: copied > 0 ? "checkmark" : "doc.on.doc")
                        .font(.caption.weight(.medium))
                        .contentTransition(.symbolEffect(.replace))
                        .frame(minHeight: 28)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.borderless)
                .accessibilityLabel(copied > 0 ? "Code copied" : "Copy code")
                .sensoryFeedback(.success, trigger: copied)
            }
            .padding(.leading, 10)
            .padding(.trailing, 8)
            .padding(.top, 2)
            SideScroll { width in
                Text(verbatim: code)
                    .font(.callout.monospaced())
                    .fixedSize(horizontal: true, vertical: false)
                    .padding(.horizontal, 10)
                    .padding(.bottom, 10)
                    .padding(.top, 2)
                    .frame(minWidth: width, alignment: .leading)
            }
        }
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 8))
        // "Copied" goes back to "Copy" after a moment.
        .task(id: copied) {
            guard copied > 0 else { return }
            try? await Task.sleep(for: .seconds(2))
            copied = 0
        }
    }
}

/// An image in agent text: its alt text as a link. Nothing loads in the app; a tap opens the
/// address in the browser, where the owner sees it first.
struct LinkOnlyImage: MarkdownImageRenderer {
    func makeBody(configuration: MarkdownImageRendererConfiguration) -> some View {
        Link(destination: configuration.url) {
            Label(configuration.alternativeText.flatMap { $0.isEmpty ? nil : $0 } ?? (configuration.url.host() ?? "Image"),
                  systemImage: "photo")
        }
        .font(.callout)
    }
}
