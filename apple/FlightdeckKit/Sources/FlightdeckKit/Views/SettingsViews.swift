import HabAPI
import SwiftUI

/// The Settings page (app.md 20.7): the level picker, then one group per
/// area, with search over titles and ids.
struct SettingsPage: View {
    let center: SettingsCenter
    let board: Board
    var push: PushRegistrar?
    var openUsage: (() -> Void)?
    @Environment(\.sshSetup) private var ssh
    @Environment(\.voiceKit) private var voice
    @Environment(\.capcom) private var capcom
    @State private var query = ""

    var body: some View {
        ScrollViewReader { proxy in
            Form {
                if query.isEmpty {
                    LevelSection(center: center) { id in
                        withAnimation { proxy.scrollTo(id, anchor: .center) }
                    }
                }
                ForEach(SettingArea.allCases, id: \.self) { area in
                    let rows = rows(in: area)
                    let links = links(in: area)
                    if !rows.isEmpty || !links.isEmpty {
                        Section(area.title) {
                            ForEach(rows) { d in
                                SettingRow(def: d, center: center)
                                    .id(d.id)
                            }
                            ForEach(links, id: \.self) { link($0) }
                        }
                    }
                }
                if matches(AcknowledgementsView.title) {
                    Section {
                        NavigationLink(AcknowledgementsView.title) {
                            AcknowledgementsView().navigationTitle(AcknowledgementsView.title)
                        }
                    }
                }
            }
            .searchable(text: $query, prompt: "Search settings")
        }
    }

    private func matches(_ text: String) -> Bool {
        let q = query.trimmingCharacters(in: .whitespaces)
        return q.isEmpty || text.localizedCaseInsensitiveContains(q)
    }

    /// The registry rows of an area this device shows. A row kept on its
    /// own page shows only where that page is linked.
    private func rows(in area: SettingArea) -> [SettingDef] {
        SettingsRegistry.all.filter { d in
            guard d.area == area, d.shownHere, d.built, matches(d.title) || matches(d.id) else { return false }
            if d.id == SettingsRegistry.sshRequireFaceID { return ssh != nil }
            if d.kind == .page { return !availableLinks(area).isEmpty }
            return true
        }
    }

    private enum Link: Hashable {
        case notifications, voice, usage, ssh, capcom
        var title: String {
            switch self {
            case .capcom: return "Capcom host"
            case .notifications: return "Notifications"
            case .voice: return "Voice"
            case .usage: return "Usage warnings"
            case .ssh: return "SSH keys and habs"
            }
        }
    }

    /// The pages an area links to on this device.
    private func availableLinks(_ area: SettingArea) -> [Link] {
        switch area {
        case .agents where capcom != nil && !board.habs.isEmpty: return [.capcom]
        case .notifications where push != nil: return [.notifications]
        case .voice where voice != nil: return [.voice]
        case .usage where openUsage != nil: return [.usage]
        case .habs where ssh != nil: return [.ssh]
        default: return []
        }
    }

    /// The links that match the search: by their own title, or by a row kept on their page.
    private func links(in area: SettingArea) -> [Link] {
        let pageRowMatches = rows(in: area).contains { $0.kind == .page }
        return availableLinks(area).filter { matches($0.title) || pageRowMatches }
    }

    @ViewBuilder
    private func link(_ l: Link) -> some View {
        switch l {
        case .notifications:
            if let push {
                NavigationLink(l.title) { NotificationSettingsView(push: push, board: board).navigationTitle(l.title) }
            }
        case .voice:
            if let voice {
                NavigationLink(l.title) { VoiceSettingsView(kit: voice).navigationTitle(l.title) }
            }
        case .usage:
            Button(l.title) { openUsage?() }
        case .capcom:
            if let capcom {
                NavigationLink(l.title) { CapcomHostSettingsView(center: capcom).navigationTitle(l.title) }
            }
        case .ssh:
            if let ssh {
                NavigationLink(l.title) { SSHSettingsView(setup: ssh).navigationTitle("SSH") }
            }
        }
    }
}

/// The level picker at the top of Settings (app.md 20.2).
struct LevelSection: View {
    let center: SettingsCenter
    /// Scrolls to a row by its setting id.
    let scrollTo: (String) -> Void
    @State private var working = false

