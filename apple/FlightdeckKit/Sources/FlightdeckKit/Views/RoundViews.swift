import HabAPI
import SwiftUI

extension EnvironmentValues {
    @Entry public var rounds: RoundCenter? = nil
}

/// The round badge on a run session's row and header (rounds.md 10.3).
struct RoundBadge: View {
    let name: String

    var body: some View {
        Label(name, systemImage: "clock.arrow.circlepath")
            .labelStyle(.titleAndIcon)
            .font(.caption2.weight(.medium))
            .lineLimit(1)
            .padding(.horizontal, 5)
            .padding(.vertical, 1)
            .background(Capsule().fill(Color.accentColor.opacity(0.15)))
            .foregroundStyle(Color.accentColor)
            .accessibilityLabel("Round \(name)")
    }
}

/// The board's Rounds part: one section per hab that runs rounds, the run session filter,
/// and New round (rounds.md 10.3).
struct BoardRoundsSections: View {
    let center: RoundCenter
    let now: Date
    @State private var creating: RoundDraft?

    var body: some View {
        let habs = center.habs
        Section("Rounds") {
            Toggle("Show run sessions", isOn: Binding(get: { center.showRunSessions }, set: { center.showRunSessions = $0 }))
            Button {
                creating = RoundDraft(habID: habs.first { $0.online }?.id ?? habs.first?.id ?? "")
            } label: {
                Label("New round", systemImage: "plus")
            }
            .disabled(!habs.contains { $0.online })
        }
        ForEach(habs) { h in
            Section {
                if h.rows.isEmpty {
                    Text("No rounds.")
                        .foregroundStyle(.secondary)
                }
                ForEach(h.rows) { row in
                    NavigationLink(value: row) {
                        RoundRowView(row: row, now: now)
                    }
                }
            } header: {
                Text(h.online ? "Rounds on \(h.label)" : "Rounds on \(h.label) · offline")
            }
        }
        .sheet(item: $creating) { d in
            RoundFormSheet(center: center, draft: d) { creating = nil }
        }
    }
}

extension RoundDraft: Identifiable {
    public var id: String { kept?.id ?? "new-\(habID)" }
}

/// One round in the list: name, mode, trigger, next run, last run state, the mark.
struct RoundRowView: View {
    let row: RoundRow
    let now: Date

    var body: some View {
        let r = row.round
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline) {
                Text(r.name)
                    .font(.body.weight(.medium))
                    .lineLimit(1)
                Spacer(minLength: 8)
                if let m = row.mark {
                    Text(m.rawValue)
                        .font(.caption)
                        .foregroundStyle(m == .paused ? .orange : .secondary)
                } else if let last = r.lastRun {
                    Text(RoundText.state(last.state))
                        .font(.caption)
                        .foregroundStyle(stateColor(last.state))
                }
            }
            Text(detail)
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(2)
        }
        .padding(.vertical, 2)
    }

    private var detail: String {
        let r = row.round
        var parts = [r.mode.rawValue, RoundText.trigger(r.trigger)]
        if row.mark == nil, let n = r.nextSlot {
            parts.append("next " + RoundText.slot(n, roundZone: r.trigger.zone, now: now))
        }
        return parts.joined(separator: " · ")
    }
}

func stateColor(_ s: RoundRunState) -> Color {
    switch s {
    case .failed, .timedOut: return .red
    case .blocked: return .orange
    case .running, .queued, .waiting: return .blue
    case .succeeded: return .green
    default: return .secondary
    }
}

