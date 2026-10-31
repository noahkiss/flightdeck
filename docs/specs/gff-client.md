# gff terminal client: behaviour spec

Status: draft for review. Part 5, study stage.

This spec says how `gff`, the Go terminal client, behaves. Later Part 5 stages build from it: the
shared connection library, the client core, the sidebar and tabs, the status board, the shortcut
bar and system info, and the parity review. It describes behaviour in our own words. It copies no
code from the reference projects.

`gff` talks to habs only. What a hab serves is in `hab-logic-api.md`. How a client reaches a hab is
in `transport.md`. What the agent is doing comes from `harness-adapters.md`. The app's matching
behaviour is in `app.md`; where both clients show the same thing, this spec follows the app.

Owner rules this spec works within:

- SSH first. A tailnet is only a network underneath, never a product transport. No mosh.
- No hub. `gff` connects to each hab itself.
- The hab owns structure. `gff` owns its own presentation: tabs, splits, focus, theme.
- `gff` replaces the owner's current terminal workspace. It does not run beside it, read its files
  or depend on any of the owner's tools. That workspace retires once `gff` passes the parity
  checklist (12).

Each section ends with edge cases and its sources. Facts marked *(gk)* come from general knowledge
of the named project, not from a page opened for this spec. A stage that builds on them confirms
them first.

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [Screen](#3-screen)
4. [Sidebar](#4-sidebar)
5. [Tabs and panes](#5-tabs-and-panes)
6. [Sessions across hosts](#6-sessions-across-hosts)
7. [Status board](#7-status-board)
8. [Shortcut bar](#8-shortcut-bar)
9. [System info](#9-system-info)
10. [Pane size](#10-pane-size)
11. [Connection](#11-connection)
12. [Parity checklist](#12-parity-checklist)
13. [Plugins and add-ons to confirm with the owner](#13-plugins-and-add-ons-to-confirm-with-the-owner)
14. [Calls gff needs from the hab](#14-calls-gff-needs-from-the-hab)
15. [Open questions](#15-open-questions)
16. [Sources](#16-sources)

---

## 1. References

### 1.1 zellij

- A terminal workspace in Rust. One server per session owns the PTYs and renders one composed
  screen per client. Clients attach to the server.
- **Modes.** Normal mode passes keys to the pane. A leader key enters a mode (pane, tab, resize,
  move, session, scroll); the next key acts. Locked mode passes every key through except the unlock
  key.
- **Bars are plugins.** The default layout has a tab bar on top and a status bar at the bottom.
  The status bar shows the keys of the current mode. A layout can add more single-row plugin panes.
- **Stacks.** Panes can stack: one expanded, the others folded to a title row.
- **Size.** The smallest attached client sets the size of every pane in a tab.
- **Resurrection.** The server saves the layout on a timer and can rebuild it after a reboot.
- **Remote.** Each host runs its own server. Reaching another host means SSH and a second attach.

What we take: modes with leaders, a locked mode, a mode-aware bottom bar, stacks, single-row bars.
What we leave: one composed screen per client, smallest-client size, a server that owns the layout.

### 1.2 herdr *(gk)*

- A terminal workspace for coding agents. A sidebar lists workspaces and agents with their state,
  so the user sees at a glance which agent waits.
- A thin local client reaches a remote server over SSH and streams the UI back.

What we take: the sidebar as the agent overview. What we leave: one server rendering the UI.

### 1.3 Rex

- A Go server with a native Mac client. One control connection per client, one data connection per
  visible terminal, a snapshot then raw bytes, explicit size ownership (`hab-logic-api.md` 1.1).
- The server owns the layout: windows (tabs), splits and floating layers.
- Each device lists several hosts, each with several endpoints tried in turn (`r1b`).

What we take: the connection model and the size owner, already in `hab-logic-api.md`. What we
leave: a server-owned layout.

### 1.4 tssh *(gk)*

- An SSH client written in Go. It reads the user's OpenSSH config (host aliases, user, port,
  identity files, jump hosts) and uses the SSH agent, so it behaves like `ssh` with no new setup.
- It adds keepalives and reconnect help. It also has a UDP mode in the style of mosh.

What we take: an in-process Go SSH client that honours the user's OpenSSH config. What we leave:
the UDP mode (owner rule: no mosh).

### 1.5 The earlier GFF web client *(gk)*

- Shows tabs from the terminal workspace, with each tab's agent state.
- Reads pane content from screen dumps, chat from transcripts.

What we take: per-tab state markers. What we leave: screen dumps (`hab-logic-api.md` 1.3).

### 1.6 The owner's current terminal setup

Studied as a behaviour reference only. It is one long-lived zellij session per machine with:

- a tab bar on top with the session name and a clock;
- a second row with agent usage budgets and a count of running agents;
- a bottom bar with the keys of the current mode and a system-info cluster on the right;
- stacked panes as the main way to add a pane, and splits on direction keys;
- keymap trims that leave common program and shell keys to the program;
- agent awareness: which pane runs which agent, and its state;
- agents that drive the workspace themselves through a CLI and an MCP server, on this host and
  others.

Section 12 lists each behaviour and its `gff` equivalent. Section 13 lists the plugins.

---

## 2. Model

### 2.1 What gff holds

| Item | Where it comes from | Kept by gff |
|---|---|---|
| Hosts | `gff`'s config file; later, the mesh peer list (Part 3) | Label, endpoints, pinned device id |
| Projects, sessions, panes, agent state | Each hab's event stream (`hab-logic-api.md` 3.5) | In memory; dropped on a new epoch |
| Pane screens | Data streams (`hab-logic-api.md` 5) | One parser per visible pane |
| Workspace: tabs, splits, stacks, focus | `gff` | A local state file per workspace |
| Keymap, theme, bars | `gff`'s config file | Yes |

### 2.2 Words

| Word | Meaning here |
|---|---|
| workspace | One saved set of tabs. `gff` opens the default workspace unless told another |
| tab | A client-side page holding a layout of panes. The hab knows nothing of tabs |
| pane | A view of one hab pane. Two views of one hab pane are allowed |
| sidebar | The narrow list of sessions on the left |
| status board | The full-screen list of every session on every hab, as in `app.md` 3 |
| shortcut bar | The bottom row: keys for the current mode, then system info |
| needs you | As `app.md` 2.2: `blocked`, `error`, or `done` and not seen |
| online, offline | Whether `gff` has a live control connection to a hab |

### 2.3 Processes

- `gff` is one process per terminal window. It has no server of its own. Programs live in the
  holder on each host, so closing `gff` ends no program.
- Several `gff` processes may run at once, on one machine or many. Each is one client per hab.
- `gff` shares its connection library with the Flightdeck app (Part 5 stage 2; see 15.2).

Sources: `hab-logic-api.md` 2.2 (structure on the hab, presentation per client); `app.md` 2.

---

## 3. Screen

### 3.1 Layout

```
+---------------------------------------------------------------+
| tab bar: tabs ............ needs-you count  warnings  clock   |
| info row (optional): agent usage, agent counts                |
+-----------+---------------------------------------------------+
| sidebar   | panes of the current tab                          |
|           |                                                   |
+-----------+---------------------------------------------------+
| shortcut bar: keys for this mode ........ system info         |
+---------------------------------------------------------------+
```

- Every bar is one row. The info row and the sidebar can be turned off.
- Pane frames draw only a top line: the pane's title, its host, its agent name and state, and a
  short note (an exit status, "not owner", "offline").
- A zoomed pane fills the whole pane area. A full-screen mode hides every bar too.

### 3.2 Rendering

- **Composed view (default).** `gff` keeps one terminal parser per visible pane. It draws each
  pane's cells into its rectangle, then draws the bars, and writes only the changed cells to the
  host terminal. The parser choice is a plan `/consensus` (libghostty through cgo, or pure Go,
  behind one interface).
- **Pass-through view.** In full-screen mode with one pane, `gff` may write the pane's bytes
  straight to the host terminal. It starts from a `snapshot-vt` (`hab-logic-api.md` 5.5, Q6).
  It leaves pass-through the moment a bar, a split or a resize would need composing.
- **Host terminal features.** `gff` asks the host terminal for the kitty keyboard protocol, mouse
  reporting, bracketed paste, OSC 8 links, styled underlines and true colour, and falls back when
  the host lacks one.
- **Terminal title.** `gff` sets the host window title to `<host> · <workspace> | <pane title>`.
  An empty part drops with its separator.
- **Theme.** Chrome uses theme tokens. The default theme maps the tokens to the host terminal's
  own ANSI colours, so `gff` matches any terminal theme with no config. A theme file can override
  them.

### 3.3 Edge cases

| Case | Behaviour |
|---|---|
| The host terminal is too small for the bars (under about 40 x 10) | Bars hide, then the sidebar; the focused pane keeps the space |
| The host terminal lacks the kitty keyboard protocol | `gff` reads legacy keys; key combinations that legacy encoding cannot carry are not offered in the default keymap |
| A pane program sets a palette colour (OSC 4) | It changes that pane's parser only, never the host terminal |
| A pane program sets the window title (OSC 0/2) | It becomes the pane's title, shown in its frame and in the host title when focused |
| Pass-through and a host resize | `gff` leaves pass-through, composes once, and may re-enter |
| A pane prints a terminal query | The hab already answered and removed it (`hab-logic-api.md` 5.6). `gff` never answers a query from a stream |

Sources: zellij layout and frames (1.1); the owner's setup (1.6); `hab-logic-api.md` 5.5, 5.6, Q6;
r1-sessions section 2 (pass-through for one pane, a parser per pane for splits).

---

## 4. Sidebar

### 4.1 Behaviour

- **One list for every hab.** Sessions from all online habs, merged by host id. Seq is never
  compared across habs (`hab-logic-api.md` 7.9).
- **Order by who needs you**, with the same ranks as the app's board (`app.md` 3.1): blocked,
  error, done not seen, working, idle, then no agent, exited and unfiled panes. A row moves only
  when its rank changes, so the list does not jump.
- **Grouping.** A key switches between grouping by project and grouping by host.
- **A row shows:** a state mark and colour, the session name, the host label, and the time in that
  state. A needs-you row adds the request kind (permission, question, auth).
- **Rows in view.** A mark shows which sessions are on screen in the current tab, and a dimmer one
  for other tabs.
- **Actions.** Enter opens the session: it focuses an existing view, or adds a pane to the current
  tab. A second key opens it in a new tab. Other keys answer an approval (7.2), start or resume a
  harness, and close an exited session.
- **Filter.** Typing in the focused sidebar filters by session, project or host name.
- **Width.** About 28 columns, adjustable. Names truncate in the middle so the end stays readable.
- **Mouse.** A click opens; the wheel scrolls.

### 4.2 Edge cases

| Case | Behaviour |
|---|---|
| A hab goes offline | Its rows stay, dimmed, with "offline since". Actions that need the hab are off |
| A hab restarts (new epoch) | Its rows are replaced from the new snapshot in one swap, with no flash |
| Two habs hold the same project | One project group; rows labelled by host |
| Many sessions | Needs-you rows first; idle rows fold per project after a few |
| The host terminal is narrow (under about 100 columns) | The sidebar hides; a key shows it as an overlay |
| A hab's API revision does not fit | The host shows one "update needed" row with the side to update. Its sessions are hidden, not stale |
| An unfiled pane | Shown at the bottom of its host, so the owner can file or close it |
| A session is in view in another `gff` window | No mark: view marks are per `gff` process |

Sources: herdr sidebar (1.2) *(gk)*; `app.md` 3.1 and 3.2 (ranks, offline rows, unfiled panes);
`hab-logic-api.md` 2.2, 7.9.

---

## 5. Tabs and panes

### 5.1 Tabs

- **Client-side.** A tab is `gff` presentation. The hab stores no tab, split or focus
  (`hab-logic-api.md` 2.2).
- **Mixed hosts.** One tab may hold panes from several habs. Each frame names its host.
- **Tab bar.** Tab names left to right; the active tab marked. Each tab shows a needs-you mark when
  a session in it needs you, a bell mark when a pane rang the bell, and an activity mark when a
  hidden pane printed. The right side shows the needs-you count across all habs, a warnings badge
  and a clock.
- **Startup.** The config may name startup tabs, each with a working directory and an optional
  command. They are made only when the workspace is new.
- **Saved.** The workspace (tabs, splits, stacks, which hab pane each view shows, focus) is written
  to `gff`'s local state file on each change, debounced. The next `gff` start restores it.
- **Tab actions.** New, rename, close, move left and right, go to by number, go to last.

### 5.2 Panes

- **New pane.** Starts a program on a hab: by default the focused pane's host, in the focused
  pane's working directory. The pane stacks onto the focused pane unless a direction is given.
- **Splits.** A split on any of four directions; the new pane lands on that side.
- **Stacks.** One pane in a stack is expanded; the others fold to their title row. Focus moving
  past the end of a stack wraps.
- **Zoom.** Toggles the focused pane to fill the pane area.
- **Close view versus end program.** Closing a view removes it from the tab; the program keeps
  running and stays in the sidebar. Ending the program is a separate action that confirms first.
- **Reopen.** The last ten closed views can be reopened in place.
- **Focus.** Moves on direction keys from normal mode, and wraps to the next tab at the edge when
  the config says so.
- **Scrolling.** The wheel scrolls a pane's scrollback with no mode change. Scrollback comes from
  the local parser; scrolling sends nothing to the hab.
- **Typing while scrolled up.** The key goes to the program. The view stays where it is until the
  owner scrolls down or presses a key that jumps to the bottom.
- **Selection.** A mouse drag selects text in one pane and copies it through the host terminal's
  clipboard (OSC 52). Links (OSC 8 and detected URLs) open on a modifier click.
- **Bell.** A bell marks the pane frame and its tab for about one second, then clears.
- **Exit.** A pane whose program exits shows its exit status in the frame note. A clean exit of a
  console pane offers a new shell; an agent pane offers Resume (6.3).

### 5.3 Edge cases

| Case | Behaviour |
|---|---|
| A saved view points at a hab pane that is gone | The view shows "exited" with Resume or Close |
| A saved view points at an offline hab | The view shows the last screen it had, dimmed, or "offline" if none |
| Two views of one hab pane in one tab | Allowed. Each opens its own data stream; both render at the owner's size |
| A pane is in a hidden tab | Its data stream closes (11.3). Its marks come from events, not bytes |
| Two `gff` windows open the same workspace | See 15.4 |
| A new pane on a host that is offline | Refused with "<host> is offline" |
| A pane's program uses the alternate screen | Wheel scrolling sends arrow keys or mouse events, as the program's mouse mode asks, not local scrollback |

Sources: zellij tabs, stacks and modes (1.1); the owner's setup (1.6); `hab-logic-api.md` 2.2 and
5.1; Rex windows (1.3).

---

## 6. Sessions across hosts

### 6.1 Behaviour

- **Every hab at once.** `gff` opens one control connection to each configured host and subscribes
  with scope `host`. The sidebar, the board and the tab bar merge them by host id.
- **Identity.** A session is named by (host id, session id). Two hosts can hold sessions with the
  same name; rows always show the host.
- **Hosts.** The first build reads hosts from `gff`'s config file: a label and one or more
  endpoints (the local socket, or an SSH destination). Part 3 adds the mesh peer list, so one
  configured hab can name the rest. Nothing reads another tool's machine list.
- **Starting work.** "New session" asks for a host and a project directory, then has that hab start
  the harness or a shell (14).
- **A running agent stays on its host.** `gff` never moves a session between hosts (owner
  decision, master plan).
- **Clean environment.** A pane the hab starts takes the hab's own environment, never the
  environment of the `gff` process that asked. An agent's identity variables cannot leak into
  unrelated panes.

### 6.2 Restore after a reboot

- The holder survives `gff-hab` restarts, not a machine reboot. After a reboot the hab marks each
  session exited and keeps its record (`hab-logic-api.md` 2.2).
- `gff` restores the workspace. Each view of an exited agent session shows Resume, which starts the
  harness with its resume id. Resume never runs on its own.

### 6.3 Edge cases

| Case | Behaviour |
|---|---|
| One host is slow to answer | Other hosts show at once. The slow host shows "connecting" |
| A host's clock differs | Times in state come from the hab's own `at` stamps against the hab's own time (`app.md` 3.2) |
| The same host is reachable on two endpoints | One connection: endpoints are tried in order, the first to complete wins (11.1) |
| A host's device id changed | Refused until the owner accepts it (11.4). Its rows show "host key changed" |
| A host is in the config but has no hab installed | The row says "no hab on <host>" with the install hint |

Sources: Rex hosts and endpoints (1.3, `r1b`); `transport.md` 4.1, 6.4; `hab-logic-api.md` 7.9;
master plan owner decisions (a running agent stays on its host).

---

## 7. Status board

### 7.1 Behaviour

- **A full-screen view** on one key, over the panes. Same ranks, grouping and row content as the
  app's board (`app.md` 3.1), so both clients agree.
- **A row shows:** session name, project, host, harness, state with its kind, time in state, and
  one line of context: the pending command for a permission, the question for a question, else the
  last agent message.
- **Preview.** The selected row shows a live preview of its pane beside the list, through an
  observer stream (`observe: true`). An observer never resizes and never takes ownership
  (`hab-logic-api.md` 6.4).
- **Seen.** Opening a session from the board, or focusing its pane anywhere, reports focus to the
  hab, which turns `done` into `idle` (`app.md` 17, `session.focus`).
- **Leaving.** Esc returns to the tab view as it was.

### 7.2 Approvals

- A `blocked:permission` row shows the tool, the exact command or file list, and the working
  directory, in full.
- Allow and Deny keys answer it through the hab (`approval.answer`, `app.md` 17). Broader choices
  ("allow for this session") open the pane so the owner answers in the harness itself.
- An answer elsewhere (the app, another `gff`, the harness) clears the row at once.
- A stale answer is refused with `conflict`; the board shows "already answered".
- A row whose state comes from screen text only offers no answer keys, only "open"
  (`harness-adapters.md`, Rule 2).

### 7.3 Notifications

- When a session becomes needs-you and is not on screen, `gff` marks its tab and may send a desktop
  notification through the host terminal (OSC 9 or OSC 777). Off by default.

### 7.4 Edge cases

| Case | Behaviour |
|---|---|
| A hab goes offline while a row is blocked | The row stays at the top, dimmed; answer keys are off |
| The command is longer than the row | The row wraps under the selection; the full text is always reachable |
| Many approvals at once | One row each, grouped by session |
| The preview pane is huge | The preview renders at the owner's size, scaled down by clipping to its corner, never by resizing |

Sources: `app.md` 3 and 7 (ranks, row content, approval flow, answered elsewhere);
`harness-adapters.md` (Rule 2, Permission answers); `hab-logic-api.md` 6.4.

---

## 8. Shortcut bar

### 8.1 Behaviour

- **Mode-aware.** The bottom row shows the keys valid in the current mode, each with a short label.
  In normal mode it shows the leaders; in a mode it shows that mode's keys.
- **Derived from the live keymap.** Hints come from the keymap `gff` actually loaded. An unbound
  key disappears from the bar. No hint list is kept by hand.
- **Modes.** Normal, locked, pane, tab, resize, move, session, and the sidebar and board when they
  have focus. Each mode is entered by a leader from normal mode and left with Esc or Enter.
- **Locked mode** passes every key to the pane except the unlock key. The bar shows only that key.
- **Keys left to programs.** The default keymap does not take keys that shells and harnesses use:
  for example Ctrl+B (harnesses background a task with it), Ctrl+S, Alt+F and Alt+B (shell word
  moves) and Alt+arrows. The leaders are few, and the config can change every one.
- **Fitting.** When the row is too short, hints drop by priority, then shorten to the key alone.
  A configurable cap limits how many show.
- **System info** fills the right end (9).

### 8.2 Keys reach the pane intact

- `gff` reads keys from the host terminal with the kitty keyboard protocol when the host offers it.
- It encodes each key for the pane in the form the pane's program asked for: the kitty protocol at
  the level the program enabled, else legacy bytes. The pane's keyboard mode comes from its parser.
- Paste arrives as one `pane.input` call, with bracketed paste markers only if the program enabled
  bracketed paste.

### 8.3 Edge cases

| Case | Behaviour |
|---|---|
| The config binds one key twice | The later binding wins; `gff` warns once at start, naming the file and line |
| The config names an unknown action or key | Warned and skipped; the rest of the config loads |
| A leader collides with a key the focused program needs | Locked mode gets it through; the bar shows the unlock key |
| A key combination the host terminal cannot send | It is not offered as a default binding |
| A mode is entered and no key follows | The mode stays until Esc; no timeout |

Sources: zellij modes and status bar (1.1); the owner's setup (1.6: hints from the live keymap,
trims that free program keys, tolerant config); `hab-logic-api.md` 3.8 (one input call per paste).

---

## 9. System info

### 9.1 Behaviour

- **Right end of the shortcut bar.** CPU load, memory, disk use, battery when present, and uptime,
  each as a short figure. Each item can be hidden.
- **Whose figures.** The focused pane's host, labelled with the host when it is not the local
  machine. This needs a host-stats call or event from the hab (14). Until then, the local machine
  only.
- **Clock** at the right end of the tab bar, in the local time zone, format configurable.
- **Warnings badge** in the tab bar: a hab that needs an update, a hab or holder build that is
  superseded, a host key change, a host offline. Selecting it lists the warnings.
- **Usage row** (on by default, second row): each harness account's usage windows (for example
  5-hour, weekly, per model) with percent used and reset time, a warning colour near a limit, and
  per-harness counts (working, needs you) across all habs. Usage is first-class (15.6).

### 9.2 Edge cases

| Case | Behaviour |
|---|---|
| The focused host is offline | Its figures dim and show their age |
| A machine has no battery | The battery item hides |
| The hab does not offer host stats (an older hab) | `gff` shows local figures, unlabelled |
| Figures refresh | At most every 2 s; hidden bars do not refresh |

Sources: the owner's setup (1.6: system info on the bottom bar, clock and badges on the tab bar,
usage row); `hab-logic-api.md` 3.4 (capabilities for features a host may lack).

---

## 10. Pane size

`gff` follows `hab-logic-api.md` 6. This section says what the client does.

### 10.1 Rules

- **Opening never takes ownership.** Showing a pane, focusing it or resizing the host terminal
  does not make `gff` the owner.
- **Typing takes ownership.** Input from `gff` reaches the hab as `pane.input`. If `gff` is still
  the latest source of input after about 300 ms, the hab makes it the owner (reason `typed`) and
  applies its desired size. Further typing changes nothing.
- **Desired size.** Whenever a view's rectangle changes (a split, a zoom, a host resize), `gff`
  sends `pane.set_desired_size` for that pane. It coalesces a burst and sends the last size,
  trailing, at most every 50 ms. The hab applies it at once only if `gff` owns the pane or the pane
  has no owner.
- **Two views, one pane.** `gff` sends the size of the focused view.
- **Fit.** A key sends `pane.take`: "fit this pane to this window" without typing.
- **Observers never resize.** Board previews and sidebar thumbnails open data streams with
  `observe: true`. They never take ownership, never send a size, never send input.
- **Not the owner.** `gff` renders the pane at the owner's size. Larger than the view: it clips and
  follows the cursor, with a frame note "owner <label> <cols>x<rows>". Smaller: it pads the rest in
  a dim fill.
- **Redraw.** One key sends `pane.redraw` with mode `snapshot`: a fresh snapshot on this stream
  only. A second key sends mode `nudge`: snapshot, then the PTY one column narrower and back about
  100 ms later, for a program that needs a size change to repaint. **`gff` never sends Ctrl+L**,
  which is input that harnesses may bind.
- **The host terminal resizes.** `gff` repaints its own screen from its parsers. That touches no
  program.

### 10.2 Edge cases

| Case | Behaviour |
|---|---|
| The owner types in the app, then in `gff`, then in the app | One resize per change after the 300 ms debounce, not one per key |
| A window drag sends twenty sizes | `gff` sends the last one; the hab applies the owner's latest at most every 50 ms |
| `gff` owns a pane and quits | The control connection closes; the pane keeps its size and has no owner (`disconnect`). Nothing reflows |
| `gff` reconnects within 60 s | It keeps ownership (`restore`) unless another client took it |
| A pane is zoomed and unzoomed quickly | Only the final size goes out |
| The screen looks wrong after a hab crash | The pane frame shows "suspect"; redraw (snapshot) fixes the view; nudge fixes the program |
| Two `gff` windows type into one pane | Both inputs land in order; ownership follows the debounce; each shows who else typed (`pane.activity`) |

Sources: `hab-logic-api.md` 6.1 to 6.7, 7.1; r1-sessions section 4 (owner model, debounce, nudge,
no Ctrl+L); herdr observers (`hab-logic-api.md` 1.2).

---

## 11. Connection

### 11.1 Transports

| Host | Transport | Authentication |
|---|---|---|
| This machine | The hab's Unix socket (`hab-logic-api.md` 4.1) | Socket permissions and peer credentials |
| Another machine | SSH to the system sshd, running `gff-hab connect` (4.2) | The user's own SSH keys |
| Another machine with the embedded server on | SSH to `gff-hab`'s own server (`transport.md` 4.2) | A key paired with that hab |

- **SSH client.** `gff` uses an in-process Go SSH client, as tssh does. It reads the user's OpenSSH
  config for the destination (host alias, host name, user, port, identity files, jump hosts) and
  uses the SSH agent. It checks the system sshd's host key against the user's known-hosts file.
  See 15.3 for the alternative of running the system `ssh`.
- **Endpoints.** A host may list several endpoints. `gff` tries them in order, starting the next a
  short time after the previous one, and keeps the first that completes.
- **No mosh, no UDP carrier.** A dropped link reconnects (11.5). A later carrier is a separate
  owner decision (`transport.md` 3, option F).

### 11.2 Control connection

- One per hab. The first request is `hab.hello`; then `events.subscribe {scope: host}`.
- Seq rule: apply each response, then only events with a higher seq. A gap, a `closing` notice or a
  silent close means reconnect and read state again (`hab-logic-api.md` 3.5).
- Liveness: `hab.ping` every 15 s when idle. No message for 45 s means the connection is dead.
- Input, ownership, redraw and approvals go over the control connection.

### 11.3 Data connections

- **One per visible pane.** A view on screen opens one; a hidden view closes its own. Observer
  previews open observer streams.
- **Opening.** `data.ticket {pane, observe?}` on the control connection, then a data connection
  with the ticket and, to resume, the holder offset `gff` holds for that pane.
- **Over SSH:** each data connection is another session channel on the same SSH connection
  (`hab-logic-api.md` 4.2, option A). Past nine open channels on one host through the system sshd,
  `gff` opens a second SSH connection (`transport.md` 7.6).
- **Flow.** `gff` returns credit as it applies bytes. A pane more than 4 MiB behind gets a fresh
  snapshot from the hab.
- **Offsets.** `gff` keeps the offset after the last applied byte for every pane it has shown, even
  after the view closes, so reopening resumes instead of re-snapshotting when the hab still has the
  bytes.

### 11.4 Device ids

- The welcome carries the hab's `device_id`. `gff` pins it on first contact in its own state file.
- A changed id fails the connection. `gff` shows both ids and says to accept it on purpose. It never
  accepts on its own. A welcome with no `device_id` leaves the hab unpinned (`transport.md` 4.1).

### 11.5 Reconnect

1. A lost control connection retries at once, then with backoff and jitter up to 30 s apart.
2. A wake from sleep, or a network change, retries at once. `gff` detects a wake by a jump between
   wall-clock and monotonic time.
3. The hello carries the reconnect token. Within 60 s and the same epoch, `gff` keeps its client id,
   ownership and tickets (`hab-logic-api.md` 3.7).
4. A new epoch means a fresh snapshot of structure. Every object is replaced, whatever revision it
   had.
5. Data streams reopen with their offsets. The hab continues from there, or sends a snapshot.
6. While a host is down, its panes keep their last screen, dimmed, with "reconnecting". Typing into
   them is refused with a frame note, never buffered.

### 11.6 Edge cases

| Case | Behaviour |
|---|---|
| The laptop sleeps for an hour | On wake, every host reconnects at once. New client id; structure re-read; streams resume or snapshot |
| A slow link | Events have their own 8 MiB backlog limit; data streams have credit. One busy pane never stalls the sidebar |
| The revision does not fit | The host shows "update needed" with the side to update (`hab-logic-api.md` 7.3) |
| The SSH agent has no usable key | The host shows the SSH error and which key files were tried |
| A jump host is down | The host shows offline with the jump host named |
| The system sshd refuses more sessions | `gff` opens another SSH connection and retries the channel |
| A network that drops SSH | Every endpoint fails fast; the host shows "network blocks SSH" when TCP connects but SSH never starts |
| Typed input during a reconnect | Refused, not queued: a delayed keystroke could approve something the owner no longer wants |

Sources: `hab-logic-api.md` 3, 4, 5; `transport.md` 4, 5, 7; tssh (1.4) *(gk)*; `app.md` 10.2
(backoff, endpoints).

---

## 12. Parity checklist

Each row is a behaviour the owner relies on in the current terminal workspace. The parity review
(Part 5 stage 8) passes a row when its test passes. Tests follow `docs/testing.md`: the lowest
layer that proves the behaviour. "Screen test" means `gff` runs against a fake hab and its output is
read back through a terminal parser.

| Behaviour | How the owner uses it today | gff equivalent | Test that proves it |
|---|---|---|---|
| Workspace opens on launch | Every new terminal window lands in the same long-lived workspace, already holding its tabs | `gff` opens the default workspace and restores its tabs (5.1) | Screen test: start, add a tab, quit, start; the tab is back with the same panes |
| Startup tabs | A new workspace starts with two named tabs in the projects directory | Startup tabs from config (5.1) | Screen test: a new workspace shows the configured tabs with their directories |
| Tab bar with session name and clock | Glances at the top row for tabs, workspace name and time | Tab bar (5.1, 9.1) | Screen test: tab names, active mark and clock in the top row |
| Agent usage row | Watches agent usage budgets and the count of running agents on a second row | Usage row (9.1), first-class (15.6) | Screen test: counts match the fake hab's agent states |
| Mode-aware shortcut bar | Reads the bottom row for the keys of the current mode | Shortcut bar (8.1) | Unit: hints for each mode come from the loaded keymap |
| Unbound key leaves the bar | Removing a binding also removes its hint | Hints derive from the live keymap (8.1) | Unit: unbind a key in config; its hint is absent |
| Keys left to programs | Harness and shell keys (Ctrl+B, Alt+F, Alt+B, Ctrl+S, Alt+arrows) reach the program | Default keymap takes none of them (8.1) | Screen test: each key arrives in the fake hab's `pane.input` as typed |
| Locked mode | Locks the workspace so every key goes to the program | Locked mode (8.1) | Screen test: in locked mode every key but unlock reaches the pane |
| System info on the bottom bar | Checks CPU, memory, disk, battery and uptime at a glance | System info (9.1) | Unit: the formatter over fixed figures; screen test: the cluster sits at the right end |
| Stacked panes by default | A new pane stacks on the current one; stacks are the main layout | New pane stacks (5.2) | Screen test: new pane gives a stack of two, one expanded |
| Splits on direction keys | Splits left, right, up or down from pane mode | Splits (5.2) | Screen test: each direction places the new pane on that side |
| Focus moves on direction keys | Moves focus with one chord from normal mode | Focus keys (5.2) | Screen test: focus moves and wraps in a stack |
| Zoom and close | Zooms a pane to full size; closes a pane | Zoom; close view versus end program (5.2) | Screen test: close view leaves the session in the sidebar; end program confirms first |
| Reopen a closed pane | Brings back a pane closed by mistake | Reopen the last ten views (5.2) | Screen test: close, reopen; same pane, same place |
| Scroll without a mode | Scrolls with the wheel, no scroll mode | Local scrollback on the wheel (5.2) | Screen test: wheel up shows earlier lines; no bytes reach the hab |
| Typing while scrolled up | Types into a pane while reading back; the view stays put | Key goes to the program; view stays (5.2) | Screen test: scrolled view unchanged after typing; input reached the hab |
| Thin pane frames with agent names | One title line per pane shows its handle, agent name and state | Top-line frames with title, host, agent and state (3.1) | Screen test: frame shows the fake agent's name and state |
| Exit status on the frame | A pane that exited non-zero is marked with its status | Frame note (5.2) | Screen test: an `exited` frame shows the status |
| Bell marker | A bell marks the pane briefly, then clears | Bell mark on pane and tab for about 1 s (5.2) | Screen test: the mark appears, then clears (needs the bell event, 14) |
| Finished command drops to a shell | A command pane that exits cleanly becomes a shell | Console pane offers a new shell (5.2) | E2E: a console command exits 0; one key gives a shell in the same directory |
| Large floating pane | Temporary panes (an image, a quick command) float over the tab at most of its size | Open question 15.9 | Decided in 15.9 |
| Window title names the host | The terminal window title shows host, workspace and pane | Title template (3.2) | Screen test: the OSC 2 output matches the template |
| Several windows mirror one workspace | Two terminal windows on one machine show the same tabs and focus | Open question 15.4 | Decided in 15.4 |
| Agents on every host | Lists coding agents in every session on every listed host | Sidebar and board across habs (4, 6, 7) | E2E: two habs; the sidebar lists both hosts' sessions, ranked |
| Agent state per pane | Sees which agent is working, blocked or done | State marks from adapter events (4.1) | Screen test: each fake state shows its mark and rank |
| Restore after a reboot | After a restart, agents come back with their resume line ready, not run | Workspace restored; exited sessions offer Resume (6.2) | E2E: kill the holder; restart; views show Resume; nothing ran |
| Clean environment for new panes | A rebuilt workspace drops agent identity variables so shells do not claim to be agents | Panes take the hab's environment (6.1) | E2E: start `gff` with an agent variable set; a new pane's environment lacks it |
| Agents drive the workspace | Agents open panes, read screens, wait for output, type input and supervise other agents, here and on other hosts | The hab API, plus `gff` CLI verbs over the shared library (15.10) | E2E: a script opens a pane, waits for a line, reads it, sends input, over `ssh localhost` |
| Kitty keyboard and OSC 8 links | Full key combinations and clickable links | Host features (3.2) and key encoding (8.2) | Unit: key encoder goldens per keyboard mode; screen test: an OSC 8 link survives composing |
| Theme | One dark theme across the workspace | Theme tokens mapped to the host palette (3.2) | Unit: every chrome colour comes from a token |
| Status notices | A badge warns of a stale build or a missing permission | Warnings badge (9.1) | Screen test: an "update needed" hab raises the badge |
| Pane size follows the typist | Not available today: the smallest client sets the size | Typing takes ownership (10.1) | E2E (shared with `docs/testing.md` 9): two clients; typing moves ownership once; an observer never resizes |

---

## 13. Plugins and add-ons to confirm with the owner

Each item is loaded by the current workspace config. The recommendation is ours; the owner
decides (Part 5 decision "which zellij behaviours and plugins become `gff` features").

| Plugin or add-on | What it does | Recommendation |
|---|---|---|
| Top tab bar (a fork built-in) | Tabs, workspace name alias, clock, warning badge; colours from the theme | **Carry over** as the `gff` tab bar (5.1) |
| Agent usage bar (a local plugin) | Second row: per-provider usage windows and budgets with reset times, read from a file other tools write, plus a count of running agents | **Carry over as first-class**: usage windows and resets come from the harness adapters through the hab, not from another tool's file; counts come from hab events (15.6) |
| Bottom shortcut bar (a fork built-in) | Mode-aware hints from the live keymap; CPU, memory, disk, battery and uptime on the right | **Carry over** as the shortcut bar and system info (8, 9) |
| Session event feed (a background plugin) | Pushes workspace events to the earlier GFF server, which then reads the tree | **Drop**: hab events replace it (`hab-logic-api.md` 3.5) |
| Agent session hook script | Reports a harness session's id and state to the workspace through terminal escapes | **Replace** with the harness adapters (`harness-adapters.md`) |
| Workspace MCP server and CLI verbs | Let agents list, read, wait on, type into and create panes, also on other hosts | **Replace** with the hab API and `gff` CLI verbs; an MCP surface is 15.10 |
| `session-manager` and `welcome-screen` (stock) | Pick, create and resurrect sessions | **Replace** with the sidebar and status board (4, 7) |
| `tab-bar`, `compact-bar`, `status-bar` (stock) | Default bars, superseded by the bars above | **Drop** |
| `strider` and `filepicker` (stock) | File browser and picker | **Drop**; open a shell or an editor instead |
| `configuration` (stock) | Edit keybindings from inside the UI | **Drop**; the config file is the source, warnings name bad lines |
| `plugin-manager` (stock) | Load and inspect plugins | **Drop** for now; modules (Part 7) bring their own manager |
| `about` (stock) | Version and help page | **Replace** with `gff --version` and a help overlay listing the keymap |
| Web client settings | Font for the browser client | **Drop**: the app is the remote client |
| Older bar builds kept as backups | Earlier tab bar, shortcut bar and status bar builds | **Drop** |

---

## 14. Calls gff needs from the hab

These are not in `api/methods.go` today. Names are proposals. Where `app.md` 17 already proposes a
call, `gff` uses the same one.

| Call or event | Purpose | Also needed by |
|---|---|---|
| `session.create {project_dir, harness?, command?, cwd?}` | Start a harness or a shell in a new pane, filed as a session | App (start, resume) |
| `session.resume {session}` | Start the harness again with its resume id | App |
| `pane.close {pane}` | End a program, after the client confirms | App (close an exited session) |
| `session.focus {session, active}` | Mark `done` seen | App (`app.md` 17) |
| `approval.answer {pane, request, choice}` | Answer a pending request by id | App (`app.md` 17) |
| event `pane.bell` | A pane rang the bell; throttled | App (optional) |
| event `pane.output` | A pane printed; at most once per second per pane, for activity marks on hidden tabs | App (thumbnails off) |
| `hab.stats` or event `hab.stats` | CPU, memory, disk, battery, uptime of the host, with capability `host-stats` | App (optional) |

---

## 15. Open questions

Each question has options and a recommendation. All are **Decision: open** for the owner unless
marked.

### 15.1 The parser for the composed view

Already a plan `/consensus` (libghostty through cgo, or pure Go, behind one interface). This spec
needs only: input from `snapshot-vt` and raw bytes, a cell grid with styles, scrollback, the
program's modes (keyboard protocol, mouse, bracketed paste, alternate screen) and the title.

Recommendation: libghostty behind the interface, matching `gff-hab` and the app, with the pure-Go
fake for tests.

### 15.2 The shared library and the app's SSH

The Part 5 plan shares the Go connection library with the app. `transport.md` 9.3 and `app.md` 10
put the app's SSH on `swift-nio-ssh` with a Secure Enclave key, which Go cannot reach.

| Option | For | Against |
|---|---|---|
| A. The Go library speaks the protocol over a byte-stream seam; each platform supplies the stream (Go SSH in `gff`, Swift SSH in the app) | One protocol implementation; the key stays in the Secure Enclave | A seam to design and test across languages |
| B. The Go library owns SSH everywhere | One SSH stack | Cannot sign with a Secure Enclave key without a callback into Swift on every signature |
| C. The app keeps its own Swift protocol code | No xcframework | Two implementations of seq, offsets and reconnect |

Recommendation: **A.** The library owns hello, seq, events, tickets, offsets, credit and reconnect.
It asks its host for "a new control stream" and "a new data stream" to a hab.

### 15.3 In-process SSH or the system `ssh`

| Option | For | Against |
|---|---|---|
| A. In-process Go SSH that reads the OpenSSH config (tssh style) | Many channels on one connection with no setup; sleep and wake handled in one place | Must track OpenSSH config features; exotic options may be missing |
| B. Run the system `ssh` per channel, with connection sharing | Every OpenSSH feature and the user's own setup | Needs connection sharing set up; one process per channel; harder to detect a dead link |

Recommendation: **A**, with a config option to run a given command per host instead (as
`gff-hab api call -via` does), for setups A cannot reach.

### 15.4 Several windows on one workspace

The owner's current setup mirrors one workspace into every terminal window: same tabs, same focus.

| Option | For | Against |
|---|---|---|
| A. Independent windows; each has its own workspace file | Simple; each window is a client | Two windows drift |
| B. Windows that open the same workspace share it: tab and split changes sync through the local state file, focus stays per window | Familiar; no server | Needs file watching and merge rules |
| C. Full mirror, focus included | Exactly today's behaviour | Two windows can never show different panes |

Recommendation: **B.** Tabs follow the owner; each window keeps its own focus. A "follow" key makes
one window mirror another's focus while held.

### 15.5 Default keymap

| Option | For | Against |
|---|---|---|
| A. Modes with leaders, close to the owner's current keys | No relearning for the owner | Leaders take a few Ctrl keys |
| B. One prefix key, tmux style | Takes one key only | Two strokes for everything |

Recommendation: **A** as the default, with B as a shipped alternative keymap.

### 15.6 Agent usage budgets

The current usage row reads a file that other tools write. Flightdeck must not read another tool's
file.

Decided (owner, 2026-10-31): usage is first-class in gff and the app. Each harness adapter reports
its account's usage windows through a documented interface of that harness (for example the data a
harness passes to its status line or hooks). gff-hab keeps the latest reading per account and host
and serves it to clients, which show it in the usage row, alerts and widgets.

### 15.7 Activity marks on hidden tabs

Hidden panes have no data stream. Marks need events.

Recommendation: add `pane.output` (throttled, at most once per second per pane) and `pane.bell`
to the hab API (14). Agent state already arrives as events.

### 15.8 Desktop notifications from gff

Recommendation: off by default. When on, OSC 9 or OSC 777 through the host terminal, only for
needs-you sessions not on screen. Push stays the app's job.

### 15.9 Floating panes

| Option | For | Against |
|---|---|---|
| A. A floating pane layer, sized to about 90% of the pane area | Matches today's temporary panes and image viewing | A second layout layer to build |
| B. Temporary panes open as a zoomed pane in a scratch tab | Nothing new to build | Not over the current work |

Recommendation: **B** for the first build, **A** after the parity review if the owner misses it.

### 15.10 Agents driving the workspace

Agents use today's CLI and MCP server to open panes, read screens, wait for output, type, and
supervise other agents, also on other hosts.

| Option | For | Against |
|---|---|---|
| A. `gff` CLI verbs over the shared library (`gff ls`, `read`, `wait`, `send`, `new`) | Same connection model; works across hosts | A second surface beside the hab's own `api call` |
| B. An MCP server built into the hab | Agents need no client binary | Each hab serves it; cross-host needs the mesh |
| C. Both: hab methods first, then a thin MCP over them | Each layer stays small | Two layers to keep in step |

Recommendation: **C**, with the MCP in the plan part that owns agent tooling (Part 8 or 9). Waiting
on output needs a hab call ("wait for a line, for quiet, for exit, for a state change") so no
client polls the screen.

### 15.11 Which plugins carry over (owner)

Section 13 lists each plugin and add-on with a recommendation. The owner confirms or changes each
row. Answer recorded in the Part 5 plan.

---

## 16. Sources

Opened while writing this spec, 2026-10-31.

- Our specs: `docs/specs/hab-logic-api.md`, `docs/specs/transport.md`, `docs/specs/app.md`,
  `docs/specs/harness-adapters.md`, `docs/testing.md`, `docs/layout.md`, `api/methods.go`.
- zellij documentation for configuration, keybindings, layouts and plugins:
  https://zellij.dev/documentation
- The project's research notes on sessions, streaming, telemetry and PTY size (sections 2 to 4),
  and on Rex's local install. None of their private detail is reproduced here.
- The owner's current terminal configuration, layouts and plugin list, read as behaviour only.
  No path, host or tool name from it is reproduced here.

Not opened: the herdr, tssh and earlier GFF sources. Facts marked *(gk)* need a check when the
stage that relies on them starts.
