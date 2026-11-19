#if os(iOS)
import FlightdeckShared
import HabAPI
import SwiftUI
import UIKit

/// The tabs of the app shell (app.md 3.8).
enum ShellTab: String, Hashable {
    case sessions, capcom, rounds, settings
}

/// The iPhone and iPad root (app.md 3.8, 3.9): first run while no hab is in the app, else four
/// tabs with the usage strip as the tab bar's bottom accessory.
struct AppShell: View {
    let fleet: Fleet
    @State private var tab: ShellTab = .sessions
    @State private var opened: BoardRow?
    /// Debug builds: the Capcom jobs list opened at launch.
    @State private var capcomJobsShown = false
    @Environment(\.horizontalSizeClass) private var sizeClass

    private var board: Board { fleet.board }

    var body: some View {
        content
            .modifier(ApprovalSheetHost(center: fleet.approvals, openChat: openChat))
            .modifier(SendFileSheetHost(center: fleet.files, board: board))
            .modifier(CapcomHost(center: fleet.capcom, board: board, opened: $opened))
            .usageSheet(fleet.usage, governor: fleet.capcom)
            .onChange(of: opened) { _, r in
                guard r != nil else { return }
                tab = .sessions
                fleet.rounds.sessionOpened()
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
            .modifier(ShellLaunchArguments(fleet: fleet, tab: $tab, opened: $opened, capcomJobsShown: $capcomJobsShown))
            #endif
    }

    @ViewBuilder
    private var content: some View {
        if FirstRun.shows(habs: board.habs.count, demo: fleet.isDemo) {
            FirstRunView(fleet: fleet)
        } else if #available(iOS 26.1, *) {
            modernTabs
        } else {
            legacyTabs
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
        SessionsTab(fleet: fleet, opened: $opened, footer: !Self.hasAccessory) { tab = .settings }
    }

    private static var hasAccessory: Bool {
        if #available(iOS 26.1, *) { return true }
        return false
    }

    @available(iOS 26.1, *)
    private var modernTabs: some View {
        TabView(selection: $tab) {
            Tab("Sessions", systemImage: "rectangle.stack", value: ShellTab.sessions) {
                sessions
            }
            .badge(board.needsYouCount)
            Tab("Capcom", systemImage: "dot.radiowaves.left.and.right", value: ShellTab.capcom) {
                CapcomTab(fleet: fleet)
            }
            Tab("Rounds", systemImage: "clock.arrow.circlepath", value: ShellTab.rounds) {
                RoundsTab(center: fleet.rounds)
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

    private var legacyTabs: some View {
        TabView(selection: $tab) {
            sessions
                .tabItem { Label("Sessions", systemImage: "rectangle.stack") }
                .badge(board.needsYouCount)
                .tag(ShellTab.sessions)
            CapcomTab(fleet: fleet)
                .tabItem { Label("Capcom", systemImage: "dot.radiowaves.left.and.right") }
                .tag(ShellTab.capcom)
            RoundsTab(center: fleet.rounds)
                .tabItem { Label("Rounds", systemImage: "clock.arrow.circlepath") }
                .tag(ShellTab.rounds)
            SettingsTab(fleet: fleet)
                .tabItem { Label("Settings", systemImage: "slider.horizontal.3") }
                .tag(ShellTab.settings)
        }
    }
}

#if DEBUG
/// Debug builds only: launch arguments that open a screen with no taps, so a build can be
/// checked and screenshotted headlessly. `-FlightdeckTab capcom|rounds|settings`,
/// `-FlightdeckOpenCapcom YES`, `-FlightdeckOpenSession <session>`, `-FlightdeckOpenCapcomJobs YES`,
/// `-FlightdeckShow review|group|hab`.
struct ShellLaunchArguments: ViewModifier {
    let fleet: Fleet
    @Binding var tab: ShellTab
    @Binding var opened: BoardRow?
    @Binding var capcomJobsShown: Bool
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
                for _ in 0..<100 {
                    if let row = fleet.board.rows.first(where: { $0.target.session == id }) {
                        opened = row
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
/// width pushes the session; a regular one shows it beside the list.
struct SessionsTab: View {
    let fleet: Fleet
    @Binding var opened: BoardRow?
    /// Before iOS 26.1 the usage strip sits under the list (app.md 3.7).
    let footer: Bool
    let openSettings: () -> Void
    @Environment(\.horizontalSizeClass) private var sizeClass

    var body: some View {
        if sizeClass == .compact {
            NavigationStack {
                list
                    .navigationDestination(item: $opened) { row in
                        session(row, compact: true)
                            .toolbar(.hidden, for: .tabBar)
                    }
            }
        } else if let row = opened {
            BoardSessionSplit {
                NavigationStack { list }
            } session: {
                session(row, compact: false)
            }
        } else {
            BoardSessionSplit {
                NavigationStack { list }
            } session: {
                ContentUnavailableView("Choose a session", systemImage: "rectangle.stack",
                                       description: Text("Pick a row to open its chat and terminal."))
            }
        }
    }

    private var list: some View {
        SessionsListView(fleet: fleet, opened: $opened)
            .modifier(OptionalUsageFooter(usage: footer ? fleet.usage : nil))
            .navigationTitle("Sessions")
            .navigationBarTitleDisplayMode(.large)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    HostsMenu(board: fleet.board, openSettings: openSettings)
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

struct OptionalUsageFooter: ViewModifier {
    let usage: UsageCenter?

    func body(content: Content) -> some View {
        if let usage { content.usageFooter(usage) } else { content }
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

    private var board: Board { fleet.board }

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
            let sections = sections
            ScrollViewReader { proxy in
            List {
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
                if sections.isEmpty {
                    ContentUnavailableView("No sessions", systemImage: "rectangle.stack",
                                           description: Text("Sessions on your computers show here."))
                }
            }
        }
    }

    private func open(_ row: BoardRow) {
        board.open(row)
        opened = row
    }

    @ViewBuilder
    private func item(_ item: SessionItem, now: Date) -> some View {
        switch item {
        case .permission(let row):
            PermissionRow(row: row, approvals: fleet.approvals, now: now) { open(row) }
        case .question(let row):
            ActionRow(row: row, now: now, action: "Answer", open: { open(row) }, act: { open(row) })
        case .confirm(let card):
            if let center = fleet.confirms { ConfirmRow(center: center, card: card, now: now) }
        case .group(let g):
            NavigationLink(value: g) { GroupRow(group: g, now: now) }
        case .session(let row):
            Button { open(row) } label: { SessionRow(row: row, now: now) }
                .buttonStyle(.plain)
        }
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
                        .lineLimit(2)
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
                        .lineLimit(3)
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
                            .lineLimit(3)
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
                            .lineLimit(3)
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
                    .lineLimit(3)
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
            .navigationTitle("Review")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Close", action: close)
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
                        .lineLimit(2)
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

/// The Capcom tab: the existing Capcom view as a tab root (app.md 3.8).
struct CapcomTab: View {
    let fleet: Fleet
    @Environment(\.horizontalSizeClass) private var sizeClass

    var body: some View {
        if sizeClass == .compact {
            NavigationStack {
                CapcomScreen(fleet: fleet, compact: true, close: nil)
            }
        } else {
            CapcomScreen(fleet: fleet, compact: false, close: nil)
        }
    }
}

/// The Rounds tab (app.md 3.8, 21.2): the rounds per hab, the run session switch, New round,
/// and Capcom jobs.
struct RoundsTab: View {
    let center: RoundCenter

    var body: some View {
        NavigationStack {
            TimelineView(.periodic(from: .now, by: 30)) { ctx in
                List {
                    if center.any {
                        BoardRoundsSections(center: center, now: ctx.date)
                    }
                    CapcomJobsSection(center: center)
                }
                .overlay {
                    if !center.any && !center.jobs.available {
                        ContentUnavailableView("No rounds", systemImage: "clock.arrow.circlepath",
                                               description: Text("Scheduled jobs on your computers show here."))
                    }
                }
            }
            .navigationDestination(for: RoundRow.self) { row in
                RoundPage(center: center, habID: row.habID, roundID: row.round.id)
            }
            .navigationTitle("Rounds")
        }
    }
}
#endif
