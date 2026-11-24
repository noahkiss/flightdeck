import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

// The chat view's content (app.md 4.11): tool sentences, the kind mapping, the fold, the Show in
// chat store, the pinned banners and answers from the chat.

private func tool(_ name: String, _ input: [String: JSONValue]? = nil, output: String? = nil, status: ToolStatus = .done, target: String? = nil) -> ChatTool {
    ChatTool(input: input.map(JSONValue.object), name: name, output: output, status: status, target: target)
}

private func card(_ t: ChatTool) -> ToolCard { ChatRender.card(t, children: []) }

private func item(_ id: String, _ t: ChatTool) -> ChatItem { .tool(id: id, card: card(t)) }

final class ToolSentenceTests: XCTestCase {
    func testEditReadsAsASentenceWithCountsAndLineNumbers() throws {
        let c = card(tool("Edit", ["file_path": .string("src/api/stations.ts"), "old_string": .string("// TODO readings"),
                                   "new_string": .string("router.get('/stations/:id/readings',\n  limit(48), readings)")],
                          output: "The file has been updated. Here's the result of running `cat -n` on a snippet:\n    41\u{2192}router.get('/stations/:id', show)\n    42\u{2192}router.get('/stations/:id/readings',\n    43\u{2192}  limit(48), readings)"))
        XCTAssertEqual(c.sentence.text, "Edited src/api/stations.ts +2 -1")
        XCTAssertEqual(c.kind, .edits)
        guard case .diff(let d) = c.detail else { return XCTFail("no diff") }
        let lines = try XCTUnwrap(d.files.first?.hunks.first?.lines)
        XCTAssertEqual(lines.map { $0.old ?? $0.new }, [42, 42, 43], "numbers come from the cat -n snippet")
        XCTAssertTrue(c.fields.isEmpty, "the diff already shows the path and the texts")
    }

    func testEditWithoutASnippetHasNoNumbers() throws {
        let c = card(tool("edit", ["filePath": .string("/work/app/main.go"), "oldString": .string("a"), "newString": .string("b")], output: "Edit applied."))
        XCTAssertEqual(c.sentence.text, "Edited /work/app/main.go +1 -1")
        guard case .diff(let d) = c.detail else { return XCTFail("no diff") }
        XCTAssertTrue(d.files[0].hunks[0].lines.allSatisfy { $0.old == nil && $0.new == nil })
    }

    func testSearchCountsMatchesAndFiles() {
        let out = "src/a.ts:3:readings\nsrc/a.ts:9:readings\nsrc/b.ts:2:readings\nsrc/b.ts:42:readings\nsrc/c.tsx:7:x\nsrc/c.tsx:30:y"
        XCTAssertEqual(card(tool("Grep", ["pattern": .string("readings")], output: out)).sentence.text, "Searched for \"readings\" · 6 matches in 3 files")
        XCTAssertEqual(card(tool("Grep", ["pattern": .string("greeting")], output: "Found 2 files\nsrc/a.ts\nsrc/b.ts")).sentence.text,
                       "Searched for \"greeting\" · in 2 files")
        XCTAssertEqual(card(tool("Grep", ["pattern": .string("zzz")], output: "No matches found")).sentence.text, "Searched for \"zzz\" · no matches")
        XCTAssertEqual(card(tool("Grep", ["pattern": .string("x")], status: .running)).sentence.text, "Searching for \"x\"")
        XCTAssertEqual(card(tool("Glob", ["pattern": .string("src/api/*.ts")], output: "a.ts\nb.ts\nc.ts")).sentence.text,
                       "Found files matching \"src/api/*.ts\" · 3 files")
    }

    func testCommandsReadsAndWebCalls() {
        XCTAssertEqual(card(tool("Bash", ["command": .string("npm test\nnpm run lint"), "description": .string("Run the tests")])).sentence.text, "Ran npm test")
        XCTAssertEqual(card(tool("exec", ["command": .string("ls")], status: .running)).sentence.text, "Running ls")
        XCTAssertEqual(card(tool("Read", ["file_path": .string("src/db/schema.ts")])).sentence.text, "Read src/db/schema.ts")
        XCTAssertEqual(card(tool("webfetch", ["url": .string("https://example.com/docs")])).sentence.text, "Fetched example.com/docs")
        XCTAssertEqual(card(tool("web_search", target: "pytest rename fixture")).sentence.text, "Searched the web for \"pytest rename fixture\"")
        XCTAssertEqual(card(tool("TodoWrite", ["todos": .array([])])).sentence.text, "Updated the plan")
        XCTAssertEqual(card(tool("Task", ["description": .string("Find the slow query"), "prompt": .string("...")])).sentence.text,
                       "Asked a subagent: Find the slow query")
        XCTAssertEqual(card(tool("Write", ["file_path": .string("a.md"), "content": .string("one\ntwo\nthree\n")])).sentence.text, "Wrote a.md +3")
    }

