import Foundation
import HabAPI

/// Invented conversations for demo mode. Every name, path and line here is made up.
enum DemoChat {
    static func msg(_ session: String, _ n: Int, _ kind: ChatKind, _ minutesAgo: Double, text: String? = nil,
                    tool: ChatTool? = nil, event: ChatEvent? = nil, plan: [PlanStep]? = nil, sender: String? = nil) -> ChatMessage {
        ChatMessage(at: Fixture.stamp(DemoFleet.ago(minutesAgo)), event: event, id: "\(session)-m\(n)", kind: kind,
                    plan: plan, sender: sender, session: session, text: text, tool: tool)
    }

    static func input(_ pairs: [String: String]) -> JSONValue {
        .object(pairs.mapValues { .string($0) })
    }

    /// The tide chart session: a plan, reads, an edit, and a pending migration.
    static func tide(_ s: String) -> [ChatMessage] {
        [
            msg(s, 1, .user, 14, text: "Add a chart of tide heights to the station page. Use the readings we already store."),
            msg(s, 2, .agent, 13.8, text: "I'll look at how readings are stored first, then add the chart."),
            msg(s, 3, .plan, 13.7, plan: [
                PlanStep(status: .done, text: "Find where readings are stored"),
                PlanStep(status: .done, text: "Add a readings endpoint for one station"),
                PlanStep(status: .inProgress, text: "Draw the chart on the station page"),
                PlanStep(status: .pending, text: "Add a migration for the new index"),
            ]),
            msg(s, 4, .tool, 13.5, tool: ChatTool(input: input(["pattern": "readings"]), name: "Grep", output: "src/db/schema.ts\nsrc/api/stations.ts", status: .done, target: "readings")),
            msg(s, 5, .tool, 13.2, tool: ChatTool(input: input(["file_path": "src/db/schema.ts"]), name: "Read", output: "export const readings = table('readings', {\n  station: text(),\n  at: timestamp(),\n  height_m: real(),\n})", status: .done, target: "src/db/schema.ts")),
            msg(s, 6, .agent, 12, text: "Readings live in one table, keyed by station and time. A query by station will need an index:\n\n```sql\nCREATE INDEX readings_station_at ON readings (station, at);\n```\n\nI'll add the endpoint next."),
            msg(s, 7, .tool, 10, tool: ChatTool(input: input(["file_path": "src/api/stations.ts", "old_string": "router.get('/stations/:id', station)", "new_string": "router.get('/stations/:id', station)\nrouter.get('/stations/:id/readings', readings)"]), name: "Edit", output: "The file was updated.", status: .done, target: "src/api/stations.ts")),
            msg(s, 8, .event, 9, text: "Allowed Edit (once)", event: .permission),
            msg(s, 9, .agent, 4, text: "The chart now draws on the station page:\n\n- **x axis:** time, last 48 hours\n- **y axis:** height in metres\n- high and low tides are marked\n\nThe index needs a migration before this is fast on large stations."),
            msg(s, 10, .tool, 2, tool: ChatTool(input: input(["command": "npm run migrate", "description": "Apply the new index"]), name: "Bash", status: .running, target: "npm run migrate")),
        ]
    }

    /// The API docs session: finished.
    static func docs(_ s: String) -> [ChatMessage] {
        [
            msg(s, 1, .user, 30, text: "Write reference docs for the public API. One page per resource."),
            msg(s, 2, .tool, 29, tool: ChatTool(input: input(["pattern": "src/api/*.ts"]), name: "Glob", output: "src/api/stations.ts\nsrc/api/tides.ts\nsrc/api/alerts.ts", status: .done, target: "src/api/*.ts")),
            msg(s, 3, .tool, 20, tool: ChatTool(input: input(["file_path": "docs/api/stations.md", "content": "# Stations\n\n`GET /stations/:id` returns one station."]), name: "Write", output: "File created.", status: .done, target: "docs/api/stations.md")),
            msg(s, 4, .event, 15, text: "Conversation compacted", event: .compaction),
            msg(s, 5, .tool, 3, tool: ChatTool(input: input(["command": "npm run docs:check"]), name: "Bash", output: "3 pages, 0 broken links", status: .done, target: "npm run docs:check")),
            msg(s, 6, .agent, 1, text: "## Done\n\nI wrote three pages under `docs/api/`: stations, tides and alerts. Each lists its routes, parameters and an example response. The link check passes."),
        ]
    }

