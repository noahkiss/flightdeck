import AppIntents
import FlightdeckShared
import SwiftUI
import WidgetKit

/// The usage widget (app.md 15.1): small shows one account as a ring,
/// medium two accounts, the Lock Screen gauge the tightest window. It reads
/// the snapshot the app writes to the App Group and never reaches a hab.
struct UsageWidget: Widget {
    var body: some WidgetConfiguration {
        AppIntentConfiguration(kind: UsageSnapshot.widgetKind, intent: SelectUsageAccount.self, provider: UsageProvider()) { entry in
            UsageWidgetView(entry: entry)
                .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Usage")
        .description("Usage limits of a harness account.")
        .supportedFamilies([.systemSmall, .systemMedium, .accessoryCircular])
    }
}

// MARK: - Configuration

struct UsageAccountEntity: AppEntity {
    var id: String
    var name: String

    static var typeDisplayRepresentation: TypeDisplayRepresentation = "Account"
    static var defaultQuery = UsageAccountQuery()
    var displayRepresentation: DisplayRepresentation { DisplayRepresentation(title: "\(name)") }

    init(_ a: UsageAccountReading) {
        id = a.key
        name = [a.harnessName, a.label, "· " + a.habLabel].compactMap { $0 }.joined(separator: " ")
    }
}

struct UsageAccountQuery: EntityQuery {
    func entities(for identifiers: [String]) async throws -> [UsageAccountEntity] {
        all().filter { identifiers.contains($0.id) }
    }

    func suggestedEntities() async throws -> [UsageAccountEntity] {
        all()
    }

    private func all() -> [UsageAccountEntity] {
        let accounts = UsageSnapshot.load()?.accounts ?? []
        return UsageMath.byTightest(accounts, now: Date()).map(UsageAccountEntity.init)
    }
}

struct SelectUsageAccount: WidgetConfigurationIntent {
    static var title: LocalizedStringResource = "Account"
    static var description = IntentDescription("Pick the account the widget shows. With none, it shows the account nearest its limit.")

    @Parameter(title: "Account")
    var account: UsageAccountEntity?
}

// MARK: - Timeline

struct UsageEntry: TimelineEntry {
    var date: Date
    var snapshot: UsageSnapshot?
    var selected: String?

    /// The accounts to show: the chosen one first, then the tightest others.
    var pick: (accounts: [UsageAccountReading], missing: Bool) {
        UsageWidgetPick.accounts(snapshot?.accounts ?? [], selected: selected, now: date)
    }
}

struct UsageProvider: AppIntentTimelineProvider {
    func placeholder(in context: Context) -> UsageEntry {
        UsageEntry(date: Date(), snapshot: nil, selected: nil)
    }

    func snapshot(for configuration: SelectUsageAccount, in context: Context) async -> UsageEntry {
        UsageEntry(date: Date(), snapshot: UsageSnapshot.load(), selected: configuration.account?.id)
    }

    /// One entry now, and one at each moment the widget's text changes on its
    /// own: a reset time passing and a reading turning stale. Countdowns tick
    /// between entries with no reload.
    func timeline(for configuration: SelectUsageAccount, in context: Context) async -> Timeline<UsageEntry> {
        let now = Date()
        let snap = UsageSnapshot.load()
        var moments: Set<Date> = []
        for a in snap?.accounts ?? [] {
            for w in a.windows {
                if let r = w.resetsAt, r > now { moments.insert(r) }
                let stale = w.readAt.addingTimeInterval(min(UsageMath.staleAfter, UsageMath.length(of: w.name) ?? UsageMath.staleAfter))
                if stale > now { moments.insert(stale) }
            }
        }
        let later = moments.sorted().prefix(12).map { UsageEntry(date: $0.addingTimeInterval(1), snapshot: snap, selected: configuration.account?.id) }
        // With no moment ahead, nothing changes on its own: the app reloads on new readings.
        return Timeline(entries: [UsageEntry(date: now, snapshot: snap, selected: configuration.account?.id)] + later, policy: later.isEmpty ? .never : .atEnd)
    }
}

// MARK: - Views

struct UsageWidgetView: View {
    let entry: UsageEntry
    @Environment(\.widgetFamily) private var family

