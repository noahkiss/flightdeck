import HabAPI
import SwiftUI

/// The app's root: the tab shell on iPhone and iPad, the status board on the Mac. On the Mac
/// it also counts windows for
/// the lifecycle rule. On iPhone and iPad the `App` drives the rule with
/// `Fleet.apply(_:)` from its own scene phase, which combines every window.
public struct FlightdeckRoot: View {
    private let fleet: Fleet
    /// The window's commands for the menus (app.md 3.13), and its token for its own opens.
    @State private var commands = WindowCommands(kind: .board)

    public init(fleet: Fleet) {
        self.fleet = fleet
    }

    public var body: some View {
        shell
            .openPaneOnLaunch(fleet)
            .modifier(FleetEnvironment(fleet: fleet))
            .sshSheets(fleet.ssh)
            .modifier(PublishCommands(fleet: fleet, commands: commands))
            .modifier(CountsWindow(fleet: fleet))
            #if os(macOS)
            .onAppear { fleet.boardWindowOpened() }
            .onDisappear { fleet.boardWindowClosed() }
            #endif
    }

    @ViewBuilder
    private var shell: some View {
        #if os(macOS)
        BoardScreen(fleet: fleet)
        #else
        // iPhone and iPad: the tab shell (app.md 3.8).
        AppShell(fleet: fleet)
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

/// The status board: one list on iPhone; on iPad and Mac the list, headed
/// "Sessions", plus a second column with connections (app.md 3.3). Session
/// cards join that column when live previews exist.
public struct BoardScreen: View {
    private let board: Board
    /// With a fleet, a row opens its session view; without, a sheet of its fields.
    private let fleet: Fleet?
    @State private var opened: BoardRow?
    /// The Capcom view is open (app.md 4.10); it and `opened` never both hold.
    @State private var capcomShown = false
    /// The open session's row id, or "capcom", kept per window (app.md 3.12).
    @SceneStorage("flightdeck.board.open") private var savedOpen = ""
    /// The board's search and host scope (app.md 3.6), here so they outlive a switch of layout.
    @State private var query = ""
    @State private var scope: BoardSearchScope = .all
    @State private var showConnections = false
    /// The New session form, while it is open (app.md 3.10).
    @State private var newSession: NewSessionDraft?
    /// Debug builds: the Capcom jobs list opened at launch.
    @State private var capcomJobsShown = false
    #if os(iOS)
    @Environment(\.horizontalSizeClass) private var sizeClass
    #endif
    /// The window's commands (app.md 3.13); its id counts this window's opens (app.md 3.12).
    @Environment(\.windowCommands) private var commands
    @Environment(\.openWindow) private var openWindow
    @Environment(\.supportsMultipleWindows) private var multipleWindows

    private var window: UUID? { commands?.id }

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
            .sessionHandoff(row, board: board)
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
        a.open(row, window: window)
        return a.presenting
    }

    /// The board's Capcom row: opens the Capcom view; nil without a fleet.
    private var openCapcom: (() -> Void)? {
        guard fleet != nil else { return nil }
        return { capcomShown = true }
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

    /// The board and, beside it, the open session or the Capcom view.
    @ViewBuilder
    private var layout: some View {
            if compact {
                NavigationStack {
                    BoardList(board: board, opened: $opened, actions: fleet?.sessionActions, query: $query, scope: $scope, finds: commands?.finds ?? 0, openRequest: openRequest, openCapcom: openCapcom)
                        .usageFooter(fleet?.usage)
                        .navigationTitle("Flightdeck")
                        .navigationDestination(item: fleet == nil ? .constant(nil) : $opened) { row in
                            session(row, compact: true)
                        }
                        .navigationDestination(isPresented: $capcomShown) {
                            if let fleet { CapcomScreen(fleet: fleet, compact: true, close: nil) }
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
                                        .keyboardShortcut(.cancelAction)
                                }
                            }
                    }
                }
            } else if let fleet, capcomShown {
                BoardSessionSplit {
                    NavigationStack {
                        BoardList(board: board, opened: $opened, actions: fleet.sessionActions, query: $query, scope: $scope, finds: commands?.finds ?? 0, openRequest: openRequest, openCapcom: openCapcom, heading: "Sessions")
                            .usageFooter(fleet.usage)
                            .navigationTitle("Flightdeck")
                    }
                } session: {
                    CapcomScreen(fleet: fleet, compact: false, close: { capcomShown = false })
                }
            } else if fleet != nil, let row = opened {
                BoardSessionSplit {
                    NavigationStack {
                        BoardList(board: board, opened: $opened, actions: fleet?.sessionActions, query: $query, scope: $scope, finds: commands?.finds ?? 0, openRequest: openRequest, openCapcom: openCapcom, heading: "Sessions")
                        .usageFooter(fleet?.usage)
                            .navigationTitle("Flightdeck")
                    }
                } session: {
                    session(row, compact: false)
                }
            } else {
                // The session cards column (app.md 3.3) returns when live previews exist.
                HStack(spacing: 0) {
                    NavigationStack {
                        BoardList(board: board, opened: $opened, actions: fleet?.sessionActions, query: $query, scope: $scope, finds: commands?.finds ?? 0, openRequest: openRequest, openCapcom: openCapcom, heading: "Sessions")
                        .usageFooter(fleet?.usage)
                            .navigationTitle("Flightdeck")
                    }
                    .frame(minWidth: 360)
                    Divider()
                    ConnectionsList(board: board, push: fleet?.push, usage: fleet?.usage, settings: fleet?.settings)
                        .frame(minWidth: 260, idealWidth: 300, maxWidth: 360)
                }
            }
    }

