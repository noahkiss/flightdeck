import HabAPI
import SwiftUI

extension EnvironmentValues {
    /// The token of the Capcom view a session view sits in; nil outside one (app.md 4.10).
    @Entry var capcomView: UUID? = nil
    /// The Capcom tab's subtitle while the session view is that tab's root; nil elsewhere.
    @Entry var capcomTab: String? = nil
}

/// The board's Capcom row (app.md 4.10): opens the standing Capcom conversation, and shows
/// its agent state as the board shows a session's.
struct CapcomEntryRow: View {
    let center: CapcomCenter
    var rows: [BoardRow] = []
    var now: Date = .now

    var body: some View {
        let e = center.entry
        let row = center.standingRow(in: rows)
        HStack(spacing: 10) {
            Image(systemName: "dot.radiowaves.left.and.right")
                .foregroundStyle(.tint)
                .frame(width: 20)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline) {
                    Text("Capcom")
                        .font(.body.weight(.medium))
                    Spacer(minLength: 8)
                    if let row {
                        HStack(spacing: 5) {
                            Circle()
                                .fill(row.stateColor)
                                .frame(width: 8, height: 8)
                                .accessibilityHidden(true)
                            Text(row.stateText(now: now))
                                .lineLimit(1)
                        }
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    }
                }
                Text(center.entryText(e))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            if e == .loading {
                ProgressView()
                    .controlSize(.small)
            }
        }
        .padding(.vertical, 2)
        .contentShape(Rectangle())
    }
}

/// The Capcom host picker (capcom.md 7.3): every hab in the app, the home marked. Choosing an
/// online hab with Capcom assigns it at once.
struct CapcomHostList: View {
    let center: CapcomCenter

    var body: some View {
        ForEach(center.hostChoices) { c in
            Button {
                Task { await center.assign(habID: c.id) }
            } label: {
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(c.label)
                            .foregroundStyle(c.choosable ? .primary : .secondary)
                        if let why = c.unavailable {
                            Text(why)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                    Spacer()
                    if center.assigning == c.id {
                        ProgressView()
                            .controlSize(.small)
                    } else if c.isHome {
                        Image(systemName: "checkmark")
                            .foregroundStyle(.tint)
                            .accessibilityLabel("Capcom host")
                    }
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .pointerHover()
            .disabled(!c.choosable || c.isHome || center.assigning != nil)
        }
    }
}

/// The Capcom host page in Settings.
struct CapcomHostSettingsView: View {
    let center: CapcomCenter
    /// The settings center, for the home hab's quiet hours; nil leaves the row out.
    var settings: SettingsCenter? = nil
    @Environment(\.rounds) private var rounds

    var body: some View {
        Form {
            Section {
                CapcomHostList(center: center)
            } header: {
                Text("Runs Capcom")
            } footer: {
                VStack(alignment: .leading, spacing: 4) {
                    if !center.hostChoices.contains(where: \.isHome) {
                        Text(center.entryText(center.entry))
                    }
                    if let f = center.assignFailure {
                        Text(f)
                            .foregroundStyle(.red)
                    }
                }
            }
            if let r = center.deviceContext {
                CapcomShareSettings(reporter: r)
            }
            if let rounds {
                CapcomJobsSection(center: rounds)
            }
            if let settings, case .ready(let home) = center.entry {
                CapcomQuietHoursSection(settings: settings, habID: home)
            }
            if let sites = center.sites {
                Section {
                    if sites.isEmpty {
                        Text("None").foregroundStyle(.secondary)
                    }
                    ForEach(sites, id: \.self) { s in
                        HStack {
                            Text(s)
                            Spacer()
                            Button("Remove", systemImage: "minus.circle", role: .destructive) {
                                Task { await center.removeSite(s) }
                            }
                            .labelStyle(.iconOnly)
                            .buttonStyle(.borderless)
                        }
                    }
                } header: {
                    Text("Allowed sites")
                } footer: {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("Capcom opens pages on these sites without asking.")
                        if let f = center.sitesFailure {
                            Text(f).foregroundStyle(.red)
                        }
                    }
                }
            }
        }
        .onAppear { center.pickerAppeared() }
        .task { await center.loadSites() }
    }
}

/// The inline host picker of the Capcom view with no host assigned: the suggested hab
/// preselected, then Assign.
struct CapcomAssignInline: View {
    let center: CapcomCenter
    @State private var choice: String?

    var body: some View {
        let options = center.hostChoices.filter(\.choosable)
        VStack(spacing: 10) {
            if options.isEmpty {
                Text("No online hab runs Capcom.")
                    .foregroundStyle(.secondary)
            } else {
                Picker("Capcom host", selection: $choice) {
                    ForEach(options) { Text($0.label).tag(Optional($0.id)) }
                }
                .pickerStyle(.menu)
                .fixedSize()
                Button {
                    guard let id = choice else { return }
                    Task { await center.assign(habID: id) }
                } label: {
                    if center.assigning != nil {
                        ProgressView().controlSize(.small)
                    } else {
                        Text("Assign")
                    }
                }
                .buttonStyle(.borderedProminent)
                .disabled(choice == nil || center.assigning != nil)
            }
            if let f = center.assignFailure {
                Text(f)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .multilineTextAlignment(.center)
            }
            Button("Check again") { Task { await center.refreshHome() } }
                .buttonStyle(.borderless)
        }
        .onAppear {
            center.pickerAppeared()
            if choice == nil { choice = center.suggestedHost }
        }
        .onChange(of: options.map(\.id)) { _, ids in
            if choice == nil || !ids.contains(choice!) { choice = center.suggestedHost }
        }
    }
}

/// The Capcom view (app.md 4.10): the standing conversation on the Capcom home, in the
/// session view's Capcom style, or why it cannot open.
struct CapcomScreen: View {
    let fleet: Fleet
    let compact: Bool
    let close: (() -> Void)?
    /// The Capcom tab's root on a phone (app.md 3.8, 4.10): a large "Capcom" title with where
    /// it runs, and the pacing line over the composer.
    var tab = false
    /// This view's token: each window's Capcom view counts on its own.
    @State private var token = UUID()

