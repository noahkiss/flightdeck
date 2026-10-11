# flightdeck patch ledger

Base: upstream `main` at `a1d6aa609` (`1.0.14-165-33-ga1d6aa6`).

Each patch is one commit on top of the base. Entries are in series order.

| # | Commit subject | What it changes | Why |
|---|---|---|---|
| 1 | `docs: add flightdeck fork notes` | Adds this ledger, `NOTICE`, `CLAUDE.md`, a flightdeck section at the top of `AGENTS.md` and a note at the top of `README.md`. | Credits rootshell and records the fork rules. |
| 2 | `feat(identity): sign under the flightdeck team and namespace` | Adds the tracked `Configuration/Flightdeck.xcconfig`, which sets the team, the org identifier, the app group and the iCloud container. Each value is one line there and appears nowhere else. `Identity.xcconfig` includes it with one `#include?` line, just before the `DeveloperSettings.xcconfig` include. | Upstream's identifiers belong to upstream's team. Upstream's own override variables carry the change, so no `project.pbxproj`, `.entitlements` or `Info.plist` literal moves. A local `DeveloperSettings.xcconfig` still wins. |
