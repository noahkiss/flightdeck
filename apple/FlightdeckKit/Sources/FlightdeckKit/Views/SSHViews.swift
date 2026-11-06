import NIOSSH
import SwiftUI
#if os(iOS)
import UIKit
#else
import AppKit
#endif

extension EnvironmentValues {
    @Entry public var sshSetup: SSHSetup? = nil
}

/// Settings › SSH: this device's key, the habs reached over SSH, and the
/// background action check.
public struct SSHSettingsView: View {
    @Bindable var setup: SSHSetup
    @State private var adding = false
    @State private var pairing = false
    @State private var confirmNewKey = false
    @State private var message: String?

    public init(setup: SSHSetup) {
        self.setup = setup
    }

    public var body: some View {
        Form {
            keySection
            ImportedKeysSection(setup: setup)
            Section("Habs over SSH") {
                ForEach(setup.entries) { e in
                    VStack(alignment: .leading, spacing: 2) {
                        Text(e.label ?? e.destination)
                        if e.label != nil {
                            Text(e.destination).font(.caption).foregroundStyle(.secondary)
                        }
                        Text("Host key " + e.hostKeyFingerprint)
                            .font(.caption2.monospaced())
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                        HabKeyMenu(setup: setup, entry: e, message: $message)
                    }
                    .contextMenu {
                        Button("Remove", role: .destructive) { remove(e) }
                    }
                }
                .onDelete { idx in
                    for i in idx { remove(setup.entries[i]) }
                }
                Button("Add a hab by address") { adding = true }
                Button("Pair with a code") { pairing = true }
            }
            #if os(iOS)
            if !setup.entries.isEmpty {
                Section("Background action") {
                    ForEach(setup.entries) { e in
                        Button("Send a test notification for \(e.label ?? e.destination)") {
                            Task {
                                do {
                                    try await HabActions.scheduleTest(entry: e)
                                    message = "The notification arrives in 5 seconds."
                                } catch {
                                    message = "\(error)"
                                }
                            }
                        }
                    }
                }
            }
            #endif
            if let d = ColdStart.firstReply {
                Section("Timing") {
                    LabeledContent("Start to first hab reply", value: "\(d.components.seconds * 1000 + d.components.attoseconds / 1_000_000_000_000_000) ms")
                }
            }
            if let message {
                Section { Text(message).font(.callout) }
            }
        }
        .formStyle(.grouped)
        .onAppear {
            setup.loadKey()
            setup.loadImportedKeys()
        }
        .sheet(isPresented: $adding) {
            NavigationStack {
                AddHabView(setup: setup, prefill: nil) { adding = false }
            }
        }
        .sheet(isPresented: $pairing) {
            NavigationStack {
                PairingView(setup: setup, offer: nil) { pairing = false }
            }
        }
        .confirmationDialog("Make a new key?", isPresented: $confirmNewKey, titleVisibility: .visible) {
            Button("Make a new key") {
                do { try setup.replaceKey() } catch { message = "\(error)" }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Every host must get the new public key.")
        }
    }

    @ViewBuilder
    private var keySection: some View {
        Section("This device's key") {
            if let k = setup.deviceKey, let line = setup.authorizedKeysLine {
                LabeledContent("Type", value: (setup.keyMove?.kind ?? k.kind) == .secureEnclave ? "P-256, Secure Enclave" : "P-256, Keychain")
                VStack(alignment: .leading, spacing: 4) {
                    Text("Fingerprint").font(.caption).foregroundStyle(.secondary)
                    Text(setup.keyMove?.fingerprint ?? k.fingerprint).font(.caption.monospaced()).textSelection(.enabled)
                }
                PublicKeyLine(line: line)
                if let m = setup.keyMove {
                    // An earlier build's key signs only while unlocked (transport.md 7.2).
                    Text("New key. Add it to each host so the Watch can answer while iPhone is locked. The old key works until then.")
                        .font(.callout)
                    if !m.waiting.isEmpty {
                        Text("Waiting for: " + m.waiting.map { $0.label ?? $0.destination }.joined(separator: ", "))
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
                #if os(iOS)
                Toggle("Require Face ID for SSH keys", isOn: Binding(
                    get: { setup.faceID },
                    set: { on in
                        Task {
                            do { try await setup.setFaceID(on) } catch { message = "\(error)" }
                        }
                    }
                ))
                if !setup.faceID && k.biometryBuiltIn {
                    // A key made by an earlier build with Face ID in the Secure Enclave.
                    Text("This key asks for Face ID until you make a new one.").foregroundStyle(.secondary)
                    Button("Make a new key") { confirmNewKey = true }
                }
                #endif
            } else if let e = setup.keyError {
                Text(e).foregroundStyle(.secondary)
                Button("Try again") { setup.loadKey() }
            } else {
                ProgressView()
            }
        }
    }

    private func remove(_ e: SSHHabEntry) {
        Task {
            do { try await setup.remove(e) } catch { message = "\(error)" }
        }
    }
}

/// The OpenSSH public key line with copy and share controls.
struct PublicKeyLine: View {
    let line: String

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(line)
                .font(.caption2.monospaced())
                .textSelection(.enabled)
                .lineLimit(4)
            HStack {
                Button("Copy public key") { copyToPasteboard(line) }
                Spacer()
                ShareLink(item: line) { Label("Share", systemImage: "square.and.arrow.up") }
            }
            .buttonStyle(.borderless)
        }
    }
}

func copyToPasteboard(_ s: String) {
    #if os(iOS)
    UIPasteboard.general.string = s
    #else
    NSPasteboard.general.clearContents()
    NSPasteboard.general.setString(s, forType: .string)
    #endif
}

/// Values a pairing code fills in before "Add a hab by address".
struct AddHabPrefill: Equatable {
    var host: String
    var label: String?
    var deviceID: String?
}

/// Add a hab through the host's own sshd (app.md 18.8): copy the device key
/// to the host, confirm the host key, test, save.
struct AddHabView: View {
    let setup: SSHSetup
    let prefill: AddHabPrefill?
    let done: () -> Void

