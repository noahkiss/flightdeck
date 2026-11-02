import HabAPI
import SwiftUI

/// The chat view of one session (app.md 4): the header, the messages, and the composer.
public struct ChatView: View {
    @Bindable var model: ChatModel
    /// Opens the terminal view, from the composer's button. Nil hides it.
    var openTerminal: (() -> Void)?
    /// Opens voice mode, from a long press on the microphone. Nil: no voice mode.
    var openVoice: (() -> Void)?
    /// A change focuses the composer (voice mode's Keyboard control).
    var focusComposer = 0
    @State private var draft = ""
    @State private var attachments: [DraftAttachment] = []
    /// Files a Capcom composer could not attach as chips: they stay in the draft.
    @State private var keptFiles: Set<UUID> = []
    @Environment(\.sendFiles) private var sendFiles

    public init(model: ChatModel, openTerminal: (() -> Void)? = nil, openVoice: (() -> Void)? = nil, focusComposer: Int = 0) {
        self.model = model
        self.openTerminal = openTerminal
        self.openVoice = openVoice
        self.focusComposer = focusComposer
    }

    public var body: some View {
        VStack(spacing: 0) {
            ChatBanners(context: model.context, load: model.load, openTerminal: openTerminal)
            SessionConfirmCards(target: model.target)
            ChatTranscript(model: model)
            Divider()
            ComposerView(model: model, draft: $draft, attachments: $attachments, openTerminal: openTerminal, openVoice: openVoice, focusRequest: focusComposer)
        }
        .onAppear {
            model.start()
            // A file sent here from another app waits in the composer (app.md 4.4).
            attachments += sendFiles?.take(for: model.target) ?? []
        }
        .onChange(of: sendFiles?.handed) { _, _ in
            attachments += sendFiles?.take(for: model.target) ?? []
        }
        // Text "New conversation" carried here (capcom.md 8.2).
        .onChange(of: model.handoff, initial: true) { _, text in
            guard let text else { return }
            model.handoff = nil
            draft = draft.isEmpty ? text : draft + "\n" + text
        }
        // A Capcom composer attaches files as context chips (capcom.md 8.1).
        // A refused file goes back to the draft, with the hab's reason under the composer.
        .onChange(of: attachments, initial: true) { _, files in
            let fresh = files.filter { !keptFiles.contains($0.id) }
            guard model.context.isCapcom, !fresh.isEmpty else { return }
            attachments.removeAll { f in fresh.contains { $0.id == f.id } }
            Task {
                for f in fresh where !(await model.attachFile(f)) {
                    keptFiles.insert(f.id)
                    attachments.append(f)
                }
            }
        }
        .onDisappear { model.stop() }
    }
}

/// The header line: harness and host, with the state.
public struct ChatHeader: View {
    let context: ChatContext

    public init(context: ChatContext) {
        self.context = context
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 1) {
            Text(context.title)
                .font(.headline)
                .lineLimit(1)
            HStack(spacing: 4) {
                Image(systemName: harnessSymbol(context.harness))
                    .accessibilityHidden(true)
                Text(subtitle)
                    .lineLimit(1)
            }
            .font(.caption)
            .foregroundStyle(.secondary)
        }
    }

    private var subtitle: String {
        var parts = [harnessName(context.harness), context.hostLabel]
        if let m = context.model { parts.append(m) }
        if !context.online { parts.append("offline") }
        return parts.filter { !$0.isEmpty }.joined(separator: " · ")
    }
}

func harnessName(_ h: String?) -> String {
    switch h {
    case "claude"?: return "Claude Code"
    case "codex"?: return "Codex"
    case "opencode"?: return "opencode"
    case let x?: return x
    case nil: return "Shell"
    }
}

func harnessSymbol(_ h: String?) -> String {
    switch h {
    case "claude"?: return "sparkle"
    case "codex"?: return "chevron.left.forwardslash.chevron.right"
    case "opencode"?: return "curlybraces"
    default: return "terminal"
    }
}

/// Offline and degraded banners (app.md 4.3).
struct ChatBanners: View {
    let context: ChatContext
    let load: ChatLoad
    let openTerminal: (() -> Void)?

