#if os(iOS)
import FlightdeckShared
import HabAPI
import SwiftUI
import UIKit

/// The tabs of the app shell (app.md 3.8). The scene keeps the open one (app.md 3.12).
enum ShellTab: String, Hashable {
    case sessions, capcom, rounds, settings
}

/// The iPhone and iPad root (app.md 3.8, 3.9): first run while no hab is in the app, else four
/// tabs with the usage strip as the tab bar's bottom accessory.
struct AppShell: View {
    let fleet: Fleet
    /// The open tab and session, kept per window (app.md 3.12).
    @SceneStorage("flightdeck.shell.tab") private var tab: ShellTab = .sessions
    @SceneStorage("flightdeck.shell.open") private var savedOpen = ""
    @State private var opened: BoardRow?
    /// The restore is opening the saved session: the tab stays as the scene kept it.
    @State private var restoring = false
    /// The New session form, while it is open (app.md 3.10).
    @State private var newSession: NewSessionDraft?
    /// Debug builds: the Capcom jobs list opened at launch.
    @State private var capcomJobsShown = false
    @Environment(\.horizontalSizeClass) private var sizeClass

    private var board: Board { fleet.board }

    var body: some View {
        content
            .modifier(ApprovalSheetHost(center: fleet.approvals, openChat: openChat))
            .modifier(SendFileSheetHost(center: fleet.files, board: board))
            .modifier(CapcomHost(center: fleet.capcom, board: board, opened: $opened))
            // A started session opens once the board lists it (app.md 3.10).
            .modifier(NewSessionHost(center: fleet.newSessions, board: board, draft: $newSession) { row in
                board.open(row)
                opened = row
            })
            .usageSheet(fleet.usage, governor: fleet.capcom)
            // Close, rename and restart from a row (app.md 3.11).
            .modifier(SessionActionsHost(center: fleet.sessionActions))
            .modifier(SessionActionFollow(center: fleet.sessionActions, board: board, opened: $opened))
            .modifier(RestoreOpenSession(board: board, opened: $opened, saved: savedOpen) { restoring = true })
            .onChange(of: opened) { _, r in
                savedOpen = r?.id ?? ""
                guard r != nil else { return }
                if restoring {
                    restoring = false
                } else {
                    tab = .sessions
                }
                fleet.rounds.sessionOpened()
                fleet.newSessions.opened()
            }
            // A round's run session: Run now, a run in the history, or a round notification (app.md 21).
            .onChange(of: fleet.rounds.openRequest) { _, t in
                guard let t else { return }
                fleet.rounds.openRequest = nil
                open(habID: t.habID, session: t.session)
            }
            // A group member's session, from its card (groups.md 5.4).
            .onChange(of: fleet.groups.openRequest) { _, t in
                guard let t else { return }
                fleet.groups.openRequest = nil
                open(habID: t.habID, session: t.session)
            }
            // A file sent to a session from the send sheet opens that session (app.md 4.4).
            .onChange(of: fleet.files.openRequest) { _, t in
                guard let t else { return }
                fleet.files.openRequest = nil
                if let row = board.rows.first(where: { $0.target == t }) { opened = row }
            }
            #if DEBUG
            .modifier(ShellLaunchArguments(fleet: fleet, tab: $tab, opened: $opened, capcomJobsShown: $capcomJobsShown, newSession: $newSession))
            #endif
    }

    @ViewBuilder
    private var content: some View {
        if FirstRun.shows(habs: board.habs.count, demo: fleet.isDemo) {
            FirstRunView(fleet: fleet)
        } else {
            tabs
        }
    }

    private func open(habID: String, session: String?) {
        if let row = board.rows.first(where: { $0.target.habID == habID && $0.target.session == session }) {
            board.open(row)
            opened = row
        }
    }

    /// The sheet's chat button: open the session at the request.
    private func openChat(_ t: SessionTarget) {
        fleet.approvals?.presenting = false
        if let row = board.rows.first(where: { $0.target == t }) { opened = row }
    }

    private var sessions: some View {
        SessionsTab(fleet: fleet, opened: $opened, newSession: $newSession) { tab = .settings }
    }

    private var tabs: some View {
        TabView(selection: $tab) {
            Tab("Sessions", systemImage: "rectangle.stack", value: ShellTab.sessions) {
                sessions
            }
            .badge(board.needsYouCount)
            Tab("Capcom", systemImage: "dot.radiowaves.left.and.right", value: ShellTab.capcom) {
                CapcomTab(fleet: fleet)
            }
            Tab("Rounds", systemImage: "clock.arrow.circlepath", value: ShellTab.rounds) {
                RoundsTab(fleet: fleet)
            }
            Tab("Settings", systemImage: "slider.horizontal.3", value: ShellTab.settings) {
                SettingsTab(fleet: fleet)
            }
        }
        .tabViewStyle(.sidebarAdaptable)
        .tabBarMinimizeBehavior(.onScrollDown)
        .tabViewBottomAccessory(isEnabled: !(fleet.usage?.book.accounts.isEmpty ?? true)) {
            if let usage = fleet.usage { UsageAccessory(book: usage.book) }
        }
    }
}

