#if os(iOS)
import HabAPI
import SwiftUI
import UIKit

/// The Settings tab (app.md 20.7): Computers with pairing, how much agents may do alone, this
/// device's settings, and help and diagnostics. A regular width shows the list as a split
/// view's sidebar, and each page in the detail column, as the system's Settings does.
struct SettingsTab: View {
    let fleet: Fleet
    @State private var pairing = false
    @Environment(\.sshSetup) private var ssh
    @Environment(\.voiceKit) private var voice
    @Environment(\.horizontalSizeClass) private var sizeClass

    private var board: Board { fleet.board }

    var body: some View {
        if sizeClass == .compact {
            NavigationStack { root }
        } else {
            NavigationSplitView {
                root
                    .navigationSplitViewColumnWidth(min: 340, ideal: 400, max: 460)
            } detail: {
                NavigationStack {
                    ContentUnavailableView("Choose a setting", systemImage: "slider.horizontal.3",
                                           description: Text("Pick a computer or a page from the list."))
                }
            }
        }
    }

    private var root: some View {
            Form {
                computers
                if let settings = fleet.settings { LevelGroup(center: settings, board: board, push: fleet.push, usage: fleet.usage) }
                device
                Section {
                    NavigationLink("Help and diagnostics") { HelpPage().navigationTitle("Help and diagnostics") }
                } footer: {
                    Text("Usage warnings live on the usage bar. Capcom and round settings live on their tabs.")
                }
            }
            .navigationTitle("Settings")
            .navigationDestination(for: HabLink.self) { l in
                HabDetailPage(fleet: fleet, habID: l.id)
            }
            .sheet(isPresented: $pairing) {
                if let ssh {
                    NavigationStack {
                        PairingView(setup: ssh, offer: nil) { pairing = false }
                    }
                }
            }
    }

    private var computers: some View {
        Section {
            ForEach(board.habs) { h in
                NavigationLink(value: HabLink(id: h.id)) {
                    HabRow(hab: h, capcomHome: fleet.capcom.homeDevice)
                }
            }
            Button("Pair another computer") { pairing = true }
                .disabled(ssh == nil)
        } header: {
            Text("Computers")
        } footer: {
            if ssh == nil, fleet.isDemo {
                Text("Demo data is on. Restart the app to pair a computer.")
            }
        }
    }

    private var deviceName: String {
        UIDevice.current.userInterfaceIdiom == .pad ? "This iPad" : "This iPhone"
    }

    private var device: some View {
        Section(deviceName) {
            if let push = fleet.push {
                NavigationLink {
                    NotificationSettingsView(push: push, board: board).navigationTitle("Notifications")
                } label: {
                    LabeledContent("Notifications", value: push.permission == .granted ? "On" : push.permission == .denied ? "Off" : "")
                }
            }
            if let ssh {
                FaceIDToggle(ssh: ssh, settings: fleet.settings)
            }
            if let voice {
                NavigationLink {
                    VoiceSettingsView(kit: voice).navigationTitle("Voice")
                } label: {
                    Text("Voice")
                }
            }
            if let usage = fleet.usage {
                NavigationLink {
                    Form { UsageWarningSettings(alerts: usage.alerts) }
                        .navigationTitle("Usage warnings")
                } label: {
                    Text("Usage warnings")
                }
            }
        }
    }
}

struct HabLink: Hashable {
    let id: String
}

/// One computer in Settings: a state dot, its label, and "Online · 6 sessions · runs Capcom".
struct HabRow: View {
    let hab: Board.Hab
    let capcomHome: String?

    private var line: String {
        var parts = [hab.statusText]
        let n = hab.state.store.sessions.count
        if hab.state.status == .online { parts.append(inflected("^[\(n) session](inflect: true)")) }
        if let d = hab.state.welcome?.deviceId, d == capcomHome { parts.append("runs Capcom") }
        return parts.joined(separator: " · ")
    }

    var body: some View {
        HStack(spacing: 12) {
            Circle()
                .fill(hab.state.status == .online ? Color.green : Color.secondary.opacity(0.5))
                .frame(width: 10, height: 10)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                Text(hab.label)
                    .font(.body.weight(.semibold))
                Text(line)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
        }
        .accessibilityElement(children: .combine)
    }
}

/// A computer's page (app.md 20.7): what the Connections list showed, and its sessions.
struct HabDetailPage: View {
    let fleet: Fleet
    let habID: String
    @Environment(\.sshSetup) private var ssh

