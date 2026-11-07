import HabAPI
import SwiftUI

extension EnvironmentValues {
    /// Agent groups on every hab (groups.md 5.4); nil shows none.
    @Entry public var groups: GroupCenter? = nil
}

/// A group member's mark on its session row: the group's title.
struct GroupBadge: View {
    let title: String

    var body: some View {
        Label(title, systemImage: "person.3")
            .labelStyle(.titleAndIcon)
            .font(.caption2.weight(.medium))
            .lineLimit(1)
            .padding(.horizontal, 5)
            .padding(.vertical, 1)
            .background(Capsule().fill(Color.teal.opacity(0.15)))
            .foregroundStyle(Color.teal)
            .accessibilityLabel("Group \(title)")
    }
}

/// The status board's Groups section (groups.md 5.4), below the sessions.
struct BoardGroupsSection: View {
    let center: GroupCenter
    let now: Date

    var body: some View {
        let cards = center.boardCards
        if !cards.isEmpty {
            Section("Groups") {
                ForEach(cards) { GroupCardView(center: center, card: $0, now: now) }
            }
        }
    }
}

/// The groups a coordinator made, above its chat.
struct SessionGroupCards: View {
    let target: SessionTarget
    @Environment(\.groups) private var center

    var body: some View {
        if let center, case let cards = center.cards(coordinator: target), !cards.isEmpty {
            TimelineView(.periodic(from: .now, by: 30)) { ctx in
                ScrollView {
                    VStack(spacing: 8) {
                        ForEach(cards) { GroupCardView(center: center, card: $0, now: ctx.date) }
                    }
                    .padding(8)
                }
                .frame(maxHeight: 360)
                .fixedSize(horizontal: false, vertical: true)
            }
            Divider()
        }
    }
}

/// One group's card (groups.md 5.4): the header with its waits, the items, the members (a tap
/// opens the session), the lane, the reports waiting for the coordinator, and the actions.
struct GroupCardView: View {
    let center: GroupCenter
    let card: GroupCard
    let now: Date
    @State private var showMembers = true

    private var g: GroupRecord { card.group }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            header
            if !g.items.isEmpty { items }
            if !g.members.isEmpty { members }
            if let l = GroupText.lane(g) {
                Label(l, systemImage: "arrow.triangle.merge")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if !g.inbox.isEmpty { inbox }
            if card.isOver, !g.leftovers.isEmpty {
                Text("Left over: " + g.leftovers.joined(separator: ", "))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            if let e = center.errors[card.id] {
                Text(e)
                    .font(.caption)
                    .foregroundStyle(.red)
            }
            if !card.isOver { actions }
        }
        .padding(.vertical, 4)
        .opacity(card.online ? 1 : 0.6)
        .confirmationDialog("Close group \(g.title)?", isPresented: Binding(
            get: { center.closeAsk?.id == card.id }, set: { if !$0, center.closeAsk?.id == card.id { center.closeAsk = nil } }
        ), titleVisibility: .visible) {
            Button("Close group", role: .destructive) { center.confirmClose() }
            Button("Cancel", role: .cancel) { center.closeAsk = nil }
        } message: {
            Text(center.closeMessage)
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(alignment: .firstTextBaseline) {
                Text(g.title)
                    .font(.headline)
                    .lineLimit(2)
                Spacer(minLength: 8)
                Text(g.state.capitalized)
                    .font(.caption.weight(.medium))
                    .foregroundStyle(stateColor)
            }
            Text(byline)
                .font(.caption)
                .foregroundStyle(.secondary)
            if let p = GroupText.pause(g) {
                Text(p).font(.caption).foregroundStyle(.orange)
            }
            if let w = GroupText.waits(g) {
                Text(w).font(.caption).foregroundStyle(.orange)
            }
        }
    }

    private var byline: String {
        var parts = [card.habLabel]
        if let e = GroupText.elapsed(g, now: now) { parts.append(e) }
        parts.append(GroupText.counts(g))
        if !card.online { parts.append("offline") }
        return parts.joined(separator: " · ")
    }

    private var stateColor: Color {
        switch g.state {
        case "running": return .green
        case "paused", "confirming": return .orange
        case "dropped": return .red
        default: return .secondary
        }
    }