    var body: some View {
        VStack(spacing: 0) {
            if !context.online && context.status != .stopped {
                banner(offlineText, symbol: "wifi.slash", tint: .secondary)
            }
            if context.agent?.warning != nil {
                HStack {
                    banner("Chat view may be incomplete", symbol: "exclamationmark.triangle", tint: .orange)
                    if let openTerminal {
                        Button("Terminal", action: openTerminal)
                            .font(.caption)
                            .padding(.trailing, 12)
                    }
                }
            }
        }
    }

    private var offlineText: String {
        switch context.status {
        case .connecting: return "Reconnecting"
        case .updateNeeded(let side): return side == .app ? "Update the app to reach this hab" : "Update the hab"
        default:
            return context.lostAt.map { "Offline since \($0.formatted(date: .omitted, time: .shortened))" } ?? "Offline"
        }
    }

    private func banner(_ text: String, symbol: String, tint: Color) -> some View {
        Label(text, systemImage: symbol)
            .font(.caption)
            .foregroundStyle(tint)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            .background(.bar)
    }
}

/// Where the list scrolls when its rows change.
public enum ScrollFollow: Equatable {
    case none
    case toBottom
    /// Keep this row where it was: older rows went in above it.
    case keep(String)

    /// The first and last row ids before and after a change.
    public struct Ends: Equatable {
        public var first: String?
        public var last: String?
        public init(first: String?, last: String?) {
            self.first = first
            self.last = last
        }
    }

    /// A prepend keeps the old first row in place. A new last row follows
    /// only while the owner is at the bottom. A first load or a replaced
    /// conversation goes to the bottom.
    public static func action(from old: Ends, to new: Ends, atBottom: Bool) -> ScrollFollow {
        if old.last == nil { return new.last == nil ? .none : .toBottom }
        let firstChanged = old.first != new.first
        let lastChanged = old.last != new.last
        if firstChanged && !lastChanged, let f = old.first { return .keep(f) }
        if firstChanged && lastChanged { return .toBottom }
        if lastChanged && atBottom { return .toBottom }
        return .none
    }
}

/// The scrolling list of messages. It opens at the tail; older pages load
/// when the top row appears.
struct ChatTranscript: View {
    let model: ChatModel
    @State private var atBottom = true

    var body: some View {
        let items = model.items
        let placed = ContentPlacement.of(items: model.content.items, messages: model.messages, rowIDs: Set(items.map(\.id)))
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    if model.hasOlder {
                        HStack {
                            Spacer()
                            if model.loadingOlder { ProgressView().controlSize(.small) } else { Text("Earlier messages").font(.caption).foregroundStyle(.secondary) }
                            Spacer()
                        }
                        .onAppear { Task { await model.loadOlder() } }
                    }
                    ForEach(items) { item in
                        ChatItemView(item: item, images: model.images)
                            .id(item.id)
                        ForEach(placed.byRow[item.id] ?? [], id: \.id) { c in
                            InlineContentView(model: model.content, item: c)
                        }
                    }
                    ForEach(placed.trailing, id: \.id) { c in
                        InlineContentView(model: model.content, item: c)
                    }
                    ModifiedFilesCard(model: model.content)
                    ForEach(model.outgoing) { o in
                        OutgoingView(outgoing: o, model: model)
                            .id("out-\(o.id)")
                    }
                    Color.clear.frame(height: 1).id(Self.bottom)
                        .onAppear { atBottom = true }
                        .onDisappear { atBottom = false }
                }
                .padding(12)
            }
            .defaultScrollAnchor(.bottom)
            .overlay { placeholder }
            .onChange(of: ScrollFollow.Ends(first: items.first?.id, last: items.last?.id)) { old, new in
                switch ScrollFollow.action(from: old, to: new, atBottom: atBottom) {
                case .none: break
                case .toBottom: proxy.scrollTo(Self.bottom, anchor: .bottom)
                case .keep(let id): proxy.scrollTo(id, anchor: .top)
                }
            }
            .onChange(of: model.outgoing.count) { old, new in
                // The owner just sent: show it.
                if new > old { proxy.scrollTo(Self.bottom, anchor: .bottom) }
            }
        }
    }

    static let bottom = "chat-bottom"

    @ViewBuilder private var placeholder: some View {
        // Shown content and the modified-files card fill the list too: no placeholder over them.
        if model.messages.isEmpty && model.outgoing.isEmpty && model.content.items.isEmpty {
            switch model.load {
            case .loading:
                ProgressView()
            case .waiting:
                ContentUnavailableView("Waiting for the first message", systemImage: "text.bubble")
            case .unavailable(let hint):
                ContentUnavailableView("No chat", systemImage: "text.bubble", description: Text(hint))
            case .failed(let e):
                ContentUnavailableView("Chat did not load", systemImage: "exclamationmark.bubble", description: Text(e))
            case .ready:
                ContentUnavailableView("No messages", systemImage: "text.bubble")
            }
        }
    }
}

