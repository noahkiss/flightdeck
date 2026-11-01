import HabAPI
import SwiftUI
#if os(iOS)
import UIKit
#elseif os(macOS)
import AppKit
#endif

// MARK: - Voice mode

/// Voice mode over one session (app.md 13.1): opens a `VoiceModel`, holds
/// the hab's connection while open, and closes on End or when the model ends.
struct VoiceModeScreen: View {
    @State private var model: VoiceModel
    private let kit: VoiceKit
    private let habID: String
    private let close: (_ keyboard: Bool) -> Void

    init(chat: ChatModel, connection: HabConnection?, kit: VoiceKit, close: @escaping (_ keyboard: Bool) -> Void) {
        _model = State(initialValue: kit.voiceModel(chat: chat, connection: connection))
        self.kit = kit
        self.habID = chat.target.habID
        self.close = close
    }

    var body: some View {
        VoiceModeView(model: model, close: close)
            .task {
                kit.hold(habID, true)
                model.onEnd = { _ in }
                await model.start()
            }
            .onDisappear {
                kit.hold(habID, false)
                Task { await model.end() }
            }
    }
}

/// The full-screen voice view: level meter, session line, live words, the
/// state, a large approval card when one waits, and End, Mute and Keyboard.
public struct VoiceModeView: View {
    let model: VoiceModel
    let close: (_ keyboard: Bool) -> Void

    public init(model: VoiceModel, close: @escaping (_ keyboard: Bool) -> Void) {
        self.model = model
        self.close = close
    }

    public var body: some View {
        VStack(spacing: 20) {
            header
            Spacer(minLength: 8)
            LevelMeter(level: model.muted ? -160 : model.level, active: model.phase == .hearing)
                .frame(width: 160, height: 160)
                .accessibilityHidden(true)
            Text(stateLine)
                .font(.headline)
                .foregroundStyle(.secondary)
            if let tool = model.toolLine, model.phase == .waiting {
                Text(tool)
                    .font(.callout.monospaced())
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            ScrollView {
                Text(model.liveText.isEmpty ? " " : model.liveText)
                    .font(.title2)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: .infinity)
                    .textSelection(.enabled)
            }
            .frame(maxHeight: 200)
            if case .confirming = model.phase {
                Button("Send") { Task { await model.confirmSend() } }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.large)
            }
            if let r = model.request {
                VoiceApprovalCard(request: r, model: model)
            }
            if let n = model.notice {
                Text(n)
                    .font(.callout)
                    .foregroundStyle(.orange)
                    .multilineTextAlignment(.center)
            }
            Spacer(minLength: 8)
            controls
            footer
        }
        .padding(24)
        .frame(maxWidth: 640)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(.background)
        .contentShape(Rectangle())
        .onTapGesture { Task { await model.tap() } }
        .onChange(of: model.isEnded) { _, ended in
            if ended { close(false) }
        }
    }

    private var header: some View {
        VStack(spacing: 4) {
            Text(model.chat.context.title)
                .font(.title3.weight(.semibold))
                .lineLimit(1)
            Text(sessionLine)
                .font(.callout)
                .foregroundStyle(.secondary)
                .lineLimit(1)
        }
    }

    private var sessionLine: String {
        let c = model.chat.context
        let state = c.online ? c.agentStatus.rawValue : "offline"
        return [harnessName(c.harness), c.hostLabel, state].filter { !$0.isEmpty }.joined(separator: " · ")
    }

    private var stateLine: String {
        if model.muted { return "Muted" }
        switch model.phase {
        case .starting: return "Starting"
        case .listening: return "Listening"
        case .hearing: return "Hearing you"
        case .transcribing: return "Writing it down"
        case .confirming: return "Say send, or tap Send"
        case .waiting: return "Working"
        case .speaking: return "Speaking"
        case .asking: return "Reading the request"
        case .answering: return "Allow or deny?"
        case .tapToAnswer: return "Tap Allow or Deny"
        case .ended(let why): return why ?? "Ended"
        }
    }

    private var controls: some View {
        HStack(spacing: 28) {
            VoiceControl(title: "Keyboard", symbol: "keyboard") {
                Task {
                    await model.end()
                    close(true)
                }
            }
            VoiceControl(title: model.muted ? "Unmute" : "Mute", symbol: model.muted ? "mic.slash.fill" : "mic.fill") {
                model.setMuted(!model.muted)
            }
            VoiceControl(title: "End", symbol: "xmark", tint: .red) {
                Task {
                    await model.end()
                    close(false)
                }
            }
        }
    }

    @ViewBuilder private var footer: some View {
        VStack(spacing: 2) {
            if let e = model.engineLabel {
                Text("Text by \(e)")
            } else if let first = model.engines.first {
                Text("Speech recognition: \(first.label)")
            }
            if let f = model.engines.lastFallback {
                Text(f)
                    .lineLimit(2)
            } else {
                ForEach(SpeechEngineKind.allCases) { k in
                    if case .failed(let why)? = model.engines.status[k] {
                        Text("\(k.label): \(why)")
                            .lineLimit(2)
                    }
                }
            }
        }
        .font(.caption)
        .foregroundStyle(.secondary)
        .multilineTextAlignment(.center)
    }
}

