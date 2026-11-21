import HabAPI
import SwiftUI

extension NewSessionDraft: Identifiable {
    public var id: String { key }
}

/// The New session control (app.md 3.10): a plus in the bar that opens the form.
struct NewSessionButton: View {
    let center: NewSessionCenter
    @Binding var draft: NewSessionDraft?

    var body: some View {
        Button("New session", systemImage: "plus") { draft = center.draft() }
            .disabled(center.habs.isEmpty)
            .help("Start a session")
    }
}

/// Shows the New session form, and opens the started session once the board lists it.
struct NewSessionHost: ViewModifier {
    let center: NewSessionCenter
    let board: Board
    @Binding var draft: NewSessionDraft?
    let open: (BoardRow) -> Void

    func body(content: Content) -> some View {
        content
            .sheet(item: $draft) { d in
                NewSessionSheet(center: center, draft: d) { draft = nil }
            }
            .onChange(of: center.pendingRow(in: board.rows)) { _, r in
                guard let r else { return }
                center.opened()
                open(r)
            }
    }
}

/// The New session form (app.md 3.10): the computer, where to start, and the agent.
struct NewSessionSheet: View {
    let center: NewSessionCenter
    @State var draft: NewSessionDraft
    let close: () -> Void
    @State private var error: String?
    @State private var starting = false

    init(center: NewSessionCenter, draft: NewSessionDraft, close: @escaping () -> Void) {
        self.center = center
        _draft = State(initialValue: draft)
        self.close = close
    }

    var body: some View {
        let choices = center.choices(habID: draft.habID)
        NavigationStack {
            Form {
                Section {
                    Picker("Computer", selection: $draft.habID) {
                        ForEach(center.habs) { Text($0.label).tag($0.id) }
                    }
                    Picker("Project", selection: $draft.place) {
                        Text("None").tag(NewSessionDraft.Place.home)
                        ForEach(choices.projects) { p in
                            Text(p.name).tag(NewSessionDraft.Place.project(p.id))
                        }
                        Text("Other folder").tag(NewSessionDraft.Place.folder)
                    }
                    if let p = project(choices), p.folders.count > 1 {
                        Picker("Folder", selection: folderBinding(p)) {
                            ForEach(p.folders, id: \.self) { Text(Self.short($0)).tag($0) }
                        }
                    }
                    if draft.place == .folder {
                        TextField("Full path, starting with /", text: $draft.typedFolder)
                            .font(.body.monospaced())
                            .autocorrectionDisabled()
                            #if os(iOS)
                            .textInputAutocapitalization(.never)
                            #endif
                            .onSubmit { Task { await start() } }
                    }
                } footer: {
                    Text(whereLine(choices))
                }
                Section {
                    Picker("Agent", selection: $draft.harness) {
                        ForEach(choices.harnesses, id: \.self) { h in
                            Text(OpenRequest.harnessName(h)).tag(String?.some(h))
                        }
                        Text("Shell").tag(String?.none)
                    }
                } footer: {
                    if !center.loading.contains(draft.habID), choices.harnesses.isEmpty {
                        Text("No agent was found on this computer. A shell starts.")
                    }
                }
                if let error {
                    Text(error).foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .scrollDismissesKeyboard(.interactively)
            .navigationTitle("New session")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    // Off while Start runs: the session it starts opens when the call answers.
                    Button("Cancel", action: close)
                        .disabled(starting)
                        .keyboardShortcut(.cancelAction)
                }
                ToolbarItem(placement: .confirmationAction) {
                    if starting {
                        ProgressView()
                    } else {
                        Button("Start") { Task { await start() } }
                            .disabled(center.habs.isEmpty || center.loading.contains(draft.habID))
                    }
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 440, minHeight: 340)
        #endif
        .task(id: draft.habID) {
            error = nil
            await center.load(habID: draft.habID)
            draft.fit(center.choices(habID: draft.habID))
        }
    }

    private func project(_ c: NewSessionChoices) -> NewSessionProject? {
        guard case .project(let id) = draft.place else { return nil }
        return c.projects.first { $0.id == id }
    }

    private func folderBinding(_ p: NewSessionProject) -> Binding<String> {
        Binding(get: { draft.folder ?? p.folders.first ?? "" }, set: { draft.folder = $0 })
    }

    /// The footer under where to start: the folder it starts in, in words.
    private func whereLine(_ c: NewSessionChoices) -> String {
        if center.loading.contains(draft.habID) { return "Reading the projects on this computer." }
        switch draft.place {
        case .home:
            return "Starts in the home folder, in no project."
        case .folder:
            return "The folder must be inside a git repository."
        case .project:
            guard let p = project(c) else { return "" }
            if let f = draft.folder ?? p.folders.first { return "Starts in \(f)." }
            return "Starts in the folder the project last used."
        }
    }

    /// A folder as the picker shows it: its last two parts, so worktrees stay apart.
    static func short(_ path: String) -> String {
        let parts = path.split(separator: "/")
        return parts.suffix(2).joined(separator: "/")
    }

    private func start() async {
        guard !starting else { return }
        error = nil
        starting = true
        defer { starting = false }
        do {
            draft.prepareStart()
            try await center.start(draft)
            close()
        } catch {
            self.error = (error as? NewSessionError)?.description ?? "\(error)"
        }
    }
}