    private var items: some View {
        VStack(alignment: .leading, spacing: 4) {
            ForEach(g.items, id: \.id) { it in
                VStack(alignment: .leading, spacing: 1) {
                    HStack(alignment: .firstTextBaseline) {
                        Text(it.title)
                            .font(.callout)
                            .lineLimit(1)
                        Spacer(minLength: 6)
                        Text(it.state)
                            .font(.caption)
                            .foregroundStyle(it.state == "stalled" ? .orange : .secondary)
                    }
                    let detail = [it.lead, it.branch].compactMap { $0 }.filter { !$0.isEmpty }
                    if !detail.isEmpty {
                        Text(detail.joined(separator: " · "))
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    if let r = it.reason, !r.isEmpty {
                        Text(r)
                            .font(.caption)
                            .foregroundStyle(.orange)
                    }
                }
                .accessibilityElement(children: .combine)
            }
        }
    }

    private var members: some View {
        DisclosureGroup(isExpanded: $showMembers) {
            VStack(alignment: .leading, spacing: 4) {
                ForEach(g.members, id: \.name) { m in memberRow(m) }
            }
        } label: {
            Text("Members")
                .font(.caption.weight(.medium))
                .foregroundStyle(.secondary)
        }
    }

    private func memberRow(_ m: GroupMember) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Button {
                center.open(card, m)
            } label: {
                VStack(alignment: .leading, spacing: 1) {
                    HStack(spacing: 4) {
                        Text(m.name)
                            .font(.callout)
                            .lineLimit(1)
                        Text(m.role.rawValue)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    Text(memberDetail(m))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                    if let r = m.reason, !r.isEmpty {
                        Text(r).font(.caption).foregroundStyle(.orange)
                    }
                }
            }
            .buttonStyle(.plain)
            .disabled(center.target(card, m) == nil)
            Spacer(minLength: 6)
            Text(GroupText.memberState(m))
                .font(.caption)
                .foregroundStyle(m.state == "failed" ? .red : .secondary)
            if card.canRetry(m) {
                Button("Retry") { Task { await center.retry(card, m) } }
                    .buttonStyle(.bordered)
                    .controlSize(.mini)
                    .disabled(center.busy.contains(card.id))
            }
        }
    }

    private func memberDetail(_ m: GroupMember) -> String {
        var parts = [m.host, harnessName(m.harness)]
        if let model = m.model, !model.isEmpty { parts.append(model) }
        if let c = m.contextPct { parts.append("\(c)% context") }
        return parts.filter { !$0.isEmpty }.joined(separator: " · ")
    }

    private var inbox: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("\(g.inbox.count) report\(g.inbox.count == 1 ? "" : "s") wait for the coordinator")
                .font(.caption.weight(.medium))
                .foregroundStyle(.secondary)
            ForEach(Array(g.inbox.suffix(3).enumerated()), id: \.offset) { _, r in
                Text((r.from.map { "\($0): " } ?? "") + r.text)
                    .font(.caption)
                    .lineLimit(2)
            }
        }
    }

    private var actions: some View {
        HStack {
            if card.canPause {
                Button("Pause") { Task { await center.pause(card) } }
            }
            if card.canResume {
                Button("Resume") { Task { await center.resume(card) } }
            }
            if card.canClose {
                Button("Close", role: .destructive) { center.askClose(card) }
            }
            Spacer()
            if center.busy.contains(card.id) { ProgressView().controlSize(.small) }
        }
        .buttonStyle(.bordered)
        .controlSize(.small)
        .disabled(center.busy.contains(card.id))
    }
}

/// A group's plan on its confirmation card (groups.md 3.2): the task, each item, each member,
/// the lane target and the trust rule. In edit mode the owner changes hosts, harnesses, models
/// and names, drops an item or a member, and turns review off for an item.
struct GroupPlanCard: View {
    let center: ConfirmCenter
    let card: ConfirmCard
    @State var draft: GroupPlanDraft
    let group: ConfirmGroup
    let editing: Bool