    /// Opens a row in this window, as a tap does.
    private func open(_ row: BoardRow) {
        board.open(row, window: window)
        opened = row
    }

    /// The board's part of the menus (app.md 3.13): New Session, Find, the Go menu, Next Needing
    /// You and Open in New Window.
    private func registerCommands() {
        guard let commands, let fleet else { return }
        commands.goTargets = [.sessions, .capcom]
        commands.go = { t in
            switch t {
            case .sessions:
                opened = nil
                capcomShown = false
            case .capcom: capcomShown = true
            default: break
            }
        }
        commands.newSession = { newSession = fleet.newSessions.draft() }
        commands.openRow = { open($0) }
        commands.openWindow = multipleWindows ? { openWindow(value: $0) } : nil
    }

    public var body: some View {
        layout
        .sheet(item: fleet == nil ? $opened : .constant(nil)) { row in
            SessionPlaceholder(row: row) { opened = nil }
        }
        .modifier(ApprovalSheetHost(center: fleet?.approvals, openChat: openChat))
        .modifier(SendFileSheetHost(center: fleet?.files, board: board))
        .modifier(CapcomHost(center: fleet?.capcom, board: board, opened: $opened))
        .modifier(BoardNewSession(fleet: fleet, draft: $newSession, openedID: opened?.id) { row in
            open(row)
        })
        // One detail at a time: a session or the Capcom view.
        .onChange(of: opened) { _, r in
            guard r != nil else { return }
            capcomShown = false
            fleet?.rounds.sessionOpened()
            fleet?.newSessions.opened()
        }
        .onChange(of: capcomShown) { _, on in if on { opened = nil } }
        .modifier(BoardSessionActions(fleet: fleet, opened: $opened, capcomShown: $capcomShown, saved: $savedOpen, window: window))
        // Links and Handoff open here, in the window that received them (app.md 3.15).
        .modifier(BoardLinks(fleet: fleet, open: open))
        // What this window offers the menus (app.md 3.13).
        .onChange(of: opened?.id, initial: true) { _, id in commands?.openRowID = id }
        .onAppear(perform: registerCommands)
        .onChange(of: newSession != nil) { _, up in commands?.setSheet("newSession", up) }
        // A round's run session: Run now, a run in the history, or a round notification (app.md 21).
        .onChange(of: fleet?.rounds.openRequest) { _, t in
            guard let t else { return }
            fleet?.rounds.openRequest = nil
            if let row = board.rows.first(where: { $0.target.habID == t.habID && $0.target.session == t.session }) {
                open(row)
            }
        }
        // A group member's session, from its card (groups.md 5.4).
        .onChange(of: fleet?.groups.openRequest) { _, t in
            guard let t else { return }
            fleet?.groups.openRequest = nil
            if let row = board.rows.first(where: { $0.target.habID == t.habID && $0.target.session == t.session }) {
                open(row)
            }
        }
        // A file sent to a session from the send sheet opens that session (app.md 4.4).
        .onChange(of: fleet?.files.openRequest) { _, t in
            guard let t else { return }
            fleet?.files.openRequest = nil
            if let row = board.rows.first(where: { $0.target == t }) { opened = row }
        }
        .usageSheet(fleet?.usage, governor: fleet?.capcom)
        // A usage notification or widget tap while the Connections sheet is
        // up: close it first, since a second sheet cannot present over it.
        .onChange(of: fleet?.usage?.book.presented) { _, p in
            if let p, showConnections { showUsage(p.account) }
        }
        #if DEBUG
        // Debug builds only: `-FlightdeckOpenCapcom YES` opens the Capcom view at launch.
        .task {
            if fleet != nil, UserDefaults.standard.bool(forKey: "FlightdeckOpenCapcom") { capcomShown = true }
        }
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
        // Debug builds only: `-FlightdeckOpenCapcomJobs YES` opens the Capcom jobs list at launch.
        .task {
            if fleet != nil, UserDefaults.standard.bool(forKey: "FlightdeckOpenCapcomJobs") { capcomJobsShown = true }
        }
        .sheet(isPresented: $capcomJobsShown) {
            if let fleet {
                NavigationStack {
                    CapcomJobsPage(center: fleet.rounds)
                        .navigationTitle("Capcom jobs")
                        .toolbar { Button("Done") { capcomJobsShown = false } }
                }
            }
        }
        #endif
        #if os(macOS)
        .frame(minWidth: fleet != nil && (opened != nil || capcomShown) ? 960 : 680, minHeight: 420)
        #endif
    }
}