/// A round control with a label under it.
struct VoiceControl: View {
    let title: String
    let symbol: String
    var tint: Color = .primary
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            VStack(spacing: 6) {
                Image(systemName: symbol)
                    .font(.title2)
                    .frame(width: 60, height: 60)
                    .background(.quaternary, in: Circle())
                    .foregroundStyle(tint)
                Text(title)
                    .font(.caption)
            }
        }
        .buttonStyle(.plain)
        .accessibilityLabel(title)
    }
}

/// A circle that grows with the level.
struct LevelMeter: View {
    let level: Float
    let active: Bool

    var body: some View {
        let norm = CGFloat(max(0, min(1, (level + 60) / 60)))
        ZStack {
            Circle()
                .fill(Color.accentColor.opacity(0.12))
            Circle()
                .fill(Color.accentColor.opacity(active ? 0.6 : 0.35))
                .scaleEffect(0.35 + 0.65 * norm)
                .animation(.easeOut(duration: 0.12), value: norm)
            Image(systemName: "waveform")
                .font(.system(size: 36, weight: .medium))
                .foregroundStyle(.white)
        }
    }
}

/// The permission request in large type, with Deny and Allow (app.md 13.3).
struct VoiceApprovalCard: View {
    let request: PermissionRequest
    let model: VoiceModel

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(title)
                .font(.title3.weight(.semibold))
            if let c = request.command {
                ScrollView {
                    Text(c)
                        .font(.body.monospaced())
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                }
                .frame(maxHeight: 120)
            } else if let files = request.files {
                Text(files.joined(separator: "\n"))
                    .font(.body.monospaced())
            }
            HStack(spacing: 12) {
                Button {
                    Task { await model.answerByTap(false) }
                } label: {
                    Text("Deny").frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
                Button {
                    Task { await model.answerByTap(true) }
                } label: {
                    Text("Allow").frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
            }
            .controlSize(.large)
        }
        .padding(14)
        .background(Color.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 12))
    }

    private var title: String {
        if let t = request.title, !t.isEmpty { return t }
        return request.tool.map { "Allow \($0)?" } ?? "Allow this request?"
    }
}

// MARK: - Opening voice mode

/// The session header's voice button, and the screen it opens. Off for a
/// session with no chat reader (app.md 13.6).
struct VoiceButton: View {
    let model: ChatModel
    @Binding var shown: Bool
    @Environment(\.voiceKit) private var kit
    @State private var denied = false

    var body: some View {
        if kit != nil {
            Button("Voice", systemImage: "waveform") { open() }
                .help("Voice mode")
                .disabled(!VoiceButton.allowed(model))
                .alert("Microphone access is off", isPresented: $denied) {
                    Button("Open Settings") { openPrivacySettings() }
                    Button("Cancel", role: .cancel) {}
                } message: {
                    Text("Turn on microphone access for Flightdeck to use voice mode and dictation.")
                }
        }
    }

    static func allowed(_ m: ChatModel) -> Bool {
        if case .unavailable = m.load { return false }
        return m.target.session != nil && m.context.harness != nil
    }

    private func open() {
        if VoiceAudio.permissionDenied {
            denied = true
        } else {
            shown = true
        }
    }
}

/// Opens the system's privacy settings for the microphone.
@MainActor
func openPrivacySettings() {
    #if os(iOS)
    if let u = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(u) }
    #elseif os(macOS)
    if let u = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Microphone") { NSWorkspace.shared.open(u) }
    #endif
}

extension View {
    /// Presents voice mode over the session: full screen on iPhone and iPad, a sheet on the Mac.
    func voiceMode(_ shown: Binding<Bool>, chat: ChatModel, keyboard: @escaping () -> Void) -> some View {
        modifier(VoiceModePresenter(shown: shown, chat: chat, keyboard: keyboard))
    }
}

struct VoiceModePresenter: ViewModifier {
    @Binding var shown: Bool
    let chat: ChatModel
    let keyboard: () -> Void
    @Environment(\.voiceKit) private var kit
    @Environment(\.habConnections) private var habs

    func body(content: Content) -> some View {
        #if os(iOS)
        content.fullScreenCover(isPresented: $shown) { screen }
        #else
        content.sheet(isPresented: $shown) {
            screen.frame(minWidth: 460, minHeight: 620)
        }
        #endif
    }