    private var center: CapcomCenter { fleet.capcom }

    var body: some View {
        // A ZStack, not a Group: the appear and disappear hooks stay on this container and
        // do not fire again when the content swaps between the state and the conversation.
        ZStack {
            if let m = center.standing {
                VStack(spacing: 0) {
                    if let r = center.deviceContext { CapcomPermissionAsk(reporter: r) }
                    if !tab { CapcomPacingLine(center: center, usage: fleet.usage) }
                    SessionView(model: m, compact: compact, close: close)
                        .environment(\.capcomTab, tab ? center.tabSubtitle(center.entry) : nil)
                        .environment(\.chatAboveComposer, tab ? AnyView(CapcomPacingLine(center: center, usage: fleet.usage, plain: true)) : nil)
                }
                    .id(m.target)
                    .environment(\.capcomEarlier, center.earlier)
                    .onAppear {
                        fleet.focus.show(m.target)
                        // A reply a notification could not send waits in the composer (7.6).
                        center.takeUnsent(into: m)
                        Task { await center.loadEarlier() }
                    }
                    .onDisappear { fleet.focus.hide(m.target) }
            } else {
                state
            }
        }
        .environment(\.capcomView, token)
        // Opens on appearing; the center opens again when the home comes online or moves,
        // or the standing conversation exits.
        .onAppear { center.viewAppeared(token) }
        .onDisappear { center.viewDisappeared(token) }
    }

    @ViewBuilder
    private var state: some View {
        if compact {
            stateContent
                .navigationTitle("Capcom")
                .modifier(TabSubtitle(text: tab ? center.tabSubtitle(center.entry) : nil))
                #if os(iOS)
                .navigationBarTitleDisplayMode(tab ? .large : .inline)
                #endif
        } else {
            VStack(spacing: 0) {
                HStack {
                    Text("Capcom")
                        .font(.headline)
                    Spacer()
                    if let close {
                        Button("Close", systemImage: "xmark", action: close)
                            .labelStyle(.iconOnly)
                            .buttonStyle(.borderless)
                            .keyboardShortcut(.cancelAction)
                    }
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
                Divider()
                stateContent
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
    }

    @ViewBuilder
    private var stateContent: some View {
        let e = center.entry
        if let failure = center.standingFailure, case .ready = e, !center.opening {
            ContentUnavailableView {
                Label("Could not open Capcom", systemImage: "exclamationmark.triangle")
            } description: {
                Text(failure)
            } actions: {
                Button("Try again") { Task { await center.openStanding() } }
            }
        } else {
            switch e {
            case .loading, .ready:
                ProgressView(e == .loading ? "Looking for the Capcom host" : "Opening Capcom")
            case .unassigned:
                ContentUnavailableView {
                    Label("No Capcom host", systemImage: "dot.radiowaves.left.and.right")
                } description: {
                    Text("Choose the hab that runs Capcom.")
                } actions: {
                    CapcomAssignInline(center: center)
                }
            case .noHabOnline:
                ContentUnavailableView("No hab online", systemImage: "wifi.slash",
                                       description: Text("Capcom opens when a hab is online."))
            case .notInApp:
                ContentUnavailableView {
                    Label("Capcom host not in this app", systemImage: "questionmark.circle")
                } description: {
                    Text("The hab that runs Capcom is not in this app.")
                } actions: {
                    Button("Check again") { Task { await center.refreshHome() } }
                }
            case .offline(let id):
                ContentUnavailableView("\(center.habs.label(id)) is offline", systemImage: "wifi.slash",
                                       description: Text("Capcom runs on \(center.habs.label(id)). It opens when that hab is online."))
            }
        }
    }
}

/// The usage governor's line above the Capcom thread (capcom-service.md 10.1), while Capcom is
/// not at `ok`: "Small model for your messages · 40% of the week used, share 30%". Nothing shows
/// when the home's catalog has no governor section. A tap opens the usage sheet.
struct CapcomPacingLine: View {
    let center: CapcomCenter
    let usage: UsageCenter?
    /// Plain text over the composer (the Capcom tab), not a bar over the thread.
    var plain = false

    var body: some View {
        TimelineView(.periodic(from: .now, by: 30)) { ctx in
            if let line = center.governorLine(now: ctx.date) {
                Button {
                    usage?.book.present(account: nil)
                } label: {
                    HStack(spacing: 6) {
                        Image(systemName: "gauge.with.dots.needle.67percent")
                        Text(line)
                            .lineLimit(2)
                        Spacer(minLength: 0)
                    }
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 6)
                    .frame(maxWidth: plain ? nil : .infinity, alignment: .leading)
                    .background(plain ? AnyShapeStyle(.clear) : AnyShapeStyle(.bar))
                    // Over the composer the line sits in the bar the thread scrolls under: on glass (app.md 4.2).
                    .glassEffect(plain ? .regular.interactive() : .identity, in: .capsule)
                }
                .buttonStyle(.plain)
                // Interactive glass already reacts to the pointer over the composer.
                .pointerHover(!plain)
                // Over the composer it sits in the composer's padded glass container.
                .frame(maxWidth: .infinity, alignment: .leading)
                .disabled(usage == nil)
                .accessibilityHint(usage == nil ? "" : "Opens usage")
            }
        }
    }
}