#if DEBUG
/// Debug builds only: launch arguments that open a screen with no taps, so a build can be
/// checked and screenshotted headlessly. `-FlightdeckTab capcom|rounds|settings`,
/// `-FlightdeckOpenCapcom YES`, `-FlightdeckOpenSession <session>`, `-FlightdeckOpenCapcomJobs YES`,
/// `-FlightdeckShow review|group|hab`, `-FlightdeckNewSession YES` (the New session form).
struct ShellLaunchArguments: ViewModifier {
    let fleet: Fleet
    @Binding var tab: ShellTab
    @Binding var opened: BoardRow?
    @Binding var capcomJobsShown: Bool
    @Binding var newSession: NewSessionDraft?
    /// `-FlightdeckShow review|group|hab` opens that screen in a sheet.
    @State private var shown: String?

    struct DebugSheet: Identifiable {
        let id: String
    }

    func body(content: Content) -> some View {
        content
            .task {
                let d = UserDefaults.standard
                if let t = d.string(forKey: "FlightdeckTab").flatMap(ShellTab.init(rawValue:)) { tab = t }
                if d.bool(forKey: "FlightdeckOpenCapcom") { tab = .capcom }
                if d.bool(forKey: "FlightdeckOpenCapcomJobs") { capcomJobsShown = true }
            }
            .task {
                guard let id = UserDefaults.standard.string(forKey: "FlightdeckOpenSession"), !id.isEmpty else { return }
                // Up to 30 s: a loaded machine starts the demo habs slowly.
                for _ in 0..<300 {
                    if let row = fleet.board.rows.first(where: { $0.target.session == id }) {
                        opened = row
                        return
                    }
                    try? await Task.sleep(for: .milliseconds(100))
                }
            }
            .task {
                guard UserDefaults.standard.bool(forKey: "FlightdeckNewSession") else { return }
                // Up to 30 s for a hab to come online.
                for _ in 0..<300 {
                    if let d = fleet.newSessions.draft() {
                        newSession = d
                        return
                    }
                    try? await Task.sleep(for: .milliseconds(100))
                }
            }
            .task {
                guard let what = UserDefaults.standard.string(forKey: "FlightdeckShow") else { return }
                try? await Task.sleep(for: .seconds(3))
                shown = what
            }
            .sheet(item: Binding(get: { shown.map(DebugSheet.init) }, set: { shown = $0?.id })) { s in
                switch s.id {
                case "review":
                    if let c = fleet.confirms, let card = c.approvals.first {
                        ConfirmReviewSheet(center: c, cardID: card.id) { shown = nil }
                    }
                case "group":
                    if let card = fleet.groups.boardCards.first {
                        NavigationStack { GroupPage(center: fleet.groups, id: card.id) }
                    }
                default:
                    NavigationStack { HabDetailPage(fleet: fleet, habID: fleet.board.habs.first?.id ?? "") }
                }
            }
            .sheet(isPresented: $capcomJobsShown) {
                NavigationStack {
                    CapcomJobsPage(center: fleet.rounds)
                        .navigationTitle("Capcom jobs")
                        .toolbar { Button("Done") { capcomJobsShown = false } }
                }
            }
    }
}
#endif

// MARK: - Sessions

/// The Sessions tab (app.md 3.8): the list, the hosts menu, and the opened session. A compact
/// width pushes the session; a regular one shows it in a split view's detail column, with the
/// open row selected in the list.
struct SessionsTab: View {
    let fleet: Fleet
    @Binding var opened: BoardRow?
    @Binding var newSession: NewSessionDraft?
    let openSettings: () -> Void
    @Environment(\.horizontalSizeClass) private var sizeClass
    /// The board's search and host scope (app.md 3.6).
    @State private var query = ""
    @State private var scope: BoardSearchScope = .all

    var body: some View {
        if sizeClass == .compact {
            NavigationStack {
                list
                    .navigationDestination(item: $opened) { row in
                        session(row, compact: true)
                            .toolbar(.hidden, for: .tabBar)
                    }
            }
        } else {
            NavigationSplitView {
                list(selectable: true)
                    .navigationSplitViewColumnWidth(min: 300, ideal: 360, max: 420)
            } detail: {
                NavigationStack {
                    if let row = opened {
                        session(row, compact: false)
                            .navigationBarTitleDisplayMode(.inline)
                    } else {
                        ContentUnavailableView("Choose a session", systemImage: "rectangle.stack",
                                               description: Text("Pick a row to open its chat and terminal."))
                    }
                }
            }
        }
    }

