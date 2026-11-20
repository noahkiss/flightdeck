import HabAPI
import SwiftUI

/// Quiet hours on the Capcom host page (app.md 20.4; capcom-service.md 9.2, 9.9): a switch that
/// writes `off`, and the start and end once on. It writes the home hab only. The row hides when
/// the home hab does not list the setting, shows a value it cannot read as it is and never
/// overwrites it, and shows a locked value with no controls.
struct CapcomQuietHoursSection: View {
    let settings: SettingsCenter
    let habID: String
    /// The range the switch turns on with: the last one this page saw.
    @State private var lastRange = QuietHours.standard

    private var setting: Setting? { settings.hab(habID)?.settings[QuietHours.id] }

    var body: some View {
        if let s = setting {
            Section {
                content(s)
            } header: {
                Text("Quiet hours")
            } footer: {
                Text("Capcom keeps working at night, but holds its pushes until quiet hours end, then sends one that counts them. An urgent event still pushes at once. Times follow the time zone a device last shared.")
            }
        }
    }

    @ViewBuilder
    private func content(_ s: Setting) -> some View {
        // An offline home hab shows the value queued for it (app.md 20.8).
        let shown = settings.hab(habID).flatMap { LevelMatcher.value($0, QuietHours.id, queued: settings.queue.items) } ?? s.value
        let raw: String = { if case .string(let v) = shown { return v } else { return "" } }()
        if let q = QuietHours.parse(raw) {
            Toggle("Hold Capcom's pushes at night", isOn: Binding(get: { q != .off }, set: { on in
                write(on ? lastRange : .off)
            }))
            .disabled(s.isLocked)
            .onAppear { if q != .off { lastRange = q } }
            .onChange(of: q) { _, new in if new != .off { lastRange = new } }
            if case .range(let a, let b) = q {
                time("From", a) { write(q.with(start: $0)) }
                    .disabled(s.isLocked)
                time("Until", b) { write(q.with(end: $0)) }
                    .disabled(s.isLocked)
            }
        } else {
            LabeledContent("Quiet hours", value: raw)
        }
        if s.isLocked {
            Text("Set in the hab's config").font(.caption).foregroundStyle(.secondary)
        }
    }

    private func time(_ title: String, _ minutes: Int, _ set: @escaping (Int) -> Void) -> some View {
        DatePicker(title, selection: Binding(get: { QuietHours.date(minutes) }, set: { set(QuietHours.minutes($0)) }),
                   displayedComponents: .hourAndMinute)
            .environment(\.timeZone, QuietHours.pickerCalendar.timeZone)
            .environment(\.calendar, QuietHours.pickerCalendar)
    }

    private func write(_ q: QuietHours) {
        Task { await settings.setOneHab(QuietHours.id, habID: habID, .string(q.value)) }
    }
}