    @State private var destination = ""
    @State private var label = ""
    @State private var command = ""
    /// The key the new entry signs with: the generated key unless the user picks an imported one.
    @State private var keyID = DeviceKeys.defaultID
    @State private var step: Step = .form
    @State private var error: String?
    /// The destination the shown host key came from.
    @State private var probed: SSHDestination?
    /// "Set up with a password" (transport.md 6.1.6): add the key with the
    /// account password once, instead of by hand.
    @State private var usePassword = false
    @State private var password = ""

    enum Step: Equatable {
        case form
        case probing
        case confirm(String, String) // fingerprint, OpenSSH line
        case testing
    }

    var body: some View {
        Form {
            Section {
                TextField(prefill.map { "user@" + SSHEndpoint(host: $0.host).description } ?? "user@host or user@host:port", text: $destination)
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    .keyboardType(.URL)
                    #endif
                    .autocorrectionDisabled()
                TextField("Label (optional)", text: $label)
            } header: {
                Text("Host")
            } footer: {
                if prefill != nil && destination.isEmpty {
                    Text("Type the user name and @ before the host.")
                }
            }
            Section {
                if !setup.importedKeys.isEmpty {
                    Picker("Key", selection: $keyID) {
                        Text(setup.keyName(DeviceKeys.defaultID)).tag(DeviceKeys.defaultID)
                        ForEach(setup.importedKeys, id: \.id) { k in
                            Text(setup.keyName(k.id)).tag(k.id)
                        }
                    }
                }
                if let line = keyLine {
                    PublicKeyLine(line: line)
                } else {
                    Text(setup.keyError ?? "No device key yet.").foregroundStyle(.secondary)
                }
            } header: {
                Text(setup.importedKeys.isEmpty ? "This device's key" : "Key")
            } footer: {
                if usePassword {
                    Text("The app signs in once with the account password, adds this key to ~/.ssh/authorized_keys, and forgets the password.")
                } else {
                    Text(keyID == DeviceKeys.defaultID
                        ? "Add this line to ~/.ssh/authorized_keys for that user on the host."
                        : "A host that already trusts this key needs nothing more. Otherwise add this line to ~/.ssh/authorized_keys for that user on the host.")
                }
            }
            Section {
                TextField(NIOSSHTransport.defaultCommand, text: $command)
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif
                    .autocorrectionDisabled()
                    .font(.body.monospaced())
            } header: {
                Text("Command on the host")
            } footer: {
                Text("Leave empty to run gff-hab connect. Use a full path when gff-hab is not on the remote PATH.")
            }
            if case .confirm(let fp, _) = step {
                Section {
                    Text(fp).font(.caption.monospaced()).textSelection(.enabled)
                    if usePassword {
                        SecureField("Password for \(probed?.user ?? "the user")", text: $password)
                            .textContentType(.password)
                        Button("Trust this host key and add the key") { Task { await trust() } }
                            .disabled(password.isEmpty)
                    } else {
                        Button("Trust this host key") { Task { await trust() } }
                    }
                    Button("Cancel", role: .cancel) { step = .form }
                } header: {
                    Text("Host key")
                } footer: {
                    Text("Compare with ssh-keygen -lf on the host's key, for example /etc/ssh/ssh_host_ed25519_key.pub.")
                }
            }
            if let error {
                Section { Text(error).foregroundStyle(.red).font(.callout).textSelection(.enabled) }
            }
            if !usePassword && step == .form {
                Section {
                    Button("Set up with a password") { usePassword = true }
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Add a hab")
        .onDisappear { password = "" }
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel", action: done)
            }
            ToolbarItem(placement: .confirmationAction) {
                switch step {
                case .probing, .testing: ProgressView()
                default:
                    Button("Connect") { Task { await probe() } }
                        .disabled(SSHDestination.parse(destination) == nil || keyLine == nil)
                }
            }
        }
        .onChange(of: destination) { _, _ in resetConfirm() }
        .onChange(of: keyID) { _, _ in resetConfirm() }
        .onChange(of: command) { _, _ in resetConfirm() }
        .onAppear {
            setup.loadKey()
            setup.loadImportedKeys()
            if let p = prefill, label.isEmpty {
                label = p.label ?? ""
            }
        }
    }

    /// The chosen key's line for the host's authorized keys.
    private var keyLine: String? {
        if keyID == DeviceKeys.defaultID { return setup.authorizedKeysLine }
        return setup.importedKeys.first { $0.id == keyID }?.authorizedKeysLine
    }

    /// An edit after the probe drops the shown host key: it belongs to the old address.
    private func resetConfirm() {
        if case .confirm = step {
            step = .form
            probed = nil
        }
    }

    private func probe() async {
        guard let d = SSHDestination.parse(destination) else { return }
        error = nil
        step = .probing
        do {
            let key = try await setup.probe(d, keyID: keyID)
            probed = d
            step = .confirm(SSHFingerprint.of(key), String(openSSHPublicKey: key))
        } catch {
            self.error = "\(error)"
            step = .form
        }
    }

    private func trust() async {
        guard case .confirm(let fp, let line) = step, let d = probed else { return }
        let cmd = command.trimmingCharacters(in: .whitespaces)
        let entry = SSHHabEntry(
            label: label.trimmingCharacters(in: .whitespaces).isEmpty ? nil : label.trimmingCharacters(in: .whitespaces),
            user: d.user,
            endpoints: [d.endpoint],
            hostKeyFingerprint: fp,
            hostKey: line,
            keyID: keyID,
            command: cmd.isEmpty ? nil : cmd,
            deviceID: prefill?.deviceID
        )
        step = .testing
        error = nil
        do {
            if usePassword {
                // The password is used once and dropped, pass or fail.
                let p = password
                password = ""
                _ = try await setup.installKey(d, hostKeyFingerprint: fp, password: p, keyID: keyID, command: entry.command, deviceName: PairingDevice.name)
            }
            _ = try await setup.test(entry)
            try setup.add(entry)
            done()
        } catch {
            self.error = "\(error)"
            step = .confirm(fp, line)
        }
    }
}

/// This device's name and kind, as the pairing request sends them.
@MainActor
enum PairingDevice {
    static var name: String {
        #if os(iOS)
        return UIDevice.current.name
        #else
        return Host.current().localizedName ?? "Mac"
        #endif
    }

