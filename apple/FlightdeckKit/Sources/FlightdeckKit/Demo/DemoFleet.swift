import Foundation
import HabAPI

/// Demo mode: two invented hosts with invented sessions across every rank,
/// served by fake habs. A short script changes states every few seconds.
/// Start the app with `-FlightdeckDemo YES`.
///
/// Every name here is invented. None names a real host, user, path or project.
public enum DemoFleet {
    /// The launch argument that turns demo mode on.
    public static let argument = "FlightdeckDemo"

    public static func isRequested(_ defaults: UserDefaults = .standard) -> Bool {
        defaults.bool(forKey: argument)
    }

    /// "Look around with demo data" on first run (app.md 3.9): the demo habs join this run of
    /// the app. Nothing is saved, so the next start opens on pairing again.
    @MainActor
    public static func start(_ fleet: Fleet) async {
        guard !fleet.isDemo else { return }
        fleet.isDemo = true
        await populate(fleet)
    }

    /// Adds the two demo habs to `fleet` and starts the script.
    @MainActor
    public static func populate(_ fleet: Fleet, script: Bool = true) async {
        let workbench = await makeWorkbench()
        let attic = await makeAttic()
        await DemoChat.populate(workbench: workbench, attic: attic)
        fleet.keepAlive += [workbench, attic]
        fleet.add(HabConnection(id: "demo-workbench", transport: workbench.transport(endpoint: "demo")), label: nil)
        fleet.add(HabConnection(id: "demo-attic", transport: attic.transport(endpoint: "demo")), label: nil)
        if script {
            fleet.keepAlive.append(ScriptBox(Task { await run(workbench: workbench, attic: attic) }))
            fleet.keepAlive.append(ScriptBox(await DemoTerminal.populate(workbench: workbench, attic: attic)))
        }
    }

    private final class ScriptBox {
        let task: Task<Void, Never>
        init(_ task: Task<Void, Never>) { self.task = task }
        deinit { task.cancel() }
    }

    static func ago(_ minutes: Double) -> Date {
        Date().addingTimeInterval(-minutes * 60)
    }

