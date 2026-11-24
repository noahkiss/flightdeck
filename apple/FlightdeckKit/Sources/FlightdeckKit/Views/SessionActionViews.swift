import HabAPI
import SwiftUI
#if os(iOS)
import UIKit
#else
import AppKit
#endif

extension EnvironmentValues {
    /// The window's questions and alerts for the row actions (app.md 3.11).
    @Entry var sessionAsk: SessionAsk? = nil
}

/// A session row's actions (app.md 3.11): the context menu, and a swipe to the left for Close
/// and Rename. The questions and alerts are the window's (`SessionActionsHost`). A row with a
/// call running offers nothing until it answers.
struct SessionRowActions: ViewModifier {
    let row: BoardRow
    let center: SessionActionCenter?
    @Environment(\.sessionAsk) private var ask
    @Environment(\.windowCommands) private var commands

    func body(content: Content) -> some View {
        if let center, let ask, case let offers = center.offers(row), !offers.isEmpty || row.target.session != nil {
            let busy = center.running.contains(row.id)
            let link = SessionLink.of(row, board: center.board)
            content
                .contextMenu {
                    // Above the row actions: a window of its own and the read marks (app.md 3.11).
                    if let link, let open = commands?.openWindow {
                        Button("Open in New Window", systemImage: "macwindow.badge.plus") { open(link) }
                    }
                    if center.board.canMark(row) {
                        if row.rank == .doneUnseen {
                            Button("Mark as Read", systemImage: "envelope.open") { center.board.markSeen(row) }
                        } else {
                            Button("Mark as Unread", systemImage: "envelope.badge") { center.board.markUnseen(row) }
                        }
                    }
                    if link != nil || center.board.canMark(row) { Divider() }
                    ForEach(offers, id: \.self) { button($0, center, ask, link: link) }
                        .disabled(busy)
                    // A row whose offers hold no Copy name still gets its link.
                    if let link, !offers.contains(.copyName) {
                        Button("Copy Link", systemImage: "link") { Self.copy(link.url.absoluteString) }
                    }
                }
                .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                    // No destructive role: the list would take the row out before the question answers.
                    if offers.contains(.close) {
                        Button("Close", systemImage: "xmark.circle") { ask.askClose(row) }
                            .tint(.red)
                            .disabled(busy)
                    }
                    if offers.contains(.rename) {
                        Button("Rename", systemImage: "pencil") { ask.askRename(row) }
                            .disabled(busy)
                    }
                }
        } else {
            content
        }
    }

    @ViewBuilder
    private func button(_ a: SessionAction, _ center: SessionActionCenter, _ ask: SessionAsk, link: SessionLink?) -> some View {
        switch a {
        case .rename:
            Button("Rename…", systemImage: "pencil") { ask.askRename(row) }
        case .restart:
            Button("Restart", systemImage: "arrow.clockwise") {
                Task { await ask.run { await center.restart(row) } }
            }
        case .copyName:
            Button("Copy name", systemImage: "doc.on.doc") { Self.copy(row.name ?? row.title) }
            if let link {
                Button("Copy Link", systemImage: "link") { Self.copy(link.url.absoluteString) }
            }
        case .close:
            Divider()
            Button("Close…", systemImage: "xmark.circle", role: .destructive) { ask.askClose(row) }
        }
    }

    static func copy(_ s: String) {
        #if os(iOS)
        UIPasteboard.general.string = s
        #else
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(s, forType: .string)
        #endif
    }
}

extension View {
    /// The row's actions (app.md 3.11); no change without a center.
    func sessionRowActions(_ row: BoardRow, _ center: SessionActionCenter?) -> some View {
        modifier(SessionRowActions(row: row, center: center))
    }
}

/// The Close question, the Rename alert and the failure alert of the row actions (app.md 3.11),
/// over the Sessions list or the Mac board. Each window holds its own, so a question shows
/// only where it was asked.
struct SessionActionsHost: ViewModifier {
    let center: SessionActionCenter
    @State private var ask = SessionAsk()
    /// The menus ask through this window's questions too (app.md 3.13).
    @Environment(\.windowCommands) private var commands

