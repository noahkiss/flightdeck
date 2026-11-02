import Foundation
import HabAPI
import Observation

/// How the center reads and writes one app-side setting (app.md 20.3).
public struct AppSettingPort {
    /// The value this device holds; nil when the device does not show the row.
    public var get: @MainActor () -> JSONValue?
    /// Applies a value. Throws when it did not take, such as a refused Face ID check.
    public var set: @MainActor (JSONValue) async throws -> Void

    public init(get: @escaping @MainActor () -> JSONValue?, set: @escaping @MainActor (JSONValue) async throws -> Void) {
        self.get = get
        self.set = set
    }
}

/// The settings and levels of app.md 20: the picker's state, level picks,
/// hab-side writes with the queue and the new-hab check.
@MainActor
@Observable
public final class SettingsCenter {
    /// Each hab's settings view, in the order habs were added.
    public private(set) var habs: [HabSettingsView] = []
    public private(set) var queue: SettingsQueue
    /// The last picked level, kept in the app's defaults (app.md 20.2).
    public private(set) var picked: SettingLevel?
    /// The level notice of the last level pick or queue flush.
    public var notice: String?
    /// The app-side settings by id.
    @ObservationIgnored public var ports: [String: AppSettingPort] = [:]

    @ObservationIgnored private let defaults: UserDefaults
    @ObservationIgnored private let setter: (String, SettingsSetParams) async throws -> SettingsResult
    /// The last write or connect step queued for each hab: writes to one hab run one at a
    /// time, so each reads the revisions the one before it left (review S2, S3).
    @ObservationIgnored private var tails: [String: Task<Void, Never>] = [:]

    static let levelKey = "FlightdeckSettingsLevel"

    /// `setter` sends settings.set to the hab with the given id.
    public init(defaults: UserDefaults = .standard, setter: @escaping (String, SettingsSetParams) async throws -> SettingsResult) {
        self.defaults = defaults
        self.setter = setter
        queue = SettingsQueue.load(defaults)
        picked = defaults.string(forKey: Self.levelKey).flatMap(SettingLevel.init(rawValue:))
    }

    // MARK: - State

    /// The app-side values this device shows.
    public var appValues: [String: JSONValue] {
        var out: [String: JSONValue] = [:]
        for d in SettingsRegistry.levelled where d.store == .app && d.shownHere {
            if let v = ports[d.id]?.get() { out[d.id] = v }
        }
        return out
    }

    /// The picker's state (app.md 20.2).
    public var level: LevelState {
        LevelMatcher.match(app: appValues, habs: habs, queued: queue.items, prefer: picked)
    }

    public func row(_ id: String) -> HabRowStatus {
        LevelMatcher.row(id, habs: habs, queued: queue.items)
    }

    public func hab(_ id: String) -> HabSettingsView? { habs.first { $0.habID == id } }

    func label(_ habID: String) -> String { hab(habID)?.label ?? habID }

    // MARK: - Habs

    /// Takes a hab's connection state; the fleet calls it on every update.
    public func update(habID: String, label: String, state: HabState) {
        let v = HabSettingsView(habID: habID, label: label, online: state.status == .online,
                                hasSettings: state.welcome?.capabilities.contains(Self.capability) ?? false,
                                epoch: state.welcome?.epoch, settings: state.settings)
        if let i = habs.firstIndex(where: { $0.habID == habID }) {
            var v = v
            if habs[i].epoch == v.epoch {
                // A state that predates a write's answer must not roll the row back.
                for (id, held) in habs[i].settings where held.revision > (v.settings[id]?.revision ?? 0) {
                    v.settings[id] = held
                }
            }
            if habs[i] != v { habs[i] = v }
        } else {
            habs.append(v)
        }
    }

    /// A hab that is no longer in the app: its view and its queued writes go.
    public func forget(habID: String) {
        habs.removeAll { $0.habID == habID }
        queue.remove(habID: habID)
        queue.save(defaults)
    }

    /// The capability that says a hab serves its settings.
    public static let capability = "settings"

    /// A hab connected and applied its first snapshot: send its queued writes, then give each
    /// row still at its default the picked level's value (app.md 20.2, 20.5).
    ///
    /// A connect that arrives while an earlier step for the hab is in flight waits for it, then
    /// runs with the view current at that point: deferred, never skipped. A queued write leaves
    /// the saved queue only once the hab answered it.
    public func connectionOnline(habID: String) async {
        await serialized(habID) { await self.flush(habID: habID) }
    }

    private func flush(habID: String) async {
        guard let h = hab(habID), h.online, h.hasSettings else { return }
        let queuedIDs = Set(queue.items(for: habID).map(\.settingID))
        let plan = SettingsWrites.connectPlan(hab: h, queued: queue.items(for: habID), picked: picked)
        // Dropped before sending, or already in place: settled now.
        let sending = Set(plan.send.map(\.id))
        for id in plan.settled where !sending.contains(id) { queue.remove(habID: habID, settingID: id) }
        queue.save(defaults)
        var sentences = plan.dropped.map { SettingsWrites.sentence($0, habLabel: h.label) }
        if !plan.send.isEmpty {
            let out = await SettingsWrites.send(plan.send, habID: habID) { try await self.setter(habID, $0) }
            merge(habID: habID, out.written)
            for w in out.written { queue.remove(habID: habID, settingID: w.id) }
            for d in out.dropped where queuedIDs.contains(d.settingID) { queue.remove(habID: habID, settingID: d.settingID) }
            // An unreached queued write stays queued as it was; a new-hab row is checked again
            // at the next connect.
            queue.save(defaults)
            sentences += out.dropped.map { SettingsWrites.sentence($0, habLabel: h.label) }
        }
        if !sentences.isEmpty { notice = sentences.joined(separator: " ") }
    }

