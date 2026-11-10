# gff parity review: results

The parity review (Part 5 stage 8) ran each row of `gff-client.md` 12 and
checked the plugin table of `gff-client.md` 13. It judges by daily use: can a user do this in
`gff` as they do in the reference zellij workspace (`gff-client.md` 1.6)?

## How it ran

- `gff`, `gff-hab` and `gff-hab-holder` built from `main` with `-tags ghostty`, on macOS.
- Two private habs, each with its own holder, socket and state folder in a temporary build folder,
  `GFF_HAB_DETECT=off`. The second hab ran with a clean environment (`env -i`). A third host
  entry pointed at a socket with no hab.
- `gff` ran in a pty driven by a small script; its output was read back through a terminal
  parser (pyte) and kept as raw bytes for escape checks.
- Agent states came from a stand-in program that prints OSC 7501 reports; usage from
  `gff-hab statusline claude` fed a hand-written status-line input.

Marks: **pass** (the test passes), **fail** (built, but the test fails), **gap** (not built).

## Results

18 pass, 3 fail, 11 gap, of 32 rows.

| # | Behaviour | Mark | Evidence |
|---|---|---|---|
| 1 | Workspace opens on launch | pass | gff saves the workspace to a file and restores its tabs, names, layout, views and focus at the next start (`TestWorkspaceSaved`, `gff-client.md` 5.6) |
| 2 | Startup tabs | pass | `[[workspace.tabs]]` with a name, host, folder and command, made only in a new workspace (`TestStartupTabs`) |
| 3 | Tab bar with session name and clock | pass | Tabs as `1:work 2:2` after a rename, the current one bold, `!` marks, the workspace name beside the clock; tab mode `r` renames, `x` closes (programs keep running), Tab goes to the last tab (`TestTabRenameCloseLast`, `gff-client.md` 5.5) |
| 4 | Agent usage row | pass | `claude · 5h 42% resets 1h 56m · 7d 61% resets 3d 23h · over pace on 7d`, and `claude 1 working`, then `claude 1 needs you` |
| 5 | Mode-aware shortcut bar | pass | `session ·`, `pane ·`, `tab ·` and `locked · Ctrl+G unlock` each list their own keys |
| 6 | Unbound key leaves the bar | pass | `[keys.pane] f = "none"` removes `f zoom` from pane mode's hints |
| 7 | Keys left to programs | pass | A raw reader in the pane got Ctrl+B, Alt+F, Alt+B, Ctrl+S, Alt+arrows, Alt+Shift+Left, Ctrl+N, Ctrl+Q, Ctrl+H and a kitty Ctrl+B byte for byte |
| 8 | Locked mode | pass | After Ctrl+G, Ctrl+P, Ctrl+T, Ctrl+O and Alt+H reached the pane; Ctrl+G unlocked |
| 9 | System info on the bottom bar | pass | `cpu 63% mem 97% disk 70% bat 36% up 2d 9h` at the right end. This machine's figures only: the hab serves no `hab.stats` yet |
| 10 | Stacked panes by default | pass | Pane mode `n` stacks a new pane onto the focused one: one expanded, the other folded to its frame row (`TestNewPaneStacksAndSplits`, `gff-client.md` 5.4) |
| 11 | Splits on direction keys | pass | Pane mode `l`/`r`, `j`/`d`, `h` and `k` put the new pane on that side, in the focused pane's folder (`TestNewPaneStacksAndSplits`, `TestNewPaneInFolder`); Alt+h/j/k/l move focus across the nested layout and wrap in a stack (`TestFocusAcrossNestedLayout`) |
| 12 | Focus moves on direction keys | pass | Alt+H and Alt+L move focus between two side-by-side views, typing follows. Alt+Shift+arrows cannot be bound (8.4) |
| 13 | Zoom and close | pass | Ctrl+P `f` zooms; Ctrl+P `x` closes the view and the session stays in the sidebar; the sidebar's `x` asks "end zsh-2 on alpha? the program gets a hang-up (y/n)" |
| 14 | Reopen a closed pane | gap | No reopen action in the keymap |
| 15 | Scroll without a mode | pass | The wheel over a view scrolls its local scrollback and sends nothing to the hab; Shift+PageUp/PageDown/End by keys; a mouse program gets the wheel, an alternate screen gets arrows (`TestWheelScrollsLocally`, `gff-client.md` 5.7) |
| 16 | Typing while scrolled up | pass | Typed keys reach the program and the scrolled view stays on its rows (`TestWheelScrollsLocally`) |
| 17 | Thin pane frames with agent names | pass | `zsh · alpha · working`, `zsh · alpha · blocked:permission`. The frame names the session and host; the harness shows on the board only |
| 18 | Exit status on the frame | pass | `… · alpha · exited 126`, `sh · alpha · exited 0` |
| 19 | Bell marker | pass | `bell` on the frame about 1.3 s after the bell, gone by 2.8 s |
| 20 | Finished command drops to a shell | gap | `exited 0` on the frame; no key starts a shell |
| 21 | Large floating pane | pass (15.9 B) | A zoomed pane in a scratch tab works: Ctrl+T `n`, `n`, Enter, Enter, Ctrl+P `f`. The sidebar stays beside the zoom, the tab cannot be closed, and the scratch program keeps running after its view closes. Open: B or A (15.9) |
| 22 | Window title names the host | fail | gff writes no OSC 0 or 2 to the host; a pane's title is ignored |
| 23 | Several windows mirror one workspace | gap | Decided 15.4 B; not built: each gff holds its own tabs |
| 24 | Agents on every host | pass | The sidebar lists both habs' sessions; the new-session prompt cycles hosts with Tab |
| 25 | Agent state per pane | pass | Working `~`, blocked `!`, exited `_` in the sidebar; the board puts the blocked session under Needs you; the tab gets `!` and the count `1 need you` |
| 26 | Restore after a reboot | pass | After the holder is killed and restarted, views show `exited · Enter resume` and nothing runs; Enter starts the harness with its resume id in the same view (`TestResumeAfterHolderLoss`, `gff-client.md` 6.4). The view comes back after a gff restart with the saved workspace (5.6) |
| 27 | Clean environment for new panes | pass | gff started with `CLAUDECODE=1` and `AGENT_KIND=claude`; a new pane on the clean hab has none of them. A hab started from an agent's shell passes that shell's agent variables on: panes take the hab's environment, and the installed service starts clean |
| 28 | Agents drive the workspace | gap | `session.create`, `pane.input`, `session.send` and `gff-hab wait` work; no method reads a pane's screen or waits for output, and gff has no CLI verbs |
| 29 | Kitty keyboard and OSC 8 links | fail | An OSC 8 link's text draws but no OSC 8 reaches the host; styled underlines and true colour do. The composed view never asks for the kitty protocol |
| 30 | Theme | pass | Chrome colours are host palette indexes (`palette()` in `workspace.go`, `bars.go`), so gff follows the terminal's theme. No theme file yet |
| 31 | Status notices | pass | A host with no hab raises `1 warning`; Ctrl+O `!` lists `gone connecting`. A hab older than gff or another hab, and panes on a drained holder, are listed too |
| 32 | Pane size follows the typist | pass | Two gff clients, 120x30 and 80x20, on one pane: the second typed and the first shows `owner … 80x20` with the dim fill |

