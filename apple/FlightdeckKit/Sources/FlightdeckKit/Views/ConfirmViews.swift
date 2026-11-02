import HabAPI
import SwiftUI

extension EnvironmentValues {
    /// Confirmation cards across every hab (capcom.md 6.1); nil shows none.
    @Entry public var confirmations: ConfirmCenter? = nil
}

/// A list of confirmation cards, for the Capcom view (capcom.md 6.1).
public struct ConfirmCardList: View {
    let center: ConfirmCenter
    let cards: [ConfirmCard]

    public init(center: ConfirmCenter, cards: [ConfirmCard]) {
        self.center = center
        self.cards = cards
    }

    public var body: some View {
        VStack(spacing: 8) {
            ForEach(cards) { ConfirmCardView(center: center, card: $0) }
        }
    }
}

/// The cards of one session, above its chat: open ones in full, the last few
/// closed ones as one line each.
struct SessionConfirmCards: View {
    let target: SessionTarget
    @Environment(\.confirmations) private var center

    var body: some View {
        if let center, case let cards = center.cards(for: target), !cards.isEmpty {
            ScrollView {
                ConfirmCardList(center: center, cards: cards)
                    .padding(8)
            }
            .frame(maxHeight: 360)
            .fixedSize(horizontal: false, vertical: true)
            Divider()
        }
    }
}

/// One confirmation card (capcom.md 6.3): what will run, the owner's edits,
/// Confirm and Cancel, and the time left. A record shows no buttons; a closed
/// card shows one line with its outcome.
struct ConfirmCardView: View {
    let center: ConfirmCenter
    let card: ConfirmCard
    @State private var editing = false

    private var c: Confirmation { card.confirmation }

    var body: some View {
        if c.isClosed {
            closedLine
        } else {
            TimelineView(.periodic(from: .now, by: 1)) { ctx in
                VStack(alignment: .leading, spacing: 8) {
                    header
                    if editing, let d = center.draft(card), let s = c.start {
                        ConfirmEditor(center: center, card: card, draft: d, start: s)
                    } else {
                        facts
                    }
                    status(at: ctx.date)
                    if center.hasButtons(card) { buttons }
                }
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(.background.secondary, in: RoundedRectangle(cornerRadius: 10))
            }
        }
    }

    private var title: String {
        switch c.kind {
        case .setup: return c.record == true ? "Project set-up" : "Set up a project?"
        default: return c.record == true ? "Session start" : "Start a session?"
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
                .font(.headline)
            Text(byline)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private var byline: String {
        var parts: [String] = []
        if let f = c.from { parts.append("From \(f)") }
        parts.append(card.habLabel)
        if c.record == true { parts.append("Ran without asking") }
        return parts.joined(separator: " · ")
    }

    @ViewBuilder
    private var facts: some View {
        if let orig = c.start {
            let s = center.draft(card)?.applied(to: orig) ?? orig
            Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                row("Host", s.host)
                row("Project", s.project)
                row("Folder", s.folder, mono: true)
                row("Branch", s.branch)
                row("Harness", harnessName(s.harness))
                row("Model", s.model)
                row("Name", s.name)
            }
            .font(.callout)
            if let seed = s.seed, !seed.isEmpty {
                Text(seed)
                    .font(.callout)
                    .lineLimit(editing ? nil : 6)
                    .textSelection(.enabled)
                    .padding(8)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(.background, in: RoundedRectangle(cornerRadius: 6))
                    .accessibilityLabel("Seed: \(seed)")
            }
        } else if let s = c.setup {
            Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                row("Host", s.host)
                row("Repository", s.repository, mono: true)
                row("Branch", s.branch)
                row("Bootstrap", s.bootstrap, mono: true)
            }
            .font(.callout)
        }
    }

    @ViewBuilder
    private func row(_ label: String, _ value: String?, mono: Bool = false) -> some View {
        if let value, !value.isEmpty {
            GridRow {
                Text(label)
                    .foregroundStyle(.secondary)
                Text(value)
                    .font(mono ? .callout.monospaced() : .callout)
                    .textSelection(.enabled)
                    .lineLimit(2)
                    .truncationMode(.middle)
            }
        }
    }

