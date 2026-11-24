import SwiftUI

extension Fleet {
    /// Opens a link from outside the app (app.md 3.15): a file, a usage link, a request link
    /// or a pairing link here; a session link goes back to the window that received it, which
    /// opens the session. The iOS and Mac apps share it.
    public func open(url: URL) -> SessionLink? {
        switch AppLink.kind(url) {
        case .file:
            // Any file from the share sheet or Files goes to the send sheet (app.md 4.4); a key
            // file goes to key import (app.md 10.4).
            if !files.open(url) { ssh?.openKeyFile(url) }
        case .session(let link):
            return link
        case .usage:
            _ = usage?.open(url: url)
        case .request:
            approvals?.open(url: url)
        case .pair:
            ssh?.open(url: url)
        case .unknown:
            // A malformed or unknown link is ignored (app.md 3.15).
            break
        }
        return nil
    }
}

/// The fleet's environment for every window: the board windows, a session window and the
/// Mac's Settings window.
struct FleetEnvironment: ViewModifier {
    let fleet: Fleet

    func body(content: Content) -> some View {
        content
            .environment(\.habConnections, fleet.lookup)
            .environment(\.terminals, fleet.terminals)
            .environment(\.sshSetup, fleet.ssh)
            .environment(\.voiceKit, fleet.voice)
            .environment(\.sendFiles, fleet.files)
            .environment(\.confirmations, fleet.confirms)
            .environment(\.capcom, fleet.capcom)
            .environment(\.rounds, fleet.rounds)
            .environment(\.groups, fleet.groups)
            .environment(\.approvals, fleet.approvals)
            .environment(\.drafts, fleet.drafts)
    }
}

/// Mac: a window counts for the lifecycle rule, so the habs stay connected while any is open
/// (app.md 12.1). iPhone and iPad follow the app's scene phase instead.
struct CountsWindow: ViewModifier {
    let fleet: Fleet

    func body(content: Content) -> some View {
        #if os(macOS)
        content
            .onAppear { fleet.windowOpened() }
            .onDisappear { fleet.windowClosed() }
        #else
        content
        #endif
    }
}

/// Opens session links and Handoff in the window that received them (app.md 3.15): the
/// session opens as a tap on its row does, once the board lists it, within a minute.
struct SessionLinkHost: ViewModifier {
    let fleet: Fleet
    let open: (BoardRow) -> Void
    @State private var pending: PendingLink?
    @State private var unknown = false

    func body(content: Content) -> some View {
        content
            .onOpenURL { url in
                if let link = fleet.open(url: url) { start(link) }
            }
            .onContinueUserActivity(SessionLink.activityType) { activity in
                if let link = SessionLink(userInfo: activity.userInfo) { start(link) }
            }
            .onChange(of: fleet.board.rows) { _, _ in attempt() }
            .alert("No paired computer has this session.", isPresented: $unknown) {
                Button("OK", role: .cancel) {}
            }
    }

    private func start(_ link: SessionLink) {
        pending = PendingLink(link, now: .now)
        attempt()
    }

    private func attempt() {
        guard let p = pending else { return }
        if p.expired(now: .now) {
            pending = nil
            return
        }
        switch p.link.resolve(in: fleet.board) {
        case .row(let row):
            pending = nil
            open(row)
        case .unknownHab:
            pending = nil
            unknown = true
        case .waiting:
            break
        }
    }
}

/// The open session advertises Handoff after it stays on screen for a short while, with ids
/// only (app.md 3.15).
struct SessionHandoff: ViewModifier {
    let link: SessionLink?
    @State private var dwelled = false

    func body(content: Content) -> some View {
        content
            .userActivity(SessionLink.activityType, isActive: link != nil && dwelled) { activity in
                guard let link else { return }
                activity.addUserInfoEntries(from: link.userInfo)
                activity.requiredUserInfoKeys = Set(link.userInfo.keys)
                activity.isEligibleForHandoff = true
                activity.isEligibleForSearch = false
                activity.isEligibleForPublicIndexing = false
                activity.targetContentIdentifier = link.url.absoluteString
            }
            .task(id: link) {
                dwelled = false
                guard link != nil else { return }
                do {
                    try await Task.sleep(for: SessionLink.handoffDwell)
                    dwelled = true
                } catch {}
            }
    }
}

extension View {
    /// Handoff for the session on screen (app.md 3.15).
    func sessionHandoff(_ row: BoardRow, board: Board) -> some View {
        modifier(SessionHandoff(link: SessionLink.of(row, board: board)))
    }
}

/// A session in its own window (app.md 3.14): the session by ids, shown once the board lists
/// it, following a restart to its new pane.
public struct SessionWindowRoot: View {
    private let fleet: Fleet
    private let link: SessionLink?
    @State private var commands = WindowCommands(kind: .session)
    @Environment(\.openWindow) private var openWindow
    /// The minute to wait for the row is over.
    @State private var expired = false
    /// The board listed the session once: a later absence is a removal.
    @State private var listed = false

