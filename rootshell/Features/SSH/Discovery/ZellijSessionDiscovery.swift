//
//  ZellijSessionDiscovery.swift
//  rootshell
//
//  Data models, parser, and discovery method for zellij session detection on SSH hosts.
//

import Foundation
import Citadel
import NIO
import NIOFoundationCompat
import os

// MARK: - Data Models

struct ZellijSessionInfo: Identifiable, Equatable, Sendable {
    let name: String
    /// Whether this is the currently attached session (has "(current)" suffix)
    let isAttached: Bool
    /// Whether this session has exited and can be resurrected
    let isExited: Bool
    /// Human-readable creation time, e.g. "Created 2h 30m ago"
    let createdAgo: String
    /// ANSI-captured terminal content for preview rendering
    var capturedContent: String?
    var flightdeck: FlightdeckZellijExtras? = nil  // FLIGHTDECK-HOOK: zellij.info.extras

    var id: String { name }
}

// MARK: - Errors

enum ZellijDiscoveryError: LocalizedError {
    case notConnected
    case timeout
    case zellijNotInstalled
    case zellijVersionTooOld

    var errorDescription: String? {
        switch self {
        case .notConnected: return "SSH client not connected"
        case .timeout: return "Zellij discovery timed out"
        case .zellijNotInstalled: return "zellij is not installed on the remote host"
        case .zellijVersionTooOld: return "zellij 0.44.0 or later is required"
        }
    }
}

// MARK: - Discovery Command

enum ZellijDiscoveryCommand {
    /// Per-session preview loop, zellij >= 0.44 only. `dump-screen` without
    /// `--pane-id` resolves the focused pane through a connected client, so it
    /// is empty for detached sessions; fall back to the focused (else first)
    /// terminal pane from `list-panes --json`. The awk splits the pretty
    /// printed JSON on `}`; every key it needs precedes the nested
    /// `index_in_pane_group: {}`. EXITED sessions cannot be dumped.
    static let captureLoop: String =
        "for s in $(zellij list-sessions --no-formatting 2>/dev/null | grep -v EXITED | cut -d \" \" -f1); do"
        + " printf \"::CAPTURE:%s::\\n\" \"$s\";"
        + " _zd=$(zellij -s \"$s\" action dump-screen --ansi 2>/dev/null);"
        + " if [ -z \"$_zd\" ]; then"
        + " _zp=$(zellij -s \"$s\" action list-panes --json 2>/dev/null | awk \"BEGIN{RS=\\\"}\\\"}"
        + " /\\\"is_plugin\\\": *false/ && /\\\"is_selectable\\\": *true/ && !/\\\"is_suppressed\\\": *true/ && match(\\$0,/\\\"id\\\": *[0-9]+/){"
        + " s=substr(\\$0,RSTART,RLENGTH); sub(/.*: */,\\\"\\\",s); if(/\\\"is_focused\\\": *true/){f=s; exit} if(f==\\\"\\\"){f=s} }"
        + " END{if(f!=\\\"\\\")print f}\");"
        + " [ -n \"$_zp\" ] && _zd=$(zellij -s \"$s\" action dump-screen --pane-id \"terminal_$_zp\" --ansi 2>/dev/null);"
        + " fi;"
        + " printf \"%s\\n\" \"$_zd\";"
        + " done;"

    /// Uses login shell (-l) so PATH and env vars are set correctly.
    /// Session listing works on any version; previews require >= 0.44.0.
    static let command: String = {
        return "sh -lc '\(SSHConfig.remoteExecPathPrefix)command -v zellij >/dev/null 2>&1 || exit 1; echo \"::SESSIONS::\"; zellij list-sessions --no-formatting 2>/dev/null; _zv=$(zellij --version 2>/dev/null | grep -oE \"[0-9]+\\.[0-9]+\" | head -1); _zmaj=${_zv%%.*}; _zmin=${_zv#*.}; if [ \"${_zmaj:-0}\" -gt 0 ] 2>/dev/null || [ \"${_zmin:-0}\" -ge 44 ] 2>/dev/null; then echo \"::CAPTURES::\"; \(captureLoop) fi'"
    }()
}

