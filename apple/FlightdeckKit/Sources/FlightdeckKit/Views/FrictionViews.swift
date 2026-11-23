import HabAPI
import SwiftUI

/// One friction note while the owner writes it (diagnostics.md 9): their words, and whether the
/// work could not be done in Flightdeck. Send stores it on the session's own hab with the
/// session, the pane and a tail of that hab's logs; the hab answers with the note's trace.
@MainActor @Observable
public final class FrictionNote {
    /// The hab stores friction notes (`diag.friction`).
    public nonisolated static let capability = "friction"
    /// The hab refuses a longer text (diagnostics.md 9.2).
    public nonisolated static let maxText = 2000

    public var text = ""
    /// The work could not be done in Flightdeck. Off: a preference.
    public var blocker = false
    public private(set) var sending = false
    /// The stored note's trace; set once the hab took the note.
    public private(set) var trace: String?
    public private(set) var error: String?

    let session: String?
    let pane: String?
    @ObservationIgnored let connection: HabConnection?

    public init(session: String?, pane: String?, connection: HabConnection?) {
        self.session = session
        self.pane = pane
        self.connection = connection
    }

    var trimmed: String { text.trimmingCharacters(in: .whitespacesAndNewlines) }

    public var canSend: Bool {
        !trimmed.isEmpty && trimmed.count <= Self.maxText && !sending && trace == nil && connection != nil
    }

    public func send() async {
        guard canSend, let connection else { return }
        sending = true
        error = nil
        defer { sending = false }
        do {
            let res = try await connection.call(HabMethods.diagFriction, FrictionParams(
                blocker: blocker ? true : nil, pane: pane, session: session, text: trimmed))
            trace = res.trace
        } catch {
            self.error = "Not saved: \(ChatModel.describe(error))"
        }
    }
}

/// The sheet that takes a friction note: a multi-line field, the blocker switch, Cancel and
/// Send. Once the hab takes the note it shows the trace and Done.
struct FrictionSheet: View {
    @State private var note: FrictionNote
    let host: String
    @Environment(\.dismiss) private var dismiss
    @FocusState private var focused: Bool

    init(note: FrictionNote, host: String) {
        _note = State(initialValue: note)
        self.host = host
    }

    var body: some View {
        NavigationStack {
            Form {
                if let trace = note.trace {
                    Section {
                        Label("Note saved", systemImage: "checkmark.circle")
                        LabeledContent("Trace") {
                            Text(trace).monospaced().textSelection(.enabled)
                        }
                    } footer: {
                        Text("`gff friction show \(trace)` on \(host) prints it with its log.")
                    }
                } else {
                    Section {
                        TextField("What fell short?", text: $note.text, axis: .vertical)
                            .lineLimit(4...10)
                            .focused($focused)
                    } footer: {
                        if note.trimmed.count > FrictionNote.maxText {
                            Text("\(note.trimmed.count) of \(FrictionNote.maxText) characters")
                                .foregroundStyle(.red)
                        } else {
                            Text("Saved on \(host) with this session and the last 10 minutes of its logs.")
                        }
                    }
                    Section {
                        Toggle("Blocker", isOn: $note.blocker)
                    } footer: {
                        Text("Turn on when you could not do the work in Flightdeck.")
                    }
                    if let error = note.error {
                        Section { Text(error).foregroundStyle(.red) }
                    }
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Note friction")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                if note.trace == nil {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel") { dismiss() }
                            .keyboardShortcut(.cancelAction)
                    }
                    ToolbarItem(placement: .confirmationAction) {
                        if note.sending {
                            ProgressView()
                        } else {
                            // The note takes several lines: Return writes one, Cmd-Return sends (app.md 3.8).
                            Button("Send") { Task { await note.send() } }
                                .disabled(!note.canSend)
                                .keyboardShortcut(.return, modifiers: .command)
                        }
                    }
                } else {
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Done") { dismiss() }
                            .keyboardShortcut(.defaultAction)
                    }
                }
            }
            .onAppear { focused = true }
        }
        // Typed words are work a stray swipe would lose (app.md 3.8).
        .interactiveDismissDisabled(note.trace == nil && (note.sending || !note.trimmed.isEmpty))
        .presentationDetents([.medium, .large])
        #if os(macOS)
        .frame(minWidth: 420, minHeight: 320)
        #endif
    }
}
