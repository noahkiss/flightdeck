import Foundation
import SwiftUI

/// Files opened in Flightdeck from another app ("Open in", the share sheet, Files), on their
/// way to a session (app.md 4.4). A key file goes to key import instead (app.md 10.4).
///
/// The flow: a file arrives and the send sheet opens; the owner picks a session or Capcom; the
/// session opens with the file in its composer, where the owner can mark it up, add text and
/// send it.
@MainActor
@Observable
public final class SendFileCenter {
    /// A file waiting for the owner to pick where it goes.
    public struct Incoming: Identifiable, Equatable {
        public var id = UUID()
        public var name: String
        /// The prepared file, or nil when it could not be read.
        public var attachment: DraftAttachment?
        public var error: String?
    }

    /// The file the send sheet shows.
    public var incoming: Incoming?
    /// A session to open: the board opens it, then clears this.
    public var openRequest: SessionTarget?
    /// Counts files handed to a composer, so an open session view takes them at once.
    public private(set) var handed = 0
    /// The standing Capcom conversation, once a hab offers one (capcom.md 7.2). Nil hides the
    /// Capcom row.
    @ObservationIgnored public var capcom: (() -> SessionTarget?)?

    @ObservationIgnored private var drafts: [SessionTarget: [DraftAttachment]] = [:]

    public init() {}

    /// What an opened file is: a key for key import, or a file to send.
    public enum Route: Equatable {
        case key
        case file
    }

    /// Only a private key file goes to key import: at most 64 KiB, and its first line that is
    /// not blank opens a private key block (`-----BEGIN OPENSSH PRIVATE KEY-----`, a PEM
    /// `... PRIVATE KEY-----`). Key import then explains a broken or encrypted one. Anything
    /// else, a public key or a README that quotes a key included, is a file to send.
    public nonisolated static func route(_ data: Data) -> Route {
        guard data.count <= OpenSSHKeyFile.maxSize,
              let text = String(data: data.prefix(4096), encoding: .utf8) ?? String(data: data.prefix(4096), encoding: .isoLatin1) else { return .file }
        let first = text.split(whereSeparator: \.isNewline).lazy
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .first { !$0.isEmpty } ?? ""
        return first.hasPrefix("-----BEGIN ") && first.hasSuffix("PRIVATE KEY-----") ? .key : .file
    }

    /// Takes a file opened from another app. A copy iOS placed in the app's Inbox is removed
    /// once read. Returns false when the file is a key and belongs to key import.
    @discardableResult
    public func open(_ url: URL) -> Bool {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        let name = url.lastPathComponent
        if let size = (try? url.resourceValues(forKeys: [.fileSizeKey]))?.fileSize, size > AttachmentLimits.maxBytes {
            incoming = Incoming(name: name, error: "\(name) is over the 25 MB limit")
            removeInboxCopy(url)
            return true
        }
        let data: Data
        do {
            data = try Data(contentsOf: url)
        } catch {
            incoming = Incoming(name: name, error: "Could not read the file: \(error.localizedDescription)")
            return true
        }
        if Self.route(data) == .key { return false }
        receive(data, name: name)
        removeInboxCopy(url)
        return true
    }

    /// Prepares a file's bytes and opens the send sheet.
    public func receive(_ data: Data, name: String) {
        let a = AttachmentPrep.file(data, name: name)
        if let r = AttachmentLimits.refusal(a) {
            incoming = Incoming(name: name, error: r)
        } else {
            incoming = Incoming(name: name, attachment: a)
        }
    }

    /// Sends the waiting file to a session's composer and asks the board to open the session.
    public func send(to target: SessionTarget) {
        guard let a = incoming?.attachment else { return }
        drafts[target, default: []].append(a)
        incoming = nil
        handed += 1
        openRequest = target
    }

    /// The files waiting for a session's composer; taking them clears them.
    public func take(for target: SessionTarget) -> [DraftAttachment] {
        drafts.removeValue(forKey: target) ?? []
    }

    private func removeInboxCopy(_ url: URL) {
        #if os(iOS)
        if SSHSetup.isOwnInboxCopy(url, home: NSHomeDirectory()) {
            try? FileManager.default.removeItem(at: url)
        }
        #endif
    }
}

extension EnvironmentValues {
    @Entry public var sendFiles: SendFileCenter? = nil
}

/// The send sheet: the file, then where it goes — Capcom when a hab offers it, then every
/// session by host. Offline sessions are listed but cannot be picked.
struct SendFileSheet: View {
    let center: SendFileCenter
    let board: Board
    let incoming: SendFileCenter.Incoming
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            List {
                Section {
                    HStack(spacing: 12) {
                        if let a = incoming.attachment, let img = DraftThumbs.image(a) {
                            Image(decorative: img, scale: 1).resizable().scaledToFill()
                                .frame(width: 56, height: 56)
                                .clipShape(RoundedRectangle(cornerRadius: 8))
                        } else {
                            Image(systemName: "doc").font(.title2).frame(width: 56, height: 56)
                        }
                        VStack(alignment: .leading) {
                            Text(incoming.attachment?.name ?? incoming.name).lineLimit(2)
                            if let a = incoming.attachment {
                                Text(ByteCountFormatter.string(fromByteCount: Int64(a.data.count), countStyle: .file))
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    }
                    if let e = incoming.error {
                        Text(e).foregroundStyle(.red)
                    }
                }
                if incoming.attachment != nil {
                    if let t = center.capcom?() {
                        Section {
                            Button { center.send(to: t) } label: {
                                Label("Capcom", systemImage: "bubble.left.and.text.bubble.right")
                            }
                        }
                    }
                    ForEach(SendTargets.groups(board.rows), id: \.host) { g in
                        Section(g.host) {
                            ForEach(g.rows) { row in
                                Button { center.send(to: row.target) } label: {
                                    VStack(alignment: .leading) {
                                        Text(row.title)
                                        if let p = row.project {
                                            Text(p).font(.caption).foregroundStyle(.secondary)
                                        }
                                    }
                                }
                                .disabled(row.offline)
                            }
                        }
                    }
                    if board.rows.isEmpty {
                        Text("No sessions").foregroundStyle(.secondary)
                    }
                }
            }
            .navigationTitle("Send to")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        center.incoming = nil
                        dismiss()
                    }
                }
            }
        }
    }
}

/// Send targets grouped by host, in the board's order.
enum SendTargets {
    struct Group {
        var host: String
        var rows: [BoardRow]
    }

    /// Sessions only (a row with a session id), grouped by host in first-seen order.
    static func groups(_ rows: [BoardRow]) -> [Group] {
        var out: [Group] = []
        for r in rows where r.target.session != nil {
            if let i = out.firstIndex(where: { $0.host == r.host }) {
                out[i].rows.append(r)
            } else {
                out.append(Group(host: r.host, rows: [r]))
            }
        }
        return out
    }
}

/// Shows the send sheet for a file opened from another app.
struct SendFileSheetHost: ViewModifier {
    let center: SendFileCenter?
    let board: Board

    func body(content: Content) -> some View {
        if let center {
            content.sheet(item: Binding(get: { center.incoming }, set: { center.incoming = $0 })) { inc in
                SendFileSheet(center: center, board: board, incoming: inc)
                #if os(macOS)
                    .frame(minWidth: 420, minHeight: 480)
                #endif
            }
        } else {
            content
        }
    }
}
