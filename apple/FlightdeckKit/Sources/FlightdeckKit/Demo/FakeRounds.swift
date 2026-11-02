import Foundation
import HabAPI

/// The fake hab's rounds (rounds.md 13), with the real hab's replies and refusal text
/// (`cmd/gff-hab/rounds.go`, `internal/rounds`). Next slots are rough: a preset's next
/// multiple from now, a cron round one hour out, a once round its instant.
struct FakeRounds {
    private(set) var rounds: [String: Round] = [:]
    /// Each round's runs, oldest first.
    private(set) var runs: [String: [RoundRun]] = [:]
    private var nextID = 1
    var now: () -> Date = Date.init

    func list() -> [Round] { rounds.values.sorted { $0.name < $1.name } }

    func entry(_ ref: RoundRef) throws -> Round {
        let id = ref.id ?? "", name = ref.name ?? ""
        if !id.isEmpty && !name.isEmpty { throw FakeFailure(kind: .invalid, hint: "give a round's id or its name, not both") }
        if id.isEmpty && name.isEmpty { throw FakeFailure(kind: .invalid, hint: "name a round by id or name") }
        guard let r = rounds[id] ?? rounds.values.first(where: { !name.isEmpty && $0.name == name }) else {
            throw FakeFailure(kind: .notFound, hint: "no round \(id)\(name) on this hab")
        }
        return r
    }

    private func check(_ s: RoundSpec, projects: [String: Project], sessions: [String: Session]) throws {
        if s.name.isEmpty { throw FakeFailure(kind: .invalid, hint: "name: required") }
        let ok = s.name.count <= 64 && s.name.unicodeScalars.allSatisfy { CharacterSet.alphanumerics.contains($0) && $0.isASCII || $0 == "-" || $0 == "_" }
        if !ok { throw FakeFailure(kind: .invalid, hint: "name: use up to 64 letters, digits, - and _") }
        if s.mode == .fresh {
            guard let p = s.project, !p.isEmpty else { throw FakeFailure(kind: .invalid, hint: "project: required for a fresh round") }
            if projects[p] == nil { throw FakeFailure(kind: .notFound, hint: "project \(p) does not exist") }
            if (s.command ?? []).isEmpty && (s.prompt ?? "").isEmpty {
                throw FakeFailure(kind: .invalid, hint: "prompt: required, unless command")
            }
        } else if let t = s.target, sessions[t] == nil {
            throw FakeFailure(kind: .notFound, hint: "target session \(t) does not exist on this hab")
        }
        if s.trigger.kind == .cron, let e = s.trigger.every, slotSpan(e) == nil {
            throw FakeFailure(kind: .invalid, hint: "trigger: every \(e): cron cannot express it; nearest presets: 5m, 10m")
        }
    }

    private func slotSpan(_ every: String) -> TimeInterval? {
        guard let n = Int(every.dropLast()), n > 0 else { return nil }
        switch every.last {
        case "m": return [1, 2, 5, 10, 15, 20, 30].contains(n) ? Double(n) * 60 : nil
        case "h": return [1, 2, 3, 4, 6, 8, 12].contains(n) ? Double(n) * 3600 : nil
        case "d": return n == 1 ? 86400 : nil
        default: return nil
        }
    }

    private func nextSlot(_ t: RoundTrigger) -> String? {
        switch t.kind {
        case .cron:
            let span = t.every.flatMap(slotSpan) ?? 3600
            let n = now().timeIntervalSince1970
            return Fixture.stamp(Date(timeIntervalSince1970: (floor(n / span) + 1) * span))
        case .once: return t.at
        default: return nil
        }
    }

    private func round(_ s: RoundSpec, id: String, revision: UInt64, created: String, old: Round?) -> Round {
        var trigger = s.trigger
        if trigger.kind == .cron, let e = trigger.every {
            trigger.cron = RoundText.presets.first { $0.name == e }?.cron
            trigger.every = nil
        }
        if trigger.kind == .cron, trigger.zone == nil { trigger.zone = "UTC" }
        let stamp = Fixture.stamp(now())
        return Round(
            catchUp: s.catchUp ?? .once, command: s.command, createdAt: created, cwd: s.cwd, expiresAt: s.expiresAt,
            harness: s.harness, harnessArgs: s.harnessArgs, id: id, lastRun: old?.lastRun, maxRuns: s.maxRuns,
            mode: s.mode, model: s.model, name: s.name, nextSlot: old?.paused == true ? nil : nextSlot(trigger),
            notify: s.notify ?? .failures, onFinish: s.onFinish ?? .close, paused: old?.paused,
            pausedReason: old?.pausedReason, project: s.project, prompt: s.prompt, revision: revision, target: s.target,
            timeoutMs: s.timeoutMs ?? 3_600_000, trigger: trigger, updatedAt: stamp, waitMs: s.waitMs)
    }

