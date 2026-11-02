import HabAPI
import SwiftUI

/// The app's root: the status board. On the Mac it also counts windows for
/// the lifecycle rule. On iPhone and iPad the `App` drives the rule with
/// `Fleet.apply(_:)` from its own scene phase, which combines every window.
public struct FlightdeckRoot: View {
    private let fleet: Fleet

    public init(fleet: Fleet) {
        self.fleet = fleet
    }

    public var body: some View {
        #if os(macOS)
        BoardScreen(fleet: fleet)
            .environment(\.habConnections, fleet.lookup)
            .environment(\.terminals, fleet.terminals)
            .openPaneOnLaunch(fleet)
            .environment(\.sshSetup, fleet.ssh)
            .environment(\.voiceKit, fleet.voice)
            .environment(\.sendFiles, fleet.files)
            .sshSheets(fleet.ssh)
            .onAppear { fleet.windowOpened() }
            .onDisappear { fleet.windowClosed() }
        #else
        BoardScreen(fleet: fleet)
            .environment(\.habConnections, fleet.lookup)
            .environment(\.terminals, fleet.terminals)
            .openPaneOnLaunch(fleet)
            .environment(\.sshSetup, fleet.ssh)
            .environment(\.voiceKit, fleet.voice)
            .environment(\.sendFiles, fleet.files)
            .sshSheets(fleet.ssh)
        #endif
    }
}

extension Fleet {
    /// iPhone and iPad: the app-level scene phase, which is active while any
    /// window is active. `inactive` keeps the connections and starts the Live
    /// Activity.
    public func apply(_ phase: ScenePhase) {
        switch phase {
        case .active: scene(active: true)
        case .background: scene(active: false)
        default: scene(active: nil)
        }
    }
}

/// The status board: one list on iPhone; on iPad and Mac the list plus a
/// second column with session cards and connections (app.md 3.3).
public struct BoardScreen: View {
    private let board: Board
    /// With a fleet, a row opens its session view; without, a sheet of its fields.
    private let fleet: Fleet?
    @State private var opened: BoardRow?
    @State private var showConnections = false
    #if os(iOS)
    @Environment(\.horizontalSizeClass) private var sizeClass
    #endif

    public init(board: Board) {
        self.board = board
        self.fleet = nil
    }

    public init(fleet: Fleet) {
        self.board = fleet.board
        self.fleet = fleet
    }

    /// The opened row's session view, when a fleet supplies chat models.
    private func session(_ row: BoardRow, compact: Bool) -> some View {
        SessionView(model: fleet!.chat(for: row.target), compact: compact, close: { opened = nil })
            .id(row.target)
            // session.focus while the session is on screen (app.md 8.6).
            .onAppear {
                fleet?.focus.show(row.target)
                Task { await fleet?.activities?.opened(row.target) }
            }
            .onDisappear { fleet?.focus.hide(row.target) }
    }

    /// A rank 1 row with a request opens the approval sheet (app.md 7.2).
    private func openRequest(_ row: BoardRow) -> Bool {
        guard row.rank == .blocked, !row.offline, let a = fleet?.approvals else { return false }
        a.open(row)
        return a.presenting
    }

    /// The sheet's chat button: open the session at the request.
    private func openChat(_ t: SessionTarget) {
        fleet?.approvals?.presenting = false
        if let row = board.rows.first(where: { $0.target == t }) { opened = row }
    }

    private var compact: Bool {
        #if os(iOS)
        return sizeClass == .compact
        #else
        return false
        #endif
    }

    /// Opens the usage sheet, closing the Connections sheet first when it is up.
    private func showUsage(_ account: String?) {
        guard let book = fleet?.usage?.book else { return }
        guard showConnections else {
            book.present(account: account)
            return
        }
        showConnections = false
        book.presented = nil
        Task { @MainActor in
            try? await Task.sleep(for: .milliseconds(500))
            book.present(account: account)
        }
    }