    static func makeWorkbench() async -> FakeHab {
        let hab = FakeHab(hostID: "workbench")
        await hab.setHeartbeat(.seconds(15))
        await hab.load(
            projects: [
                Fixture.project("p-tide", name: "tide-tables", sessions: ["s1", "s3"]),
                Fixture.project("p-seed", name: "seed-catalog", sessions: ["s2", "s6"]),
                Fixture.project("p-lantern", name: "paper-lantern", sessions: ["s4", "s5"]),
            ],
            sessions: [
                Fixture.session("s1", name: "claude", pane: "%1", project: "p-tide", model: "claude-sonnet", title: "Add a tide height chart", contextPct: 62),
                Fixture.session("s2", name: "codex", pane: "%2", project: "p-seed", model: "gpt-codex", title: "Fix the CSV import"),
                Fixture.session("s3", name: "claude-2", pane: "%3", project: "p-tide", model: "claude-opus", title: "Write the API docs", contextPct: 31),
                Fixture.session("s4", name: "opencode", pane: "%4", project: "p-lantern", model: "qwen-coder", title: "Refactor the layout engine"),
                Fixture.session("s5", name: "dev server", pane: "%5", project: "p-lantern"),
                groupMember,
            ],
            panes: ["%1", "%2", "%3", "%4", "%5", "%7"].enumerated().map { i, p in Fixture.pane(p, session: i == 5 ? "s6" : "s\(i + 1)") }
                + [Fixture.pane("%6", session: nil)],
            agents: [
                Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission,
                              request: Fixture.request(title: "Run the database migration?", tool: "Bash", command: "npm run migrate", id: "req-demo-migrate"), at: ago(2)),
                Fixture.agent(pane: "%2", harness: "codex", state: .error, at: ago(6)),
                Fixture.agent(pane: "%3", harness: "claude", state: .done, at: ago(1)),
                Fixture.agent(pane: "%4", harness: "opencode", state: .working, at: ago(4)),
                Fixture.agent(pane: "%7", harness: "claude", state: .working, at: ago(9)),
            ]
        )
        await addGroup(hab)
        await hab.setUsage(demoUsage(Date()))
        await addConfirmations(hab)
        await addCapcom(hab)
        await addCapcomJobs(hab)
        await hab.setCapability(FrictionNote.capability, true)
        return hab
    }

    /// The group's lead: a member session that folds into the group's row (app.md 3.1).
    static var groupMember: Session {
        var s = Fixture.session("s6", name: "claude-7c41e0-lead-1", pane: "%7", project: "p-seed", model: "claude-sonnet",
                                title: "Split the parser: the API", contextPct: 27)
        s.group = SessionGroup(id: "g-demo1", item: "api", role: .lead)
        return s
    }

    /// One running group on workbench (groups.md 5.4): the coordinator is claude-2, the lead
    /// works on the API, and the UI waits for it.
    static func addGroup(_ hab: FakeHab) async {
        await hab.setCapability(GroupCenter.capability, true)
        let made = ago(40)
        let plan = GroupPlan(brief: "Split the parser into an API and a UI part", items: [
            GroupPlanItem(id: "api", title: "API"),
            GroupPlanItem(after: "api", id: "ui", title: "UI"),
        ], members: [
            GroupPlanMember(harness: "claude", item: "api", role: .lead),
            GroupPlanMember(harness: "codex", item: "ui", role: .lead),
        ], project: "seed-catalog", title: "Split the parser")
        await hab.loadGroup(Group(
            confirmation: GroupConfirmationRef(id: "none"),
            coordinator: GroupCoordinator(address: "workbench/claude-2", name: "claude-2", session: "s3"),
            created: Fixture.stamp(made), generation: 1, home: FakeHab.deviceID(host: "workbench"), id: "g-demo1",
            inbox: [], items: [
                GroupItem(branch: "group/g-demo1/api", id: "api", lead: "claude-7c41e0-lead-1", state: "working", title: "API"),
                GroupItem(after: "api", id: "ui", reason: "waits for the API to reach review", state: "queued", title: "UI"),
            ], lane: GroupLaneState(queue: []), leftovers: [],
            members: [
                GroupMember(agent: AgentStatus(rawValue: "working"), contextPct: 27, harness: "claude", host: "workbench", item: "api",
                            name: "claude-7c41e0-lead-1", role: .lead, session: "s6", state: "running"),
                GroupMember(harness: "codex", host: "workbench", item: "ui", name: "codex-7c41e0-lead-2", role: .lead, state: "planned"),
            ],
            pause: GroupPause(), plan: plan, project: "seed-catalog", revision: 1, state: "running", title: "Split the parser",
            updated: Fixture.stamp(made), waits: []))
    }

    /// Two Capcom jobs (capcom-service.md 9.6): one that may hand off work and acted last
    /// time, one that only reads and messages.
    static func addCapcomJobs(_ hab: FakeHab) async {
        await hab.setCapability(RoundCenter.capability, true)
        await hab.setCapability(CapcomJobs.capability, true)
        try? await hab.loadRound(RoundSpec(
            actions: [.handOff], mode: .capcom, name: "build-failures",
            prompt: "When a build fails, find the project's agent and hand it the failing test with the log.",
            trigger: RoundTrigger(event: "notice.raised", kind: .event)))
        try? await hab.loadRound(RoundSpec(
            mode: .capcom, name: "morning-brief",
            prompt: "Each morning, tell me what my agents finished overnight and what waits for me.",
            trigger: RoundTrigger(every: "1d", kind: .cron)))
        await hab.loadRoundOutcome(named: "build-failures", "acted")
        await hab.loadRoundOutcome(named: "morning-brief", "told")
    }

    /// One start that waits for the owner and one that ran at once (capcom.md 6.1).
    static func addConfirmations(_ hab: FakeHab) async {
        await hab.setCapability(ConfirmBook.capability, true)
        await hab.setConfirmTargets([ConfirmTarget(host: "workbench", ready: true), ConfirmTarget(host: "attic", ready: true)])
        let made = ago(3)
        await hab.requestConfirmation(Confirmation(
            created: Fixture.stamp(made), expiresAt: Fixture.stamp(made.addingTimeInterval(3600)), from: "claude-2",
            id: "3fa9c2d81b0e4c6a9e5f7d2b1c0a8e64", kind: .start,
            start: ConfirmStart(branch: "main", folder: "/srv/work/tide-tables", harness: "claude", host: "attic",
                                name: "claude-3fa9c2", project: "tide-tables",
                                seed: "Run the full test suite on this host and report which tests fail."),
            state: .open))
    }

    /// Capcom runs on workbench (capcom.md 7): the standing conversation, with one notice
    /// waiting as a chip.
    static func addCapcom(_ hab: FakeHab) async {
        await hab.setCapability("capcom", true)
        await hab.loadCapcomSession("capcom-s1", name: "capcom", pane: "%c1", standing: true)
        await hab.addNotice(id: "n-demo1", title: "Tests failed in seed-catalog")
        try? await hab.attachElsewhere(session: "capcom-s1", notice: "n-demo1")
    }

    static func makeAttic() async -> FakeHab {
        let hab = FakeHab(hostID: "attic")
        await hab.setHeartbeat(.seconds(15))
        await hab.load(
            projects: [
                Fixture.project("p-lantern", name: "paper-lantern", sessions: ["s11"]),
                Fixture.project("p-seed", name: "seed-catalog", sessions: ["s12", "s14"]),
                Fixture.project("p-tide", name: "tide-tables", sessions: ["s13"]),
            ],
            sessions: [
                Fixture.session("s11", name: "claude", pane: "%11", project: "p-lantern", model: "claude-sonnet", title: "Pick a color palette", contextPct: 18),
                Fixture.session("s12", name: "claude-2", pane: "%12", project: "p-seed", model: "claude-sonnet", title: "Speed up search", contextPct: 84),
                Fixture.session("s13", name: "codex", pane: "%13", project: "p-tide", model: "gpt-codex", title: "Update dependencies"),
                Fixture.session("s14", name: "claude-3", pane: nil, project: "p-seed", title: "Old experiment", exited: true),
            ],
            panes: [
                Fixture.pane("%11", session: "s11"),
                Fixture.pane("%12", session: "s12"),
                Fixture.pane("%13", session: "s13"),
            ],
            agents: [
                Fixture.agent(pane: "%11", harness: "claude", state: .blocked, kind: .question, at: ago(3)),
                Fixture.agent(pane: "%12", harness: "claude", state: .working, at: ago(10)),
                Fixture.agent(pane: "%13", harness: "codex", state: .idle, at: ago(25)),
            ]
        )
        // attic knows Capcom runs on workbench.
        await hab.setCapability("capcom", true)
        await hab.setCapcomHome(assignment: 1, home: FakeHab.deviceID(host: "workbench"))
        return hab
    }

    /// Invented usage: a Claude account near its 5-hour limit and a Codex
    /// account over pace on its week (app.md 14).
    static func demoUsage(_ now: Date) -> [UsageAccount] {
        let t = Fixture.stamp(now.addingTimeInterval(-90))
        let at = { (s: TimeInterval) in Fixture.stamp(now.addingTimeInterval(s)) }
        return [
            UsageAccount(harness: "claude", key: "claude-demo01", label: "Max 5x", windows: [
                UsageWindow(name: "5h", readAt: t, resetsAt: at(1 * 3600 + 58 * 60), usedPct: 86),
                UsageWindow(name: "7d", readAt: t, resetsAt: at(4 * 86400 + 23 * 3600), usedPct: 31),
                UsageWindow(name: "7d opus", readAt: t, resetsAt: at(4 * 86400 + 23 * 3600), usedPct: 12),
            ]),
            UsageAccount(harness: "codex", key: "codex-demo02", label: "Plus", windows: [
                UsageWindow(name: "5h", readAt: t, resetsAt: at(3 * 3600 + 10 * 60), usedPct: 22),
                UsageWindow(name: "7d", readAt: t, resetsAt: at(5 * 86400 + 2 * 3600), usedPct: 47),
            ]),
        ]
    }

    /// Loops: a working session finishes, another asks for permission, both go back to work.
    static func run(workbench: FakeHab, attic: FakeHab) async {
        let step: Duration = .seconds(8)
        while !Task.isCancelled {
            try? await Task.sleep(for: step)
            await workbench.put(Fixture.agent(pane: "%4", harness: "opencode", state: .done))
            try? await Task.sleep(for: step)
            await attic.put(Fixture.agent(pane: "%12", harness: "claude", state: .blocked, kind: .permission,
                                          request: Fixture.request(title: "Delete the build cache?", tool: "Bash", command: "rm -rf .cache/build", id: "req-demo-cache")))
            try? await Task.sleep(for: step)
            await workbench.put(Fixture.agent(pane: "%4", harness: "opencode", state: .working))
            await attic.put(Fixture.agent(pane: "%12", harness: "claude", state: .working))
        }
    }
}
