import HabAPI
import SwiftUI

extension EnvironmentValues {
    /// Earlier threads other hosts kept (capcom-service.md 7.1), for the top of the thread.
    @Entry var capcomEarlier: [CapcomEarlierThread] = []
}

/// The Capcom service's thread (capcom-service.md 7.2, 7.6), in place of a session's
/// transcript: bubbles with delivered and read marks, one quiet line per turn's tools, report
/// and round lines, cards, notes, dividers that open to their summary, the typing mark, and
/// the pinned proposals line. The composer below is the session's.
struct CapcomThreadView: View {
    let thread: CapcomThread
    let model: ChatModel
    @Environment(\.capcomEarlier) private var earlier
    @State private var atBottom = true
    @State private var showProposals = false

    var body: some View {
        let rows = thread.rows
        let lastUser = rows.last { if case .user = $0 { return true }; return false }?.id
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    if !earlier.isEmpty {
                        CapcomEarlierSection(earlier: earlier, images: model.images)
                    }
                    if thread.hasOlder {
                        HStack {
                            Spacer()
                            if thread.loadingOlder { ProgressView().controlSize(.small) } else { Text("Earlier messages").font(.caption).foregroundStyle(.secondary) }
                            Spacer()
                        }
                        .onAppear { Task { await thread.loadOlder() } }
                    }
                    ForEach(rows) { row in
                        CapcomRowView(row: row, thread: thread, images: model.images, hostID: model.context.hostID, showMark: row.id == lastUser)
                            .id(row.id)
                    }
                    ForEach(model.outgoing.filter(Self.showsOutgoing)) { o in
                        OutgoingView(outgoing: o, model: model)
                            .id("out-\(o.id)")
                    }
                    if thread.typing {
                        CapcomTypingMark()
                            .id("typing")
                    }
                    Color.clear.frame(height: 1).id(Self.bottom)
                        .onAppear { atBottom = true }
                        .onDisappear { atBottom = false }
                }
                .padding(12)
            }
            .defaultScrollAnchor(.bottom)
            .overlay { placeholder }
            .onChange(of: ScrollFollow.Ends(first: rows.first?.id, last: rows.last?.id)) { old, new in
                switch ScrollFollow.action(from: old, to: new, atBottom: atBottom) {
                case .none: break
                case .toBottom: proxy.scrollTo(Self.bottom, anchor: .bottom)
                case .keep(let id): proxy.scrollTo(id, anchor: .top)
                }
            }
            .onChange(of: thread.typing) { _, on in
                if on && atBottom { proxy.scrollTo(Self.bottom, anchor: .bottom) }
            }
            .onChange(of: model.outgoing.count) { old, new in
                if new > old { proxy.scrollTo(Self.bottom, anchor: .bottom) }
            }
        }
        .safeAreaInset(edge: .bottom, spacing: 0) {
            VStack(spacing: 4) {
                if let n = thread.notice {
                    Text(n).font(.caption).foregroundStyle(.red).frame(maxWidth: .infinity, alignment: .leading)
                }
                if let line = CapcomThread.proposalsLine(thread.proposals?.count ?? 0) {
                    Button { showProposals = true } label: {
                        Label(line, systemImage: "tray.full")
                            .font(.callout)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .buttonStyle(.borderless)
                }
            }
            .padding(.horizontal, 12)
            .padding(.vertical, thread.notice == nil && (thread.proposals ?? []).isEmpty ? 0 : 6)
        }
        .sheet(isPresented: $showProposals) {
            CapcomProposalsSheet(records: thread.proposals ?? [])
        }
    }

    /// The thread shows the user's sent messages itself; the composer's list keeps the ones
    /// still on their way or refused.
    static func showsOutgoing(_ o: Outgoing) -> Bool {
        switch o.status {
        case .sent, .delivered: return false
        default: return true
        }
    }

    static let bottom = "capcom-bottom"

    @ViewBuilder private var placeholder: some View {
        if thread.items.isEmpty && model.outgoing.isEmpty {
            switch thread.load {
            case .loading: ProgressView()
            case .failed(let e): ContentUnavailableView("Thread did not load", systemImage: "exclamationmark.bubble", description: Text(e))
            case .unavailable(let h): ContentUnavailableView("No thread", systemImage: "text.bubble", description: Text(h))
            default: ContentUnavailableView("Ask Capcom anything", systemImage: "dot.radiowaves.left.and.right")
            }
        }
    }
}

/// One row of the thread.
struct CapcomRowView: View {
    let row: CapcomRow
    let thread: CapcomThread
    let images: AttachmentImages?
    let hostID: String
    /// The newest user message shows its delivered or read mark.
    let showMark: Bool

