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
    /// The draft, when the session view holds it (the Capcom toolbar's New conversation reads it).
    private var heldDraft: Binding<String>?
    @State private var ownDraft = ""
    @State private var attachments: [DraftAttachment] = []
    /// Why a file was not attached, by the attach button or a drop (app.md 4.4).
    @State private var attachNotice: String?
    /// Files a Capcom composer could not attach as chips: they stay in the draft.
    @State private var keptFiles: Set<UUID> = []
    @Environment(\.sendFiles) private var sendFiles
    @Environment(\.approvals) private var approvals
    /// A line over the composer, such as the Capcom tab's pacing line.
    @Environment(\.chatAboveComposer) private var aboveComposer
    #if os(iOS)
    @Environment(\.horizontalSizeClass) private var sizeClass
    #endif

    /// The chat column's widest on iPad (app.md 11); a phone and the Mac use the full width.
    static let readableMax: CGFloat = 820

    private var readableWidth: CGFloat? {
        #if os(iOS)
        return sizeClass == .regular ? Self.readableMax : nil
        #else
        return nil
        #endif
    }

    public init(model: ChatModel, openTerminal: (() -> Void)? = nil, openVoice: (() -> Void)? = nil, focusComposer: Int = 0, draft: Binding<String>? = nil) {
        self.model = model
        self.openTerminal = openTerminal
        self.openVoice = openVoice
        self.focusComposer = focusComposer
        self.heldDraft = draft
    }

    private var draftBinding: Binding<String> { heldDraft ?? $ownDraft }

    /// The composer takes files now, as its attach button does (app.md 4.4).
    private var takesFiles: Bool {
        ChatDrop.takesFiles(ChatAnswer.composerMode(gate: model.gate, agent: model.context.agent, card: approvals?.card(for: model.target)))
    }

    private var draft: String {
        get { draftBinding.wrappedValue }
        nonmutating set { draftBinding.wrappedValue = newValue }
    }

    public var body: some View {
        Group {
            if let thread = model.thread {
                // The Capcom service (capcom-service.md 7.6): the thread replaces the transcript.
                CapcomThreadView(thread: thread, model: model)
            } else {
                ChatTranscript(model: model, openTerminal: openTerminal)
            }
        }
        // The banners and the composer float over the messages (app.md 4.2, 4.3): the messages
        // scroll under them, and the soft scroll edge effect keeps the bars legible, as in Messages.
        .scrollEdgeEffectStyle(.soft, for: .vertical)
        .safeAreaBar(edge: .top, spacing: 0) {
            VStack(spacing: 0) {
                ChatBanners(context: model.context, load: model.load, openTerminal: openTerminal)
                PinnedBanners(target: model.target)
            }
        }
        .safeAreaBar(edge: .bottom, spacing: 0) {
            // One glass container holds the whole bar (app.md 4.2): the pacing line, the Capcom
            // thread's lines and the composer.
            ComposerView(model: model, draft: draftBinding, attachments: $attachments, attachNotice: $attachNotice,
                         openTerminal: openTerminal, openVoice: openVoice, focusRequest: focusComposer, above: aboveComposer)
        }
        // Drop a file on the chat (app.md 4.4): it joins the composer as the attach button's do.
        .modifier(ChatDropTarget(enabled: takesFiles, takesFiles: { takesFiles }, draft: draftBinding, attachments: $attachments, notice: $attachNotice))
        // A readable column on a wide iPad: messages and composer stay under 820 pt, centred.
        .frame(maxWidth: readableWidth)
        .frame(maxWidth: .infinity)
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

extension EnvironmentValues {
    /// A view the chat shows over its composer; nil shows none.
    @Entry var chatAboveComposer: AnyView? = nil
}

/// The way out of the keyboard on a phone (app.md 4.2), as in Messages: dragging the
/// messages down takes the keyboard with the finger. The thread bounces even when it is too
/// short to scroll, so the drag works on an empty or short thread too. With the keyboard gone
/// the tab bar shows again.
struct DismissesKeyboard: ViewModifier {
    func body(content: Content) -> some View {
        #if os(iOS)
        content
            .scrollBounceBehavior(.always, axes: .vertical)
            .scrollDismissesKeyboard(.interactively)
        #else
        content
        #endif
    }
}

#if os(iOS) && DEBUG
/// Debug builds: ends editing in whichever field holds the keyboard.
@MainActor
func hideKeyboard() {
    UIApplication.shared.sendAction(#selector(UIResponder.resignFirstResponder), to: nil, from: nil, for: nil)
}
#endif

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
                if let r = context.round { RoundBadge(name: r) }
                Image(systemName: harnessSymbol(context.harness))
                    .accessibilityHidden(true)
                Text(subtitle)
                    .lineLimit(1)
            }
            .font(.caption)
            .foregroundStyle(.secondary)
        }
    }

    private var subtitle: String { Self.line(context) }

    /// "Claude Code · workbench · opus", with "offline" when it is; `round` leads with
    /// "Round <name>" for a bar that has no badge.
    static func line(_ context: ChatContext, round: Bool = false) -> String {
        var parts: [String] = []
        if round, let r = context.round { parts.append("Round \(r)") }
        parts += [harnessName(context.harness), context.hostLabel]
        if let m = context.model { parts.append(m) }
        if let s = startModeLabel(context.startMode) { parts.append(s) }
        if !context.online { parts.append("offline") }
        return parts.filter { !$0.isEmpty }.joined(separator: " · ")
    }
}