## Plugins and add-ons (13)

| Item | Recommendation | Result |
|---|---|---|
| Top tab bar | Carry over | Built, without the workspace name and tab names (row 3) |
| Agent usage bar | Carry over, first-class | Built (row 4) |
| Bottom shortcut bar | Carry over | Built (rows 5, 6, 9) |
| Session event feed | Drop | Nothing to do |
| Agent session hook script | Replace with adapters | Built: OSC 7501 reports drive the frame, sidebar and board (row 25) |
| Workspace MCP server and CLI verbs | Replace with the hab API and gff verbs | Partly: no screen read, no wait for output, no gff verbs (row 28) |
| `session-manager`, `welcome-screen` | Replace with the sidebar and board | Built, but exited sessions cannot be resumed (row 26) |
| Stock bars, `strider`, `filepicker`, `plugin-manager`, web client settings, older bar builds | Drop | Nothing to do |
| `configuration` | Drop; the config file is the source | Bad lines are named at start (8.4 tests) |
| `about` | `gff --version` and a help overlay | `gff version` prints `gff dev`; no help overlay |

## Gaps, ranked

Blocks daily use:

| Outcome | Rows |
|---|---|
| Save and restore the workspace, with startup tabs | 1, 2 |
| New panes from pane mode, stacked by default, splits in four directions | 10, 11 |
| Scrollback in the composed view, by wheel, with typing while scrolled | 15, 16 |
| Resume an exited session from the sidebar and board | 26 |
| Tab rename, close, go to last, and the workspace name | 3 |
| Agents read pane screens and wait for output through the hab | 28 |
| Several windows share one workspace, focus per window | 23 |

Nice to have:

| Outcome | Rows |
|---|---|
| A console command that exits cleanly offers a shell | 20 |
| Reopen the last closed views | 14 |
| Host window title from a template | 22 |
| OSC 8 links and the kitty keyboard protocol in the composed view | 29 |
| Help overlay that lists the keymap | 13 |

## Other findings

- Removing an exited session's record from the sidebar leaves an `unfiled <id>` row for its
  exited pane.
- The board's context column stays empty for an OSC 7501 report that carries a title and a
  message.
- zellij's resize and move modes have no gff mode (8.4 limits); not a checklist row.

## For the owner

- Retiring the current workspace waits on the seven gaps that block daily use.
- Floating panes (15.9): option B works today but is five keys and leaves a tab and a program
  behind. Keep B, or build A, the floating layer?
