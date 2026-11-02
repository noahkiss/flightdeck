import FlightdeckShared
import HabAPI
import SwiftUI

/// The approval sheet (app.md 7.2): one request, its facts, the exact request,
/// the expiry, and Deny and Allow. Plans and questions from Claude show as
/// structured answers (landscape.md 4.4).
public struct ApprovalSheet: View {
    @Bindable var model: ApprovalSheetModel
    let openChat: (SessionTarget) -> Void
    let close: () -> Void
    @State private var picks: [Int: Set<String>] = [:]
    @State private var replyText = ""

    public init(model: ApprovalSheetModel, openChat: @escaping (SessionTarget) -> Void, close: @escaping () -> Void) {
        self.model = model
        self.openChat = openChat
        self.close = close
    }

    public var body: some View {
        NavigationStack {
            TimelineView(.periodic(from: .now, by: 1)) { _ in
                if let r = model.current {
                    content(r)
                } else {
                    ContentUnavailableView("Nothing waits for you", systemImage: "checkmark.circle")
                }
            }
            .navigationTitle(positionText)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Close", action: close)
                }
                if let r = model.current {
                    ToolbarItem(placement: .primaryAction) {
                        Button {
                            openChat(r.target)
                        } label: {
                            Label("Open chat", systemImage: "bubble.left.and.text.bubble.right")
                        }
                    }
                }
            }
        }
        .onChange(of: model.finished) { _, done in
            if done { close() }
        }
        .onChange(of: model.current?.id) { _, _ in
            picks = [:]
            replyText = ""
        }
        #if os(macOS)
        .frame(minWidth: 460, minHeight: 420)
        #endif
    }

    private var positionText: String {
        let p = model.position
        return p.count > 1 ? "\(p.index) of \(p.count)" : "Approval"
    }

    @ViewBuilder
    private func content(_ r: OpenRequest) -> some View {
        VStack(spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    header(r)
                    Text(r.title)
                        .font(.title3.weight(.semibold))
                        .fixedSize(horizontal: false, vertical: true)
                    Text(facts(r))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    requestBlock(r)
                    if let line = r.request?.contextLine, !line.isEmpty {
                        Text(line)
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .lineLimit(2)
                    }
                    status(r)
                }
                .padding()
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            Divider()
            buttons(r)
                .padding()
        }
    }

    private func header(_ r: OpenRequest) -> some View {
        HStack(spacing: 6) {
            Text(OpenRequest.harnessName(r.harness))
            Text("·")
            Text(r.kindText)
            Spacer()
            if let pct = r.contextPct {
                ProgressView(value: Double(min(pct, 100)), total: 100)
                    .frame(width: 60)
                    .accessibilityLabel("Context")
                Text("\(pct)%")
                    .foregroundStyle(pct >= 95 ? .red : pct >= 80 ? .orange : .secondary)
            }
        }
        .font(.caption)
        .foregroundStyle(.secondary)
    }

    private func facts(_ r: OpenRequest) -> String {
        var parts: [String] = []
        if let p = r.project { parts.append(p) } else { parts.append(r.sessionName) }
        parts.append(r.habLabel)
        if let m = r.model { parts.append(m) }
        if let s = r.since { parts.append(s.formatted(date: .omitted, time: .shortened)) }
        return parts.joined(separator: " · ")
    }

    @ViewBuilder
    private func requestBlock(_ r: OpenRequest) -> some View {
        switch r.form {
        case .command(let c):
            code(c)
            if let cwd = r.request?.cwd {
                Text(cwd).font(.caption.monospaced()).foregroundStyle(.secondary)
            }
        case .files(let fs):
            VStack(alignment: .leading, spacing: 4) {
                ForEach(fs, id: \.self) { Text($0).font(.callout.monospaced()) }
            }
        case .plan(let plan):
            // Agent text is Markdown, never a localization key.
            Text(RequestForm.planText(plan))
                .font(.callout)
                .textSelection(.enabled)
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 8))
        case .questions(let qs):
            ForEach(Array(qs.enumerated()), id: \.offset) { i, q in
                VStack(alignment: .leading, spacing: 6) {
                    if qs.count > 1 { Text(q.question).font(.callout.weight(.medium)) }
                    ForEach(q.options, id: \.label) { o in
                        Button {
                            toggle(i, o.label, multi: q.multiSelect)
                        } label: {
                            HStack(alignment: .firstTextBaseline) {
                                Image(systemName: (picks[i] ?? []).contains(o.label) ? "checkmark.circle.fill" : "circle")
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(o.label)
                                    if let d = o.description { Text(d).font(.caption).foregroundStyle(.secondary) }
                                }
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                    }
                }
            }
        case .raw(let s):
            code(s)
        case .none:
            if r.kind == .question {
                Text("Open the chat to answer.").foregroundStyle(.secondary)
            } else if r.kind == .auth {
                Text("Open the terminal to sign in.").foregroundStyle(.secondary)
            }
        }
    }

    private func code(_ s: String) -> some View {
        ScrollView(.horizontal) {
            Text(s)
                .font(.callout.monospaced())
                .textSelection(.enabled)
                .padding(10)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder
    private func status(_ r: OpenRequest) -> some View {
        switch model.phase {
        case .answeredElsewhere:
            Label("Answered elsewhere", systemImage: "checkmark.circle")
                .foregroundStyle(.secondary)
        case .expired:
            Label("Expired: answer in the terminal", systemImage: "clock.badge.xmark")
                .foregroundStyle(.orange)
        case .failed(let why):
            Label(why, systemImage: "exclamationmark.triangle")
                .foregroundStyle(.red)
                .font(.callout)
        case .sending:
            ProgressView()
        case .open:
            if let left = model.remaining {
                Text("Expires in \(Self.countdown(left))")
                    .font(.callout.monospacedDigit())
                    .foregroundStyle(left < 60 ? .orange : .secondary)
            }
            if r.offline {
                Text("The hab is offline.").foregroundStyle(.secondary)
            } else if !r.answerable && r.kind == .permission {
                Text("Needs you in the terminal.").foregroundStyle(.secondary)
            }
        }
    }

    @ViewBuilder
    private func buttons(_ r: OpenRequest) -> some View {
        switch model.phase {
        case .answeredElsewhere, .expired:
            HStack {
                if model.phase == .expired {
                    Button("Open terminal") { openChat(r.target) }
                        .buttonStyle(.bordered)
                }
                Spacer()
                Button(model.position.count > 1 ? "Next" : "Done") { model.next() }
                    .buttonStyle(.borderedProminent)
            }
        default:
            if case .questions(let qs) = r.form {
                Button {
                    let chosen = qs.indices.map { i in qs[i].options.map(\.label).filter { (picks[i] ?? []).contains($0) } }
                    Task { await model.reply(RequestForm.answerText(qs, chosen)) }
                } label: {
                    Text("Send answer").frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .disabled(!model.canAnswer || qs.indices.contains { (picks[$0] ?? []).isEmpty })
            } else {
                HStack(spacing: 12) {
                    Button(role: .destructive) {
                        Task { await model.deny() }
                    } label: {
                        Text(r.form.isPlan ? "Keep planning" : "Deny").frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.bordered)
                    Button {
                        Task { await model.allow() }
                    } label: {
                        Text(r.form.isPlan ? "Approve plan" : "Allow").frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                }
                .controlSize(.large)
                .disabled(!model.canAnswer)
            }
        }
    }

    private func toggle(_ i: Int, _ label: String, multi: Bool) {
        var s = picks[i] ?? []
        if s.contains(label) {
            s.remove(label)
        } else {
            if !multi { s = [] }
            s.insert(label)
        }
        picks[i] = s
    }

    /// "4m 12s", "38s".
    static func countdown(_ t: TimeInterval) -> String {
        let s = Int(t.rounded(.down))
        if s >= 3600 { return "\(s / 3600)h \(s % 3600 / 60)m" }
        if s >= 60 { return "\(s / 60)m \(s % 60)s" }
        return "\(s)s"
    }
}

/// Settings › Notifications: the two push settings and each hab's push status
/// (app.md 8.3, 8.4, 18.5).
public struct NotificationSettingsView: View {
    @Bindable var push: PushRegistrar
    let board: Board

    public init(push: PushRegistrar, board: Board) {
        self.push = push
        self.board = board
    }

    public var body: some View {
        Form {
            Section {
                Toggle("Show commands in notifications", isOn: $push.showCommands)
                Toggle("Notify when a turn finishes", isOn: $push.doneBanners)
            }
            if push.permission == .denied {
                Section {
                    Text("Notifications are off for Flightdeck in system Settings.")
                        .foregroundStyle(.secondary)
                }
            }
            if let e = push.tokenError {
                Section {
                    Text("No push token: \(e)").foregroundStyle(.secondary)
                }
            }
            Section("Habs") {
                ForEach(board.habs) { h in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(h.label)
                        if let s = push.status[h.id] {
                            if s == .notSetUp {
                                Text("Push is not set up on \(h.label). Approvals show while the app is open.")
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                                Link("Push setup guide", destination: PushRegistrar.setupGuide)
                                    .font(.caption)
                            } else {
                                Text(s.text).font(.caption).foregroundStyle(.secondary)
                            }
                        } else {
                            Text("Not connected").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        #if os(macOS)
        .formStyle(.grouped)
        #endif
    }
}
