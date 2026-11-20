import Foundation
import HabAPI
import Observation

/// One Capcom job: a round in `capcom` mode (capcom-service.md 9.6).
public struct CapcomJob: Identifiable, Hashable, Sendable {
    public var habID: String
    public var habLabel: String
    public var round: Round
    public var id: String { "\(habID)/r/\(round.id)" }

    /// The words the job allows now.
    public var words: Set<RoundAction> { Set(round.actions ?? []) }

    /// The last run's outcome word (`told`, `quiet`, `acted`, `unseen`, `unknown`), if any.
    public var outcome: String? { round.lastRun?.outcome?.word }

    public var row: RoundRow { RoundRow(habID: habID, round: round) }
}

/// A grant waiting for the person: the job's prompt as they read it, and the revision it
/// was read at. Confirm sends that revision, so a change since fails the grant.
public struct CapcomJobGrant: Identifiable, Equatable, Sendable {
    public var habID: String
    /// The round as the person read it.
    public var round: Round
    public var word: RoundAction
    /// The job changed since the person opened it: the hab refused a grant at an older revision.
    public var changed: Bool
    public var id: String { "\(habID)/r/\(round.id)/\(word.rawValue)" }
    public var jobID: String { "\(habID)/r/\(round.id)" }
    public var revision: UInt64 { round.revision }
    public var prompt: String { round.prompt ?? "" }
}

/// The Capcom jobs list (capcom-service.md 9.6, app.md 21.5): the rounds in `capcom` mode on
/// every hab that knows action words, and the switches that set each job's words. Turning a
/// word on is a person's grant: the first grant on a job, or one after its prompt changed,
/// shows the prompt first, and every grant carries the revision the person read. Turning a
/// word off sends the removal at once.
@MainActor
@Observable
public final class CapcomJobs {
    /// The welcome capability that says a hab knows a capcom round's action words.
    public nonisolated static let capability = "round-actions"

    /// The action words in the hab's order, each with its switch label.
    public nonisolated static let words: [(word: RoundAction, title: String)] = [
        (.handOff, "Hand off work to an agent"),
        (.startSession, "Start a session"),
        (.setUpProject, "Set up a project"),
        (.group, "Start or change a group"),
        (.card, "Add calendar and reminder cards"),
        (.fetch, "Open pages it has not seen"),
        (.drive, "Type into panes and keep agents fresh"),
    ]

    /// A grant that waits for the person to read the job's prompt.
    public var ask: CapcomJobGrant?
    /// The last refusal or failure, by job id.
    public private(set) var errors: [String: String] = [:]
    /// Jobs with a change in flight.
    public private(set) var busy: Set<String> = []

    @ObservationIgnored private let board: Board
    @ObservationIgnored private let connection: @MainActor (String) -> HabConnection?
    @ObservationIgnored private let defaults: UserDefaults
    /// The prompt each job had when the person last granted it a word, as a SHA-256 digest
    /// by job id. A grant with the same prompt needs no second read.
    @ObservationIgnored private var seen: [String: String]

    static let seenKey = "FlightdeckCapcomJobPrompts"

    public init(board: Board, defaults: UserDefaults = .standard,
                connection: @escaping @MainActor (String) -> HabConnection?) {
        self.board = board
        self.defaults = defaults
        self.connection = connection
        seen = defaults.dictionary(forKey: Self.seenKey) as? [String: String] ?? [:]
    }

    // MARK: - List

    private func knows(_ h: Board.Hab) -> Bool {
        let caps = h.state.welcome?.capabilities ?? []
        return caps.contains(RoundCenter.capability) && caps.contains(Self.capability)
    }

    /// Whether any hab in the app knows Capcom jobs. Without one, the section does not show.
    public var available: Bool { board.habs.contains(where: knows) }

    /// Every `capcom` round on the habs that know action words, by name, then hab.
    public var jobs: [CapcomJob] {
        board.habs.filter(knows).flatMap { h in
            h.state.rounds.values.filter { $0.mode == .capcom }.map { CapcomJob(habID: h.id, habLabel: h.label, round: $0) }
        }
        .sorted { ($0.round.name, $0.habLabel, $0.round.id) < ($1.round.name, $1.habLabel, $1.round.id) }
    }

    public func job(habID: String, round: String) -> CapcomJob? {
        guard let h = board.habs.first(where: { $0.id == habID }), knows(h),
              let r = h.state.rounds[round], r.mode == .capcom else { return nil }
        return CapcomJob(habID: h.id, habLabel: h.label, round: r)
    }

    /// The last outcome in plain words.
    public nonisolated static func outcomeText(_ word: String?) -> String {
        switch word {
        case nil: return "No run yet"
        case "told": return "Told you"
        case "quiet": return "Stayed quiet"
        case "acted": return "Acted"
        case "unseen": return "Not seen"
        case "unknown": return "No answer"
        case let w?: return w
        }
    }