/// A round's page: its definition, the actions, and its run history (rounds.md 10.3).
struct RoundPage: View {
    let center: RoundCenter
    let habID: String
    let roundID: String
    @State private var runs: [RoundRun] = []
    @State private var more = false
    @State private var loadError: String?
    @State private var editing: RoundDraft?
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        if let row = center.row(habID: habID, round: roundID) {
            page(row)
        } else {
            ContentUnavailableView("Round deleted", systemImage: "clock.badge.xmark",
                                   description: Text("This round is no longer on its hab."))
        }
    }

    private func page(_ row: RoundRow) -> some View {
        let r = row.round
        let busy = center.busy.contains(row.id)
        return List {
            Section {
                LabeledContent("Runs", value: RoundText.trigger(r.trigger))
                if let n = r.nextSlot, row.mark == nil {
                    LabeledContent("Next run", value: RoundText.slot(n, roundZone: r.trigger.zone))
                }
                if let m = row.mark {
                    LabeledContent(m.rawValue, value: (m == .paused ? r.pausedReason : r.completedReason) ?? "")
                }
                LabeledContent("Mode", value: r.mode.rawValue)
                if let c = r.command, !c.isEmpty {
                    LabeledContent("Command", value: c.joined(separator: " "))
                } else if let h = r.harness {
                    LabeledContent("Agent", value: [harnessName(h), r.model].compactMap { $0 }.joined(separator: " · "))
                }
                if let p = r.projectRemote ?? r.projectDir ?? r.project {
                    LabeledContent("Project", value: p)
                }
                if let e = r.triggerError {
                    Text(e).foregroundStyle(.red)
                }
            }
            Section {
                Button("Run now") { Task { await center.runNow(row) } }
                    .disabled(busy)
                Button(r.paused == true ? "Unpause" : "Pause") { Task { await center.setPaused(row, r.paused != true) } }
                    .disabled(busy)
                Button("Edit") { editing = RoundDraft(habID: habID, round: r) }
                Button("Delete", role: .destructive) { center.askDelete(row) }
                if let e = center.errors[row.id] {
                    Text(e)
                        .font(.callout)
                        .foregroundStyle(.red)
                }
            }
            Section("Runs") {
                if runs.isEmpty {
                    Text(loadError ?? "No runs yet.")
                        .foregroundStyle(.secondary)
                }
                ForEach(runs, id: \.id) { run in
                    RunRowView(run: run, zone: r.trigger.zone) { open(run) }
                }
                if more {
                    Button("Older runs") { Task { await load(older: true) } }
                }
            }
        }
        .navigationTitle(r.name)
        .task(id: roundID) { await load(older: false) }
        .onChange(of: center.habRuns(habID: habID, round: roundID)) { _, fresh in
            runs = RoundPageRuns.merge(runs, fresh)
        }
        .confirmationDialog("Delete round \(r.name)?", isPresented: Binding(
            get: { center.deleteAsk?.id == row.id }, set: { if !$0 { center.deleteAsk = nil } }
        ), titleVisibility: .visible) {
            Button("Delete", role: .destructive) { Task { await center.confirmDelete() } }
            Button("Cancel", role: .cancel) { center.deleteAsk = nil }
        } message: {
            Text("A run in progress goes on to its end.")
        }
        .sheet(item: $editing) { d in
            RoundFormSheet(center: center, draft: d) { editing = nil }
        }
    }

    private func open(_ run: RoundRun) {
        guard let s = run.session else { return }
        center.openRequest = SessionTarget(habID: habID, session: s, pane: nil)
    }

    private func load(older: Bool) async {
        do {
            let page = try await center.runs(habID: habID, round: roundID, before: older ? runs.last?.number : nil)
            runs = older ? runs + page.runs : RoundPageRuns.merge(page.runs, center.habRuns(habID: habID, round: roundID))
            more = page.more ?? false
            loadError = nil
        } catch {
            loadError = RoundCenter.text(error)
        }
    }
}

/// One run in a round's history: slot, state, missed, duration, the result's first line.
struct RunRowView: View {
    let run: RoundRun
    let zone: String?
    let open: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline) {
                Text("Run \(run.number)")
                    .font(.body.weight(.medium))
                Text(run.slot.map { RoundText.slot($0, roundZone: zone) } ?? "by hand")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                Spacer(minLength: 8)
                Text(RoundText.state(run.state))
                    .font(.caption)
                    .foregroundStyle(stateColor(run.state))
            }
            let extra = [run.missed.flatMap { $0 > 0 ? "\($0) missed" : nil }, RoundText.duration(run), run.reason]
                .compactMap { $0 }
            if !extra.isEmpty {
                Text(extra.joined(separator: " · "))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            }
            if let line = RoundText.firstLine(run.result) {
                Text(line)
                    .font(.callout)
                    .lineLimit(2)
            }
            if run.session != nil {
                Button("Open session", action: open)
                    .font(.caption)
            }
        }
        .padding(.vertical, 2)
    }
}

/// The New round and Edit sheet.
struct RoundFormSheet: View {
    let center: RoundCenter
    @State var draft: RoundDraft
    let close: () -> Void
    @State private var error: String?
    @State private var saving = false
    @Environment(\.habConnections) private var connections

    init(center: RoundCenter, draft: RoundDraft, close: @escaping () -> Void) {
        self.center = center
        _draft = State(initialValue: draft)
        self.close = close
    }

