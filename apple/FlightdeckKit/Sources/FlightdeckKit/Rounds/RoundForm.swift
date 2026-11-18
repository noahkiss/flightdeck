import Foundation
import HabAPI

/// The New round and Edit form's fields (rounds.md 3, 4). The hab checks every rule again; the
/// form checks only what it needs to build a `RoundSpec`.
public struct RoundDraft: Equatable, Sendable {
    public enum When: String, CaseIterable, Sendable, Identifiable {
        case every = "Every", cron = "Cron", once = "Once", manual = "By hand"
        public var id: String { rawValue }
    }

    public enum Runs: String, CaseIterable, Sendable, Identifiable {
        case harness = "Agent", command = "Command"
        public var id: String { rawValue }
    }

    public var habID = ""
    public var name = ""
    public var mode: RoundMode = .fresh
    public var when: When = .every
    public var every = "1h"
    public var cron = ""
    /// The round's zone; empty keeps the hab's zone at creation, or the round's on edit.
    public var zone = ""
    public var at = Date().addingTimeInterval(3600)
    public var runs: Runs = .harness
    public var project: String?
    public var target: String?
    public var harness = "claude"
    public var model = ""
    public var prompt = ""
    /// The program and its arguments, split on spaces.
    public var command = ""
    public var notify: RoundNotify = .failures
    public var onFinish: RoundOnFinish = .close
    public var catchUp: RoundCatchUp = .once
    public var timeoutMinutes = 60
    /// 0: no limit.
    public var maxRuns = 0
    public var runNow = false
    /// The round's fields the form does not show, kept as they were on edit.
    var kept: Round?

    public init(habID: String = "") { self.habID = habID }

    /// The form for editing `r`.
    public init(habID: String, round r: Round) {
        self.habID = habID
        kept = r
        name = r.name
        mode = r.mode
        switch r.trigger.kind {
        case .cron:
            if let p = RoundText.preset(cron: r.trigger.cron ?? "") {
                when = .every
                every = p
            } else {
                when = .cron
                cron = r.trigger.cron ?? ""
            }
        case .once:
            when = .once
            at = r.trigger.at.flatMap(RoundText.parse) ?? at
        default:
            when = .manual
        }
        zone = r.trigger.zone ?? ""
        if let c = r.command, !c.isEmpty {
            runs = .command
            command = c.joined(separator: " ")
        } else {
            runs = .harness
            harness = r.harness ?? harness
            prompt = r.prompt ?? ""
        }
        model = r.model ?? ""
        project = r.project
        target = r.target
        notify = r.notify ?? .failures
        onFinish = r.onFinish ?? .close
        catchUp = r.catchUp ?? .once
        timeoutMinutes = Self.minutes(r.timeoutMs)
        maxRuns = r.maxRuns ?? 0
    }

    public var isEdit: Bool { kept != nil }

    /// A timeout as the stepper shows it: whole minutes, at least 1; 60 when unset.
    static func minutes(_ ms: Int?) -> Int { ms.map { max(1, $0 / 60_000) } ?? 60 }

    /// The spec the hab gets, or the first thing missing.
    public func spec() -> Result<RoundSpec, RoundActionError> {
        let name = name.trimmingCharacters(in: .whitespaces)
        guard !name.isEmpty else { return .failure(RoundActionError("Give the round a name.")) }
        var t = RoundTrigger(kind: .manual)
        let z = zone.trimmingCharacters(in: .whitespaces)
        switch when {
        case .every:
            t = RoundTrigger(every: every, kind: .cron, zone: z.isEmpty ? nil : z)
        case .cron:
            let c = cron.trimmingCharacters(in: .whitespaces)
            guard !c.isEmpty else { return .failure(RoundActionError("Write a cron expression.")) }
            t = RoundTrigger(cron: c, kind: .cron, zone: z.isEmpty ? nil : z)
        case .once:
            t = RoundTrigger(at: RoundText.stamp(at), kind: .once, zone: z.isEmpty ? nil : z)
        case .manual:
            break
        }
        var s = RoundSpec(mode: mode, name: name, trigger: t)
        // Fields the form does not show stay as they were.
        if let k = kept {
            s.cwd = k.cwd
            s.expiresAt = k.expiresAt
            s.harnessArgs = k.harnessArgs
            s.waitMs = k.waitMs
        }
        s.model = model.isEmpty ? nil : model
        switch mode {
        case .fresh:
            guard let p = project else { return .failure(RoundActionError("Choose a project.")) }
            s.project = p
            if runs == .command {
                let argv = command.split(whereSeparator: \.isWhitespace).map(String.init)
                guard !argv.isEmpty else { return .failure(RoundActionError("Write the command to run.")) }
                s.command = argv
                s.model = nil
                s.harnessArgs = nil
            } else {
                guard !prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                    return .failure(RoundActionError("Write the prompt."))
                }
                s.harness = harness
                s.prompt = prompt
            }
            s.onFinish = onFinish
        default:
            guard let target else { return .failure(RoundActionError("Choose the session to \(mode == .fork ? "fork" : "resume").")) }
            guard !prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                return .failure(RoundActionError("Write the prompt."))
            }
            s.target = target
            s.prompt = prompt
            s.cwd = nil
            if mode == .fork { s.onFinish = onFinish }
        }
        s.notify = notify
        s.catchUp = catchUp
        // An edit keeps the round's own timeout, which may not be whole minutes, unless the
        // stepper moved.
        if let k = kept, timeoutMinutes == Self.minutes(k.timeoutMs) {
            s.timeoutMs = k.timeoutMs ?? timeoutMinutes * 60_000
        } else {
            s.timeoutMs = timeoutMinutes * 60_000
        }
        s.maxRuns = maxRuns > 0 ? maxRuns : nil
        return .success(s)
    }
}