    @ViewBuilder
    private func status(at date: Date) -> some View {
        switch center.phase(card) {
        case .open, .sending:
            if let r = center.remaining(card, at: date) {
                Text("Expires in \(ConfirmCenter.countdown(r))")
                    .font(.caption)
                    .foregroundStyle(r < 120 ? .orange : .secondary)
                    .monospacedDigit()
            }
        case .failed(let why):
            Text(why)
                .font(.caption)
                .foregroundStyle(.red)
        case .alreadyAnswered:
            Text("Already answered")
                .font(.caption)
                .foregroundStyle(.secondary)
        case .expired:
            Text("Expired: nothing runs")
                .font(.caption)
                .foregroundStyle(.secondary)
        case .offline:
            Text("Offline")
                .font(.caption)
                .foregroundStyle(.secondary)
        case .running, .record:
            Label("Running", systemImage: "hourglass")
                .font(.caption)
                .foregroundStyle(.secondary)
        case .closed:
            EmptyView()
        }
    }

    private var buttons: some View {
        let on = center.canAnswer(card)
        let sending = center.phase(card) == .sending
        return HStack {
            Button("Cancel", role: .cancel) {
                Task { await center.cancel(card) }
            }
            .disabled(!on)
            if c.start != nil {
                Button(editing ? "Done" : "Edit") { editing.toggle() }
                    .disabled(!on)
            }
            Spacer()
            if sending { ProgressView().controlSize(.small) }
            Button(center.isEdited(card) ? "Confirm with changes" : "Confirm") {
                Task {
                    await center.confirm(card)
                    editing = false
                }
            }
            .buttonStyle(.borderedProminent)
            .disabled(!on)
        }
        .buttonStyle(.bordered)
        .controlSize(.small)
    }

    private var closedLine: some View {
        HStack(spacing: 6) {
            Image(systemName: symbol)
                .foregroundStyle(c.state == .done ? .green : .secondary)
                .accessibilityHidden(true)
            Text(ConfirmCenter.outcome(c))
                .lineLimit(2)
            Spacer()
            if let s = c.start?.harness {
                Text(harnessName(s))
                    .foregroundStyle(.secondary)
            }
        }
        .font(.caption)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: 8))
    }

    private var symbol: String {
        switch c.state {
        case .done: return "checkmark.circle"
        case .failed, .unknown: return "exclamationmark.triangle"
        default: return "xmark.circle"
        }
    }
}

/// The edit fields of a start card: host with its folder, harness, model, seed.
struct ConfirmEditor: View {
    let center: ConfirmCenter
    let card: ConfirmCard
    @State var draft: ConfirmDraft
    let start: ConfirmStart

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            hostField
            TextField("Folder", text: $draft.folder)
                .font(.callout.monospaced())
                .autocorrectionDisabled()
            Picker("Harness", selection: $draft.harness) {
                ForEach(harnesses, id: \.self) { Text(harnessName($0)).tag($0) }
            }
            TextField("Model", text: $draft.model, prompt: Text(start.model ?? "Model"))
                .autocorrectionDisabled()
            Text("Seed")
                .font(.caption)
                .foregroundStyle(.secondary)
            TextEditor(text: $draft.seed)
                .font(.callout)
                .frame(minHeight: 80, maxHeight: 200)
                .scrollContentBackground(.hidden)
                .padding(4)
                .background(.background, in: RoundedRectangle(cornerRadius: 6))
            HStack {
                if let p = draft.problem(against: start) {
                    Text(p)
                        .font(.caption)
                        .foregroundStyle(.red)
                }
                Spacer()
                if center.isEdited(card) {
                    Button("Undo changes") {
                        center.resetDraft(card)
                        draft = ConfirmDraft(start)
                    }
                    .controlSize(.small)
                }
            }
        }
        .textFieldStyle(.roundedBorder)
        .onChange(of: draft) { _, d in center.setDraft(d, for: card) }
        .task { await center.loadTargets(card.habID) }
    }

    /// Known harnesses, plus the card's own if the hab sent another.
    private var harnesses: [String] {
        ConfirmDraft.harnesses.contains(start.harness) ? ConfirmDraft.harnesses : ConfirmDraft.harnesses + [start.harness]
    }

    @ViewBuilder
    private var hostField: some View {
        let targets = center.targets[card.habID] ?? []
        if targets.isEmpty {
            TextField("Host", text: $draft.host)
                .autocorrectionDisabled()
        } else {
            Picker("Host", selection: $draft.host) {
                if !targets.contains(where: { $0.host == start.host }) {
                    Text(start.host).tag(start.host)
                }
                ForEach(targets, id: \.host) { t in
                    Text(t.ready == false ? "\(t.host) (\(t.reason ?? "not ready"))" : t.host)
                        .tag(t.host)
                        .selectionDisabled(t.ready == false && t.host != start.host)
                }
            }
        }
    }
}
