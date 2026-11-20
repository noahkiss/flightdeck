import Foundation

/// Invented terminal output for demo mode, so a pane opened from the demo
/// board shows a live terminal. Each pane echoes what is typed, as a shell in
/// cooked mode does; the dev server pane prints a request log on a timer.
/// Every name and path here is invented.
enum DemoTerminal {
    static let esc = "\u{1B}"

    /// Gives every demo pane a byte stream, and returns the task that keeps
    /// the dev server's log going.
    static func populate(workbench: FakeHab, attic: FakeHab) async -> Task<Void, Never> {
        for p in ["%1", "%2", "%3", "%4", "%6"] {
            await workbench.terminals.add(p, cols: 100, rows: 30, echo: true)
            await workbench.terminals.write(p, Data(shell(project: p == "%2" ? "seed-catalog" : p == "%4" ? "paper-lantern" : "tide-tables").utf8))
        }
        // The blocked Claude session paints its permission prompt at any size.
        await workbench.terminals.setRepaint("%1") { c, r in Data(agentScreen(cols: c, rows: r).utf8) }
        for p in ["%11", "%12", "%13"] {
            await attic.terminals.add(p, cols: 100, rows: 30, echo: true)
            await attic.terminals.write(p, Data(shell(project: "paper-lantern").utf8))
        }
        await workbench.terminals.add("%5", cols: 100, rows: 30, echo: true)
        await workbench.terminals.write("%5", Data(devServerStart.utf8))
        return Task {
            var n = 0
            let paths = ["/", "/api/lanterns", "/api/lanterns/7", "/assets/app.js", "/api/palette", "/healthz"]
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(2))
                n += 1
                let path = paths[n % paths.count]
                let ms = 3 + (n * 7) % 40
                let status = n % 11 == 0 ? "\(esc)[33m304\(esc)[0m" : "\(esc)[32m200\(esc)[0m"
                let line = "\(esc)[2m\(clock(n))\(esc)[0m GET \(path) \(status) \(ms) ms\r\n"
                await workbench.terminals.write("%5", Data(line.utf8))
            }
        }
    }

    static func clock(_ n: Int) -> String {
        let s = 9 * 3600 + 41 * 60 + 2 * n
        return String(format: "%02d:%02d:%02d", s / 3600 % 24, s / 60 % 60, s % 60)
    }

    static func shell(project: String) -> String {
        let p = "\(esc)[1;34m~/src/\(project)\(esc)[0m \(esc)[35mmain\(esc)[0m $ "
        return [
            p + "git log --oneline -4\r\n",
            "\(esc)[33m3f1c2ab\(esc)[0m Draw the tide chart axis\r\n",
            "\(esc)[33m98d04e1\(esc)[0m Read heights from the station file\r\n",
            "\(esc)[33m51aa7c9\(esc)[0m Add the station list\r\n",
            "\(esc)[33m0c2e113\(esc)[0m First commit\r\n",
            p + "make test\r\n",
            "ok   chart      0.41s\r\n",
            "ok   stations   0.12s\r\n",
            "\(esc)[32mPASS\(esc)[0m 2 packages\r\n",
            p,
        ].joined()
    }

    /// An agent's screen as a coding agent paints it, waiting on a permission
    /// prompt, laid out for `cols` by `rows`. Invented text.
    static func agentScreen(cols: Int, rows: Int) -> String {
        let w = max(cols, 20)
        let dim = "\(esc)[2m", bold = "\(esc)[1m", off = "\(esc)[0m"
        let dot = "\(esc)[32m●\(off)"
        let amber = "\(esc)[33m"
        var lines: [String] = []
        func say(_ text: String, first: String, rest: String, firstWidth: Int, style: String = "") {
            let body = wrap(text, width: w - firstWidth)
            for (i, l) in body.enumerated() {
                lines.append((i == 0 ? first : rest) + style + l + (style.isEmpty ? "" : off))
            }
        }
        say("Read(src/db/schema.ts)", first: "\(dot) ", rest: "  ", firstWidth: 2, style: bold)
        say("Read 48 lines", first: "  \(dim)⎿\(off)  ", rest: "     ", firstWidth: 5, style: dim)
        lines.append("")
        say("Readings live in one table, keyed by station and time. A query by station needs an index.",
            first: "\(dot) ", rest: "  ", firstWidth: 2)
        lines.append("")
        say("Edit(src/api/stations.ts)", first: "\(dot) ", rest: "  ", firstWidth: 2, style: bold)
        say("Added 3 lines, removed 1 line", first: "  \(dim)⎿\(off)  ", rest: "     ", firstWidth: 5, style: dim)
        lines.append("")
        say("The chart now draws on the station page. One new index makes the query fast.",
            first: "\(dot) ", rest: "  ", firstWidth: 2)
        lines.append("")
        // The permission box, as wide as the screen.
        let inner = w - 4
        func boxed(_ text: String, style: String = "") -> [String] {
            wrap(text, width: inner).map { l in
                let pad = String(repeating: " ", count: max(0, inner - l.count))
                return "\(amber)│\(off) " + style + l + (style.isEmpty ? "" : off) + pad + " \(amber)│\(off)"
            }
        }
        lines.append(amber + "╭" + String(repeating: "─", count: w - 2) + "╮" + off)
        lines += boxed("Bash command", style: bold)
        lines += boxed("")
        lines += boxed("  npm run migrate")
        lines += boxed("  Apply the new index", style: dim)
        lines += boxed("")
        lines += boxed("Do you want to proceed?")
        lines += boxed("❯ 1. Yes", style: "\(esc)[36m")
        lines += boxed("  2. Yes, and don't ask again for npm run commands in this folder")
        lines += boxed("  3. No, and tell Claude what to do differently")
        lines.append(amber + "╰" + String(repeating: "─", count: w - 2) + "╯" + off)
        lines.append(dim + "  Esc to cancel" + off)
        let shown = lines.suffix(max(rows - 1, 1))
        return shown.joined(separator: "\r\n")
    }

    /// Words of `text` in lines of at most `width` characters.
    static func wrap(_ text: String, width: Int) -> [String] {
        guard !text.isEmpty else { return [""] }
        let lead = String(text.prefix { $0 == " " })
        var out: [String] = []
        var line = lead
        for word in text.split(separator: " ") {
            let candidate = line.trimmingCharacters(in: .whitespaces).isEmpty ? line + word : line + " " + word
            if candidate.count > width, !line.trimmingCharacters(in: .whitespaces).isEmpty {
                out.append(line)
                line = lead + word
            } else {
                line = candidate
            }
        }
        out.append(line)
        return out.map { String($0.prefix(max(width, 1))) }
    }

    static let devServerStart = [
        "\(esc)[1;34m~/src/paper-lantern\(esc)[0m \(esc)[35mmain\(esc)[0m $ npm run dev\r\n",
        "\r\n",
        "  \(esc)[1;32mready\(esc)[0m in 412 ms\r\n",
        "  Local:   http://localhost:5173/\r\n",
        "\r\n",
    ].joined()
}