    var body: some View {
        switch row {
        case .user(_, let text, let att, let context, let read):
            HStack {
                Spacer(minLength: 40)
                VStack(alignment: .trailing, spacing: 3) {
                    if !context.isEmpty { CapcomSentChips(items: context, hostID: hostID) }
                    if !att.isEmpty { AttachmentThumbRow(ids: att, images: images) }
                    if !text.isEmpty {
                        Text(text)
                            .textSelection(.enabled)
                            .padding(.horizontal, 12)
                            .padding(.vertical, 8)
                            .background(Color.accentColor.opacity(0.18), in: RoundedRectangle(cornerRadius: 14))
                    }
                    if showMark {
                        Text(read ? "Read" : "Delivered")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                }
            }
        case .capcom(_, let text, let wroteFirst):
            HStack {
                VStack(alignment: .leading, spacing: 3) {
                    if wroteFirst {
                        Label("Wrote first", systemImage: "bell")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                    MarkdownView(blocks: MarkdownBlock.parse(text))
                        .textSelection(.enabled)
                        .padding(.horizontal, 12)
                        .padding(.vertical, 8)
                        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 14))
                }
                Spacer(minLength: 40)
            }
        case .tools(_, let line, let calls):
            DisclosureGroup {
                VStack(alignment: .leading, spacing: 2) {
                    ForEach(Array(calls.enumerated()), id: \.offset) { _, c in
                        Text(c.summary.isEmpty ? c.name : c.summary).font(.caption).foregroundStyle(.secondary)
                    }
                }
            } label: {
                Label(line, systemImage: "wrench.and.screwdriver").font(.caption).foregroundStyle(.secondary)
            }
        case .quiet(_, let line, let detail):
            DisclosureGroup {
                Text(detail).font(.caption).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
            } label: {
                Label(line, systemImage: "arrow.down.message").font(.caption).foregroundStyle(.secondary)
            }
        case .card(let it):
            CapcomCardRow(item: it, thread: thread)
        case .note(_, let text):
            Text(text)
                .font(.caption)
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity)
        case .divider(_, let label, let summary):
            CapcomDividerRow(label: label, summary: summary)
        }
    }
}

/// A thin rule with its label; a tap shows the summary the fresh session opened with.
struct CapcomDividerRow: View {
    let label: String
    let summary: String
    @State private var open = false

    var body: some View {
        VStack(spacing: 4) {
            Button {
                if !summary.isEmpty { open.toggle() }
            } label: {
                HStack(spacing: 8) {
                    VStack { Divider() }
                    Text(label).font(.caption).foregroundStyle(.secondary).fixedSize()
                    VStack { Divider() }
                }
            }
            .buttonStyle(.plain)
            .accessibilityHint(summary.isEmpty ? "" : "Shows the summary")
            if open {
                Text(summary)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }
}

/// A card in the thread. An open approval card offers the hab's choices: Deny, Allow this
/// site (a web fetch with a URL), Allow (5.3). An answered one says how.
struct CapcomCardRow: View {
    let item: CapcomItem
    let thread: CapcomThread

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(item.text ?? "Capcom asks", systemImage: item.card?.type == "approval" ? "hand.raised" : "rectangle.on.rectangle")
                .font(.callout)
            if let a = item.answer {
                Text(CapcomCardChoice.answered(a))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else if item.card?.type == "approval", thread.session != nil {
                HStack {
                    ForEach(CapcomCardChoice.buttons(item), id: \.self) { c in
                        Button(c.title, role: c == .deny ? .destructive : nil) {
                            Task { await thread.answer(item, c.answer) }
                        }
                        .buttonStyle(.bordered)
                        .disabled(thread.answering.contains(item.id))
                    }
                }
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 12))
    }
}

/// "Capcom is typing" with three dots.
struct CapcomTypingMark: View {
    var body: some View {
        HStack(spacing: 6) {
            ProgressView().controlSize(.small)
            Text("Capcom is typing").font(.caption).foregroundStyle(.secondary)
        }
        .accessibilityElement(children: .combine)
    }
}

/// "Earlier threads": each other host's thread by its label, read-only.
struct CapcomEarlierSection: View {
    let earlier: [CapcomEarlierThread]
    let images: AttachmentImages?

    var body: some View {
        DisclosureGroup {
            ForEach(earlier) { e in
                DisclosureGroup(e.label) {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(e.thread.rows) { row in
                            CapcomRowView(row: row, thread: e.thread, images: nil, hostID: e.habID, showMark: false)
                        }
                    }
                }
                .font(.callout)
            }
        } label: {
            Label("Earlier threads", systemImage: "clock.arrow.circlepath").font(.callout)
        }
    }
}

/// The proposals Capcom filed that wait for review (6.4). Review itself is Logbook's
/// approval stage; this lists them.
struct CapcomProposalsSheet: View {
    let records: [LogbookRecord]
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            List(records, id: \.id) { r in
                VStack(alignment: .leading, spacing: 2) {
                    Text(r.title)
                    if let b = r.body, !b.isEmpty {
                        Text(b).font(.caption).foregroundStyle(.secondary).lineLimit(3)
                    }
                }
            }
            .navigationTitle("Proposals to review")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Done") { dismiss() } }
            }
        }
        .frame(minWidth: 360, minHeight: 300)
    }
}