    private var list: some View { list(selectable: false) }

    private func list(selectable: Bool) -> some View {
        SessionsListView(fleet: fleet, opened: $opened, selectable: selectable, newSession: $newSession, query: query, scope: scope)
            .navigationTitle("Sessions")
            .navigationBarTitleDisplayMode(.large)
            .searchable(text: $query, prompt: "Search sessions")
            .modifier(HostScopes(board: fleet.board, scope: $scope))
            // iPhone: the field waits in the bottom toolbar as a button until tapped.
            .searchToolbarBehavior(sizeClass == .compact ? .minimize : .automatic)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    HostsMenu(board: fleet.board, openSettings: openSettings)
                }
                ToolbarItem(placement: .primaryAction) {
                    NewSessionButton(center: fleet.newSessions, draft: $newSession)
                }
            }
            .navigationDestination(for: GroupSummary.self) { g in
                GroupPage(center: fleet.groups, id: g.id)
            }
    }

    private func session(_ row: BoardRow, compact: Bool) -> some View {
        SessionView(model: fleet.chat(for: row.target), compact: compact, close: { opened = nil })
            .id(row.target)
            // session.focus while the session is on screen (app.md 8.6).
            .onAppear {
                fleet.focus.show(row.target)
                Task { await fleet.activities?.opened(row.target) }
            }
            .onDisappear { fleet.focus.hide(row.target) }
    }
}

/// The hosts control (app.md 3.8): a dot and "2 hosts", and a menu of each hab's state.
struct HostsMenu: View {
    let board: Board
    let openSettings: () -> Void

    private var allOnline: Bool { board.onlineCount == board.habs.count }

    private var title: String {
        let n = board.habs.count
        if allOnline { return n == 1 ? "1 host" : "\(n) hosts" }
        return "\(board.onlineCount) of \(n) online"
    }

    var body: some View {
        Menu {
            Section("Computers") {
                ForEach(board.habs) { h in
                    Label("\(h.label) · \(h.statusText)",
                          systemImage: h.state.status == .online ? "checkmark.circle" : "circle.dashed")
                }
            }
            Button("Computers and pairing", systemImage: "desktopcomputer", action: openSettings)
        } label: {
            HStack(spacing: 6) {
                Circle()
                    .fill(allOnline ? Color.green : Color.secondary)
                    .frame(width: 8, height: 8)
                Text(title)
                    .lineLimit(1)
            }
        }
        .accessibilityLabel("Hosts: \(board.onlineCount) of \(board.habs.count) online")
    }
}

/// The Sessions list (app.md 3.1, 3.8): Needs you, Working, Done and Other, with an action per
/// kind of row.
struct SessionsListView: View {
    let fleet: Fleet
    @Binding var opened: BoardRow?
    /// iPad: the open session's row shows selected, and a tap on a row selects it.
    var selectable = false
    /// The New session form, for the empty list's action.
    @Binding var newSession: NewSessionDraft?
    /// The search's words and host scope (app.md 3.6).
    var query = ""
    var scope: BoardSearchScope = .all

    private var board: Board { fleet.board }

    /// The list as the search filters it.
    private func filtered(_ all: [SessionsSection]) -> [SessionsSection] {
        BoardSearch.filter(all, query: query, scope: BoardSearch.fit(scope, habs: board.habs.map(\.id)))
    }

    private var sections: [SessionsSection] {
        var rows = RoundCenter.filter(board.sections, showRunSessions: fleet.rounds.showRunSessions)
        // The standing Capcom conversation has its own tab; it shows here only when it needs you.
        if let capcom = fleet.capcom.standingRow(in: board.rows) {
            rows = SessionsList.without(capcom, in: rows)
        }
        let groups = fleet.groups.boardCards.map { card in
            GroupSummary.make(card) { fleet.groups.target(card, $0) }
        }
        return SessionsList.build(sections: rows, confirms: fleet.confirms?.approvals ?? [], groups: groups)
    }