    public init(fleet: Fleet, link: SessionLink?) {
        self.fleet = fleet
        self.link = link
    }

    private var resolution: SessionLink.Resolution? { link?.resolve(in: fleet.board) }

    /// The approval sheet's chat button: this window shows its own session; another session
    /// opens in its own window.
    private func openChat(_ t: SessionTarget) {
        fleet.approvals?.presenting = false
        guard t.session != row?.target.session || t.habID != row?.target.habID,
              let r = fleet.board.rows.first(where: { $0.target == t }),
              let l = SessionLink.of(r, board: fleet.board) else { return }
        openWindow(value: l)
    }

    private var row: BoardRow? {
        if case .row(let r) = resolution { return r }
        return nil
    }

    public var body: some View {
        content
            .modifier(SessionActionsHost(center: fleet.sessionActions))
            // An approval sheet from outside shows here too, so a notification tap never
            // shows nothing (app.md 3.14).
            .modifier(ApprovalSheetHost(center: fleet.approvals, openChat: openChat))
            .modifier(FleetEnvironment(fleet: fleet))
            .modifier(PublishCommands(fleet: fleet, commands: commands))
            .modifier(CountsWindow(fleet: fleet))
            .onChange(of: row?.id, initial: true) { _, id in
                commands.openRowID = id
                if id != nil { listed = true }
            }
            .task {
                try? await Task.sleep(for: .seconds(SessionRestore.limit))
                expired = true
            }
            #if os(macOS)
            .frame(minWidth: 720, minHeight: 420)
            #endif
    }

    @ViewBuilder
    private var content: some View {
        if let row {
            SessionWindowContent(fleet: fleet, row: row)
        } else if listed {
            ContentUnavailableView("This session was removed.", systemImage: "rectangle.stack")
        } else if link == nil || expired || resolution == .unknownHab {
            ContentUnavailableView("This session is not on any paired computer.", systemImage: "rectangle.stack")
        } else {
            ProgressView("Opening \(link?.session ?? "")")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }
}

/// The session view of a session window.
private struct SessionWindowContent: View {
    let fleet: Fleet
    let row: BoardRow
    #if os(iOS)
    @Environment(\.horizontalSizeClass) private var sizeClass
    #endif

    var body: some View {
        #if os(iOS)
        NavigationStack {
            session(compact: sizeClass == .compact)
                .navigationBarTitleDisplayMode(.inline)
        }
        #else
        // The session view sets the title, the subtitle and the customisable toolbar.
        session(compact: false)
        #endif
    }

    private func session(compact: Bool) -> some View {
        SessionView(model: fleet.chat(for: row.target), compact: compact, inWindow: true)
            .id(row.target)
            .sessionHandoff(row, board: fleet.board)
            // session.focus while the session is on screen (app.md 8.6).
            .onAppear { fleet.focus.show(row.target) }
            .onDisappear { fleet.focus.hide(row.target) }
    }
}

#if os(macOS)
/// The Mac's Settings window (app.md 3.8, 20.7): the full Settings page, ⌘,.
public struct FlightdeckSettings: View {
    private let fleet: Fleet

    public init(fleet: Fleet) {
        self.fleet = fleet
    }

    public var body: some View {
        NavigationStack {
            if let settings = fleet.settings {
                SettingsPage(center: settings, board: fleet.board, push: fleet.push, openUsage: fleet.usage.map { u in
                    { u.book.present(account: nil) }
                })
            } else {
                ContentUnavailableView("No settings", systemImage: "slider.horizontal.3",
                                       description: Text("Settings show once a computer is paired."))
            }
        }
        .frame(minWidth: 520, idealWidth: 600, minHeight: 560, idealHeight: 720)
        .modifier(FleetEnvironment(fleet: fleet))
        .modifier(CountsWindow(fleet: fleet))
    }
}
#endif

/// The board's link handling, with a fleet (app.md 3.15).
struct BoardLinks: ViewModifier {
    let fleet: Fleet?
    let open: (BoardRow) -> Void

    func body(content: Content) -> some View {
        if let fleet {
            content.modifier(SessionLinkHost(fleet: fleet, open: open))
        } else {
            content
        }
    }
}

/// Next Needing You as a toolbar button (app.md 3.16): off when no other session needs the
/// owner.
struct NextNeedingYouButton: View {
    let board: Board
    let openedID: String?
    let open: (BoardRow) -> Void

    var body: some View {
        let n = NextNeedingYou.count(in: board.sections, excluding: openedID)
        Button("Next Needing You", systemImage: "bell.badge") {
            if let r = NextNeedingYou.next(in: board.sections, after: openedID) { open(r) }
        }
        .disabled(n == 0)
        .help(n == 0 ? "Nothing else needs you" : "Open the next session that needs you (⌥⌘A)")
        .accessibilityValue(n == 0 ? "" : "\(n) waiting")
    }
}