    public var body: some View {
        Group {
            if compact {
                NavigationStack {
                    BoardList(board: board, opened: $opened, openRequest: openRequest)
                        .usageFooter(fleet?.usage)
                        .navigationTitle("Flightdeck")
                        .navigationDestination(item: fleet == nil ? .constant(nil) : $opened) { row in
                            session(row, compact: true)
                        }
                        .toolbar {
                            ToolbarItem {
                                Button {
                                    showConnections = true
                                } label: {
                                    Label("Connections", systemImage: "server.rack")
                                }
                            }
                        }
                }
                .sheet(isPresented: $showConnections) {
                    NavigationStack {
                        ConnectionsList(board: board, push: fleet?.push, usage: fleet?.usage, settings: fleet?.settings, openUsage: { showUsage(nil) })
                            .navigationTitle("Connections")
                            .toolbar {
                                ToolbarItem(placement: .confirmationAction) {
                                    Button("Done") { showConnections = false }
                                }
                            }
                    }
                }
            } else if fleet != nil, let row = opened {
                HStack(spacing: 0) {
                    NavigationStack {
                        BoardList(board: board, opened: $opened, openRequest: openRequest)
                        .usageFooter(fleet?.usage)
                            .navigationTitle("Flightdeck")
                    }
                    .frame(minWidth: 300, idealWidth: 340, maxWidth: 380)
                    Divider()
                    session(row, compact: false)
                        .frame(minWidth: 640)
                }
            } else {
                HStack(spacing: 0) {
                    NavigationStack {
                        BoardList(board: board, opened: $opened, openRequest: openRequest)
                        .usageFooter(fleet?.usage)
                            .navigationTitle("Flightdeck")
                    }
                    .frame(minWidth: 360)
                    Divider()
                    VStack(spacing: 0) {
                        SessionCardsPlaceholder()
                        Divider()
                        ConnectionsList(board: board, push: fleet?.push, usage: fleet?.usage, settings: fleet?.settings)
                    }
                    .frame(minWidth: 260, idealWidth: 300, maxWidth: 360)
                }
            }
        }
        .sheet(item: fleet == nil ? $opened : .constant(nil)) { row in
            SessionPlaceholder(row: row) { opened = nil }
        }
        .modifier(ApprovalSheetHost(center: fleet?.approvals, openChat: openChat))
        .modifier(SendFileSheetHost(center: fleet?.files, board: board))
        // A file sent to a session from the send sheet opens that session (app.md 4.4).
        .onChange(of: fleet?.files.openRequest) { _, t in
            guard let t else { return }
            fleet?.files.openRequest = nil
            if let row = board.rows.first(where: { $0.target == t }) { opened = row }
        }
        .usageSheet(fleet?.usage)
        // A usage notification or widget tap while the Connections sheet is
        // up: close it first, since a second sheet cannot present over it.
        .onChange(of: fleet?.usage?.book.presented) { _, p in
            if let p, showConnections { showUsage(p.account) }
        }
        #if DEBUG
        // Debug builds only: `-FlightdeckOpenSession <session>` opens that session at launch,
        // so a build can be checked with no clicks.
        .task {
            guard fleet != nil, let id = UserDefaults.standard.string(forKey: "FlightdeckOpenSession"), !id.isEmpty else { return }
            for _ in 0..<100 {
                if let row = board.rows.first(where: { $0.target.session == id }) {
                    opened = row
                    return
                }
                try? await Task.sleep(for: .milliseconds(100))
            }
        }
        #endif
        #if os(macOS)
        .frame(minWidth: fleet != nil && opened != nil ? 960 : 680, minHeight: 420)
        #endif
    }
}

struct BoardList: View {
    let board: Board
    @Binding var opened: BoardRow?
    /// Returns true when the row opened the approval sheet instead.
    var openRequest: (BoardRow) -> Bool = { _ in false }

