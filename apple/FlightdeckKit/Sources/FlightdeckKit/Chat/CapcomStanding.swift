import Foundation
import HabAPI

/// What one hab's catalog says of the Capcom home (capcom.md 7.6).
public enum CapcomBelief: Equatable, Sendable {
    /// No home: the hab has no Capcom, never saw an assignment, or its catalog could not be read.
    case none
    /// The home's device id at an assignment counter.
    case home(assignment: UInt64, device: String)
}

/// Where the Capcom view stands (app.md 4.10).
public enum CapcomEntry: Equatable, Sendable {
    /// A hab is still connecting, or a catalog read is on its way, and no home is known yet.
    case loading
    /// Every hab that answered names no home.
    case unassigned
    /// No hab is online, so no home is known.
    case noHabOnline
    /// The home's device id names no hab in this app.
    case notInApp
    /// The home is a hab in this app that is not online.
    case offline(habID: String)
    /// The home is online.
    case ready(habID: String)
}

/// One hab in the Capcom host picker (app.md 4.10).
public struct CapcomHostChoice: Identifiable, Equatable, Sendable {
    /// The hab's id in the app.
    public var id: String
    public var label: String
    /// This hab is the Capcom home now.
    public var isHome: Bool
    /// Why it cannot be chosen; nil when it can.
    public var unavailable: String?

    public var choosable: Bool { unavailable == nil }
}

/// The Capcom view's side of `CapcomCenter` (app.md 4.10): it finds the home hab from each
/// hab's catalog and opens the standing conversation there with `capcom.open` and no context.
extension CapcomCenter {
    /// The home's device id: the belief with the highest assignment counter; a tie goes to the
    /// lower device id, as on the habs (capcom.md 7.6).
    public var homeDevice: String? {
        var best: (UInt64, String)?
        for case .home(let a, let d) in beliefs.values {
            if let b = best, a < b.0 || (a == b.0 && d >= b.1) { continue }
            best = (a, d)
        }
        return best?.1
    }

    /// Where the Capcom view stands now.
    public var entry: CapcomEntry {
        let all = habs.statuses()
        if let home = homeDevice {
            guard let id = habs.device(home) else {
                return all.contains { $0.status == .connecting } ? .loading : .notInApp
            }
            return habs.online(id) ? .ready(habID: id) : .offline(habID: id)
        }
        let waiting = all.contains { h in
            reading[h.id] != nil || h.status == .connecting || (h.status == .online && beliefs[h.id] == nil)
        }
        if waiting { return .loading }
        // Only an online hab's answer says no host is assigned; a kept one from an offline
        // hab may be stale, and Check again could read nothing.
        if !all.contains(where: { $0.status == .online }) { return .noHabOnline }
        return .unassigned
    }

    /// The row's line under "Capcom" on the board.
    public func entryText(_ e: CapcomEntry) -> String {
        switch e {
        case .loading: return "Looking for the Capcom host"
        case .unassigned: return "No Capcom host assigned"
        case .noHabOnline: return "No hab online"
        case .notInApp: return "The Capcom host is not in this app"
        case .offline(let id): return "\(habs.label(id)) · offline"
        case .ready(let id): return habs.label(id)
        }
    }

    /// A hab came online: read its catalog for the home. A hab without the `capcom`
    /// capability names none. A `quiet` read (after `catalog.updated`) does not count as
    /// on its way, so the entry does not flash back to loading.
    public func habOnline(_ habID: String, quiet: Bool = false) async {
        guard let conn = habs.connection(habID) else { return }
        guard await conn.has(capability: CapcomChip.capability) else {
            beliefs[habID] = CapcomBelief.none
            standingSessions[habID] = nil
            return
        }
        if !quiet { reading[habID, default: 0] += 1 }
        defer {
            if !quiet {
                let n = (reading[habID] ?? 1) - 1
                reading[habID] = n > 0 ? n : nil
            }
            standingCheck()
        }
        let r = try? await conn.call(HabMethods.catalogGet, CatalogGetParams())
        guard habs.connection(habID) === conn else { return }
        // A failed quiet read keeps what the last read found.
        if quiet, r == nil { return }
        if let r { catalogRead[habID] = max(catalogRead[habID] ?? 0, r.generation) }
        let cat = r?.catalog
        var next = CapcomBelief.none
        if let c = cat?.capcom, let home = c.home, !home.isEmpty {
            next = .home(assignment: c.assignment, device: home)
        }
        // A quiet read answered before an assign the hab already answered never lowers the
        // counter this hab's belief holds.
        if quiet, case .home(let held, _) = beliefs[habID] {
            let got: UInt64
            if case .home(let a, _) = next { got = a } else { got = 0 }
            if got < held { return }
        }
        if cat?.alwaysOn == true { alwaysOn.insert(habID) } else if cat != nil { alwaysOn.remove(habID) }
        standingSessions[habID] = cat?.capcom?.sessions.first { $0.standing == true }?.session
        let before = homeDevice
        beliefs[habID] = next
        // The home changed another way: an earlier picker refusal no longer applies.
        if homeDevice != before { assignFailure = nil }
    }