    mutating func create(_ p: RoundCreateParams, projects: [String: Project], sessions: [String: Session]) throws -> Round {
        try check(p.round, projects: projects, sessions: sessions)
        if rounds.values.contains(where: { $0.name == p.round.name }) {
            throw FakeFailure(kind: .conflict, hint: "a round named \(p.round.name) exists")
        }
        let id = "r\(nextID)"
        nextID += 1
        let r = round(p.round, id: id, revision: 1, created: Fixture.stamp(now()), old: nil)
        rounds[id] = r
        return r
    }

    mutating func update(_ p: RoundUpdateParams, projects: [String: Project], sessions: [String: Session]) throws -> Round {
        if p.id.isEmpty { throw FakeFailure(kind: .invalid, hint: "round.update needs the round's id") }
        try check(p.round, projects: projects, sessions: sessions)
        guard let old = rounds[p.id] else { throw FakeFailure(kind: .notFound, hint: "no round \(p.id) on this hab") }
        if let rev = p.revision, rev != 0, rev != old.revision {
            throw FakeFailure(kind: .conflict, hint: "round \(p.round.name) is at revision \(old.revision), not \(rev); read it again")
        }
        if let other = rounds.values.first(where: { $0.name == p.round.name && $0.id != p.id }) {
            throw FakeFailure(kind: .conflict, hint: "a round named \(other.name) exists")
        }
        let r = round(p.round, id: p.id, revision: old.revision + 1, created: old.createdAt, old: old)
        rounds[p.id] = r
        return r
    }

    /// Marks a round deleted at its next revision and drops it; returns the event's round.
    mutating func delete(_ ref: RoundRef) throws -> Round {
        var r = try entry(ref)
        rounds[r.id] = nil
        r.revision += 1
        r.deleted = true
        r.lastRun = nil
        return r
    }

    mutating func setPaused(_ ref: RoundRef, _ on: Bool) throws -> Round? {
        var r = try entry(ref)
        guard (r.paused == true) != on else { return nil }
        r.paused = on ? true : nil
        r.pausedReason = on ? "paused by the user" : nil
        r.nextSlot = on ? nil : nextSlot(r.trigger)
        if !on { r.failures = nil; r.skips = nil }
        r.revision += 1
        rounds[r.id] = r
        return r
    }

    /// Starts a run by hand. Refused with conflict while one is live, or when the round is
    /// paused or completed.
    mutating func startRun(_ ref: RoundRef) throws -> RoundRun {
        let r = try entry(ref)
        if let last = r.lastRun, !last.state.isFinal { throw FakeFailure(kind: .conflict, hint: "the round has a live run") }
        if r.paused == true { throw FakeFailure(kind: .conflict, hint: "the round is paused") }
        if r.completed == true { throw FakeFailure(kind: .conflict, hint: "the round has completed") }
        let n = (runs[r.id]?.last?.number ?? 0) + 1
        let run = RoundRun(created: Fixture.stamp(now()), id: "\(r.id)-run\(n)", number: n, round: r.id, state: .queued, trigger: .manual)
        put(run)
        return run
    }

    /// Stores a run's new state and the round's `lastRun`.
    mutating func put(_ run: RoundRun) {
        var list = runs[run.round] ?? []
        if let i = list.firstIndex(where: { $0.id == run.id }) { list[i] = run } else { list.append(run) }
        runs[run.round] = list
        if var r = rounds[run.round], r.lastRun.map({ $0.number <= run.number }) ?? true {
            r.lastRun = run
            rounds[run.round] = r
        }
    }

    func run(_ id: String) -> RoundRun? { runs.values.lazy.flatMap { $0 }.first { $0.id == id } }

    func page(_ p: RoundRunsParams) throws -> (runs: [RoundRun], more: Bool) {
        let r = try entry(RoundRef(id: p.id, name: p.name))
        if let l = p.limit, l < 0 { throw FakeFailure(kind: .invalid, hint: "limit must not be negative") }
        let limit = min(p.limit.flatMap { $0 == 0 ? nil : $0 } ?? 50, 200)
        let newest = (runs[r.id] ?? []).reversed().filter { p.before == nil || $0.number < p.before! }
        return (Array(newest.prefix(limit)), newest.count > limit)
    }
}
