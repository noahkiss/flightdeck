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
    /// capability names none.
    public func habOnline(_ habID: String) async {
        guard let conn = habs.connection(habID) else { return }
        guard await conn.has(capability: CapcomChip.capability) else {
            beliefs[habID] = CapcomBelief.none
            return
        }
        reading[habID, default: 0] += 1
        defer {
            let n = (reading[habID] ?? 1) - 1
            reading[habID] = n > 0 ? n : nil
            standingCheck()
        }
        let r = try? await conn.call(HabMethods.catalogGet, CatalogGetParams())
        guard habs.connection(habID) === conn else { return }
        if let c = r?.catalog?.capcom, let home = c.home, !home.isEmpty {
            beliefs[habID] = .home(assignment: c.assignment, device: home)
        } else {
            beliefs[habID] = CapcomBelief.none
        }
    }

    /// A hab left the app: its belief goes.
    public func forget(habID: String) {
        beliefs[habID] = nil
        reading[habID] = nil
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
        await openStanding(on: id)
        opening = false
        // The home moved while the open was on its way: open there too. Each pass needs a
        // new home, so this ends.
        if case .ready(let now) = entry, now != id { await openStanding() }
    }

    private func openStanding(on id: String) async {
        do {
            _ = try await run(habID: id, context: [], new: false, request: false)
            standingFailure = nil
        } catch let e as RPCError where e.kind == .unavailable {
            await refreshHome()
            if case .ready(let next) = entry, next != id, (try? await run(habID: next, context: [], new: false, request: false)) != nil {
                standingFailure = nil
                return
            }
            standingFailure = Self.message(e, label: habs.label(id), doing: nil)
        } catch {
            standingFailure = Self.message(error, label: habs.label(id), doing: nil)
        }
    }

    // MARK: - While a Capcom view is on screen

    /// A Capcom view appeared: open the standing conversation, and keep it open while shown.
    public func viewAppeared() {
        viewers += 1
        lastEntry = entry
        Task { await openStanding() }
    }

    /// A Capcom view went away.
    public func viewDisappeared() {
        viewers = max(0, viewers - 1)
    }

    /// While a Capcom view is on screen: a changed entry (the home came online, moved, or was
    /// found) opens the standing conversation again, and so does a standing conversation that
    /// exited, at most once per `resumeGap` for the same session. The board and each catalog
    /// read call it. Returns whether it started an open.
    @discardableResult
    func standingCheck() -> Bool {
        guard viewers > 0 else { return false }
        let e = entry
        if e != lastEntry {
            lastEntry = e
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