/// The row actions and the reopen of the last session (app.md 3.11, 3.12), with a fleet.
struct BoardSessionActions: ViewModifier {
    let fleet: Fleet?
    @Binding var opened: BoardRow?
    @Binding var capcomShown: Bool
    /// The window's saved open session, or "capcom".
    @Binding var saved: String
    /// The window, whose own opens cancel its restore (app.md 3.12).
    var window: UUID?

    func body(content: Content) -> some View {
        if let fleet {
            content
                .modifier(SessionActionsHost(center: fleet.sessionActions))
                .modifier(SessionActionFollow(center: fleet.sessionActions, board: fleet.board, opened: $opened))
                // The Capcom view, an approval sheet or a tap on any row cancels the restore (app.md 3.12).
                .modifier(RestoreOpenSession(board: fleet.board, opened: $opened, saved: saved, window: window,
                                             interrupt: "\(capcomShown)|\(fleet.approvals?.interrupts(window: window) ?? false)|\(fleet.board.opens(in: window))"))
                .onChange(of: capcomShown) { _, on in saved = on ? SessionRestore.capcom : (opened?.id ?? "") }
                .onChange(of: opened) { _, r in saved = r?.id ?? (capcomShown ? SessionRestore.capcom : "") }
                // The Mac keeps the Capcom view the same way (app.md 3.12).
                .task { if saved == SessionRestore.capcom { capcomShown = true } }
        } else {
            content
        }
    }
}

/// The search's host scopes (app.md 3.6): All, then each hab, with more than one hab.
struct HostScopes: ViewModifier {
    let board: Board
    @Binding var scope: BoardSearchScope

    /// iPhone and iPad: while the search is open. Mac: always, under the toolbar's field.
    private static var activation: SearchScopeActivation {
        #if os(iOS)
        return .onSearchPresentation
        #else
        return .automatic
        #endif
    }

    func body(content: Content) -> some View {
        if board.habs.count > 1 {
            content.searchScopes($scope, activation: Self.activation) {
                Text("All").tag(BoardSearchScope.all)
                ForEach(board.habs) { h in Text(h.label).tag(BoardSearchScope.hab(h.id)) }
            }
        } else {
            content
        }
    }
}

/// The board's New session control and form (app.md 3.10), with a fleet: a plus in the
/// window's toolbar on the Mac, and the started session opened once the board lists it.
struct BoardNewSession: ViewModifier {
    let fleet: Fleet?
    @Binding var draft: NewSessionDraft?
    /// The open row, for Next Needing You (app.md 3.16).
    var openedID: String?
    let open: (BoardRow) -> Void

