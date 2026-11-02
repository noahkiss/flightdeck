import HabAPI
import SwiftUI

/// The console of one session, as a sheet (app.md 6, 11): it finds or starts
/// the session's console shell, then shows its cards.
public struct ConsoleSheet: View {
    let target: SessionTarget
    @Environment(\.habConnections) private var habs
    @Environment(\.capcom) private var capcom
    @Environment(\.dismiss) private var dismiss
    @State private var model: ConsoleModel?
    @State private var failure: String?
    @State private var attempt = 0

    public init(target: SessionTarget) {
        self.target = target
    }

    public var body: some View {
        NavigationStack {
            Group {
                if let model {
                    ConsoleView(model: model, restart: { attempt += 1 }, attachToCapcom: capcom.map { c in { card in attach(card, pane: model.pane, center: c) } })
                        .id(model.pane)
                } else if let failure {
                    ContentUnavailableView {
                        Label("No console", systemImage: "apple.terminal")
                    } description: {
                        Text(failure)
                    } actions: {
                        Button("Try again") { attempt += 1 }
                    }
                } else {
                    ProgressView("Starting a shell")
                }
            }
            .navigationTitle("Console")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 640, idealWidth: 820, minHeight: 480, idealHeight: 640)
        #endif
        .task(id: attempt) { await open() }
    }

    /// Opens Capcom with the command attached (capcom.md 8.4), on this session's hab.
    private func attach(_ card: ConsoleCard, pane: String, center: CapcomCenter) {
        let hab = target.habID
        dismiss()
        Task { await center.open(habID: hab, context: [CapcomContextRef(kind: .command, ref: "\(pane):\(card.id)")]) }
    }

    private func open() async {
        guard let session = target.session, let conn = habs.find(target.habID) else {
            failure = "This session's hab is not connected."
            return
        }
        model?.stop()
        model = nil
        failure = nil
        do {
            let pane = try await ConsoleOpener.open(session: session, calls: .init(connection: conn))
            model = ConsoleModel(pane: pane, connection: conn)
        } catch {
            failure = "The shell did not start: \(error)"
        }
    }
}

/// Cards for each command, the terminal for full-screen programs, and an
/// input line (app.md 6.1).
public struct ConsoleView: View {
    @Bindable var model: ConsoleModel
    /// Starts a new shell after this one exited.
    var restart: (() -> Void)?
    /// Opens Capcom with a command attached; nil offers no such action.
    var attachToCapcom: ((ConsoleCard) -> Void)?
    @State private var setupShown = false
    @FocusState private var inputFocused: Bool

    public init(model: ConsoleModel, restart: (() -> Void)? = nil, attachToCapcom: ((ConsoleCard) -> Void)? = nil) {
        self.model = model
        self.restart = restart
        self.attachToCapcom = attachToCapcom
    }

