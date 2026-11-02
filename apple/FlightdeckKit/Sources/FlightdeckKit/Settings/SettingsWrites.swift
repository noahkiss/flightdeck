import Foundation
import HabAPI

/// A write to an offline hab, saved so it survives a relaunch (app.md 20.5).
public struct QueuedSettingWrite: Codable, Sendable, Equatable {
    public var habID: String
    public var settingID: String
    public var value: JSONValue
    /// The revision the app last saw, and the epoch it was seen in.
    public var revision: UInt64
    public var epoch: String?
    /// The value the app last saw on the hab.
    public var seen: JSONValue?

    public init(habID: String, settingID: String, value: JSONValue, revision: UInt64, epoch: String?, seen: JSONValue?) {
        self.habID = habID
        self.settingID = settingID
        self.value = value
        self.revision = revision
        self.epoch = epoch
        self.seen = seen
    }
}

/// The queue of writes to offline habs, kept in the app's defaults.
public struct SettingsQueue: Sendable, Equatable {
    public private(set) var items: [QueuedSettingWrite] = []

    public init(items: [QueuedSettingWrite] = []) {
        self.items = items
    }

    /// Adds a write. A later write to the same id on the same hab replaces the queued one.
    public mutating func put(_ w: QueuedSettingWrite) {
        items.removeAll { $0.habID == w.habID && $0.settingID == w.settingID }
        items.append(w)
    }

    public mutating func remove(habID: String, settingID: String) {
        items.removeAll { $0.habID == habID && $0.settingID == settingID }
    }

    public mutating func remove(habID: String) {
        items.removeAll { $0.habID == habID }
    }

    public func items(for habID: String) -> [QueuedSettingWrite] {
        items.filter { $0.habID == habID }
    }

    static let key = "FlightdeckSettingsQueue"

    public static func load(_ d: UserDefaults) -> SettingsQueue {
        guard let data = d.data(forKey: key), let items = try? JSONDecoder().decode([QueuedSettingWrite].self, from: data) else {
            return SettingsQueue()
        }
        return SettingsQueue(items: items)
    }

    public func save(_ d: UserDefaults) {
        if let data = try? JSONEncoder().encode(items) { d.set(data, forKey: Self.key) }
    }
}

/// Why a row of a write did not take (app.md 20.2, 20.5).
public enum SettingDrop: Sendable, Equatable {
    /// Another device wrote it since: `conflict`, or a new epoch whose value is not the one the app saw.
    case changedElsewhere
    case locked
    case notOnHab
    case invalid
    /// The hab refused the call a second time; this row was not tried again.
    case notTried
    /// Another failure, such as `unavailable`.
    case failed(String)
}

/// A dropped row: which setting on which hab, and why.
public struct DroppedSetting: Sendable, Equatable {
    public var settingID: String
    public var habID: String
    public var reason: SettingDrop
}

/// The writes to send to one hab when it connects, and the queued writes dropped before sending.
public struct ConnectPlan: Sendable, Equatable {
    public var send: [SettingWrite] = []
    public var dropped: [DroppedSetting] = []
    /// Queued writes the plan settled: sent, dropped, or already in place.
    public var settled: [String] = []
}

/// What one settings.set try did on one hab.
public struct SendOutcome: Sendable {
    public var written: [Setting] = []
    public var dropped: [DroppedSetting] = []
    /// The call did not reach the hab (a transport error): the rows did not take and may be queued.
    public var unreached: [SettingWrite] = []
}

