import XCTest

/// Synthetic fixtures only, in the shape zellij-nkmk prints them.
final class ZellijForkDiscoveryTests: XCTestCase {
    private let sessionsJSON = """
    [
      {
        "name": "demo",
        "created_seconds_ago": 9000,
        "is_current": false,
        "is_dead": false,
        "connected_clients": 1,
        "web_client_count": 0,
        "tab_count": 2,
        "pane_count": 3,
        "server_build": "same"
      },
      {
        "name": "old",
        "created_seconds_ago": 90,
        "is_current": false,
        "is_dead": true
      },
      {
        "name": "bare",
        "created_seconds_ago": 0,
        "is_current": false,
        "is_dead": false
      }
    ]
    """

    private let demoAgentsJSON = """
    [
      {
        "handle": "quiet-heron",
        "pane_id": 4,
        "tab_id": 7,
        "tab_position": 1,
        "tab_name": "review",
        "title": "Reviewing",
        "kind": "codex",
        "source": "command"
      },
      {
        "handle": "sunny-otter",
        "pane_id": 1,
        "tab_id": 3,
        "tab_position": 0,
        "tab_name": "develop",
        "title": "Working",
        "kind": "claude",
        "agent_id": "00000000-0000-0000-0000-000000000000",
        "source": "command+env",
        "session_source": "record",
        "cwd": "/home/user/src"
      }
    ]
    """

    private func forkOutput(sessions: String, agents: [(String, String)], captures: [(String, String)] = []) -> String {
        var out = "noise before the frame\n::FDZ1::\n::FDZ_SESSIONS::\n\(sessions)\n::FDZ_AGENTS::\n"
        for (name, json) in agents {
            out += "::FDZ_AGENT:\(name)::\n\(json)\n\n"
        }
        if !captures.isEmpty {
            out += "::CAPTURES::\n"
            for (name, content) in captures {
                out += "::CAPTURE:\(name)::\n\(content)\n"
            }
        }
        return out
    }

    // MARK: - Parser

    func testParsesLiveDeadAndMetadataLessSessions() throws {
        let output = forkOutput(
            sessions: sessionsJSON,
            agents: [("demo", demoAgentsJSON), ("bare", "[]")],
            captures: [("demo", "$ ls\nfile.txt\n\n  ")]
        )
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(output))
        XCTAssertEqual(sessions.map(\.name), ["demo", "old", "bare"])

        let demo = sessions[0]
        XCTAssertTrue(demo.isAttached)
        XCTAssertFalse(demo.isExited)
        XCTAssertEqual(demo.agents.map(\.handle), ["sunny-otter", "quiet-heron"])
        XCTAssertEqual(demo.agents.first?.agentId, "00000000-0000-0000-0000-000000000000")
        XCTAssertEqual(demo.extras.tabCount, 2)
        XCTAssertEqual(demo.extras.paneCount, 3)
        XCTAssertEqual(demo.extras.connectedClients, 1)
        XCTAssertEqual(demo.capturedContent, "$ ls\nfile.txt")

        let old = sessions[1]
        XCTAssertFalse(old.isAttached)
        XCTAssertTrue(old.isExited)
        XCTAssertNil(old.entry.connectedClients)
        XCTAssertNil(old.entry.tabCount)
        XCTAssertEqual(old.agents, [])