    var body: some View {
        TimelineView(.periodic(from: .now, by: 30)) { ctx in
            let all = sections
            let sections = filtered(all)
            ScrollViewReader { proxy in
            List(selection: selectable ? selection : nil) {
                ForEach(board.habs.filter(\.needsUpdate)) { h in
                    Label("\(h.label): \(h.updateText)", systemImage: "exclamationmark.triangle")
                }
                ForEach(sections) { section in
                    Section {
                        ForEach(section.items) { item($0, now: ctx.date) }
                    } header: {
                        Text(section.title)
                            .foregroundStyle(section.kind == .needsYou ? Color.orange : Color.secondary)
                    }
                    .id(section.kind)
                }
            }
            // Pull to refresh retries every hab at once (app.md 10.2).
            .refreshable { await fleet.refresh() }
            #if DEBUG
            // Debug builds only: `-FlightdeckScrollTo Working` scrolls to a section, for screenshots.
            .task {
                guard let k = UserDefaults.standard.string(forKey: "FlightdeckScrollTo").flatMap(BoardSectionKind.init(rawValue:)) else { return }
                try? await Task.sleep(for: .seconds(2))
                proxy.scrollTo(k, anchor: .top)
            }
            #endif
            }
            .overlay {
                if sections.isEmpty, !all.isEmpty {
                    ContentUnavailableView("No matches", systemImage: "magnifyingglass",
                                           description: Text("Try other words, or check the host scope."))
                } else if sections.isEmpty {
                    ContentUnavailableView {
                        Label("No sessions", systemImage: "rectangle.stack")
                    } description: {
                        Text("Sessions on your computers show here.")
                    } actions: {
                        Button("New session") { newSession = fleet.newSessions.draft() }
                            .disabled(fleet.newSessions.habs.isEmpty)
                    }
                }
            }
        }
    }

    private func open(_ row: BoardRow) {
        board.open(row)
        opened = row
    }

    /// The selected row is the open session; selecting a row opens it.
    private var selection: Binding<String?> {
        Binding(get: { opened?.id }, set: { id in
            guard let id, let row = board.rows.first(where: { $0.id == id }) else { return }
            open(row)
        })
    }

    @ViewBuilder
    private func item(_ item: SessionItem, now: Date) -> some View {
        switch item {
        case .permission(let row):
            PermissionRow(row: row, approvals: fleet.approvals, now: now) { open(row) }
                .modifier(OpenRowMark(id: row.id, open: selectable && opened?.id == row.id))
                .sessionRowActions(row, fleet.sessionActions)
        case .question(let row):
            ActionRow(row: row, now: now, action: "Answer", open: { open(row) }, act: { open(row) })
                .modifier(OpenRowMark(id: row.id, open: selectable && opened?.id == row.id))
                .sessionRowActions(row, fleet.sessionActions)
        case .confirm(let card):
            if let center = fleet.confirms { ConfirmRow(center: center, card: card, now: now) }
        case .group(let g):
            NavigationLink(value: g) { GroupRow(group: g, now: now) }
        case .session(let row):
            if selectable {
                SessionRow(row: row, now: now).tag(row.id)
                    .sessionRowActions(row, fleet.sessionActions)
            } else {
                Button { open(row) } label: { SessionRow(row: row, now: now) }
                    .buttonStyle(.plain)
                    .sessionRowActions(row, fleet.sessionActions)
            }
        }
    }
}

/// The open session's mark on a row with its own buttons (iPad): a light accent wash, not the
/// list's selection, which would sit the buttons on the accent colour. The row's own tag keeps
/// it out of the selection: the ForEach would tag it with the session id, which the list
/// then draws selected (white text on the light wash).
struct OpenRowMark: ViewModifier {
    let id: String
    let open: Bool

    func body(content: Content) -> some View {
        content
            .tag("mark/" + id)
            .listRowBackground(open ? Color.accentColor.opacity(0.18) : nil)
    }
}

/// A row's mark: amber for needs you, teal for working, a green check for done (app.md 3.8).
struct StateMark: View {
    let row: BoardRow

    var body: some View {
        Group {
            switch row.state {
            case .error?:
                Image(systemName: "exclamationmark.circle.fill").foregroundStyle(.orange)
            case .done? where row.rank != .doneUnseen:
                Image(systemName: "checkmark").foregroundStyle(.green).fontWeight(.bold)
            case .done?:
                Image(systemName: "checkmark.circle.fill").foregroundStyle(.orange)
            default:
                Circle().fill(row.stateColor).frame(width: 10, height: 10)
            }
        }
        .font(.subheadline)
        .frame(width: 16)
        .accessibilityLabel(row.stateWord)
    }
}

/// Text beside a trailing part while it fits; at accessibility sizes the trailing part goes
/// under the text (app.md 3.8).
struct AdaptiveRow<Leading: View, Trailing: View>: View {
    @ViewBuilder var leading: Leading
    @ViewBuilder var trailing: Trailing
    @Environment(\.dynamicTypeSize) private var type

    var body: some View {
        if type.isAccessibilitySize {
            VStack(alignment: .leading, spacing: 8) {
                leading
                trailing
            }
        } else {
            HStack(alignment: .center, spacing: 12) {
                leading
                    .frame(maxWidth: .infinity, alignment: .leading)
                trailing
            }
        }
    }
}

extension BoardRow {
    /// The row's second line: the state in words where it matters, then project and hab.
    func subtitle(showState: Bool) -> String {
        var parts: [String] = []
        if showState { parts.append(stateWord) }
        if let p = project { parts.append(p) }
        parts.append(host)
        return parts.joined(separator: " · ")
    }