    @ViewBuilder private var screen: some View {
        if let kit {
            VoiceModeScreen(chat: chat, connection: habs.find(chat.target.habID), kit: kit) { toKeyboard in
                shown = false
                if toKeyboard { keyboard() }
            }
        }
    }
}

// MARK: - Dictation

/// Dictation's microphone button: tap starts, tap again stops (app.md 13.4).
struct DictationButton: View {
    let dictation: DictationModel?
    let start: () -> Void
    let stop: () -> Void

    var body: some View {
        let listening = dictation?.phase == .listening
        let busy = dictation?.phase == .transcribing
        Button {
            listening ? stop() : start()
        } label: {
            Image(systemName: listening ? "mic.circle.fill" : "mic")
                .foregroundStyle(listening ? Color.red : Color.accentColor)
        }
        .buttonStyle(.borderless)
        .disabled(busy)
        .accessibilityLabel(listening ? "Stop dictation" : "Dictate")
    }
}

/// Dictation into a pane: the text goes as one write when the owner stops,
/// without Enter (app.md 13.4).
struct PaneDictationButton: View {
    let pane: String
    let connection: HabConnection
    @Environment(\.voiceKit) private var kit
    @State private var dictation: DictationModel?
    @State private var denied = false

    var body: some View {
        if let kit {
            DictationButton(dictation: dictation) {
                if VoiceAudio.permissionDenied {
                    denied = true
                    return
                }
                let d = kit.dictation(connection: connection, session: nil)
                dictation = d
                Task { await d.start() }
            } stop: {
                guard let d = dictation else { return }
                Task {
                    if let text = await d.stop(), !text.isEmpty {
                        _ = try? await connection.call(HabMethods.paneInput, InputParams(data: Self.paneText(text), pane: pane))
                    }
                    dictation = nil
                }
            }
            .help("Dictate into the pane")
            .alert("Microphone access is off", isPresented: $denied) {
                Button("Open Settings") { openPrivacySettings() }
                Button("Cancel", role: .cancel) {}
            }
        }
    }

    /// The text as typed into the pane: control characters (ESC, newlines)
    /// become spaces, so dictation never runs a command or a key sequence.
    static func paneText(_ text: String) -> Data {
        let clean = String(text.unicodeScalars.map { CharacterSet.controlCharacters.contains($0) ? " " : Character($0) })
        return Data(clean.utf8)
    }
}

// MARK: - Settings

/// Settings › Voice: the speech engines in their order, the on-device
/// model's download, and the voice mode switches.
public struct VoiceSettingsView: View {
    @Bindable var settings: VoiceSettingsStore
    let parakeet: ParakeetHost

    public init(kit: VoiceKit) {
        self.settings = kit.settings
        self.parakeet = kit.parakeet
    }

    public var body: some View {
        Form {
            Section {
                Toggle("On the hab", isOn: engine(.hab))
                if parakeet.offered {
                    Toggle("On this device (Parakeet)", isOn: engine(.parakeet))
                    parakeetRow
                }
                Toggle("Apple speech", isOn: engine(.apple))
            } header: {
                Text("Speech recognition")
            } footer: {
                Text(settings.value.enabledEngines.isEmpty ? "Voice mode and dictation are off." : "Tried in this order.")
            }
            Section("Voice mode") {
                Toggle("Ask for a spoken summary", isOn: $settings.value.askSummary)
                Toggle("Spoken commands", isOn: $settings.value.spokenCommands)
                Toggle("Confirm before send", isOn: $settings.value.confirmBeforeSend)
                Toggle("Stop speech when I talk", isOn: $settings.value.bargeIn)
                Toggle("Noisy room", isOn: $settings.value.noisyRoom)
                Picker("Pause before send", selection: $settings.value.silence) {
                    ForEach([0.8, 1.2, 1.6, 2.0], id: \.self) { s in
                        Text(String(format: "%.1f s", s)).tag(s)
                    }
                }
            }
        }
        .formStyle(.grouped)
        .task { await parakeet.refresh() }
    }

    private func engine(_ k: SpeechEngineKind) -> Binding<Bool> {
        Binding(get: { settings.value.isOn(k) }, set: { settings.value.set(k, $0) })
    }

    @ViewBuilder private var parakeetRow: some View {
        if let p = parakeet.downloadProgress {
            ProgressView(value: p) {
                Text("Downloading the model")
            }
        } else if parakeet.isDownloaded {
            Button("Remove the model", role: .destructive) { Task { await parakeet.removeDownload() } }
        } else {
            Button("Download the model (about 450 MB)") { Task { await parakeet.download() } }
        }
        Text("Parakeet TDT 0.6B v2 by NVIDIA, CC-BY-4.0")
            .font(.caption)
            .foregroundStyle(.secondary)
        if let e = parakeet.lastError {
            Text(e)
                .font(.caption)
                .foregroundStyle(.red)
        }
    }
}