    var body: some View {
        TimelineView(.periodic(from: .now, by: 30)) { ctx in
            List {
                ForEach(board.habs.filter(\.needsUpdate)) { h in
                    Label("\(h.label): \(h.updateText)", systemImage: "exclamationmark.triangle")
                        .foregroundStyle(.orange)
                }
                ForEach(board.sections) { section in
                    Section(section.kind.rawValue) {
                        ForEach(section.rows) { row in
                            Button {
                                if openRequest(row) { return }
                                board.open(row)
                                opened = row
                            } label: {
                                BoardRowView(row: row, now: ctx.date)
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
            }
            .overlay {
                if board.sections.isEmpty {
                    ContentUnavailableView("No sessions", systemImage: "rectangle.stack",
                                           description: Text(board.habs.isEmpty ? "No habs configured." : "Sessions on your habs show here."))
                }
            }
        }
    }
}

struct BoardRowView: View {
    let row: BoardRow
    let now: Date

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Circle()
                .fill(row.stateColor)
                .frame(width: 9, height: 9)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline) {
                    Text(row.title)
                        .font(.body.weight(.medium))
                        .lineLimit(1)
                    Spacer(minLength: 8)
                    Text(row.stateText(now: now))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                if let c = row.context {
                    Text(c)
                        .font(.callout.monospaced())
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                HStack(spacing: 5) {
                    Image(systemName: row.harnessSymbol)
                        .accessibilityLabel(row.harness ?? "shell")
                    Text(row.detailLine)
                        .lineLimit(1)
                    if let pct = row.contextPct {
                        Text("\(pct)%")
                            .foregroundStyle(pct >= 95 ? .red : pct >= 80 ? .orange : .secondary)
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
                if row.offline {
                    Text(row.offlineSince.map { "Offline since \($0.formatted(date: .omitted, time: .shortened))" } ?? "Offline")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .padding(.vertical, 2)
        .contentShape(Rectangle())
        .opacity(row.offline ? 0.5 : 1)
    }
}

struct ConnectionsList: View {
    let board: Board
    var push: PushRegistrar?
    var usage: UsageCenter?
    var settings: SettingsCenter?
    /// Opens the usage sheet; the board closes this list first when it is a sheet.
    var openUsage: (() -> Void)?
    @Environment(\.sshSetup) private var ssh
    @Environment(\.voiceKit) private var voice
    @State private var showSSH = false
    @State private var showNotifications = false
    @State private var showVoice = false
    @State private var showSettings = false

    var body: some View {
        List {
            Section("\(board.onlineCount) of \(board.habs.count) online") {
                ForEach(board.habs) { h in
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        Circle()
                            .fill(h.state.status == .online ? Color.green : Color.secondary.opacity(0.5))
                            .frame(width: 8, height: 8)
                            .accessibilityHidden(true)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(h.label)
                            Text(h.endpoint)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                            if h.state.status != .online, let e = h.state.lastError {
                                Text(e)
                                    .font(.caption2)
                                    .foregroundStyle(.secondary)
                                    .lineLimit(2)
                            }
                        }
                        Spacer()
                        Text(h.statusText)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            Section {
                if settings != nil {
                    Button("Settings") { showSettings = true }
                }
                if ssh != nil, settings == nil {
                    Button("SSH keys and habs") { showSSH = true }
                }
                if push != nil, settings == nil {
                    Button("Notifications") { showNotifications = true }
                }
                if usage != nil {
                    Button("Usage") {
                        if let openUsage { openUsage() } else { usage?.book.present(account: nil) }
                    }
                }
                if voice != nil, settings == nil {
                    Button("Voice") { showVoice = true }
                }
            }
        }
        .sheet(isPresented: $showSettings) {
            if let settings {
                NavigationStack {
                    SettingsPage(center: settings, board: board, push: push, openUsage: usage == nil ? nil : {
                        showSettings = false
                        if let openUsage { openUsage() } else { usage?.book.present(account: nil) }
                    })
                    .navigationTitle("Settings")
                    .toolbar {
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Done") { showSettings = false }
                        }
                    }
                }
                #if os(macOS)
                .frame(minWidth: 480, minHeight: 560)
                #endif
            }
        }
        .sheet(isPresented: $showVoice) {
            if let voice {
                NavigationStack {
                    VoiceSettingsView(kit: voice)
                        .navigationTitle("Voice")
                        .toolbar {
                            ToolbarItem(placement: .confirmationAction) {
                                Button("Done") { showVoice = false }
                            }
                        }
                }
                #if os(macOS)
                .frame(minWidth: 440, minHeight: 480)
                #endif
            }
        }
        .sheet(isPresented: $showNotifications) {
            if let push {
                NavigationStack {
                    NotificationSettingsView(push: push, board: board)
                        .navigationTitle("Notifications")
                        .toolbar {
                            ToolbarItem(placement: .confirmationAction) {
                                Button("Done") { showNotifications = false }
                            }
                        }
                }
                #if os(macOS)
                .frame(minWidth: 440, minHeight: 420)
                #endif
            }
        }
        .sheet(isPresented: $showSSH) {
            if let ssh {
                NavigationStack {
                    SSHSettingsView(setup: ssh)
                        .navigationTitle("SSH")
                        .toolbar {
                            ToolbarItem(placement: .confirmationAction) {
                                Button("Done") { showSSH = false }
                            }
                        }
                }
                #if os(macOS)
                .frame(minWidth: 480, minHeight: 520)
                #endif
            }
        }
    }
}

/// Holds the place of the session cards (live previews), which come with data streams.
struct SessionCardsPlaceholder: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Sessions")
                .font(.headline)
            RoundedRectangle(cornerRadius: 8)
                .fill(.quaternary)
                .frame(height: 96)
                .overlay(Text("No previews").font(.caption).foregroundStyle(.secondary))
        }
        .padding(12)
    }
}

/// Holds the place of the session view until the chat and terminal views exist.
struct SessionPlaceholder: View {
    let row: BoardRow
    let close: () -> Void
    @Environment(\.habConnections) private var habs

    var body: some View {
        NavigationStack {
            Form {
                if let pane = row.target.pane, let conn = habs.find(row.target.habID) {
                    NavigationLink("Terminal") {
                        TerminalScreen(pane: pane, connection: conn)
                            .navigationTitle(row.title)
                    }
                }
                LabeledContent("Hab", value: row.host)
                if let p = row.project { LabeledContent("Project", value: p) }
                if let h = row.harness { LabeledContent("Harness", value: h) }
                LabeledContent("State", value: row.state?.rawValue ?? "none")
                if let s = row.target.session { LabeledContent("Session", value: s) }
                if let p = row.target.pane { LabeledContent("Pane", value: p) }
            }
            .navigationTitle(row.title)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Close", action: close)
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 360, minHeight: 260)
        #endif
    }
}

// MARK: - Presentation

extension Board.Hab {
    var needsUpdate: Bool {
        if case .updateNeeded = state.status { return true }
        return false
    }

    var updateText: String {
        if case .updateNeeded(let side) = state.status {
            return side == .app ? "update the app" : "update the hab"
        }
        return ""
    }

    var statusText: String {
        switch state.status {
        case .online: return "Online"
        case .connecting: return "Connecting"
        case .stopped: return "Disconnected"
        case .offline:
            return state.lostAt.map { "Offline since \($0.formatted(date: .omitted, time: .shortened))" } ?? "Offline"
        case .updateNeeded(let side): return side == .app ? "Update the app" : "Update the hab"
        }
    }
}

extension BoardRow {
    var stateColor: Color {
        switch state {
        case .blocked?: return .orange
        case .error?: return .red
        case .done?: return rank == .doneUnseen ? .blue : .secondary
        case .working?: return .green
        case .idle?: return .secondary
        default: return .gray.opacity(0.5)
        }
    }

    var harnessSymbol: String {
        switch harness {
        case "claude"?: return "sparkle"
        case "codex"?: return "chevron.left.forwardslash.chevron.right"
        case "opencode"?: return "curlybraces"
        default: return "terminal"
        }
    }

    var detailLine: String {
        [project, model, host].compactMap { $0 }.joined(separator: " · ")
    }

    func stateText(now: Date) -> String {
        let word: String
        switch state {
        case .blocked?:
            switch blockedKind {
            case .question?: word = "Question"
            case .auth?: word = "Sign-in"
            default: word = "Permission"
            }
        case .error?: word = "Error"
        case .done?: word = "Done"
        case .working?: word = "Working"
        case .idle?: word = "Idle"
        case .exited?: word = "Exited"
        default: word = target.session == nil ? "Unfiled" : "No agent"
        }
        guard let since else { return word }
        return word + " · " + Self.age(since, now: now)
    }

    static func age(_ d: Date, now: Date) -> String {
        let s = max(0, Int(now.timeIntervalSince(d)))
        if s < 60 { return "now" }
        if s < 3600 { return "\(s / 60) min" }
        if s < 86400 { return "\(s / 3600) h" }
        return "\(s / 86400) d"
    }
}

/// Shows the approval sheet while the center asks for it.
struct ApprovalSheetHost: ViewModifier {
    let center: ApprovalCenter?
    let openChat: (SessionTarget) -> Void

    func body(content: Content) -> some View {
        if let center {
            content.sheet(isPresented: Binding(get: { center.presenting }, set: { center.presenting = $0 })) {
                ApprovalSheet(model: center.sheet, openChat: openChat) { center.presenting = false }
            }
        } else {
            content
        }
    }
}