/// One chat row.
public struct ChatItemView: View {
    let item: ChatItem
    /// Loads attachment thumbnails; nil shows them as file chips.
    var images: AttachmentImages?

    public init(item: ChatItem) {
        self.item = item
    }

    init(item: ChatItem, images: AttachmentImages?) {
        self.item = item
        self.images = images
    }

    public var body: some View {
        switch item {
        case .user(_, let text, let sender, let att):
            HStack {
                Spacer(minLength: 40)
                VStack(alignment: .trailing, spacing: 3) {
                    if let sender {
                        Label("From \(sender)", systemImage: "arrow.turn.down.right")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                    if !att.isEmpty {
                        AttachmentThumbRow(ids: att, images: images)
                    }
                    if !text.isEmpty || att.isEmpty {
                        Text(text)
                            .textSelection(.enabled)
                            .padding(.horizontal, 12)
                            .padding(.vertical, 8)
                            .background(sender == nil ? Color.accentColor.opacity(0.18) : Color.purple.opacity(0.15), in: RoundedRectangle(cornerRadius: 14))
                    }
                }
            }
        case .capcomUser(_, let context, let text, let att):
            HStack {
                Spacer(minLength: 40)
                VStack(alignment: .trailing, spacing: 3) {
                    ForEach(context, id: \.self) { c in
                        Label(c.title.isEmpty ? "\(c.kind) \(c.ref)" : c.title, systemImage: CapcomChip.symbol(CapcomContextKind(rawValue: c.kind)))
                            .font(.caption)
                            .lineLimit(1)
                            .padding(.horizontal, 8)
                            .padding(.vertical, 4)
                            .background(.quaternary.opacity(0.6), in: Capsule())
                    }
                    if !att.isEmpty {
                        AttachmentThumbRow(ids: att, images: images)
                    }
                    if !text.isEmpty {
                        Text(text)
                            .textSelection(.enabled)
                            .padding(.horizontal, 12)
                            .padding(.vertical, 8)
                            .background(Color.accentColor.opacity(0.18), in: RoundedRectangle(cornerRadius: 14))
                    }
                }
            }
        case .agent(_, let blocks, let sender):
            VStack(alignment: .leading, spacing: 6) {
                if let sender {
                    Label("From \(sender)", systemImage: "arrow.turn.down.right")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
                MarkdownView(blocks: blocks)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        case .tool(_, let card):
            ToolCardView(card: card)
        case .event(_, let event, let text):
            EventLine(event: event, text: text)
        case .plan(_, let steps, let latest):
            PlanCard(steps: steps, latest: latest)
        case .handOff(_, let card):
            HandOffCardView(card: card)
        case .proposal(_, let line):
            ProposalLineView(line: line)
        case .unsupported(_, let kind):
            EventLine(event: .unknown, text: "Unsupported message (\(kind))")
        }
    }
}

/// Markdown blocks, rendered with native text.
struct MarkdownView: View {
    let blocks: [MarkdownBlock]

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach(Array(blocks.enumerated()), id: \.offset) { _, b in
                block(b)
            }
        }
        .textSelection(.enabled)
    }

    @ViewBuilder private func block(_ b: MarkdownBlock) -> some View {
        switch b {
        case .paragraph(let t):
            Text(inline(t))
        case .heading(let level, let t):
            Text(inline(t)).font(level <= 1 ? .title3.bold() : level == 2 ? .headline : .subheadline.bold())
        case .code(_, let t):
            ScrollView(.horizontal, showsIndicators: false) {
                Text(t)
                    .font(.callout.monospaced())
                    .fixedSize(horizontal: true, vertical: false)
                    .padding(8)
            }
            .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
        case .item(let marker, let level, let t):
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(marker).foregroundStyle(.secondary)
                Text(inline(t))
            }
            .padding(.leading, CGFloat(level) * 16)
        case .quote(let t):
            Text(inline(t))
                .foregroundStyle(.secondary)
                .padding(.leading, 10)
                .overlay(alignment: .leading) { Rectangle().fill(.tertiary).frame(width: 3) }
        case .rule:
            Divider()
        }
    }