    func testMCPCallsNameTheServer() {
        XCTAssertEqual(card(tool("mcp__claude_ai_Linear__get_issue", ["id": .string("TIDE-41")])).sentence.text, "Asked Linear for issue TIDE-41")
        XCTAssertEqual(card(tool("mcp__docs__search", ["query": .string("pytest fixtures")])).sentence.text, "Asked Docs to search \"pytest fixtures\"")
        XCTAssertEqual(card(tool("mcp__github__list_pull_requests", ["state": .string("open")])).sentence.text, "Asked GitHub to list pull requests")
        XCTAssertEqual(card(tool("mcp__my-server__getStatus", ["serviceId": .int(7)])).sentence.text, "Asked My server for status 7")
        XCTAssertEqual(ToolSentence.serverName("claude_ai_Google_Drive"), "Google Drive")
    }

    func testUnknownToolAndCodexPatch() {
        let u = card(tool("frobnicate", ["level": .int(3), "dry_run": .bool(true), "opts": .object(["a": .int(1)])], target: "thing"))
        XCTAssertEqual(u.sentence.text, "Used frobnicate thing")
        XCTAssertEqual(u.kind, .commands, "an unknown tool falls back to Commands")
        XCTAssertEqual(u.fields.map(\.label), ["Dry run", "Level", "Opts"])
        XCTAssertEqual(u.fields.map(\.value), ["Yes", "3", "{\"a\":1}"])
        XCTAssertEqual(u.fields.last?.code, true, "a nested value shows as JSON")

        let patch: [String: JSONValue] = [
            "/w/app.py": .object(["type": .string("update"), "unified_diff": .string("@@ -3 +3 @@\n-def greet():\n+def hello():\n")]),
            "/w/new.py": .object(["type": .string("add"), "content": .string("x = 1\n")]),
        ]
        let p = card(tool("apply_patch", patch))
        XCTAssertEqual(p.sentence.text, "Edited 2 files +2 -1")
        guard case .diff(let d) = p.detail else { return XCTFail("no diff") }
        XCTAssertEqual(d.files.map(\.newPath), ["/w/app.py", "/w/new.py"])
        XCTAssertEqual(d.files[0].hunks[0].lines.map { $0.old ?? $0.new }, [3, 3])
        XCTAssertTrue(p.fields.isEmpty)
    }

    func testFieldLabels() {
        XCTAssertEqual(ToolField.label("file_path"), "File")
        XCTAssertEqual(ToolField.label("old_string"), "Old text")
        XCTAssertEqual(ToolField.label("maxResultsPerPage"), "Max results per page")
        XCTAssertEqual(ToolField.label("-i"), "Ignore case")
    }
}

@MainActor
final class ChatFilterTests: XCTestCase {
    /// The mapping table of app.md 4.11, per harness.
    func testKindMapping() {
        let table: [(String, ChatContentKind)] = [
            ("TodoWrite", .plans), ("update_plan", .plans), ("todowrite", .plans), ("ExitPlanMode", .plans),
            ("mcp__docs__search", .connectors), ("ListMcpResourcesTool", .connectors),
            ("Task", .agents), ("Agent", .agents), ("agent_wait", .agents), ("task", .agents),
            ("Bash", .commands), ("exec", .commands), ("shell", .commands), ("KillShell", .commands),
            ("Edit", .edits), ("MultiEdit", .edits), ("Write", .edits), ("apply_patch", .edits), ("image_gen", .edits), ("patch", .edits),
            ("Read", .reads), ("Grep", .reads), ("Glob", .reads), ("LS", .reads), ("WebFetch", .reads), ("web_search", .reads),
            ("view_image", .reads), ("codesearch", .reads), ("list", .reads),
            ("SomethingNew", .commands),
        ]
        for (name, kind) in table {
            XCTAssertEqual(ChatKindMap.kind(tool: name), kind, name)
        }
        XCTAssertNil(ChatKindMap.kind(.user(id: "u", text: "hi", sender: nil, attachments: [])), "the owner's messages always show")
        XCTAssertEqual(ChatKindMap.kind(.user(id: "u", text: "hi", sender: "web-codex", attachments: [])), .agents)
        XCTAssertNil(ChatKindMap.kind(.agent(id: "a", text: "", summary: nil, sender: nil)), "replies always show")
        XCTAssertEqual(ChatKindMap.kind(.plan(id: "p", steps: [], latest: true)), .plans)
        XCTAssertNil(ChatKindMap.kind(.event(id: "e", event: .permission, text: "Allowed")))
        XCTAssertNil(ChatKindMap.kind(item("t", tool("Bash", ["command": .string("x")], status: .error))), "a failed call is an error")
        XCTAssertNil(ChatKindMap.kind(item("t", tool("Edit", status: .denied))))
    }

