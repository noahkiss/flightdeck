import Foundation
import HabAPI

/// A level of the picker (app.md 20.2). Custom is derived, never stored, so
/// it is not a case: a match of no level is `nil`.
public enum SettingLevel: String, Codable, CaseIterable, Sendable {
    case safe
    case balanced
    case bypass

    public var title: String {
        switch self {
        case .safe: return "Safe"
        case .balanced: return "Balanced"
        case .bypass: return "Bypass permissions"
        }
    }
}

/// A group of the Settings page (app.md 20.7), in page order.
public enum SettingArea: String, CaseIterable, Sendable {
    case agents, approvals, notifications, voice, usage, widgets, connection, security, habs

    public var title: String {
        switch self {
        case .agents: return "Agents"
        case .approvals: return "Approvals"
        case .notifications: return "Notifications"
        case .voice: return "Voice"
        case .usage: return "Usage"
        case .widgets: return "Widgets"
        case .connection: return "Connection"
        case .security: return "Security"
        case .habs: return "Habs"
        }
    }
}

/// Where a setting is kept (app.md 20.3, 20.4).
public enum SettingStore: Sendable, Equatable {
    /// On this device.
    case app
    /// On every hab, the same value on each (app.md 20.5).
    case everyHab
    /// On one hab, written per host.
    case oneHab
}

/// The values a setting takes.
public enum SettingKind: Sendable, Equatable {
    case bool
    /// A whole number in a range, with a unit for the row.
    case integer(ClosedRange<Int64>, unit: String)
    /// One word of a closed set, with a title for each.
    case word([(word: String, title: String)])
    /// Shown and edited on its own page; the registry only names it.
    case page

    public static func == (a: SettingKind, b: SettingKind) -> Bool {
        switch (a, b) {
        case (.bool, .bool), (.page, .page): return true
        case let (.integer(r1, u1), .integer(r2, u2)): return r1 == r2 && u1 == u2
        case let (.word(w1), .word(w2)): return w1.map(\.word) == w2.map(\.word)
        default: return false
        }
    }
}

/// One setting of the registry (app.md 20.3 and 20.4).
public struct SettingDef: Identifiable, Sendable {
    public var id: String
    public var title: String
    public var area: SettingArea
    public var kind: SettingKind
    public var store: SettingStore
    /// The value of each level; nil for a preference, which no level changes.
    public var levels: [SettingLevel: JSONValue]?
    /// Shown on iPhone and iPad only.
    public var iOSOnly = false

    public var isLevelled: Bool { levels != nil }
    public var isHabSide: Bool { store != .app }

    /// Whether this device shows the row.
    public var shownHere: Bool {
        #if os(iOS)
        return true
        #else
        return !iOSOnly
        #endif
    }
}

/// The setting ids and their rows (app.md 20.3, 20.4).
public enum SettingsRegistry {
    public static let startElsewhere = "confirm.start_elsewhere"
    public static let approvalMaxAge = "approval.max_age"
    public static let logbookApproval = "logbook.approval"
    public static let voiceYesAllows = "voice.yes_allows"
    public static let sshRequireFaceID = "ssh.require_face_id"
    public static let permissionWait = "hab.permission_wait"

    /// The levelled settings, in registry order (app.md 20.3).
    public static let levelled: [SettingDef] = [
        SettingDef(id: startElsewhere, title: "Confirm before an agent starts work on another host", area: .agents, kind: .bool, store: .everyHab,
                   levels: [.safe: .bool(true), .balanced: .bool(true), .bypass: .bool(false)]),
        SettingDef(id: approvalMaxAge, title: "Remote answers expire after", area: .approvals, kind: .integer(0...1440, unit: "min"), store: .everyHab,
                   levels: [.safe: .int(60), .balanced: .int(60), .bypass: .int(0)]),
        SettingDef(id: logbookApproval, title: "Require approval for memories", area: .agents,
                   kind: .word([(word: "off", title: "Off"), (word: "agents", title: "Agents")]), store: .everyHab,
                   levels: [.safe: .string("agents"), .balanced: .string("off"), .bypass: .string("off")]),
        SettingDef(id: voiceYesAllows, title: "\"Yes\" counts as Allow", area: .voice, kind: .bool, store: .app,
                   levels: [.safe: .bool(false), .balanced: .bool(true), .bypass: .bool(true)]),
        SettingDef(id: sshRequireFaceID, title: "Require Face ID for SSH keys", area: .security, kind: .bool, store: .app,
                   levels: [.safe: .bool(true), .balanced: .bool(false), .bypass: .bool(false)], iOSOnly: true),
    ]

    /// The preferences: settings no level changes (app.md 20.4). Those of
    /// kind `.page` keep their rows on the page their area links to.
    public static let preferences: [SettingDef] = [
        SettingDef(id: "notify.done", title: "Notify when a turn finishes", area: .notifications, kind: .page, store: .app),
        SettingDef(id: "notify.show_commands", title: "Show commands in notifications", area: .notifications, kind: .page, store: .app),
        SettingDef(id: "notify.command_done", title: "Notify when a long command finishes", area: .notifications, kind: .page, store: .app),
        SettingDef(id: "notify.tap", title: "Open a notification in", area: .notifications, kind: .page, store: .app),
        SettingDef(id: "usage.warnings", title: "Usage warnings, with thresholds", area: .usage, kind: .page, store: .app),
        SettingDef(id: "widget.hide_titles", title: "Hide titles on the Lock Screen", area: .widgets, kind: .page, store: .app),
        SettingDef(id: "mac.keep_connected", title: "Stay connected in the background", area: .connection, kind: .page, store: .app),
        SettingDef(id: permissionWait, title: "Answer permissions from the app (blocking hook)", area: .approvals,
                   kind: .integer(0...120, unit: "s"), store: .oneHab),
        SettingDef(id: "voice.*", title: "Engines, summary request, spoken commands, confirm before send, barge-in, noisy room, pause", area: .voice, kind: .page, store: .app),
        SettingDef(id: "voice.language", title: "Dictation language", area: .voice, kind: .page, store: .app),
    ]

    public static let all: [SettingDef] = levelled + preferences

    /// The levelled settings kept on every hab.
    public static var levelledHabSide: [SettingDef] { levelled.filter { $0.store == .everyHab } }

    public static func def(_ id: String) -> SettingDef? { all.first { $0.id == id } }

    /// The value `hab.permission_wait`'s switch sets when turned on (app.md 20.4).
    public static let permissionWaitOn: Int64 = 30
}