    /// The hab's own catalog changed (`catalog.updated`): read it again, so a re-assignment
    /// from the command line or another device shows at once. Changes that arrive during a
    /// read give one more read after it.
    public func catalogChanged(_ habID: String, generation: UInt64? = nil) async {
        // A generation already read changes nothing.
        if let g = generation, let read = catalogRead[habID], g <= read { return }
        guard rereading.insert(habID).inserted else {
            rereadAgain.insert(habID)
            return
        }
        defer { rereading.remove(habID) }
        repeat {
            rereadAgain.remove(habID)
            await habOnline(habID, quiet: true)
        } while rereadAgain.contains(habID)
    }

    /// A hab left the app: its belief goes.
    public func forget(habID: String) {
        beliefs[habID] = nil
        reading[habID] = nil
        standingSessions[habID] = nil
        alwaysOn.remove(habID)
        catalogRead[habID] = nil
        fresh.remove(habID)
    }

    /// Reads every online hab's catalog again: "Check again", and an open the old home
    /// refused after a re-assignment.
    public func refreshHome() async {
        let ids = habs.statuses().filter { $0.status == .online }.map(\.id)
        await withTaskGroup(of: Void.self) { g in
            for id in ids { g.addTask { await self.habOnline(id) } }
        }
    }

    /// Opens the standing conversation on the home with `capcom.open` and no context, which
    /// starts or resumes it as needed. The view calls it on appearing and whenever `entry`
    /// changes. A refusal with `unavailable` (the home moved) reads the catalogs again and
    /// tries the new home once.
    public func openStanding() async {
        guard case .ready(let id) = entry, !opening else { return }
        opening = true
        let dropped = await openStanding(on: id)
        opening = false
        if case .ready(let now) = entry {
            // The home moved while the open was on its way: open there too. Each pass needs a
            // new home, so this ends.
            if now != id { await openStanding(); return }
            // The link dropped and is back already: one more try.
            if dropped, retryAfterDrop {
                retryAfterDrop = false
                await openStanding()
            }
        }
    }

    /// One open on `id`. Returns true when it failed because the link dropped; it then marks
    /// one retry for when the home is online again.
    private func openStanding(on id: String) async -> Bool {
        do {
            _ = try await run(habID: id, context: [], new: false, request: false)
            standingFailure = nil
            retryAfterDrop = false
        } catch let e as RPCError where e.kind == .unavailable {
            await refreshHome()
            if case .ready(let next) = entry, next != id, (try? await run(habID: next, context: [], new: false, request: false)) != nil {
                standingFailure = nil
                return false
            }
            standingFailure = Self.message(e, label: habs.label(id), doing: nil)
        } catch let e as ConnectionClosed {
            standingFailure = Self.message(e, label: habs.label(id), doing: nil)
            retryAfterDrop = true
            return true
        } catch {
            standingFailure = Self.message(error, label: habs.label(id), doing: nil)
        }
        return false
    }

    // MARK: - The standing conversation's row state

    /// The standing conversation on the home, for the entry row's agent state: the session
    /// the home's catalog marks standing, the newer fact, else the last open's session when it
    /// ran on the home. A hab just made home has none until its next open.
    public var standingTarget: SessionTarget? {
        guard case .ready(let id) = entry else { return nil }
        if let s = standingSessions[id] { return SessionTarget(habID: id, session: s, pane: nil) }
        guard !fresh.contains(id), let m = standing, m.target.habID == id, m.target.session != nil else { return nil }
        return m.target
    }

