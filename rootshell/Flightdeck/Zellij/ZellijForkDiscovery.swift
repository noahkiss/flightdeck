//
//  ZellijForkDiscovery.swift
//  FlightDeck
//
//  Session discovery for hosts that run zellij-nkmk. The fork's
//  `list-sessions` prints a table with a NAME header and a lowercase
//  `exited` status, which upstream's line parser reads as a session called
//  NAME. On the fork this reads `zellij ls --json` instead, plus the agents:
//  one `list-agents --all-sessions --json` where the fork has that flag, else
//  one `list-agents --json` per live session.
//
//  The remote section wraps upstream's zellij section verbatim, so a stock
//  host runs exactly what upstream runs. The fork's output starts with
//  `::FDZ1::`; `parse` returns nil without it, and upstream's parser runs.
//
//  rootshellTests compiles this file directly (see FLIGHTDECK.md, Hooks), so
//  it must not reference app types.
//

import Foundation

nonisolated enum ZellijForkDiscovery {
    static let frameMarker = "::FDZ1::"
    static let sessionsMarker = "::FDZ_SESSIONS::"
    static let agentsMarker = "::FDZ_AGENTS::"
    static let agentPrefix = "::FDZ_AGENT:"
    /// Precedes one `list-agents --all-sessions --json` array, whose rows
    /// name their session. Hosts without that flag answer nothing on stdout,
    /// and the section falls back to one `list-agents` per live session.
    static let allAgentsMarker = "::FDZ_ALL::"
    static let capturesMarker = "::CAPTURES::"
    static let capturePrefix = "::CAPTURE:"

    /// Live session names on the fork: `ls -s` marks dead sessions `(EXITED)`.
    static let forkLiveNames =
        "$(zellij ls -s 2>/dev/null | grep -v \"(EXITED\" | cut -d \" \" -f1)"

    /// The session-name source of upstream's preview loop. On the fork it
    /// yields the table header and dead sessions, so the fork branch swaps in
    /// `forkLiveNames`. scripts/flightdeck-hooks.json lists this text and
    /// `stockCaptureStart` under `upstreamText`, so CI fails if upstream
    /// rewrites them.
    static let stockCaptureNames =
        "$(zellij list-sessions --no-formatting 2>/dev/null | grep -v EXITED | cut -d \" \" -f1)"

    /// Where upstream's preview block starts inside its zellij section.
    static let stockCaptureStart = " _zv=$(zellij --version"

    // MARK: - Remote section

    /// Wraps upstream's zellij section: the fork branch when the host runs
    /// zellij-nkmk and `ls --json` answers with an array, else `stock`
    /// unchanged. The text goes inside upstream's single-quoted `sh -lc`, so
    /// it holds no single quotes. Every command sends stderr to /dev/null:
    /// the fork warns there when a session runs a different build.
    static func section(stock: String, skipCaptures: Bool) -> String {
        var captures = ""
        if !skipCaptures, let start = stock.range(of: stockCaptureStart) {
            captures = String(stock[start.lowerBound...])
                .replacingOccurrences(of: stockCaptureNames, with: forkLiveNames)
        }
        return "_fdz=\"\"; if zellij --version 2>/dev/null | grep -q nkmk; then _fdz=$(zellij ls --json 2>/dev/null); fi;"
            + " case \"$_fdz\" in \"[\"*)"
            + " echo \"\(frameMarker)\"; echo \"\(sessionsMarker)\"; printf \"%s\\n\" \"$_fdz\";"
            + " echo \"\(agentsMarker)\";"
            + " _fda=$(zellij action list-agents --all-sessions --json 2>/dev/null);"
            + " case \"$_fda\" in \"[\"*) echo \"\(allAgentsMarker)\"; printf \"%s\\n\" \"$_fda\";;"
            + " *) for s in \(forkLiveNames); do"
            + " printf \"\(agentPrefix)%s::\\n\" \"$s\";"
            + " zellij -s \"$s\" action list-agents --json 2>/dev/null; echo;"
            + " done;; esac;"
            + captures
            + " ;; *) \(stock) ;; esac;"
    }

    // MARK: - Parser

    /// Parses the fork branch's output. Returns nil when the output does not
    /// carry the fork frame or its session list does not decode, so the
    /// caller falls back to upstream's parser.
    static func parse(_ output: String) -> [ZellijForkSession]? {
        guard let frame = output.range(of: frameMarker) else { return nil }
        var body = Substring(output[frame.upperBound...])

        var capturesBlock: Substring = ""
        if let captures = body.range(of: capturesMarker) {
            capturesBlock = body[captures.upperBound...]
            body = body[..<captures.lowerBound]
        }

        guard let sessionsStart = body.range(of: sessionsMarker) else { return nil }
        let afterSessions = body[sessionsStart.upperBound...]
        let agentsStart = afterSessions.range(of: agentsMarker)
        let sessionsJSON = agentsStart.map { afterSessions[..<$0.lowerBound] } ?? afterSessions
        guard let entries: [ZellijForkSessionEntry] = decodeArray(sessionsJSON) else { return nil }

        var agentsBySession: [String: [ZellijForkAgent]] = [:]
        if let agentsStart {
            let agentsBlock = afterSessions[agentsStart.upperBound...]
            if let all = agentsBlock.range(of: allAgentsMarker) {
                let agents: [ZellijForkAgent] = decodeArray(agentsBlock[all.upperBound...]) ?? []
                for agent in agents {
                    guard let session = agent.session else { continue }
                    agentsBySession[session, default: []].append(agent)
                }
            } else {
                for (name, block) in chunks(agentsBlock, prefix: agentPrefix) {
                    agentsBySession[name] = decodeArray(Substring(block)) ?? []
                }
            }
            agentsBySession = agentsBySession.mapValues { agents in
                agents.sorted { ($0.tabPosition, $0.paneId) < ($1.tabPosition, $1.paneId) }
            }
        }

        var captures: [String: String] = [:]
        for (name, block) in chunks(capturesBlock, prefix: capturePrefix) {
            let trimmed = block.replacingOccurrences(of: "\\s+$", with: "", options: .regularExpression)
            if !trimmed.isEmpty {
                captures[name] = trimmed
            }
        }

        return entries.map { entry in
            ZellijForkSession(
                entry: entry,
                agents: entry.isDead ? [] : agentsBySession[entry.name] ?? [],
                capturedContent: captures[entry.name]
            )
        }
    }

    /// Splits `<prefix>name::\n<content>` records.
    private static func chunks(_ block: Substring, prefix: String) -> [(String, String)] {
        var result: [(String, String)] = []
        for chunk in block.components(separatedBy: prefix).dropFirst() {
            guard let end = chunk.range(of: "::\n") ?? chunk.range(of: "::\r\n") else { continue }
            let name = String(chunk[..<end.lowerBound])
            result.append((name, String(chunk[end.upperBound...])))
        }
        return result
    }

    /// Decodes the JSON array between the first `[` and the last `]`, so
    /// noise lines around it do not matter. One malformed element is dropped,
    /// not the whole array.
    private static func decodeArray<T: Decodable>(_ text: Substring) -> [T]? {
        guard let open = text.firstIndex(of: "["),
              let close = text.lastIndex(of: "]"),
              open < close,
              let data = String(text[open...close]).data(using: .utf8),
              let elements = try? JSONDecoder().decode([Lossy<T>].self, from: data)
        else { return nil }
        return elements.compactMap(\.value)
    }

    private nonisolated struct Lossy<T: Decodable>: Decodable {
        let value: T?
        init(from decoder: Decoder) throws {
            value = try? T(from: decoder)
        }
    }

    // MARK: - Presentation

    /// The picker subtitle, e.g. `claude · develop, codex · review`
    /// (agent kind · tab name), in tab order. Nil when there are no agents.
    static func subtitle(for agents: [ZellijForkAgent]) -> String? {
        let parts = agents.map { agent in
            agent.tabName.isEmpty ? agent.kind : "\(agent.kind) · \(agent.tabName)"
        }
        return parts.isEmpty ? nil : parts.joined(separator: ", ")
    }

    /// The `Created … ago` text stock zellij prints in `list-sessions`, from
    /// the same number. zellij formats it with humantime's
    /// `format_duration`: whole seconds, zero units left out, a year of
    /// 365.25 days and a month of 30.44 days.
    static func createdAgo(seconds: UInt64) -> String {
        "Created \(humanDuration(seconds)) ago"
    }

    static func humanDuration(_ total: UInt64) -> String {
        if total == 0 { return "0s" }
        var rest = total
        let years = rest / 31_557_600
        rest %= 31_557_600
        let months = rest / 2_630_016
        rest %= 2_630_016
        let days = rest / 86_400
        rest %= 86_400
        let hours = rest / 3_600
        rest %= 3_600
        let minutes = rest / 60
        let seconds = rest % 60

        var parts: [String] = []
        func plural(_ value: UInt64, _ unit: String) {
            guard value > 0 else { return }
            parts.append(value == 1 ? "\(value)\(unit)" : "\(value)\(unit)s")
        }
        plural(years, "year")
        plural(months, "month")
        plural(days, "day")
        if hours > 0 { parts.append("\(hours)h") }
        if minutes > 0 { parts.append("\(minutes)m") }
        if seconds > 0 { parts.append("\(seconds)s") }
        return parts.joined(separator: " ")
    }
}