    var showsStateInSubtitle: Bool { rank == .blocked || rank == .error || rank == .other }
}

/// A plain session row: mark, title, project and hab, and the time in state.
struct SessionRow: View {
    let row: BoardRow
    let now: Date

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
            StateMark(row: row)
            AdaptiveRow {
                VStack(alignment: .leading, spacing: 2) {
                    Text(row.title)
                        .font(.body.weight(.semibold))
                        .lineLimit(4)
                        .minimumScaleFactor(0.7)
                    Text(row.subtitle(showState: row.showsStateInSubtitle))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                    if let r = row.round { RoundBadge(name: r) }
                    if row.offline {
                        Text(row.offlineSince.map { "Offline since \($0.formatted(date: .omitted, time: .shortened))" } ?? "Offline")
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                    }
                }
            } trailing: {
                if let since = row.since {
                    Text(BoardRow.age(since, now: now))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .contentShape(Rectangle())
        .opacity(row.offline ? 0.5 : 1)
        .accessibilityElement(children: .combine)
    }
}

/// A needs-you row with one action button: Answer for a question (app.md 3.1).
struct ActionRow: View {
    let row: BoardRow
    let now: Date
    let action: String
    let open: () -> Void
    let act: () -> Void

    var body: some View {
        AdaptiveRow {
            Button(action: open) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(row.title)
                        .font(.body.weight(.semibold))
                        .lineLimit(4)
                        .minimumScaleFactor(0.7)
                    Text(row.subtitle(showState: true) + (row.since.map { " · " + BoardRow.age($0, now: now) } ?? ""))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityElement(children: .combine)
        } trailing: {
            Button(action, action: act)
                .buttonStyle(.bordered)
                .buttonBorderShape(.capsule)
                .fixedSize()
                .disabled(row.offline)
                .accessibilityHint("Opens the chat")
        }
        .opacity(row.offline ? 0.5 : 1)
    }
}

/// A permission row (app.md 3.1): the title, the command in a box, the state line, and Deny and
/// Allow, which answer through the approval sheet's path.
struct PermissionRow: View {
    let row: BoardRow
    let approvals: ApprovalCenter?
    let now: Date
    let open: () -> Void

