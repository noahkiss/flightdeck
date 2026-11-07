import CoreLocation
import EventKit
import Foundation
import HabAPI
#if os(macOS)
import Security
#endif
#if os(iOS)
import UIKit
#endif

// The system halves of the device context and the card writes (capcom-service.md 8.1, 8.5):
// CoreLocation, the reverse geocoder and EventKit. The Mac app runs with the hardened
// runtime, which needs the location and calendars entitlements for these; a build without
// them shares nothing and writes nothing.

/// Entitlements of this process, read at run time.
public enum ProcessEntitlements {
    public static let location = "com.apple.security.personal-information.location"
    public static let calendars = "com.apple.security.personal-information.calendars"

    /// Whether this process has the entitlement. iPhone and iPad need none for these.
    public static func has(_ name: String) -> Bool {
        #if os(macOS)
        guard let task = SecTaskCreateFromSelf(nil),
              let v = SecTaskCopyValueForEntitlement(task, name as CFString, nil) else { return false }
        return (v as? Bool) == true
        #else
        return true
        #endif
    }
}

/// CoreLocation: significant-change updates in the background, a fresh fix while the app is
/// open, the place name and country from the reverse geocoder. Holds no fix of its own.
@MainActor
public final class SystemLocationSource: NSObject, DeviceLocationSource, CLLocationManagerDelegate {
    public var onFix: ((DeviceLocation) -> Void)?
    public var onAuthorization: (() -> Void)?
    private let manager = CLLocationManager()
    private let geocoder = CLGeocoder()
    private let allowed = ProcessEntitlements.has(ProcessEntitlements.location)

    public override init() {
        super.init()
        manager.delegate = self
        manager.desiredAccuracy = kCLLocationAccuracyHundredMeters
    }

    public var authorization: ShareAuth {
        guard allowed else { return .denied }
        switch manager.authorizationStatus {
        case .notDetermined: return .notDetermined
        case .denied, .restricted: return .denied
        default: return .granted
        }
    }

    public func requestPermission() {
        guard allowed else { return }
        manager.requestAlwaysAuthorization()
    }

    public func start() {
        guard allowed, CLLocationManager.significantLocationChangeMonitoringAvailable() else { return }
        manager.startMonitoringSignificantLocationChanges()
        manager.requestLocation()
    }

    public func refresh() {
        guard allowed else { return }
        manager.requestLocation()
    }

    public func stop() {
        manager.stopMonitoringSignificantLocationChanges()
    }

    public nonisolated func locationManager(_ manager: CLLocationManager, didUpdateLocations locations: [CLLocation]) {
        guard let l = locations.last else { return }
        Task { @MainActor in await self.deliver(l) }
    }

    public nonisolated func locationManager(_ manager: CLLocationManager, didFailWithError error: Error) {}

    public nonisolated func locationManagerDidChangeAuthorization(_ manager: CLLocationManager) {
        Task { @MainActor in self.onAuthorization?() }
    }

    private func deliver(_ l: CLLocation) async {
        var fix = DeviceLocation(accuracy: l.horizontalAccuracy, country: nil, lat: l.coordinate.latitude, lon: l.coordinate.longitude,
                                 place: nil, time: DeviceContextReporter.stamp(l.timestamp))
        if let p = try? await geocoder.reverseGeocodeLocation(l).first {
            let words = [p.subLocality, p.locality].compactMap { $0 }.filter { !$0.isEmpty }
            fix.place = words.isEmpty ? p.name : words.joined(separator: ", ")
            fix.country = p.isoCountryCode
        }
        onFix?(fix)
    }
}

/// EventKit calendars: read access for the snapshot.
@MainActor
public final class SystemCalendarSource: DeviceCalendarSource {
    private let store = EKEventStore()
    private let allowed = ProcessEntitlements.has(ProcessEntitlements.calendars)

    public init() {}

    public var authorization: ShareAuth {
        guard allowed else { return .denied }
        switch EKEventStore.authorizationStatus(for: .event) {
        case .notDetermined: return .notDetermined
        case .fullAccess: return .granted
        default: return .denied
        }
    }

    public func requestAccess() async -> Bool {
        guard allowed else { return false }
        return (try? await store.requestFullAccessToEvents()) ?? false
    }

    public func calendars() -> [DeviceCalendarInfo] {
        guard authorization == .granted else { return [] }
        return store.calendars(for: .event).map { DeviceCalendarInfo(id: $0.calendarIdentifier, title: $0.title) }
    }

