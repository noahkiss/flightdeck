import HabAPI
import SwiftUI

/// A data source's mark under a Capcom message: the Apple Weather mark, linked to its data
/// sources page (capcom-service.md 8.3).
struct CapcomMarkView: View {
    let mark: CapcomMark

    var body: some View {
        let label = HStack(spacing: 2) {
            if let s = mark.symbol { Image(systemName: s) }
            Text(mark.text)
        }
        .font(.caption2)
        .foregroundStyle(.secondary)
        .padding(.leading, 12)
        if let url = mark.url {
            Link(destination: url) { label }
                .accessibilityLabel(mark.symbol == "apple.logo" ? "Apple \(mark.text)" : mark.text)
        } else {
            label
        }
    }
}

/// A calendar or reminder card (capcom-service.md 8.5): every field, then Add, Edit and
/// Dismiss. Add claims the card on the hab first and writes only when the hab takes it.
struct CapcomEventCardView: View {
    let item: CapcomItem
    let thread: CapcomThread
    @Environment(\.capcom) private var center
    @State private var editing: CapcomEventDraft?

    var body: some View {
        TimelineView(.everyMinute) { ctx in
            if let d = CapcomEventDraft.of(item) {
                content(d, now: ctx.date)
            }
        }
        .sheet(item: Binding(get: { editing.map(EditBox.init) }, set: { editing = $0?.draft })) { box in
            CapcomEventEditor(draft: box.draft) { edited in
                editing = nil
                Task { await add(edited) }
            } cancel: {
                editing = nil
            }
        }
    }

    @ViewBuilder
    private func content(_ d: CapcomEventDraft, now: Date) -> some View {
        let cards = center?.cards
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: d.kind == .calendar ? "calendar" : "checklist")
                    .foregroundStyle(.secondary)
                Text(d.title).font(.callout.weight(.medium))
                Spacer(minLength: 0)
                if item.answer == nil, d.isPast(now) {
                    Text("Past").font(.caption.weight(.medium)).foregroundStyle(.secondary)
                }
            }
            if let w = d.whenText() {
                Text(w).font(.callout)
            }
            if let l = d.location {
                Label(l, systemImage: "mappin.and.ellipse").font(.caption).foregroundStyle(.secondary)
            }
            if let n = d.notes {
                Text(n).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            }
            if let u = d.url {
                Link(u.absoluteString, destination: u).font(.caption).lineLimit(1).truncationMode(.middle)
            }
            if let a = item.answer {
                Text(CapcomCardAdder.answered(a)).font(.caption).foregroundStyle(.secondary)
                if let cards, let f = cards.failures[item.id] {
                    HStack {
                        Text(f).font(.caption).foregroundStyle(.red)
                        if cards.canRetry(item) {
                            Button("Retry") { Task { await cards.retry(item) } }
                                .buttonStyle(.bordered)
                                .disabled(cards.busy.contains(item.id))
                        }
                    }
                }
            } else if let cards, thread.cardClaim != nil {
                buttons(cards, now: now)
                if !cards.canWrite {
                    Text("Add from your phone.").font(.caption).foregroundStyle(.secondary)
                }
                if let r = cards.refusals[item.id] {
                    Text(r).font(.caption).foregroundStyle(.red)
                }
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 12))
    }

    private func buttons(_ cards: CapcomCardAdder, now: Date) -> some View {
        HStack {
            ForEach(CapcomCardAdder.buttons(item, now: now), id: \.self) { b in
                switch b {
                case .add:
                    Button("Add") { Task { await add(nil) } }
                        .buttonStyle(.borderedProminent)
                        .disabled(!cards.canWrite || cards.busy.contains(item.id))
                case .edit:
                    Button("Edit") { editing = CapcomEventDraft.of(item) }
                        .buttonStyle(.bordered)
                        .disabled(!cards.canWrite || cards.busy.contains(item.id))
                case .dismiss:
                    Button("Dismiss") {
                        Task {
                            guard let claim = thread.cardClaim else { return }
                            await cards.dismiss(item, claim: claim, applied: thread.apply)
                        }
                    }
                    .buttonStyle(.bordered)
                    .disabled(cards.busy.contains(item.id))
                }
            }
        }
    }

    private func add(_ edited: CapcomEventDraft?) async {
        guard let cards = center?.cards, let claim = thread.cardClaim else { return }
        await cards.add(item, edited: edited, claim: claim, applied: thread.apply)
    }

    private struct EditBox: Identifiable {
        var draft: CapcomEventDraft
        var id: String { "edit" }
    }
}

/// The event editor of a card's Edit, filled in with the card's fields. Save counts as Add.
/// It is the app's own form: the system editor saves the event itself, before the hab could
/// take the answer.
struct CapcomEventEditor: View {
    @State var draft: CapcomEventDraft
    let save: (CapcomEventDraft) -> Void
    let cancel: () -> Void

