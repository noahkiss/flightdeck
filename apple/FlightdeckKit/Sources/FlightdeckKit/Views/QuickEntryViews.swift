#if os(macOS)
import SwiftUI

/// The quick entry panel's content (app.md 3.18): the field, where the message goes, Send and
/// Send to New Chat, and the recent messages. The Mac app's panel hosts it.
public struct QuickEntryView: View {
    @Bindable private var entry: QuickEntry
    /// Changes each time the panel shows, so the field takes the keyboard again.
    private let shown: Int
    private let close: () -> Void
    @FocusState private var focused: Bool
    @State private var selection: TextSelection?

    public init(entry: QuickEntry, shown: Int, close: @escaping () -> Void) {
        self.entry = entry
        self.shown = shown
        self.close = close
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(alignment: .firstTextBaseline, spacing: 10) {
                Image(systemName: "dot.radiowaves.left.and.right")
                    .foregroundStyle(.tint)
                    .accessibilityHidden(true)
                TextField("Ask Capcom", text: $entry.text, selection: $selection, axis: .vertical)
                    .textFieldStyle(.plain)
                    .lineLimit(1...6)
                    .focused($focused)
                    .accessibilityLabel("Message to Capcom")
                    .onKeyPress(.return, phases: .down) { press in returnKey(press.modifiers) }
                    .onChange(of: entry.text) { _, _ in entry.failure = nil }
            }
            .font(.title3)
            if let why = entry.failure {
                Text(why)
                    .font(.callout)
                    .foregroundStyle(.red)
            }
            HStack(spacing: 8) {
                Text(entry.line)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                Spacer(minLength: 8)
                if entry.sending {
                    ProgressView().controlSize(.small)
                }
                Button("Send to New Chat") { send(new: true) }
                    .disabled(!entry.canSend)
                    .help("Start a new Capcom conversation with this message (⌘Return)")
                Button("Send") { send(new: false) }
                    .buttonStyle(.borderedProminent)
                    .disabled(!entry.canSend)
                    .help("Send to Capcom's conversation (Return)")
            }
            if !entry.recents.isEmpty {
                Divider()
                HStack {
                    Text("Recent")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.secondary)
                    Spacer()
                    Button("Clear") { entry.clearRecents() }
                        .buttonStyle(.borderless)
                        .font(.caption)
                }
                ForEach(entry.recents, id: \.self) { recent in
                    Button {
                        entry.pick(recent)
                        focused = true
                    } label: {
                        Label(recent, systemImage: "clock.arrow.circlepath")
                            .lineLimit(1)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityHint("Puts the message back in the field")
                }
            }
        }
        .padding(18)
        .frame(width: 620)
        .glassEffect(.regular, in: .rect(cornerRadius: 22))
        .onExitCommand(perform: close)
        .task(id: shown) { focused = true }
    }

    /// Return sends, ⌘Return sends to a new chat, Shift- or Option-Return writes a new line, as
    /// the composer (app.md 4.2). With a Japanese, Chinese or Korean input method a plain Return
    /// stays the input method's.
    private func returnKey(_ modifiers: EventModifiers) -> KeyPress.Result {
        if modifiers.contains(.command) {
            send(new: true)
            return .handled
        }
        switch ComposerKeys.returnKey(modifiers) {
        case .send:
            guard ComposerKeys.returnSends(languages: ComposerKeys.inputLanguages) else { return .ignored }
            send(new: false)
        case .newline:
            (entry.text, selection) = ComposerKeys.insertingNewline(entry.text, at: selection)
        }
        return .handled
    }

    private func send(new: Bool) {
        guard entry.canSend else { return }
        Task {
            if await entry.send(new: new) { close() }
        }
    }
}
#endif