    /// Every tool in the hab's fixtures maps to a kind; the snapshot prints it.
    func testFixtureToolsAllHaveAKind() throws {
        for (_, files) in try ChatSnapshotTests.fixtures() {
            for f in files {
                for m in try ChatSnapshotTests.messages(f) where m.kind == .tool {
                    if let t = m.tool { _ = ChatKindMap.kind(tool: t.name) }
                }
            }
        }
    }

    func testFoldRunsAndSummary() {
        let items: [ChatItem] = [
            .user(id: "u1", text: "go", sender: nil, attachments: []),
            item("r1", tool("Read", ["file_path": .string("a")])),
            item("e1", tool("Edit", ["file_path": .string("a"), "old_string": .string("x"), "new_string": .string("y")])),
            item("r2", tool("Grep", ["pattern": .string("p")])),
            item("c1", tool("Bash", ["command": .string("ls")])),
            item("r3", tool("Glob", ["pattern": .string("*")])),
            item("e2", tool("Write", ["file_path": .string("b"), "content": .string("z")])),
            item("m1", tool("mcp__claude_ai_Linear__get_issue", ["id": .string("TIDE-41")])),
            .agent(id: "a1", text: "done", summary: nil, sender: nil),
            item("c2", tool("Bash", ["command": .string("x")], status: .error)),
        ]
        let rows = ChatFold.rows(items, hidden: [.reads, .edits, .commands])
        XCTAssertEqual(rows.map(\.id), ["u1", "fold-r1", "m1", "a1", "c2"])
        guard case .folded(_, let hidden, let summary) = rows[1] else { return XCTFail("no fold") }
        XCTAssertEqual(hidden.count, 6)
        XCTAssertEqual(summary, "6 steps hidden: 3 reads, 2 edits, 1 command")
        XCTAssertEqual(ChatFold.rows(items, hidden: []).count, items.count, "nothing hidden: every row")
        XCTAssertEqual(ChatFold.summary([item("x", tool("Bash", ["command": .string("ls")]))]), "1 step hidden: 1 command")
        // Ties keep the sheet's order.
        XCTAssertEqual(ChatFold.summary([item("a", tool("Read")), item("b", tool("Bash"))]), "2 steps hidden: 1 command, 1 read")
    }

    @MainActor
    func testStoreScopesPersistAndPrune() throws {
        let d = try XCTUnwrap(UserDefaults(suiteName: "ChatFilterTests-\(UUID().uuidString)"))
        let a = SessionTarget(habID: "h", session: "s1", pane: "%1")
        let b = SessionTarget(habID: "h", session: "s2", pane: "%2")
        let store = ChatShowStore(defaults: d)
        XCTAssertEqual(store.hidden(for: a), [])
        store.set(.reads, shown: false, scope: .every, for: a)
        XCTAssertEqual(store.hidden(for: b), [.reads], "every session")

        store.setScope(.session, for: a)
        store.set(.edits, shown: false, scope: .session, for: a)
        XCTAssertEqual(store.hidden(for: a), [.reads, .edits])
        XCTAssertEqual(store.hidden(for: b), [.reads], "only this one leaves the others")
        XCTAssertEqual(store.scope(for: a), .session)

        // Another store on the same defaults reads it back.
        let again = ChatShowStore(defaults: d)
        XCTAssertEqual(again.hidden(for: a), [.reads, .edits])
        XCTAssertEqual(again.scope(for: b), .every)

        // The hab no longer lists s1: its override goes; another hab's stays.
        let other = SessionTarget(habID: "g", session: "s1", pane: nil)
        again.setScope(.session, for: other)
        again.prune(habID: "h", sessions: ["s2"])
        XCTAssertEqual(again.scope(for: a), .every)
        XCTAssertEqual(again.scope(for: other), .session)
        XCTAssertEqual(ChatShowStore(defaults: d).settings.sessions.keys.sorted(), ["g/s1"])

        // Back to every session drops the override.
        again.setScope(.every, for: other)
        XCTAssertTrue(again.settings.sessions.isEmpty)
    }

