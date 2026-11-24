import AppIntents
import FlightdeckShared
import SwiftUI
import WidgetKit

/// The needs-you widget (app.md 15.4): how many sessions wait for the owner, the oldest first,
/// with Deny for a permission request and a tap that opens it. It reads the snapshot the app
/// writes to the App Group and never reaches a hab itself. It shows no command, so it offers no
/// Allow: Allow stays where the command shows.
struct NeedsYouWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: NeedsYouSnapshot.widgetKind, provider: NeedsYouProvider()) { entry in
            NeedsYouWidgetView(entry: entry)
                .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Needs you")
        .description("Sessions that wait for you, with Deny for a request.")
        .supportedFamilies([.systemSmall, .systemMedium, .systemLarge, .accessoryRectangular, .accessoryInline])
    }
}

struct NeedsYouEntry: TimelineEntry {
    var date: Date
    var snapshot: NeedsYouSnapshot?
}

struct NeedsYouProvider: TimelineProvider {
    func placeholder(in context: Context) -> NeedsYouEntry {
        NeedsYouEntry(date: Date(), snapshot: nil)
    }

    func getSnapshot(in context: Context, completion: @escaping (NeedsYouEntry) -> Void) {
        completion(NeedsYouEntry(date: Date(), snapshot: NeedsYouSnapshot.load()))
    }

    /// One entry; the app reloads the widget when what it shows changes.
    func getTimeline(in context: Context, completion: @escaping (Timeline<NeedsYouEntry>) -> Void) {
        completion(Timeline(entries: [NeedsYouEntry(date: Date(), snapshot: NeedsYouSnapshot.load())], policy: .never))
    }
}

struct NeedsYouWidgetView: View {
    let entry: NeedsYouEntry
    @Environment(\.widgetFamily) private var family

    private var items: [NeedsYouSnapshot.Item] { entry.snapshot?.items ?? [] }
    private var count: Int { entry.snapshot?.count ?? 0 }

    var body: some View {
        content
            .widgetURL(items.first?.url ?? URL(string: "flightdeck://board"))
    }

    @ViewBuilder
    private var content: some View {
        switch family {
        case .accessoryInline:
            Text(count == 0 ? "Nothing needs you" : "\(count) need you")
        case .accessoryRectangular:
            VStack(alignment: .leading) {
                Text(count == 0 ? "Nothing needs you" : "\(count) need you").font(.headline)
                if let first = items.first {
                    // Hidden while the device is locked: a session's name can say what it works on.
                    Text(first.title).lineLimit(1).privacySensitive()
                }
            }
        default:
            VStack(alignment: .leading, spacing: 6) {
                header
                if entry.snapshot == nil {
                    Text("Open Flightdeck").font(.caption).foregroundStyle(.secondary)
                } else if items.isEmpty {
                    Spacer(minLength: 0)
                    Label("Nothing needs you", systemImage: "checkmark.circle")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Spacer(minLength: 0)
                } else {
                    ForEach(items.prefix(rows)) { item in
                        NeedsYouRow(item: item, buttons: family != .systemSmall)
                    }
                    Spacer(minLength: 0)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        }
    }

    private var rows: Int {
        switch family {
        case .systemSmall: return 1
        case .systemMedium: return 2
        default: return NeedsYouSnapshot.maxItems
        }
    }

    private var header: some View {
        HStack(alignment: .firstTextBaseline) {
            Text(count == 0 ? "Needs you" : "\(count) need you")
                .font(.headline)
            Spacer(minLength: 4)
            if let s = entry.snapshot {
                // The app writes only while connected: say how old the list is.
                Text(s.writtenAt, style: .time)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

/// One session: what it waits for, its name and computer, and Deny for a request.
struct NeedsYouRow: View {
    let item: NeedsYouSnapshot.Item
    let buttons: Bool

    var body: some View {
        HStack(spacing: 8) {
            Link(destination: item.url) {
                HStack(spacing: 6) {
                    Image(systemName: symbol)
                        .foregroundStyle(item.kind == .approval ? .orange : .secondary)
                        .accessibilityHidden(true)
                    VStack(alignment: .leading, spacing: 0) {
                        Text(item.title).font(.caption.weight(.semibold)).lineLimit(1)
                        Text(item.habLabel).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
                    }
                    Spacer(minLength: 0)
                }
            }
            .accessibilityLabel("\(kindText): \(item.title) on \(item.habLabel)")
            if buttons, item.kind == .approval, let pane = item.pane, let request = item.request {
                // Deny needs no unlock and no command to judge (app.md 7.6); it runs in the app.
                Button(role: .destructive, intent: ActivityDenyIntent(hab: item.hab, pane: pane, request: request)) {
                    Label("Deny", systemImage: "xmark")
                }
                .labelStyle(.iconOnly)
                .buttonStyle(.bordered)
                .accessibilityLabel("Deny \(item.title)")
            }
        }
    }

    private var symbol: String {
        switch item.kind {
        case .approval: return "hand.raised"
        case .question: return "questionmark.bubble"
        case .other: return "exclamationmark.circle"
        }
    }

    private var kindText: String {
        switch item.kind {
        case .approval: return "Needs permission"
        case .question: return "Asks a question"
        case .other: return "Needs you"
        }
    }
}

/// "Talk to Capcom" for Control Center, the Lock Screen and the Action button (app.md 15.4).
struct TalkToCapcomControl: ControlWidget {
    static let kind = "flightdeck.control.capcom"

    var body: some ControlWidgetConfiguration {
        StaticControlConfiguration(kind: Self.kind) {
            ControlWidgetButton(action: AskCapcomIntent()) {
                Label("Talk to Capcom", systemImage: "dot.radiowaves.left.and.right")
            }
        }
        .displayName("Talk to Capcom")
        .description("Opens Capcom in Flightdeck.")
    }
}