// MARK: - Parser

enum ZellijDiscoveryParser {

    private nonisolated static let logger = Logger(
        subsystem: "com.rootshell",
        category: "ZellijDiscoveryParser"
    )

    static func parse(output: String) -> [ZellijSessionInfo] {
        let parts = output.components(separatedBy: "::SESSIONS::")
        guard parts.count >= 2 else {
            return []
        }

        let afterSessions = parts[1]

        // Split off captures section (optional)
        let capturesSplit = afterSessions.components(separatedBy: "::CAPTURES::")
        let sessionsBlock = capturesSplit[0]
        let capturesBlock = capturesSplit.count >= 2 ? capturesSplit[1] : ""

        // Parse captures, keyed by session name
        var capturesBySession: [String: String] = [:]
        if !capturesBlock.isEmpty {
            let capturePattern = "::CAPTURE:"
            let captureChunks = capturesBlock.components(separatedBy: capturePattern)
            for chunk in captureChunks {
                guard let markerEnd = chunk.range(of: "::\n") ?? chunk.range(of: "::\r\n") else { continue }
                let sessionName = String(chunk[chunk.startIndex..<markerEnd.lowerBound])
                let content = String(chunk[markerEnd.upperBound...])
                let trimmed = content.replacingOccurrences(of: "\\s+$", with: "", options: .regularExpression)
                if !trimmed.isEmpty {
                    capturesBySession[sessionName] = trimmed
                }
            }
        }

        // Parse session lines
        // Format: "session-name [Created 2h 30m ago]"
        // Optional suffixes: "(current)", "(EXITED - attach to resurrect)"
        var sessions: [ZellijSessionInfo] = []
        for line in sessionsBlock.split(separator: "\n", omittingEmptySubsequences: true) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            guard !trimmed.isEmpty else { continue }

            let isAttached = trimmed.contains("(current)")
            let isExited = trimmed.contains("(EXITED")

            // Extract session name: everything before the first " ["
            let name: String
            if let bracketRange = trimmed.range(of: " [") {
                name = String(trimmed[trimmed.startIndex..<bracketRange.lowerBound])
            } else {
                // Fallback: entire line is the name (shouldn't normally happen)
                name = trimmed
            }

            // Extract "Created X ago" from brackets
            let createdAgo: String
            if let openBracket = trimmed.range(of: "["),
               let closeBracket = trimmed.range(of: "]") {
                createdAgo = String(trimmed[openBracket.upperBound..<closeBracket.lowerBound])
                    .trimmingCharacters(in: .whitespaces)
            } else {
                createdAgo = ""
            }

            let session = ZellijSessionInfo(
                name: name,
                isAttached: isAttached,
                isExited: isExited,
                createdAgo: createdAgo,
                capturedContent: capturesBySession[name]
            )
            sessions.append(session)
        }

        let count = sessions.count
        logger.info("Parsed \(count) zellij sessions")
        return sessions
    }
}

// MARK: - Discovery Runner

@MainActor
enum ZellijDiscoveryRunner {

    private nonisolated static let logger = Logger(
        subsystem: "com.rootshell",
        category: "ZellijDiscovery"
    )

    /// Execute the discovery command on the given SSH client with a 5-second timeout.
    static func execute(on client: SSHClient) async throws -> [ZellijSessionInfo] {
        let output: String = try await withThrowingTaskGroup(of: String.self) { group in
            group.addTask { @Sendable in
                let buf = try await client.executeCommand(ZellijDiscoveryCommand.command)
                let data = Data(buffer: buf)
                return String(data: data, encoding: .utf8) ?? ""
            }
            group.addTask { @Sendable in
                try await Task.sleep(for: .seconds(5))
                throw ZellijDiscoveryError.timeout
            }
            let result = try await group.next()!
            group.cancelAll()
            return result
        }

        if output.isEmpty {
            logger.info("Empty output from zellij discovery (zellij not installed or no sessions)")
            throw ZellijDiscoveryError.zellijNotInstalled
        }

        return ZellijDiscoveryParser.parse(output: output)
    }
}