public enum SettingsWrites {
    /// What to send to a hab that just connected (app.md 20.2 "A new hab", 20.5 "The queue"):
    ///
    /// - Each queued write: same epoch, sent with its revision; new epoch, sent with the
    ///   snapshot's revision only when the hab still holds the value the app last saw, else
    ///   dropped. A locked row or one the hab lacks drops. A write whose value is already in
    ///   place is settled with nothing sent.
    /// - Each levelled hab-side setting whose source is still `default`, with no queued write,
    ///   takes the picked level's value with the snapshot's revision. A locked one is skipped.
    public static func connectPlan(hab: HabSettingsView, queued: [QueuedSettingWrite], picked: SettingLevel?,
                                   defs: [SettingDef] = SettingsRegistry.levelledHabSide) -> ConnectPlan {
        var plan = ConnectPlan()
        guard hab.hasSettings else { return plan }
        var queuedIDs = Set<String>()
        for q in queued where q.habID == hab.habID {
            queuedIDs.insert(q.settingID)
            plan.settled.append(q.settingID)
            guard let s = hab.settings[q.settingID] else {
                plan.dropped.append(DroppedSetting(settingID: q.settingID, habID: hab.habID, reason: .notOnHab))
                continue
            }
            if s.isLocked {
                plan.dropped.append(DroppedSetting(settingID: q.settingID, habID: hab.habID, reason: .locked))
                continue
            }
            if q.epoch != nil, q.epoch == hab.epoch {
                if s.revision == q.revision, s.value == q.value { continue }
                plan.send.append(SettingWrite(id: q.settingID, revision: q.revision, value: q.value))
            } else if s.value != q.seen {
                plan.dropped.append(DroppedSetting(settingID: q.settingID, habID: hab.habID, reason: .changedElsewhere))
            } else if s.value != q.value {
                plan.send.append(SettingWrite(id: q.settingID, revision: s.revision, value: q.value))
            }
        }
        guard let picked else { return plan }
        for d in defs where !queuedIDs.contains(d.id) {
            guard let want = d.levels?[picked], let s = hab.settings[d.id], s.source == .default, s.value != want else { continue }
            plan.send.append(SettingWrite(id: d.id, revision: s.revision, value: want))
        }
        return plan
    }

    /// Sends rows to one hab, all or nothing, with the drop-and-resend-once rule (app.md 20.5):
    /// a refusal that names a row by `data.id` (`conflict`, `locked`, `not_found`, `invalid`)
    /// drops that row and sends the rest once more with their revisions unchanged. A second
    /// refusal ends the try; the rows left are not tried again.
    public static func send(_ rows: [SettingWrite], habID: String,
                            via set: (SettingsSetParams) async throws -> SettingsResult) async -> SendOutcome {
        var out = SendOutcome()
        var left = rows
        for attempt in 0..<2 {
            guard !left.isEmpty else { break }
            do {
                out.written = try await set(SettingsSetParams(values: left)).settings
                return out
            } catch let e as RPCError {
                let reason = dropReason(e)
                guard let id = e.data?.id, left.contains(where: { $0.id == id }), let reason else {
                    let why = e.data?.hint ?? e.message
                    out.dropped += left.map { DroppedSetting(settingID: $0.id, habID: habID, reason: .failed(why)) }
                    return out
                }
                out.dropped.append(DroppedSetting(settingID: id, habID: habID, reason: reason))
                left.removeAll { $0.id == id }
                if attempt == 1 {
                    out.dropped += left.map { DroppedSetting(settingID: $0.id, habID: habID, reason: .notTried) }
                }
            } catch {
                out.unreached = left
                return out
            }
        }
        return out
    }

    static func dropReason(_ e: RPCError) -> SettingDrop? {
        switch e.kind {
        case .conflict?: return .changedElsewhere
        case .locked?: return .locked
        case .notFound?: return .notOnHab
        case .invalid?: return .invalid
        default: return nil
        }
    }

    /// The sentence the level notice uses for one dropped row.
    public static func sentence(_ d: DroppedSetting, habLabel: String) -> String {
        let title = SettingsRegistry.def(d.settingID)?.title ?? d.settingID
        switch d.reason {
        case .changedElsewhere: return "\(title) on \(habLabel) was changed from another device."
        case .locked: return "\(title) is set in \(habLabel)'s config."
        case .notOnHab: return "\(title) is not on \(habLabel)."
        case .invalid: return "\(habLabel) refused \(title)."
        case .notTried: return "\(title) on \(habLabel) was not changed."
        case .failed(let why): return "\(title) on \(habLabel) was not changed: \(why)."
        }
    }

    /// The level notice: "Safe: 3 settings changed", or "Safe: 2 of 3 changed." with a
    /// sentence for each row that did not take (app.md 20.2).
    public static func levelNotice(_ level: SettingLevel, changed: Int, total: Int, sentences: [String]) -> String {
        let head: String
        if total == 0 {
            head = "\(level.title): no setting needed a change"
        } else if changed == total {
            head = "\(level.title): " + (total == 1 ? "1 setting changed" : "\(total) settings changed")
        } else {
            head = "\(level.title): \(changed) of \(total) changed"
        }
        return sentences.isEmpty ? head : head + ". " + sentences.joined(separator: " ")
    }
}