    private func inline(_ t: String) -> AttributedString {
        (try? AttributedString(markdown: t, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace))) ?? AttributedString(t)
    }
}

/// A tool call card, folded by default (app.md 4.1).
struct ToolCardView: View {
    let card: ToolCard
    @State private var open = false
    @State private var showAll = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button {
                withAnimation(.easeOut(duration: 0.15)) { open.toggle() }
            } label: {
                HStack(spacing: 6) {
                    Image(systemName: open ? "chevron.down" : "chevron.right")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .frame(width: 10)
                    statusIcon
                    Text(card.name).font(.callout.weight(.medium))
                    if let t = card.target {
                        Text(t)
                            .font(.callout.monospaced())
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    Spacer(minLength: 0)
                    if card.status != .done {
                        Text(ChatRender.statusWord(card.status))
                            .font(.caption)
                            .foregroundStyle(statusColor)
                    }
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("\(card.name) \(card.target ?? ""), \(ChatRender.statusWord(card.status))")
            .accessibilityHint(open ? "Folds the tool call" : "Shows the tool call")
            if open {
                VStack(alignment: .leading, spacing: 8) {
                    detail
                    if let o = card.output, !o.isEmpty {
                        output(o)
                    }
                    if !card.children.isEmpty {
                        VStack(alignment: .leading, spacing: 10) {
                            ForEach(card.children) { ChatItemView(item: $0) }
                        }
                        .padding(.leading, 8)
                        .overlay(alignment: .leading) { Rectangle().fill(.quaternary).frame(width: 2) }
                    }
                }
                .padding(.top, 8)
                .padding(.leading, 16)
            }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 7)
        .background(.quaternary.opacity(card.oneLine && !open ? 0.25 : 0.45), in: RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder private var detail: some View {
        switch card.detail {
        case .command(let c):
            Text("$ " + c)
                .font(.callout.monospaced())
                .textSelection(.enabled)
        case .diff(let lines):
            VStack(alignment: .leading, spacing: 0) {
                ForEach(Array(lines.enumerated()), id: \.offset) { _, l in
                    Text((l.kind == .removed ? "- " : "+ ") + l.text)
                        .font(.caption.monospaced())
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .background(l.kind == .removed ? Color.red.opacity(0.12) : Color.green.opacity(0.12))
                }
            }
            .textSelection(.enabled)
        case .input(let j):
            Text(j)
                .font(.caption.monospaced())
                .textSelection(.enabled)
        case .none:
            EmptyView()
        }
    }

    private func output(_ o: String) -> some View {
        let lines = o.components(separatedBy: "\n")
        let limit = 12
        let shown = showAll || lines.count <= limit ? o : lines.prefix(limit).joined(separator: "\n")
        return VStack(alignment: .leading, spacing: 4) {
            Text(shown)
                .font(.caption.monospaced())
                .foregroundStyle(.secondary)
                .textSelection(.enabled)
            if lines.count > limit && !showAll {
                Button("Show all \(lines.count) lines") { showAll = true }
                    .font(.caption)
            }
            if card.outputTruncated {
                Text("The hab sent the first part of the output.")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
        }
    }

    private var statusColor: Color {
        switch card.status {
        case .error: return .red
        case .denied: return .orange
        case .running: return .blue
        default: return .secondary
        }
    }

    @ViewBuilder private var statusIcon: some View {
        switch card.status {
        case .running: ProgressView().controlSize(.mini)
        case .error: Image(systemName: "xmark.circle.fill").foregroundStyle(.red).font(.caption)
        case .denied: Image(systemName: "hand.raised.fill").foregroundStyle(.orange).font(.caption)
        default: Image(systemName: "checkmark.circle").foregroundStyle(.secondary).font(.caption)
        }
    }
}

struct EventLine: View {
    let event: ChatEvent
    let text: String

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: symbol)
                .accessibilityHidden(true)
            Text(text)
                .lineLimit(3)
        }
        .font(.caption)
        .foregroundStyle(.secondary)
        .frame(maxWidth: .infinity, alignment: .center)
        .padding(.vertical, 2)
    }

    private var symbol: String {
        switch event {
        case .compaction: return "arrow.down.right.and.arrow.up.left"
        case .permission: return "hand.raised"
        case .interrupt: return "stop.circle"
        case .command: return "slash.circle"
        case .notice: return "info.circle"
        case .reset: return "arrow.clockwise"
        default: return "questionmark.circle"
        }
    }
}

/// A plan checklist (app.md 4.5).
struct PlanCard: View {
    let steps: [PlanStep]
    let latest: Bool