/// The words the session details show for a start mode other than ask (harness-adapters.md,
/// Start mode). Nil for ask or none; an unknown word shows as it is.
func startModeLabel(_ mode: String?) -> String? {
    switch mode {
    case nil, ""?, "ask"?: return nil
    case "auto"?: return "Auto mode"
    case "bypass"?: return "Bypass permissions"
    case let x?: return "Start mode \(x)"
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
        let offline = !context.online && context.status != .stopped
        let warning = context.agent?.warning != nil
        if offline || warning {
            // Status capsules on plain glass, as the system shows a status over content; only the
            // Terminal button is interactive glass.
            GlassEffectContainer(spacing: 6) {
                VStack(spacing: 6) {
                    if offline {
                        banner(offlineText, symbol: "wifi.slash", tint: .primary)
                            .glassEffect(.regular, in: .capsule)
                    }
                    if warning {
                        HStack(spacing: 6) {
                            banner("Chat view may be incomplete", symbol: "exclamationmark.triangle", tint: .orange)
                                .glassEffect(.regular, in: .capsule)
                            if let openTerminal {
                                Button("Terminal", action: openTerminal)
                                    .font(.caption.weight(.semibold))
                                    .buttonStyle(.glass)
                            }
                        }
                    }
                }
            }
            .padding(.horizontal, 12)
            .padding(.top, 6)
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
            .font(.footnote.weight(.semibold))
            .foregroundStyle(tint)
            .lineLimit(2)
            .padding(.horizontal, 14)
            .padding(.vertical, 8)
    }
}

/// The scrolling list of messages. It opens at the tail; older pages load
/// when the top row appears. Hidden kinds fold into one line (app.md 4.11).
struct ChatTranscript: View {
    let model: ChatModel
    var openTerminal: (() -> Void)?
    /// Folded lines the owner opened, by row id.
    @State private var openFolds: Set<String> = []

    var body: some View {
        let items = model.items
        let rows = ChatFold.rows(items, hidden: model.show?.hidden(for: model.target) ?? [])
        let placed = ContentPlacement.of(items: model.content.items, messages: model.messages, rowIDs: Set(items.map(\.id)))
        ChatScroll(rowIDs: rows.map(\.id), readKey: Self.readKey(model.target), sent: model.outgoing.count,
                   messageIDs: Set(items.filter(\.isMessage).map(\.id)), rotors: ChatRotors.of(rows)) { divider in
            if model.hasOlder {
                HStack {
                    Spacer()
                    if model.loadingOlder { ProgressView().controlSize(.small) } else { Text("Earlier messages").font(.caption).foregroundStyle(.secondary) }
                    Spacer()
                }
                .onAppear { Task { await model.loadOlder() } }
            }
            ForEach(rows) { row in
                if row.id == divider { UnreadDivider() }
                switch row {
                case .item(let item):
                    ChatItemView(item: item, images: model.images, actions: actions)
                        .id(item.id)
                    ForEach(placed.byRow[item.id] ?? [], id: \.id) { c in
                        InlineContentView(model: model.content, item: c)
                    }
                case .folded(let id, let hidden, let summary):
                    let open = Binding(get: { openFolds.contains(id) }, set: { if $0 { openFolds.insert(id) } else { openFolds.remove(id) } })
                    FoldedStepsView(summary: summary, items: hidden, open: open, images: model.images)
                        .id(id)
                    if openFolds.contains(id) {
                        ForEach(hidden.flatMap { placed.byRow[$0.id] ?? [] }, id: \.id) { c in
                            InlineContentView(model: model.content, item: c)
                        }
                    }
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
        }
        .overlay { placeholder }
    }

    /// A thread's key for its read mark.
    static func readKey(_ t: SessionTarget) -> String { "\(t.habID)|\(t.session ?? t.pane ?? "")" }

    private var actions: MessageActions { MessageActions(model: model) }


    @ViewBuilder private var placeholder: some View {
        // Shown content and the modified-files card fill the list too: no placeholder over them.
        if model.messages.isEmpty && model.outgoing.isEmpty && model.content.items.isEmpty {
            switch model.load {
            case .loading:
                ProgressView()
                    .allowsHitTesting(false)
            case .waiting, .unavailable, .failed, .ready:
                if let s = ChatEmptyState.of(model.load, context: model.context) {
                    // Only the buttons take touches: a drag anywhere else reaches the messages,
                    // so it can put the keyboard away (4.2).
                    ContentUnavailableView {
                        Label(s.title, systemImage: s.symbol)
                            .allowsHitTesting(false)
                    } description: {
                        Text(s.line)
                            .allowsHitTesting(false)
                    } actions: {
                        if s.retry {
                            Button("Try again") { Task { await model.loadTail() } }
                                .buttonStyle(.bordered)
                        }
                        if let openTerminal {
                            Button("Open terminal", action: openTerminal)
                                .buttonStyle(.borderedProminent)
                        }
                    }
                }
            }
        }
    }
}

/// One chat row.
public struct ChatItemView: View {
    let item: ChatItem
    /// Loads attachment thumbnails; nil shows them as file chips.
    var images: AttachmentImages?
    /// The message menu's lookups; nil reads the row as drawn.
    var actions: MessageActions?

    public init(item: ChatItem) {
        self.item = item
    }

    init(item: ChatItem, images: AttachmentImages?, actions: MessageActions? = nil) {
        self.item = item
        self.images = images
        self.actions = actions
    }

    public var body: some View {
        let id = item.id
        row
            .messageMenu(id: id, content: MessageMenu.content(item), current: actions.map { a in { a.current(id) } })
    }

    @ViewBuilder private var row: some View {
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
                            .padding(.horizontal, 12)
                            .padding(.vertical, 8)
                            .background(sender == nil ? Color.accentColor.opacity(0.18) : Color.purple.opacity(0.15), in: RoundedRectangle(cornerRadius: 14))
                            // A turn starts here: VoiceOver's Headings rotor steps by turn (app.md 22.1).
                            .accessibilityAddTraits(.isHeader)
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
                            .padding(.horizontal, 12)
                            .padding(.vertical, 8)
                            .background(Color.accentColor.opacity(0.18), in: RoundedRectangle(cornerRadius: 14))
                            .accessibilityAddTraits(.isHeader)
                    }
                }
            }
        case .agent(_, let text, let summary, let sender):
            VStack(alignment: .leading, spacing: 6) {
                if let sender {
                    Label("From \(sender)", systemImage: "arrow.turn.down.right")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
                MarkdownBody(text: text)
                if let summary { SpokenSummaryCaption(text: summary) }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            // The whole row takes the long press, not only its words.
            .contentShape(Rectangle())
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

/// The reply's spoken summary as a quiet caption, folded (app.md 13.7).
struct SpokenSummaryCaption: View {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    let text: String
    @State private var open = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Button {
                withAnimation(gateMotion(.easeOut(duration: 0.15), reduce: reduceMotion)) { open.toggle() }
            } label: {
                Label("Spoken summary", systemImage: open ? "chevron.down" : "speaker.wave.2")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
            .pointerHover()
            .disclosureState(open)
            if open {
                Text(text)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
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
        // The long-press menu of a message on its way (app.md 4.12): Retry where it failed.
        .contextMenu {
            switch outgoing.status {
            case .notDelivered, .failed:
                Button("Retry", systemImage: "arrow.clockwise") { model.retry(outgoing.id) }
            case .queued:
                Button("Cancel", systemImage: "xmark") { model.cancel(outgoing.id) }
            default:
                EmptyView()
            }
            if !outgoing.text.isEmpty {
                Button("Copy", systemImage: "doc.on.doc") { copyToPasteboard(outgoing.text) }
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

extension View {
    /// A line in the composer's bar (a notice, the line after an answer) on a glass capsule, so it
    /// stays legible over the messages that scroll under the bar (app.md 4.2).
    func composerNote() -> some View {
        padding(.horizontal, 12)
            .padding(.vertical, 6)
            .glassEffect(.regular, in: .capsule)
    }
}

/// Where the composer's dictation writes (app.md 13.4): heard words follow the draft as it was
/// when the dictation began.
struct DictationDraft: Equatable {
    /// The draft before the dictation began.
    private var base = ""
    private var session = 0

    /// Starts a dictation over `draft`; returns its session.
    mutating func begin(_ draft: String) -> Int {
        session += 1
        base = draft
        return session
    }

    /// The draft with `heard` after it, or nil when that dictation no longer writes.
    func draft(heard: String, session s: Int) -> String? {
        guard s == session else { return nil }
        return ComposerView.join(base, heard)
    }

    /// Ends the dictation: its later words write nothing. Send calls it, so words heard after the
    /// draft went out never write the sent message back.
    mutating func end() {
        session += 1
        base = ""
    }
}

/// The composer (app.md 4.2): text, a terminal button, Stop while working, and Send.
struct ComposerView: View {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    let model: ChatModel
    @Binding var draft: String
    @Binding var attachments: [DraftAttachment]
    /// Why a file was not attached; the chat holds it, so a drop can say it too.
    @Binding var attachNotice: String?
    let openTerminal: (() -> Void)?
    var openVoice: (() -> Void)?
    var focusRequest = 0
    /// A line over the composer, in its glass container (the Capcom tab's pacing line).
    var above: AnyView?
    @FocusState private var focused: Bool
    @Environment(\.voiceKit) private var voiceKit
    @Environment(\.habConnections) private var habs
    @Environment(\.approvals) private var approvals
    @Environment(\.dynamicTypeSize) private var typeSize
    @State private var dictation: DictationModel?
    /// Where heard words go: after the draft as it was when dictation started.
    @State private var dictationDraft = DictationDraft()
    /// The dictation session that writes into the draft now.
    @State private var dictationSession = 0
    @State private var micDenied = false
    /// Why dictation did not start or wrote nothing.
    @State private var dictationNotice: String?
    /// Ties the field, Send and Stop together, so Send morphs out of the field (app.md 4.2).
    @Namespace private var glass
    /// Where the caret is, so Shift-Return writes the new line there.
    @State private var selection: TextSelection?
    /// Haptics (app.md 4.12): a light tap on send, a success when an answer lands, a warning when
    /// a message fails.
    @State private var sentTaps = 0

    var body: some View {
        let gate = model.gate
        let card = approvals?.card(for: model.target)
        let mode = ChatAnswer.composerMode(gate: gate, agent: model.context.agent, card: card)
        // Glass as in Messages (app.md 4.2): a glass attach button, the field on glass with the
        // microphone inside it, and Send as the one prominent control. One container holds every
        // glass piece of the bar (cards, lines, chips, the field row), so they sample together
        // and Send morphs in beside the field.
        GlassEffectContainer(spacing: Self.spacing) {
        VStack(alignment: .leading, spacing: Self.spacing) {
            if let above { above }
            if let thread = model.thread { CapcomThreadBar(thread: thread) }
            if gate == .blocked, let a = model.context.agent {
                if a.kind == .trust {
                    TrustCardView(model: model, openTerminal: openTerminal)
                } else {
                    ScrollView {
                        RequestArea(agent: a, model: model, openTerminal: openTerminal)
                    }
                    // A tall request (large text, many options) scrolls instead of squeezing the chat and the composer.
                    .scrollBounceBehavior(.basedOnSize)
                    .frame(maxHeight: typeSize.isAccessibilitySize ? 300 : 440)
                    .fixedSize(horizontal: false, vertical: true)
                }
            } else if let note = model.answerNote {
                Label(note, systemImage: "checkmark.circle.fill")
                    .font(.footnote.weight(.medium))
                    .composerNote()
                    .accessibilityAddTraits(.updatesFrequently)
            }
            if gate == .noAgent {
                HStack {
                    Text(model.context.sessionExited ? "The session has exited." : "No agent runs in this session.")
                        .font(.callout)
                    if model.context.sessionExited {
                        Button("Resume") { Task { await model.resume() } }
                    }
                }
                .composerNote()
            }
            if let n = model.notice {
                Text(n)
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(.red)
                    .composerNote()
            }
            if let n = dictationNotice ?? dictation?.notice ?? attachNotice {
                Text(n)
                    .font(.footnote.weight(.medium))
                    .composerNote()
            }
            if model.context.isCapcom && !model.pending.isEmpty {
                CapcomChipStrip(model: model)
            }
            if !attachments.isEmpty {
                DraftStrip(attachments: $attachments)
            }
            Group {
                if typeSize.isAccessibilitySize {
                    // At accessibility sizes the field takes its own row; the buttons go below it (app.md 4.11).
                    VStack(alignment: .leading, spacing: Self.spacing) {
                        field(mode, gate: gate)
                        HStack(spacing: Self.spacing) {
                            leadingButtons(mode)
                            Spacer(minLength: 0)
                            trailingButtons(mode)
                        }
                    }
                } else {
                    HStack(alignment: .bottom, spacing: Self.spacing) {
                        leadingButtons(mode)
                        field(mode, gate: gate)
                        trailingButtons(mode)
                    }
                }
            }
        }
        }
        .animation(gateMotion(.smooth(duration: 0.25), reduce: reduceMotion), value: hasDraft)
        .sensoryFeedback(.impact(weight: .light), trigger: sentTaps)
        .sensoryFeedback(.success, trigger: model.answerNote) { _, new in new != nil }
        .sensoryFeedback(.warning, trigger: model.failedCount) { old, new in new > old }
        .padding(.horizontal, 10)
        .padding(.top, 6)
        .padding(.bottom, 10)
        .onChange(of: focusRequest) { _, _ in focused = true }
        // "Tell it what to do" on a request card (app.md 4.11): the denial landed; the composer
        // takes the owner's instructions.
        .onChange(of: model.composeRequest) { _, _ in focused = true }
        #if DEBUG
        // Debug builds only: `-FlightdeckFocusComposer YES` raises the keyboard at launch, so
        // the keyboard's way out can be screenshotted (app.md 4.2).
        .task {
            guard UserDefaults.standard.bool(forKey: "FlightdeckFocusComposer") else { return }
            try? await Task.sleep(for: .seconds(2))
            focused = true
            // `-FlightdeckThenHideKeyboard YES`: later, put the keyboard away again.
            guard UserDefaults.standard.bool(forKey: "FlightdeckThenHideKeyboard") else { return }
            try? await Task.sleep(for: .seconds(4))
            #if os(iOS)
            hideKeyboard()
            #endif
        }
        #endif
        .onChange(of: dictation?.liveText) { _, live in
            guard let live, dictation?.phase == .listening,
                  let text = dictationDraft.draft(heard: live, session: dictationSession) else { return }
            draft = text
        }
        .onDisappear {
            if let d = dictation { Task { await d.cancel() } }
        }
    }

    private var hasDraft: Bool {
        !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !attachments.isEmpty || model.sendsContextAlone
    }

    /// The gap between the composer's glass pieces.
    static let spacing: CGFloat = 8

    @ViewBuilder private func leadingButtons(_ mode: ChatAnswer.ComposerMode) -> some View {
        AttachButton(attachments: $attachments) { attachNotice = $0 }
            .disabled(mode == .off || mode == .answer)
            .glassEffect(.regular.interactive(), in: .circle)
            .glassEffectID("attach", in: glass)
    }

    /// The field on glass, with the microphone inside it while there is nothing to send.
    private func field(_ mode: ChatAnswer.ComposerMode, gate: SendGate) -> some View {
        HStack(alignment: .bottom, spacing: 0) {
            TextField(ChatAnswer.placeholder(mode, gate: gate, agent: model.context.agent, capcom: model.context.isCapcom), text: $draft, selection: $selection, axis: .vertical)
                .lineLimit(1...8)
                .textFieldStyle(.plain)
                .focused($focused)
                .disabled(mode == .off)
                // The same gap whether or not the microphone shows, so the text never jumps.
                .padding(.horizontal, 16)
                .padding(.vertical, 11)
                .frame(minHeight: 44)
                .accessibilityLabel("Message")
                // A hardware keyboard, as in Messages (app.md 4.2): Return sends; Shift-Return and
                // Option-Return start a new line. The on-screen keyboard's Return is not a key
                // press, so it still writes a new line on a phone.
                .onKeyPress(.return, phases: .down) { press in
                    switch ComposerKeys.returnKey(press.modifiers) {
                    case .send:
                        // With a CJK input method a plain Return is the input method's (M3 in
                        // app.md 4.12); Cmd-Return still sends.
                        guard press.modifiers.contains(.command) || ComposerKeys.returnSends(languages: ComposerKeys.inputLanguages) else {
                            return .ignored
                        }
                        send()
                    case .newline:
                        (draft, selection) = ComposerKeys.insertingNewline(draft, at: selection)
                    }
                    return .handled
                }
                #if os(macOS)
                // The field's own Return, should a key press not reach the handler above.
                .onSubmit { send() }
                #endif
            if showsDictation {
                DictationButton(dictation: dictation, start: startDictation, stop: stopDictation)
                    .disabled(mode == .off && dictation == nil)
                    .simultaneousGesture(LongPressGesture(minimumDuration: 0.5).onEnded { _ in
                        if dictation == nil, VoiceButton.allowed(model) { openVoice?() }
                    })
                    .alert("Microphone access is off", isPresented: $micDenied) {
                        Button("Open Settings") { openPrivacySettings() }
                        Button("Cancel", role: .cancel) {}
                    }
            }
        }
        .glassEffect(.regular.interactive(), in: .rect(cornerRadius: 22))
        .glassEffectID("field", in: glass)
    }

    /// The microphone shows while the draft is empty, or while it dictates.
    private var showsDictation: Bool {
        voiceKit != nil && (!hasDraft || dictation != nil)
    }

    /// Stop while the agent works; then Send once the draft holds something. Dictate sits in the field.
    @ViewBuilder private func trailingButtons(_ mode: ChatAnswer.ComposerMode) -> some View {
        if (model.context.agentStatus == .working || model.thread?.typing == true) && model.context.online {
            Button {
                Task { await model.interrupt() }
            } label: {
                Image(systemName: "stop.fill")
                    .font(.body.weight(.semibold))
                    .frame(width: 30, height: 30)
            }
            .buttonStyle(.glass)
            .buttonBorderShape(.circle)
            .foregroundStyle(.red)
            .glassEffectID("stop", in: glass)
            .accessibilityLabel("Stop")
        }
        if hasDraft || voiceKit == nil {
            Button { send() } label: {
                Image(systemName: "arrow.up")
                    .font(.body.weight(.bold))
                    .frame(width: 30, height: 30)
            }
            .buttonStyle(.glassProminent)
            .buttonBorderShape(.circle)
            .glassEffectID("send", in: glass)
            .disabled(mode == .off || !hasDraft)
            .keyboardShortcut(.return, modifiers: .command)
            .accessibilityLabel(mode == .answer ? "Send answer" : "Send")
            .contextMenu {
                // The Capcom thread only (capcom-service.md 10.1): run it on the small model
                // where an older hab's usage governor still makes user turns wait.
                if model.context.isCapcomService {
                    Button("Send now", systemImage: "bolt") { send(now: true) }
                }
                // While the agent works (app.md 4.2): wait for the turn, or stop it and send.
                if !model.context.isCapcomService, model.gate == .afterTurn {
                    Button("Send After This Turn", systemImage: "clock") { send() }
                    Button("Stop and Send Now", systemImage: "stop.circle") { send(interrupt: true) }
                }
            }
        }
    }

    private func send(now: Bool = false, interrupt: Bool = false) {
        let card = approvals?.card(for: model.target)
        if ChatAnswer.composerMode(gate: model.gate, agent: model.context.agent, card: card) == .answer {
            // A question with options is open: the text answers it (app.md 4.11).
            let text = draft.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !text.isEmpty, let approvals, let r = card?.request else { return }
            Task {
                if await approvals.reply(model.target, text: text) {
                    model.noteAnswer(ChatAnswer.note(allow: nil, reply: text, request: r))
                    draft = ""
                }
            }
            return
        }
        guard model.takesMessage, hasDraft else { return }
        model.send(draft, attachments: attachments, sendNow: now)
        // A refusal (a file over the limit) keeps the draft as it was.
        guard model.notice == nil else { return }
        sentTaps += 1
        // The message waits for the turn; the interrupt ends the turn, and it goes out.
        if interrupt { Task { await model.interrupt() } }
        // Send while dictating ends the dictation: its later words would write the sent text back.
        dictationDraft.end()
        if let d = dictation {
            dictation = nil
            Task { await d.cancel() }
        }
        draft = ""
        selection = nil
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
        let session = dictationDraft.begin(draft)
        dictationSession = session
        dictationNotice = nil
        Task {
            await d.start()
            // A refused microphone or no engine: the button works again.
            if d.phase == .idle, dictation === d {
                dictationNotice = d.notice
                dictation = nil
            }
        }
    }

    /// Stops; the engine's text replaces the heard words. A send meanwhile ended the
    /// dictation: the text then writes nothing.
    private func stopDictation() {
        guard let d = dictation else { return }
        let session = dictationSession
        Task {
            let text = await d.stop()
            guard dictation === d else { return }
            if let text, let full = dictationDraft.draft(heard: text, session: session) {
                draft = full
            } else if text == nil {
                dictationNotice = d.notice
            }
            dictation = nil
            focused = true
        }
    }

    nonisolated static func join(_ base: String, _ heard: String) -> String {
        guard !heard.isEmpty else { return base }
        guard !base.isEmpty else { return heard }
        return base.hasSuffix(" ") || base.hasSuffix("\n") ? base + heard : base + " " + heard
    }

}

/// The pending request, shown above the composer while the agent is blocked (app.md 4.2,
/// 4.11). It answers in place: Deny and Allow (Keep planning and Approve plan for a plan), a
/// question's options as buttons, free text from the composer. A tap on the title opens the
/// approval sheet with the full request.
struct RequestArea: View {
    let agent: AgentState
    let model: ChatModel
    var openTerminal: (() -> Void)?
    @Environment(\.approvals) private var approvals
    /// Multiple-choice picks, per question.
    @State private var picks: [Int: Set<String>] = [:]

    private var target: SessionTarget { model.target }

    var body: some View {
        let state = approvals?.card(for: target)
        VStack(alignment: .leading, spacing: 10) {
            if let state {
                Button {
                    approvals?.open(target)
                } label: {
                    facts(state.request)
                }
                .buttonStyle(.plain)
                .pointerHover()
                .accessibilityHint("Opens the full request")
                actions(state)
            } else {
                facts(nil)
                terminalLine(agent.kind == .question ? "Answer it in the terminal." : agent.kind == .auth ? "Sign in in the terminal." : "Answer it in the terminal.")
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        // On glass, since the messages scroll under the composer bar (app.md 4.2).
        .glassEffect(.regular.tint(.orange.opacity(0.15)), in: .rect(cornerRadius: 16))
        #if DEBUG
        // Debug builds only: `-FlightdeckAutoAnswer allow` allows the open permission after 3 s,
        // and `-FlightdeckAutoAnswer option` picks a question's first option, through the card's
        // own path, to show the line that follows an answer.
        .task(id: agent.request?.id) {
            let how = UserDefaults.standard.string(forKey: "FlightdeckAutoAnswer")
            guard how == "allow" || how == "option" else { return }
            try? await Task.sleep(for: .seconds(3))
            guard let r = approvals?.card(for: target)?.request, r.answerable, !r.form.isPlan else { return }
            if case .questions(let qs) = r.form {
                if how == "option", let o = qs.first?.options.first { reply(o.label, r) }
            } else if how == "allow" {
                answer(allow: true, r)
            }
        }
        #endif
    }

    private var title: String {
        if let t = agent.request?.title, !t.isEmpty { return t }
        if let tool = agent.request?.tool { return "Allow \(tool)?" }
        switch agent.kind {
        case .question?: return "\(OpenRequest.harnessName(agent.harness ?? "")) asks a question"
        case .auth?: return "The agent needs a sign-in"
        default: return "The agent waits for permission"
        }
    }

    private func facts(_ r: OpenRequest?) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .firstTextBaseline) {
                Text(r?.title ?? title)
                    .font(.headline)
                    .frame(maxWidth: .infinity, alignment: .leading)
                Image(systemName: "chevron.up")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
                    .accessibilityHidden(true)
            }
            if case .questions(let qs)? = r?.form, qs.count == 1, let h = qs[0].header, !h.isEmpty {
                Text(h).font(.subheadline).foregroundStyle(.secondary)
            }
            if let c = agent.request?.command, !c.isEmpty {
                Text(c)
                    .font(.callout.monospaced())
                    .lineLimit(6)
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(.background, in: RoundedRectangle(cornerRadius: 8))
            } else if let f = agent.request?.files, !f.isEmpty {
                Text(f.joined(separator: "\n"))
                    .font(.callout.monospaced())
                    .lineLimit(6)
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(.background, in: RoundedRectangle(cornerRadius: 8))
            }
            if let cwd = agent.request?.cwd, !cwd.isEmpty {
                Text("Runs in \(cwd) on \(r?.habLabel ?? model.context.hostLabel)")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
    }

    @ViewBuilder
    private func actions(_ state: ApprovalCenter.CardState) -> some View {
        let r = state.request
        if let f = state.failure {
            Text(ChatAnswer.failureText(r, hint: f))
                .font(.footnote)
                .foregroundStyle(.red)
        }
        if state.expired {
            terminalLine("Expired: answer it in the terminal.")
        } else if state.sending {
            ProgressView().frame(maxWidth: .infinity, minHeight: 44)
        } else if case .questions(let qs) = r.form {
            questions(qs, state)
            if state.failure != nil, let openTerminal {
                Button("Open terminal", action: openTerminal).buttonStyle(.bordered)
            }
        } else if r.answerable {
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 8) { answerButtons(r, state) }
                VStack(spacing: 8) { answerButtons(r, state) }
            }
            if !r.form.isPlan {
                // Deny, then the composer takes what the agent should do instead (app.md 4.11).
                Button {
                    answer(allow: false, r) { model.askForInstructions() }
                } label: {
                    Label("Tell it what to do", systemImage: "text.bubble")
                        .frame(maxWidth: .infinity, minHeight: 28)
                }
                .buttonStyle(.bordered)
                .disabled(!state.canAnswer)
                .accessibilityHint("Denies, then opens the message field")
            }
        } else if r.offline {
            Text("\(r.habLabel) is offline.").font(.footnote).foregroundStyle(.secondary)
        } else {
            terminalLine(r.kind == .auth ? "Sign in in the terminal." : "Answer it in the terminal.")
        }
    }

    /// Deny, Allow (Always allow joins only when the request offers it; none does yet).
    @ViewBuilder
    private func answerButtons(_ r: OpenRequest, _ state: ApprovalCenter.CardState) -> some View {
        Button(role: .destructive) {
            answer(allow: false, r)
        } label: {
            Text(r.form.isPlan ? "Keep planning" : "Deny").frame(maxWidth: .infinity, minHeight: 32)
        }
        .buttonStyle(.bordered)
        .controlSize(.large)
        .disabled(!state.canAnswer)
        Button {
            answer(allow: true, r)
        } label: {
            Text(r.form.isPlan ? "Approve plan" : "Allow").frame(maxWidth: .infinity, minHeight: 32)
        }
        .buttonStyle(.borderedProminent)
        .controlSize(.large)
        .disabled(!state.canAnswer)
    }

    /// A question's options as buttons: one question with single choice answers on the tap;
    /// otherwise taps pick, then Send answer.
    @ViewBuilder
    private func questions(_ qs: [RequestForm.Question], _ state: ApprovalCenter.CardState) -> some View {
        let direct = qs.count == 1 && !qs[0].multiSelect
        ForEach(Array(qs.enumerated()), id: \.offset) { i, q in
            VStack(alignment: .leading, spacing: 6) {
                if qs.count > 1 {
                    Text(q.question).font(.subheadline.weight(.semibold))
                }
                ForEach(q.options, id: \.label) { o in
                    let picked = picks[i]?.contains(o.label) == true
                    Button {
                        if direct {
                            reply(o.label, state.request)
                        } else {
                            var p = picks[i] ?? []
                            if q.multiSelect {
                                if picked { p.remove(o.label) } else { p.insert(o.label) }
                            } else {
                                p = [o.label]
                            }
                            picks[i] = p
                        }
                    } label: {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            if !direct {
                                Image(systemName: picked ? "checkmark.circle.fill" : "circle")
                                    .accessibilityHidden(true)
                            }
                            VStack(alignment: .leading, spacing: 2) {
                                Text(o.label).font(.body.weight(.semibold))
                                if let d = o.description, !d.isEmpty {
                                    Text(d).font(.footnote).foregroundStyle(.secondary)
                                }
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .padding(.horizontal, 12)
                        .padding(.vertical, 10)
                        .frame(minHeight: 44)
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .pointerHover()
                    .background(.background, in: RoundedRectangle(cornerRadius: 12))
                    .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(picked ? Color.accentColor : Color.clear, lineWidth: 2))
                    .disabled(!state.canAnswer)
                    .accessibilityAddTraits(picked ? .isSelected : [])
                    .accessibilityHint(direct ? "Answers with this option" : "Picks this option")
                }
            }
        }
        if !direct {
            Button {
                let chosen = qs.indices.map { i in qs[i].options.map(\.label).filter { (picks[i] ?? []).contains($0) } }
                reply(RequestForm.answerText(qs, chosen), state.request)
            } label: {
                Text("Send answer").frame(maxWidth: .infinity, minHeight: 32)
            }
            .buttonStyle(.borderedProminent)
            .controlSize(.large)
            .disabled(!state.canAnswer || qs.indices.contains { (picks[$0] ?? []).isEmpty })
        }
    }

    private func terminalLine(_ text: String) -> some View {
        ViewThatFits(in: .horizontal) {
            HStack {
                Text(text).font(.footnote).foregroundStyle(.secondary)
                Spacer()
                if let openTerminal { Button("Open terminal", action: openTerminal).buttonStyle(.bordered) }
            }
            VStack(alignment: .leading, spacing: 6) {
                Text(text).font(.footnote).foregroundStyle(.secondary)
                if let openTerminal { Button("Open terminal", action: openTerminal).buttonStyle(.bordered) }
            }
        }
    }

    private func answer(allow: Bool, _ r: OpenRequest, then: (@MainActor () -> Void)? = nil) {
        guard let approvals else { return }
        Task {
            if await approvals.answer(target, allow: allow) {
                model.noteAnswer(ChatAnswer.note(allow: allow, request: r))
                then?()
            }
        }
    }

    private func reply(_ text: String, _ r: OpenRequest) {
        guard let approvals else { return }
        Task {
            if await approvals.reply(target, text: text) {
                picks = [:]
                model.noteAnswer(ChatAnswer.note(allow: nil, reply: text, request: r))
            }
        }
    }
}