    var body: some View {
        let state = center.level
        Section {
            Picker("Level", selection: Binding(
                get: { state.level?.rawValue ?? "custom" },
                set: { new in
                    guard let l = SettingLevel(rawValue: new) else { return }
                    working = true
                    Task {
                        await center.pick(l)
                        working = false
                    }
                }
            )) {
                ForEach(SettingLevel.allCases, id: \.self) { Text($0.title).tag($0.rawValue) }
                if state.level == nil { Text("Custom").tag("custom") }
            }
            .disabled(working)
            if let first = state.exceptions.first {
                Button(state.exceptions.count == 1 ? "1 exception" : "\(state.exceptions.count) exceptions") {
                    scrollTo(first.settingID)
                }
                .font(.footnote)
            }
            if let n = center.notice {
                Text(n)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        } footer: {
            Text(state.title)
        }
    }
}

/// One registry row: its control, where it is kept, and its id.
struct SettingRow: View {
    let def: SettingDef
    let center: SettingsCenter

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            control
            caption
            Text(def.id)
                .font(.caption2.monospaced())
                .foregroundStyle(.tertiary)
        }
    }

    @ViewBuilder
    private var control: some View {
        switch def.store {
        case .app:
            if case .bool = def.kind, case .bool(let on)? = center.ports[def.id]?.get() {
                Toggle(def.title, isOn: Binding(get: { on }, set: { v in Task { await center.setApp(def.id, .bool(v)) } }))
            } else {
                Text(def.title)
            }
        case .everyHab:
            let row = center.row(def.id)
            ValueControl(def: def, value: row.value) { v in Task { await center.setEveryHab(def.id, v) } }
                .disabled(row.unavailable || (!row.lockedOn.isEmpty && row.lockedOn.count == holdingCount))
        case .oneHab:
            Text(def.title)
            ForEach(center.habs.filter { $0.hasSettings && $0.settings[def.id] != nil }, id: \.habID) { h in
                let s = h.settings[def.id]!
                PermissionWaitRow(label: h.label, setting: s, shown: LevelMatcher.value(h, def.id, queued: center.queue.items)) { v in Task { await center.setOneHab(def.id, habID: h.habID, v) } }
                    .disabled(s.isLocked)
            }
        }
    }

    private var holdingCount: Int {
        center.habs.filter { $0.hasSettings && $0.settings[def.id] != nil }.count
    }

    @ViewBuilder
    private var caption: some View {
        switch def.store {
        case .app:
            EmptyView()
        case .everyHab:
            let row = center.row(def.id)
            Group {
                if row.unavailable {
                    Text("No hab serves this setting")
                } else {
                    Text("On every hab")
                    if !row.lockedOn.isEmpty { Text("Set in the hab's config on \(row.lockedOn.joined(separator: ", "))") }
                    if !row.differsOn.isEmpty { Text("Differs on \(row.differsOn.joined(separator: ", "))") }
                }
            }
            .font(.caption)
            .foregroundStyle(.secondary)
        case .oneHab:
            if center.habs.allSatisfy({ !$0.hasSettings || $0.settings[def.id] == nil }) {
                Text("No hab serves this setting").font(.caption).foregroundStyle(.secondary)
            }
        }
    }
}

/// The control for a hab-side value of a levelled setting.
struct ValueControl: View {
    let def: SettingDef
    let value: JSONValue?
    let set: (JSONValue) -> Void

    static let minutes: [Int64] = [0, 15, 30, 60, 120, 240, 480, 1440]

    var body: some View {
        switch def.kind {
        case .bool:
            Toggle(def.title, isOn: Binding(get: { if case .bool(let b)? = value { return b } else { return false } },
                                            set: { set(.bool($0)) }))
        case .word(let words):
            Picker(def.title, selection: Binding(get: { if case .string(let s)? = value { return s } else { return "" } },
                                                 set: { set(.string($0)) })) {
                ForEach(words, id: \.word) { Text($0.title).tag($0.word) }
            }
        case .integer:
            let current: Int64 = { if case .int(let n)? = value { return n } else { return -1 } }()
            Picker(def.title, selection: Binding(get: { current }, set: { set(.int($0)) })) {
                ForEach(Self.minutes + (Self.minutes.contains(current) || current < 0 ? [] : [current]), id: \.self) { n in
                    Text(n == 0 ? "Never" : "\(n) min").tag(n)
                }
            }
        case .page:
            Text(def.title)
        }
    }
}

/// `hab.permission_wait` on one hab: a switch (0 or 30 s) with a stepper once on (app.md 20.4).
struct PermissionWaitRow: View {
    let label: String
    let setting: Setting
    /// The value shown: the queued one for an offline hab, else the hab's (app.md 20.8).
    let shown: JSONValue?
    let set: (JSONValue) -> Void

    private var seconds: Int64 { if case .int(let n)? = shown { return n } else { return 0 } }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Toggle("On \(label)", isOn: Binding(get: { seconds > 0 },
                                                set: { set(.int($0 ? SettingsRegistry.permissionWaitOn : 0)) }))
            if seconds > 0 {
                Stepper("Wait up to \(seconds) s", value: Binding(get: { seconds }, set: { set(.int($0)) }), in: 1...120)
            }
            if setting.isLocked {
                Text("Set in the hab's config").font(.caption).foregroundStyle(.secondary)
            }
        }
    }
}