    func body(content: Content) -> some View {
        @Bindable var ask = ask
        let q = ask.closing.map(SessionActionCenter.question)
        content
            .environment(\.sessionAsk, ask)
            .onAppear { commands?.ask = ask }
            .confirmationDialog(q?.title ?? "", isPresented: $ask.closingShown, titleVisibility: .visible, presenting: ask.closing) { row in
                Button(SessionActionCenter.question(row).button, role: .destructive) {
                    Task { await ask.run { await center.close(row) } }
                }
                Button("Cancel", role: .cancel) {}
            } message: { row in
                Text(SessionActionCenter.question(row).message)
            }
            .alert("Rename session", isPresented: Binding(get: { ask.renaming != nil }, set: { if !$0 { ask.renaming = nil } }),
                   presenting: ask.renaming) { row in
                TextField("Name", text: $ask.newName)
                    .autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif
                Button("Cancel", role: .cancel) {}
                Button("Rename") {
                    let name = ask.newName
                    Task { await ask.run { await center.rename(row, to: name) } }
                }
                .keyboardShortcut(.defaultAction)
            } message: { _ in
                Text("Letters, digits, dot, dash and underscore, up to 64.")
            }
            .alert(ask.failure?.title ?? "", isPresented: Binding(get: { ask.failure != nil }, set: { if !$0 { ask.failure = nil } }),
                   presenting: ask.failure) { _ in
                Button("OK", role: .cancel) {}
            } message: { f in
                if let m = f.message { Text(m) }
            }
    }
}

/// Follows the row actions in a window (app.md 3.11): a removed open session closes, and an
/// open session follows its restart to the new pane once the board lists it. Every window
/// sees every event; each keeps its own wait.
struct SessionActionFollow: ViewModifier {
    let center: SessionActionCenter
    let board: Board
    @Binding var opened: BoardRow?
    /// The restart this window still waits to follow.
    @State private var pending: SessionTarget?

    func body(content: Content) -> some View {
        content
            .onChange(of: center.removal) { _, e in
                guard let e else { return }
                if opened?.id == e.rowID { opened = nil }
            }
            .onChange(of: center.restart) { _, e in
                guard let e, let o = opened, o.target.habID == e.target.habID, o.target.session == e.target.session else { return }
                pending = e.target
                follow(board.rows)
            }
            .onChange(of: board.rows) { _, rows in follow(rows) }
    }

    private func follow(_ rows: [BoardRow]) {
        guard let t = pending else { return }
        guard let o = opened, o.target.habID == t.habID, o.target.session == t.session else {
            pending = nil
            return
        }
        let row = rows.first { r in
            guard r.target.habID == t.habID, r.target.session == t.session else { return false }
            // A restart that timed out names no pane: follow once the session has a new one.
            return t.pane.map { r.target.pane == $0 } ?? (r.target.pane != nil && r.target.pane != o.target.pane)
        }
        guard let row else { return }
        pending = nil
        opened = row
    }
}

/// Reopens the session a window last showed (app.md 3.12), once the board lists its row,
/// within a minute. Any other open before that cancels it: a change of `opened`, or of
/// `interrupt` (a tab, the Capcom view, an approval sheet, a tap on any row).
struct RestoreOpenSession<Interrupt: Equatable>: ViewModifier {
    let board: Board
    @Binding var opened: BoardRow?
    /// The scene's saved row id.
    let saved: String
    /// The window the restore runs in; its opens count there (app.md 3.12).
    var window: UUID? = nil
    let interrupt: Interrupt
    /// Called just before the restore opens the row.
    var willOpen: () -> Void = {}
    @State private var restore: SessionRestore?

    func body(content: Content) -> some View {
        content
            .task {
                guard opened == nil, !Self.debugOpens else { return }
                restore = SessionRestore(saved: saved, now: .now)
                attempt(board.rows)
            }
            .onChange(of: board.rows) { _, rows in attempt(rows) }
            .onChange(of: opened) { _, _ in restore = nil }
            .onChange(of: interrupt) { _, _ in restore = nil }
    }

    private func attempt(_ rows: [BoardRow]) {
        guard let r = restore else { return }
        if r.expired(now: .now) {
            restore = nil
            return
        }
        guard let row = r.row(in: rows, now: .now) else { return }
        restore = nil
        willOpen()
        board.open(row, window: window)
        opened = row
    }

    /// Debug builds: `-FlightdeckOpenSession` wins over the restore.
    private static var debugOpens: Bool {
        #if DEBUG
        return !(UserDefaults.standard.string(forKey: "FlightdeckOpenSession") ?? "").isEmpty
        #else
        return false
        #endif
    }
}