    var body: some View {
        let (accounts, missing) = entry.pick
        Group {
            if entry.snapshot == nil {
                Text("Open Flightdeck")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else if missing {
                Text("Account not reporting")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else if accounts.isEmpty {
                Text("No usage readings")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                switch family {
                case .accessoryCircular:
                    UsageGauge(accounts: accounts, now: entry.date)
                case .systemMedium:
                    HStack(spacing: 12) {
                        UsageAccountTile(account: accounts[0], now: entry.date)
                        if accounts.count > 1 {
                            Divider()
                            UsageAccountTile(account: accounts[1], now: entry.date)
                        }
                    }
                default:
                    UsageAccountTile(account: accounts[0], now: entry.date)
                }
            }
        }
        .widgetURL(UsageLink.url(account: family == .accessoryCircular ? nil : accounts.first?.key))
    }
}

/// One account: the ring for its tightest window, the window and countdown, the state.
struct UsageAccountTile: View {
    let account: UsageAccountReading
    let now: Date

    var body: some View {
        let w = UsageMath.tightest(account, now: now)
        let stale = UsageMath.isStale(account, now: now)
        VStack(alignment: .leading, spacing: 4) {
            Text([account.harnessName, account.label].compactMap { $0 }.joined(separator: " "))
                .font(.caption.weight(.semibold))
                .lineLimit(1)
            if let w {
                HStack(alignment: .center, spacing: 8) {
                    UsageRing(pct: UsageMath.effectivePct(w, now: now), stale: stale || UsageMath.isStale(w, now: now))
                        .frame(width: 52, height: 52)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(w.name).font(.caption.weight(.medium))
                        if let r = w.resetsAt, r > now {
                            Text(r, style: .relative)
                                .font(.caption2.monospacedDigit())
                                .foregroundStyle(.secondary)
                                .lineLimit(1)
                        }
                    }
                }
                Text(stale ? "No recent reading" : UsageText.short(w, now: now))
                    .font(.caption2)
                    .foregroundStyle(stale ? Color.secondary : UsageMath.isFull(w) ? Color.red : Color.secondary)
                    .lineLimit(1)
            } else {
                Text("No limits reported")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            if stale {
                Text("as of \(account.readAt.formatted(date: .omitted, time: .shortened))")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .opacity(stale ? 0.6 : 1)
    }
}

struct UsageRing: View {
    let pct: Int
    let stale: Bool

    var body: some View {
        ZStack {
            Circle().stroke(.quaternary, lineWidth: 6)
            Circle()
                .trim(from: 0, to: CGFloat(pct) / 100)
                .stroke(stale ? Color.secondary : tint, style: StrokeStyle(lineWidth: 6, lineCap: .round))
                .rotationEffect(.degrees(-90))
            Text("\(pct)%")
                .font(.caption.weight(.semibold).monospacedDigit())
                .minimumScaleFactor(0.6)
        }
    }

    private var tint: Color { pct >= 95 ? .red : pct >= 80 ? .orange : .accentColor }
}

/// The Lock Screen gauge: the tightest window of the first account, which is
/// the chosen one, else the account nearest its limit (app.md 15.3).
struct UsageGauge: View {
    let accounts: [UsageAccountReading]
    let now: Date

    var body: some View {
        if let a = accounts.first, let w = UsageMath.tightest(a, now: now) {
            let pct = UsageMath.effectivePct(w, now: now)
            Gauge(value: Double(pct), in: 0...100) {
                Text(w.name)
            } currentValueLabel: {
                Text("\(pct)")
            }
            .gaugeStyle(.accessoryCircularCapacity)
        } else {
            Image(systemName: "gauge.with.dots.needle.33percent")
        }
    }
}
