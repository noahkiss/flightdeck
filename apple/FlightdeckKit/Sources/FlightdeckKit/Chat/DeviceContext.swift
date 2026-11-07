import Foundation
import HabAPI
import Observation
import os

// Device context for Capcom's personal tools (capcom-service.md 8.1): the phone's time zone,
// its latest location fix and a calendar snapshot, sent with `device.context.put` to the
// Capcom home hab only. The sources sit behind protocols, so tests use fakes and never touch
// CoreLocation or EventKit.

/// Whether the system lets the app read a source.
public enum ShareAuth: Equatable, Sendable {
    /// Not asked yet.
    case notDetermined
    /// Denied or restricted; on the Mac also a build without the entitlement.
    case denied
    case granted
}

/// A calendar the user can pick for the snapshot.
public struct DeviceCalendarInfo: Hashable, Sendable, Identifiable {
    public var id: String
    public var title: String
    public init(id: String, title: String) {
        self.id = id
        self.title = title
    }
}

/// Where location fixes come from: CoreLocation in the app, a fake in tests.
@MainActor
public protocol DeviceLocationSource: AnyObject {
    var authorization: ShareAuth { get }
    /// Called with each fix, its place name and country already filled in where the reverse
    /// geocoder gave them.
    var onFix: ((DeviceLocation) -> Void)? { get set }
    /// Called when the system's answer to the permission changes.
    var onAuthorization: (() -> Void)? { get set }
    /// Asks the system for the location permission.
    func requestPermission()
    /// Starts significant-change updates and takes a fresh fix.
    func start()
    /// Takes a fresh fix, while the app is open.
    func refresh()
    func stop()
}

/// Where calendar events come from: EventKit in the app, a fake in tests.
@MainActor
public protocol DeviceCalendarSource: AnyObject {
    var authorization: ShareAuth { get }
    /// Asks the system for read access to calendars; true when granted.
    func requestAccess() async -> Bool
    func calendars() -> [DeviceCalendarInfo]
    /// Events between the two dates, from the calendars named (nil: every calendar).
    func events(from: Date, to: Date, calendars: [String]?) -> [DeviceEvent]
}

/// The Capcom preferences of this device (app.md 20.4), kept in the app's defaults.
@MainActor
@Observable
public final class CapcomPrefs {
    public nonisolated static let shareLocationID = "capcom.share_location"
    public nonisolated static let shareCalendarID = "capcom.share_calendar"
    public nonisolated static let calendarDaysID = "capcom.calendar_days"
    nonisolated static let calendarsKey = "capcom.share_calendar.calendars"
    nonisolated static let askedKey = "capcom.location_asked"
    public nonisolated static let daysRange = 1...31
    public nonisolated static let defaultDays = 7

    @ObservationIgnored private let defaults: UserDefaults

    public var shareLocation: Bool {
        didSet { defaults.set(shareLocation, forKey: Self.shareLocationID) }
    }
    public var shareCalendar: Bool {
        didSet { defaults.set(shareCalendar, forKey: Self.shareCalendarID) }
    }
    /// The calendars picked for the snapshot, by id; nil: every calendar.
    public var calendarIDs: [String]? {
        didSet { defaults.set(calendarIDs, forKey: Self.calendarsKey) }
    }
    /// Days of events in the snapshot, 1 to 31.
    public var calendarDays: Int {
        didSet {
            // An assignment inside didSet does not call it again.
            let c = Self.clamp(calendarDays)
            if c != calendarDays { calendarDays = c }
            defaults.set(c, forKey: Self.calendarDaysID)
        }
    }
    /// The first Capcom open asked for the location permission.
    public var locationAsked: Bool {
        didSet { defaults.set(locationAsked, forKey: Self.askedKey) }
    }

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        shareLocation = defaults.object(forKey: Self.shareLocationID) as? Bool ?? true
        shareCalendar = defaults.object(forKey: Self.shareCalendarID) as? Bool ?? true
        calendarIDs = defaults.stringArray(forKey: Self.calendarsKey)
        calendarDays = Self.clamp(defaults.object(forKey: Self.calendarDaysID) as? Int ?? Self.defaultDays)
        locationAsked = defaults.bool(forKey: Self.askedKey)
    }

    nonisolated static func clamp(_ n: Int) -> Int { min(max(n, daysRange.lowerBound), daysRange.upperBound) }
}

/// Sends this device's context to the Capcom home hab (capcom-service.md 8.1): the time zone
/// with every put, the location on each update, the calendar when the app opens and with each
/// location update, and `location_off` or `calendar_off` with the reason when a share is off
/// or the system denies it. It keeps the latest fix only, in memory, and never logs it.
@MainActor
@Observable
public final class DeviceContextReporter {
    /// The capability a hab's welcome lists when it takes `device.context.put`.
    public nonisolated static let capability = "device-context"
    /// Events per snapshot, the hab's ceiling.
    public nonisolated static let maxEvents = 2000

