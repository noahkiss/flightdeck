import FlightdeckShared
import SwiftUI

// The usage footer (app.md 3.7) and the usage sheet (14.2).

extension UsageAccountReading {
    var harnessSymbol: String {
        switch harness {
        case "claude": return "sparkle"
        case "codex": return "chevron.left.forwardslash.chevron.right"
        case "opencode": return "curlybraces"
        default: return "gauge.with.dots.needle.33percent"
        }
    }

    /// "Claude Max 5x", or the harness alone.
    var title: String {
        [harnessName, label].compactMap { $0 }.joined(separator: " ")
    }
}

func usageTint(_ pct: Int) -> Color {
    pct >= 95 ? .red : pct >= 80 ? .orange : .accentColor
}

/// The board's footer: one bar per account, for its tightest window. A tap
/// opens the usage sheet at that account.
struct UsageFooter: View {
    let book: UsageBook

    var body: some View {
        if !book.accounts.isEmpty {
            TimelineView(.periodic(from: .now, by: 30)) { ctx in
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 14) {
                        ForEach(UsageMath.byTightest(Array(book.accounts.values), now: ctx.date)) { a in
                            Button {
                                book.present(account: a.key)
                            } label: {
                                UsageFooterBar(account: a, online: book.isOnline(a), now: ctx.date)
                            }
                            .buttonStyle(.plain)
                        }
                    }
                    .padding(.horizontal, 16)
                    .padding(.vertical, 8)
                }
                .background(.bar)
                .overlay(alignment: .top) { Divider() }
            }
        }
    }
}

struct UsageFooterBar: View {
    let account: UsageAccountReading
    let online: Bool
    let now: Date

    var body: some View {
        let w = UsageMath.tightest(account, now: now)
        let stale = UsageMath.isStale(account, now: now) || !online
        HStack(spacing: 6) {
            Image(systemName: account.harnessSymbol)
                .font(.caption)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 4) {
                    Text(account.harnessName).font(.caption.weight(.medium))
                    if let w {
                        Text("\(w.name) \(UsageMath.effectivePct(w, now: now))%")
                            .font(.caption.monospacedDigit())
                            .foregroundStyle(.secondary)
                    } else {
                        Text("No limits").font(.caption).foregroundStyle(.secondary)
                    }
                }
                if let w {
                    UsageBar(window: w, now: now, stale: stale || UsageMath.isStale(w, now: now))
                        .frame(width: 110, height: 5)
                }
            }
        }
        .opacity(stale ? 0.55 : 1)
        .accessibilityElement(children: .combine)
        .accessibilityHint("Opens usage")
    }
}

/// A usage bar with the pace tick. Empty and dashed once the reset passed
/// with no reading since; grey when stale.
struct UsageBar: View {
    let window: UsageWindowReading
    let now: Date
    var stale = false
    var showPace = false

    var body: some View {
        GeometryReader { g in
            let passed = UsageMath.resetPassed(window, now: now)
            let pct = Double(UsageMath.effectivePct(window, now: now)) / 100
            ZStack(alignment: .leading) {
                if passed {
                    Capsule()
                        .strokeBorder(style: StrokeStyle(lineWidth: 1, dash: [3, 2]))
                        .foregroundStyle(.secondary)
                } else {
                    Capsule().fill(.quaternary)
                    Capsule()
                        .fill(stale ? Color.secondary : usageTint(UsageMath.shownPct(window)))
                        .frame(width: max(0, g.size.width * pct))
                }
                if showPace, let p = UsageMath.pace(window, now: now) {
                    Rectangle()
                        .fill(.primary)
                        .frame(width: 2, height: g.size.height + 6)
                        .offset(x: min(g.size.width - 2, max(0, g.size.width * p - 1)))
                        .accessibilityHidden(true)
                }
            }
        }
    }
}

/// The usage sheet: one card per account, then the warning settings.
public struct UsageSheet: View {
    let book: UsageBook
    @Bindable var alerts: UsageAlerts
    var focus: String?
    let close: () -> Void

    public init(center: UsageCenter, focus: String?, close: @escaping () -> Void) {
        self.book = center.book
        self.alerts = center.alerts
        self.focus = focus
        self.close = close
    }

    public var body: some View {
        NavigationStack {
            TimelineView(.periodic(from: .now, by: 30)) { ctx in
                ScrollViewReader { proxy in
                    List {
                        if book.accounts.isEmpty {
                            ContentUnavailableView("No usage readings", systemImage: "gauge.with.dots.needle.33percent",
                                                   description: Text("Accounts show here once a hab reports their limits."))
                        }
                        ForEach(UsageMath.byTightest(Array(book.accounts.values), now: ctx.date)) { a in
                            Section {
                                UsageCard(account: a, online: book.isOnline(a), alerts: alerts, now: ctx.date)
                            }
                            .id(a.key)
                        }
                        UsageWarningSettings(alerts: alerts)
                    }
                    .onAppear {
                        if let focus { proxy.scrollTo(focus, anchor: .top) }
                    }
                }
            }
            .navigationTitle("Usage")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done", action: close)
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 440, minHeight: 480)
        #endif
    }
}