    // MARK: - Switches

    /// A switch moved. Off sends the removal at once. On asks the person to read the prompt
    /// first, unless they granted this job a word with this same prompt before.
    public func set(_ job: CapcomJob, _ word: RoundAction, on: Bool) async {
        errors[job.id] = nil
        if !on {
            await remove(job, word)
            return
        }
        guard !job.words.contains(word) else { return }
        let grant = CapcomJobGrant(habID: job.habID, round: job.round, word: word, changed: false)
        if seen[job.id] == Self.digest(grant.prompt) {
            await send(grant)
        } else {
            ask = grant
        }
    }

    /// The person read the prompt and said yes: send the grant at the revision they read.
    public func confirm() async {
        guard let g = ask else { return }
        ask = nil
        await send(g)
    }

    public func cancel() { ask = nil }

    public func clearError(_ job: CapcomJob) { errors[job.id] = nil }

    /// `round.update` with the word added, at the grant's revision. A conflict means the job
    /// changed since the person read it: reload it and show its prompt again; never grant.
    private func send(_ g: CapcomJobGrant) async {
        let id = g.jobID
        guard let conn = connection(g.habID) else {
            errors[id] = "That hab is no longer in the app."
            return
        }
        busy.insert(id)
        defer { busy.remove(id) }
        var spec = RoundSpec(round: g.round)
        spec.actions = Self.normal((g.round.actions ?? []) + [g.word])
        do {
            _ = try await conn.call(HabMethods.roundUpdate, RoundUpdateParams(id: g.round.id, revision: g.revision, round: spec))
            remember(id, prompt: g.prompt)
        } catch let e as RPCError where e.kind == .conflict {
            do {
                let fresh = try await conn.call(HabMethods.roundGet, RoundGetParams(id: g.round.id)).round
                guard fresh.mode == .capcom, !(fresh.actions ?? []).contains(g.word) else { return }
                ask = CapcomJobGrant(habID: g.habID, round: fresh, word: g.word, changed: true)
            } catch {
                errors[id] = RoundCenter.sentence(RoundCenter.text(error))
            }
        } catch {
            errors[id] = RoundCenter.sentence(RoundCenter.text(error))
        }
    }

    /// `round.update` with the word taken out. A removal only narrows what the job may do, so
    /// a conflict reloads the job and sends the removal once more on the fresh round.
    private func remove(_ job: CapcomJob, _ word: RoundAction) async {
        guard let conn = connection(job.habID) else {
            errors[job.id] = "That hab is no longer in the app."
            return
        }
        busy.insert(job.id)
        defer { busy.remove(job.id) }
        var round = job.round
        for attempt in 0..<2 {
            guard (round.actions ?? []).contains(word) else { return }
            var spec = RoundSpec(round: round)
            spec.actions = (round.actions ?? []).filter { $0 != word }
            do {
                _ = try await conn.call(HabMethods.roundUpdate, RoundUpdateParams(id: round.id, revision: round.revision, round: spec))
                return
            } catch let e as RPCError where e.kind == .conflict && attempt == 0 {
                do {
                    round = try await conn.call(HabMethods.roundGet, RoundGetParams(id: round.id)).round
                } catch {
                    errors[job.id] = RoundCenter.sentence(RoundCenter.text(error))
                    return
                }
            } catch {
                errors[job.id] = RoundCenter.sentence(RoundCenter.text(error))
                return
            }
        }
    }

    private func remember(_ id: String, prompt: String) {
        seen[id] = Self.digest(prompt)
        defaults.set(seen, forKey: Self.seenKey)
    }

    static func digest(_ s: String) -> String { AttachmentUpload.sha256(Data(s.utf8)) }

    /// The words in the hab's order, each once.
    nonisolated static func normal(_ w: [RoundAction]) -> [RoundAction] {
        let set = Set(w)
        return words.map(\.word).filter(set.contains)
    }
}

extension RoundSpec {
    /// The whole definition of `r`, as `round.update` takes it: an update replaces every
    /// field (rounds.md 13). The project's remote is read only and stays out.
    public init(round r: Round) {
        self.init(actions: r.actions, catchUp: r.catchUp, command: r.command, cooldownMs: r.cooldownMs, cwd: r.cwd,
                  debounceMs: r.debounceMs, expiresAt: r.expiresAt, group: r.group, harness: r.harness,
                  harnessArgs: r.harnessArgs, maxRuns: r.maxRuns, mode: r.mode, model: r.model, name: r.name,
                  notify: r.notify, onFinish: r.onFinish, paused: r.paused, project: r.project, projectDir: r.projectDir,
                  prompt: r.prompt, secrets: r.secrets, target: r.target, timeoutMs: r.timeoutMs, trigger: r.trigger,
                  waitMs: r.waitMs)
    }
}
