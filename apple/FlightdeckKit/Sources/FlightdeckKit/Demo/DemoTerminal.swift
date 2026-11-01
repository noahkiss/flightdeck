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

    static let devServerStart = [
        "\(esc)[1;34m~/src/paper-lantern\(esc)[0m \(esc)[35mmain\(esc)[0m $ npm run dev\r\n",
        "\r\n",
        "  \(esc)[1;32mready\(esc)[0m in 412 ms\r\n",
        "  Local:   http://localhost:5173/\r\n",
        "\r\n",
    ].joined()
}