    var body: some View {
        let hab = fleet.board.habs.first { $0.id == habID }
        Form {
            if let hab {
                Section {
                    LabeledContent("State", value: hab.statusText)
                    LabeledContent("Address", value: hab.endpoint)
                    if hab.state.status == .online {
                        LabeledContent("Sessions", value: "\(hab.state.store.sessions.count)")
                    }
                    if let d = hab.state.welcome?.deviceId, d == fleet.capcom.homeDevice {
                        LabeledContent("Capcom", value: "Runs here")
                    }
                } footer: {
                    if hab.state.status != .online, let e = hab.state.lastError {
                        Text(e)
                    }
                }
                if let entry = sshEntry(hab) {
                    Section("Over SSH") {
                        LabeledContent("Login", value: entry.destination)
                        if let ssh {
                            NavigationLink("SSH keys and habs") { SSHSettingsView(setup: ssh).navigationTitle("SSH") }
                        }
                    }
                }
            } else {
                Text("This computer is no longer in the app.").foregroundStyle(.secondary)
            }
        }
        .navigationTitle(hab?.label ?? "Computer")
        .navigationBarTitleDisplayMode(.inline)
    }

    private func sshEntry(_ hab: Board.Hab) -> SSHHabEntry? {
        guard habID.hasPrefix("nio:") else { return nil }
        return ssh?.entry(id: String(habID.dropFirst(4)))
    }
}

/// "How much agents may do alone" (app.md 20.7): the level as a segmented control, what it
/// means in one sentence, the exception count, and the full page.
struct LevelGroup: View {
    let center: SettingsCenter
    let board: Board
    let push: PushRegistrar?
    let usage: UsageCenter?
    @State private var working = false

    static func explanation(_ level: SettingLevel?) -> String {
        switch level {
        case .safe?:
            return "Safe: agents ask before they start work on another computer, a group's plan waits for you, and SSH keys need Face ID."
        case .balanced?:
            return "Balanced: agents start work on other computers without asking. A group's plan still waits for you."
        case .bypass?:
            return "Bypass: Flightdeck adds no question of its own. Your agents' own permission prompts still ask."
        case nil:
            return "Custom: your settings match no level. Pick one to set them all at once."
        }
    }

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
                Text("Safe").tag(SettingLevel.safe.rawValue)
                Text("Balanced").tag(SettingLevel.balanced.rawValue)
                Text("Bypass").tag(SettingLevel.bypass.rawValue)
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .disabled(working)
            Text(Self.explanation(state.level))
                .font(.subheadline)
                .foregroundStyle(.secondary)
            if !state.exceptions.isEmpty {
                Text("\(state.exceptions.count) settings differ from this level.", bundle: #bundle)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            if let n = center.notice {
                Text(n)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            NavigationLink("All settings") {
                SettingsPage(center: center, board: board, push: push, openUsage: usage.map { u in { u.book.present(account: nil) } })
                    .navigationTitle("All settings")
            }
        } header: {
            Text("How much agents may do alone")
        }
    }
}

/// "Face ID before Allow": `ssh.require_face_id`, through the settings center when there is one.
struct FaceIDToggle: View {
    let ssh: SSHSetup
    let settings: SettingsCenter?

    var body: some View {
        Toggle("Face ID before Allow", isOn: Binding(
            get: { ssh.faceID },
            set: { on in
                Task {
                    if let settings {
                        await settings.setApp(SettingsRegistry.sshRequireFaceID, .bool(on))
                    } else {
                        try? await ssh.setFaceID(on)
                    }
                }
            }
        ))
    }
}

/// Help and diagnostics (app.md 20.7): the setting keys switch, SSH keys, acknowledgements.
struct HelpPage: View {
    @AppStorage(SettingKeys.showKeysDefault) private var showKeys = false
    @Environment(\.sshSetup) private var ssh

    var body: some View {
        Form {
            Section {
                Toggle("Show setting keys", isOn: $showKeys)
            } footer: {
                Text("Shows each setting's key under its name in All settings, so a doc or an issue can name it.")
            }
            if let ssh {
                Section {
                    NavigationLink("SSH keys and habs") { SSHSettingsView(setup: ssh).navigationTitle("SSH") }
                }
            }
            Section {
                NavigationLink(AcknowledgementsView.title) {
                    AcknowledgementsView().navigationTitle(AcknowledgementsView.title)
                }
            }
        }
    }
}
#endif

/// Where the "Show setting keys" switch is kept (app.md 20.7).
enum SettingKeys {
    static let showKeysDefault = "flightdeck.settings.show_keys"
}