    @MainActor
    func testStoreReadsAJSONStringAndSkipsUnknownKinds() throws {
        let d = try XCTUnwrap(UserDefaults(suiteName: "ChatFilterTests-\(UUID().uuidString)"))
        d.set(#"{"hidden":["reads","future-kind"],"sessions":{"h/s1":["edits"]}}"#, forKey: ChatShowStore.key)
        let store = ChatShowStore(defaults: d)
        XCTAssertEqual(store.settings.hidden, [.reads])
        XCTAssertEqual(store.hidden(for: SessionTarget(habID: "h", session: "s1", pane: nil)), [.edits])
    }
}

@MainActor
final class PinnedSummaryTests: XCTestCase {
    func testGroupLine() {
        var g = GroupTests.group()
        // GroupTests' group: api working (member working), ui queued, codex member failed.
        XCTAssertEqual(PinnedSummary.group(g).text, "Group: Split the parser · 1 working, 1 waiting, 1 failed")
        g.state = "paused"
        XCTAssertEqual(PinnedSummary.group(g).text, "Group: Split the parser · Paused")
        g.state = "ended"
        XCTAssertEqual(PinnedSummary.group(g).detail, "Ended")
    }

    func testConfirmLine() {
        let c = Confirmation(created: Fixture.stamp(Date()), expiresAt: Fixture.stamp(Date().addingTimeInterval(3600)), from: "claude-2",
                             id: "c1", kind: .start,
                             start: ConfirmStart(branch: "main", folder: "/w", harness: "claude", host: "attic", name: "n", project: "p", seed: "x"), state: .open)
        XCTAssertEqual(PinnedSummary.confirm(c, remaining: 56 * 60).text, "Start a session? · from claude-2 · 56 min left")
        XCTAssertEqual(PinnedSummary.left(40), "40 s left")
        XCTAssertEqual(PinnedSummary.left(2 * 3600 + 5 * 60), "2 h 5 min left")
        XCTAssertEqual(PinnedSummary.left(3600), "1 h left")
    }
}

@MainActor
final class ChatAnswerTests: XCTestCase {
    private func request(_ tool: String = "Bash", command: String? = "npm run migrate", input: JSONValue? = nil, kind: BlockedKind = .permission) -> OpenRequest {
        OpenRequest(habID: "h", habLabel: "workbench", host: "workbench", pane: "%1", session: "s1", sessionName: "claude", project: nil, model: nil,
                    harness: "claude", contextPct: nil, kind: kind,
                    request: PermissionRequest(command: command, id: "r1", input: input, title: "Run it?", tool: tool), since: nil, offline: false)
    }

    private var question: JSONValue {
        .object(["questions": .array([.object([
            "question": .string("Which palette?"), "multiSelect": .bool(false),
            "options": .array([.object(["label": .string("Harbor at night")]), .object(["label": .string("Garden shed")])]),
        ])])])
    }