    private var store: HabStore? { center.store(habID: draft.habID) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    if !draft.isEdit {
                        Picker("Hab", selection: $draft.habID) {
                            ForEach(center.habs.filter(\.online)) { Text($0.label).tag($0.id) }
                        }
                    }
                    TextField("Name", text: $draft.name)
                        .disabled(draft.isEdit)
                    Picker("Mode", selection: $draft.mode) {
                        Text("Fresh").tag(RoundMode.fresh)
                        Text("Resume").tag(RoundMode.resume)
                        Text("Fork").tag(RoundMode.fork)
                    }
                }
                Section("When") {
                    Picker("Runs", selection: $draft.when) {
                        ForEach(RoundDraft.When.allCases) { Text($0.rawValue).tag($0) }
                    }
                    switch draft.when {
                    case .every:
                        Picker("Every", selection: $draft.every) {
                            ForEach(RoundText.presets, id: \.name) { Text($0.name).tag($0.name) }
                        }
                    case .cron:
                        TextField("Cron expression", text: $draft.cron)
                            .font(.body.monospaced())
                    case .once:
                        DatePicker("At", selection: $draft.at)
                    case .manual:
                        EmptyView()
                    }
                    if draft.when == .every || draft.when == .cron {
                        TextField("Time zone (the hab's when empty)", text: $draft.zone)
                    }
                    if !draft.isEdit {
                        Toggle("Run now", isOn: $draft.runNow)
                    }
                }
                Section("What") {
                    if draft.mode == .fresh {
                        Picker("Project", selection: $draft.project) {
                            Text("Choose").tag(String?.none)
                            ForEach(projects, id: \.id) { Text($0.name).tag(Optional($0.id)) }
                        }
                        if let s = suggestion {
                            Button("Run it on \(s.label)? It is always on.") { moveTo(s) }
                                .font(.callout)
                        }
                        Picker("Runs", selection: $draft.runs) {
                            ForEach(RoundDraft.Runs.allCases) { Text($0.rawValue).tag($0) }
                        }
                    } else {
                        Picker("Session", selection: $draft.target) {
                            Text("Choose").tag(String?.none)
                            ForEach(sessions, id: \.id) { Text($0.name).tag(Optional($0.id)) }
                        }
                    }
                    if draft.mode == .fresh && draft.runs == .command {
                        TextField("Command", text: $draft.command)
                            .font(.body.monospaced())
                    } else {
                        if draft.mode == .fresh {
                            Picker("Agent", selection: $draft.harness) {
                                Text("Claude Code").tag("claude")
                                Text("Codex").tag("codex")
                                Text("opencode").tag("opencode")
                            }
                        }
                        TextField("Model (the agent's default when empty)", text: $draft.model)
                        TextField("Prompt", text: $draft.prompt, axis: .vertical)
                            .lineLimit(3...10)
                    }
                }
                Section("Limits") {
                    Stepper("Timeout: \(draft.timeoutMinutes) min", value: $draft.timeoutMinutes, in: 1...1440)
                    Stepper(draft.maxRuns == 0 ? "No run limit" : "Stop after \(draft.maxRuns) runs", value: $draft.maxRuns, in: 0...10000)
                    Picker("Notify", selection: $draft.notify) {
                        Text("On failures").tag(RoundNotify.failures)
                        Text("Always").tag(RoundNotify.always)
                        Text("Never").tag(RoundNotify.never)
                    }
                    Picker("After missed runs", selection: $draft.catchUp) {
                        Text("Run once").tag(RoundCatchUp.once)
                        Text("Skip").tag(RoundCatchUp.skip)
                    }
                    if draft.mode != .resume {
                        Picker("After a good run", selection: $draft.onFinish) {
                            Text("Close the session").tag(RoundOnFinish.close)
                            Text("Keep it open").tag(RoundOnFinish.keep)
                        }
                    }
                }
                if let error {
                    Text(error).foregroundStyle(.red)
                }
            }
            .formStyle(.grouped)
            .navigationTitle(draft.isEdit ? "Edit round" : "New round")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel", action: close) }
                ToolbarItem(placement: .confirmationAction) {
                    Button(draft.isEdit ? "Save" : "Create") { Task { await save() } }
                        .disabled(saving)
                }
            }
        }
    }

    private var projects: [Project] { (store?.projects.values.map { $0 } ?? []).sorted { $0.name < $1.name } }
    private var sessions: [Session] {
        (store?.sessions.values.filter { $0.round == nil && $0.state != .exited || $0.id == draft.target }.map { $0 } ?? [])
            .sorted { $0.name < $1.name }
    }
    private var suggestion: RoundHab? {
        draft.isEdit ? nil : center.suggestion(habID: draft.habID, project: draft.project)
    }

    private func moveTo(_ h: RoundHab) {
        let p = draft.project.flatMap { store?.projects[$0] }
        draft.habID = h.id
        draft.project = p.flatMap { RoundCenter.project(like: $0, in: center.store(habID: h.id) ?? HabStore()) }?.id
    }

    private func save() async {
        switch draft.spec() {
        case .failure(let e):
            error = e.description
        case .success(let spec):
            saving = true
            defer { saving = false }
            do {
                if let k = draft.kept {
                    try await center.update(habID: draft.habID, id: k.id, revision: k.revision, spec)
                } else {
                    try await center.create(habID: draft.habID, spec, runNow: draft.runNow)
                }
                close()
            } catch {
                self.error = RoundCenter.sentence(RoundCenter.text(error))
            }
        }
    }
}