    /// Runs `work` after every earlier step for the hab has finished.
    private func serialized<T: Sendable>(_ habID: String, _ work: @escaping @MainActor () async -> T) async -> T {
        let prev = tails[habID]
        let t = Task { @MainActor in
            await prev?.value
            return await work()
        }
        tails[habID] = Task { @MainActor in _ = await t.value }
        return await t.value
    }

    /// Takes a write's answer into the hab's view at once, without waiting for the event.
    private func merge(habID: String, _ written: [Setting]) {
        guard let i = habs.firstIndex(where: { $0.habID == habID }) else { return }
        for s in written where s.revision > (habs[i].settings[s.id]?.revision ?? 0) {
            habs[i].settings[s.id] = s
        }
    }

    /// Queues a write for a hab, with the revision, epoch and value the app last saw.
    private func enqueue(habID: String, id: String, value: JSONValue) {
        let h = hab(habID)
        let s = h?.settings[id]
        queue.put(QueuedSettingWrite(habID: habID, settingID: id, value: value, revision: s?.revision ?? 0, epoch: h?.epoch, seen: s?.value))
        queue.save(defaults)
    }

    /// Writes rows to one hab: at once when online, else through the queue. A hab whose
    /// settings the app has never seen gets nothing queued; the new-hab check covers it.
    private func write(habID: String, rows: [(id: String, value: JSONValue)]) async -> [DroppedSetting] {
        await serialized(habID) { await self.writeNow(habID: habID, rows: rows) }
    }

    private func writeNow(habID: String, rows: [(id: String, value: JSONValue)]) async -> [DroppedSetting] {
        guard let h = hab(habID), h.hasSettings else { return [] }
        let rows = rows.filter { r in
            guard let s = h.settings[r.id] else { return false }
            return !s.isLocked
        }
        guard !rows.isEmpty else { return [] }
        if !h.online {
            for r in rows { enqueue(habID: habID, id: r.id, value: r.value) }
            return []
        }
        let writes = rows.map { SettingWrite(id: $0.id, revision: h.settings[$0.id]?.revision ?? 0, value: $0.value) }
        let out = await SettingsWrites.send(writes, habID: habID) { try await self.setter(habID, $0) }
        merge(habID: habID, out.written)
        for w in out.unreached { enqueue(habID: habID, id: w.id, value: w.value) }
        for w in out.written { queue.remove(habID: habID, settingID: w.id) }
        for x in out.dropped { queue.remove(habID: habID, settingID: x.settingID) }
        queue.save(defaults)
        return out.dropped
    }

    // MARK: - Writes

    /// Sets a setting kept on every hab to one value on each (app.md 20.5).
    public func setEveryHab(_ id: String, _ value: JSONValue) async {
        var sentences: [String] = []
        for h in habs {
            let dropped = await write(habID: h.habID, rows: [(id, value)])
            sentences += dropped.map { SettingsWrites.sentence($0, habLabel: h.label) }
        }
        if !sentences.isEmpty { notice = sentences.joined(separator: " ") }
    }

    /// Sets a setting kept per host on one hab (`hab.permission_wait`).
    public func setOneHab(_ id: String, habID: String, _ value: JSONValue) async {
        let dropped = await write(habID: habID, rows: [(id, value)])
        if !dropped.isEmpty { notice = dropped.map { SettingsWrites.sentence($0, habLabel: label(habID)) }.joined(separator: " ") }
    }

    /// Sets an app-side setting by hand. A refusal (Face ID) keeps the old value.
    public func setApp(_ id: String, _ value: JSONValue) async {
        guard let p = ports[id] else { return }
        do {
            try await p.set(value)
        } catch {
            notice = refusedSentence(id)
        }
    }

    private func refusedSentence(_ id: String) -> String {
        let title = SettingsRegistry.def(id)?.title ?? id
        return "\(title) did not change: authentication was refused."
    }

    /// Picks a level: writes its column at once, with no confirmation (app.md 20.2). App-side
    /// values apply on this device; hab-side values go to every hab, through the queue for an
    /// offline one. Preferences never change.
    public func pick(_ level: SettingLevel) async {
        picked = level
        defaults.set(level.rawValue, forKey: Self.levelKey)
        var total = 0
        var failedIDs = Set<String>()
        var sentences: [String] = []
        for d in SettingsRegistry.levelled where d.store == .app && d.shownHere {
            guard let want = d.levels?[level], let p = ports[d.id], let have = p.get(), have != want else { continue }
            total += 1
            do {
                try await p.set(want)
            } catch {
                failedIDs.insert(d.id)
                sentences.append(refusedSentence(d.id))
            }
        }
        var needed = Set<String>()
        var offline: [String] = []
        for h in habs where h.hasSettings {
            var rows: [(id: String, value: JSONValue)] = []
            for d in SettingsRegistry.levelledHabSide {
                guard let want = d.levels?[level], let s = h.settings[d.id], !s.isLocked else { continue }
                if LevelMatcher.value(h, d.id, queued: queue.items) != want {
                    rows.append((d.id, want))
                    needed.insert(d.id)
                }
            }
            guard !rows.isEmpty else { continue }
            if !h.online { offline.append(h.label) }
            for x in await write(habID: h.habID, rows: rows) {
                failedIDs.insert(x.settingID)
                sentences.append(SettingsWrites.sentence(x, habLabel: h.label))
            }
        }
        total += needed.count
        if !offline.isEmpty {
            sentences.append("\(offline.joined(separator: ", ")) \(offline.count == 1 ? "gets" : "get") the change when \(offline.count == 1 ? "it connects" : "they connect").")
        }
        notice = SettingsWrites.levelNotice(level, changed: total - failedIDs.count, total: total, sentences: sentences)
    }
}