    var body: some View {
        let done = steps.filter { $0.status == .done }.count
        VStack(alignment: .leading, spacing: 6) {
            Label(latest ? "Plan · \(done) of \(steps.count)" : "Earlier plan · \(done) of \(steps.count)", systemImage: "checklist")
                .font(.callout.weight(.medium))
                .foregroundStyle(latest ? .primary : .secondary)
            if latest {
                ForEach(Array(steps.enumerated()), id: \.offset) { _, s in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Image(systemName: s.status == .done ? "checkmark.square" : s.status == .inProgress ? "arrow.right.square" : "square")
                            .foregroundStyle(s.status == .inProgress ? Color.accentColor : .secondary)
                        Text(s.text)
                            .strikethrough(s.status == .done)
                            .foregroundStyle(s.status == .done ? .secondary : .primary)
                            .fontWeight(s.status == .inProgress ? .medium : .regular)
                    }
                    .font(.callout)
                }
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 8))
    }
}

/// A message the owner sent that the transcript does not show yet.
struct OutgoingView: View {
    let outgoing: Outgoing
    let model: ChatModel

    var body: some View {
        HStack {
            Spacer(minLength: 40)
            VStack(alignment: .trailing, spacing: 3) {
                if !outgoing.attachments.isEmpty {
                    HStack(spacing: 6) {
                        ForEach(outgoing.attachments) { a in
                            if let img = DraftThumbs.image(a) {
                                Image(decorative: img, scale: 1).resizable().scaledToFill()
                                    .frame(width: 64, height: 64)
                                    .clipShape(RoundedRectangle(cornerRadius: 8))
                            } else {
                                Label(a.name, systemImage: "doc").font(.caption)
                            }
                        }
                    }
                    .opacity(0.8)
                }
                if !outgoing.context.isEmpty {
                    CapcomSentChips(items: outgoing.context, hostID: model.context.hostID)
                }
                if !outgoing.text.isEmpty {
                    Text(SpokenText.hidingSummaryRequest(outgoing.text))
                        .padding(.horizontal, 12)
                        .padding(.vertical, 8)
                        .background(Color.accentColor.opacity(0.10), in: RoundedRectangle(cornerRadius: 14))
                        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color.accentColor.opacity(0.3), style: StrokeStyle(lineWidth: 1, dash: [4, 3])))
                }
                HStack(spacing: 8) {
                    Text(statusText)
                        .foregroundStyle(statusColor)
                    switch outgoing.status {
                    case .queued:
                        Button("Cancel") { model.cancel(outgoing.id) }
                    case .notDelivered, .failed:
                        Button("Retry") { model.retry(outgoing.id) }
                        Button("Dismiss") { model.dismiss(outgoing.id) }
                    default:
                        EmptyView()
                    }
                }
                .font(.caption)
                .buttonStyle(.borderless)
            }
        }
    }

    private var statusText: String {
        switch outgoing.status {
        case .queued: return "Sends after this turn"
        case .sending: return Self.uploadText(outgoing) ?? "Sending"
        case .sent: return "Sent"
        case .delivered: return "Delivered"
        case .notDelivered: return "Not delivered"
        case .failed(let e): return "Not sent: \(e)"
        }
    }

    /// "Uploading 2 of 3" while files go up; nil once they are all on the hab.
    static func uploadText(_ o: Outgoing) -> String? {
        guard o.uploaded.count < o.attachments.count else { return nil }
        let n = o.attachments.count
        let a = o.attachments[o.uploaded.count]
        let pct = a.data.isEmpty ? 0 : min(100, o.uploadedBytes * 100 / a.data.count)
        return n == 1 ? "Uploading \(pct)%" : "Uploading \(o.uploaded.count + 1) of \(n), \(pct)%"
    }

    private var statusColor: Color {
        switch outgoing.status {
        case .notDelivered, .failed: return .red
        default: return .secondary
        }
    }
}