    /// The palette session: a question to the owner.
    static func palette(_ s: String) -> [ChatMessage] {
        [
            msg(s, 1, .user, 8, text: "Pick a color palette for the lantern themes. Warm, with good contrast."),
            msg(s, 2, .agent, 6, text: "I made two options. Both pass contrast checks for body text.\n\n1. **Ember:** deep red, amber, cream\n2. **Dusk:** plum, peach, sand\n\nWhich one should I apply?"),
        ]
    }

    /// The search session: working, with a message from another agent.
    static func search(_ s: String) -> [ChatMessage] {
        [
            msg(s, 1, .user, 20, text: "Search feels slow on the full catalog. Find out why and fix it."),
            msg(s, 2, .tool, 19, tool: ChatTool(input: input(["command": "npm run bench -- search"]), name: "Bash", output: "search 'tomato': 840 ms\nsearch 'bean': 910 ms", status: .done, target: "npm run bench -- search")),
            msg(s, 3, .agent, 18, text: "Each search scans every row. A full-text index should fix it."),
            msg(s, 4, .user, 12, text: "The nightly import rebuilds the catalog table, so keep the index in the import too.", sender: "import-agent"),
            msg(s, 5, .tool, 11, tool: ChatTool(input: input(["file_path": "src/search.ts", "old_string": "rows.filter(r => r.name.includes(q))", "new_string": "db.fts('catalog', q)"]), name: "Edit", output: "The file was updated.", status: .done, target: "src/search.ts")),
            msg(s, 6, .tool, 1, tool: ChatTool(input: input(["command": "npm test"]), name: "Bash", status: .running, target: "npm test")),
        ]
    }

    /// The standing Capcom conversation: a question answered from the habs' state.
    static func capcom(_ s: String) -> [ChatMessage] {
        [
            msg(s, 1, .user, 50, text: "Which sessions need me right now?"),
            msg(s, 2, .agent, 49, text: "Two do:\n\n- **tide-tables** on workbench asks to run the database migration.\n- **paper-lantern** on attic asks which palette to apply.\n\nThe rest are working or done."),
            msg(s, 3, .user, 30, text: "Is anyone already on seed-catalog?"),
            msg(s, 4, .agent, 29, text: "Yes. A Claude Code session on attic is speeding up search, and a Codex session on workbench is fixing the CSV import."),
        ]
    }

    /// Loads the demo conversations and makes `session.send` answer with a short invented reply.
    static func populate(workbench: FakeHab, attic: FakeHab) async {
        await workbench.setChat(session: "s1", tide("s1"))
        await DemoContent.populate(workbench, session: "s1")
        await workbench.setChat(session: "s3", docs("s3"))
        await workbench.setChat(session: "s4", [
            msg("s4", 1, .user, 5, text: "Split the layout engine into measure and place passes."),
            msg("s4", 2, .agent, 4, text: "Working on it. The measure pass is done; the place pass is next."),
        ])
        await workbench.setChat(session: "capcom-s1", capcom("capcom-s1"))
        await attic.setChat(session: "s11", palette("s11"))
        await attic.setChat(session: "s12", search("s12"))
        await attic.setChat(session: "s13", [
            msg("s13", 1, .user, 40, text: "Update the dependencies and run the tests."),
            msg("s13", 2, .agent, 26, text: "All dependencies are current. The tests pass."),
        ])
        for (hab, panes) in [(workbench, ["s1": "%1", "s3": "%3", "s4": "%4", "capcom-s1": "%c1"]), (attic, ["s11": "%11", "s12": "%12", "s13": "%13"])] {
            let panes = panes
            await hab.setSend(works: true, anyHarness: true) { [weak hab] p in
                guard let hab else { return }
                await reply(hab, send: p, pane: panes[p.session])
            }
        }
    }

    /// A demo answer: the owner's message, a working state, then a short reply.
    static func reply(_ hab: FakeHab, send p: SessionSendParams, pane: String?) async {
        let n = await hab.chat(p.session).count + 1
        await hab.putChat(ChatMessage(at: Fixture.stamp(Date()), id: "\(p.session)-u\(n)", kind: .user, session: p.session, text: p.text))
        guard let pane, let harness = await hab.agent(pane)?.harness else { return }
        await hab.put(Fixture.agent(pane: pane, harness: harness, state: .working))
        try? await Task.sleep(for: .seconds(2))
        await hab.putChat(ChatMessage(at: Fixture.stamp(Date()), id: "\(p.session)-r\(n)", kind: .agent, session: p.session,
                                      text: "This is demo mode, so nothing ran. In a real session the agent answers here."))
        await hab.put(Fixture.agent(pane: pane, harness: harness, state: .done))
    }
}
