import HabAPI
import SwiftUI
#if os(iOS)
import UIKit
#else
import AppKit
#endif

/// A session row's actions (app.md 3.11): the context menu, and a swipe to the left for Close
/// and Rename. The questions and alerts live in `SessionActionsHost`.
struct SessionRowActions: ViewModifier {
    let row: BoardRow
    let center: SessionActionCenter?

    func body(content: Content) -> some View {
        if let center, case let offers = center.offers(row), !offers.isEmpty {
            content
                .contextMenu {
                    ForEach(offers, id: \.self) { button($0, center) }
                }
                .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                    // No destructive role: the list would take the row out before the question answers.
                    if offers.contains(.close) {
                        Button("Close", systemImage: "xmark.circle") { center.askClose(row) }
                            .tint(.red)
                    }
                    if offers.contains(.rename) {
                        Button("Rename", systemImage: "pencil") { center.askRename(row) }
                    }
                }
        } else {
            content
        }
    }

    @ViewBuilder
    private func button(_ a: SessionAction, _ center: SessionActionCenter) -> some View {
        switch a {
        case .rename:
            Button("Rename…", systemImage: "pencil") { center.askRename(row) }
        case .restart:
            Button("Restart", systemImage: "arrow.clockwise") { Task { await center.restart(row) } }
        case .copyName:
            Button("Copy name", systemImage: "doc.on.doc") { Self.copy(row.name ?? row.title) }
        case .close:
            Divider()
            Button("Close…", systemImage: "xmark.circle", role: .destructive) { center.askClose(row) }
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
/// over the Sessions list or the Mac board.
struct SessionActionsHost: ViewModifier {
    @Bindable var center: SessionActionCenter

    func body(content: Content) -> some View {
        let q = center.closing.map(SessionActionCenter.question)
        content
            .confirmationDialog(q?.title ?? "", isPresented: present(\.closing), titleVisibility: .visible, presenting: center.closing) { row in
                Button(q?.button ?? "Close", role: .destructive) { Task { await center.close(row) } }
                Button("Cancel", role: .cancel) {}
            } message: { _ in
                Text(q?.message ?? "")
            }
            .alert("Rename session", isPresented: present(\.renaming), presenting: center.renaming) { row in
                TextField("Name", text: $center.newName)
                    .autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif
                Button("Cancel", role: .cancel) {}
                Button("Rename") { Task { await center.rename(row, to: center.newName) } }
                    .keyboardShortcut(.defaultAction)
            } message: { _ in
                Text("Letters, digits, dot, dash and underscore, up to 64.")
            }
            .alert(center.failure?.title ?? "", isPresented: Binding(get: { center.failure != nil }, set: { if !$0 { center.failure = nil } }),
                   presenting: center.failure) { _ in
                Button("OK", role: .cancel) {}
            } message: { f in
                if let m = f.message { Text(m) }
            }
    }

    /// A question's binding: up while its row is set; dismissing it clears the row.
    private func present(_ key: ReferenceWritableKeyPath<SessionActionCenter, BoardRow?>) -> Binding<Bool> {
        Binding(get: { center[keyPath: key] != nil }, set: { if !$0 { center[keyPath: key] = nil } })
    }
}

/// Follows the row actions in the shell (app.md 3.11): a removed open session closes, and an
/// open session follows its restart to the new pane once the board lists it.
struct SessionActionFollow: ViewModifier {
    let center: SessionActionCenter
    let board: Board
    @Binding var opened: BoardRow?

    func body(content: Content) -> some View {
        content
            .onChange(of: center.removed) { _, id in
                guard let id else { return }
                center.removalSeen()
                if opened?.id == id { opened = nil }
            }
            .onChange(of: center.restarted) { _, _ in follow(board.rows) }
            .onChange(of: board.rows) { _, rows in follow(rows) }
    }

    private func follow(_ rows: [BoardRow]) {
        guard let t = center.restarted, let row = rows.first(where: { $0.target == t }) else { return }
        center.restarted = nil
        if let o = opened, o.target.habID == t.habID, o.target.session == t.session { opened = row }
    }
}

/// Reopens the session a window last showed (app.md 3.12), once the board lists its row,
/// within a minute. Any other open before that cancels it.
struct RestoreOpenSession: ViewModifier {
    let board: Board
    @Binding var opened: BoardRow?
    /// The scene's saved row id.
    let saved: String
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
        board.open(row)
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