    public let prefs: CapcomPrefs
    @ObservationIgnored public let location: DeviceLocationSource
    @ObservationIgnored public let calendar: DeviceCalendarSource
    /// Sends one put to the home hab; true when the hab took it.
    @ObservationIgnored public var send: ((DeviceContextPutParams) async -> Bool)?
    @ObservationIgnored var now: () -> Date
    @ObservationIgnored var timeZone: () -> TimeZone
    /// The latest fix, until it reaches the hab; the only one the device keeps.
    @ObservationIgnored private var latestFix: DeviceLocation?
    /// A put failed: the next connection to the home sends the whole context.
    @ObservationIgnored public private(set) var owed = false
    @ObservationIgnored private var started = false
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "device-context")

    public init(prefs: CapcomPrefs, location: DeviceLocationSource, calendar: DeviceCalendarSource,
                now: @escaping () -> Date = Date.init, timeZone: @escaping () -> TimeZone = { .current }) {
        self.prefs = prefs
        self.location = location
        self.calendar = calendar
        self.now = now
        self.timeZone = timeZone
        location.onFix = { [weak self] fix in
            Task { @MainActor in await self?.locationUpdated(fix) }
        }
        location.onAuthorization = { [weak self] in
            Task { @MainActor in await self?.sharesChanged() }
        }
    }

    // MARK: - Triggers

    /// The app launched, perhaps in the background for a location update: starts the updates
    /// so CoreLocation delivers the fix that woke it.
    public func launch() {
        startLocation()
    }

    /// The app opened: a fresh fix where sharing is on, and the whole context.
    public func appOpened() async {
        startLocation()
        if prefs.shareLocation, location.authorization == .granted { location.refresh() }
        await put(withLocation: true)
    }

    /// The home hab came online: the whole context, so it holds the latest.
    public func homeOnline() async {
        startLocation()
        await put(withLocation: true)
    }

    /// A location update, foreground or background.
    public func locationUpdated(_ fix: DeviceLocation) async {
        guard prefs.shareLocation, location.authorization == .granted else { return }
        latestFix = fix
        await put(withLocation: true)
    }

    /// A share preference, the picked calendars, the day count or a permission changed.
    public func sharesChanged() async {
        startLocation()
        await put(withLocation: true)
    }

    /// The first Capcom open: asks for the location permission, then calendar read access.
    public func askPermissions() async {
        prefs.locationAsked = true
        if prefs.shareLocation, location.authorization == .notDetermined {
            location.requestPermission()
        }
        if prefs.shareCalendar, calendar.authorization == .notDetermined {
            _ = await calendar.requestAccess()
            await put(withLocation: false)
        }
    }

    /// Whether the first Capcom open should ask for the location permission.
    public var shouldAsk: Bool {
        !prefs.locationAsked && prefs.shareLocation && location.authorization == .notDetermined
    }

    private func startLocation() {
        let on = prefs.shareLocation && location.authorization == .granted
        if on, !started {
            location.start()
            started = true
        } else if !on {
            if started { location.stop() }
            started = false
            latestFix = nil
        }
    }

    // MARK: - The put

    private func put(withLocation: Bool) async {
        let p = params(withLocation: withLocation)
        guard let send else {
            owed = true
            return
        }
        if await send(p) {
            owed = false
            log.info("device context sent")
        } else {
            owed = true
            log.info("device context not sent; it goes at the next connection")
        }
    }

    /// The put as it stands now. The location goes only while sharing is on and permitted.
    public func params(withLocation: Bool = true) -> DeviceContextPutParams {
        var p = DeviceContextPutParams(tz: timeZone().identifier)
        if !prefs.shareLocation {
            p.locationOff = "off"
        } else if location.authorization == .denied {
            p.locationOff = "no_permission"
        } else if withLocation, location.authorization == .granted, let fix = latestFix {
            p.location = fix
        }
        if !prefs.shareCalendar {
            p.calendarOff = "off"
        } else if calendar.authorization == .denied {
            p.calendarOff = "no_permission"
        } else if calendar.authorization == .granted {
            p.calendar = snapshot()
        }
        return p
    }

    /// The next `capcom.calendar_days` days of events, from the start of today.
    public func snapshot() -> DeviceCalendar {
        let t = now()
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = timeZone()
        let from = cal.startOfDay(for: t)
        let to = cal.date(byAdding: .day, value: prefs.calendarDays, to: from) ?? from.addingTimeInterval(Double(prefs.calendarDays) * 86400)
        let events = Array(calendar.events(from: from, to: to, calendars: prefs.calendarIDs).prefix(Self.maxEvents))
        return DeviceCalendar(events: events, from: Self.stamp(from), time: Self.stamp(t), to: Self.stamp(to))
    }

    nonisolated static func stamp(_ d: Date) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f.string(from: d)
    }
}