    static var kind: Pairing.Kind {
        #if os(iOS)
        return UIDevice.current.userInterfaceIdiom == .pad ? .tablet : .phone
        #else
        return .desktop
        #endif
    }
}

/// Pair with a hab's code (app.md 18.8): scan it, or paste the code or
/// link, then run the exchange with the hab's pairing window. While the
/// owner confirms on the hab, the last four characters of this device's key
/// fingerprint show large.
struct PairingView: View {
    let setup: SSHSetup
    @State var offer: PairingCode?
    let done: () -> Void

    @State private var text = ""
    @State private var error: String?
    @State private var scanning = false
    @State private var name = ""
    @State private var step: Step = .ready
    @State private var task: Task<Void, Never>?

    enum Step: Equatable {
        case ready
        case connecting
        /// The owner is asked; the code to type on the hab.
        case waiting(String)
        /// Paired, but the answer had no sshd host key: compare and trust.
        case hostKey(SSHHabEntry, String, String) // entry, fingerprint, OpenSSH line
        case testing
        /// Paired, but the first connection failed: the key is on the host,
        /// so try the connection again rather than pairing again.
        case retry(SSHHabEntry)
    }

    var body: some View {
        Form {
            if let o = offer {
                habSection(o)
                stepSections(o)
            } else {
                #if os(iOS)
                Section {
                    if scanning {
                        QRScannerView { code in
                            scanning = false
                            read(code)
                        }
                        .frame(height: 280)
                        .listRowInsets(EdgeInsets())
                    } else {
                        Button("Scan the code") { scanning = true }
                    }
                }
                #endif
                Section {
                    TextField("flightdeck://pair?…", text: $text, axis: .vertical)
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                        .autocorrectionDisabled()
                        .font(.callout.monospaced())
                    Button("Read the code") { read(text) }
                        .disabled(text.isEmpty)
                } header: {
                    Text("Code or link")
                }
            }
            if let error {
                Section { Text(error).foregroundStyle(.red).font(.callout).textSelection(.enabled) }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Pair with a code")
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button(step == .ready ? "Close" : "Cancel") {
                    task?.cancel()
                    done()
                }
            }
        }
        .onAppear {
            setup.loadKey()
            if name.isEmpty { name = PairingDevice.name }
        }
        .onDisappear { task?.cancel() }
    }