    var body: some View {
        NavigationStack {
            Form {
                TextField("Title", text: $draft.title)
                if draft.kind == .calendar {
                    Toggle("All day", isOn: $draft.allDay)
                    DatePicker("Starts", selection: date(\.start), displayedComponents: draft.allDay ? [.date] : [.date, .hourAndMinute])
                    DatePicker("Ends", selection: date(\.end), displayedComponents: draft.allDay ? [.date] : [.date, .hourAndMinute])
                    TextField("Location", text: text(\.location))
                } else {
                    Toggle("Due", isOn: Binding(get: { draft.due != nil }, set: { draft.due = $0 ? (draft.due ?? Date()) : nil }))
                    if draft.due != nil {
                        DatePicker("Due", selection: date(\.due), displayedComponents: draft.dueIsDate ? [.date] : [.date, .hourAndMinute])
                    }
                }
                TextField("Notes", text: text(\.notes), axis: .vertical)
            }
            .navigationTitle(draft.kind == .calendar ? "Event" : "Reminder")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel", action: cancel) }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Add") { save(draft) }
                        .disabled(draft.title.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            }
        }
    }

    private func date(_ k: WritableKeyPath<CapcomEventDraft, Date?>) -> Binding<Date> {
        Binding(get: { draft[keyPath: k] ?? draft.start ?? Date() }, set: { draft[keyPath: k] = $0 })
    }

    private func text(_ k: WritableKeyPath<CapcomEventDraft, String?>) -> Binding<String> {
        Binding(get: { draft[keyPath: k] ?? "" }, set: { draft[keyPath: k] = $0.isEmpty ? nil : $0 })
    }
}

/// Capcom's sharing rows in Capcom settings (app.md 20.4): location, calendar with its
/// calendars, and the days of events.
struct CapcomShareSettings: View {
    let reporter: DeviceContextReporter
    @State private var calendars: [DeviceCalendarInfo] = []

    var body: some View {
        @Bindable var prefs = reporter.prefs
        Section {
            Toggle("Share location", isOn: $prefs.shareLocation)
                .onChange(of: prefs.shareLocation) { _, on in
                    Task {
                        if on, reporter.location.authorization == .notDetermined { reporter.location.requestPermission() }
                        await reporter.sharesChanged()
                    }
                }
            if prefs.shareLocation, reporter.location.authorization == .denied {
                Text("Location is off for Flightdeck in the system settings.").font(.caption).foregroundStyle(.secondary)
            }
        } header: {
            Text("Location")
        } footer: {
            Text("Capcom reads your location only when a question needs it.")
        }
        Section {
            Toggle("Share calendar", isOn: $prefs.shareCalendar)
                .onChange(of: prefs.shareCalendar) { _, on in
                    Task {
                        if on, reporter.calendar.authorization == .notDetermined { _ = await reporter.calendar.requestAccess() }
                        calendars = reporter.calendar.calendars()
                        await reporter.sharesChanged()
                    }
                }
            if prefs.shareCalendar {
                Stepper("Next \(prefs.calendarDays) \(prefs.calendarDays == 1 ? "day" : "days")", value: $prefs.calendarDays, in: CapcomPrefs.daysRange)
                    .onChange(of: prefs.calendarDays) { _, _ in Task { await reporter.sharesChanged() } }
                if reporter.calendar.authorization == .denied {
                    Text("Calendars are off for Flightdeck in the system settings.").font(.caption).foregroundStyle(.secondary)
                }
                ForEach(calendars) { c in
                    Toggle(c.title, isOn: Binding(get: { prefs.calendarIDs?.contains(c.id) ?? true }, set: { on in
                        var ids = prefs.calendarIDs ?? calendars.map(\.id)
                        ids.removeAll { $0 == c.id }
                        if on { ids.append(c.id) }
                        prefs.calendarIDs = Set(ids) == Set(calendars.map(\.id)) ? nil : ids
                        Task { await reporter.sharesChanged() }
                    }))
                }
            }
        } header: {
            Text("Calendar")
        } footer: {
            Text("Capcom reads the events of the calendars you pick.")
        }
        .task { calendars = reporter.calendar.calendars() }
    }
}

/// The first Capcom open: one line on why, then the system's location prompt.
struct CapcomPermissionAsk: View {
    let reporter: DeviceContextReporter

    var body: some View {
        if reporter.shouldAsk {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Image(systemName: "location")
                Text("Capcom uses your location and calendar to answer questions about your day.")
                    .font(.callout)
                Spacer(minLength: 0)
                Button("Continue") { Task { await reporter.askPermissions() } }
                    .buttonStyle(.borderedProminent)
                Button("Not now") { reporter.prefs.locationAsked = true }
                    .buttonStyle(.bordered)
            }
            .padding(10)
            .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 10))
            .padding(.horizontal, 12)
            .padding(.top, 8)
        }
    }
}
