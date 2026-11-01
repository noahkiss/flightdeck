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

    /// Every demo hab's connection id starts with this.
    public static let habPrefix = "demo-"

    public static func isRequested(_ defaults: UserDefaults = .standard) -> Bool {
        defaults.bool(forKey: argument)
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
                Fixture.project("p-seed", name: "seed-catalog", sessions: ["s2"]),
                Fixture.project("p-lantern", name: "paper-lantern", sessions: ["s4", "s5"]),
            ],
            sessions: [
                Fixture.session("s1", name: "claude", pane: "%1", project: "p-tide", model: "claude-sonnet", title: "Add a tide height chart", contextPct: 62),
                Fixture.session("s2", name: "codex", pane: "%2", project: "p-seed", model: "gpt-codex", title: "Fix the CSV import"),
                Fixture.session("s3", name: "claude-2", pane: "%3", project: "p-tide", model: "claude-opus", title: "Write the API docs", contextPct: 31),
                Fixture.session("s4", name: "opencode", pane: "%4", project: "p-lantern", model: "qwen-coder", title: "Refactor the layout engine"),
                Fixture.session("s5", name: "dev server", pane: "%5", project: "p-lantern"),
            ],
            panes: ["%1", "%2", "%3", "%4", "%5"].enumerated().map { i, p in Fixture.pane(p, session: "s\(i + 1)") }
                + [Fixture.pane("%6", session: nil)],
            agents: [
                Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission,
                              request: Fixture.request(title: "Run the database migration?", tool: "Bash", command: "npm run migrate"), at: ago(2)),
                Fixture.agent(pane: "%2", harness: "codex", state: .error, at: ago(6)),
                Fixture.agent(pane: "%3", harness: "claude", state: .done, at: ago(1)),
                Fixture.agent(pane: "%4", harness: "opencode", state: .working, at: ago(4)),
            ]
        )
        await hab.setUsage(demoUsage(Date()))
        return hab
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
                                          request: Fixture.request(title: "Delete the build cache?", tool: "Bash", command: "rm -rf .cache/build")))
            try? await Task.sleep(for: step)
            await workbench.put(Fixture.agent(pane: "%4", harness: "opencode", state: .working))
            await attic.put(Fixture.agent(pane: "%12", harness: "claude", state: .working))
        }
    }
}