    func body(content: Content) -> some View {
        if let fleet {
            content
                #if os(macOS)
                // Customisable (app.md 3.13): View › Customize Toolbar… adds, removes and moves them.
                .toolbar(id: "board") {
                    ToolbarItem(id: "next-needing-you", placement: .primaryAction) {
                        NextNeedingYouButton(board: fleet.board, openedID: openedID, open: open)
                    }
                    ToolbarItem(id: "new-session", placement: .primaryAction) {
                        NewSessionButton(center: fleet.newSessions, draft: $draft)
                    }
                }
                #endif
                .modifier(NewSessionHost(center: fleet.newSessions, board: fleet.board, draft: $draft, open: open))
        } else {
            content
        }
    }
}

/// iPad and Mac with a session open: the board list on the left, the session
/// on the right (app.md 4.6). The list gets a fixed width from the window: as
/// wide as it may be while the session keeps its minimum. A plain HStack of
/// flexible frames gave the list its maximum first, so at the 960 pt minimum
/// window the row came out wider than the window, centered, and clipped the
/// list's left edge.
struct BoardSessionSplit<ListContent: View, SessionContent: View>: View {
    static var listMin: CGFloat { 300 }
    static var listMax: CGFloat { 380 }
    static var sessionMin: CGFloat { 640 }

    /// The list's width in a window `width` wide; the divider takes 1 pt.
    static func listWidth(in width: CGFloat) -> CGFloat {
        min(listMax, max(listMin, width - 1 - sessionMin))
    }

    @ViewBuilder var list: ListContent
    @ViewBuilder var session: SessionContent

    var body: some View {
        GeometryReader { geo in
            HStack(spacing: 0) {
                list
                    .frame(width: Self.listWidth(in: geo.size.width))
                Divider()
                session
                    .frame(maxWidth: .infinity)
            }
        }
    }
}

struct BoardList: View {
    let board: Board
    @Binding var opened: BoardRow?
    /// Close, rename and restart from a row (app.md 3.11); nil offers none.
    var actions: SessionActionCenter? = nil
    /// The board's search and host scope (app.md 3.6), held by the screen.
    @Binding var query: String
    @Binding var scope: BoardSearchScope
    /// Find Sessions' count (app.md 3.13): a change puts the cursor in the search field.
    var finds = 0
    /// Returns true when the row opened the approval sheet instead.
    var openRequest: (BoardRow) -> Bool = { _ in false }
    /// Opens the Capcom view from its row; nil shows no row.
    var openCapcom: (() -> Void)? = nil
    /// A heading above the list, for the iPad and Mac columns.
    var heading: String? = nil
    @Environment(\.confirmations) private var confirms
    @Environment(\.capcom) private var capcom
    @Environment(\.rounds) private var rounds
    @Environment(\.groups) private var groups
    @Environment(\.windowCommands) private var commands

    @State private var searchShown = false

    private var searching: Bool { BoardSearch.isActive(query: query, scope: scope) }
    /// Words typed: the Capcom row, groups and rounds step aside. A host scope alone hides
    /// none of them, so nothing that waits for the owner goes unseen.
    private var typed: Bool { !BoardSearch.words(query).isEmpty }

    /// The confirmations the search keeps: by scope, and by title while words are typed.
    private func confirmCards(_ all: [ConfirmCard]) -> [ConfirmCard] {
        let s = BoardSearch.fit(scope, habs: board.habs.map(\.id))
        let w = BoardSearch.words(query)
        return all.filter { BoardSearch.matches(.confirm($0), words: w, scope: s) }
    }