    var body: some View {
        let card = approvals?.card(for: row.target)
        VStack(alignment: .leading, spacing: 10) {
            Button(action: open) {
                VStack(alignment: .leading, spacing: 6) {
                    AdaptiveRow {
                        Text(row.title)
                            .font(.body.weight(.semibold))
                            .lineLimit(4)
                        .minimumScaleFactor(0.7)
                    } trailing: {
                        if let since = row.since {
                            Text(BoardRow.age(since, now: now))
                                .font(.subheadline)
                                .foregroundStyle(.secondary)
                        }
                    }
                    if let c = row.context {
                        Text(c)
                            .font(.callout.monospaced())
                            .lineLimit(4)
                        .minimumScaleFactor(0.7)
                            .padding(.horizontal, 10)
                            .padding(.vertical, 6)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .background(Color(uiColor: .tertiarySystemFill), in: RoundedRectangle(cornerRadius: 8))
                    }
                    Text(row.subtitle(showState: true))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityElement(children: .combine)
            if let card {
                if card.expired {
                    Text("This request expired. Answer it in the session.")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                } else {
                    AnswerButtons(sending: card.sending, enabled: card.canAnswer && !card.sending) {
                        Task { await approvals?.answer(row.target, allow: false) }
                    } allow: {
                        Task { await approvals?.answer(row.target, allow: true) }
                    }
                }
                if let f = card.failure {
                    Text(f)
                        .font(.subheadline)
                        .foregroundStyle(.red)
                }
            }
        }
        .padding(.vertical, 4)
        .opacity(row.offline ? 0.5 : 1)
    }
}

/// Deny and Allow side by side while both fit on one line each, else stacked (app.md 3.8).
struct AnswerButtons: View {
    let sending: Bool
    let enabled: Bool
    let deny: () -> Void
    let allow: () -> Void

    var body: some View {
        ViewThatFits(in: .horizontal) {
            HStack(spacing: 8) { denyButton; allowButton }
            VStack(spacing: 8) { allowButton; denyButton }
        }
        .controlSize(.large)
        .disabled(!enabled)
    }

    private var denyButton: some View {
        Button(action: deny) {
            Text("Deny").lineLimit(1).frame(maxWidth: .infinity)
        }
        .buttonStyle(.bordered)
        .tint(Color(uiColor: .label))
    }

    private var allowButton: some View {
        Button(action: allow) {
            Group {
                if sending { ProgressView() } else { Text("Allow").lineLimit(1) }
            }
            .frame(maxWidth: .infinity)
        }
        .buttonStyle(.borderedProminent)
    }
}

/// A confirmation card as one row (app.md 3.1): the title, who asked, the time left, and
/// Review, which opens the whole card in a sheet sized to it.
struct ConfirmRow: View {
    let center: ConfirmCenter
    let card: ConfirmCard
    let now: Date
    @State private var reviewing = false

    private var c: Confirmation { card.confirmation }

    private var byline: String {
        var parts: [String] = []
        if let f = c.from { parts.append("Asked by \(f)") }
        if let p = c.start?.project ?? c.group?.plan.project { parts.append(p) }
        if let left = center.remaining(card, at: now) {
            parts.append(left > 0 ? "expires in \(Self.minutes(left))" : "expired")
        }
        return parts.joined(separator: " · ")
    }

    /// "56 min", "1 h 5 min", "under a minute".
    static func minutes(_ t: TimeInterval) -> String {
        let m = Int(t / 60)
        if m < 1 { return "under a minute" }
        if m < 60 { return "\(m) min" }
        return m % 60 == 0 ? "\(m / 60) h" : "\(m / 60) h \(m % 60) min"
    }

    var body: some View {
        AdaptiveRow {
            VStack(alignment: .leading, spacing: 2) {
                Text(ConfirmCardContent.title(c))
                    .font(.body.weight(.semibold))
                    .lineLimit(4)
                        .minimumScaleFactor(0.7)
                Text(byline)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            .accessibilityElement(children: .combine)
        } trailing: {
            Button("Review") { reviewing = true }
                .buttonStyle(.bordered)
                .buttonBorderShape(.capsule)
                .fixedSize()
        }
        .sheet(isPresented: $reviewing) {
            ConfirmReviewSheet(center: center, cardID: card.id) { reviewing = false }
        }
    }
}

/// The whole confirmation card in a sheet sized to its content (app.md 3.1). It closes when
/// the card is answered.
struct ConfirmReviewSheet: View {
    let center: ConfirmCenter
    let cardID: String
    let close: () -> Void
    @State private var height: CGFloat = 420

    var body: some View {
        NavigationStack {
            ScrollView {
                if let card = center.cards.first(where: { $0.id == cardID }) {
                    ConfirmCardView(center: center, card: card)
                        .padding()
                        .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height = $0 }
                }
            }
            // The card's seed and fields take the keyboard; a drag on the card puts it away.
            .scrollDismissesKeyboard(.interactively)
            .navigationTitle("Review")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Close", action: close)
                        .keyboardShortcut(.cancelAction)
                }
            }
        }
        .presentationDetents([.height(min(height + 90, 720)), .large])
        .presentationDragIndicator(.visible)
        .onChange(of: center.approvals.contains { $0.id == cardID }) { _, open in
            if !open { close() }
        }
    }
}

/// A group as one Working row (app.md 3.1).
struct GroupRow: View {
    let group: GroupSummary
    let now: Date

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
            Group {
                if group.over {
                    Image(systemName: "checkmark").foregroundStyle(.green).fontWeight(.bold)
                } else {
                    Circle().fill(Color.teal).frame(width: 10, height: 10)
                }
            }
            .font(.subheadline)
            .frame(width: 16)
            .accessibilityHidden(true)
            AdaptiveRow {
                VStack(alignment: .leading, spacing: 2) {
                    Text(group.title)
                        .font(.body.weight(.semibold))
                        .lineLimit(4)
                        .minimumScaleFactor(0.7)
                    Text(group.line)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
            } trailing: {
                if let since = group.since {
                    Text(BoardRow.age(since, now: now))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .accessibilityElement(children: .combine)
    }
}

/// One group's page: its card (groups.md 5.4). A member row opens its session.
struct GroupPage: View {
    let center: GroupCenter
    let id: String

    var body: some View {
        TimelineView(.periodic(from: .now, by: 30)) { ctx in
            List {
                if let card = center.card(id) {
                    GroupCardView(center: center, card: card, now: ctx.date)
                } else {
                    Text("This group is gone.").foregroundStyle(.secondary)
                }
            }
        }
        .navigationTitle(center.card(id)?.group.title ?? "Group")
        .navigationBarTitleDisplayMode(.inline)
    }
}

/// The usage strip as the tab bar's bottom accessory (app.md 3.7): the two tightest accounts,
/// each with its name, a bar and the used percentage. Tap opens the usage sheet.
struct UsageAccessory: View {
    let book: UsageBook

    var body: some View {
        TimelineView(.periodic(from: .now, by: 30)) { ctx in
            let accounts = Array(UsageMath.byTightest(Array(book.accounts.values), now: ctx.date).prefix(2))
            Button {
                book.present(account: nil)
            } label: {
                HStack(spacing: 14) {
                    ForEach(accounts) { a in
                        UsageAccessoryItem(account: a, online: book.isOnline(a), now: ctx.date)
                    }
                }
                .padding(.horizontal, 16)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityHint("Opens usage")
        }
        // The accessory is one bar tall, as the tab bar is; its text stops growing where the
        // tab bar's does. VoiceOver reads the full words.
        .dynamicTypeSize(...DynamicTypeSize.xxLarge)
    }
}

struct UsageAccessoryItem: View {
    let account: UsageAccountReading
    let online: Bool
    let now: Date

    var body: some View {
        let w = account.unavailable ? nil : UsageMath.tightest(account, now: now)
        let stale = UsageMath.isStale(account, now: now) || !online
        HStack(spacing: 8) {
            Text(account.harnessName)
                .font(.footnote.weight(.semibold))
                .lineLimit(1)
                .fixedSize()
            if let w {
                UsageBar(window: w, now: now, stale: stale || UsageMath.isStale(w, now: now))
                    .frame(height: 6)
                    .frame(minWidth: 24)
                Text("\(UsageMath.effectivePct(w, now: now))%")
                    .font(.footnote.weight(.semibold).monospacedDigit())
                    .foregroundStyle(.secondary)
                    .fixedSize()
            } else {
                Text(account.unavailable ? UsageMath.unavailableText : "No limits")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
        }
        .opacity(stale ? 0.55 : 1)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(w.map { "\(account.harnessName): \(UsageMath.effectivePct($0, now: now)) percent of the \($0.name) window" } ?? account.harnessName)
    }
}

// MARK: - Capcom and Rounds

/// The Capcom tab: the Capcom view as a tab root (app.md 3.8, 4.10). iPhone and iPad share
/// it: a "Capcom" title in the system bar, the conversation full width.
struct CapcomTab: View {
    let fleet: Fleet

    var body: some View {
        NavigationStack {
            CapcomScreen(fleet: fleet, compact: true, close: nil, tab: true)
        }
    }
}

/// The Rounds tab (app.md 3.8, 21.2): every hab's rounds in one list, split by what starts
/// them, with New round and the run session switch in the bar.
struct RoundsTab: View {
    let fleet: Fleet
    @State private var creating: RoundDraft?
    /// iPad: the round shown in the detail column, by hab and round id.
    @State private var selected: RoundRow?
    @Environment(\.horizontalSizeClass) private var sizeClass

    private var center: RoundCenter { fleet.rounds }

    /// The Capcom home, whose `capcom.event_actions` the job rows read.
    private var home: String? {
        switch fleet.capcom.entry {
        case .ready(let id), .offline(let id): return id
        default: return nil
        }
    }

    /// A compact width pushes a round's page; a regular one shows it in a split view's detail
    /// column, with its row selected (app.md 21.2).
    var body: some View {
        if sizeClass == .compact {
            NavigationStack {
                list(selectable: false)
                    .navigationDestination(for: RoundRow.self) { row in
                        RoundPage(center: center, habID: row.habID, roundID: row.round.id)
                    }
            }
        } else {
            NavigationSplitView {
                list(selectable: true)
                    .navigationSplitViewColumnWidth(min: 320, ideal: 400, max: 480)
            } detail: {
                NavigationStack {
                    if let r = selected {
                        RoundPage(center: center, habID: r.habID, roundID: r.round.id)
                            .id(r.id)
                    } else {
                        ContentUnavailableView("Choose a round", systemImage: "clock.arrow.circlepath",
                                               description: Text("Pick a round to see its runs."))
                    }
                }
            }
        }
    }

    /// The selected row by id: a round's revision changes its value, not its id.
    private func selection(_ sections: [RoundsListSection]) -> Binding<String?> {
        Binding(get: { selected?.id }, set: { id in
            guard let id else { return }
            selected = sections.lazy.flatMap(\.rows).first { $0.row.id == id }?.row
        })
    }

    private func list(selectable: Bool) -> some View {
            TimelineView(.periodic(from: .now, by: 30)) { ctx in
                let sections = center.listSections(home: home, now: ctx.date)
                ScrollViewReader { proxy in
                List(selection: selectable ? selection(sections) : nil) {
                    ForEach(sections) { s in
                        Section {
                            ForEach(s.rows) { row in
                                if selectable {
                                    RoundsListRowView(row: row, center: center, onAccent: selected?.id == row.row.id)
                                        .tag(row.row.id)
                                } else {
                                    NavigationLink(value: row.row) {
                                        RoundsListRowView(row: row, center: center)
                                    }
                                }
                            }
                        } header: {
                            // The note beside the title while both fit on one line, else under it.
                            ViewThatFits(in: .horizontal) {
                                HStack(alignment: .firstTextBaseline) {
                                    Text(s.title).lineLimit(1)
                                    Spacer()
                                    if let n = s.note { Text(n).textCase(nil).lineLimit(1) }
                                }
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(s.title)
                                    if let n = s.note { Text(n).textCase(nil) }
                                }
                            }
                        }
                        .id(s.id)
                    }
                }
                .overlay {
                    if sections.isEmpty { empty }
                }
                #if DEBUG
                // Debug builds: `-FlightdeckRoundsSection events` scrolls to that section.
                .task {
                    guard let k = UserDefaults.standard.string(forKey: "FlightdeckRoundsSection") else { return }
                    try? await Task.sleep(for: .seconds(3))
                    proxy.scrollTo(k, anchor: .top)
                }
                #endif
                }
            }
            .navigationTitle("Rounds")
            .navigationBarTitleDisplayMode(.large)
            .toolbar {
                if center.any {
                    ToolbarItem(placement: .primaryAction) {
                        Button("New round", systemImage: "plus") { newRound() }
                            .disabled(!center.habs.contains { $0.online })
                    }
                    ToolbarItem(placement: .secondaryAction) {
                        Toggle("Show run sessions", isOn: Binding(get: { center.showRunSessions },
                                                                  set: { center.showRunSessions = $0 }))
                    }
                }
            }
            .sheet(item: $creating) { d in
                RoundFormSheet(center: center, draft: d) { creating = nil }
            }
    }

    @ViewBuilder
    private var empty: some View {
        if center.any {
            ContentUnavailableView {
                Label("No rounds yet", systemImage: "clock.arrow.circlepath")
            } description: {
                Text("A round runs an agent or a command on a schedule, or when something happens.")
            } actions: {
                Button("New round") { newRound() }
                    .disabled(!center.habs.contains { $0.online })
            }
        } else {
            ContentUnavailableView("No rounds", systemImage: "clock.arrow.circlepath",
                                   description: Text("Scheduled jobs on your computers show here."))
        }
    }

    private func newRound() {
        let habs = center.habs
        creating = RoundDraft(habID: habs.first { $0.online }?.id ?? habs.first?.id ?? "")
    }
}
/// One row of the Rounds tab (app.md 21.2): the name, the prompt's first line, and the
/// trigger, project and hab; on the right the last run, the next run, Unpause, or what a
/// Capcom job may do.
struct RoundsListRowView: View {
    let row: RoundsListRow
    let center: RoundCenter
    /// The selected row (iPad) sits on the accent: tints and grey text give way to white there.
    var onAccent = false

    var body: some View {
        AdaptiveRow {
            VStack(alignment: .leading, spacing: 2) {
                Text(row.title)
                    .font(.headline)
                    .foregroundStyle(row.paused || row.completed ? .secondary : .primary)
                if let s = row.summary, !row.paused {
                    Text(s)
                        .font(.subheadline)
                        .lineLimit(2)
                }
                Text(row.meta)
                    .font(.footnote)
                    .foregroundStyle(onAccent ? AnyShapeStyle(Color.white.opacity(0.85)) : AnyShapeStyle(.secondary))
                // Unpause's refusal, in the hab's words (21.3).
                if let e = center.errors[row.id] {
                    Text(e)
                        .font(.footnote)
                        .foregroundStyle(.red)
                }
            }
        } trailing: {
            trailing
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .combine)
    }

    @ViewBuilder
    private var trailing: some View {
        if row.paused {
            Button("Unpause") { Task { await center.setPaused(row.row, false) } }
                .font(.subheadline.weight(.semibold))
                .buttonStyle(.borderless)
                .tint(onAccent ? Color.white : nil)
                .disabled(!row.online || center.busy.contains(row.id))
        } else if row.completed {
            EmptyView()
        } else if let reach = row.reach {
            Text(reach.rawValue)
                .font(.footnote.weight(.semibold))
                .padding(.horizontal, 10)
                .padding(.vertical, 4)
                .background(Capsule().fill((onAccent ? Color.white : reachColor(reach)).opacity(0.2)))
                .foregroundStyle(onAccent ? AnyShapeStyle(Color.white) : AnyShapeStyle(.primary))
                .fixedSize()
        } else if let status = row.status {
            VStack(alignment: .trailing, spacing: 2) {
                Text(status)
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(onAccent ? .white : row.statusState.map(stateColor) ?? .secondary)
                if let w = row.when {
                    Text(w)
                        .font(.footnote)
                        .foregroundStyle(onAccent ? AnyShapeStyle(Color.white.opacity(0.85)) : AnyShapeStyle(.secondary))
                }
            }
            .multilineTextAlignment(.trailing)
        }
    }

    private func reachColor(_ r: CapcomJobReach) -> Color {
        switch r {
        case .actsAlone: return .green
        case .asksFirst: return .orange
        case .tellsYou: return .secondary
        }
    }
}

#endif
