import HabAPI
import SwiftUI

/// The board's Capcom row (app.md 4.10): opens the standing Capcom conversation.
struct CapcomEntryRow: View {
    let center: CapcomCenter

    var body: some View {
        let e = center.entry
        HStack(spacing: 10) {
            Image(systemName: "dot.radiowaves.left.and.right")
                .foregroundStyle(.tint)
                .frame(width: 20)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                Text("Capcom")
                    .font(.body.weight(.medium))
                Text(center.entryText(e))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            Spacer(minLength: 8)
            if e == .loading {
                ProgressView()
                    .controlSize(.small)
            }
        }
        .padding(.vertical, 2)
        .contentShape(Rectangle())
    }
}

/// The Capcom view (app.md 4.10): the standing conversation on the Capcom home, in the
/// session view's Capcom style, or why it cannot open.
struct CapcomScreen: View {
    let fleet: Fleet
    let compact: Bool
    let close: (() -> Void)?

    private var center: CapcomCenter { fleet.capcom }

    var body: some View {
        Group {
            if let m = center.standing {
                SessionView(model: m, compact: compact, close: close)
                    .id(m.target)
                    .onAppear { fleet.focus.show(m.target) }
                    .onDisappear { fleet.focus.hide(m.target) }
            } else {
                state
            }
        }
        // Opens on appearing; the center opens again when the home comes online or moves,
        // or the standing conversation exits.
        .onAppear { center.viewAppeared() }
        .onDisappear { center.viewDisappeared() }
    }

    @ViewBuilder
    private var state: some View {
        if compact {
            stateContent
                .navigationTitle("Capcom")
                #if os(iOS)
                .navigationBarTitleDisplayMode(.inline)
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
                    Text("Assign a hab with gff-hab capcom assign.")
                } actions: {
                    Button("Check again") { Task { await center.refreshHome() } }
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
