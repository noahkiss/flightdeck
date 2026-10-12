//
//  ZellijForkModels.swift
//  FlightDeck
//
//  Plain value types for what zellij-nkmk reports about its sessions and the
//  coding agents in them. No UI and no shell framing live here, so any view
//  can consume these unchanged.
//
//  The Decodable mirrors follow zellij-nkmk's JSON contract:
//  `SessionListEntry` (`zellij ls --json`) and `AgentListEntry`
//  (`zellij action list-agents --json`). Keys the app does not read are
//  ignored; keys zellij omits are optional here.
//
//  rootshellTests compiles this file directly (see FLIGHTDECK.md, Hooks), so
//  it must not reference app types.
//

import Foundation

/// One element of `zellij ls --json`.
nonisolated struct ZellijForkSessionEntry: Decodable, Equatable, Sendable {
    let name: String
    let createdSecondsAgo: UInt64
    let isCurrent: Bool
    let isDead: Bool
    /// Omitted by zellij for a dead session, and for a live one whose metadata does not parse.
    let connectedClients: Int?
    let tabCount: Int?
    let paneCount: Int?

    nonisolated enum CodingKeys: String, CodingKey {
        case name
        case createdSecondsAgo = "created_seconds_ago"
        case isCurrent = "is_current"
        case isDead = "is_dead"
        case connectedClients = "connected_clients"
        case tabCount = "tab_count"
        case paneCount = "pane_count"
    }
}

/// One element of `zellij action list-agents --json`: a pane running a coding agent.
nonisolated struct ZellijForkAgent: Decodable, Equatable, Sendable {
    /// The pane's two-word handle, its stable address.
    let handle: String
    let paneId: Int
    let tabId: Int
    let tabPosition: Int
    let tabName: String
    let title: String
    /// The harness, e.g. `claude`, `codex`, `opencode`, `pi`.
    let kind: String
    let agentId: String?
    /// What the detection rests on, e.g. `command` or `command+env`.
    let source: String?
    /// The session, present only in `list-agents --all-sessions` rows.
    let session: String?

    // Program status (OSC 7501) the agent reported, from zellij-nkmk with
    // the `program-status-osc` capability. Each key is absent when nothing
    // was reported.

    /// `idle`, `working`, `done`, `blocked` or `error`.
    let state: String?
    /// With `blocked`: `permission`, `question` or `auth`.
    let stateKind: String?
    /// The program's own name, e.g. `claude-code`.
    let stateApp: String?
    let stateTitle: String?
    let stateMsg: String?
    /// 0-100, with `working` or `blocked`.
    let stateProgress: Int?
    /// When the state was reported, host epoch milliseconds.
    let stateAt: Int64?

    nonisolated enum CodingKeys: String, CodingKey {
        case handle
        case paneId = "pane_id"
        case tabId = "tab_id"
        case tabPosition = "tab_position"
        case tabName = "tab_name"
        case title
        case kind
        case agentId = "agent_id"
        case source
        case session
        case state
        case stateKind = "state_kind"
        case stateApp = "state_app"
        case stateTitle = "state_title"
        case stateMsg = "state_msg"
        case stateProgress = "state_progress"
        case stateAt = "state_at"
    }
}

/// One session as FlightDeck sees it: zellij's entry, its agents, and its preview.
nonisolated struct ZellijForkSession: Equatable, Sendable {
    let entry: ZellijForkSessionEntry
    /// Ordered by tab position, then pane id.
    let agents: [ZellijForkAgent]
    var capturedContent: String?

    var name: String { entry.name }
    var isAttached: Bool { entry.isCurrent || (entry.connectedClients ?? 0) > 0 }
    var isExited: Bool { entry.isDead }

    var extras: FlightdeckZellijExtras {
        FlightdeckZellijExtras(
            agents: agents,
            connectedClients: entry.connectedClients,
            tabCount: entry.tabCount,
            paneCount: entry.paneCount
        )
    }
}

/// What FlightDeck adds to upstream's `ZellijSessionInfo`.
nonisolated struct FlightdeckZellijExtras: Equatable, Sendable {
    var agents: [ZellijForkAgent] = []
    var connectedClients: Int?
    var tabCount: Int?
    var paneCount: Int?
}
