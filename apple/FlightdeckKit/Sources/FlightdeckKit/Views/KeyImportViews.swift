import NIOSSH
import SwiftUI
import UniformTypeIdentifiers
#if os(iOS)
import UIKit
#else
import AppKit
#endif

extension SSHSetup {
    /// How Settings names a key.
    func keyName(_ id: String) -> String {
        if id == DeviceKeys.defaultID { return "This device's key" }
        guard let k = importedKeys.first(where: { $0.id == id }) else { return "No key" }
        return k.comment.isEmpty ? "\(k.type.label) key" : "\(k.type.label), \(k.comment)"
    }
}

/// Settings › SSH › Imported keys: each key with its detail, and the import
/// button (app.md 10.4).
struct ImportedKeysSection: View {
    let setup: SSHSetup
    @State private var importing = false

    var body: some View {
        Section {
            ForEach(setup.importedKeys, id: \.id) { k in
                NavigationLink {
                    ImportedKeyDetailView(setup: setup, key: k)
                } label: {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(setup.keyName(k.id))
                        Text(k.fingerprint)
                            .font(.caption2.monospaced())
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                }
            }
            if let e = setup.importedKeysError {
                Text(e).foregroundStyle(.secondary)
            }
            Button("Import a key") { importing = true }
        } header: {
            Text("Imported keys")
        }
        .sheet(isPresented: $importing) {
            NavigationStack {
                KeyImportView(setup: setup, pending: nil) { importing = false }
            }
            #if os(macOS)
            .frame(minWidth: 460, minHeight: 440)
            #endif
        }
    }
}

/// One imported key: type, fingerprint, comment, public key line, the habs
/// that use it, Face ID and Delete.
struct ImportedKeyDetailView: View {
    let setup: SSHSetup
    let key: ImportedKey
    @Environment(\.dismiss) private var dismiss
    @State private var confirmDelete = false
    @State private var message: String?

    var body: some View {
        Form {
            Section {
                LabeledContent("Type", value: key.type.label)
                if !key.comment.isEmpty {
                    LabeledContent("Comment", value: key.comment)
                }
                VStack(alignment: .leading, spacing: 4) {
                    Text("Fingerprint").font(.caption).foregroundStyle(.secondary)
                    Text(key.fingerprint).font(.caption.monospaced()).textSelection(.enabled)
                }
                PublicKeyLine(line: key.authorizedKeysLine)
                #if os(iOS)
                Toggle("Require Face ID for this key", isOn: Binding(
                    get: { setup.importedKeys.first { $0.id == key.id }?.requiresBiometry ?? key.requiresBiometry },
                    set: { on in
                        Task {
                            do { try await setup.setBiometry(on, forImported: key.id) } catch { message = "\(error)" }
                        }
                    }
                ))
                #endif
            }
            Section("Used by") {
                let users = setup.entries(using: key.id)
                if users.isEmpty {
                    Text("No hab uses this key.").foregroundStyle(.secondary)
                }
                ForEach(users) { e in Text(e.label ?? e.destination) }
            }
            Section {
                Button("Delete key", role: .destructive) { confirmDelete = true }
            }
            if let message {
                Section { Text(message).font(.callout) }
            }
        }
        .formStyle(.grouped)
        .navigationTitle(key.type.label)
        .confirmationDialog("Delete this key?", isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("Delete key", role: .destructive) {
                do {
                    try setup.deleteImportedKey(key.id)
                    dismiss()
                } catch {
                    message = "\(error)"
                }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Habs that use it stop connecting until you choose another key.")
        }
    }
}

/// Import an OpenSSH private key from paste, Files or another app: read it,
/// ask for the passphrase when it has one, store the key, show its public
/// line. The text and passphrase are dropped once the key is stored.
struct KeyImportView: View {
    let setup: SSHSetup
    let pending: PendingKeyFile?
    let done: () -> Void

    @State private var text = ""
    @State private var file: OpenSSHKeyFile?
    @State private var passphrase = ""
    @State private var error: String?
    @State private var working = false
    @State private var picking = false
    @State private var result: (key: ImportedKey, replaced: Bool)?
    /// The running import, cancelled when the sheet goes away.
    @State private var importTask: Task<Void, Never>?