    func testComposerModes() {
        let perm = Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission)
        let q = request("AskUserQuestion", command: nil, input: question)
        let qCard = ApprovalCenter.CardState(request: q, canAnswer: true, sending: false, failure: nil, expired: false)
        let pCard = ApprovalCenter.CardState(request: request(), canAnswer: true, sending: false, failure: nil, expired: false)
        XCTAssertEqual(ChatAnswer.composerMode(gate: .now, agent: nil, card: nil), .message)
        XCTAssertEqual(ChatAnswer.composerMode(gate: .blocked, agent: perm, card: qCard), .answer)
        XCTAssertEqual(ChatAnswer.composerMode(gate: .blocked, agent: perm, card: pCard), .queue)
        XCTAssertEqual(ChatAnswer.composerMode(gate: .blocked, agent: perm, card: nil), .queue, "no approvals center: the message still waits")
        var closed = qCard
        closed.canAnswer = false
        XCTAssertEqual(ChatAnswer.composerMode(gate: .blocked, agent: perm, card: closed), .off, "a question the card cannot answer: nothing queues behind its dialog")
        let elicit = Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .question)
        XCTAssertEqual(ChatAnswer.composerMode(gate: .blocked, agent: elicit, card: nil), .off)
        let trust = Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .trust)
        XCTAssertEqual(ChatAnswer.composerMode(gate: .blocked, agent: trust, card: nil), .off)
        XCTAssertEqual(ChatAnswer.placeholder(.answer, gate: .blocked, agent: perm), "Message, or answer above")
    }

    func testNotesAndFailureWords() {
        XCTAssertEqual(ChatAnswer.note(allow: true, request: request()), "Allowed · npm run migrate")
        XCTAssertEqual(ChatAnswer.note(allow: false, request: request()), "Denied · npm run migrate")
        XCTAssertEqual(ChatAnswer.note(allow: nil, reply: "Harbor at night", request: request("AskUserQuestion", command: nil, input: question)),
                       "Answered · Harbor at night")
        let q = request("AskUserQuestion", command: nil, input: question)
        XCTAssertEqual(ChatAnswer.failureText(q, hint: "answering a question is not built yet; answer it in the terminal"),
                       "Flightdeck can't answer this question yet. Answer it in the terminal.")
        XCTAssertEqual(ChatAnswer.failureText(request(), hint: "conflict: request r1 is not open on pane %1"),
                       "The answer didn't go through. Try again, or answer in the terminal.")
    }

    func testEmptyStatesUsePlainWords() {
        var c = ChatContext()
        c.hostLabel = "attic"
        c.harness = "codex"
        let s = ChatEmptyState.of(.unavailable("session s2 has no transcript reader"), context: c)
        XCTAssertEqual(s?.title, "No chat for this session")
        XCTAssertEqual(s?.line, "Codex on attic keeps no conversation Flightdeck can show. The terminal shows the session as it runs.")
        XCTAssertFalse(s?.line.contains("reader") ?? true, "no hab hint on screen")
        XCTAssertEqual(ChatEmptyState.of(.failed("rpc: timeout"), context: c)?.retry, true)
        XCTAssertNil(ChatEmptyState.of(.loading, context: c))
    }

    /// A permission keeps the composer on: the message waits in the app and goes once the
    /// agent is at its prompt again.
    func testMessageWaitsBehindAPermission() {
        var ctx = ChatContext()
        ctx.status = .online
        ctx.agent = Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission)
        let m = ChatModel(target: SessionTarget(habID: "h", session: "s1", pane: "%1"), messages: [], context: ctx)
        XCTAssertTrue(m.queuesWhileBlocked)
        m.send("then add a legend")
        XCTAssertEqual(m.outgoing.map(\.status), [.queued])
        ctx.agent = Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .question)
        m.update(ctx)
        XCTAssertFalse(m.takesMessage, "a question with no id: send is off")
    }

    /// The answer line shows, then goes.
    func testAnswerNoteExpires() async {
        let m = ChatModel(target: SessionTarget(habID: "h", session: "s1", pane: "%1"), messages: [], context: ChatContext())
        m.answerNoteLife = .milliseconds(50)
        m.noteAnswer("Allowed · npm run migrate")
        XCTAssertEqual(m.answerNote, "Allowed · npm run migrate")
        await eventually("note gone") { m.answerNote == nil }
    }

    /// A question answered from the chat reaches the hab as choice answer with the text; the
    /// real hab's refusal shows as a failure and returns false.
    func testReplyFromTheChat() async throws {
        let hab = FakeHab(hostID: "testhost")
        await hab.load(projects: [Fixture.project("p", name: "lantern", sessions: ["s1"])],
                       sessions: [Fixture.session("s1", name: "claude", pane: "%1", project: "p")],
                       panes: [Fixture.pane("%1", session: "s1")],
                       agents: [Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission,
                                              request: Fixture.request(title: "Which palette?", tool: "AskUserQuestion", id: "q1", input: question))])
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "attic")
        let d = try XCTUnwrap(UserDefaults(suiteName: "ChatAnswerTests-\(UUID().uuidString)"))
        let c = ApprovalCenter(board: fleet.board, answerer: ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone"), defaults: d)
        c.isUnlocked = { true }
        fleet.approvals = c
        await fleet.setActive(true)
        await eventually("online") { fleet.board.onlineCount == 1 }
        let t = SessionTarget(habID: "h", session: "s1", pane: "%1")
        await eventually("card") { c.card(for: t) != nil }

        let refused = await c.reply(t, text: "Harbor at night")
        XCTAssertFalse(refused)
        let failure = try XCTUnwrap(c.card(for: t)?.failure)
        XCTAssertEqual(ChatAnswer.failureText(try XCTUnwrap(c.card(for: t)?.request), hint: failure),
                       "Flightdeck can't answer this question yet. Answer it in the terminal.")

        await hab.setAnswersQuestions(true)
        let ok = await c.reply(t, text: "Harbor at night")
        XCTAssertTrue(ok)
        let answers = await hab.answers
        XCTAssertEqual(answers.last?.choice, .answer)
        XCTAssertEqual(answers.last?.text, "Harbor at night")
        XCTAssertEqual(answers.last?.request, "q1")
        await fleet.setActive(false)
    }
}