    @ViewBuilder
    private func habSection(_ o: PairingCode) -> some View {
        Section("Hab") {
            if let l = o.label { LabeledContent("Label", value: l) }
            VStack(alignment: .leading, spacing: 4) {
                Text("Hab id").font(.caption).foregroundStyle(.secondary)
                Text(o.deviceID).font(.caption.monospaced()).textSelection(.enabled)
            }
            ForEach(o.endpoints, id: \.self) { ep in
                Text(ep.description).font(.callout.monospaced())
            }
        }
    }

    @ViewBuilder
    private func stepSections(_ o: PairingCode) -> some View {
        switch step {
        case .ready:
            Section {
                TextField("Device name", text: $name)
                    .autocorrectionDisabled()
            } header: {
                Text("This device")
            } footer: {
                Text("The hab shows this name when it asks to confirm.")
            }
            Section {
                Button("Pair this device") { start(o) }
                    .disabled(setup.deviceKey == nil)
                if setup.deviceKey == nil, let e = setup.keyError {
                    Text(e).foregroundStyle(.secondary)
                }
            }
            Section {
                NavigationLink("Add the key by hand") {
                    AddHabView(setup: setup, prefill: AddHabPrefill(host: o.endpoints.first?.host ?? "", label: o.label, deviceID: o.deviceID), done: done)
                }
            } footer: {
                Text("For a host that cannot run gff-hab pair.")
            }
        case .connecting:
            Section {
                HStack {
                    ProgressView()
                    Text("Connecting to the hab")
                }
            }
        case .waiting(let code):
            Section {
                VStack(spacing: 12) {
                    Text(code)
                        .font(.system(size: 64, weight: .semibold, design: .monospaced))
                        .textSelection(.enabled)
                        .accessibilityLabel(code.map { String($0) }.joined(separator: " "))
                    Text("Type these on \(o.label ?? "the hab") to confirm")
                        .font(.callout)
                        .multilineTextAlignment(.center)
                }
                .frame(maxWidth: .infinity)
                .padding(.vertical, 12)
            } footer: {
                Text("Upper and lower case count.")
            }
        case .hostKey(let e, let fp, _):
            Section {
                Text(fp).font(.caption.monospaced()).textSelection(.enabled)
                Button("Trust this host key") { trust(e, fp) }
            } header: {
                Text("Host key of \(e.destination)")
            } footer: {
                Text("The hab sent no SSH host key. Compare with ssh-keygen -lf on the host's key, for example /etc/ssh/ssh_host_ed25519_key.pub.")
            }
        case .testing:
            Section {
                HStack {
                    ProgressView()
                    Text("Checking the connection")
                }
            }
        case .retry(let e):
            Section {
                Button("Try the connection again") { finish(e) }
            } footer: {
                Text("This device is paired with \(e.destination). The hab already has its key.")
            }
        }
    }