    var body: some View {
        Form {
            if let r = result {
                Section {
                    LabeledContent("Type", value: r.key.type.label)
                    VStack(alignment: .leading, spacing: 4) {
                        Text("Fingerprint").font(.caption).foregroundStyle(.secondary)
                        Text(r.key.fingerprint).font(.caption.monospaced()).textSelection(.enabled)
                    }
                    PublicKeyLine(line: r.key.authorizedKeysLine)
                } header: {
                    Text("Imported")
                } footer: {
                    Text(r.replaced ? "This key was already on this device; it is stored once." : "Choose this key for a hab in Habs over SSH. Hosts that already trust it need no pairing.")
                }
            } else if let f = file {
                Section {
                    LabeledContent("Type", value: f.type.label)
                    if let pk = f.publicKey {
                        VStack(alignment: .leading, spacing: 4) {
                            Text("Fingerprint").font(.caption).foregroundStyle(.secondary)
                            Text(SSHFingerprint.of(pk)).font(.caption.monospaced()).textSelection(.enabled)
                        }
                    }
                    if f.isEncrypted {
                        SecureField("Passphrase", text: $passphrase)
                            .onSubmit { startImport() }
                    }
                    Button("Import") { startImport() }
                        .disabled(working || (f.isEncrypted && passphrase.isEmpty))
                    Button("Choose another key") { reset() }
                } header: {
                    Text("Key")
                }
            } else {
                Section {
                    TextField("Paste the private key", text: $text, axis: .vertical)
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                        .autocorrectionDisabled()
                        .font(.caption.monospaced())
                        .lineLimit(4...10)
                    HStack {
                        Button("Paste") { pasteFromPasteboard() }
                        Spacer()
                        Button("Choose a file") { picking = true }
                    }
                    .buttonStyle(.borderless)
                    Button("Read key") { read(Data(text.utf8)) }
                        .disabled(text.isEmpty)
                } header: {
                    Text("Private key")
                } footer: {
                    Text("An imported key also opens every other host that trusts it. The generated key exists only on this device.")
                }
            }
            if let error {
                Section { Text(error).foregroundStyle(.red).font(.callout).textSelection(.enabled) }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Import a key")
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button(result == nil ? "Cancel" : "Done", action: finish)
            }
            if working {
                ToolbarItem(placement: .confirmationAction) { ProgressView() }
            }
        }
        .fileImporter(isPresented: $picking, allowedContentTypes: [.data, .text]) { r in
            switch r {
            case .success(let url):
                let scoped = url.startAccessingSecurityScopedResource()
                defer { if scoped { url.stopAccessingSecurityScopedResource() } }
                do {
                    read(try OpenSSHKeyFile.read(contentsOf: url))
                } catch let e as KeyImportError {
                    self.error = e.description
                } catch {
                    self.error = "Could not read the file: \(error.localizedDescription)"
                }
            case .failure(let e):
                error = e.localizedDescription
            }
        }
        .onAppear {
            if let p = pending, file == nil, result == nil {
                if let e = p.error { error = e } else { read(p.data) }
            }
        }
        .onDisappear { importTask?.cancel() }
    }

    private func startImport() {
        guard importTask == nil else { return }
        importTask = Task {
            await store()
            importTask = nil
        }
    }

    private func read(_ data: Data) {
        do {
            file = try OpenSSHKeyFile.parse(data)
            error = nil
            text = ""
        } catch {
            self.error = "\(error)"
        }
    }

    private func store() async {
        guard let f = file, !working else { return }
        working = true
        defer { working = false }
        do {
            result = try await setup.importKey(f, passphrase: f.isEncrypted ? passphrase : nil)
            file = nil
            passphrase = ""
            error = nil
        } catch let e as KeyImportError where e == .wrongPassphrase {
            passphrase = ""
            error = "\(e)"
        } catch is CancellationError {
            return
        } catch {
            self.error = "\(error)"
        }
    }

    private func reset() {
        file = nil
        passphrase = ""
        error = nil
    }

    private func finish() {
        importTask?.cancel()
        text = ""
        passphrase = ""
        file = nil
        done()
    }

    private func pasteFromPasteboard() {
        #if os(iOS)
        let s = UIPasteboard.general.string
        #else
        let s = NSPasteboard.general.string(forType: .string)
        #endif
        guard let s, !s.isEmpty else {
            error = "The clipboard holds no text."
            return
        }
        read(Data(s.utf8))
    }
}

/// The key an entry signs with, and a menu to choose another.
struct HabKeyMenu: View {
    let setup: SSHSetup
    let entry: SSHHabEntry
    @Binding var message: String?

    var body: some View {
        Menu {
            Button(setup.keyName(DeviceKeys.defaultID)) { choose(DeviceKeys.defaultID) }
            ForEach(setup.importedKeys, id: \.id) { k in
                Button(setup.keyName(k.id)) { choose(k.id) }
            }
        } label: {
            switch setup.keyState(entry) {
            case .present:
                Text("Key: " + setup.keyName(entry.keyID)).font(.caption)
            case .missing:
                Text("No key: choose one").font(.caption).foregroundStyle(.red)
            case .unavailable(let why):
                Text("Key: " + setup.keyName(entry.keyID) + " (" + why + ")").font(.caption).foregroundStyle(.secondary)
            }
        }
        .menuStyle(.borderlessButton)
        .fixedSize()
    }

    private func choose(_ id: String) {
        do { try setup.setKey(id, for: entry) } catch { message = "\(error)" }
    }
}