    public var body: some View {
        VStack(spacing: 0) {
            ZStack {
                if let t = model.terminal {
                    TerminalScreen(model: t)
                        .opacity(model.showsTerminal ? 1 : 0)
                        .allowsHitTesting(model.showsTerminal)
                }
                if !model.showsTerminal {
                    cards
                        .background(.background)
                }
            }
            if !model.showsTerminal {
                Divider()
                inputLine
            }
        }
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Picker("View", selection: viewChoice) {
                    Text("Cards").tag(false)
                    Text("Terminal").tag(true)
                }
                .pickerStyle(.segmented)
                .fixedSize()
                .disabled(model.fullScreenProgram)
            }
        }
        .sheet(isPresented: $setupShown) { ConsoleSetupSheet() }
        .onAppear {
            model.start()
            inputFocused = true
        }
        .onDisappear { model.stop() }
    }

    private var viewChoice: Binding<Bool> {
        Binding(get: { model.showsTerminal }, set: { model.terminalChosen = $0 })
    }

    @ViewBuilder private var cards: some View {
        if exited {
            ContentUnavailableView {
                Label("Shell exited", systemImage: "xmark.circle")
            } actions: {
                if let restart { Button("Start a new shell", action: restart) }
            }
        } else if model.loaded && model.list.cards.isEmpty {
            ContentUnavailableView {
                Label("No commands yet", systemImage: "apple.terminal")
            } description: {
                Text("Commands show here once the shell sends prompt marks.")
            } actions: {
                Button("Shell setup") { setupShown = true }
                Button("Show terminal") { model.terminalChosen = true }
            }
        } else {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 8) {
                        if model.list.more {
                            Button("Load older commands") { Task { await model.loadOlder() } }
                                .buttonStyle(.borderless)
                                .frame(maxWidth: .infinity)
                        }
                        ForEach(model.list.cards) { card in
                            CommandCardView(card: card, expanded: model.expanded.contains(card.id)) {
                                model.toggle(card.id)
                            }
                            .contextMenu {
                                if let attachToCapcom {
                                    Button("Attach to Capcom", systemImage: "paperclip") { attachToCapcom(card) }
                                }
                            }
                            .id(card.id)
                        }
                    }
                    .padding(12)
                }
                .onChange(of: model.list.cards.last?.id, initial: true) { _, last in
                    if let last { proxy.scrollTo(last, anchor: .bottom) }
                }
            }
        }
    }

    private var exited: Bool {
        guard let t = model.terminal else { return false }
        switch t.status {
        case .exited, .gone: return true
        default: return false
        }
    }

    private var inputLine: some View {
        HStack(spacing: 8) {
            Text("$")
                .font(.body.monospaced())
                .foregroundStyle(.secondary)
            TextField("Command", text: $model.draft)
                .font(.body.monospaced())
                .textFieldStyle(.plain)
                .autocorrectionDisabled()
                #if os(iOS)
                .textInputAutocapitalization(.never)
                .submitLabel(.send)
                #endif
                .focused($inputFocused)
                .onSubmit {
                    model.send()
                    inputFocused = true
                }
            Button {
                model.sendRaw("\u{03}")
            } label: {
                Label("Interrupt", systemImage: "stop.circle")
            }
            .labelStyle(.iconOnly)
            .buttonStyle(.borderless)
            .help("Send Ctrl+C")
            .disabled(exited)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 10)
        .disabled(exited)
    }
}

/// One command: its line, folder, exit status and duration; the output
/// folds open on tap.
struct CommandCardView: View {
    let card: ConsoleCard
    let expanded: Bool
    let toggle: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Button(action: toggle) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Image(systemName: expanded ? "chevron.down" : "chevron.right")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .frame(width: 12)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(card.title)
                            .font(.body.monospaced())
                            .lineLimit(expanded ? nil : 1)
                            .multilineTextAlignment(.leading)
                        if let cwd = card.command.cwd {
                            Text(cwd)
                                .font(.caption.monospaced())
                                .foregroundStyle(.secondary)
                                .lineLimit(1)
                                .truncationMode(.head)
                        }
                    }
                    Spacer(minLength: 8)
                    CardStatus(card: card)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityHint(expanded ? "Folds the output" : "Shows the output")
            if expanded {
                output
            }
        }
        .padding(10)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder private var output: some View {
        if let lines = card.output {
            if lines.isEmpty {
                Text("No output")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                ScrollView(.horizontal) {
                    VStack(alignment: .leading, spacing: 0) {
                        if card.outputCut {
                            Text("Earlier output not shown")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                                .padding(.bottom, 4)
                        }
                        Text(lines.joined(separator: "\n"))
                            .font(.callout.monospaced())
                            .textSelection(.enabled)
                            .fixedSize(horizontal: true, vertical: false)
                    }
                }
            }
        } else {
            ProgressView()
                .controlSize(.small)
        }
    }
}

/// Exit status and duration; a running command shows how long it has run.
struct CardStatus: View {
    let card: ConsoleCard