/// The composer (app.md 4.2): text, a terminal button, Stop while working, and Send.
struct ComposerView: View {
    let model: ChatModel
    @Binding var draft: String
    @Binding var attachments: [DraftAttachment]
    let openTerminal: (() -> Void)?
    var openVoice: (() -> Void)?
    var focusRequest = 0
    @FocusState private var focused: Bool
    @Environment(\.voiceKit) private var voiceKit
    @Environment(\.habConnections) private var habs
    @State private var dictation: DictationModel?
    /// The draft before dictation started; heard words follow it.
    @State private var dictationBase = ""
    @State private var micDenied = false
    /// Why dictation did not start or wrote nothing.
    @State private var dictationNotice: String?
    /// Why a file was not attached.
    @State private var attachNotice: String?

    var body: some View {
        let gate = model.gate
        VStack(alignment: .leading, spacing: 6) {
            if gate == .blocked, let a = model.context.agent {
                PendingRequestView(agent: a)
            }
            if gate == .noAgent {
                HStack {
                    Text(model.context.sessionExited ? "The session has exited." : "No agent runs in this session.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    Spacer()
                    if model.context.sessionExited {
                        Button("Resume") { Task { await model.resume() } }
                    }
                }
            }
            if let n = model.notice {
                Text(n)
                    .font(.caption)
                    .foregroundStyle(.red)
            }
            if let n = dictationNotice ?? dictation?.notice ?? attachNotice {
                Text(n)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if model.context.isCapcom && !model.pending.isEmpty {
                CapcomChipStrip(model: model, newConversation: newConversation)
            }
            if !attachments.isEmpty {
                DraftStrip(attachments: $attachments)
            }
            HStack(alignment: .bottom, spacing: 8) {
                AttachButton(attachments: $attachments) { attachNotice = $0 }
                    .disabled(!gate.allowsSend)
                if let openTerminal {
                    Button(action: openTerminal) {
                        Image(systemName: "terminal")
                    }
                    .buttonStyle(.borderless)
                    .accessibilityLabel("Open the terminal")
                }
                if voiceKit != nil {
                    DictationButton(dictation: dictation, start: startDictation, stop: stopDictation)
                        .disabled(!gate.allowsSend && dictation == nil)
                        .simultaneousGesture(LongPressGesture(minimumDuration: 0.5).onEnded { _ in
                            if dictation == nil, VoiceButton.allowed(model) { openVoice?() }
                        })
                        .alert("Microphone access is off", isPresented: $micDenied) {
                            Button("Open Settings") { openPrivacySettings() }
                            Button("Cancel", role: .cancel) {}
                        }
                }
                TextField(placeholder(gate), text: $draft, axis: .vertical)
                    .lineLimit(1...8)
                    .textFieldStyle(.plain)
                    .focused($focused)
                    .disabled(!gate.allowsSend)
                    .padding(.horizontal, 10)
                    .padding(.vertical, 7)
                    .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 16))
                    #if os(macOS)
                    .onSubmit(send)
                    #endif
                if model.context.agentStatus == .working && model.context.online {
                    Button {
                        Task { await model.interrupt() }
                    } label: {
                        Image(systemName: "stop.circle.fill").font(.title2)
                    }
                    .buttonStyle(.borderless)
                    .foregroundStyle(.red)
                    .accessibilityLabel("Stop")
                }
                Button(action: send) {
                    Image(systemName: "arrow.up.circle.fill").font(.title2)
                }
                .buttonStyle(.borderless)
                .disabled(!gate.allowsSend || (draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && attachments.isEmpty && !model.sendsContextAlone))
                .keyboardShortcut(.return, modifiers: .command)
                .accessibilityLabel("Send")
            }
        }
        .padding(10)
        .onChange(of: focusRequest) { _, _ in focused = true }
        .onChange(of: dictation?.liveText) { _, live in
            guard let live, dictation?.phase == .listening else { return }
            draft = Self.join(dictationBase, live)
        }
        .onDisappear {
            if let d = dictation { Task { await d.cancel() } }
        }
    }