    private var plan: GroupPlan { draft.plan }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 4) {
                row("Task", plan.title)
                row("Project", plan.project)
                row("Lands on", plan.lane?.target ?? "the default branch")
                if plan.lane?.fixInPlace == true { row("Review", "may fix in place") }
            }
            .font(.callout)
            if let b = plan.brief, !b.isEmpty {
                Text(b)
                    .font(.callout)
                    .lineLimit(editing ? nil : 4)
                    .textSelection(.enabled)
            }
            Text("Items").font(.caption.weight(.medium)).foregroundStyle(.secondary)
            ForEach(plan.items, id: \.id) { itemRow($0) }
            Text("Members").font(.caption.weight(.medium)).foregroundStyle(.secondary)
            ForEach(Array(plan.members.enumerated()), id: \.offset) { i, m in
                if editing { memberEditor(i, m) } else { memberLine(m) }
            }
            if !group.trust.isEmpty {
                Label(group.trust, systemImage: "checkmark.shield")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            ForEach(group.warnings ?? [], id: \.self) { w in
                Label(w, systemImage: "exclamationmark.triangle")
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
            if editing {
                HStack {
                    if let p = draft.problem {
                        Text(p).font(.caption).foregroundStyle(.red)
                    }
                    Spacer()
                    if center.isEdited(card) {
                        Button("Undo changes") {
                            center.resetDraft(card)
                            draft = GroupPlanDraft(group.plan)
                        }
                        .controlSize(.small)
                    }
                }
            }
        }
        .textFieldStyle(.roundedBorder)
        .onChange(of: draft) { _, d in center.setGroupDraft(d, for: card) }
    }

    @ViewBuilder
    private func row(_ label: String, _ value: String?) -> some View {
        if let value, !value.isEmpty {
            GridRow {
                Text(label).foregroundStyle(.secondary)
                Text(value).textSelection(.enabled).lineLimit(2)
            }
        }
    }

    private func itemTitle(_ id: String) -> String { plan.items.first { $0.id == id }?.title ?? id }

    private func itemRow(_ it: GroupPlanItem) -> some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 1) {
                Text(it.title).font(.callout)
                if let a = it.after, !a.isEmpty {
                    Text("after \(itemTitle(a))").font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 6)
            if editing {
                Toggle("Review", isOn: Binding(get: { GroupPlanDraft.reviewOn(it) }, set: { draft.setReview(it.id, on: $0) }))
                    .toggleStyle(.switch)
                    .controlSize(.mini)
                    .fixedSize()
                Button(role: .destructive) { draft.dropItem(it.id) } label: {
                    Image(systemName: "trash")
                }
                .buttonStyle(.borderless)
                .accessibilityLabel("Drop \(it.title)")
            } else {
                Text(GroupPlanDraft.reviewOn(it) ? "review on" : "review off")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private func memberLine(_ m: GroupPlanMember) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Text("\(m.role.rawValue) · \(itemTitle(m.item))").font(.callout)
            Text([m.host ?? card.habLabel, harnessName(m.harness), m.model ?? "", m.name ?? "name chosen by the hab", "new worktree"]
                .filter { !$0.isEmpty }.joined(separator: " · "))
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private func memberEditor(_ i: Int, _ m: GroupPlanMember) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("\(m.role.rawValue) · \(itemTitle(m.item))").font(.callout)
                Spacer()
                Button(role: .destructive) { draft.dropMember(i) } label: {
                    Image(systemName: "trash")
                }
                .buttonStyle(.borderless)
                .accessibilityLabel("Drop \(m.name ?? m.role.rawValue)")
            }
            TextField("Host", text: Binding(get: { m.host ?? "" }, set: { draft.setMember(i, host: $0) }),
                      prompt: Text(card.habLabel))
                .autocorrectionDisabled()
            Picker("Harness", selection: Binding(get: { m.harness }, set: { draft.setMember(i, harness: $0) })) {
                let hs = ConfirmDraft.harnesses.contains(m.harness) ? ConfirmDraft.harnesses : ConfirmDraft.harnesses + [m.harness]
                ForEach(hs, id: \.self) { Text(harnessName($0)).tag($0) }
            }
            TextField("Model", text: Binding(get: { m.model ?? "" }, set: { draft.setMember(i, model: $0) }))
                .autocorrectionDisabled()
            TextField("Name", text: Binding(get: { m.name ?? "" }, set: { draft.setMember(i, name: $0) }),
                      prompt: Text("Chosen by the hab"))
                .autocorrectionDisabled()
        }
        .padding(6)
        .background(.background, in: RoundedRectangle(cornerRadius: 6))
    }
}