    private func read(_ s: String) {
        do {
            offer = try PairingCode.parse(s)
            error = nil
        } catch {
            self.error = "\(error)"
        }
    }

    private func start(_ o: PairingCode) {
        error = nil
        step = .connecting
        let n = name
        task = Task {
            do {
                let r = try await setup.pair(o, name: n, kind: PairingDevice.kind) { fp in
                    let code = Pairing.confirmCode(fp)
                    Task { @MainActor in
                        if step == .connecting { step = .waiting(code) }
                    }
                }
                if r.needsHostKey {
                    let key = try await setup.probe(SSHDestination(user: r.entry.user, endpoint: r.entry.endpoints[0]), keyID: r.entry.keyID)
                    step = .hostKey(r.entry, SSHFingerprint.of(key), String(openSSHPublicKey: key))
                    return
                }
                finish(r.entry)
            } catch is CancellationError {
            } catch {
                if Task.isCancelled { return }
                self.error = "\(error)"
                step = .ready
            }
        }
    }

    private func trust(_ e: SSHHabEntry, _ fp: String) {
        guard case .hostKey(_, _, let line) = step else { return }
        var entry = e
        entry.hostKeyFingerprint = fp
        entry.hostKey = line
        finish(entry)
    }

    private func finish(_ e: SSHHabEntry) {
        error = nil
        step = .testing
        task = Task {
            do {
                try await setup.finishPairing(e)
                done()
            } catch {
                if Task.isCancelled { return }
                self.error = "\(error)"
                step = .retry(e)
            }
        }
    }
}

/// The SSH settings sheet and the pairing sheet for an opened link.
struct SSHSheets: ViewModifier {
    let setup: SSHSetup?

    func body(content: Content) -> some View {
        if let setup {
            content.modifier(Present(setup: setup))
        } else {
            content
        }
    }

    /// What the one sheet shows. A pairing link and a key file can arrive
    /// together; one sheet shows them in turn, the pairing link first.
    enum Item: Identifiable {
        case pairing
        case keyFile(PendingKeyFile)
        var id: String {
            switch self {
            case .pairing: return "pairing"
            case .keyFile(let p): return "key-" + p.id.uuidString
            }
        }
    }

    /// The sheet's item: the pairing link while one waits, else the key file.
    @MainActor
    static func item(_ setup: SSHSetup) -> Item? {
        if setup.pendingCode != nil || setup.pendingCodeError != nil { return .pairing }
        return setup.pendingKeyFile.map { .keyFile($0) }
    }

    private struct Present: ViewModifier {
        @Bindable var setup: SSHSetup

        func body(content: Content) -> some View {
            content.sheet(item: Binding(
                get: { SSHSheets.item(setup) },
                set: { new in
                    guard new == nil else { return }
                    // Clear only what was showing; the other one follows.
                    switch SSHSheets.item(setup) {
                    case .pairing: setup.pendingCode = nil; setup.pendingCodeError = nil
                    case .keyFile: setup.pendingKeyFile = nil
                    case nil: break
                    }
                }
            )) { item in
                NavigationStack {
                    switch item {
                    case .keyFile(let p):
                        KeyImportView(setup: setup, pending: p) { setup.pendingKeyFile = nil }
                    case .pairing:
                        if let o = setup.pendingCode {
                            PairingView(setup: setup, offer: o) { setup.pendingCode = nil }
                        } else {
                            Form { Text(setup.pendingCodeError ?? "") }
                                .navigationTitle("Pair with a code")
                                .toolbar {
                                    ToolbarItem(placement: .cancellationAction) {
                                        Button("Close") { setup.pendingCodeError = nil }
                                    }
                                }
                        }
                    }
                }
            }
        }
    }
}

extension View {
    /// Presents the pairing sheet when the app opens a pairing link.
    public func sshSheets(_ setup: SSHSetup?) -> some View {
        modifier(SSHSheets(setup: setup))
    }
}