    /// "New conversation" beside the chips (capcom.md 8.2): the chips and the draft move to a
    /// fresh Capcom session.
    private var newConversation: (() -> Void)? {
        guard let center = model.capcom else { return nil }
        return {
            let text = draft
            Task {
                if await center.newConversation(from: model, draft: text) { draft = "" }
            }
        }
    }

    private func send() {
        guard model.gate.allowsSend else { return }
        model.send(draft, attachments: attachments)
        // A refusal (a file over the limit) keeps the draft as it was.
        guard model.notice == nil else { return }
        draft = ""
        attachments = []
        attachNotice = nil
    }

    /// Dictation writes into the draft as words are heard; they stay editable (app.md 13.4).
    private func startDictation() {
        guard let kit = voiceKit, dictation == nil else { return }
        if VoiceAudio.permissionDenied {
            micDenied = true
            return
        }
        let d = kit.dictation(connection: habs.find(model.target.habID), session: model.target.session)
        dictation = d
        dictationBase = draft
        dictationNotice = nil
        Task {
            await d.start()
            // A refused microphone or no engine: the button works again.
            if d.phase == .idle {
                dictationNotice = d.notice
                dictation = nil
            }
        }
    }

    /// Stops; the engine's text replaces the heard words.
    private func stopDictation() {
        guard let d = dictation else { return }
        Task {
            if let text = await d.stop() {
                draft = Self.join(dictationBase, text)
            } else {
                dictationNotice = d.notice
            }
            dictation = nil
            focused = true
        }
    }

    static func join(_ base: String, _ heard: String) -> String {
        guard !heard.isEmpty else { return base }
        guard !base.isEmpty else { return heard }
        return base.hasSuffix(" ") || base.hasSuffix("\n") ? base + heard : base + " " + heard
    }

    private func placeholder(_ g: SendGate) -> String {
        switch g {
        case .now: return "Message"
        case .afterTurn: return "Message (sends after this turn)"
        case .blocked: return "Answer the request first"
        case .noAgent: return "No agent"
        case .offline: return "Offline"
        }
    }
}

/// The pending request, shown above the composer while the agent is blocked.
struct PendingRequestView: View {
    let agent: AgentState

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Label(title, systemImage: agent.kind == .question ? "questionmark.bubble" : agent.kind == .auth ? "person.badge.key" : "hand.raised")
                .font(.callout.weight(.medium))
                .foregroundStyle(.orange)
            if let c = agent.request?.command {
                Text(c)
                    .font(.caption.monospaced())
                    .lineLimit(4)
            }
            if let cwd = agent.request?.cwd {
                Text(cwd)
                    .font(.caption2.monospaced())
                    .foregroundStyle(.secondary)
            }
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.orange.opacity(0.1), in: RoundedRectangle(cornerRadius: 8))
    }

    private var title: String {
        if let t = agent.request?.title, !t.isEmpty { return t }
        if let tool = agent.request?.tool { return "Allow \(tool)?" }
        switch agent.kind {
        case .question?: return "The agent asks a question"
        case .auth?: return "The agent needs a sign-in"
        default: return "The agent waits for permission"
        }
    }
}