        let bare = sessions[2]
        XCTAssertFalse(bare.isAttached)
        XCTAssertFalse(bare.isExited)
        XCTAssertEqual(bare.agents, [])
        XCTAssertNil(bare.capturedContent)
    }

    func testCurrentSessionCountsAsAttached() throws {
        let json = #"[{"name": "here", "created_seconds_ago": 5, "is_current": true, "is_dead": false}]"#
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(forkOutput(sessions: json, agents: [])))
        XCTAssertTrue(sessions[0].isAttached)
    }

    func testEmptySessionListIsEmptyNotNil() {
        let sessions = ZellijForkDiscovery.parse(forkOutput(sessions: "[]", agents: []))
        XCTAssertEqual(sessions, [])
    }

    func testDeadSessionNeverCarriesAgents() throws {
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(
            forkOutput(sessions: sessionsJSON, agents: [("old", demoAgentsJSON)])))
        XCTAssertEqual(sessions[1].agents, [])
    }

    func testNoiseAroundJSONIsIgnored() throws {
        let output = forkOutput(
            sessions: "warning line\n\(sessionsJSON)\ntrailing note",
            agents: [("demo", "another warning\n\(demoAgentsJSON)\n")]
        )
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(output))
        XCTAssertEqual(sessions.count, 3)
        XCTAssertEqual(sessions[0].agents.count, 2)
    }

    func testTruncatedSessionListReturnsNil() {
        let truncated = String(sessionsJSON.prefix(120))
        XCTAssertNil(ZellijForkDiscovery.parse(forkOutput(sessions: truncated, agents: [])))
    }

    func testTruncatedAgentListDropsOnlyThatSessionsAgents() throws {
        let output = forkOutput(
            sessions: sessionsJSON,
            agents: [("bare", demoAgentsJSON), ("demo", String(demoAgentsJSON.prefix(80)))]
        )
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(output))
        XCTAssertEqual(sessions[0].agents, [])
        XCTAssertEqual(sessions[2].agents.count, 2)
    }

    func testMalformedAgentElementIsDropped() throws {
        let agents = #"[{"handle": "broken"}, {"handle": "sunny-otter", "pane_id": 1, "tab_id": 0, "tab_position": 0, "tab_name": "main", "title": "", "kind": "pi", "source": "command"}]"#
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(
            forkOutput(sessions: sessionsJSON, agents: [("demo", agents)])))
        XCTAssertEqual(sessions[0].agents.map(\.handle), ["sunny-otter"])
    }

    func testAgentStateFieldsDecodeWhenPresent() throws {
        let agents = #"[{"handle": "sunny-otter", "pane_id": 1, "tab_id": 0, "tab_position": 0, "tab_name": "main", "title": "", "kind": "claude", "source": "command", "state": "blocked", "state_kind": "question", "state_app": "claude-code", "state_title": "Choosing", "state_msg": "answer: Pick one (A · B)", "state_progress": 40, "state_at": 1700000000000}]"#
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(
            forkOutput(sessions: sessionsJSON, agents: [("demo", agents)])))
        let agent = try XCTUnwrap(sessions[0].agents.first)
        XCTAssertEqual(agent.state, "blocked")
        XCTAssertEqual(agent.stateKind, "question")
        XCTAssertEqual(agent.stateApp, "claude-code")
        XCTAssertEqual(agent.stateTitle, "Choosing")
        XCTAssertEqual(agent.stateMsg, "answer: Pick one (A · B)")
        XCTAssertEqual(agent.stateProgress, 40)
        XCTAssertEqual(agent.stateAt, 1_700_000_000_000)
    }

    func testAgentStateFieldsAbsentDecodeAsNil() throws {
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(
            forkOutput(sessions: sessionsJSON, agents: [("demo", demoAgentsJSON)])))
        for agent in sessions[0].agents {
            XCTAssertNil(agent.state)
            XCTAssertNil(agent.stateKind)
            XCTAssertNil(agent.stateApp)
            XCTAssertNil(agent.stateTitle)
            XCTAssertNil(agent.stateMsg)
            XCTAssertNil(agent.stateProgress)
            XCTAssertNil(agent.stateAt)
            XCTAssertNil(agent.session)
        }
    }

    func testAllSessionsAgentListIsGroupedBySession() throws {
        let all = """
        [
          {"handle": "quiet-heron", "pane_id": 4, "tab_id": 7, "tab_position": 1, "tab_name": "review", "title": "", "kind": "codex", "source": "command", "session": "demo"},
          {"handle": "brave-lynx", "pane_id": 2, "tab_id": 0, "tab_position": 0, "tab_name": "main", "title": "", "kind": "pi", "source": "command", "session": "bare"},
          {"handle": "sunny-otter", "pane_id": 1, "tab_id": 3, "tab_position": 0, "tab_name": "develop", "title": "", "kind": "claude", "source": "command", "session": "demo"},
          {"handle": "lost-wren", "pane_id": 9, "tab_id": 0, "tab_position": 0, "tab_name": "x", "title": "", "kind": "claude", "source": "command"}
        ]
        """
        let output = "::FDZ1::\n::FDZ_SESSIONS::\n\(sessionsJSON)\n::FDZ_AGENTS::\n::FDZ_ALL::\n\(all)\n"
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(output))
        XCTAssertEqual(sessions[0].agents.map(\.handle), ["sunny-otter", "quiet-heron"])
        XCTAssertEqual(sessions[1].agents, [])
        XCTAssertEqual(sessions[2].agents.map(\.handle), ["brave-lynx"])
        XCTAssertEqual(sessions[2].agents.first?.session, "bare")
    }

    func testStockOutputReturnsNil() {
        let stock = "::SESSIONS::\ndemo [Created 2h 30m ago] (current)\n::CAPTURES::\n"
        XCTAssertNil(ZellijForkDiscovery.parse(stock))
        XCTAssertNil(ZellijForkDiscovery.parse(""))
    }

    // MARK: - Subtitle

    func testSubtitle() throws {
        XCTAssertNil(ZellijForkDiscovery.subtitle(for: []))
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(
            forkOutput(sessions: sessionsJSON, agents: [("demo", demoAgentsJSON)])))
        let agents = sessions[0].agents
        XCTAssertEqual(ZellijForkDiscovery.subtitle(for: Array(agents.prefix(1))), "claude · develop")
        XCTAssertEqual(ZellijForkDiscovery.subtitle(for: agents), "claude · develop, codex · review")
    }

    func testSubtitleWithoutTabName() throws {
        let agents = #"[{"handle": "sunny-otter", "pane_id": 1, "tab_id": 0, "tab_position": 0, "tab_name": "", "title": "", "kind": "opencode", "source": "command"}]"#
        let sessions = try XCTUnwrap(ZellijForkDiscovery.parse(
            forkOutput(sessions: sessionsJSON, agents: [("demo", agents)])))
        XCTAssertEqual(ZellijForkDiscovery.subtitle(for: sessions[0].agents), "opencode")
    }

    // MARK: - Remote section

    private let stockSection = "echo \"::SESSIONS::\"; zellij list-sessions --no-formatting 2>/dev/null;"
        + " _zv=$(zellij --version 2>/dev/null | head -1); if true; then echo \"::CAPTURES::\";"
        + " for s in $(zellij list-sessions --no-formatting 2>/dev/null | grep -v EXITED | cut -d \" \" -f1); do"
        + " printf \"::CAPTURE:%s::\\n\" \"$s\"; done; fi;"

    func testSectionWrapsStockVerbatimInElseBranch() {
        let section = ZellijForkDiscovery.section(stock: stockSection, skipCaptures: false)
        XCTAssertTrue(section.hasSuffix(" ;; *) \(stockSection) ;; esac;"))
        XCTAssertTrue(section.hasPrefix("_fdz=\"\"; if zellij --version 2>/dev/null | grep -q nkmk;"))
        XCTAssertFalse(section.contains("'"), "the section runs inside a single-quoted sh -lc")
    }

    func testForkBranchReadsJSONAndQuotesSessionNames() throws {
        let section = ZellijForkDiscovery.section(stock: stockSection, skipCaptures: false)
        let forkArm = try XCTUnwrap(section.components(separatedBy: " ;; *) ").first)
        XCTAssertTrue(forkArm.contains("_fdz=$(zellij ls --json 2>/dev/null)"))
        XCTAssertTrue(forkArm.contains("case \"$_fdz\" in \"[\"*)"))
        XCTAssertTrue(forkArm.contains("_fda=$(zellij action list-agents --all-sessions --json 2>/dev/null)"))
        XCTAssertTrue(forkArm.contains("case \"$_fda\" in \"[\"*) echo \"::FDZ_ALL::\";"))
        XCTAssertTrue(forkArm.contains("zellij -s \"$s\" action list-agents --json 2>/dev/null"))
        XCTAssertTrue(forkArm.contains("printf \"::FDZ_AGENT:%s::\\n\" \"$s\""))
        // Previews use the fork's live names, not the table with its header.
        XCTAssertTrue(forkArm.contains("echo \"::CAPTURES::\""))
        XCTAssertTrue(forkArm.contains(ZellijForkDiscovery.forkLiveNames))
        XCTAssertFalse(forkArm.contains(ZellijForkDiscovery.stockCaptureNames))
    }

    func testSkipCapturesDropsPreviewsFromForkBranch() throws {
        let stock = "echo \"::SESSIONS::\"; zellij list-sessions --no-formatting 2>/dev/null;"
        let section = ZellijForkDiscovery.section(stock: stock, skipCaptures: true)
        let forkArm = try XCTUnwrap(section.components(separatedBy: " ;; *) ").first)
        XCTAssertFalse(forkArm.contains("::CAPTURES::"))
        XCTAssertTrue(section.hasSuffix(" ;; *) \(stock) ;; esac;"))
    }

    // MARK: - Created text

    func testCreatedAgoMatchesZellijFormatting() {
        XCTAssertEqual(ZellijForkDiscovery.createdAgo(seconds: 0), "Created 0s ago")
        XCTAssertEqual(ZellijForkDiscovery.createdAgo(seconds: 90), "Created 1m 30s ago")
        XCTAssertEqual(ZellijForkDiscovery.createdAgo(seconds: 9000), "Created 2h 30m ago")
        XCTAssertEqual(ZellijForkDiscovery.createdAgo(seconds: 86_400), "Created 1day ago")
        XCTAssertEqual(ZellijForkDiscovery.createdAgo(seconds: 2 * 86_400 + 5), "Created 2days 5s ago")
        XCTAssertEqual(ZellijForkDiscovery.createdAgo(seconds: 2_630_016), "Created 1month ago")
        XCTAssertEqual(ZellijForkDiscovery.createdAgo(seconds: 31_557_600 * 2), "Created 2years ago")
    }
}
