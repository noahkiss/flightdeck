import SwiftUI

/// "Show in chat" (app.md 4.11): one switch per kind of chat content, for every session or
/// only this one. A sheet sized to its content.
struct ChatShowSheet: View {
    let store: ChatShowStore
    let target: SessionTarget
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Picker("Applies to", selection: Binding(get: { store.scope(for: target) }, set: { store.setScope($0, for: target) })) {
                        ForEach(ChatShowScope.allCases) { Text($0.title).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    .labelsHidden()
                    .listRowBackground(Color.clear)
                    .listRowInsets(EdgeInsets())
                }
                Section {
                    ForEach(ChatContentKind.allCases) { k in
                        Toggle(isOn: Binding(
                            get: { !store.hidden(for: target).contains(k) },
                            set: { store.set(k, shown: $0, scope: store.scope(for: target), for: target) }
                        )) {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(k.title)
                                if let s = k.subtitle {
                                    Text(s).font(.footnote).foregroundStyle(.secondary)
                                }
                            }
                        }
                    }
                } footer: {
                    Text("Replies, questions, approvals and errors always show. Hidden steps fold into one line you can open.")
                }
            }
            .navigationTitle("Show in chat")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
        .presentationDetents([.large])
        #if os(macOS)
        .frame(minWidth: 420, minHeight: 520)
        #endif
    }
}

/// The More menu's "Show in chat" item, and the sheet it opens.
struct ChatShowButton: View {
    @Binding var shown: Bool

    var body: some View {
        Button("Show in chat", systemImage: "line.3.horizontal.decrease.circle") { shown = true }
            .help("Choose what the chat shows")
    }

    /// Debug builds only: `-FlightdeckShowChatFilter YES` opens the sheet at launch.
    static var atLaunch: Bool {
        #if DEBUG
        return UserDefaults.standard.bool(forKey: "FlightdeckShowChatFilter")
        #else
        return false
        #endif
    }
}
