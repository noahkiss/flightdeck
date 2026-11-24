import Foundation
import UserNotifications

/// The three kinds of push a level applies to (app.md 8.9). The raw values are stored.
public enum NotifyKind: String, CaseIterable, Sendable {
    case approvals, questions, finished

    /// The kind a push's `flightdeck.kind` belongs to; nil for kinds no level covers (auth,
    /// error, notices, rounds, Capcom).
    public static func of(pushKind: String) -> NotifyKind? {
        switch pushKind {
        case "permission", "trust": return .approvals
        case "question": return .questions
        case "done": return .finished
        default: return nil
        }
    }

    /// The levels the owner can pick. Approvals have no Off: an approval is never dropped
    /// silently (8.3).
    public var choices: [NotifyLevel] {
        self == .approvals ? [.quiet, .normal, .timeSensitive] : NotifyLevel.allCases
    }

    /// What the hab sends with no setting: approvals and questions time-sensitive, finished off.
    public var defaultLevel: NotifyLevel {
        self == .finished ? .off : .timeSensitive
    }
}

/// How strongly one kind interrupts (app.md 8.9). The raw values are stored.
public enum NotifyLevel: String, CaseIterable, Sendable {
    /// No push of this kind: the app leaves the kind out of `push.register`.
    case off
    /// In Notification Center only: no banner, no sound, the screen stays dark.
    case quiet
    /// A banner with sound.
    case normal
    /// A banner with sound that breaks through a Focus.
    case timeSensitive = "time-sensitive"

    public var title: String {
        switch self {
        case .off: return "Off"
        case .quiet: return "Quiet"
        case .normal: return "Normal"
        case .timeSensitive: return "Time sensitive"
        }
    }

    /// The interruption level a push of this level shows with; nil for off.
    public var interruption: UNNotificationInterruptionLevel? {
        switch self {
        case .off: return nil
        case .quiet: return .passive
        case .normal: return .active
        case .timeSensitive: return .timeSensitive
        }
    }

    /// What `willPresent` shows while the app is in front.
    public var foreground: UNNotificationPresentationOptions {
        switch self {
        case .off: return []
        case .quiet: return [.list]
        case .normal, .timeSensitive: return [.banner, .list, .sound]
        }
    }
}

/// The level per kind, kept in the App Group, so the notification service extension reads what
/// the app wrote (app.md 8.9).
public struct NotifyLevels {
    private let defaults: UserDefaults

    /// The key of one kind's level.
    public static func key(_ kind: NotifyKind) -> String { "flightdeck.notify.level." + kind.rawValue }
    /// The switch "Notify when a turn finishes" that the finished level replaced.
    public static let legacyDoneKey = "flightdeck.push.done-banners"

    /// `defaults` is the App Group's in the app and the extension.
    public init(defaults: UserDefaults = SharedSettings.defaults) {
        self.defaults = defaults
    }

    /// The level of `kind`: the stored one when the owner may pick it, else the default.
    public func level(_ kind: NotifyKind) -> NotifyLevel {
        if let raw = defaults.string(forKey: Self.key(kind)), let l = NotifyLevel(rawValue: raw), kind.choices.contains(l) {
            return l
        }
        return kind.defaultLevel
    }

    public func set(_ level: NotifyLevel, for kind: NotifyKind) {
        guard kind.choices.contains(level) else { return }
        defaults.set(level.rawValue, forKey: Self.key(kind))
    }

    /// Moves the old switch over once: on becomes a quiet finished push, which is what the hab
    /// sent for it. `old` holds the switch (the app's standard defaults).
    public func migrate(from old: UserDefaults) {
        guard old.object(forKey: Self.legacyDoneKey) != nil else { return }
        if defaults.string(forKey: Self.key(.finished)) == nil {
            set(old.bool(forKey: Self.legacyDoneKey) ? .quiet : .off, for: .finished)
        }
        old.removeObject(forKey: Self.legacyDoneKey)
    }

    /// The level a push shows with; nil when no level covers its kind, so it shows as the
    /// hab sent it. `sent` is the hab's level.
    /// - A push the hab sent passive stays quiet: it replaces a banner that already alerted (a
    ///   late summary, 16.5), and must not ring again. Only `done`, which the hab always sends
    ///   passive, takes the owner's level.
    /// - Off can still meet a push the hab sent before it heard the new kinds; it shows quiet.
    public func shown(_ meta: PushMeta?, sent: UNNotificationInterruptionLevel) -> NotifyLevel? {
        guard let k = meta.flatMap({ NotifyKind.of(pushKind: $0.kind) }) else { return nil }
        if sent == .passive, k != .finished { return .quiet }
        let l = level(k)
        return l == .off ? .quiet : l
    }

    /// The notification service extension's step: the push takes the level `shown` gives.
    public func apply(to content: UNMutableNotificationContent, meta: PushMeta?) {
        guard let shown = shown(meta, sent: content.interruptionLevel) else { return }
        if let i = shown.interruption { content.interruptionLevel = i }
        content.sound = shown == .quiet ? nil : .default
    }
}
