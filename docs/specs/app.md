# Flightdeck app: behaviour spec

Status: draft for review. Part 6, study stage.

This spec says how the Flightdeck app behaves on iPhone, iPad and Mac. Later Part 6 stages build
from it: the status board, the chat view, the terminal view, the console, approvals, push and the
Live Activity. It describes behaviour in our own words. It copies no code from the reference apps.

The app talks to habs only. What a hab serves is in `hab-logic-api.md`. How the app reaches a hab
and proves who it is is in `transport.md`. What the agent is doing comes from the adapters in
`harness-adapters.md`. This spec names the calls the app needs from those specs. Where a call does
not exist yet, it says so.

Owner rules this spec works within:

- The app works with no push. Push is an extra that each user sets up with their own APNs key.
- Pairing starts on the hab: `gff-hab pair` prints a QR code.
- The app embeds no VPN and no tailnet node. A system VPN underneath is fine.
- Face ID is optional and off by default.
- iPhone and iPad ship through internal TestFlight. The Mac app ships with Developer ID,
  notarized.
- No hub. The app connects to each hab itself.
- Transcripts stay native. The app stores no agent data beyond a cache it can drop.

Each section ends with edge cases and its sources. Facts marked *(gk)* come from general knowledge
of Apple platforms, not from a page opened for this spec. A device test must confirm them.

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [Status board](#3-status-board)
4. [Chat view](#4-chat-view)
5. [Terminal view](#5-terminal-view)
6. [Console](#6-console)
7. [Approvals (pending screens)](#7-approvals-pending-screens)
8. [Push](#8-push)
9. [Live Activity](#9-live-activity)
10. [Connection](#10-connection)
11. [Layout per device](#11-layout-per-device)
12. [Battery](#12-battery)
13. [Calls the app needs from the hab](#13-calls-the-app-needs-from-the-hab)
14. [Open questions](#14-open-questions)
15. [Sources](#15-sources)

---

## 1. References

### 1.1 Happy

- An Expo app (MIT) that shows a live Claude Code or Codex session as chat. The app UI lives in
  `packages/happy-app` of `slopus/happy`.
- A chat holds four message kinds: user text, agent text, tool call, and agent event (a mode
  change, a usage limit).
- A tool call is a card: a header with the tool name and a status (running, done, error), then
  input and output sections. Tools that add little are drawn as a single collapsed line.
- A permission request is a footer on the tool card it gates. The choices are: yes; yes for this
  session; yes for all edits (edit tools only); no, and tell the agent why. For Codex: yes; yes for
  this session; stop and explain. Once answered, the card keeps the answer as a status.
- A message the owner sends shows a status line: sending, sends after this turn, or send failed.
- Happy routes everything through a hosted relay. We take the rendering, not the relay.

What we take: the four message kinds, tool cards with status, the inline permission footer, the
outgoing message status. What we leave: the relay and the account model.

### 1.2 Heeler

- An iOS client for herdr (Apache-2.0). One list of every agent across machines, sorted by status
  with blocked first, filterable by host, updated live.
- A blocked agent is answered with buttons in its chat.
- It connects over SSH: a channel per request, plus one long-lived channel for the event stream.
- A host may have several routes, such as a LAN address and an overlay or jump-host route.
- It embeds Tailscale, ZeroTier and EasyTier inside the app, so no iOS VPN is needed. It has no
  notification categories, so it offers no lock-screen actions.
- A composer drafts with the full iOS keyboard (autocorrect, dictation), then sends once. A tools
  keyboard carries agent control keys and snippets.

What we take: the "who needs you" order, one event channel per host, several routes per host, the
composer. What we leave: the embedded overlay networks (owner rule).

### 1.3 Moshi

- A closed-source iOS and Watch terminal for coding agents. Agent hooks feed a host daemon that
  holds a WebSocket to Moshi's server, which sends pushes and inbox rows.
- The inbox keeps one active row per agent session. New events update that row.
- Event kinds: approval required, task complete, session started, tool running and finished
  (throttled).
- Approval and error events raise a banner. Task complete does not: it only updates the Live
  Activity.
- The Live Activity tracks one session at a time. It shows the session title, the agent and the
  latest event. It has a Detailed style (with a context bar) and a Compact style.
- Approval events put Approve and Deny buttons in the Live Activity. When the owner answers on the
  phone or the Mac, the buttons go away at once.
- When the session ends, the final state stays about ten minutes.
- A setting picks where a tap lands: home, inbox, terminal or chat, or "smart" (approvals open the
  inbox detail, everything else opens the terminal or chat).
- The Watch app lists the same inbox, grouped by session, project and host, with pending approvals
  at the top. It answers Allow or Deny. It is not a terminal.
- With end-to-end encryption on, the server "can no longer approve anything for you", and the Live
  Activity shows no approval buttons on a host whose key the phone lacks.
- On a Mac host, the daemon can silence pushes while the Mac is unlocked (off by default). If the
  lock check fails, the push goes out, so an approval never disappears silently.

What we take: approvals on the banner and in the Live Activity, buttons withdrawn when answered
elsewhere, no banner for a finished turn, one row per session, the tap-target setting, "never drop
an approval silently". What we leave: a hosted server in the push path.

### 1.4 rootshell

- An iOS terminal (MIT). Agent sessions appear as cards with live status (working, waiting for
  input, done) in a vertical tab bar. The inbox can group by project.
- Long-running commands (builds, tests, deploys) also become cards. A card is marked unread when it
  finishes unseen. It notifies when a command blocks on input. This is off by default.
- The keyboard bar is drag-and-drop, with custom keys that send text or key sequences, and up to
  five drawer rows. Modifiers are sticky: one tap for one key, a double tap to lock.
- Tapping a notification returns to the exact window, tab and pane it came from.

What we take: unread-when-finished-unseen, the keyboard bar, sticky modifiers, a tap that lands on
the exact pane. The task names "command detection"; the public README has no feature of that name.
Its long-running command cards are the closest match.

### 1.5 Platform facts *(gk)*

- iOS suspends a backgrounded app within seconds. A push is the only way to wake the user.
- A notification action without the foreground option runs the app in the background for a short
  time. An action marked "authentication required" makes the user unlock the device first.
- Notifications that carry the same collapse id replace each other on the device.
- Mirrored notifications show their actions on a paired Apple Watch.
- A Live Activity runs up to eight hours, then stays on the Lock Screen up to four more. Since
  iOS 17 its buttons run App Intents. Since iOS 17.2 a server can start one by push
  (push-to-start). iOS limits how often a Live Activity may update.
- An APNs auth key belongs to one Apple Developer team. It can push only to apps of that team.
- A Developer ID Mac app can receive APNs pushes with a matching provisioning profile.

---

## 2. Model

### 2.1 What the app holds

| Item | Where it comes from | Kept on device |
|---|---|---|
| Habs | Pairing (`transport.md` 6.1) | Device id, label, endpoints, pinned host key |
| Projects, sessions, panes, agent state | Each hab's event stream (`hab-logic-api.md` 3.5) | A cache, dropped on a new epoch |
| Chat messages | The hab's transcript reader (capability `transcripts`) | A cache of recent pages only |
| Terminal screens | Data streams (`hab-logic-api.md` 5) | Only while visible |
| Layout, focus, theme, keyboard bar | The app | Yes, per device |
| Device key | Made on first start | Secure Enclave (phone, iPad) or Keychain (Mac) |
| Push tokens | APNs | Sent to each hab that has push set up |

The hab owns structure. The app owns presentation (`hab-logic-api.md` 2.2). No layout, split or
focus is stored on a hab.

### 2.2 Words

| Word | Meaning here |
|---|---|
| status board | The list of every session on every hab |
| session view | One session: its chat view, terminal view and console |
| needs you | A session in `blocked` (any kind) or `error`, or `done` and not seen |
| answered elsewhere | Another client, or the TUI itself, resolved a request first |
| online, offline | Whether the app has a live control connection to a hab |

---

## 3. Status board

### 3.1 Behaviour

- **One list for every hab.** The board merges each hab's snapshot and events by host id. It never
  compares seq across habs (`hab-logic-api.md` 7.9).
- **Order by who needs you.** Groups, top to bottom:

| Rank | State | Why |
|---|---|---|
| 1 | `blocked` (permission, question, auth) | The agent waits for the owner |
| 2 | `error` | The turn stopped |
| 3 | `done`, not seen | A result waits to be read |
| 4 | `working` | Running; nothing to do |
| 5 | `idle`, `done` seen | At rest |
| 6 | `none`, `exited`, unfiled panes | No agent, or nothing live |

  Inside a group, the newest change comes first. A row moves only when its group changes, so the
  list does not jump while the owner reads it.
- **Grouping.** A switch groups rows by project or by hab. The order inside each group stays the
  same.
- **A row shows:** session name, project, hab label, harness, state with its kind, time in that
  state, and one line of context: the pending command for a permission, the question for a
  question, else the last agent message.
- **Seen.** Opening a session marks it seen. The hab turns `done` into `idle` when a client focuses
  the pane (`harness-adapters.md`, States). The app reports focus (13).
- **Badge.** The app icon badge counts sessions that need you, across all online habs.
- **Offline habs.** Their rows stay, dimmed, with "offline since" and the last known state. No
  action that needs the hab is offered.
- **Actions from a row:** open, answer an approval (7), start or resume the harness when the pane
  shows `none` or `exited`, close an exited session.

### 3.2 Edge cases

| Case | Behaviour |
|---|---|
| A hab goes offline while a session was `blocked` | The row stays in rank 1, dimmed, marked "offline". The answer buttons are off |
| A hab restarts (new epoch) | The app replaces that hab's rows with the new snapshot. Rows do not flash: the app swaps them after the snapshot arrives |
| Two habs hold the same project | One project group, rows labelled by hab. Project identity is the remote (`hab-logic-api.md` 2.2) |
| Clocks differ between habs | Durations come from the hab's own `at` stamps compared with the hab's own current time, not the phone's clock |
| A hab's API revision is too old or too new | The hab shows as "update needed" with the side to update (`hab-logic-api.md` 7.3). Its rows are hidden, not stale |
| A Codex pane ran a turn with no hook event | The row shows "no hook seen" and links to the trust step (`harness-adapters.md`, Edge cases) |
| Many sessions | The board shows needs-you rows first and folds idle rows per project after a few |
| An unfiled pane | Shown in rank 6 under its hab, so the owner can file or close it |

Sources: Heeler README (status order, host filter); rootshell README (unread when finished unseen,
group by project); Moshi docs, hooks (one row per session); `hab-logic-api.md` 2.2, 3.5, 7.3, 7.9;
`harness-adapters.md`, State event model.

---

## 4. Chat view

### 4.1 Behaviour

- **Source.** The hab reads the harness transcript and sends chat messages (capability
  `transcripts`). The app never reads a transcript file and never keeps a full copy.
- **Message kinds.** User text, agent text (Markdown), tool call, and session event (compaction,
  mode change, usage limit, harness restart). Unknown kinds render as a plain "unsupported message"
  line, never as an error.
- **Tool calls are cards.** Header: tool name, a short target (file, command), status (running,
  done, error, denied). Body: input and output, collapsed by default. Reads and searches fold to one
  line. Edits show a diff. Shell commands show the exact command.
- **Subagents** show as one card with their own nested messages, folded by default.
- **Paging.** The view opens at the tail. Older messages load in pages as the owner scrolls up.
- **Live updates.** New messages append while the session is visible. A running tool card updates
  in place.
- **Messages typed in the TUI** appear too, because the transcript holds them. The chat view and the
  terminal view show one conversation.

### 4.2 The composer

- The full system keyboard, with dictation and autocorrect. Multi-line text stays one message.
- **Send follows the hab's input rules** (`harness-adapters.md`, Input delivery):

| Pane state | What the owner sees |
|---|---|
| `idle`, `done` | Message sent; its status clears when the agent starts working |
| `working` | "Sends after this turn"; the owner can cancel it until then |
| `blocked` | Send is off. The pending request is shown above the composer instead |
| `none`, `exited` | Send is off. "Start" or "Resume" is offered |

- If the hab reports "not delivered" (no working signal and no new user record in a few seconds),
  the message shows "not delivered" with a retry button. The app never retries on its own.
- **Stop** sends the harness's interrupt through the adapter. It is a separate button, never a
  side effect of sending.

### 4.3 Edge cases

| Case | Behaviour |
|---|---|
| The transcript reader is degraded (format drift) | A banner says "chat view may be incomplete" and offers the terminal view. Messages the reader skipped are not guessed |
| The transcript file does not exist yet | The view shows "waiting for the first message", not an error |
| The harness has no chat reader | The session opens in the terminal view. The chat tab is hidden |
| A huge tool output | The card shows the first lines and a "show all" control. Full output loads on demand |
| The owner sends while another client is typing in the TUI | The hab holds delivery until typing pauses. The status stays "sending" |
| The hab restarts mid-turn | The view reloads from the transcript tail. Nothing is lost, because the transcript is the record |
| A message from another agent (channels) | Marked with its sender, so the owner can tell it from their own |
| A long session after reconnect | The app fetches the tail page again and drops its cache if the epoch changed |

Sources: Happy source, `packages/happy-app` (message kinds, tool cards, outgoing status); Heeler
README (composer); `harness-adapters.md` (Input delivery, Edge cases, Rules 4); `hab-logic-api.md`
3.4.

---

## 5. Terminal view

### 5.1 Behaviour

- **Renderer.** libghostty renders the pane on the device. The app opens one data stream per
  visible pane (`hab-logic-api.md` 5.1). The stream starts with a snapshot, then raw bytes.
- **Flow.** The app returns credit as it applies bytes (5.7). A hidden pane closes its stream.
- **Resume.** The app keeps the holder offset after the last byte it applied. On reconnect it sends
  that offset and gets the rest, or a fresh snapshot (5.4).
- **Size.** The PTY has one owner (6.1). The app never takes ownership by opening a pane. It takes
  ownership when the owner types into the terminal view (6.2), or presses "fit to this screen"
  (`pane.take`). Typing in the chat composer never changes ownership: that text goes through the
  adapter, not the terminal keyboard.
- **Not the owner.** The app draws the pane at the owner's size. On a narrow phone it scales down to
  fit the width, and pinch zooms with horizontal pan.
- **Keyboard bar.** Above the system keyboard: Esc, Tab, Ctrl, Alt, arrows, and keys the owner
  adds. Modifiers are sticky: one tap for one key, a double tap to lock. More rows open as a
  drawer. The bar is the owner's own, kept per device.
- **Hardware keyboard.** On iPad and Mac, keys go straight to the pane. App shortcuts use Cmd only,
  so no terminal key is lost.
- **Selection and links.** Long press selects. Detected links open in the browser.
- **Thumbnails.** The status board and the session switcher may show live thumbnails through
  observer streams (`observe: true`), which never take ownership (6.4). Only on iPad and Mac, and
  only while visible.

### 5.2 Edge cases

| Case | Behaviour |
|---|---|
| The phone types, then the Mac types | Ownership follows the 300 ms debounce. The PTY resizes once per change, not per key |
| The phone drops while owner | The pane keeps its size and has no owner (6.5). The Mac does not reflow |
| The pane exits | The view shows the exit status and offers "Close" or "Restart" |
| The screen is marked suspect after a hab crash | A "redraw" control sends `pane.redraw` in snapshot mode. The app never sends Ctrl+L |
| A full-screen program (alternate screen) | Rendered as is; the keyboard bar stays |
| A stream falls 4 MiB behind on a slow link | The hab sends a fresh snapshot. The app replaces its screen |
| Two clients type at once | Both inputs land, in order. A small marker shows who else is typing (`pane.activity`) |
| A program sends a terminal query | The hab answers it (5.6). The app never answers a query from its stream |

Sources: rootshell README (keyboard bar, sticky modifiers); Heeler README (tools keyboard);
`hab-logic-api.md` 5 and 6.

---

## 6. Console

### 6.1 Behaviour

- **A shell beside a session.** The console is a plain shell pane on the same hab, started in the
  session's working directory. It belongs to the session: closing the session asks whether to close
  the console too.
- **On demand.** The console starts when the owner first opens it. It lives on the hab, so it
  outlives the app and app updates.
- **Command cards.** When the shell emits prompt marks (OSC 133), the hab records each command:
  text, working directory, start, end, exit status and the output range. The console then shows a
  list of cards, newest at the bottom. A card folds its output, shows its exit status and duration,
  and expands on tap.
- **Back to raw.** When a program switches to the alternate screen, the console hands over to the
  terminal renderer until the program leaves it. Then the cards return.
- **Long commands.** A command still running after a threshold (default 30 s) gets a running badge.
  If it finishes while the console is not visible, its card is marked unread and its session shows
  a small badge on the board. A push for a finished command is a setting, off by default.
- **Input.** The console input line uses the system keyboard. Enter sends the line to the shell as
  typed text. The keyboard bar works as in the terminal view.

### 6.2 Edge cases

| Case | Behaviour |
|---|---|
| The shell emits no prompt marks | The console is a plain terminal view. A hint names the shell setup that adds marks |
| A mark is missing its end (the shell crashed, Ctrl+C mid-output) | The card closes at the next prompt mark, with status "unknown" |
| Nested SSH or a subshell without marks | Its output joins the current card until marks return |
| A command blocks on input (`sudo`, a prompt) | The card shows "waiting for input". The input line sends to it |
| Huge output | The card shows the last lines; "show all" opens the range in the terminal view |
| The console pane exits | The session keeps running. The console shows "shell exited" and "Start a new shell" |

Sources: rootshell README (long-running command cards, unread when finished unseen); the Part 1
plan (the hab finds prompt marks); OSC 133 marks as documented by terminal emulators.

---

## 7. Approvals (pending screens)

**Pending screens.** This section comes from Moshi's public docs and our adapter spec. The owner's
five Moshi screens (lock-screen approval, Live Activity before and after an answer, Watch, inbox,
turn complete) have not arrived. Stage 7 revises this section when they do. Items marked
**(screens)** wait on them.

### 7.1 Where an approval appears

| Surface | Shows | Answers |
|---|---|---|
| Chat view | The request as a footer on its tool card: tool, exact command or file list, working directory | All choices the harness offers |
| Status board | The row in rank 1 with the command | Allow, Deny |
| Push banner | Session, hab, tool, the command (shortened to fit) | Allow, Deny; a text reply for questions |
| Live Activity | The session's pending request | Allow, Deny |
| Apple Watch | The mirrored banner | Allow, Deny |

"Allow for this session" and edit-wide choices appear only in the chat view. Broad choices need the
full request on screen.

### 7.2 Flow

1. The adapter reports `blocked:permission` with a request id and the exact request
   (`harness-adapters.md`, Fields).
2. The hab updates every connected client and, if push is set up, sends a banner and a Live
   Activity update (8, 9).
3. The owner answers on any surface. The answer names the hab, the pane and the request id.
4. The hab checks that the request is still open and answers through the adapter
   (`harness-adapters.md`, Permission answers). A stale id is refused with `conflict`.
5. The hab reports the resolution. Every client clears the request. The hab withdraws the banner
   and the Live Activity buttons (7.3).
6. If the adapter sees no resolution signal after a keystroke answer, the hab marks the pane
   "needs you in the terminal", and the app opens the terminal view on tap.

### 7.3 Answered elsewhere

- An answer on any client, or in the TUI itself, resolves the request everywhere.
- The hab replaces the banner with a quiet one that says where it was answered (same collapse id,
  passive level). It updates the Live Activity without buttons. **(screens)**: confirm what Moshi
  shows after the answer.
- A late tap on a withdrawn button reaches the hab with a closed id. The hab refuses it. The app
  shows "already answered on <client>".

### 7.4 Authentication

- A lock-screen Allow or Deny always needs the device unlocked (the action is marked "authentication
  required"). An Allow can run a command, so it never runs from a locked phone.
- Face ID is a setting, off by default. When on, the app asks for Face ID before an approval from
  the app, and before it opens. **(screens)**: Moshi's docs do not say whether its banner actions
  ask for Face ID.
- The Secure Enclave key needs the device unlocked to sign (`transport.md` 7.2). An unlocked device
  is therefore enough for the background answer.

### 7.5 Questions

- `blocked:question` shows the question and its options as buttons, plus free text. On a banner,
  options become actions when they fit (up to the system limit); free text is a text reply action.
  Longer option sets open the app.

### 7.6 Edge cases

| Case | Behaviour |
|---|---|
| The phone cannot reach the hab when the owner taps Allow | The background action fails within its time. The app posts a local notification: "Could not reach <hab>. Open to retry." It does not retry on its own |
| The request is answered in the TUI before the push arrives | The hab sends the withdrawal (7.3). If the first push arrives late, its collapse id is already replaced |
| Two habs ask at once | Two banners, two board rows. Each answer names its own hab and request |
| The command is longer than the push allows | The banner shows the start and "open to see all". Allow from a banner needs the full command to fit within a set limit (default 256 characters); a longer command offers only "Open" |
| The hab restarts with a request open | The adapter rebuilds state (`harness-adapters.md`, Edge cases). The request id stays the harness's, so an answer still matches if the request is still open |
| The adapter state comes from screen text only | No answer button appears. The row says "needs you in the terminal" (`harness-adapters.md`, Rule 2) |
| Many approvals in a burst | One banner per request, grouped by session. The Live Activity shows the count and the oldest request |
| Push not set up | Approvals appear in the app only, while it is open |

Sources: Moshi docs, notifications, live-activity, apple-watch, hooks; Happy source (inline
permission footer and its choices); `harness-adapters.md` (Permission answers, Rule 2, Fields);
`transport.md` 7.2; platform facts in 1.5 *(gk)*.

---

## 8. Push

### 8.1 Path

The hab sends to APNs itself, with the user's own APNs key. No hub, no relay, no Flightdeck
service.

1. The user creates an APNs auth key in their Apple Developer account.
2. They put the key file path, key id, team id, the app's bundle id and the APNs environment in the
   hab's config file. The key file has owner-only permissions. Config holds a path, never the key.
3. The app registers for push and gets a device token. It sends the token to each paired hab over
   its authenticated connection, with the device id and the kinds it wants (13).
4. Each hab sends pushes for its own sessions only. No hab relays another hab's events, so no
   push arrives twice.
5. Revoking a device on a hab (`transport.md` 6.6) drops its push tokens too.

### 8.2 What pushes

| Event | Banner | Level | Live Activity |
|---|---|---|---|
| `blocked:permission` | Yes, with Allow and Deny | Time-sensitive | Update with buttons |
| `blocked:question` | Yes, with options or reply | Time-sensitive | Update |
| `blocked:auth` | Yes, "open to sign in" | Active | Update |
| `error` | Yes | Active | Update |
| `done` | No by default (setting) | Passive when on | Update to finished |
| `working` | No | — | Update, throttled |
| Request answered elsewhere | Replace, quiet | Passive | Update without buttons |
| A long console command finished | Setting, off by default | Passive | No |

### 8.3 Rules

- **Never drop an approval silently.** If the hab cannot decide whether to hold a push, it sends it.
- **Hold while watched.** If a client reports the session in focus in an active window within the
  last 60 s, the hab skips the banner for that session. The Live Activity still updates. (Open
  question 14.4.)
- **Collapse.** Each request uses its request id as the collapse id. A later push for the same
  request replaces the earlier one.
- **Small payloads.** A push carries the hab, session, pane, state, request id and a short text. The
  full request loads when the app opens. A push stays under the APNs size limit.
- **Privacy.** A push passes through Apple. In the first build its short text is plain. A setting
  "show commands in notifications" (default on) can hide the text and show only "approval needed".
  Sealing payloads end to end is a plan decision for later (14.9).

### 8.4 Not set up

- A hab with no push config reports no `push` capability. The app shows "Push is not set up on
  <hab>" in Settings, with a link to the setup guide.
- Everything else works: live updates and approvals while the app is open.

### 8.5 Edge cases

| Case | Behaviour |
|---|---|
| APNs says the token is no longer valid | The hab drops it and logs once. The app sends a new one when it next connects |
| APNs refuses the key (wrong team, revoked) | The hab reports "push broken" in its status. The app shows it in Settings |
| APNs rate-limits | The hab backs off. Approvals keep priority over Live Activity updates |
| The hab host is asleep or offline | No push. Nothing can push for a host that is not running |
| The owner has a phone and a Mac | Both register. The hold-while-watched rule (8.3) applies per session, not per device |
| The app is a TestFlight build signed by another team | The user's key cannot push to it (1.5). See 14.3 |
| A sandbox build and a production build | Each token is tagged with its environment. The hab uses the matching APNs host |

Sources: owner decision (each user brings their own APNs setup; fail gracefully without it); Moshi
docs, hooks and notifications (which events raise a banner, never drop an approval silently,
summary limits); Heeler README (push only when blocked or done); `transport.md` 6.6; platform facts
in 1.5 *(gk)*.

---

## 9. Live Activity

### 9.1 Behaviour

- **One activity tracks one session**, as in Moshi. The tracked session is the one the owner last
  sent a message to from this device, or the one that last needed them.
- **The tracked session's hab owns the activity.** The app gives that hab the activity's push token.
  Only that hab updates it, so two habs never overwrite each other.
- **Start.** The app starts the activity when the owner sends a message and leaves the app. A hab
  may start one by push-to-start when a session needs the owner and no activity runs.
- **Content.** Session name, hab, harness, state, time in state, and the latest event (tool
  running, approval needed, finished). Two styles: Detailed (with the last agent line) and Compact.
- **Buttons.** In `blocked:permission`, Allow and Deny. They go away as soon as the request is
  resolved anywhere (7.3).
- **Finish.** On `done` or `error` the activity shows the final state. No banner for `done` (8.2).
  It ends after about ten minutes, or when the owner opens the session.
- **Other sessions.** While one session is tracked, others that need the owner raise banners. The
  activity shows a count "2 more need you" when the tracked session's hab knows of them. It does not
  know about other habs.
- **Tap.** Opens the session. A setting picks the view: chat, terminal, or smart (approvals open the
  request, everything else the chat view).

### 9.2 Edge cases

| Case | Behaviour |
|---|---|
| The owner swipes the activity away | It ends. The next event that needs the owner may start a new one by push-to-start |
| iOS ends it after eight hours | The next event starts a new one if the session is still active |
| The tracked hab goes offline | The activity freezes on its last state. The app, when it next runs, marks it "hab offline" and ends it |
| Live Activities are off in iOS Settings | Settings shows a warning with a link. Banners still work |
| Update budget runs low | `working` updates drop first. Approval and finish updates go out at high priority |
| Push not set up | The app updates the activity only while it runs. The activity ends when the app is suspended, so it never shows stale state |

Sources: Moshi docs, live-activity (one session, styles, withdrawn buttons, ten-minute finish,
push-to-start, tap setting); Heeler README (Live Activity tracks agents live); platform facts in
1.5 *(gk)*.

---

## 10. Connection

### 10.1 Behaviour

The connection follows `transport.md`. This section says what the app does.

- **Pairing starts on the hab.** The owner runs `gff-hab pair` (or presses "Pair a device" in the
  Mac app on that host). The app scans the QR code, or takes the offer as text. It pins the hab's
  host key, presents the one-time token and its public key, and waits while the owner confirms on
  the hab (`transport.md` 6.1).
- **One QR per hab.** The app pairs with each hab on its own (`transport.md` 6.2).
- **Device key.** iPhone and iPad make a P-256 key in the Secure Enclave on first start. The Mac app
  keeps its key in the Keychain. Settings shows the key's fingerprint.
- **Endpoints.** A paired hab may list several endpoints: a LAN address, a tailnet address (useful
  only when a system VPN is on), a hostname. The app tries them in the hab's order, the next one a
  short time after the previous one, and keeps the first that completes. It never needs a VPN of its
  own.
- **One control connection per hab** while the app is visible. It subscribes to events with scope
  `host`. Data streams open per visible pane on the same SSH connection (`transport.md` 4.4).
- **The Mac app on its own host** uses the local socket. No pairing (`transport.md` 7.1). On other
  hosts it uses the system sshd with the user's keys, or pairs as the phone does.
- **Before the embedded server is on.** A hab without the embedded SSH server cannot pair. The app
  offers a fallback: it shows its public key to copy, and the owner adds it to the system sshd's
  authorized keys on that host. The app then runs `gff-hab connect` over SSH. (Open question 14.8.)

### 10.2 Reconnect

- On a lost connection, the app retries with backoff and jitter (first try at once, up to 30 s
  apart). A network change retries at once.
- It sends its reconnect token. Within 60 s and the same epoch, it keeps its client id and size
  ownership (`hab-logic-api.md` 3.7).
- A new epoch means a fresh snapshot (3.5). Data streams resume by offset.
- After three failed passes over every endpoint, the hab shows offline. While the app stays in the
  foreground, it keeps trying at the slowest interval.

### 10.3 Edge cases

| Case | Behaviour |
|---|---|
| The hab presents a different host key | The connection fails. The app shows both fingerprints and says to re-pair or accept on the hab. It never accepts on its own |
| The phone was revoked | Authentication fails. The app marks the hab "this device was removed" and offers to pair again |
| Away from home with no VPN | Habs on LAN addresses show offline. Push still arrives (APNs needs no route to the hab), but a lock-screen answer fails (7.6) |
| The pairing token expired | The hab refuses. The app says "run `gff-hab pair` again" |
| The revision does not fit | The app shows which side to update (`hab-logic-api.md` 7.3) |
| Many visible panes over the system sshd | Past nine panes, the app opens a second SSH connection (`transport.md` 7.6) |
| A hab rotates its host key | The app accepts the new key only when signed by the old one, within the grace period (`transport.md` 6.5) |
| A captive portal or a network that drops SSH | Every endpoint fails fast; the hab shows offline with "network blocks SSH" when the TCP connect succeeds but SSH never starts |

Sources: `transport.md` 4, 6, 7; `hab-logic-api.md` 3.5, 3.7, 4; Heeler README (several routes
per host, one long-lived event channel). Heeler's embedded overlay networks are a reference only;
the app does not embed one (owner rule).

---

## 11. Layout per device

The hab sends one structure. Each device presents it its own way. No layout reaches the hab.

| | iPhone | iPad | Mac |
|---|---|---|---|
| Home | Status board | Sidebar board plus session | Sidebar board plus session |
| Session | Chat first; a switch flips to terminal | Chat and terminal side by side, about 50/50 | Chat and terminal side by side, about 50/50 |
| Console | A sheet over the session | A drawer under the terminal | A drawer under the terminal |
| Divider | — | Draggable; snaps to 50/50, chat only, terminal only | Same as iPad |
| Keyboard | System keyboard plus keyboard bar | Hardware keyboard, bar when no hardware keyboard | Hardware keyboard |
| Thumbnails | No | Optional, on the board | Optional, on the board |
| Windows | One | Several windows (Stage Manager, Split View) | Several windows, one session each |

Rules:

- **Chat first on the phone.** A session opens in the chat view unless the harness has no chat
  reader. The terminal is one tap away.
- **The split is per device.** The iPad's divider position does not change the Mac's.
- **Narrow iPad windows** (Slide Over, a third of the screen) use the phone layout.
- **One session in two windows** is allowed. Each window opens its own data stream; only typing
  moves size ownership.
- **Tap targets** (push, Live Activity) open the right session in the right view on every device
  (rootshell's "back to the exact pane").

### 11.1 Edge cases

| Case | Behaviour |
|---|---|
| Rotating the phone | The chat view reflows. The terminal view rescales; it does not take ownership |
| An iPad window narrows past the threshold | Switches to the phone layout, keeping the current view |
| Mac app on the hab's own host and a remote hab | One board; local and remote rows look the same, labelled by hab |
| A session with no agent (a plain shell) | Opens in the terminal view on every device |

Sources: the task (chat first on the phone, about 50/50 on larger screens); `hab-logic-api.md` 2.2
(structure on the hab, presentation per client); Heeler README (iPad multiwindow); rootshell README
(tap returns to the exact pane).

---

## 12. Battery

### 12.1 Behaviour

- **Visible: one control connection per hab.** It carries events. Heartbeats run every 15 s each way
  (`hab-logic-api.md` 3.5).
- **Data streams only for what is on screen.** A pane scrolled away or behind a sheet closes its
  stream. Thumbnails stop when the board is hidden.
- **Background: disconnect.** When the app leaves the foreground, it closes every connection
  within a few seconds, cleanly, so the hab ends size ownership at once instead of after 45 s.
- **No background sockets, no background fetch, no silent-push wakes for updates.** Push is the only
  wake path. The one background job is a notification action (Allow, Deny, Reply), which connects,
  answers and disconnects.
- **Foreground: reconnect at once**, with the reconnect token if within 60 s.
- **Low Power Mode:** thumbnails off, chat `working` updates coalesced to one per second.
- **Mac app:** stays connected while any window is open. When every window is closed, it
  disconnects like the phone, unless the owner keeps it connected in Settings.

### 12.2 Edge cases

| Case | Behaviour |
|---|---|
| The app returns after a few seconds | The reconnect token keeps the client id and size ownership. The app re-reads state with the seq rule; data streams resume by offset |
| The app returns after an hour | New client id; fresh snapshot; data streams resume by offset or snapshot |
| A slow cellular link | Events have their own 8 MiB backlog limit; data streams have credit. A slow terminal never stalls the board |
| A Live Activity with no push | It ends when the app is suspended (9.2), so the app keeps no connection alive for it |
| A notification action takes too long | iOS ends it. The app posts the local failure notice (7.6) |

Sources: the task (one event stream per hab while visible, disconnect in the background, push as
the only wake path); `hab-logic-api.md` 3.5, 3.7, 5.7; platform facts in 1.5 *(gk)*.

---

## 13. Calls the app needs from the hab

These do not exist yet in `hab-logic-api.md`. Part 2 and Part 6 stage 7 add them. Names are
proposals.

| Call or field | Purpose |
|---|---|
| `push.register {device_id, token, environment, kinds}` | Store an APNs device token for this device |
| `push.register_activity {device_id, session, token}` | Store a Live Activity token for the tracked session |
| `push.unregister {device_id, token}` | Drop a token |
| capability `push` | The hab has push config that works |
| `session.focus {session, active}` | Report focus: marks `done` seen, and feeds hold-while-watched (8.3) |
| `approval.answer {pane, request, choice, text?}` | Answer a pending request by id; refuses a closed id with `conflict` |
| `session.interrupt {session}` | Stop the current turn through the adapter (the chat view's Stop button) |
| `chat.page {session, before?, limit}` | Read a page of chat messages from the transcript reader |
| events for chat | Chat messages appended or updated while subscribed |

---

## 14. Open questions

Each question has options and a recommendation. All are **Decision: open** for the owner.

### 14.1 Live Activity across several habs

| Option | For | Against |
|---|---|---|
| A. One activity, one session, owned by that session's hab | No two habs write one activity; matches Moshi | Other habs' sessions show only as banners |
| B. One activity per hab | Each hab owns its own | Several activities on the Lock Screen; noisy |
| C. One summary activity for all habs | One glance for everything | Two habs would overwrite each other's content; needs a hub or app-side merge, which a suspended app cannot do |

Recommendation: **A.** It works without a hub and matches the best reference.

### 14.2 Face ID and the device key

`transport.md` 4.3 says the phone's key is "gated by Face ID or the passcode". The owner decided
Face ID is optional and off by default.

Recommendation: the key needs the device unlocked (this device only, never in backups). It does not
need biometrics. The Face ID setting, when on, gates the app and in-app answers. Lock-screen actions
always need unlock. Decided (coordinator, following the owner's Face ID decision): `transport.md`
4.3, 7.2 and 7.3 now say the key is usable only while the device is unlocked.

### 14.3 Bring your own APNs key versus who signs the build

An APNs key pushes only to apps of its own team. The owner's TestFlight and Developer ID builds are
signed by the owner's team, so another user's key cannot push to them.

| Option | For | Against |
|---|---|---|
| A. Push works for whoever signs the build. Others build from source under their own team and bundle id | No shared service; matches "bring your own" | Push needs an Apple Developer account and a self-built app |
| B. The owner's builds accept pushes only from the owner's habs; others get live updates only | Nothing to build | Others never get lock-screen approvals from those builds |

Recommendation: **A, with B as its consequence.** Document plainly in the push setup guide that push
needs a build signed by the same team as the key. The app works without push, so the owner's builds
still serve others for live use.

### 14.4 Hold banners while watched

Recommendation: hold the banner when any client has the session in focus in an active window
within the last 60 s. Never hold an approval when the focus report is older than that. Moshi's
equivalent is off by default; ours is on because it keys on the session, not the whole machine.

### 14.5 A banner for `done`

Recommendation: off by default. The Live Activity shows it. A per-device setting turns it on, as a
passive banner.

### 14.6 How "answered elsewhere" withdraws a banner

| Option | For | Against |
|---|---|---|
| A. Replace by collapse id with a quiet "answered on X" | Works with the app suspended | A second, quiet notification stays |
| B. A background push that lets the app remove the banner | Clean | iOS throttles background pushes; not reliable |

Recommendation: **A**, plus B where iOS allows. Confirm against the owner's Moshi screens (7).

### 14.7 A lock-screen answer when the hab is unreachable

Recommendation: fail fast, post a local "could not reach" notice, never retry on its own. A stale
retry could approve a command the owner no longer wants.

### 14.8 Connecting before the embedded SSH server exists

Recommendation: support the fallback in Part 6 stage 4. The app shows its public key; the owner
adds it to the host's authorized keys by hand. This needs no new listener, and it is the path
`transport.md` 4.1 already opens. QR pairing replaces it once the embedded server is enabled.

### 14.9 Sealed push payloads

The plan already calls a `/consensus` on this. Recommendation for the first release: plain short
text through APNs, the "show commands in notifications" setting, and a payload shape that leaves
room for a sealed body later.

### 14.10 Apple Watch

Recommendation: mirrored notifications first (Allow and Deny work on the wrist with no Watch app).
A native Watch app with an inbox is a later stage.

---

## 15. Sources

Opened while writing this spec, 2026-10-31.

- Moshi docs: https://getmoshi.app/docs/live-activity, https://getmoshi.app/docs/notifications,
  https://getmoshi.app/docs/apple-watch, https://getmoshi.app/docs/hooks
- Happy repository (MIT): https://github.com/slopus/happy, app source in `packages/happy-app`
  (message view, tool view, permission footer); https://happy.engineering/docs/how-it-works
- Heeler repository (Apache-2.0): https://github.com/ZingerLittleBee/Heeler (README)
- rootshell repository (MIT): https://github.com/kitknox/rootshell (README)
- Our specs: `docs/specs/transport.md`, `docs/specs/hab-logic-api.md`,
  `docs/specs/harness-adapters.md`

Not opened: Apple's ActivityKit, UserNotifications and APNs pages. Facts marked *(gk)* need a check
against them, or a device test, in stage 7.

Internal inputs: the project's planning notes and research reports on native apps and
notifications. None of their private detail is reproduced here.
