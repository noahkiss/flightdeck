import ActivityKit
import AppIntents
import FlightdeckShared
import SwiftUI
import WidgetKit

@main
struct FlightdeckWidgets: WidgetBundle {
    var body: some Widget {
        SessionActivityWidget()
    }
}

/// The Live Activity of one session (app.md 9.1): the session, its hab and
/// harness, the state and how long it has held, the latest event, and Allow
/// and Deny while a request is open. The hab updates it by push.
struct SessionActivityWidget: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: SessionActivityAttributes.self) { ctx in
            LockScreenView(attributes: ctx.attributes, state: ctx.state)
                .padding(14)
                .widgetURL(link(ctx.attributes, ctx.state))
        } dynamicIsland: { ctx in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    Label(stateWordOf(ctx.state), systemImage: stateSymbol(ctx.state))
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(stateTint(ctx.state))
                }
                DynamicIslandExpandedRegion(.trailing) {
                    Text(ctx.attributes.habLabel)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                DynamicIslandExpandedRegion(.center) {
                    Text(ctx.state.title)
                        .font(.callout.weight(.medium))
                        .lineLimit(1)
                }
                DynamicIslandExpandedRegion(.bottom) {
                    if ctx.state.buttons {
                        AnswerButtons(attributes: ctx.attributes, state: ctx.state)
                    } else if let d = ctx.state.detail {
                        Text(d).font(.caption).lineLimit(1)
                    }
                }
            } compactLeading: {
                Image(systemName: stateSymbol(ctx.state))
                    .foregroundStyle(stateTint(ctx.state))
            } compactTrailing: {
                Text(stateWordOf(ctx.state))
                    .font(.caption2)
            } minimal: {
                Image(systemName: stateSymbol(ctx.state))
                    .foregroundStyle(stateTint(ctx.state))
            }
            .widgetURL(link(ctx.attributes, ctx.state))
        }
    }
}

struct LockScreenView: View {
    let attributes: SessionActivityAttributes
    let state: SessionActivityAttributes.ContentState

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: stateSymbol(state))
                    .foregroundStyle(stateTint(state))
                Text(attributes.project ?? attributes.name)
                    .font(.caption.weight(.semibold))
                    .lineLimit(1)
                Text("· \(attributes.habLabel)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                Spacer()
                Text(stateWordOf(state))
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(stateTint(state))
                if let since = state.sinceDate {
                    Text(since, style: .timer)
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: 52, alignment: .trailing)
                }
            }
            Text(state.title)
                .font(.headline)
                .lineLimit(2)
            if let d = state.detail {
                Text(d)
                    .font(.callout.monospaced())
                    .foregroundStyle(.tint)
                    .lineLimit(1)
            }
            if state.buttons {
                AnswerButtons(attributes: attributes, state: state)
            }
            if state.others > 0 {
                Text("\(state.others) more need you")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

/// Deny and Allow, equal size, Allow on the right (app.md 7.2). Each runs its
/// intent in the app's process; the hab drops the buttons once the request
/// resolves anywhere (7.5). Allow asks for Face ID or the passcode unless the
/// owner turned on "Approve from the lock screen without unlocking".
struct AnswerButtons: View {
    let attributes: SessionActivityAttributes
    let state: SessionActivityAttributes.ContentState

    var body: some View {
        if let pane = state.pane, let request = state.request {
            HStack(spacing: 10) {
                Button(intent: ActivityDenyIntent(hab: attributes.hab, pane: pane, request: request)) {
                    Text("Deny").frame(maxWidth: .infinity)
                }
                .tint(.red)
                if SharedSettings.allowWithoutUnlock() {
                    Button(intent: ActivityAllowLockedIntent(hab: attributes.hab, pane: pane, request: request)) {
                        Text("Allow").frame(maxWidth: .infinity)
                    }
                    .tint(.green)
                } else {
                    Button(intent: ActivityAllowIntent(hab: attributes.hab, pane: pane, request: request)) {
                        Text("Allow").frame(maxWidth: .infinity)
                    }
                    .tint(.green)
                }
            }
            .buttonStyle(.borderedProminent)
        }
    }
}

private func link(_ a: SessionActivityAttributes, _ s: SessionActivityAttributes.ContentState) -> URL {
    RequestLink.url(hab: a.hab, pane: s.pane, request: s.buttons ? s.request : nil, session: a.session)
}

/// One word for the state (app.md 9.1).
private func stateWordOf(_ s: SessionActivityAttributes.ContentState) -> String {
    switch s.state {
    case "blocked": return s.kind == "question" ? "Question" : s.kind == "auth" ? "Sign in" : "Needs you"
    case "working": return "Working"
    case "done": return "Finished"
    case "error": return "Stopped"
    case "exited": return "Exited"
    default: return "Idle"
    }
}

private func stateSymbol(_ s: SessionActivityAttributes.ContentState) -> String {
    switch s.state {
    case "blocked": return "hand.raised.fill"
    case "working": return "gearshape.2.fill"
    case "done": return "checkmark.circle.fill"
    case "error": return "exclamationmark.triangle.fill"
    default: return "pause.circle.fill"
    }
}

private func stateTint(_ s: SessionActivityAttributes.ContentState) -> Color {
    switch s.state {
    case "blocked": return .orange
    case "working": return .blue
    case "done": return .green
    case "error": return .red
    default: return .secondary
    }
}
