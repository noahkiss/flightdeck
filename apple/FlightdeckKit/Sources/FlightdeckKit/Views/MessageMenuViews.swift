import SwiftUI

/// Reads one message aloud with the system voice the owner chose for voice mode (app.md 4.12).
/// One message at a time; Read Aloud on another stops the first.
@MainActor
@Observable
final class ReadAloud {
    static let shared = ReadAloud()
    /// The row being read, if any.
    private(set) var speaking: String?
    @ObservationIgnored private let player = VoicePreviewer()

    private init() {
        player.onEnd = { [weak self] in
            guard let self, !self.player.isSpeaking else { return }
            self.speaking = nil
        }
    }

    func read(_ text: String, id: String, voice: String?) {
        speaking = id
        player.play(text, voice: SpeechVoices.system(chosen: voice))
    }

    func stop() {
        player.stop()
        speaking = nil
    }
}

/// The long-press menu of a message (app.md 4.12): Copy, Select Text, Share, Read Aloud and
/// Copy Last Code Block; a tool card's Copy Command and Copy Output. Each action reads the row's
/// current text through `current`, so the menu works while a reply streams.
struct MessageMenuModifier: ViewModifier {
    let id: String
    let content: MessageMenu.Content?
    /// The row's content now, looked up by id when an action runs.
    var current: (() -> MessageMenu.Content?)?
    @State private var selecting: SelectText?
    @State private var copied = 0
    @Environment(\.voiceKit) private var voiceKit

    func body(content view: Content) -> some View {
        if let content {
            view
                #if os(macOS)
                // The Mac selects text by dragging; the menu is a right click.
                .textSelection(.enabled)
                #endif
                .contextMenu { menu(content) }
                .sensoryFeedback(.selection, trigger: copied)
                .sheet(item: $selecting) { s in
                    SelectTextSheet(text: s.text)
                }
        } else {
            view
        }
    }

    @ViewBuilder private func menu(_ c: MessageMenu.Content) -> some View {
        ForEach(MessageMenu.actions(c), id: \.self) { a in
            switch a {
            case .copy: button("Copy", "doc.on.doc", a)
            case .copyLastCode: button("Copy Last Code Block", "curlybraces", a)
            case .copyCommand: button("Copy Command", "terminal", a)
            case .copyOutput: button("Copy Output", "doc.plaintext", a)
            case .selectText:
                Button("Select Text", systemImage: "selection.pin.in.out") {
                    selecting = SelectText(text: (current?() ?? c).text)
                }
            case .share:
                ShareLink(item: c.text) {
                    Label("Share", systemImage: "square.and.arrow.up")
                }
            case .readAloud:
                if ReadAloud.shared.speaking == id {
                    Button("Stop Reading", systemImage: "stop.circle") { ReadAloud.shared.stop() }
                } else {
                    Button("Read Aloud", systemImage: "speaker.wave.2") {
                        ReadAloud.shared.read((current?() ?? c).speech, id: id, voice: voiceKit?.settings.value.speechVoice)
                    }
                }
            }
        }
    }

    private func button(_ title: String, _ symbol: String, _ a: MessageMenu.Action) -> some View {
        Button(title, systemImage: symbol) {
            guard let text = MessageMenu.copied(a, current?() ?? content ?? MessageMenu.Content(text: "", speech: "")) else { return }
            copyToPasteboard(text)
            copied += 1
        }
    }

    struct SelectText: Identifiable {
        let id = UUID()
        let text: String
    }
}

extension View {
    /// The message menu for a row; nil content adds none.
    func messageMenu(id: String, content: MessageMenu.Content?, current: (() -> MessageMenu.Content?)? = nil) -> some View {
        modifier(MessageMenuModifier(id: id, content: content, current: current))
    }
}

/// A message in a sheet, where any part of it can be selected and copied across blocks.
struct SelectTextSheet: View {
    let text: String
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                SelectableMarkdown(text: text)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
            }
            .navigationTitle("Select Text")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 420, minHeight: 360)
        #endif
        .presentationDetents([.medium, .large])
    }
}

/// What a session chat's rows need for their menus: each row's current content by id.
@MainActor
struct MessageActions {
    let model: ChatModel

    func current(_ id: String) -> MessageMenu.Content? {
        model.items.first { $0.id == id }.flatMap(MessageMenu.content)
    }
}