    public func events(from: Date, to: Date, calendars ids: [String]?) -> [DeviceEvent] {
        guard authorization == .granted else { return [] }
        let cals = ids.map { want in store.calendars(for: .event).filter { want.contains($0.calendarIdentifier) } }
        if let cals, cals.isEmpty { return [] }
        let pred = store.predicateForEvents(withStart: from, end: to, calendars: cals)
        return store.events(matching: pred).sorted { $0.startDate < $1.startDate }.map { e in
            DeviceEvent(allDay: e.isAllDay ? true : nil, calendar: e.calendar?.title, end: DeviceContextReporter.stamp(e.endDate),
                        location: CapcomEventDraft.nonEmpty(e.location), start: DeviceContextReporter.stamp(e.startDate), title: e.title ?? "")
        }
    }
}

/// EventKit writes for the cards: write-only calendar access, or reminders access, asked at
/// the first Add.
@MainActor
public final class SystemEventWriter: CapcomEventWriter {
    private let store = EKEventStore()
    private let allowed = ProcessEntitlements.has(ProcessEntitlements.calendars)

    public init() {}

    public var canWrite: Bool { allowed }

    public func write(_ d: CapcomEventDraft) async throws {
        guard allowed else { throw CapcomWriteError("This build cannot write calendars.") }
        switch d.kind {
        case .calendar: try await writeEvent(d)
        case .reminder: try await writeReminder(d)
        }
    }

    private func writeEvent(_ d: CapcomEventDraft) async throws {
        let s = EKEventStore.authorizationStatus(for: .event)
        if s == .notDetermined {
            guard try await store.requestWriteOnlyAccessToEvents() else { throw CapcomWriteError("Calendar access is off.") }
        } else if s != .writeOnly && s != .fullAccess {
            throw CapcomWriteError("Calendar access is off. Turn it on in Settings.")
        }
        guard let start = d.start else { throw CapcomWriteError("The event has no start.") }
        let e = EKEvent(eventStore: store)
        e.title = d.title
        e.isAllDay = d.allDay
        e.timeZone = d.allDay ? nil : d.timeZone
        e.startDate = start
        e.endDate = d.end ?? (d.allDay ? start : start.addingTimeInterval(3600))
        e.location = d.location
        e.notes = d.notes
        e.url = d.url
        e.calendar = store.defaultCalendarForNewEvents
        do {
            try store.save(e, span: .thisEvent, commit: true)
        } catch {
            throw CapcomWriteError("The calendar did not save the event: \(error.localizedDescription)")
        }
    }

    private func writeReminder(_ d: CapcomEventDraft) async throws {
        let s = EKEventStore.authorizationStatus(for: .reminder)
        if s == .notDetermined {
            guard try await store.requestFullAccessToReminders() else { throw CapcomWriteError("Reminders access is off.") }
        } else if s != .fullAccess {
            throw CapcomWriteError("Reminders access is off. Turn it on in Settings.")
        }
        let r = EKReminder(eventStore: store)
        r.title = d.title
        r.notes = d.notes
        r.url = d.url
        r.location = d.location
        if let due = d.due {
            var cal = Calendar(identifier: .gregorian)
            cal.timeZone = d.timeZone
            let parts: Set<Calendar.Component> = d.dueIsDate ? [.year, .month, .day] : [.year, .month, .day, .hour, .minute]
            var c = cal.dateComponents(parts, from: due)
            c.timeZone = d.dueIsDate ? nil : d.timeZone
            r.dueDateComponents = c
        }
        guard let list = store.defaultCalendarForNewReminders() else { throw CapcomWriteError("No reminders list to add to.") }
        r.calendar = list
        do {
            try store.save(r, commit: true)
        } catch {
            throw CapcomWriteError("Reminders did not save it: \(error.localizedDescription)")
        }
    }
}

/// Runs a short piece of work while iOS may suspend the app: it asks for background time
/// first, so a put from a background location update can reach the hab.
enum BackgroundWork {
    @MainActor
    static func run(_ name: String, _ work: @escaping @MainActor () async -> Bool) async -> Bool {
        #if os(iOS)
        let app = UIApplication.shared
        var id = UIBackgroundTaskIdentifier.invalid
        id = app.beginBackgroundTask(withName: name) {
            app.endBackgroundTask(id)
            id = .invalid
        }
        defer {
            if id != .invalid { app.endBackgroundTask(id) }
        }
        #endif
        return await work()
    }
}