    var body: some View {
        HStack(spacing: 6) {
            if card.command.state == .running {
                ProgressView().controlSize(.mini)
                if let s = card.started {
                    TimelineView(.periodic(from: .now, by: 1)) { ctx in
                        Text(ConsoleTime.duration(ctx.date.timeIntervalSince(s)))
                            .monospacedDigit()
                    }
                } else {
                    Text("Running")
                }
            } else {
                Text(card.status)
                    .foregroundStyle(card.failed ? Color.red : Color.secondary)
                if let d = card.duration {
                    Text(ConsoleTime.duration(d))
                        .monospacedDigit()
                        .foregroundStyle(.secondary)
                }
            }
        }
        .font(.caption)
    }
}

/// The shell lines that make cards work: prompt marks (OSC 133) and the
/// working directory (OSC 7).
struct ConsoleSetupSheet: View {
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    Text("Add these lines to the shell's startup file on the hab's host, then start a new shell.")
                    Text("A prompt theme that rebuilds the prompt on every command, or a bash PROMPT_COMMAND array, drops the prompt marks.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    snippet("zsh (~/.zshrc)", ConsoleShellSetup.zsh)
                    snippet("bash 4.4 or newer (~/.bashrc)", ConsoleShellSetup.bash)
                }
                .padding()
            }
            .navigationTitle("Shell setup")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 560, minHeight: 420)
        #endif
    }

    private func snippet(_ title: String, _ text: String) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(title).font(.headline)
                Spacer()
                ShareLink(item: text) { Label("Share", systemImage: "square.and.arrow.up") }
                    .labelStyle(.iconOnly)
            }
            ScrollView(.horizontal) {
                Text(text)
                    .font(.caption.monospaced())
                    .textSelection(.enabled)
                    .fixedSize(horizontal: true, vertical: false)
                    .padding(8)
            }
            .background(.background.secondary, in: RoundedRectangle(cornerRadius: 6))
        }
    }
}

/// Shell lines that send the marks the hab reads (hab-logic-api.md 2.5).
public enum ConsoleShellSetup {
    public static let zsh = #"""
    # Flightdeck console: prompt marks (OSC 133) and the folder (OSC 7).
    _fd_enc() {
      local LC_ALL=C s=$1 out= c
      local -i i
      for (( i = 1; i <= ${#s}; i++ )); do
        c=$s[i]
        if [[ $c == [A-Za-z0-9._~/-] ]]; then out+=$c; else printf -v c '%%%02X' "'$c"; out+=$c; fi
      done
      REPLY=$out
    }
    _fd_precmd() {
      local s=$?
      _fd_enc "$PWD"
      print -rn -- $'\e]133;D;'$s$'\a\e]7;file://'${HOST}${REPLY}$'\a'
    }
    _fd_preexec() {
      _fd_enc "$1"
      print -rn -- $'\e]133;C;cmdline_url='$REPLY$'\a'
    }
    typeset -ga precmd_functions preexec_functions
    precmd_functions=(_fd_precmd $precmd_functions)
    preexec_functions+=(_fd_preexec)
    PS1=$'%{\e]133;A\a%}'$PS1$'%{\e]133;B\a%}'
    """#

    public static let bash = #"""
    # Flightdeck console: prompt marks (OSC 133) and the folder (OSC 7).
    _fd_enc() {
      local LC_ALL=C s=$1 out= c i
      for (( i = 0; i < ${#s}; i++ )); do
        c=${s:i:1}
        case $c in [A-Za-z0-9._~/-]) out+=$c ;; *) printf -v c '%%%02X' "'$c"; out+=$c ;; esac
      done
      REPLY=$out
    }
    _fd_precmd() {
      local s=$?
      _fd_enc "$PWD"
      printf '\033]133;D;%s\007\033]7;file://%s%s\007' "$s" "$HOSTNAME" "$REPLY"
    }
    PROMPT_COMMAND="_fd_precmd${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
    PS1='\[\033]133;A\007\]'"$PS1"'\[\033]133;B\007\]'
    PS0='\033]133;C\007'
    """#
}