    /// The board row of the standing conversation, if the board lists it.
    public func standingRow(in rows: [BoardRow]) -> BoardRow? {
        guard let t = standingTarget else { return nil }
        return rows.first { $0.target.habID == t.habID && $0.target.session == t.session }
    }

    // MARK: - Host picker (capcom.md 7.3)

    /// The highest assignment counter any hab's catalog named.
    public var highestAssignment: UInt64 {
        beliefs.values.reduce(0) { n, b in
            if case .home(let a, _) = b { return max(n, a) }
            return n
        }
    }

    /// Every hab in the app, in the app's order, for the host picker. Only an online hab with
    /// the `capcom` capability can be chosen.
    public var hostChoices: [CapcomHostChoice] {
        let home = homeDevice.flatMap { habs.device($0) }
        return habs.statuses().map { h in
            let why: String?
            if h.status == .connecting { why = "Connecting" }
            else if h.status != .online { why = "Offline" }
            else if !habs.hasCapcom(h.id) { why = "No Capcom" }
            else { why = nil }
            return CapcomHostChoice(id: h.id, label: habs.label(h.id), isHome: h.id == home, unavailable: why)
        }
    }

    /// The hab the picker suggests when no host is assigned: one marked always on, else the
    /// first hab in the app, among those that can be chosen.
    public var suggestedHost: String? {
        let ok = hostChoices.filter(\.choosable)
        return (ok.first { alwaysOn.contains($0.id) } ?? ok.first)?.id
    }

    /// Makes `habID` the Capcom home: `capcom.assign` with host `self` on that hab, with a
    /// counter above every one the app has seen, so no peer link is needed. The answer
    /// becomes that hab's belief at once. Returns whether it succeeded.
    @discardableResult
    public func assign(habID: String) async -> Bool {
        guard assigning == nil, let choice = hostChoices.first(where: { $0.id == habID }), choice.choosable,
              let conn = habs.connection(habID) else { return false }
        assigning = habID
        defer { assigning = nil }
        do {
            let r = try await conn.call(HabMethods.capcomAssign, CapcomAssignParams(assignment: highestAssignment + 1, host: "self"))
            beliefs[habID] = .home(assignment: r.assignment, device: r.home)
            if !choice.isHome {
                // A new home starts a fresh standing conversation (capcom.md 7.3): its old one
                // is no longer standing.
                standingSessions[habID] = nil
                fresh.insert(habID)
            }
            assignFailure = nil
            standingCheck()
            return true
        } catch {
            assignFailure = Self.message(error, label: habs.label(habID), doing: nil)
            return false
        }
    }

    /// A host picker appeared: an earlier refusal is not shown again.
    public func pickerAppeared() {
        assignFailure = nil
    }

    // MARK: - While a Capcom view is on screen

    /// A Capcom view appeared: open the standing conversation, and keep it open while shown.
    /// `view` names that view, one per window; a second call with the same token (a redraw
    /// that swaps the view's content) opens nothing.
    public func viewAppeared(_ view: UUID) {
        guard viewers.insert(view).inserted else { return }
        lastEntry = entry
        Task { await openStanding() }
    }

    /// A Capcom view went away.
    public func viewDisappeared(_ view: UUID) {
        viewers.remove(view)
    }

    /// While a Capcom view is on screen: a changed entry (the home came online, moved, or was
    /// found) opens the standing conversation again, and so does a standing conversation that
    /// exited, at most once per `resumeGap` for the same session, and so does an open that
    /// failed when the link dropped, once the home is online again. The board and each catalog
    /// read call it. Returns whether it started an open.
    @discardableResult
    func standingCheck() -> Bool {
        guard !viewers.isEmpty else { return false }
        let e = entry
        if e != lastEntry {
            lastEntry = e
            retryAfterDrop = false
            Task { await openStanding() }
            return true
        }
        if retryAfterDrop, case .ready = e, !opening {
            retryAfterDrop = false
            Task { await openStanding() }
            return true
        }
        guard let m = standing, m.context.sessionExited, case .ready(let id) = e, m.target.habID == id,
              !opening, let session = m.target.session else { return false }
        if let last = lastResume, last.session == session, Date().timeIntervalSince(last.at) < resumeGap { return false }
        lastResume = (session, Date())
        Task { await openStanding() }
        return true
    }
}