struct UsageCard: View {
    let account: UsageAccountReading
    let online: Bool
    let alerts: UsageAlerts
    let now: Date

    var body: some View {
        let stale = UsageMath.isStale(account, now: now)
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Image(systemName: account.harnessSymbol)
                    .accessibilityLabel(account.harnessName)
                VStack(alignment: .leading, spacing: 2) {
                    Text(account.title).font(.headline)
                    Text(subtitle(stale: stale))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            if account.windows.isEmpty {
                Text("No limits reported")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            ForEach(account.windows, id: \.name) { w in
                UsageWindowRow(account: account, window: w, alerts: alerts, now: now, stale: stale || !online || UsageMath.isStale(w, now: now))
            }
            if let s = UsageText.status(account, now: now) {
                Text(s)
                    .font(.callout.weight(.medium))
                    .foregroundStyle(s.contains("reached") ? Color.red : s.hasPrefix("Over") ? Color.orange : Color.secondary)
            }
        }
        .padding(.vertical, 4)
        .opacity(stale ? 0.6 : 1)
    }

    /// "workbench · 3 min ago", "workbench offline · as of 14:02".
    private func subtitle(stale: Bool) -> String {
        let read = account.readAt
        var parts = [account.habLabel + (online ? "" : " offline")]
        if stale || !online {
            parts.append("as of \(read.formatted(date: Calendar.current.isDate(read, inSameDayAs: now) ? .omitted : .abbreviated, time: .shortened))")
        } else {
            parts.append(BoardRow.age(read, now: now) == "now" ? "just now" : BoardRow.age(read, now: now) + " ago")
        }
        return parts.joined(separator: " · ")
    }
}

struct UsageWindowRow: View {
    let account: UsageAccountReading
    let window: UsageWindowReading
    let alerts: UsageAlerts
    let now: Date
    let stale: Bool

    var body: some View {
        let passed = UsageMath.resetPassed(window, now: now)
        VStack(alignment: .leading, spacing: 5) {
            HStack(alignment: .firstTextBaseline) {
                Text(window.name).font(.callout.weight(.medium))
                Spacer()
                Text(passed ? "–" : "\(UsageMath.shownPct(window))%")
                    .font(.callout.monospacedDigit())
                if let r = UsageText.resetsIn(window, now: now), !passed {
                    Text(r)
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                        .frame(minWidth: 56, alignment: .trailing)
                }
            }
            UsageBar(window: window, now: now, stale: stale, showPace: true)
                .frame(height: 8)
                .padding(.vertical, 3)
            if passed, let r = window.resetsAt {
                Text("Reset at \(r.formatted(date: .omitted, time: .shortened)); no reading since")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if alerts.isWaiting(account: account.key, window: window.name) {
                Button("Cancel alert for \(window.name)") {
                    alerts.cancelFree(account: account.key, window: window.name)
                }
                .font(.caption)
            } else if alerts.canAlertWhenFree(window) {
                Button("Alert me when \(window.name) is free") {
                    alerts.alertWhenFree(account, window)
                }
                .font(.caption)
            }
        }
        .accessibilityElement(children: .contain)
    }
}

struct UsageWarningSettings: View {
    @Bindable var alerts: UsageAlerts

    var body: some View {
        Section {
            Toggle("Usage warnings", isOn: $alerts.warningsOn)
            if alerts.warningsOn {
                ForEach(Array(alerts.thresholds.enumerated()), id: \.offset) { i, t in
                    Stepper(value: Binding(
                        get: { t },
                        set: { v in
                            var all = alerts.thresholds
                            all[i] = v
                            alerts.thresholds = all
                        }
                    ), in: UsageAlerts.stepRange(i, in: alerts.thresholds), step: 5) {
                        Text("Warn at \(t)%")
                    }
                }
            }
        } footer: {
            Text("Each warning shows once per window.")
        }
    }
}

extension View {
    /// The usage footer under the board (app.md 3.7).
    func usageFooter(_ center: UsageCenter?) -> some View {
        safeAreaInset(edge: .bottom, spacing: 0) {
            if let center { UsageFooter(book: center.book) }
        }
    }

    /// The usage sheet while the book asks for it: from a footer bar, a
    /// widget or a usage notification.
    func usageSheet(_ center: UsageCenter?) -> some View {
        modifier(UsageSheetHost(center: center))
    }
}

struct UsageSheetHost: ViewModifier {
    let center: UsageCenter?

    func body(content: Content) -> some View {
        if let center {
            content
                .task {
                    // Debug builds only: `-FlightdeckOpenUsage <account key or "all">`
                    // opens the usage sheet at launch, so a build can be checked with no clicks.
                    #if DEBUG
                    if let key = UserDefaults.standard.string(forKey: "FlightdeckOpenUsage"), !key.isEmpty {
                        try? await Task.sleep(for: .seconds(1))
                        center.book.present(account: key == "all" ? nil : key)
                    }
                    #endif
                }
                .sheet(item: Binding(get: { center.book.presented }, set: { center.book.presented = $0 })) { p in
                UsageSheet(center: center, focus: p.account) { center.book.presented = nil }
            }
        } else {
            content
        }
    }
}
