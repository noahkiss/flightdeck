import Foundation
import HabAPI

/// What the app holds about one hab's settings: from its last snapshot and
/// events (app.md 20.5).
public struct HabSettingsView: Sendable, Equatable {
    public var habID: String
    public var label: String
    public var online: Bool
    /// The hab's welcome named `settings`. A hab without it is left out of
    /// every match (app.md 20.2).
    public var hasSettings: Bool
    public var epoch: String?
    public var settings: [String: Setting]

    public init(habID: String, label: String, online: Bool, hasSettings: Bool, epoch: String? = nil, settings: [String: Setting] = [:]) {
        self.habID = habID
        self.label = label
        self.online = online
        self.hasSettings = hasSettings
        self.epoch = epoch
        self.settings = settings
    }
}

extension Setting {
    /// Set in the hab's config, a flag or the environment: read-only. An
    /// unknown source word reads as config (app.md 20.5).
    public var isLocked: Bool { source != .default && source != .client }
}

/// Why a row holds a value the matched level does not (app.md 20.2).
public enum ExceptionReason: String, Sendable, Equatable {
    /// Set in the hab's config.
    case locked
    /// The hab is offline; the row shows the value it last had (or the one queued for it).
    case offline
}

/// A row whose value differs from the matched level, but which does not
/// break the match.
public struct LevelException: Sendable, Equatable {
    public var settingID: String
    public var habID: String
    public var habLabel: String
    public var reason: ExceptionReason
}

/// The picker's state: a level with its exceptions, or Custom (`level` nil).
public struct LevelState: Sendable, Equatable {
    public var level: SettingLevel?
    public var exceptions: [LevelException]

    public static let custom = LevelState(level: nil, exceptions: [])

    /// "Safe", "Safe, 1 exception", or "Custom".
    public var title: String {
        guard let level else { return "Custom" }
        switch exceptions.count {
        case 0: return level.title
        case 1: return "\(level.title), 1 exception"
        default: return "\(level.title), \(exceptions.count) exceptions"
        }
    }
}

/// One hab-side row as the page shows it.
public struct HabRowStatus: Sendable, Equatable {
    /// The value shown: the first online, unlocked hab's, else the first hab's.
    public var value: JSONValue?
    /// Labels of habs whose value differs from `value`.
    public var differsOn: [String]
    /// Labels of habs whose config sets the row.
    public var lockedOn: [String]
    /// No hab with `settings` holds the row: show it read-only.
    public var unavailable: Bool
}

/// The level rule (app.md 20.2): which column every levelled setting
/// matches, and which locked or offline rows are exceptions.
public enum LevelMatcher {
    /// The value a hab holds for a row: the queued write when one waits,
    /// else its last known value.
    static func value(_ hab: HabSettingsView, _ id: String, queued: [QueuedSettingWrite]) -> JSONValue? {
        if let q = queued.last(where: { $0.habID == hab.habID && $0.settingID == id }) { return q.value }
        return hab.settings[id]?.value
    }

    /// The picker's state. `app` holds the app-side values this device
    /// shows; a setting it does not show is absent and left out. `prefer`,
    /// the last picked level, is tried first, so a set of rows that two
    /// columns share shows the level the user picked.
    public static func match(defs: [SettingDef] = SettingsRegistry.levelled, app: [String: JSONValue], habs: [HabSettingsView],
                             queued: [QueuedSettingWrite] = [], prefer: SettingLevel? = nil) -> LevelState {
        var order = SettingLevel.allCases
        if let p = prefer { order.removeAll { $0 == p }; order.insert(p, at: 0) }
        for level in order {
            if let ex = exceptions(level, defs: defs, app: app, habs: habs, queued: queued) {
                return LevelState(level: level, exceptions: ex)
            }
        }
        return .custom
    }

    /// The exceptions when every row matches `level`, else nil.
    static func exceptions(_ level: SettingLevel, defs: [SettingDef], app: [String: JSONValue], habs: [HabSettingsView],
                           queued: [QueuedSettingWrite]) -> [LevelException]? {
        var out: [LevelException] = []
        for d in defs {
            guard let want = d.levels?[level] else { continue }
            if d.store == .app {
                guard let v = app[d.id] else { continue }
                if v != want { return nil }
                continue
            }
            for h in habs where h.hasSettings {
                guard let s = h.settings[d.id] else { continue } // not on this hab
                let v = s.isLocked ? s.value : (value(h, d.id, queued: queued) ?? s.value)
                if s.isLocked || !h.online {
                    if v != want {
                        out.append(LevelException(settingID: d.id, habID: h.habID, habLabel: h.label, reason: s.isLocked ? .locked : .offline))
                    }
                } else if v != want {
                    return nil
                }
            }
        }
        return out
    }

    /// How a row kept on habs shows: its value, the habs that differ, the
    /// habs that lock it.
    public static func row(_ id: String, habs: [HabSettingsView], queued: [QueuedSettingWrite] = []) -> HabRowStatus {
        let holding = habs.filter { $0.hasSettings && $0.settings[id] != nil }
        guard !holding.isEmpty else { return HabRowStatus(value: nil, differsOn: [], lockedOn: [], unavailable: true) }
        let lead = holding.first { $0.online && !($0.settings[id]?.isLocked ?? false) } ?? holding.first { $0.online } ?? holding[0]
        let shown = value(lead, id, queued: queued)
        var differs: [String] = []
        var locked: [String] = []
        for h in holding {
            if h.settings[id]?.isLocked == true { locked.append(h.label) }
            if value(h, id, queued: queued) != shown { differs.append(h.label) }
        }
        return HabRowStatus(value: shown, differsOn: differs, lockedOn: locked, unavailable: false)
    }
}