    /// The board's sections with run sessions left out when the filter hides them. A run
    /// session that needs the owner always shows (app.md 21.2). The search filters the rest.
    private var sections: [BoardSection] {
        let all = RoundCenter.filter(board.sections, showRunSessions: rounds?.showRunSessions ?? true)
        return BoardSearch.filter(all, query: query, scope: BoardSearch.fit(scope, habs: board.habs.map(\.id)))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if let heading {
                Text(heading)
                    .font(.headline)
                    .accessibilityAddTraits(.isHeader)
                    .padding(.horizontal, 16)
                    .padding(.top, 10)
                    .padding(.bottom, 4)
            }
            list
        }
    }

    /// The Capcom row shows with a fleet and at least one hab.
    private var showsCapcom: Bool { capcom != nil && openCapcom != nil && !board.habs.isEmpty }

    private var list: some View {
        TimelineView(.periodic(from: .now, by: 30)) { ctx in
            List {
                ForEach(board.habs.filter(\.needsUpdate)) { h in
                    Label("\(h.label): \(h.updateText)", systemImage: "exclamationmark.triangle")
                        .foregroundStyle(.orange)
                }
                // The standing Capcom conversation (app.md 4.10), above every section.
                if showsCapcom, !typed, let capcom, let openCapcom {
                    Button(action: openCapcom) {
                        CapcomEntryRow(center: capcom, rows: board.rows, now: ctx.date)
                    }
                    .buttonStyle(.plain)
                    .pointerHover()
                    if board.sections.isEmpty, confirms?.approvals.isEmpty ?? true {
                        Text("Sessions on your habs show here.")
                            .foregroundStyle(.secondary)
                    }
                }
                // The approvals area's confirmations (capcom.md 6.1): starts other agents ask for.
                if let confirms, case let cards = confirmCards(confirms.approvals), !cards.isEmpty {
                    Section("Confirm") {
                        ForEach(cards) { ConfirmCardView(center: confirms, card: $0) }
                    }
                }
                ForEach(sections) { section in
                    Section(section.kind.rawValue) {
                        ForEach(section.rows) { row in
                            Button {
                                if openRequest(row) { return }
                                board.open(row, window: commands?.id)
                                opened = row
                            } label: {
                                BoardRowView(row: row, now: ctx.date)
                            }
                            .buttonStyle(.plain)
                            .pointerHover()
                            .sessionRowActions(row, actions)
                        }
                    }
                }
                // The group cards (groups.md 5.4), below the sessions.
                if let groups, !typed {
                    BoardGroupsSection(center: groups, now: ctx.date)
                }
                // The Rounds list per hab (rounds.md 10.3), below the sessions.
                if let rounds, rounds.any, !typed {
                    BoardRoundsSections(center: rounds, now: ctx.date)
                }
            }
            .navigationDestination(for: RoundRow.self) { row in
                if let rounds { RoundPage(center: rounds, habID: row.habID, roundID: row.round.id) }
            }
            .searchable(text: $query, isPresented: $searchShown, prompt: "Search sessions")
            .onChange(of: finds) { _, _ in searchShown = true }
            .modifier(HostScopes(board: board, scope: $scope))
            #if os(iOS)
            // The scopes show only while the search is open; closing it drops the scope too.
            .onChange(of: searchShown) { _, on in if !on { scope = .all } }
            #endif
            .overlay {
                if searching, sections.isEmpty, confirmCards(confirms?.approvals ?? []).isEmpty {
                    ContentUnavailableView("No matches", systemImage: "magnifyingglass",
                                           description: Text("Try other words, or check the host scope."))
                } else if board.sections.isEmpty, confirms?.approvals.isEmpty ?? true, !showsCapcom, !(rounds?.any ?? false),
                   groups?.boardCards.isEmpty ?? true {
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
            StateDot(row, size: 9)
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
                    if let r = row.round { RoundBadge(name: r) }
                    if let g = row.group { GroupBadge(title: g) }
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
                    #if os(macOS)
                    // The Mac's Settings window (app.md 3.8).
                    SettingsLink { Text("Settings") }
                    #else
                    Button("Settings") { showSettings = true }
                    #endif
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
                                .keyboardShortcut(.cancelAction)
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
                                    .keyboardShortcut(.cancelAction)
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
                                    .keyboardShortcut(.cancelAction)
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
                                    .keyboardShortcut(.cancelAction)
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
    /// Amber means "needs you" only, green means done (app.md 3.8).
    var stateColor: Color {
        switch state {
        case .blocked?, .error?: return .orange
        case .done?: return rank == .doneUnseen ? .orange : .green
        case .working?: return .teal
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
        guard let since else { return stateWord }
        return stateWord + " · " + Self.age(since, now: now)
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
    /// A sheet raised from a row shows in that row's window only (app.md 3.12).
    @Environment(\.windowCommands) private var commands

    func body(content: Content) -> some View {
        if let center {
            let mine = center.presenter == nil || center.presenter == commands?.id
            content
                // Closing the window that raised the sheet ends it (app.md 3.12).
                .onDisappear { center.windowClosed(commands?.id) }
                .sheet(isPresented: Binding(get: { center.presenting && mine }, set: { center.presenting = $0 })) {
                    ApprovalSheet(model: center.sheet, openChat: openChat) { center.presenting = false }
                }
        } else {
            content
        }
    }
}
