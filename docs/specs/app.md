# Flightdeck app: behaviour spec

Status: draft for review. Part 6, study stage.

This spec says how the Flightdeck app behaves on iPhone, iPad and Mac. Later Part 6 stages build
from it: the status board, the chat view, the terminal view, the console, approvals, push, the
Live Activity, voice mode, usage, widgets and notifications on the Watch. It describes behaviour
in our own words. It copies no code from the reference apps.

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
- Flightdeck adds few confirmations, and few choices are settings (owner, 2026-11-01). One level
  picker (Safe, Balanced, Bypass permissions) sets the levelled ones at once; touching one shows
  Custom. Section 20 holds the rule, the levels and the registry. A new confirmation or setting
  joins that registry, or its dropped list.
- Settings also hold the Capcom host picker, which assigns and re-assigns the host that runs
  Capcom (`capcom.md` 7.3; owner, 2026-11-01).

Each section ends with edge cases and its sources. Facts marked *(gk)* come from general knowledge
of Apple platforms, not from a page opened for this spec. A device test must confirm them.

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [Status board](#3-status-board)
4. [Chat view](#4-chat-view)
5. [Terminal view](#5-terminal-view)
6. [Console](#6-console)
7. [Approvals](#7-approvals)
8. [Push](#8-push)
9. [Live Activity](#9-live-activity)
10. [Connection](#10-connection)
11. [Layout per device](#11-layout-per-device)
12. [Battery](#12-battery)
13. [Voice mode](#13-voice-mode)
14. [Usage](#14-usage)
15. [Widgets](#15-widgets)
16. [Notifications on Apple Watch](#16-notifications-on-apple-watch)
17. [Calls the app needs from the hab](#17-calls-the-app-needs-from-the-hab)
18. [Open questions](#18-open-questions)
19. [Sources](#19-sources)
20. [Settings and levels](#20-settings-and-levels)

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
- Its App Store screens add: an approval sheet with the command, project, host, model, context
  percentage and an expiry countdown; a board grouped as needs you, working and done, with live
  session cards and a connections list; a sidebar by project and branch; a marked-up screenshot
  sent in chat; a plan checklist in chat; terminal dictation; a usage sheet with 5-hour, 7-day and
  per-model windows, reset times and "alert me when free"; Home Screen usage widgets; a Watch app
  with approvals, an inbox and usage. Its store text promises on-device voice recognition and SSH
  keys in the Keychain behind Face ID.

What we take: approvals on the banner and in the Live Activity, buttons withdrawn when answered
elsewhere, no banner for a finished turn, one row per session, the tap-target setting, "never drop
an approval silently", and everything the App Store screens show (owner, 2026-10-31) except the
Watch app. What we leave: a hosted server in the push path, Mosh, and a Watch app (we write banners
for the Watch instead, 16).

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
| Usage readings | Each hab's adapters (14.1) | The latest reading per account, for widgets |
| Attachments | The owner's photos and files (4.4) | No. The hab keeps the copy it received; the original stays where it was |
| Device key | Made on first start | Secure Enclave (phone, iPad) or Keychain (Mac) |
| Imported SSH keys | The owner's own key files (10.4) | Keychain, this device only, never in backups |
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
- **A row shows:** a title, one line of context, then project, model and hab label, with the
  harness icon, the time in state and a state dot. The title is the pending request's title for a
  permission (7.2), the question for a question, else the session name. The context line is the
  pending command, the question's lead, or the last agent message ("Editing src/session/store.ts").
- **Section labels.** The board labels its groups in plain words: Needs you (ranks 1 to 3),
  Working, Done (rank 5), and Other.
- **Seen.** Opening a session marks it seen. The hab turns `done` into `idle` when a client focuses
  the pane (`harness-adapters.md`, States). The app reports focus (17).
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

### 3.3 Session cards and connections

On iPad and Mac the board has a second column beside the list.

- **Session cards.** A row of cards for the sessions active most recently (default four). Each card
  is a live, scaled preview of the session's pane through an observer stream (5.1), which never
  takes size ownership. Under the preview: the session title and project. On the preview: the hab
  label, and a badge for the route in use (local, LAN, VPN). Tap opens the session.
- **Connections.** A list of paired habs: label, `user@endpoint`, and a dot for online or offline.
  A count of online habs heads the list. Tap opens the hab: its projects, its endpoints, its
  device list, and "Pair a device".
- **New session.** A floating button starts a session: pick a hab, a project and a harness.
- On iPhone the board is the list alone. Cards and connections live behind tabs in the top bar
  (home, devices, network, settings).

| Case | Behaviour |
|---|---|
| A card's hab goes offline | The card freezes on its last frame, dimmed, with "offline since" |
| Low Power Mode or a hidden board | Cards stop their streams and show a still frame (12.1) |
| More active sessions than cards | The row scrolls sideways. Streams run only for cards on screen |
| A pane on the alternate screen (a full-screen program) | The card shows it as is |
| A session with no pane output yet | The card shows the harness's name and "starting" |

### 3.4 Context percentage

- Each agent session reports how full its context window is, as a percentage, from its adapter
  (`harness-adapters.md`). The hab sends it with the session's state.
- It shows as a small bar with the number on: the session's sidebar row, the approval sheet and
  lock-screen card (7.2), and the Live Activity.
- The bar turns amber at 80 per cent and red at 95 per cent. A compaction resets it; the chat view
  shows the compaction as a session event (4.1).

| Case | Behaviour |
|---|---|
| The adapter cannot read token counts | No bar. Never a guessed number |
| The model changed mid-session (a larger window) | The percentage uses the window of the model in use now |
| A session with no agent | No bar |

### 3.5 Sidebar

On iPad and Mac, inside a session, a sidebar lists every session grouped by project.

- **Project header:** project name, current branch, and how far it is ahead of or behind its
  upstream ("main ↑2"). The hab reads this from the repository in the project's folder.
- **Under each project:** its agent sessions (title, model, context percentage, state dot, time),
  then its plain shells and consoles by name ("dev server", "logs").
- Projects fold. The order inside follows the board's ranks.
- On iPhone the same list opens as a sheet from the session's title.

| Case | Behaviour |
|---|---|
| A project on two habs with different branches | Two headers, one per hab, each with its own branch |
| A detached HEAD or no upstream | The header shows the short commit, and no ahead count |
| A folder that is not a repository | The header shows the folder name only |

### 3.6 Search

- A search field heads the sidebar and the board. It filters as the owner types.
- **First build:** it matches names the app already holds: projects, branches, session titles,
  shells, habs, harnesses and models. It works offline, from the cache.
- **Later:** full-text search over chat messages, run on each hab by its transcript reader
  (`chat.search`, 17). Results show the message, its session and hab, and open the chat at it.

| Case | Behaviour |
|---|---|
| An offline hab | Its cached names still match, dimmed |
| No match | "No matches" and a hint to check the filter for project or hab |

### 3.7 Usage footer

The board's footer holds one bar per harness account in use: harness icon, name, and a bar for its
tightest window. Tap opens the usage sheet (14).

Sources: Moshi App Store screens (needs-you, working and done groups; session cards with live
previews; connections list; sidebar by project and branch with shells; context percentage on
rows; search field; usage bars in the footer); `hab-logic-api.md` 5, 6.4; `harness-adapters.md`.

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
- **Plans** render as a checklist (4.5), not as a tool card.
- **Paging.** The view opens at the tail. Older messages load in pages as the owner scrolls up.
- **Live updates.** New messages append while the session is visible. A running tool card updates
  in place.
- **Messages typed in the TUI** appear too, because the transcript holds them. The chat view and the
  terminal view show one conversation.

### 4.2 The composer

- The full system keyboard, with autocorrect. Multi-line text stays one message.
- **Controls:** an attach button (4.4), a button that opens the terminal view, a microphone for
  dictation (13.4), and Send.
- **Voice mode** opens from a long press on the microphone, or from the session's menu (13).
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

### 4.4 Images and files

The owner shows the agent something instead of describing it: a screenshot of a bug, a photo of a
whiteboard, a log file.

- **Attach.** The attach button offers: Photos, Camera, Files, and Paste. The share sheet from
  another app ("Send to Flightdeck") lands in the same composer, after the owner picks a session.
- **Mark up.** Before sending, an image opens in a markup view: draw, circle, arrow, text, crop.
  The markup is drawn into the image the agent receives. The original stays on the device only.
- **Preview.** Attachments sit above the text as thumbnails with a remove control. One message may
  carry several.
- **Send.** The app uploads each file to the session's hab over the authenticated connection
  (`attachment.put`, 17). The hab writes it into the project's workspace, in a folder for
  attachments that the hab excludes from version control with a local, untracked ignore rule. It
  never edits a tracked ignore file.
- **Handed to the harness.** The adapter delivers the message the way the harness accepts images:
  as an image input when the harness has a structured input for one, else as the file path in the
  message text, which every supported harness can read with its own file tools. Which path each
  harness takes is the adapter's choice (`harness-adapters.md`, Input delivery).
- **In the chat view** the attachment shows as a thumbnail on the owner's message. Tap opens it
  full screen. The thumbnail comes from the hab's copy, so the chat view on another device shows it
  too.
- **Privacy.** The app strips location and camera metadata from photos before upload, always
  (no setting, 20.6).
- **Clean-up.** The hab keeps attachments for a set time (default 30 days), then deletes them. A
  file the agent moved or committed is no longer in the folder, so it is not touched.

| Case | Behaviour |
|---|---|
| A file over the size limit (default 25 MB) | The app refuses it before upload and names the limit |
| A large photo | The app downscales it to the longest side the harness accepts (default 2048 px), as PNG for screenshots and JPEG for photos. No setting sends the original (20.6) |
| The upload breaks off | The message stays "not sent" with a retry. The hab discards partial files |
| The session is `working` | The message waits as "sends after this turn" (4.2); its files upload when it goes out (built choice below) |
| The harness accepts no images at all | Not built: no warning yet. The path still goes in the text, which every supported harness reads |
| A name clash in the folder | The hab prefixes the time and a short id; it never overwrites |
| The project is not a repository | The folder is still made; nothing to ignore |
| The hab's disk is full | The hab refuses the upload with a clear error; nothing partial stays |
| A file type the app cannot preview (an archive, a binary) | It sends as a file chip with name and size |

Built choices (task-mthck7r4):

- **Upload.** `attachment.put`, then `attachment.write` runs of at most 1 MiB in order, as
  `content.read` serves bytes; the last run checks the SHA-256 (`hab-logic-api.md`,
  `attachment.*`). One control message is at most 16 MiB, so a 25 MB file cannot go in one.
  The hab needs the `attachments` capability; an older hab gets "this hab cannot take files;
  update gff-hab" before any upload. The app uploads when the message goes out, so a queued
  message ("sends after this turn") uploads when the turn ends. A retry uploads only the
  files the hab does not hold yet. The row shows "Uploading 2 of 3, 40%". A put of the same
  file for the same session drops that session's earlier open upload of it, so retries never
  fill the open limit. A hab whose byte count stops growing fails the message ("upload ...
  stopped") instead of looping.
- **Where the hab stores it.** `<session folder>/.flightdeck/attachments/<id>-<name>`. The
  session folder is the one it started in (recorded at `session.create`), else its
  transcript's, else its project's most recent folder. A folder the hab cannot write in (a
  session started in `/`), a `.flightdeck` or `.flightdeck/attachments` that is a link (a
  cloned repository can commit one), or no folder at all, falls back to
  `attachments/<session>/` in the hab's state folder. The id is 12 hex digits. The file name
  keeps letters, digits, `.`, `-` and `_`; anything else becomes `-`, and it is cut to 100
  bytes with a short extension kept. The session folder part of the path may still hold
  spaces.
- **The ignore rule.** The hab writes `.flightdeck/.gitignore` holding `*` when it is missing:
  the folder ignores itself, including the ignore file. This is local and untracked, needs no
  git call, works in every worktree, and never edits a tracked file or `info/exclude`. An
  ignore file already there is left alone.
- **Clean-up.** The hab keeps an index in `attachments.json` in its state folder. It deletes
  a file 30 days after upload, checked at start and hourly, only when the file is still at its
  path with the stored size. A file that is gone is forgotten; one that cannot be checked
  (a disk that is not mounted) keeps its record. An upload with no write for 2 minutes is
  dropped with its partial file. Partial files left by a stopped hab go at the next start,
  including one a write still held when it stopped. At most 8 uploads are open at once. The
  hab keeps the last 64 thumbnails in memory and decodes no source over 24 M pixels.
- **Handed to the harness.** Each harness gets an image the way its prompt takes one, so the
  model receives a real image, not only a path (`harness-adapters.md`, Input delivery). Files
  that are not images go as paths after the text. A message may be files alone.
  - Claude Code (verified in the installed 2.1.316 bundle, its paste handler): it splits a
    paste into lines and reads each line that is one absolute `.png`, `.jpg`, `.jpeg`, `.gif`
    or `.webp` path as an image, after its read-permission check; the other lines stay text.
    So the hab sends one paste: the text, a blank line, one path per line. A path it may not
    read stays text. Inferred, not run: the transcript then holds the image blocks and an
    `[Image #N]` marker instead of the paths.
  - Codex (inferred from the installed 0.166 binary: its TUI logs `attach_image path=` and
    `pasted image size=`, and the open-source TUI attaches a paste that is one image path and
    nothing else): each image goes as its own paste, then the text, then Enter. A space,
    typed outside the pastes, follows each image, so a path Codex keeps as text stays apart
    from what follows. Not run against a live Codex.
  - opencode: one prompt through its server, the paths in the text; its read tool takes
    images. Its server takes file parts, but this build does not use them (not installed
    here to verify the shape).
- **Thumbnails in the chat.** `chat.page` and `chat.message` give each user message the ids
  of its files: the trailing paths when the harness kept them as text, else the send it came
  from, matched by its text once image markers (`[Image #N]`, `[image]`, Codex's
  `[image WxH FMT]`) and paths are taken out, within 10 minutes, the nearest in time. The hab
  keeps the last 500 sends with files for this. The text loses the markers. The chat view
  shows 96 pt thumbnails from `attachment.get` with `thumbnail` (a JPEG of at most 320 px from
  the hab). Tap opens the hab's copy full screen. A file with no thumbnail shows as a chip
  with its name and size; a file the hab no longer has says so. A thumbnail that failed for a
  dropped connection is tried again after 1, 3 and 8 seconds, then shows a Retry control.
- **Prepared on the phone.** An image is written again through ImageIO with no metadata, so
  location and camera data never leave the phone. The longest side is at most 2048 px. A PNG
  source (a screenshot) stays PNG; any other image becomes JPEG at quality 0.85, so a RAW or
  DNG picked in Files arrives as a 2048 px JPEG. Other files (a video, a PDF) go as they are,
  metadata included: the promise covers photos. Not built: the setting that keeps metadata,
  the setting that sends originals, and the warning for a harness that takes no images.
- **Attach menu.** Photos (the system picker, up to 10, needs no library permission), Files,
  and Paste (an image on the clipboard). Not built: Camera.
- **Mark up.** iPhone and iPad: tap a thumbnail in the composer. PencilKit's pens, marker,
  eraser, lasso and ruler draw over the image, laid exactly on its fitted frame. Done draws the
  marks into the image at full size; Cancel keeps it as it was. Not built: crop and text.
  The Mac has no markup (PencilKit's canvas is iOS only).
- **Open in Flightdeck.** The app takes any file from the share sheet or Files (the
  `public.data` and `public.text` registration of key import, 10.4). Only a private key file
  goes to key import: at most 64 KiB, and its first line that is not blank opens a private
  key block (`-----BEGIN ... PRIVATE KEY-----`); key import explains a broken or encrypted
  one. Any other file, a public key or a README that quotes a key included, opens the send
  sheet: the file with its size, a Capcom row when
  a hab offers the standing Capcom conversation (none does yet, so the row is hidden), then
  every session by host; offline sessions cannot be picked. Picking a session opens it with
  the file in the composer, where the owner can mark it up, add text and send. A file over
  25 MB shows the limit and offers no target.

### 4.5 Plan checklist

- When the agent writes or updates a plan (Claude's task list tool, Codex's plan update), the chat
  view draws it as a checklist card: one line per step, done steps ticked and struck through, the
  step in progress marked.
- A later update to the same plan changes the card in place, so the chat holds one live plan, not
  a copy per update. Earlier versions fold under it.
- The latest plan also shows as a small "3 of 5" chip in the session header. Tap scrolls to it.
- The adapter maps each harness's plan record to one shape: steps with text and status (pending,
  in progress, done).

| Case | Behaviour |
|---|---|
| A harness with no plan record | No card; plans appear as the agent's text |
| A plan with a step the agent dropped | The step leaves the card; the folded history keeps it |
| Two plans in one turn (a subagent's plan) | The subagent's plan shows inside its subagent card |
| A very long plan | The card shows the steps around the one in progress and "show all" |

Sources: Moshi App Store screens ("Show it, don't describe it": a marked-up screenshot sent in chat;
a plan checklist in chat); `harness-adapters.md` (Conversation, Input delivery); the
`transcripts` capability in `hab-logic-api.md`.


### 4.6 Built choices (task-6aflfy2k)

The chat view stage. Code in `apple/FlightdeckKit` (`Chat/`, `Views/ChatViews.swift`,
`Views/SessionViews.swift`).

- **One chat model per session.** `Fleet.chat(for:)` hands out a `ChatModel`, shared while a
  view holds it. The fleet passes it the hab's state on every change: online or not, the
  session, its pane and the pane's agent state.
- **Events.** The connection actor gives each listener the hab's `chat.message` events with
  their seq and a connection count. The model keeps only its session's messages. It replaces a
  message by id and appends a new id.
- **Tail and seq.** The view opens with `chat.page` for the tail (50 messages). Events that
  arrive while the page loads wait. After it, the model drops those with a seq at or below the
  page's seq on the same connection, and applies the rest. The rule stays after the page: an
  event and a page reply reach the model by separate paths, so an older event can arrive late.
- **Older pages** load with `before` when the top row appears. An unknown `before` takes the
  tail again. A page that returns after a tail reload or a reset is dropped.
- **Reconnect.** A return to online asks `after: cursor` until `more` is false, at most 20
  pages; past that it takes the tail. Its pages follow the seq rule per message: an event
  waits, and applies only when its seq is above the page that last carried its message. A
  `conflict`, an empty cursor or any error takes the tail again. A `reset` event voids pages in
  flight and takes the tail again. An event from an older connection is dropped.
- **New native session.** The model remembers the native session id its tail came from. When
  the agent state names another one (`/clear`, a new agent in the pane, a resume), it takes the
  tail again: the hab opens the new transcript only on a `chat.page`.
- **Scrolling.** New rows follow only while the owner is at the bottom. An older page keeps the
  row that was first in place. A first load or a replaced conversation goes to the bottom; a
  message the owner sends always shows.
- **Shared model.** Views count on `start` and `stop`; the listener stops when the last view
  leaves. Rows are kept in the model and rebuilt per changed message, so a long history does not
  parse its Markdown and tool input again on each event.
- **No reader yet.** `unavailable` from `chat.page` shows the hab's hint. The model tries again
  when the agent state names a native session or a harness. An empty page with an empty cursor
  shows "waiting for the first message".
- **The hab decides.** `session.send` holds the send gate (`harness-adapters.md`, built
  choices). The app shapes only the composer: `blocked` shows the pending request above it and
  turns send off; `none` and an exited session turn it off, and an exited session offers Resume
  (`session.resume`); offline turns it off. A pane with no agent state yet (after a hab restart,
  until the next hook) still sends, and the hab's answer shows.
- **After this turn.** The hab does not queue. While the agent works, the app keeps the message
  ("Sends after this turn", with Cancel) and sends one at the next `idle`, `done` or `error`. A
  `conflict` while the agent works puts the message back in that queue.
- **Refusals** show the hab's error kind and hint on the message ("conflict: the agent is
  blocked"), with Retry and Dismiss.
- **Send path.** The composer calls `session.send`. Only when the hab answers `unavailable` for
  a harness with no adapter (not Claude Code) and the agent is at its prompt does the app type
  through `pane.input` as the adapter would: the text in bracketed paste (any ESC removed), then
  Enter as its own call. A pane with no agent state never gets typed into.
- **Read back.** A sent message waits 5 s for a `working` state or a user record with the same
  text. Then it shows "not delivered" with Retry and Dismiss. The app never retries on its own.
  A matching user record removes the outgoing message, from an event or a tail page; the end of the turn removes one that
  `working` confirmed.
- **Stop** calls `session.interrupt`, else sends Escape through `pane.input`. It shows only
  while the agent works.
- **Rows.** `ChatRender.items` turns messages into rows: user, agent (Markdown blocks), tool
  card, event line, plan checklist, and "unsupported" for an unknown kind. A message whose
  `parent` is a tool call nests in that card. Only the latest top-level plan shows its steps;
  earlier plans fold to one line.
- **Markdown.** A small line parser splits blocks: paragraphs, headings, fenced code, list
  items (task boxes too), quotes and rules. Inline Markdown uses
  `AttributedString(markdown:)`. No third-party renderer.
- **Tool cards** are folded. Reads and searches fold to one line. Shell tools show the exact
  command, edits a removed and added diff, `Write` its content as added lines, Codex's
  `apply_patch` the changed lines of each file's diff, other tools their input as JSON. Output shows 12 lines with "show all"; a truncated output says the hab
  sent the first part (no call fetches more yet).
- **Banners.** Offline or reconnecting shows a banner and turns the composer off. An agent
  state with a version warning shows "chat view may be incomplete" with a Terminal button.
- **Layout.** Phone: a board row pushes the session; chat first, a segmented switch flips to
  the terminal. A session with no harness opens on the terminal. iPad and Mac: the board list
  stays on the left; the session fills the rest with chat and terminal at 50/50 under one header
  (harness, host, model). The divider does not drag yet.
- **Terminal slot.** The terminal half shows the pane's `TerminalScreen` (5), reached through
  the hab connection in the environment; a session with no pane shows a placeholder.
  `.terminalSlot { target in ... }` replaces the view. On the phone the switch shows one view at
  a time, so the terminal's data stream opens when it shows and closes when the chat returns.
- **Snapshot tests.** The tests read `internal/transcript/testdata/<harness>-*.chat.json` in
  place, so the app and the hab share one fixture set. Each renders to plain text
  (`ChatRender.text`) and is compared with `Tests/FlightdeckKitTests/Snapshots/`. On macOS each
  also renders through `ImageRenderer`, checked for size only: pixels change with fonts and OS
  versions. The Claude Code and Codex fixtures have snapshots; opencode has no reader or
  fixtures yet, and its files drop in by name.
- **Demo mode.** The fake hab serves `chat.page` (with `before` and a cursor), sends
  `chat.message`, and records `pane.input`. In demo mode six sessions hold invented
  conversations, and a sent message gets an invented reply.
- **Live check.** `LiveTests.testLiveChatSendAndReply` drives a Claude Code session through
  the same model. Runs on 2026-11-01, first by `pane.input`, then by the hab's `session.send`:
  the message reached the TUI, the user record and the reply came back as events, and
  `claude --resume` from a plain terminal recalled the turn.

### 4.7 Rich content (task-btkd1ekc)

The agent shows the owner something instead of describing it: a screenshot, a rendered
document, the files it changed.

**Study.** From public docs, release notes and general knowledge *(gk)*; nothing installed.

| Tool | Images | Documents | Diffs | Side panel | On a phone |
|---|---|---|---|---|---|
| Claude apps | Inline in the message; tap opens a full-size viewer | An artifact is a card in the chat; open shows it rendered, with versions, copy and download | Claude Code on desktop lists the changed files and shows a diff per file | Artifacts open in a panel to the right of the chat; the chat stays usable | The card opens the artifact full screen |
| ChatGPT apps | Generated images inline; tap opens a viewer with zoom and save | Canvas holds a document or code beside the chat | Codex tasks list changed files with `+n -m` counts and a unified diff per file | Canvas takes the right side; the chat narrows | Canvas and diffs open full screen; one thing at a time |
| Happy | Images the owner sends show as thumbnails | Tool cards only; no document viewer | Edit and Write tool cards draw a diff; a files screen lists git status and opens a file's diff | None; a mobile-first stack | Push to a files screen, then to one file |
| Paseo | Attachments in the composer | A sandboxed HTML preview (`landscape.md` 2.11) | A diff and PR panel per workspace (`landscape.md` 2.13) | The diff panel sits beside the agent on desktop | The panel becomes its own screen |

What carries over:

- **Inline stays small.** Every tool keeps the chat a column of cards. A big thing opens
  somewhere else: a panel on a wide screen, full screen on a phone.
- **Images inline, tap to zoom.** All four do it. Pinch and double tap zoom on a phone.
- **Documents fold.** A card with the title and the first lines; open renders the whole thing.
- **Changed files are a list first,** each with its change and counts, then a unified diff per
  file. Side-by-side diffs are a desktop extra; nobody uses them on a phone.
- **Paseo reads any path the daemon can read** (`landscape.md` 2.17). Our cheap guard: the hab
  reads only files a session referenced.

**Behaviour.**

- **Shown content.** The agent calls `content.show` (the agent kit wraps it later; today
  `gff-hab show` is its command line). The hab stores a reference to the file on its host: the
  path, a kind, a title and the time. It never copies the file into its own format. A show of
  the same path again replaces the reference and moves it to the end.
- **Derived content.** The hab also lists what the session touched without a show: images a
  tool call read, wrote or viewed (Claude `Read`/`Write` of an image, Codex `view_image` and
  `image_gen`), files the session wrote or edited (Claude `Write`, `Edit`, `MultiEdit`,
  `NotebookEdit`; Codex `apply_patch`), and the git status of the session's folder.
- **Inline in the chat.** An image shows under the tool call that touched it, or at the time it
  was shown. A shown document is a card: title, kind, the first lines (Markdown rendered).
  Tap expands it in place; a second control opens it full size (the panel on Mac and iPad, a
  sheet on iPhone). A PDF renders with PDFKit. HTML shows as text, or in a web view with
  JavaScript off.
- **Modified files.** One card in the chat lists them, folded, with each file's change. A file
  opens its diff.
- **Side panel.** On Mac and iPad a panel to the right of the session lists the session's
  documents, images and modified files. Selecting one shows it in the panel. On iPhone the same
  list opens as a sheet from the session header.
- **Offline host.** Content the app already holds (the last list and the bytes cache) still
  shows while its hab is offline. Anything else shows a plain placeholder ("Not available while
  <hab> is offline"), never an error, and loads when the hab returns. No setting (20.6). Not
  built: `ContentModel` checks for offline before its bytes cache, so cached files show the
  placeholder too.

| Case | Behaviour |
|---|---|
| The file was deleted or moved after the show | The card says "File no longer on the host"; the reference stays until the session ends |
| A file over the read limit (32 MiB) | The card names the size and offers nothing to load |
| A file that changes between chunks | The app starts the read again once; then shows what it has with "changed while loading" |
| A binary file the app cannot render | A file chip with name and size |
| A modified file outside a repository | The diff comes from the transcript's edit calls |
| A path in git status that leads out of the folder through a symlink | Left out of the list |

**Built choices.**

- **API.** `content.show`, `content.list`, `content.read`, `content.diff` and the event
  `content.updated`, under the capability `content` (`hab-logic-api.md` 3.9). An item's id is a
  hash of the session and the path, so a shown file that the session also modified is one item
  with both marks.
- **Which session.** `content.show` takes `session`, or `pane_token`: the token in the calling
  pane's `GFF_HAB`, which the hab maps to the pane and then to its session. `gff-hab show` sends
  the token when `-session` is absent, so an agent needs no id.
- **The folder.** The session's folder is the `cwd` its transcript records (Claude Code's
  records, Codex's session meta), else the last one it recorded (kept in `content.json`, since
  a restarted hab has no agent state until the next hook), else the most recently used folder
  of its project.
- **Reads are bounded.** `content.read` serves only ids `content.list` would return for that
  session; any other id is `not_found`. Chunks are at most 1 MiB (default 256 KiB), files at most
  32 MiB. The hab checks the path again before each chunk and refuses one that is not a
  regular file (a device reacts to an open), then checks the open file again. Each chunk
  carries the file's size and modification time, so the app sees a change. A shown path is
  stored with its symlinks resolved, so it is the same item as a git status entry for the file.
  This guard is cheap, not a boundary: any control client can show any path, and could start a
  shell anyway.
- **Git status paths** are resolved through symlinks and must stay inside the repository's top
  folder; one that leads out is left out, and one swapped for such a symlink after the list is
  refused on read. Paths from tool calls have no folder bound: a file the agent wrote or edited
  anywhere is listed. A write that failed or was denied changed nothing, so its path is listed
  only when it is an image (as a read). The list holds at most 500 items; past that, items only
  git status lists go first, then the oldest.
- **Diffs.** A tracked file: `git diff HEAD -- <path>` with the same restricted environment as
  project git status. An untracked or added file: every line added. Outside a repository: the
  transcript's edit calls in order, as hunks. At most 512 KiB, then `truncated`.
- **Events.** The hab sends `content.updated` for a show. Derived items change with tool calls;
  the app lists again, debounced, after a tool `chat.message` that is done.
- **Stored references** live in the state directory (`content.json`), at most 200 per session,
  and go when the session record goes (`session.deleted`). The file is synced before its
  rename. A show and a folder change write before they answer. A record that goes drops the
  references at once and leaves the write to one background writer, since it happens inside the
  structure store's event callback; each write takes the store's latest state and skips one
  older than the file, so writes never land out of order, and the hab's shutdown writes what is
  left. A failed or crash-lost background write is not tried again: the prune at the next start
  drops every session whose record is gone, so no retry loop is needed. A file that does not parse is moved to `content.json.bad` and the hab starts with no
  references, with a log line: a store of references never stops the hab.
- **App.** `ContentModel` per session merges `content.list` with `content.updated` by id, keeps
  the last list while offline (and marks it), and loads bytes on demand in chunks. A list reply
  older than one already applied is dropped. `DiffParser` splits a unified diff into files,
  hunks and lines.
- **The hab's call limit.** The hab runs at most 4 content calls per connection. The app sends
  `content.read` and `content.diff` through 3 slots per connection, shared by every session's
  cards, which leaves room for `content.list`. Views that ask for the same file version at once
  share one load. A busy answer is tried again 3 times, with a wait from 200 ms that doubles.
  Busy is an `unavailable` whose hint says "already run on this connection", the hab's
  per-connection limit; any other `unavailable` (a hab with no content service, a file it could
  not read) fails at once. Known limit: busy is told by the hint's words. The next API revision
  should add an additive busy field to the error data, set only by the limit; the app would read
  it first and keep the phrase for older habs. A load that still fails says "Could not load this file"; the hab's hint stays out
  of the card.
- **Bytes cache.** Keyed by item and file version (size and modification time, from the list),
  at most 64 MiB per open chat, the least recently used out first. A list that names a new
  version drops the old bytes.
- **Rendering.** Images decode once, off the main thread, with ImageIO thumbnails: at most 1080
  px on the long side inline, 2048 in a document view, 4096 zoomed. Decoded images are kept by
  bitmap bytes: a 32nd of the device's memory, from 64 to 256 MiB (128 MiB on a 4 GiB phone, two
  zoomed images and the inline ones), and at most 24 of them; the least recently used goes
  first, and the newest always stays. Every open chat shares this one cache (keys name the hab
  and the session, since item ids are per hab), and it empties when the system reports memory
  pressure.
  Text shows its first 256 KiB, cut at a line end, with "Show all", which renders the file as a
  lazy list of 200-line runs. Markdown renders its first 256 KiB; past that a line under it says
  "Showing the first 256 KB of <size>" with "Show all as text", the same lazy list (a whole
  large file as Markdown would stall the view). Every content size label (chip, too large, cut
  line, "Show all") uses binary units, so one file reads the same size everywhere. Markdown, a diff file's parse and a text file's runs are made
  once per file version (the list's version and the byte count); a redraw reuses them. The
  version is the one the shown bytes were loaded for, not the list's newest: while a new version
  loads, the old bytes stay on screen under the old key. Bytes that changed while loading get a
  key of their own. A PDF view loads a new
  document only when its bytes change, so a redraw keeps its scroll position and zoom.

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
- **Dictation.** A microphone key on the bar starts on-device dictation (13.4). A strip replaces the
  bar while it listens: a level meter, "Dictating…", and a stop button. The text goes to the pane
  as one bracketed paste when the owner stops. It never presses Enter on its own.
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

### 5.3 Built choices (task-29y4k1ih)

The terminal view and its data-stream client. Code in `apple/FlightdeckKit/Sources/FlightdeckKit/Terminal`.

- **A Swift data-stream client** (app lead decision). The app reads data streams in Swift, over the
  same `Transport` as its control connection. It does not use the Go library built for Apple
  platforms: the app already speaks the control protocol in Swift, and this keeps the Go runtime
  and gomobile out of the iOS app. Swift's libghostty is the only parser the app carries.
- **libghostty-spm**, `GhosttyTerminal` product, pinned exactly to 1.6.20260909 (commit
  7e45d271, Ghostty 82938b633ba6), as Heeler and herdrm pin it. The version scheme has a date, so
  a range would drift; each bump needs a review of the binary checksum. Its in-memory backend
  takes the hab's bytes and hands back what the terminal writes.
- **`PaneStream`**, one per open terminal view: `data.ticket`, then `transport.open(.data)` (a
  socket connection, or one more `ssh <host> gff-hab connect` process), then a hello with the held
  offset. Frames follow `hab-logic-api.md` 5.3. The consumer applies each frame before the next is
  read, and credit goes back for applied bytes once 16 KiB is owed, so a slow view slows the hab.
- **Resume.** The stream keeps the holder offset after the last applied byte. A dropped data
  connection or a reject other than `not_found` opens a new connection with a new ticket and that
  offset, after 0.25 s doubling to 5 s. A frame that starts above the held offset is a removed
  query; one that starts below is trimmed and counted (the hab never sends one). `not_found`
  ends the stream ("This pane is gone"); `exited` ends it with the exit status and a Close button.
  Proof: a scripted byte stream with four drops ends with the exact output, one snapshot and
  nothing trimmed (`PaneStreamTests`), and the same over a real hab (`LiveTerminalTests`).
- **Snapshot.** The view resets libghostty (RIS, then the scrollback cleared) and feeds the
  `snapshot-vt` bytes. One surface lives as long as the view. The hab sends a resync's snapshot
  before its resize frame; when a resize frame changes the size before any bytes follow, the view
  draws the snapshot again at the new size.
- **Hidden and shown again.** A view that disappears (a push on top, a tab switch) closes its
  stream. When it comes back it opens a new one from the offset it applied: its screen is kept,
  so the hab sends only what was missed, or a snapshot.
- **Background.** The fleet keeps the open terminal views (`TerminalRegistry`). When the app goes
  to the background it closes their data connections before the control connections, and no
  stream retries; in the foreground the streams that were open resume by offset.
- **Grid equals the PTY.** The surface is laid out at the pane's size in cells, plus half a cell
  so rounding never loses a column, then scaled to fit the view. Output waits until libghostty
  reports that grid and it has held still for 100 ms: libghostty reports a new grid before its
  parser has resized. More than 4 MiB of held output is dropped and a `pane.redraw` snapshot asked
  for instead.
- **Size rule.** The view's space only gives the size this client wants. It goes out with
  `pane.set_desired_size` only while this client owns the pane, 120 ms after the last change. The
  view reads its client id again on every welcome and resize frame, since a control reconnect
  without the token gives it a new one.
  Typing takes ownership through the hab's 300 ms debounce, so the app sends nothing extra
  before it. Viewing, rotating and window resizes never send a size. "Fit to this screen" sends
  `pane.take`. Pinch zoom with horizontal pan is not built yet.
- **Input.** What libghostty writes goes to `pane.input` through one pump: one call in flight,
  keys typed meanwhile sent together in the next call, in order, at most 1 MiB per call.
  libghostty's replies to queries the hab leaves in the stream are dropped: OSC 52, colour and DCS
  replies, device attributes, mode, keyboard flag, device status and size reports. A cursor
  position report looks like a modified F3, so it passes only from the owner.
- **Viewing never takes the size.** Any input from a client that does not own the pane takes the
  size after the debounce (`hab-logic-api.md` 6.2). So while this client does not own the pane,
  mouse motion, the mouse wheel and focus reports are dropped. Clicks and keys pass: they are meant
  as input. A wheel in a viewed mouse-mode program therefore does nothing until this client types or
  presses "Fit to this screen".
- **Keyboard.** Hardware keys reach libghostty's view directly on iPad and Mac; the app adds no
  key shortcuts in the terminal. The phone uses libghostty-spm's keyboard bar: Esc, Tab, Ctrl and
  Alt (one tap for one key, a double tap locks), arrows, a few symbols and Paste. Owner-added keys,
  the drawer and the dictation key are not built yet.
- **Redraw** sends `pane.redraw` with mode `nudge`: a fresh snapshot, then the PTY one column
  narrower and back. Never Ctrl+L.
- **One dark scheme** for the terminal, so the space around a scaled pane matches it.
- **Route.** For now the session sheet from a board row has a "Terminal" link, through
  `EnvironmentValues.habConnections`. `TerminalScreen(pane:connection:)` is self-contained, for
  the session view to host beside the chat. Debug builds open a pane at launch with
  `-FlightdeckOpenPane <pane>`.
- **Demo mode.** The fake hab serves tickets, data connections, input, ownership and redraw.
  Every demo pane has an invented shell screen that echoes typing; the dev server pane prints a
  request log every 2 s.
- **Not built:** selection and links, exit "Restart", `pane.activity` markers, thumbnails, more
  scrollback on demand, and closing and resuming data streams when the control connection comes
  back.

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

### 6.3 Built choices (task-3x22hva9)

Code in `apple/FlightdeckKit/Sources/FlightdeckKit/Console`.

- **The console is a session.** The session view's Console button finds or starts a session
  named `<session> console`: `session.create` with the session's project and no argv, so the
  hab starts its `$SHELL`.
- **Its folder** (app lead's call, fix round): the app sends `cwd` = the folder of the session
  pane's newest command (`command.list` limit 1, `Command.cwd`) when the shell reported one.
  The API exposes no spawn folder of a session, so there is no second choice. Otherwise, or when
  that folder is gone (`invalid`, retried once without `cwd`), the hab picks: the project's
  most recently used folder that still exists, else its home (`hab-logic-api.md` 3.9). An agent
  session sends no marks, so its console starts in the project's last folder, which can be
  another worktree of the same repository.
- **Reuse.** A running console of that name is reused, from any device, even before its pane's
  event arrived; a running one is never closed. A running one with no pane yet answers "still
  starting". Only a console that no longer runs has its record removed (`session.close`) so the
  name is free. Two opens at once: the second `session.create` meets `conflict`, and the app
  takes the console the first one started. The link from a console to its session is the name
  only: a renamed session gets a new console, and closing a session does not yet ask about its
  console. A console's own session view has no Console button.
- **Where it shows.** A sheet over the session on every device, for now. The drawer under the
  terminal on iPad and Mac (11) is not built.
- **Cards.** `command.list` (newest page, then older pages on demand) and `command.updated`
  events, through a tap on the control connection like the chat's. A card shows the command
  line, its folder, the exit status ("Exit 0", red when not 0; "Ended" when no status came) and
  the duration; a running card shows a spinner and the time so far. The list reads again when
  an event says `dropped`, or when the control connection's generation changed (checked every
  2 s), since events have no replay.
- **The folder** comes from the hab: the shell's OSC 7 report before the command
  (`Command.cwd`, `hab-logic-api.md` 2.5). A shell that sends no OSC 7 shows no folder.
- **Output** is read on first open, at most the last 256 KiB of the range, and shown as plain
  text: escape sequences dropped, carriage return, backspace, tab and erase-to-end-of-line
  applied on the line, trailing blank lines dropped, at most the last 500 lines, with "Earlier
  output not shown" when cut. What a program drew on the alternate screen is left out. An open
  card of a running command reads on every 2 s from where its last read stopped, keeping the
  last 256 KiB; when it ends, the card reads its whole range again. A read that comes back
  after the command went on or ended is dropped and made again.
- **Alternate screen.** The console keeps the pane's terminal model running while it is open.
  The model follows `CSI ?1049h`, `?1047h`, `?47h` and their `l` forms, and RIS, in its stream
  and snapshots (`ScreenModeScanner`). While the alternate screen is on, the view shows the
  terminal and the Cards/Terminal switch is locked; when it ends, the cards return. The owner can
  also choose the terminal by hand.
- **No marks.** With no command yet, the console says commands show once the shell sends prompt
  marks, and offers "Shell setup" (the zsh and bash lines, `ConsoleShellSetup`: OSC 133 A to D
  and OSC 7, each encoded byte by byte; bash 4.4 or newer) and "Show terminal". A prompt
  theme that rebuilds the prompt on every command, or a bash `PROMPT_COMMAND` array, drops
  the marks; the sheet says so. The app cannot tell "no marks" from "no command yet": the
  hab's API does not say whether a pane ever sent a mark. The shipped zsh lines leave
  `PROMPT_SP` on; its mark lands in the output before `D`, and the card text drops the blank
  line it leaves.
- **Input line.** Enter sends the line and a carriage return through `pane.input`, as typed
  text, with control characters other than tab dropped. Pasted text with a newline goes as one
  bracketed paste, so the shell does not run each line on its own. A stop button sends Ctrl+C.
  Inputs go one call at a time, in the order given: a Ctrl+C never overtakes the line it should
  stop. When a call fails, the inputs queued behind it are dropped with that one error, rather
  than each waiting out its own timeout on a dead link; input given after the failure goes
  again. Closing the console drops what is still queued. Typing in the console takes the pane's size like any input
  (5.3). The keyboard bar is the terminal view's, only while the terminal shows.
- **Quick actions per host: none.** The hab serves no catalog yet (`mesh.md` 5, `catalog.get` is
  not built), so there are no declared facts to read. The console shows no quick actions until
  it does.
- **Tests.** `cmd/gff-hab/testdata/console/basic.marks.json` is a hand-written marked-shell
  stream; `TestConsoleFixtures` feeds it through the hab's tracker into `basic.commands.json`,
  and `ConsoleTests` renders the cards from it (`Snapshots/console-basic.txt`). `LiveConsoleTests`
  (with `FLIGHTDECK_LIVE_SOCKET`) runs `ls`, `false`, `sleep 2` and `vim` in a real console.
  Debug builds open a session's console at launch with `-FlightdeckOpenConsole <session>`.
- **Not built:** the running badge's 30 s threshold and unread marks, the board badge, the push
  for a finished command, "waiting for input", "show all" in the terminal view, closing a
  console with its session, demo mode for the console, and a nonce for marks (so nothing acts on
  a card's text or exit).

---

## 7. Approvals

This section comes from Moshi's public docs, nine screens from Moshi's App Store page, and our
adapter spec. The screens show the approval sheet, the lock-screen card and Moshi's Watch app.
We have no Watch app, so the Watch gets banners written for it (16). The screens do not show the
Live Activity after an answer, so 7.5 keeps the behaviour from Moshi's docs.

### 7.1 Where an approval appears

| Surface | Shows | Answers |
|---|---|---|
| Chat view | The request as a footer on its tool card: tool, exact command or file list, working directory | All choices the harness offers |
| Status board | The row in rank 1 with its title and command | Tap opens the approval sheet |
| Approval sheet | Everything in 7.2 | Allow, Deny; "Open chat" for broader choices |
| Push banner and lock screen | Project, model, context bar, title, the command (shortened to fit) | Allow, Deny; a text reply for questions |
| Live Activity | The session's pending request | Allow, Deny |
| Apple Watch | The mirrored banner, written for the Watch (16) | Approve, Deny; options for questions |

"Allow for this session" and edit-wide choices appear only in the chat view. Broad choices need the
full request on screen.

### 7.2 The approval sheet

A sheet rises over the board when the owner taps a rank 1 row, a banner, or the Live Activity. It
holds one request. Top to bottom:

- **Header line:** harness, the request kind (permission, question, sign-in), and the session's
  context bar with its percentage (3.4).
- **Title:** a short question. It is the harness's own description of the call when the call has
  one (Claude's shell tool carries a one-line description). Otherwise it is built from the tool:
  "Run a command?", "Edit 3 files?". The app never asks a model to write the title.
- **Facts line:** project, hab, model, and the time the request opened.
- **The request:** the exact command in a monospaced block, or the file list with a diff link. It
  never truncates in the sheet; a long command scrolls.
- **Context line:** the agent's last message before the request, one or two lines, when the
  transcript has one. It tells the owner why the agent asks.
- **Expiry:** "Expires in 4m 12s", counting down (7.4).
- **Buttons:** Deny and Allow, equal size, Allow on the right. A chat icon opens the session at the
  request, where the broader choices live.

After an answer the sheet closes and the row leaves rank 1. If the request resolves elsewhere while
the sheet is open, the buttons turn off and the sheet says where it was answered.

The lock-screen card holds the same facts in less room: project and model on the first line with
the context bar, the title, then the command on one line in the accent colour, then Deny and Allow.

### 7.3 Flow

1. The adapter reports `blocked:permission` with a request id and the exact request
   (`harness-adapters.md`, Fields).
2. The hab updates every connected client and, if push is set up, sends a banner and a Live
   Activity update (8, 9).
3. The owner answers on any surface. The answer names the hab, the pane and the request id.
4. The hab checks that the request is still open and answers through the adapter
   (`harness-adapters.md`, Permission answers). A stale id is refused with `conflict`.
5. The hab reports the resolution. Every client clears the request. The hab withdraws the banner
   and the Live Activity buttons (7.5).
6. If the adapter sees no resolution signal after a keystroke answer, the hab marks the pane
   "needs you in the terminal", and the app opens the terminal view on tap.

### 7.4 Expiry

- Each open request carries an `expires_at` from the hab. It is the earlier of two times: the
  answer path's own deadline, and the hab's limit for remote answers.
- **The answer path's deadline.** With a blocking permission hook, the hook gives up after its
  timeout and the harness falls back to its own dialog (`harness-adapters.md`, Open decisions:
  a per-host setting, off by default, about 30 s).
  After that, only a keystroke answer or the terminal can resolve it.
- **The hab's limit.** A setting on the hab (`approval.max_age`, 20.3), default 60 minutes, none
  at Bypass permissions. A request older than this takes no remote Allow. The owner must open
  the session and answer there. A stale approval is easy to grant by habit, and the context may
  have changed.
- When the countdown reaches zero, Allow and Deny turn off on every surface. The card says
  "Expired: answer in the terminal" and opens the terminal view on tap. The hab refuses a late
  answer with `conflict`.
- When neither time applies (a keystroke answer path, and no hab limit: `approval.max_age` 0),
  no countdown shows.
- The countdown runs on the device from `expires_at`. The hab's clock decides; the app corrects for
  the offset it measured on connect (3.2).

### 7.5 Answered elsewhere

- An answer on any client, or in the TUI itself, resolves the request everywhere.
- The hab replaces the banner with a quiet one that says where it was answered (same collapse id,
  passive level). It updates the Live Activity without buttons. Moshi's docs say its buttons go
  away at once; the screens do not show the state after an answer.
- A late tap on a withdrawn button reaches the hab with a closed id. The hab refuses it. The app
  shows "already answered on <client>".

### 7.6 Authentication

- A lock-screen Allow or Deny always needs the device unlocked (the action is marked "authentication
  required"). An Allow can run a command, so it never runs from a locked phone.
- The app adds no Face ID gate of its own to approvals or to opening the app. Face ID is one
  setting on the SSH keys, "Require Face ID for SSH keys" (`ssh.require_face_id`, off by
  default; 10.7, 20.3). Moshi's store text puts Face ID on its SSH keys, not on approvals.
  The device passcode guards the rest (20.6).
- The device key needs the device unlocked to sign (`transport.md` 7.2). An unlocked device is
  therefore enough for the background answer.
- A Watch answer needs the Watch on the wrist and unlocked (16.1).

### 7.7 Questions

- `blocked:question` shows the question and its options as buttons, plus free text. On a banner,
  options become actions when they fit (up to the system limit); free text is a text reply action.
  Longer option sets open the app.

### 7.8 Edge cases

| Case | Behaviour |
|---|---|
| The phone cannot reach the hab when the owner taps Allow | The background action fails within its time. The app posts a local notification: "Could not reach <hab>. Open to retry." It does not retry on its own |
| The request is answered in the TUI before the push arrives | The hab sends the withdrawal (7.5). If the first push arrives late, its collapse id is already replaced |
| Two habs ask at once | Two banners, two board rows. Each answer names its own hab and request |
| The command is longer than the banner allows | The banner shows the start and "…". Approve from a banner needs the full command to fit the Watch body budget after safe shortening (16.1); a longer command offers only Deny and "Open" |
| The hab restarts with a request open | The adapter rebuilds state (`harness-adapters.md`, Edge cases). The request id stays the harness's, so an answer still matches if the request is still open |
| The adapter state comes from screen text only | No answer button appears. The row says "needs you in the terminal" (`harness-adapters.md`, Rule 2) |
| Many approvals in a burst | One banner per request, grouped by session. The Live Activity shows the count and the oldest request |
| Push not set up | Approvals appear in the app only, while it is open |
| The request expires while the sheet is open | The buttons turn off at zero. The sheet says "Expired: answer in the terminal" and offers to open it |
| The phone's clock is wrong | The countdown will use the offset from the hab's clock measured at hello (14.6); not built: only usage reads it today. A banner shows the expiry as text from the hab, not a live timer |
| The call has no description and an unknown tool | The title is "Allow <tool name>?". The request block shows the raw input |
| The context percentage is unknown | The context bar is hidden, not shown as zero |
| The agent's last message is very long | The context line shows its first two lines; the chat icon opens the rest |
| A burst of approvals on the sheet | The sheet shows the oldest first, with "1 of 3". Answering moves to the next |

Sources: Moshi App Store screens (approval sheet with command, project, host, model, context
percentage and expiry; lock-screen card; Watch approval); Moshi docs, notifications,
live-activity, apple-watch, hooks; Happy source (inline permission footer and its
choices); `harness-adapters.md` (Permission answers, Rule 2, Fields); `transport.md` 7.2;
platform facts in 1.5 *(gk)*.

---

## 8. Push

### 8.1 Path

The hab sends to APNs itself, with the user's own APNs key. No hub, no relay, no Flightdeck
service.

1. The user creates an APNs auth key in their Apple Developer account.
2. They put the key file path, key id, team id, the app's bundle id and the APNs environment in the
   hab's config file. The key file has owner-only permissions. Config holds a path, never the key.
3. The app registers for push and gets a device token. It sends the token to each paired hab over
   its authenticated connection, with the device id and the kinds it wants (17).
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
  last 60 s, the hab skips the banner for that session. The Live Activity still updates. (Decided in
  18.4.)
- **Collapse.** Each request uses its request id as the collapse id. A later push for the same
  request replaces the earlier one.
- **Small payloads.** A push carries the hab, session, pane, state, request id and a short text. The
  full request loads when the app opens. A push stays under the APNs size limit.
- **Privacy.** A push passes through Apple. In the first build its short text is plain. A setting
  "show commands in notifications" (default on) can hide the text and show only "approval needed".
  Sealing payloads end to end is a plan decision for later (18.9).

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
| The app is a TestFlight build signed by another team | The user's key cannot push to it (1.5). See 18.3 |
| A sandbox build and a production build | Each token is tagged with its environment. The hab uses the matching APNs host |

### 8.6 Built choices (task-oe7ioczy, hab side)

The hab half is built: `internal/push` and `cmd/gff-hab/push.go`. The setup guide is
`docs/push.md`; the full list is in `hab-logic-api.md` 3.9, Push. Settled behaviour the app
relies on:

- **Presence tiers.** No banner while a connected client has the session in focus (reported
  within 60 s). No banner to a device whose app reported itself in the foreground within 180 s;
  the app shows its own notice from the live event. Otherwise a push. The app keeps the hab
  informed with `session.focus`: `active` true at least every 30 s while a session is on screen
  in an active window, `active` false when it leaves or goes to the background.
- **Withdrawal.** An answer through `approval.answer` replaces the banner at once, same collapse
  id, passive, "Answered on <label>", where the label is the answering client's hello `label`.
  The Live Activity loses its buttons in the same step. A request closed in the TUI is replaced
  with "Answered in the terminal"; one whose pane's program exited with "Session ended".
- **Categories.** `approval` (Approve, Deny), `approval-deny` (Deny, Open on iPhone) and `open`
  (Open on iPhone). The app registers all three. Question banners use `open` until answering a
  question through `approval.answer` is built. A permission request with no request id uses
  `open`: no answer can name it.
- **Expiry text.** The hab leaves the expiry out of the body and sends `expires_at` in the
  payload's `flightdeck` object, with `mutable-content` set. The notification service extension
  appends " · Expires 14:05" in local time.
- **Opt-ins.** `done` banners go only to devices that registered the `done` kind. Notices from
  `notice.raise` go to every registered device. `hide_commands` on `push.register` is the
  "show commands in notifications" setting turned off.
- **Live Activity content.** The content state keys are listed in `hab-logic-api.md` 3.9, Push.
  `done`, `error` and an exit end the activity with a ten-minute dismissal; the app starts a new
  one for the next turn.
- **Left for the app half.** Registration, the notification service extension, the categories,
  the widget, push-to-start, and showing "push broken" (the hab logs it; no API field yet).
  8.7 covers what the app half built.

### 8.7 Built choices (task-oe7ioczy, app side)

Code in `apple/FlightdeckKit/Sources/FlightdeckKit/Push`, the shared module
`FlightdeckShared` (push payload, expiry text, Live Activity attributes, request links), and
the iOS targets `NotificationService` and `FlightdeckWidgets`.

- **Registration.** `PushRegistrar` calls `push.register` on every hab whose welcome lists
  `push`, each time a connection reaches online (a new snapshot count), and on every online hab
  again when the token, "Show commands in notifications" or "Notify when a turn finishes"
  changes. The app disconnects in the background (12.1), so each return to the foreground
  registers again; the hab keeps one token per device id. The device id is a random UUID kept in
  the app's defaults. Kinds: `permission`, `question`, `auth`, `error`, plus `done` when that
  setting is on. A hab without `push`, or one that answers `unavailable`, shows "Push is not set
  up on <hab>" with a link to `docs/push.md` in Settings › Notifications.
- **Environment.** The build's `aps-environment` is development for Debug and production for
  Release (`APP_APS_ENVIRONMENT` in `Names.xcconfig`); Info.plist repeats it as
  `FlightdeckAPSEnvironment` and the app sends that value, so a token always names its own APNs
  host.
- **Token first.** The app asks APNs for the token before the notification prompt: the token
  needs no permission, and Live Activity pushes work with alerts off.
- **When the prompt shows.** The app asks for notification permission once, when the first hab
  whose welcome lists `push` reaches online. No setting changes this (20.6). "Alert me when
  free" and a hab's test notification ask on their own when used. Not built: the iOS app asks
  at launch (`PushDelegate`).
- **Categories.** `approval` (Deny, Approve), `approval-deny` (Deny, Open), `open` (Open) and
  `question` (Reply as text, Open). Approve, Deny and Reply need unlock (7.6). Open reads "Open on
  iPhone" on iOS, for the mirrored Watch banner (16.2). The Reply action is on `question` only:
  `approval.answer` takes text only with the `answer` choice, and the hab sends no `question`
  category until it answers questions.
- **Answers.** An action maps to `approval.answer` with the push's pane and request id and `via`
  `banner`; a push with no request id opens the app instead. The answer goes on the live
  connection when the hab is online in this process, else on a one-shot connection
  (`HabOneShot`, 20 s). A dropped live connection tries once on a fresh one; the hab refuses a
  second answer, so this never answers twice. The hab is found by the push's host id through
  the hab book (`Flightdeck/hab-book.json`, host id to connection id, ids only, written as each
  hab comes online, rewritten when a hab is removed). An SSH key that needs Face ID fails at once
  ("open Flightdeck to answer"). Logs name the answer's outcome in public and keep the hab's hint
  and dial errors private, since they can name hosts and commands.
- **Failure.** `conflict` posts "Already answered" (or "Expired: answer in the terminal" past
  `expires_at`) and nothing else. When the live try dropped and the fresh try got `conflict`,
  the first try most likely landed: the notice says "Answered". Any other failure posts "Could not reach <hab>" / "Open on
  iPhone to retry." or "Could not answer on <hab>" with the hab's hint, and keeps the request
  (at most 16, in the app's defaults). When the app is next in front and the hab is online with
  that request open, the approval sheet opens on it: the foreground prompt. Nothing retries on
  its own (18.7).
- **Notification service extension.** Built: the hab leaves the expiry out of the body, so the
  extension appends " · Expires 14:05" in the device's time zone and clock style to a permission
  banner whose `expires_at` is still ahead (with the date when it is over 20 hours away). Every
  other push passes through unchanged. The Mac has no extension yet.
- **Withdrawal while the app runs.** When a request closes on the board, the app removes
  delivered permission banners for that request id (18.6, B where iOS allows).
- **Focus.** `FocusReporter` sends `session.focus` `active` true when a session view appears in
  an active scene, again every 25 s (under the hab's 30 s, so the 60 s hold never lapses while
  the owner looks), and `active` false when the view goes or the app leaves the foreground; the
  false goes out before the connections close, with at most 1 s of waiting. Calls run in order.
  A reconnect repeats the focus after the registration, so the new client is bound to the device
  first. A Mac window showing the same session twice stays focused until both close.
- **Approval sheet.** Opens from a rank 1 row, a notification tap, a re-raised request, or a
  Live Activity link (`flightdeck://request?hab=&pane=&request=&session=`). It reads every
  request from the board, oldest first with "1 of 3". The request on screen stays after it closes:
  when the hab's `agent.state` shows it closed without this sheet's answer, the buttons turn off
  and it says "Answered elsewhere". A tap on a banner or Live Activity whose request is gone
  never opens another request in its place (that would put a different command under Allow):
  once the hab is online the sheet shows that request as answered elsewhere; before that the
  request waits for the foreground. The countdown uses the device clock (no offset is measured
  yet). A failed answer keeps the buttons on with the hab's hint.
- **Plans and questions** (`landscape.md` 4.4). A permission request for Claude's
  `ExitPlanMode` shows the plan, with "Keep planning" (deny) and "Approve plan" (allow). One for
  `AskUserQuestion` shows each question's options; the answer is `answer` with `text` set to the
  chosen labels joined with ", " (several questions: one "<header>: <labels>" line each). The hab
  refuses `answer` until the Claude adapter builds it; the sheet then shows the hab's hint.
- **Live Activity.** `LiveActivityTracker` tracks the session the owner last sent a message to,
  or the one that last entered `blocked`, whichever came later. ActivityKit starts an activity
  only while the app is in front, so the tracker starts it when the scene turns inactive on its
  way out, not in the background: for that session, if it is working or blocked and its hab has
  push registered (without push nothing could update it after suspension, 9.2). Registered
  means the hab's last `push.register` succeeded: each new connection registers again, and the
  scene can turn inactive while that repeat is in flight, so a repeat counts as registered. A
  first registration still in flight does not, nor does one after a new device token, which no
  hab holds until it answers. One activity at
  a time: any other is ended first, and its token dropped on its hab with `push.unregister`.
  The first content of a session blocked on an answerable permission carries its request with
  Allow and Deny, since the hab sends nothing until the state changes. The activity's push
  token goes to that hab with `push.register_activity`, on the live connection or a one-shot
  one. Opening the session in the app ends it and drops its token. An answer from the activity
  that landed or was refused drops its buttons at once, before the hab's push.
- **Live Activity buttons and unlock** (owner ruling 2026-11-01). The intents are compiled into
  both the app and the widget (App Intents reads metadata per target) and run in the app. Allow
  asks for Face ID or the passcode (`authenticationPolicy` `requiresAuthentication`); Deny runs
  without it. The device key signs only while the device is unlocked (transport.md 7.2), so an
  answer while protected data is unavailable sends nothing: the app posts "Unlock to answer" and
  raises the request in the foreground. This holds for Deny too. The built setting "Approve from
  the lock screen without unlocking" (Settings › Notifications, in the App Group) swaps Allow
  for an intent with no authentication. With today's key class that changes nothing, so the
  setting goes (20.6); the build that removes it keeps the authenticated Allow.
  Push-to-start and the "hab offline" end are not built.
- **Mac.** The Mac app answers banners and reports focus, but asks for no APNs token: its
  entitlements have no `aps-environment` until a Developer ID profile with push exists.
- **Tests.** `PushTests.swift` (registration against the fake hab, action mapping, categories,
  expiry text, one-shot and live answers, refusal and re-raise, focus cadence, the sheet,
  plans and questions, Live Activity choices and links) and `ApprovalFixTests.swift` (the Live
  Activity through a fake ActivityKit host: start on inactive, first content, one at a time,
  local button drop, end and unregister; unlock refusal, stale taps, withdrawal, the dropped
  live connection, the hab book, plan text). `LiveTests.testLivePushRegister` runs
  against a real gff-hab with push config (`FLIGHTDECK_LIVE_SOCKET`, `FLIGHTDECK_LIVE_PUSH=1`).

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
- **Content.** Session name, hab, harness, state, time in state, the context bar (3.4), and the
  latest event (tool running, approval needed, finished). Two styles: Detailed (with the last agent
  line) and Compact.
- **Dynamic Island.** Expanded: the context bar and percentage, the model and project, the state in
  one word ("Working"), and one line of what it does ("Refactoring the session store · 6 files").
  Compact: the harness icon and the context percentage.
- **Buttons.** In `blocked:permission`, Allow and Deny. They go away as soon as the request is
  resolved anywhere (7.5).
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
  keeps its key in the Keychain. Settings shows the key's fingerprint. The owner may also import an
  existing SSH key (10.4); each hab entry says which key it uses.
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
  authorized keys on that host. The app then runs `gff-hab connect` over SSH. (Decided in 18.8.)

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
| Away from home with no VPN | Habs on LAN addresses show offline. Push still arrives (APNs needs no route to the hab), but a lock-screen answer fails (7.8) |
| The pairing token expired | The hab refuses. The app says "run `gff-hab pair` again" |
| The revision does not fit | The app shows which side to update (`hab-logic-api.md` 7.3) |
| Many visible panes over the system sshd | Past nine panes, the app opens a second SSH connection (`transport.md` 7.6) |
| A hab rotates its host key | The app accepts the new key only when signed by the old one, within the grace period (`transport.md` 6.5) |
| A captive portal or a network that drops SSH | Every endpoint fails fast; the hab shows offline with "network blocks SSH" when the TCP connect succeeds but SSH never starts |

Sources: `transport.md` 4, 6, 7; `hab-logic-api.md` 3.5, 3.7, 4; Heeler README (several routes
per host, one long-lived event channel). Heeler's embedded overlay networks are a reference only;
the app does not embed one (owner rule).

### 10.4 SSH keys

Settings › Keys offers two kinds of key per device.

| | Generated key (default) | Imported key |
|---|---|---|
| What | A P-256 key made in the Secure Enclave on first start | The owner's existing OpenSSH private key |
| Types | P-256 only (the Secure Enclave makes no other) | Ed25519; ECDSA P-256, P-384 and P-521 |
| Where | Secure Enclave; the private key never exists outside it | Keychain, this device only, not synced, never in backups |
| Usable | Only while the device is unlocked | Only while the device is unlocked |
| Can be copied off the device | No | Not by the app, but a copy already exists wherever it came from |
| A host admits it by | Pairing (`transport.md` 6.1) | Already trusting it; no pairing step |

- **New users** get the generated key and pair. Nothing to import.
- **Import from** Files, the share sheet ("Open in Flightdeck"), or paste. The app reads the
  OpenSSH private key format, encrypted or not.
- **Passphrase.** For an encrypted key, the app asks for the passphrase once. It derives the
  decryption key with the format's bcrypt-based key derivation, decrypts the private key in memory,
  checks the format's built-in check words to confirm the passphrase, then discards the passphrase
  and the file's bytes. The ciphers to accept are the AES-CTR ones `ssh-keygen` writes by default
  *(gk)*. The passphrase is never stored.
- **Stored** as a Keychain item marked usable only when unlocked, this device only, and not
  synchronisable, so neither iCloud Keychain nor a backup carries it to another device. With
  "Require Face ID for SSH keys" on (10.7), using the key also asks for Face ID.
- **Never sent anywhere.** The private key goes to no hab, no log, no crash report, and no export.
  The app has no "export key" control. Only signatures leave the device, as with any SSH key.
- **Shown** in Settings: type, fingerprint, the key's comment, and which habs use it.
- **Use.** Each hab entry picks its key. A host whose system sshd already trusts the imported key
  is reachable at once: the app connects over SSH and runs `gff-hab connect` (10.1, the fallback
  path), with no QR code and no pairing. QR pairing (`transport.md` 6.1) may admit either key; the
  app offers the generated key first.
- **Delete** removes the Keychain item. Hab entries that used it fall back to "no key: pair or
  choose a key".
- **Mac app:** the same import, into the Mac's Keychain.

**The trade-off.** A generated key exists only on this device, and revoking it touches nothing
else. An imported key is a copy of a key that also lives elsewhere and that many hosts may trust. If
the phone is compromised while unlocked, that key can reach every host that trusts it, not only the
habs. Revoking it means removing it from every host and replacing it everywhere it was used. The
benefit is real: hosts that already trust the key need no pairing at all. The app says this in one
line on the import screen, and keeps the generated key as the default.

| Case | Behaviour |
|---|---|
| An RSA key | Refused: "RSA keys are not supported. Make an Ed25519 key with `ssh-keygen -t ed25519`." The SSH library has no RSA support (`transport.md` 1.8) |
| A security-key type (`sk-` keys) | Refused: its private part lives on the hardware key, not in the file |
| A key in the older PEM format | Unencrypted PKCS#8 or EC keys import. An encrypted older PEM key is refused with the command that rewrites it in the OpenSSH format (`ssh-keygen -p`) |
| A wrong passphrase | The check words do not match; the app says so and asks again. No limit on tries, since the file is already in the owner's hands |
| A cipher the app does not support | Refused, naming the cipher and the command to re-encrypt with the default |
| A public key file instead of a private one | "This is a public key. Import the private key file" |
| The same key imported twice | The app spots the fingerprint and keeps one |
| The same imported key on several devices | Every hab sees one key, so it cannot tell those devices apart. Revoking it on a hab locks out every device that uses it. The app warns when a key is used for pairing with the embedded server, and suggests the generated key there |
| The device restores from a backup | Imported keys are not in it. The app lists the hab entries that need a key again |
| A file from the share sheet with extra text around the key | The app takes the first private key block and ignores the rest |

Sources: owner decision 2026-10-31 (import a personal Ed25519 key beside the generated key);
`swift-nio-ssh` private key source (key types; no RSA, no key-file reader) and its open RSA pull
request; Citadel source (OpenSSH key reader with bcrypt and AES-CTR); Moshi store text (SSH keys in
the Keychain behind Face ID); OpenSSH key format and `ssh-keygen` defaults *(gk)*; `transport.md`
1.8, 4.3, 7.3.

### 10.5 Built choices (task-46vch3ag)

The first app stage: the connection to every hab and the status board. Code in
`apple/FlightdeckKit`.

- **Transport.** `Transport.open(kind)` returns one byte stream; `kind` is `control` or `data`, so
  a data stream per pane opens on the same carrier later (`hab-logic-api.md` 4.2, option A). The
  iOS SSH carrier (task-iv89iwd8) implements the same protocol. `reset()` drops a shared
  connection; the in-app SSH carrier shares one (10.6).
- **Mac carriers.** The local Unix socket uses gff-hab's default path rule (`$GFF_HAB_SOCKET`,
  else `gff/hab.sock` under Application Support). The ssh carrier runs the system `ssh -T -o
  BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -- <host> gff-hab connect` with
  the user's own keys and ssh config. ssh never prompts; its last stderr lines become the error
  shown under the hab. Each stream is its own ssh process for now; connection sharing comes with
  data streams. The hello has 25 s over ssh (10 s on the socket): its clock starts when ssh
  starts, so it covers connect, key exchange, auth and the remote shell.
- **Hab list (Mac).** Flightdeck's own file, `Flightdeck/habs.json` under Application Support, or
  the path in `$FLIGHTDECK_HABS`. Each entry has a `label` and exactly one of `local` (optional
  `socket`) or `ssh` (`host`, optional `command` and `options`). No file means the local hab
  only. Pairing replaces this list on the phone later.
- **No sandbox.** The Mac app drops the App Sandbox (owner decision: Developer ID, unsandboxed),
  so it can reach the local socket and run `ssh`. Its entitlements file is empty.
- **One connection actor per hab.** It sends `hab.hello` with the reconnect token, then
  `events.subscribe` with scope `host`. Events that arrive before the subscribe response wait;
  the snapshot applies first, then only events with a seq above its seq. A seq gap, or a
  heartbeat whose seq is ahead of the last event, ends the connection and starts a reconnect.
- **Every snapshot replaces the hab's objects.** No replay exists (`hab-logic-api.md` Q5), so a
  reconnect in the same epoch also reads state again; the snapshot is newer than anything held.
  A new epoch counts as such only for client state (the token no longer holds; sizes and tickets
  are dropped when those exist). Rows swap when the snapshot arrives, so they do not flash.
- **Revision rule.** Within an epoch an event whose revision is not above the held one is
  ignored. `agent.state` carries its revision in the data when the envelope has none.
- **Liveness.** `hab.ping` goes out when nothing was sent for 15 s. Nothing received for 45 s
  ends the connection. The check runs every second.
- **Reconnect.** The first retry after a connection that stayed online for 10 s goes at once. A
  shorter one counts as a failure, so a hab that drops every connection after the snapshot is
  not hammered. Later retries wait 0.5 s doubling to 30 s, then a random point in the upper half
  of that wait. Three failures in a
  row mark the hab offline; retries go on while the app is visible. A return to the foreground
  retries at once.
- **Offline and update needed.** Rows dim only when the hab is offline (three failures), with
  "offline since" the last time it was online. A short reconnect does not dim them. A revision
  mismatch stops retries until the next start and hides the hab's rows; the list and the
  connections column say which side to update.
- **Board order.** Rows sort by rank, then by when the row entered its rank, newest first. The
  first sight of a row uses the hab's `at` stamp. A change inside the same rank does not move a
  row. Times in state use the phone's clock against the hab's stamp; the API has no hab clock yet.
- **Seen.** Opening a `done` row marks it seen on this device, keyed by pane and state stamp, so
  a later `done` counts again. `session.focus` (17) replaces this in Part 6 stage 7.
- **Open hook.** `Board.onOpen` receives a `SessionTarget` (hab, session, pane). The session view
  (chat and the terminal slot, 4.6) opens from the board; a board with no fleet shows a sheet
  of the session's fields.
- **Layout.** iPhone: the list, with connections in a sheet. iPad and Mac: the list beside a
  column with a session cards placeholder and the connections list. The project and hab grouping
  switch, search and the usage footer are not built yet.
- **Lifecycle.** iPhone and iPad close every connection when the app enters the background and
  reconnect when it becomes active; `inactive` changes nothing. The App reads the app-level
  scene phase, which is active while any window is, so one iPad window going to the background
  does not disconnect another. Transitions run one after another and the last wanted state wins. The Mac counts open windows; the
  last one closed disconnects. Closing happens at once, so the hab ends size ownership at once.
  Proof: unit tests over fake and real Unix socket streams count open streams after the
  background; the app logs the count when it goes to the background and again 10 s later (iOS
  may suspend it before the second line).
- **Fake hab and demo mode.** `FakeHab` speaks the control protocol over the in-memory transport
  (hello and the revision rule, tokens per epoch, subscribe, seq, heartbeat, ping) for tests and
  demo mode. `-FlightdeckDemo YES` loads two invented hosts with sessions in every rank and
  replays a short script.

### 10.6 Built choices: the iOS SSH transport (task-iv89iwd8)

The in-app SSH carrier, the device key, hab entries, pairing codes and the background action.
Code in `apple/FlightdeckKit/Sources/FlightdeckKit/SSH`.

- **Carrier.** `NIOSSHTransport` implements `Transport` on `swift-nio-ssh` 0.15.1 (pinned
  exactly) over NIOPosix sockets. It works on iOS and macOS. One SSH connection per hab carries
  the control stream and one session channel per data stream; each channel runs the entry's
  command, `gff-hab connect` by default (`hab-logic-api.md` Q2). The hello timeout is 25 s, as on
  the ssh subprocess carrier. A dial has its own limits: 10 s for TCP per endpoint, 15 s from
  TCP to authenticated. A TCP connection where SSH never starts reports "the network may block
  SSH" (10.3).
- **Connection reuse.** Each new control stream dials a fresh SSH connection. Data streams use the
  connection of the newest control stream. An older connection takes no new channels and closes
  with its last channel, so a dead TCP connection after a network change never catches a new
  stream. A connection with no channel closes at once, so nothing stays open in the background.
- **Session limit.** When the host refuses a data channel (OpenSSH's `MaxSessions`, 10), the
  carrier dials one more connection for it (`transport.md` 7.6).
- **Channel flow control.** A channel reads only when its stream's reader asks, so the SSH window
  opens as fast as the app takes bytes (`transport.md` 4.4).
- **Endpoints.** An entry lists endpoints in order. The next one starts 300 ms after the previous
  one, or at once when the previous one failed; the first to authenticate wins and the rest
  close. When all fail, a host key or key refusal is reported before a timeout.
- **Host key.** Pinned by SHA-256 fingerprint per entry and checked before authentication. A
  different key fails with both fingerprints and "re-pair the hab, or remove it and add it again
  if you trust the new key". The connection retries with backoff like any failure and shows the
  error under the hab; it never accepts the new key on its own. First contact over the fallback
  path probes the host key (the handshake stops before auth), shows the fingerprint and asks the
  user to trust it.
- **Device key.** `DeviceKeys` makes a P-256 key in the Secure Enclave when the device has one
  (iPhone, iPad, Apple silicon Mac), else in software (the simulator, unit tests). It has no
  biometric flag. "Require Face ID for SSH keys" (10.7), one setting for this key and every
  imported key, makes the app ask for Face ID before each dial, off the network thread; an
  enclave key then signs with that authentication. The key blob is a Keychain
  item, usable only when unlocked, this device only, not synchronisable. On iOS it is in the data
  protection Keychain; the unsandboxed, possibly ad hoc signed Mac app uses the login keychain.
  There "this device only" is not enforced, so a software key on a Mac with no enclave can reach
  an (encrypted) Time Machine backup, as with other SSH tools.
  iPhone and iPad make the key on first start; the Mac makes it when Settings › SSH first opens,
  so a Mac that never uses the carrier never touches the keychain. A locked device gives "the device is locked: its SSH key works only while
  it is unlocked". The key source is the `SSHKeySource` protocol; an imported OpenSSH key
  (10.7) is the second source.
- **Settings › SSH.** Opened from the connections list. It shows the key's type, fingerprint and
  OpenSSH line with Copy and Share, "Require Face ID for SSH keys" (iOS), the SSH habs, and a
  test notification per hab.
- **Hab entries.** `Flightdeck/ssh-habs.json` under Application Support (or `$FLIGHTDECK_SSH_HABS`),
  mode 0600: id, label, user, endpoints in order, the pinned host key fingerprint and line, the key
  id, an optional command, and the hab's device id when the entry came from a pairing code. Keys
  stay in the Keychain. Entries load on every platform; the Mac keeps `habs.json` and the system
  ssh as its default, so the nio carrier is there for testing the phone's path.
- **Add a hab by address** (18.8). The user types `user@host[:port]`, copies the device key line
  to the host's `authorized_keys`, presses Connect, compares and trusts the host key, and the app
  tests with one `hab.ping` before it stores the entry and adds it to the board. A refused key says
  which fingerprint and user to add. An optional command covers a `gff-hab` that is not on the
  remote PATH.
- **Pairing codes.** `PairingCode` parses the `flightdeck://pair` link (`transport.md` 6.1) from
  the camera, pasted text or an opened link (the iOS app registers the `flightdeck` URL scheme).
  It shows the hab id and endpoints, then says the hab cannot pair yet and offers "Add the key by
  hand" with the label and device id filled in. `PairingCode(offer:)` builds the link from a
  `devices.pair` result for the hab side later.
- **Background action.** `HabActions` registers a notification category with a "Ping hab" action
  that needs unlock, and is the notification delegate from launch. The action loads the entry and
  key, runs one `hab.ping` on its own connection (`HabOneShot`: open, hello, call, close, 20 s
  deadline that holds even during a stuck TCP connect, no retry) and posts the result or "could
  not reach" (18.7). With a Face ID key it fails at once with "Open Flightdeck to retry", since a
  background action cannot prompt. `SSHSetup.perform` is the
  call site `approval.answer` reuses. Push replaces the local notification later (task-oe7ioczy).
- **Cold start.** `ColdStart` logs the time from process start (from the kernel) to the first byte
  any hab sent, once per process; Settings › SSH shows it.
- **Tests.** `SSHTransportTests` run the carrier against an in-process `swift-nio-ssh` server on
  loopback that bridges each channel to the fake hab: online over SSH, a changed host key refused
  before auth (also through `HabConnection`), the probe, a refused key, control and data on one
  connection, the session limit, a fresh connection per control stream, staggered endpoints, the
  one-shot call and a pane stream over a data channel. `SSHPairingTests` cover the code parser,
  endpoints, fingerprints against `ssh-keygen`, keys and the entry file. `SSHLiveTests` run
  against a real sshd through `apple/scripts/ssh-live-check.sh`.

### 10.7 Built choices: imported SSH keys (task-1csglxtc)

Code in `apple/FlightdeckKit/Sources/FlightdeckKit/SSH/KeyImport` and `Views/KeyImportViews.swift`.

- **Reader.** `OpenSSHKeyFile` reads the OpenSSH key format (PROTOCOL.key): one key, Ed25519 or
  ECDSA P-256, P-384 or P-521. The key type and public key come from the unencrypted part, so
  RSA, `sk-` keys, other types and unknown ciphers are refused before the passphrase prompt. It
  takes the first private key block in the text and ignores the rest; CRLF line ends are fine.
- **Limits.** Input over 64 KiB is refused ("This file is too large to be a key"); a file is
  read through `OpenSSHKeyFile.read`, which stops past that size, so "Open in" on a video
  never loads it. More than 1024 bcrypt rounds is refused when the file is read, with the
  `ssh-keygen -p -a 100` command (ssh-keygen writes 16; 100 and 256 are common). The
  derivation checks for cancellation between rounds, and closing the import sheet cancels it.
- **Ciphers.** `none`, and with the `bcrypt` key derivation: `aes128/192/256-ctr` (the
  `ssh-keygen` default is `aes256-ctr`), `aes128/192/256-cbc` and `aes128/256-gcm@openssh.com`.
  CTR and CBC use CommonCrypto, GCM uses CryptoKit. `chacha20-poly1305@openssh.com` and 3DES are
  refused with the `ssh-keygen -p -Z aes256-ctr` command. GCM and CBC cost a few lines each, and
  a key someone re-encrypted with `-Z` imports instead of failing.
- **bcrypt_pbkdf.** Our own code from OpenBSD's description, on our own Blowfish. Blowfish's
  tables come from pi, computed by `apple/scripts/gen-blowfish-tables.py` (Machin's formula in
  integer arithmetic). Tests: published Blowfish vectors and `bcrypt_pbkdf` answers from an
  independent implementation (pyca/bcrypt). The state is one raw buffer, so a 16-round key
  takes about 0.3 s in a debug build and much less in release.
- **Checks.** The two check words must match: a mismatch is "wrong passphrase" for an encrypted
  key and "damaged" for a plain one; a GCM tag failure is also "wrong passphrase". Then the
  padding (1, 2, 3 ...), the lengths, and the private key must give the file's public key.
  Messages: wrong passphrase, unsupported type or cipher, RSA, security key, public key only,
  encrypted PEM, damaged (check words, padding, a named field), cut short.
- **Older PEM keys.** Unencrypted SEC1 (`EC PRIVATE KEY`) and PKCS#8 (`PRIVATE KEY`, ECDSA or
  Ed25519) import through a small DER reader: `ssh-keygen -m PEM` writes explicit curve
  parameters, which CryptoKit's PEM reader refuses. The curve follows from the private value's
  length, and the embedded public key must match.
- **Storage.** The decrypted raw key (the Ed25519 seed or the ECDSA scalar), its type, comment
  and date go to the same Keychain service as the generated key, account
  `imported-` plus the first 12 bytes of the public key's SHA-256 in hex. The same key imported
  twice lands on the same item, and the app says it kept one. The passphrase and the file never
  reach the store or a log; the log gets the type and fingerprint. The passphrase buffer, the
  derived key, the decrypted section and the copies of the seed or scalar are zeroed after use.
  Swift cannot promise more: strings are not zeroed, and an unencrypted file's bytes stay in
  memory while the import sheet holds the file. The sheet drops them when it closes.
- **Storage class.** Every write sets `WhenUnlockedThisDeviceOnly` and not synchronisable. A test
  checks the query and, on the real Keychain, the stored item. On the Mac the login keychain
  keeps no accessibility class (10.6), so there the test checks only the sync flag.
- **Face ID.** One device-wide setting, "Require Face ID for SSH keys" (Settings › SSH, iOS), off
  by default, covers the generated key and every imported key, including keys imported later.
  One app-level check over every key is the owner's decision (2026-11-01). It replaces the
  hardware-enforced biometry the generated key had with the old switch; herdr, Paseo and Rex
  offer no more.
  When on, the app asks for Face ID before each dial. It is an app check, not a Keychain access
  rule: items stay readable whenever the device is unlocked, and changing the setting keeps
  every key and its public line. Turning it off asks for the owner's authentication (Face ID or
  the passcode) first, so whoever holds the unlocked device cannot drop the check; turning it on
  asks nothing. Each key reads the setting when it signs, not when it loads, so a key already
  loaded follows a change at its next dial. Live connections keep running: a change starts no
  reconnect, so turning it on raises no burst of Face ID prompts, one per hab. A setting that
  cannot be read counts as on. Background actions refuse a key that needs Face ID at once. The
  setting is its own item in the keys' Keychain service (account `setting-face-id`, label
  "Flightdeck setting"; no item means off), so it shares their storage class and never outlives
  them in a backup. Changes and the migration hold one lock, so a first migration on another
  thread cannot undo a change. A key's detail shows the setting, read-only.
- **Migration from per-key switches.** Earlier builds kept a Face ID flag in each stored key. On
  first use `DeviceKeys` folds them: any key with the flag on turns the setting on, then every
  key is written again without the flag (setting first, so a stop halfway leaves it on and the
  next start finishes). Hands-off default: the setting takes the strictest choice any key had,
  with no prompt. An owner who had Face ID on one imported key now has it on every key, and
  background answers over the other keys say "needs Face ID" until it is turned off; the
  migration logs the change. A generated enclave key made with the old switch has `.biometryCurrentSet`
  in the enclave, which cannot be removed. The app tells by signing once with no interaction
  allowed, so no flag is kept. Only LocalAuthentication's "not interactive" refusal counts; any
  other failure (the device locked a moment ago, a key voided by a Face ID re-enrolment) says
  nothing about Face ID and shows at the dial. Such a key still asks for Face ID with the
  setting off, and Settings › SSH says so with "Make a new key". The test of this detection
  needs an unlocked Mac with Touch ID enrolled; it skips otherwise.
- **Choosing a key.** Each hab entry names its key (`keyID`). "Add a hab by address" shows a key
  picker once an imported key exists; the generated key is preselected. Each row in Habs over
  SSH has a key menu. An entry whose key is gone (deleted, or a restore from a backup) says "No
  key: choose one" and its connection fails with "no key: pair again or choose a key". A key
  that exists but cannot be read (a locked device) shows the reason instead, so the user is not
  led to repoint the entry. Keychain errors keep their status code for this. A stored item
  that does not decode is skipped and logged; the other keys still list.
- **Import.** Settings › SSH › Imported keys › Import a key: paste into the field, Paste from the
  clipboard, or Choose a file. iOS also opens key files from the share sheet and Files: the app
  declares `public.data` and `public.text` as a viewer with rank Alternate (OpenSSH key files
  have no extension), and removes the copy iOS puts in its Inbox once read. The Inbox check
  compares standardized paths with symbolic links resolved, since the file URL may read
  /private/var while the home folder reads /var. A file that
  cannot be read opens the sheet with the read error. A pairing link and a key file that
  arrive together share one sheet and show in turn, the pairing link first. The sheet shows the
  type and fingerprint, asks for the passphrase when needed (a wrong one clears the field; no
  limit on tries), then shows the public line with Copy and Share. The trade-off line sits under
  the paste field.
- **Detail.** Type, comment, fingerprint, the public line, the habs that use it, the Face ID setting, Delete
  (with a confirmation that its habs stop connecting).
- **Not built.** The warning when an imported key is used for pairing with the embedded server
  (pairing is not built). Open in on the Mac (it imports through paste and the file picker).
- **Tests.** `KeyImportTests`: fixtures from `ssh-keygen` (`apple/scripts/gen-key-fixtures.sh`,
  throwaway keys with invented comments, stored without their BEGIN and END lines so secret
  scanners pass them) for every type, plain and encrypted, each cipher family, one round, SEC1
  and PKCS#8; wrong passphrase, padding and check words corrupted, a changed public key,
  truncation, RSA, chacha20, encrypted PEM, a public key, text around the key; the store; the
  storage class; an imported key on the in-process SSH server; the Face ID setting over both
  kinds of key (off needs authentication, a refusal keeps it on) and the migration of per-key
  flags. `SSHLiveTests.testLiveImportedKey`
  pings a real sshd with an imported passphrase-protected Ed25519 key made by `ssh-keygen`.

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
  wake path. The background jobs are a notification action (Allow, Deny, Reply, from the phone or
  the Watch), which connects, answers and disconnects, and the notification service extension,
  which updates the widget snapshot (15.1) and opens no connection.
- **One exception: voice mode** keeps its one connection while its view is open, even with the
  screen locked (13.1).
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
| A notification action takes too long | iOS ends it. The app posts the local failure notice (7.8) |

Sources: the task (one event stream per hab while visible, disconnect in the background, push as
the only wake path); `hab-logic-api.md` 3.5, 3.7, 5.7; platform facts in 1.5 *(gk)*.

---

## 13. Voice mode

A hands-free conversation with one session, like the voice modes of the Claude and ChatGPT apps.
The owner talks; the agent's reply is read aloud. It is part of the app, not Siri: no Siri intent,
no wake word, no system assistant in the path.

### 13.1 Behaviour

- **Open.** One tap on the voice button in the session header, or a long press on the composer's
  microphone. A full-screen view opens over the session: a large level meter, the session title,
  hab and state, the live transcript of what the owner says, and three controls: end, mute the
  microphone, and switch to the keyboard.
- **The loop.**
  1. **Listen.** The app records while the owner speaks. Apple's on-device recognizer shows the
     words as they are heard; when the turn ends, the hab's text replaces them (13.5).
  2. **Send.** A pause (default 1.2 s of silence) or a tap ends the turn. The text goes to the
     session as an ordinary message (`session.send`, 17), under the input rules of 4.2. With
     "confirm before send" on (off by default), the owner says "send" or taps.
  3. **Wait.** While the agent works, the view shows the state and the latest tool line. A soft
     tone repeats every so often (default 20 s), so the owner knows it is still running. Nothing
     is read aloud while it works.
  4. **Speak.** When the turn ends (`done`, `idle` or `blocked`), the app reads the reply aloud
     (13.2), then listens again.
- **Barge-in.** The microphone stays open while the app speaks, with the system's echo
  cancellation. When the owner starts to talk, speech stops at once and a new turn starts. A tap
  anywhere also stops speech.
- **Spoken commands.** A short fixed set, matched only when the whole utterance is the command:
  "stop" (interrupt the turn, as the Stop button), "repeat", "allow" or "approve", "deny", and
  "end voice". Anything else is a message to the agent. A setting turns spoken commands off.
- **One session.** Voice mode is bound to the session it opened on. Another session that needs the
  owner plays a distinct chime and shows a banner; voice mode does not read it.
- **The screen locks.** Voice mode keeps running while its view is open, the screen locks or the
  app goes to the background: it holds the audio session and the one connection to the session's
  hab. It ends after 10 minutes with no speech and no turn running, or when the owner ends it. This
  is the one exception to the background rule in 12.1 (decided in 18.11).
- **Audio routes.** Speaker, wired and Bluetooth headsets. A route change pauses listening for a
  moment, then carries on.

### 13.2 What is read aloud

- The final agent text of the turn, not tool calls and not tool output.
- The app makes it speakable: a code block becomes "a code block, shown on screen"; a table becomes
  "a table"; a file path becomes its file name; a link becomes its site name. Markdown marks are
  dropped.
- It is capped (default about 45 seconds of speech). Past the cap the app says "the rest is in the
  chat" and stops.
- A turn that ends with no text says "Done." A turn that ends in `error` says "The turn stopped"
  and the first line of the error.
- Whether a conversation layer writes a better spoken reply is a choice for `voice.md` (13.5).

### 13.3 Approvals and questions by voice

- When the session reaches `blocked:permission`, the app reads the request: the title (7.2), then
  the command in a speakable form ("wrangler deploy, dash dash env prod"). It then asks "Allow or
  deny?" and shows the approval sheet in large type.
- It listens for the answer only after it has finished asking, so its own voice cannot answer. The
  answer must be a clear match: allow, approve or yes; deny or no. "Yes" counts only while
  `voice.yes_allows` is on (the default; off at Safe, 20.3). Anything else, or a low-confidence
  result, makes it ask once more, then wait for a tap.
- It says what it did: "Allowed" or "Denied".
- Voice Allow follows the same limits as a banner Allow (7.8): a command longer than the limit, or
  a broad choice, can be denied by voice but allowed only by tap. Expiry applies (7.4). A voice
  Allow asks for no Face ID (7.6).
- `blocked:question` reads the question and numbers its options. The owner says a number, an
  option, or a free answer.

### 13.4 Dictation

- A microphone in the chat composer and on the terminal keyboard bar (5.1) starts dictation: the
  same speech-to-text as voice mode (13.5), writing into the composer or the pane.
- In the composer, words appear as heard and stay editable; the owner sends.
- In the terminal, the text goes to the pane as one paste when the owner stops, without Enter.
- The language follows the system; a setting picks another.

### 13.5 Models and providers

- This section describes what the owner experiences. The speech-to-text and text-to-speech models,
  and whether a conversation layer sits between the voice and the agent, are chosen in
  `docs/specs/voice.md`.
- Engine order (owner, 2026-11-01, following Paseo): speech recognition runs on the session's hab
  by default. Parakeet on the device and Apple's engine are options, in that order. There is no
  cloud fallback; a hosted engine runs only when the owner turns it on with their own key.
- Audio is never stored. With the hab engine, the recorded turn goes to the hab, which returns
  the text and keeps neither. The text reaches the session only when it is sent, as a typed
  message does.

### 13.6 Edge cases

| Case | Behaviour |
|---|---|
| The hab has no speech engine, or its model is still downloading | The app uses the next engine the owner enabled and names it in the voice view. A download shows its progress |
| No engine for the language (hab and device) | Voice mode and dictation stay off, with "Download <language> for on-device dictation" and a link to the setting. Never a cloud fallback |
| Microphone or speech permission denied | The voice button explains and links to Settings |
| A phone call or alarm interrupts | Listening and speech pause. After the interruption, voice mode resumes listening; a running turn is not touched |
| The hab goes offline mid-turn | The view says "lost <hab>" and keeps trying (10.2). It reads nothing until it reconnects and the turn's end is known |
| The owner speaks while the agent works | The message waits as "sends after this turn" (4.2). The app says "queued" |
| A noisy room keeps triggering barge-in | A setting raises the barge-in threshold, or turns barge-in off so only a tap stops speech |
| A misheard turn | The live transcript shows it before send. "Confirm before send" catches it; "stop" interrupts the agent |
| The reply is in another language than the voice | The app picks a system voice for the reply's language when one is installed |
| The owner says "stop" meaning a word to the agent | With spoken commands on, a lone "stop" is the command. Longer sentences go to the agent |
| The session has no chat reader | Voice mode is off for that session; the terminal view's dictation still works |
| A request expires while being read | The app says "That request expired" and shows the terminal option |

Sources: owner decision 2026-10-31 (voice mode like the Claude and ChatGPT apps, not Siri,
on-device only); Moshi App Store screens (dictation strip in the terminal) and store text
(voice to terminal with on-device recognition); voice modes of the Claude and ChatGPT apps,
general behaviour *(gk)*. Model and provider choices: `voice.md`.

### 13.7 Built choices (task-h011ghy9, app side)

Code in `apple/FlightdeckKit` (`Voice/`, `Views/VoiceViews.swift`) and the
`FlightdeckParakeet` library, which only the iOS app links.

- **Speech calls.** `Voice/SpeechAPI.swift` calls `speech.prepare` and `speech.transcribe`
  through the generated types; a transcribe waits up to 60 s. When `speech.prepare` carries an
  `error` (the last download or load failed), the hab engine skips that turn, the voice view's
  footer names the engine with the error, and the next turn asks the hab again.
- **One engine interface.** `SpeechEngine` has `prepare`, `transcribe(clip, live:)` and
  `close`. `SpeechEngines` holds the enabled engines in the fixed order: hab, Parakeet on the
  device, Apple, and prepares them at once. Every failure moves the turn to the next engine;
  a cancellation (voice mode closing) stops the turn instead. A sticky refusal skips an engine
  until voice mode or dictation closes: no `speech` capability, `unavailable` from
  `speech.prepare`, no model on the device, an older system. A hab not connected yet (no
  welcome), a model still downloading on the hab, an `unavailable` from `speech.transcribe` or
  a network error moves only this turn; the next turn asks the hab again. A prepare that
  finishes after close is closed at once, so a late Parakeet load is released.
- **Audio to the hab.** Ogg Opus (RFC 7845), mono, 16 kHz input, 20 ms packets at 24 kbit/s,
  from the system encoder. Granules come from each packet's TOC byte, not a fixed 20 ms. When the hab does not list `opus` in `formats`, or the encoder
  fails, the app sends 16-bit PCM at 16 kHz. A clip over 1 MiB as PCM goes to the next engine.
- **Apple's engine is the live words.** SpeechAnalyzer runs for the whole of voice mode and
  shows words as heard (iOS and macOS 26 and later). As the last engine, its text for the turn
  is those live words, so it costs no second pass. A turn's live words are the results that
  finished since it began, plus the result in progress; a result that finished up to 0.6 s
  before the turn began is part of it, since its words started the turn. A revision of the
  result in progress never brings back earlier speech. A phrase still in progress when a turn
  ends belongs to that turn: its later revisions and its final result start no new turn and
  are not carried into the next one. Words the owner starts before that phrase finishes are
  read by the hab or Parakeet, but may be missing from the Apple text. The microphone starts first; live words
  follow once the speech asset is installed, and the footer says "Downloading speech for live
  words" meanwhile, or why live words are off. On an older system the Apple engine is skipped
  and no live words show; the hab and Parakeet still write the text.
- **Parakeet on the device.** `ParakeetHost` loads the model while voice mode or dictation holds
  it, and unloads it when the last one lets go, on a memory warning, and when the app leaves
  the foreground. Voice mode keeps running in the background (13.1) without it: those turns go
  to the next engine, and the model loads again on return. After a memory warning it stays
  unloaded until every holder has let go. Each voice mode and dictation holds it with its own
  token, so two iPad windows do not unload each other's model. An unload waits for a running
  transcribe. A turn that ends before the engine's prepare held the model takes it then. The
  model folder is kept out of the device backup. A deferred unload after the last release is
  dropped when a new holder arrives first. The option shows on devices with at least 6 GB of
  memory (the iPhone 15 Pro, 8 GB, ran it well). The model downloads from Settings with
  progress, with its CC-BY-4.0 attribution line.
- **Turn end.** The audio level and new live words mark speech. A turn starts after 0.15 s of
  speech and ends after the pause setting (default 1.2 s) or a tap. The turn keeps 0.5 s of
  audio from before speech started, so the first word is not cut. A turn ends at 120 s, the
  hab's limit, with "The turn reached 120 s and ended there."; its start is kept. A route
  change while the owner speaks ends the turn with the audio so far, and rebuilds the live
  recognizer for the new input.
- **Echo and barge-in.** Speech plays through the same audio engine as the microphone, with
  voice processing on, so the echo canceller removes the app's own voice. While the app speaks,
  a barge-in needs 10 dB more level, and live words alone do not start a turn. "Noisy room"
  raises the speech level from -38 to -28 dBFS.
- **What is read.** The last top-level agent text after the last user message, never tool calls,
  tool output or subagent text. When it has a line that starts with "Spoken summary:", only
  what follows is read. The trimmer (`SpokenText`) turns a code block into "A code block, shown
  on screen.", a diff (fenced or bare) into "A diff, shown on screen.", a table into "A table.",
  a link into its text, a bare link into its site, a path into its file name, and drops Markdown
  marks. Tables with or without outer pipes, indented code after a blank line, and fences of
  four or more marks count. It caps at 110 words (about 45 s) and says "The rest is in the
  chat." A turn end waits up to 3 s for the reply: a new agent text after the turn's last tool
  call, with the chat still for 300 ms. A reply that arrives while the owner speaks is not read.
  A turn that ends in `error` reads "The turn stopped." and the first sentence of the agent's
  last text: the hab sends no error text.
- **The summary request.** Each spoken prompt ends with a request for a "Spoken summary:" line
  of two sentences. The chat view hides it on the owner's message, also while the message is
  still outgoing. The transcript keeps it, so the TUI shows it. A setting turns the request
  off.
- **Send path.** A spoken turn goes through `ChatModel.send` with origin `dictated`, so the send
  gate, "sends after this turn" and the read-back are the composer's.
- **Approvals.** The app reads the title and the command, muted, then listens. The read-back
  says paths whole ("home slash work slash build"), names `&&`, `||`, `;` and `|`, reads `~`
  as "home", `*` as "star" and `$VAR` as "dollar VAR", and reads the value after `=` in an
  option. It takes "allow", "approve", "yes", "deny" and "no" alone, and "allow it", "approve
  it" and "deny it". "yes" is an Allow (owner, 2026-11-01), because the answer counts only for
  the request whose read-back finished before the owner spoke: if the request changes while the answer is
  transcribed, the answer is dropped and the new request is read. A lone "allow" or "deny" in
  an ordinary turn never answers. An unclear answer asks once more, then waits for a tap. Voice
  may Allow only a command of at most 100 characters and 16 words (the banner's body budget,
  16.2), with no command substitution (`$(` or a backtick), or an edit of at most three files;
  anything else can be denied by voice and allowed by tap. At Safe, `voice.yes_allows` is
  off and "yes" asks again (20.3); not built yet. The large
  approval card's Allow and Deny always work by tap. The answer carries `via: voice`.
- **Confirm before send.** The waiting text stays until "send" (said alone) or a tap sends it;
  other words replace it.
- **End.** End and Keyboard stop everything at once: a turn being transcribed sends nothing, and
  no step reopens an ended voice mode. When voice mode ends with a reason (microphone off, no
  engine on, the idle limit), the view stays with the reason, Close, and Open Settings for the
  microphone.
- **Questions** are not read with their options yet: voice mode says "The agent asks a question.
  Answer it on screen."
- **Background.** While voice mode is open, the fleet keeps that hab's connection when the app
  goes to the background (`Fleet.holdForVoice`); the release closes it if the app is still in
  the background. The iOS app has the `audio` background mode for this.
- **Dictation.** The composer's microphone writes heard words into the draft and replaces them
  with the engine's text on stop. A long press opens voice mode. In the terminal view a
  microphone in the toolbar sends the text as one `pane.input` write on stop, with control
  characters turned into spaces and no Enter, with a space before each one after the first. It
  passes the pane's session, so the hab applies its vocabulary. Closing the terminal view stops
  it. A dictation that did not start or wrote nothing says why. Dictation ducks other audio
  instead of stopping it. There is no keyboard bar yet (5.1). Bracketed paste needs the pane's
  paste mode, which the app does not track yet.
- **Settings › Voice.** The three engines in order, each a switch (defaults: hab and Apple on,
  Parakeet off). The Parakeet row shows only where offered, with its download. Switches for the
  summary request, spoken commands, confirm before send, barge-in, a noisy room, and the pause
  before send (0.8 to 2.0 s).
- **Not built yet.** The chime and banner for another session (13.1), a reply voice per language
  (13.6), the vocabulary hints to the on-device
  engines (`voice.md`, Recommendation 2), and the language setting for dictation (13.4).
- **Known limits.**
  - No shared golden file yet checks that the hab's Ogg reader takes the app's files; the
    framing was checked by hand against the hab's reader.
  - The Opus encode, the PCM conversion and the turn's sample copy run on the main actor: a
    short hitch after a long turn.
  - SpeechAnalyzer and the 100 ms tick run for all of voice mode, in the background too. Battery
    over a 10-minute idle session is an owner device measurement.
  - The app never asks for speech recognition permission; SpeechTranscriber is expected to need
    none. A device run confirms it, and the footer says when live words are off.
  - A speech that never ends (no voice installed, an engine restart) stops after its expected
    length plus 5 s.
  - A second window's dictation leaves the audio session as the first window set it (voice
    mode's echo cancellation stays on).

---

## 14. Usage

The owner sees each harness account's usage limits before hitting them.

### 14.1 Data

- Each adapter reports usage for the account its harness uses: an account key, a label (the plan
  name, such as "Max 5x" or "Plus"), the harness, and a list of windows. A window has a name ("5h",
  "7d", or a model name), a used percentage, a reset time, and when it was read. It has a length
  and a start when the harness's data defines them. An account whose source drifted carries a
  degraded mark (14.5).
- The account key is stable and not secret: a hash the adapter makes from an account or
  organisation id the harness exposes. With no such id, the key is a hash of the hab, the harness
  and the harness's data directory (`harness-adapters.md`, Usage). No harness gives an id today,
  so every key is per hab: one account used on two habs shows as two accounts, and the merge by
  key below never joins them.
- **Documented interfaces only.** The adapter reads usage from what the harness already gives it:
  hook payloads, status-line input, transcript or rollout records, or the harness's own server
  protocol. Confirmed in Part 4 and built: Claude Code's status-line input carries its rate-limit
  windows in recent versions; Codex's rollout token-count events carry its primary and secondary
  windows; opencode reports none (`harness-adapters.md`, Usage). The adapter never reads credential files and never
  calls a private usage endpoint with the user's token.
- The hab sends usage with its state and on change (`usage.list`, `usage.updated`, 17).
- The app merges readings from every hab by account key. The newest reading wins. The card says
  which hab it came from and how old it is.

### 14.2 The usage sheet

- Opens from the footer bars (3.7), a widget, or Settings.
- One card per account: harness icon, plan label, harness and hab, and the age of the reading.
- One row per window: name, a bar, the used percentage, and the time to reset ("1h 58m",
  "4d 23h").
- **Pace marker.** A tick on each bar shows where usage would be if spent evenly over the window:
  the share of the window already gone. Left of the tick is under pace; right is over.
- **Status line** under the rows: "5h limit reached · resets in 1h 58m", "Over pace on 7d", or "On
  pace".
- **Alert me when free.** When a window is at 100 per cent, a button "Alert me when 5h is free"
  appears (14.4).

### 14.3 Warnings

- At set thresholds per window (default 80 and 95 per cent), the app shows a banner while open.
- With push set up, the hab whose reading crossed a threshold sends a passive push, at the
  thresholds each device sent on `push.register` (`usage_thresholds`; not built). The collapse id
  is the account key, the window and its reset time, so two habs that see the same account
  collapse into one notification on the device.
- A per-device setting turns usage warnings off, or changes the thresholds.

### 14.4 Alert me when free

- The app schedules a local notification at the window's reset time. It needs no hab, no
  connection and no push.
- When a newer reading moves the reset time, the app moves the notification. When a fresh reading
  shows room before the reset time, the app replaces the scheduled notification with one now
  ("Claude 5h has room again") and ends the request: the owner asked to know when it is free.
- The notification says "Claude 5h window has reset" and opens the usage sheet.

### 14.5 Edge cases

| Case | Behaviour |
|---|---|
| A stale reading (older than the window, or than an hour) | Shown grey with "as of <time>" |
| The reset time passed with no new reading | The row says "reset at <time>; no reading since" and the bar shows empty, dashed |
| A harness reports a percentage but no reset time | The row shows the percentage only; no "alert me" button |
| An API-key account with no windows | The card says "No limits reported" |
| Two accounts on one harness (work and personal) | Two cards, two footer bars |
| The same account on two habs with no account id | Two cards, each labelled by hab |
| A reading above 100 per cent | Shown as 100 per cent and "limit reached" |
| The phone's clock is wrong | Reset times come from the hab as absolute times; the app moves them by the offset of the hab's clock, measured at each hello (14.6) |
| The harness changes its format | The adapter marks usage degraded when no window reads, or a window it knows fails to parse. The footer, the card and the widget say "Usage unavailable", never a wrong number; the card adds the hab's one-line reason. Drift in a key the adapter does not know is logged only while the known windows read (`harness-adapters.md`, Usage) |
| A hab too old to send `length_s`, `starts_at` or its `time` | The app reads the length from the window name and applies no clock offset |

### 14.6 Built choices (task-dj1wa3dx)

Code: `apple/FlightdeckKit/Sources/FlightdeckShared/Usage.swift` (readings, window math, the
widget snapshot), `FlightdeckKit/Usage/` (book, alerts, center, views), `apple/Widgets/UsageWidget.swift`.

- **Feed.** Usage is not in the snapshot. After each snapshot, a connection whose welcome lists the
  `usage` capability calls `usage.list`; `usage.updated` events follow. An answer older (by seq)
  than an event already applied for the same account does not undo it. A hab without the
  capability gets no call. Readings stay across reconnects.
- **Merge.** The book keys accounts by account key. The newer reading wins, by its newest
  `read_at`; an account with no windows counts as read when it arrived. The same reading from two
  connections (one hab reached two ways) stays with the connection that has it. When that
  connection goes offline, the account moves at once to an online connection that holds the same
  reading. A read time that does not parse keeps the time held for the same values, so the same
  reading again is no change. The same reading again from the same hab keeps its arrival time, so
  an account with no windows ages. A degraded account with no windows (the hab read nothing)
  never replaces another hab's reading that has windows, is not degraded and is not stale. Such
  a reading from another hab replaces a held degraded account with no windows, whatever its
  read time: a hab that read nothing has no read time to compare. A hab that stops reporting an account leaves its
  last reading. Removing a hab removes its accounts.
- **Last known values.** The book starts from the widget snapshot, so cards, footer and widget show
  the last readings with their age before any hab connects. An account whose hab is not online
  shows "<hab> offline · as of <time>" and grey bars. Seeded accounts keep the connection id they
  came from, which includes the config index: after a reorder of the hab list, a seeded account
  shows the label and online state of the hab now at that index until a new reading arrives. Once
  the config and SSH habs are added at launch, seeded accounts of a hab no longer in the app go,
  from the book and the widget.
- **Demo mode** posts no notification and keeps its readings and alert marks in a defaults suite
  of its own, emptied at each start. The widget never sees demo accounts.
- **Window length** is the hab's `length_s`; else its `resets_at` less `starts_at`; else, from a
  hab that sends neither, read from the window name (`5h`, `7d`, `30m`, `2w`, `7d opus`,
  `spend monthly` as 30 days). A length of 0 counts as none. A window with no length (a model
  name, a spend period with no fixed length) gets no pace tick, no pace status and is stale after
  an hour. The widget snapshot keeps the length and start; a snapshot an earlier build wrote
  still reads.
- **Usage unavailable.** An account the hab marks `degraded` shows "Usage unavailable" in place of
  its numbers: in the footer (no bar), on the card (no window rows; the hab's one-line reason
  under it, kept on purpose for power users, up to two lines)
  and in the widget (no ring; the Lock Screen gauge shows its plain icon). The windows it still
  holds are kept, not shown. It sorts with the accounts that have no windows. It posts no
  warning, and a waiting "alert me when free" request stays until its reset time; the card keeps
  that request's cancel button. The mark set or cleared is a change, to the widget too. The
  Live Activity shows no usage, so it has nothing to change.
- **Clock offset.** At each hello the connection takes the welcome's `time` against the middle of
  the hello's round trip: the hab's clock less this device's, per hab connection. A round trip
  over 1 s held a transport start (the Mac's `ssh` connect and login run inside it), so the
  connection measures against the round trip's end instead: the hab writes the welcome once the
  transport is up. A welcome with no `time` clears the offset. Each account the connection
  receives keeps the offset in force when it arrived, so a reading held across a reconnect
  (to another hab, an older hab, or a hab whose clock stepped) keeps its own shift. A reading
  that comes back unchanged keeps it too: after a hab is upgraded to send `time`, its first
  readings stay unshifted until they next change, within minutes. The book
  moves every hab time (`resets_at`, `read_at`, `starts_at`) onto this device's clock by that
  offset as it reads the account, so reset countdowns, pace, staleness, warnings, "alert me
  when free" and the widget all run on local time. An offset under 1 s is measurement noise and
  is not applied. Two times within 2 s count as one when the book compares readings: one hab
  reached two ways measures its offset a little apart, and a reconnect measures it again;
  neither is a change, and a window that matches keeps its held times.
- **The offset trusts the hab's clock.** Claude Code's and Codex's reset times are the
  provider's own times, not the hab's; the book shifts them as hab times all the same. That is
  right when this device's clock is wrong. When the hab's clock is wrong, its reset times move by
  the hab's error. The hab does not say which times are the provider's; the reference tools do
  not handle this either.
- **Stale** means older than an hour, or than the window when shorter. An account's age follows
  its newest window: a window the harness stopped sending greys only its own row. **Tightest** window: the
  highest percentage that counts now (a window whose reset passed counts as 0), ties to the later
  reset. The footer orders accounts tightest first.
- **Over pace** means used above the share of the window gone. The status line names the first
  full window, else the first window over pace, else says "On pace" when any window has a pace.
- **Warnings** are local notifications, posted by the app when a reading arrives (the hab sends no
  usage push yet). Thresholds default to 80 and 95, kept per device; the sheet holds the switch
  and the thresholds (50 to 100 in steps of 5, each stopping 5 short of its neighbour, so two
  thresholds never merge). One warning per account, window, reset time and
  threshold: reset times within ten minutes are one window (Codex derives them). A jump past two
  thresholds posts the higher one. A stale reading, or one whose reset passed, warns about
  nothing. The marks persist, so a restart does not warn again. A window with no reset time is
  one window forever; its marks clear when usage falls below them, so the next climb warns again. The notification id is the
  account and window, so 95 replaces 80 in Notification Center.
- **Alert me when free** uses a calendar trigger at the reset time, as UTC components, so it fires
  at that instant across sleep and a time-zone change, and a reset on the night clocks go back is
  not ambiguous. A reset time that moves by less than a minute keeps the scheduled notification. A request whose reset time passed ends
  quietly; the scheduled notification already fired. A reading that is still full keeps the
  request, with or without a reset time. A reading that lacks the window keeps it too: a missing
  window says nothing about room. Posts and cancels reach the notification center one at a time,
  in call order.
- **Taps.** A usage notification, a widget and the footer open the sheet at the account
  (`flightdeck://usage?account=<key>`). Settings (Connections) opens it too; on iPhone the
  Connections sheet closes first, also when a notification or widget tap arrives while it is up.
- **Debug.** `-FlightdeckOpenUsage <key|all>` opens the sheet at launch (Debug builds).
- **Tests.** `UsageTests.swift` (math, merge, warnings, alert when free, snapshot, the feed from a
  fake hab; the hab's length and start, the name fallback, unavailable accounts, the clock
  offset, its noise floor and tolerance, a slow hello, the offset kept per reading, a fake hab
  whose clock runs two hours behind and then sends no time, and a degraded account with no
  windows beside a healthy one). `apple/scripts/usage-live-check.sh` runs `LiveTests.testLiveUsage` against a real
  gff-hab: a hab pane pipes hand-written status lines (full, then with room) into
  `gff-hab statusline claude` once the test creates its trigger file.

Sources: Moshi App Store screens (usage sheet with 5h, 7d and per-model windows, reset times, a pace
marker, "alert me when free"; usage bars in the footer; Home Screen widgets; Watch usage, not
taken: no Watch app);
`harness-adapters.md` (Conversation and hook data); owner decision 2026-10-31 (usage from adapters,
never from private files).

---

## 15. Widgets

### 15.1 Behaviour

| Widget | Sizes | Shows |
|---|---|---|
| Usage | Small, medium | Small: one account as a ring with the used percentage, the window name, the reset countdown, and "Limit reached" or "On pace". Medium: two accounts side by side |
| Needs you | Small, medium | Small: the count and the top item's title. Medium: the top three rows with project and time |
| Lock Screen | Circular, rectangular | Circular: a gauge for the tightest usage window. Rectangular: the needs-you count and the top title |

- **Data path.** Widgets never connect to a hab and hold no key. The app writes a small snapshot
  (needs-you rows, usage readings, context percentages) into the app group's shared storage
  whenever it has fresh data, and asks the system to reload the widgets.
- **While the app is suspended.** When push is set up, the app's notification service extension
  updates the snapshot from each push it handles (an approval adds a needs-you row; a withdrawal
  removes it) and asks for a reload *(gk)*. The app never uses silent pushes to refresh widgets
  (12.1).
- **Countdowns** use the system's self-updating timer text, so a reset countdown ticks with no
  reload *(gk)*.
- **Configuration.** The owner picks which account a usage widget shows.
- **Tap** opens the usage sheet or the session.
- The Mac app offers the same widgets.

### 15.2 Edge cases

| Case | Behaviour |
|---|---|
| The snapshot is older than an hour | The widget shows "as of <time>" |
| No snapshot yet | "Open Flightdeck" |
| A Lock Screen widget on a locked phone | It shows counts and titles only, never a command. A setting hides titles too |
| The widget's account no longer reports | It shows the last reading greyed and "no recent reading" |
| The system throttles reloads | The widget may lag. Countdown text still ticks |

### 15.3 Built choices (task-dj1wa3dx)

- **Usage widget only.** The Needs-you widgets and the rectangular Lock Screen widget are not
  built yet. The usage widget lives in the iOS widget extension beside the Live Activity: small,
  medium and the circular Lock Screen gauge.
- **Snapshot.** JSON in the App Group defaults (`flightdeck.usage.snapshot`, versioned). The app
  writes it, and asks WidgetKit to reload, only when the readings change.
- **Configuration** picks the account from the snapshot's accounts; with none chosen, the widget
  shows the tightest. Medium shows the chosen account and the next tightest. The Lock Screen gauge
  shows the tightest window of that same first account, not the tightest window of all accounts.
- **Timeline.** One entry now, plus one at each future reset time and each moment a reading turns
  stale, so "Reset" and "as of" appear with no app run. With no such moment ahead, the timeline
  never asks again; the app reloads it on new readings. The countdown is relative timer text.
- **Age.** "as of <time>" and "No recent reading" follow the reading's age, not the snapshot's.
- **A chosen account the snapshot no longer holds** (its hab was removed) shows "Account not
  reporting", never another account.
- **Mac.** No Mac widget: the Mac app has no App Group or widget extension yet. Adding one means a
  sandboxed macOS widget extension and the group entitlement on the Mac app.

Sources: Moshi App Store screens (Home Screen usage widgets with ring, percentage, reset countdown,
"Limit reached" and "On pace"); WidgetKit, app groups and notification service extensions as
documented by Apple *(gk)*.

---

## 16. Notifications on Apple Watch

There is no Watch app (owner decision, 2026-10-31). Every notification works in full from its
mirrored banner on a paired Watch. The hab writes each one for the Watch's small screen first; the
phone shows the same words.

### 16.1 Rules

- **Most important first.** The title says what the agent wants or what happened. The subtitle
  says where: project and hab. The body adds only what the title could not hold.
- **Nothing cut off.** The hab fits each field to its word budget (16.2) itself. It shortens on
  word boundaries and marks a cut with "…". It never cuts inside a word, a flag or a file name, and
  never leaves the system to clip a line.
- **Plain words.** No Markdown, no emoji, no harness jargon. The project's name, not its path. The
  hab's label, not its address. Labels longer than 16 characters are refused at setup.
- **Shortening a command.** Keep the program, its subcommand and the flags with their values. Drop,
  in this order: a leading change of directory, environment assignments, the directory part of
  paths inside the project, then the middle of long quoted strings. The key part stays visible:
  "wrangler deploy --env prod", not "cd /srv/app && NODE_ENV=… wrangler…".
- **Approve only what is shown.** The banner offers Approve only when the full command, after the
  safe drops above, fits the body budget. Otherwise it offers Deny and "Open on iPhone". This
  replaces the 256-character limit as the banner rule (7.8).
- **Times as clock times.** A banner cannot count down. The push carries `expires_at`; the phone's
  notification service extension writes it as a local clock time, "Expires 14:05", before display
  *(gk)*.
- **Actions.** Approve and Deny need authentication: on the Watch, that means on the wrist and
  unlocked *(gk)*. The action runs the phone app's handler in the background, as for a lock-screen
  answer (7.3).
- **"Show commands in notifications" off** (8.3): the title becomes "Approval needed", the body
  the project and hab only, and Approve is not offered from the banner.

### 16.2 Word budgets

Budgets are maximums. The hab counts words and characters; it keeps to both.

| Kind | Title | Subtitle | Body | Actions |
|---|---|---|---|---|
| Permission | 4 words, 28 chars: verb and object, "Run wrangler deploy?", "Edit sign-in.ts?", "Write 3 files?" | 4 words: "web-app · <hab>" | 16 words, 100 chars: the shortened command or file list, then "Expires 14:05" | Approve, Deny (or Deny, Open on iPhone) |
| Question | 8 words, 50 chars: the question, shortened | 4 words: project · hab | 16 words: the options, numbered, "1 Clerk · 2 Auth.js" | Up to three options as actions, plus Reply (dictation or Scribble on the Watch). More than three: the first two, then Open on iPhone |
| Sign-in needed | 3 words: "Sign-in needed" | 4 words: project · hab | 10 words: "<harness> needs you to sign in on <hab>." | Open on iPhone |
| Error | 4 words: "Turn stopped: <project>" | 4 words: hab · harness | 15 words: the first line of the error | Open on iPhone |
| Done (when on) | 5 words: "Done: <session title>" | 4 words: project · hab | 15 words: the first sentence of the last agent message | None |
| Answered elsewhere | 4 words: "Answered on <client>" | 4 words: project · hab | 8 words: "Allowed: wrangler deploy" or "Denied: …" | None |
| Usage warning | 5 words: "Claude 5h at 90%" | 3 words: the plan label | 6 words: "Resets in 1h 58m" | None |
| Usage free | 5 words: "Claude 5h window reset" | 3 words: the plan label | None | None |
| Console command finished | 5 words: "<command> finished" | 4 words: project · hab | 6 words: "Exit 0 · 3m 12s" | None |
| Could not reach | 4 words: "Could not reach <hab>" | None | 6 words: "Open on iPhone to retry." | None |

### 16.3 Edge cases

| Case | Behaviour |
|---|---|
| The command does not fit after safe drops | Body shows its start and "…"; actions are Deny and Open on iPhone |
| The phone is out of reach of the Watch | The Watch shows the notification only if it was already delivered; an action there fails, and the phone posts "Could not reach" when it next runs |
| The phone cannot reach the hab | The phone posts the "Could not reach" notification (7.8), which the Watch shows |
| The request expires before the tap | The hab refuses it with `conflict`; the phone posts "Expired: answer in the terminal" |
| The request was answered elsewhere | The replacement push (7.5) takes the same collapse id, so the Watch shows "Answered on <client>" |
| A project or session name longer than its share of the budget | The hab shortens it on a word boundary with "…"; the command keeps its share first |
| A language with long words | Character budgets win over word budgets |

Sources: owner decision 2026-10-31 (no Watch app; notifications designed for the Watch); Moshi App
Store screens (Watch approval: project and model under the title, the command, Approve and Deny);
Moshi docs, apple-watch and notifications; mirrored notifications and their actions on watchOS
*(gk)*.

---

## 17. Calls the app needs from the hab

The calls and events below are in the API schema now; `hab-logic-api.md` 3.9 lists their final
shapes and owning stages. Until its stage, a call answers `unavailable`. The new session,
request and project fields are in the schema as optional fields; a hab leaves each out until the
stage that fills it.

| Call or field | Purpose |
|---|---|
| `push.register {device_id, token, environment, kinds}` | Store an APNs device token for this device |
| `push.register_activity {device_id, session, token}` | Store a Live Activity token for the tracked session |
| `push.unregister {device_id, token}` | Drop a token |
| `push.register` field `hide_commands` | "Show commands in notifications" off: banners carry no command (8.3) |
| `notice.raise {title, body?, level?, session?}` | Raise a notice from a hab event or a local script; event `notice.raised` |
| capability `push` | The hab has push config that works |
| `session.focus {session, active}` | Report focus: marks `done` seen, and feeds hold-while-watched (8.3) |
| `approval.answer {pane, request, choice, text?}` | Answer a pending request by id; refuses a closed id with `conflict` |
| `session.interrupt {session}` | Stop the current turn through the adapter (the chat view's Stop button) |
| `chat.page {session, before?, limit}` | Read a page of chat messages from the transcript reader |
| events for chat | Chat messages appended or updated while subscribed |
| `session.send {session, text, attachments?, origin, idempotency_key?}` | Send a message through the adapter under the input rules of 4.2. A retry with the same `idempotency_key` delivers once; the app sends a key only to a hab whose welcome lists `idempotency-keys` (`hab-logic-api.md` 3.9). `attachments` lists ids from `attachment.put`. `origin` is typed, dictated or voice, for the hab's log only |
| `attachment.put {session, name, type, size, sha256}` | Start an upload (4.4). Returns `{id, path, chunk, done}`; refuses a file over the hab's limit |
| `attachment.write {id, offset, data}` | The next run of the upload's bytes, at most `chunk`, in order. Returns `{received, done}`; the run that completes the file checks the hash |
| `attachment.get {id, thumbnail?, offset?, limit?}` | Fetch a run of an attachment, or its thumbnail, for the chat view on any device |
| event `session.turn_end {session, final_message?}` | The turn ended; names the final agent message. Voice mode reads it aloud (13.2) |
| session fields `title`, `model`, `context_pct` | The row's title, the model in use, and how full the context window is (3.4). `context_pct` is absent when unknown |
| request fields `title`, `description?`, `context_line?`, `expires_at?` | What the approval sheet and the banner show (7.2, 7.4, 16.2) |
| chat message kind `plan` | Steps with text and status, one live plan per session (4.5) |
| project fields `branch`, `ahead`, `behind`, `head` | The sidebar's project header (3.5) |
| `usage.list {}` and event `usage.updated` | Usage readings per account: key, label, harness, windows with used percentage, reset time and read time (14.1) |
| `push.register` kind `usage` and field `usage_thresholds` | Opt in to usage warnings by push, at this device's thresholds (14.3, 20.4; the field is not built) |
| `push.register` kind `command` | Opt in to a push when a long command finishes (6.1, 20.4; not built) |
| `approval.answer` field `via` | Where the answer came from (app, banner, Watch banner, voice), for the hab's log |
| `content.show`, `content.list`, `content.read`, `content.diff` and event `content.updated` | Rich content: shown files, images from tool calls, modified files with diffs (4.7). Capability `content` |
| `chat.search {query, limit}` (later) | Full-text search over chat messages on this hab (3.6) |
| `settings.get {ids?}`, `settings.set {values}`, event `settings.updated`, snapshot field `settings`, error kind `locked` (not in the schema yet) | Read and write hab-side settings, all or nothing per call, with a revision check (20.5). Capability `settings` |

Nothing new is needed for the Watch: it answers through the phone's notification actions (16).
Voice mode needs no voice call: speech becomes text on the device and goes through
`session.send`.

---

## 18. Open questions

Each question has options and a recommendation, and ends with its decision.

### 18.1 Live Activity across several habs

| Option | For | Against |
|---|---|---|
| A. One activity, one session, owned by that session's hab | No two habs write one activity; matches Moshi | Other habs' sessions show only as banners |
| B. One activity per hab | Each hab owns its own | Several activities on the Lock Screen; noisy |
| C. One summary activity for all habs | One glance for everything | Two habs would overwrite each other's content; needs a hub or app-side merge, which a suspended app cannot do |

Recommendation: **A.** It works without a hub and matches the best reference.

Decision: A (coordinator, 2026-11-01). It matches Moshi and needs no hub.

### 18.2 Face ID and the device key

`transport.md` 4.3 says the phone's key is "gated by Face ID or the passcode". The owner decided
Face ID is optional and off by default.

Recommendation: the key needs the device unlocked (this device only, never in backups). It does not
need biometrics. The Face ID setting, when on, gates the app and in-app answers. Lock-screen actions
always need unlock.

Decision: as recommended (coordinator, following the owner's Face ID decision). `transport.md`
4.3, 7.2 and 7.3 now say the key is usable only while the device is unlocked.

Later (2026-11-01, settings levels): the one Face ID setting is "Require Face ID for SSH keys"
(10.7, owner). The gate on the app and in-app answers is dropped (7.6, 20.6).

### 18.3 Bring your own APNs key versus who signs the build

An APNs key pushes only to apps of its own team. The owner's TestFlight and Developer ID builds are
signed by the owner's team, so another user's key cannot push to them.

| Option | For | Against |
|---|---|---|
| A. Push works for whoever signs the build. Others build from source under their own team and bundle id | No shared service; matches "bring your own" | Push needs an Apple Developer account and a self-built app |
| B. The owner's builds accept pushes only from the owner's habs; others get live updates only | Nothing to build | Others never get lock-screen approvals from those builds |

Recommendation: **A, with B as its consequence.** Document plainly in the push setup guide that push
needs a build signed by the same team as the key. The app works without push, so the owner's builds
still serve others for live use.

Decision: A, with B as its consequence (owner, 2026-10-31): push needs the user's own build and
APNs key; the owner's builds use the owner's key; no relay.

### 18.4 Hold banners while watched

Recommendation: hold the banner when any client has the session in focus in an active window
within the last 60 s. Never hold an approval when the focus report is older than that. Moshi's
equivalent is off by default; ours is on because it keys on the session, not the whole machine.

Decision: as recommended (coordinator, 2026-11-01). Paseo also sends nothing to a client focused on the agent
(`landscape.md` 2.9); its in-app tier is idea 4.1 (task-oe7ioczy).

### 18.5 A banner for `done`

Recommendation: off by default. The Live Activity shows it. A per-device setting turns it on, as a
passive banner.

Decision: as recommended (coordinator, 2026-11-01). A per-device setting, so a user who wants it turns it on.

### 18.6 How "answered elsewhere" withdraws a banner

| Option | For | Against |
|---|---|---|
| A. Replace by collapse id with a quiet "answered on X" | Works with the app suspended | A second, quiet notification stays |
| B. A background push that lets the app remove the banner | Clean | iOS throttles background pushes; not reliable |

Recommendation: **A**, plus B where iOS allows. Confirm against the owner's Moshi screens (7).

Decision: A, plus B where iOS allows (coordinator, 2026-11-01). A works while the app is suspended.

### 18.7 A lock-screen answer when the hab is unreachable

Recommendation: fail fast, post a local "could not reach" notice, never retry on its own. A stale
retry could approve a command the owner no longer wants.

Decision: as recommended (coordinator, 2026-11-01). Failing fast costs nothing to build.

### 18.8 Connecting before the embedded SSH server exists

Recommendation: support the fallback in Part 6 stage 4. The app shows its public key; the owner
adds it to the host's authorized keys by hand. This needs no new listener, and it is the path
`transport.md` 4.1 already opens. QR pairing replaces it once the embedded server is enabled.

Decision: as recommended (coordinator, 2026-11-01). It is the same path as the owner's decision to import an
existing SSH key (10.4).

### 18.9 Sealed push payloads

The plan already calls a `/consensus` on this. Recommendation for the first release: plain short
text through APNs, the "show commands in notifications" setting, and a payload shape that leaves
room for a sealed body later.

Decision: as recommended for the first release (coordinator, 2026-11-01), in place of the `/consensus` run. Paseo
sends a clear-text preview (`landscape.md` 2.9); a sealed body stays a later step.

### 18.10 Apple Watch

Decided (owner, 2026-10-31): no Watch app. Every notification works in full from its mirrored
banner, written for the Watch first (16).

### 18.11 Voice mode with the screen locked

| Option | For | Against |
|---|---|---|
| A. Voice mode keeps running while its view is open, screen locked or not | Hands-free in a pocket, as the Claude and ChatGPT apps do | One live connection and the audio session in the background; battery |
| B. Voice mode ends when the screen locks | Keeps the background rule in 12.1 whole | Not hands-free: the phone must stay awake and in hand |

Recommendation: **A**, ending after 10 minutes with no speech and no turn running. It is the one
exception to 12.1, and it lasts only while the owner chose voice mode.

Decision: A (coordinator, 2026-11-01). The owner asked for voice mode like the Claude and ChatGPT apps, which keep
running with the screen locked.

### 18.12 Where attachments live

| Option | For | Against |
|---|---|---|
| A. A folder inside the project, excluded with a local, untracked ignore rule | Every harness can read it with no extra permission prompt; the agent can move it into the project | A folder in the owner's tree; must never be committed by accident |
| B. The hab's state directory, outside the project | Nothing in the project | Harnesses may ask permission to read outside the project, which breaks the flow from a phone |

Recommendation: **A**, as 4.4 says. The hab writes a local, untracked ignore rule, never a
tracked file, and clears old files after 30 days. As built: `.flightdeck/.gitignore` holding
`*`, not the repository's `info/exclude` (4.4, built choices).

Decision: A (coordinator, 2026-11-01). Not a question a user sees; harnesses read the file with no prompt.

### 18.13 Reading imported OpenSSH keys

`swift-nio-ssh` takes keys as ready-made key objects. It does not read OpenSSH key files or
decrypt them (`transport.md` 1.8).

| Option | For | Against |
|---|---|---|
| A. Our own reader for the OpenSSH key format, from OpenSSH's published format description, with the system's AES-CTR and a bcrypt-based key derivation | Small, our own code, covers Ed25519 and every ECDSA curve; fixtures from `ssh-keygen` test it | We own a crypto-adjacent parser and a key derivation; both need test vectors |
| B. A dependency that already reads the format (Citadel, MIT, built on `swift-nio-ssh`) | Proven reader with bcrypt and AES-CTR | Its reader covers Ed25519 and RSA only, not ECDSA; it pulls in a whole SSH layer we do not use |

Recommendation: **A.** The format is short and documented. Test it against keys `ssh-keygen`
writes (each type, with and without a passphrase, each round count) and against OpenSSH's own
key-derivation test vectors.

Decision: A (coordinator, 2026-11-01). It covers every key type the owner may import, ECDSA included.
Built in task-1csglxtc (10.7).

---

## 19. Sources

Opened while writing this spec, 2026-10-31.

- Moshi docs: https://getmoshi.app/docs/live-activity, https://getmoshi.app/docs/notifications,
  https://getmoshi.app/docs/apple-watch, https://getmoshi.app/docs/hooks
- Moshi's App Store page: nine screens and the store description, seen as screenshots the owner
  sent. The example hosts and projects in them are not reproduced here.
- Happy repository (MIT): https://github.com/slopus/happy, app source in `packages/happy-app`
  (message view, tool view, permission footer); https://happy.engineering/docs/how-it-works
- Heeler repository (Apache-2.0): https://github.com/ZingerLittleBee/Heeler (README)
- rootshell repository (MIT): https://github.com/kitknox/rootshell (README)
- swift-nio-ssh (Apache-2.0): https://github.com/apple/swift-nio-ssh, `NIOSSHPrivateKey.swift`
  on the main branch, and pull request 219 (RSA keys, open)
- Citadel (MIT): https://github.com/orlandos-nl/Citadel, `OpenSSHKey.swift`
- Our specs: `docs/specs/transport.md`, `docs/specs/hab-logic-api.md`,
  `docs/specs/harness-adapters.md`

Not opened: Apple's ActivityKit, UserNotifications, APNs, WidgetKit, Speech and watchOS
notification pages. Facts marked *(gk)* need a check
against them, or a device test, in stage 7.

Internal inputs: the project's planning notes and research reports on native apps and
notifications. None of their private detail is reproduced here.

---

## 20. Settings and levels

Owner decisions, 2026-11-01. Flightdeck is a power-user system: it adds few confirmations, and
few of its choices are settings. One level picker sets the few levelled settings at once. This
section is the registry; the sections it links hold each setting's behaviour. Built: nothing of
this section yet (task-yfmrj6yc). Today each built setting has its own switch where its section
says.

`ssh.require_face_id` depends on the Face ID branch (not yet on main):
it makes Face ID one app-level check before each dial, over every key, so a change never makes
a new key. Until it lands, 10.6 and 10.7 on main still describe per-key switches and an enclave
flag on the generated key.

### 20.1 What becomes a setting

- **No extra confirmation by default.** Flightdeck adds a confirmation only where an action
  crosses a line the harness does not see: another host, someone else's code, a stale request.
- **The harness already asks: no confirmation.** Flightdeck never repeats a question the
  harness's own permission prompt asks. Harness permissions belong to the harness.
- **A setting only where people really differ.** A choice with one right answer for nearly
  everyone is a fixed behaviour (20.6). A limit that only an operator tunes stays in the hab's
  config file with no app row (20.6).
- **A guard against someone else's code is never a setting.** The foreign worktree setup yes
  (`hab-logic-api.md` 3.10) stays on at every level. Paseo asks there too (`landscape.md`).
- **Each setting has a default.** Balanced holds the defaults. A hab's built-in defaults are the
  Balanced column, so a change to that column changes the hab's defaults with it.

### 20.2 Levels

| Level | Meaning |
|---|---|
| Safe | Every confirmation and check Flightdeck offers is on |
| Balanced | The defaults: hands-off, with the one cross-host confirmation kept |
| Bypass permissions | No confirmation or check that Flightdeck adds |
| Custom | Shown, never picked: the values match no level |

- **The picker** sits at the top of Settings. It shows the level whose column matches every
  levelled setting (20.3), else Custom.
- **Custom is derived, not stored.** One rule decides a match:
  - An app-side setting matches a column when this device holds that column's value. A setting
    this device does not show (`ssh.require_face_id` on the Mac) is left out.
  - A hab-side setting matches a column when every online hab with `settings` holds that value.
    A locked row (20.5) and an offline hab do not break the match; such a row shows "Set in the
    hab's config" or "Differs on <hab>". A hab without `settings` is left out too.
  - **Exceptions.** When a locked or offline row holds a value other than the matched column's,
    the picker shows the level with a count: "Safe, 1 exception". A tap on the count scrolls to
    the first such row. So Safe never shows bare while a hab's config enforces a weaker value.
    Design call: a count, not Custom, because the user cannot change a locked row from the app.
  - Changing one levelled setting by hand shows Custom at once; setting it back shows the level
    again. A write from another device to a hab-side setting can also flip this device's picker
    to Custom, since levels are per device and hab-side values are shared.
- **Picking a level** writes that column's values at once, with no confirmation. App-side values
  apply on this device. Hab-side values go to every hab: at once to each online hab, and through
  the queue (20.5) to each offline one. Preferences (20.4) never change.
- **The level notice** says what changed and what did not: "Safe: 3 settings changed", or "Safe:
  2 of 3 changed. Require approval for memories on <hab> was changed from another device." A
  refused Face ID check, a locked row, a stale revision and a dropped queued write each name
  their row. A row that did not take keeps the picker at Custom.
- **Turning a check off can ask for authentication.** "Require Face ID for SSH keys" asks for
  Face ID or the passcode before it turns off (10.7), also when a level turns it off. A refusal
  keeps it on. Turning it on asks nothing and makes no new key.
- **The cost of Safe.** With `ssh.require_face_id` on, every dial asks for Face ID. A banner,
  Watch or Live Activity answer to an SSH hab then fails at once with "Open Flightdeck to answer"
  (10.6 on the Face ID branch), since a background action cannot ask.
- **Levels are per device.** The app keeps the last picked level in its defaults. A phone at Safe
  and a Mac at Balanced is fine for app-side settings. Hab-side settings are shared: the last
  write wins (20.5).
- **A new hab.** On every connect, at the first snapshot or `settings.get` after hello, the app
  checks each levelled hab-side setting. One whose `source` is still `default` (no client wrote
  it, no config set it) takes the picked level's value, written with the revision read there; a
  locked one is skipped. Any other value stands, and the row shows
  "Differs on <hab>" when it does not match, as `logbook.md` 9.1 does.
- **What Bypass never touches.**
  - Harness permissions: permission modes, allow and deny rules, the harness's own prompts.
  - The harness's own bypass flag, such as `--dangerously-skip-permissions`. A level never sets
    it; the user picks it per session, in the harness or its config.
  - The foreign worktree setup yes (20.1).
  - Unlock for an answer from a banner or a Live Activity (7.6): the device key signs only while
    the device is unlocked (`transport.md` 7.2).
  - The voice Allow limits (13.7): a long command or a broad choice is allowed only by tap.
  - Pairing, host key pins and device ids (`transport.md`, `mesh.md` 4).

### 20.3 Registry: levelled settings

| Id | Title | Area | Type | Safe | Balanced | Bypass | Stored | Source |
|---|---|---|---|---|---|---|---|---|
| `confirm.start_elsewhere` | Confirm before an agent starts work on another host | Agents | bool | `true` | `true` (owner to confirm) | `false` | Hab, every hab | `capcom.md` 6.1, `mesh.md` 11 Q1 |
| `approval.max_age` | Remote answers expire after | Approvals | integer minutes, 0 = never, else 1 to 1440 | 60 | 60 | 0 | Hab, every hab | 7.4 |
| `logbook.approval` | Require approval for memories | Agents | `"off"` or `"agents"` | `"agents"` | `"off"` | `"off"` | Hab, every hab | `logbook.md` 9.1, `instructions-mcp-flight-rules.md` 12 |
| `voice.yes_allows` | "Yes" counts as Allow | Voice | bool | Off | On | On | App, this device | 13.3, 13.7 |
| `ssh.require_face_id` | Require Face ID for SSH keys | Security | bool | On | Off | Off | App, Keychain item; iOS only | 10.7 (Face ID branch) |

- **`confirm.start_elsewhere`** stays on in Balanced. Capcom's tools are allowed in its own
  folder (`capcom.md` 7.1), so no harness prompt stands before a start on another host. Off: the
  action runs at once and the card shows as a record. The owner's first ruling (default on)
  stands. Owner to confirm: the later power-user rule (default to no extra confirmation) could
  make it off in Balanced.
- **`approval.max_age`** bounds a remote Allow on an old request (7.4). At 0 the hab sets no
  limit: a request takes a remote answer until the answer path's own deadline, and with no such
  deadline no countdown shows (7.4). Not built: the hab sends no limit yet.
- **`logbook.approval`** is one setting for both kinds of memory: Logbook writes by agents and
  each harness's native memory. It replaces the per-harness switches
  (`instructions-mcp-flight-rules.md` 12). The user's own writes always land at once. The word
  set is client-sent, so it is closed (`hab-logic-api.md` 3.3).
- **`voice.yes_allows`** off: only "allow", "approve", "allow it" and "approve it" answer Allow,
  and "yes" makes the app ask again. The deny words are the same at every level.
- **`ssh.require_face_id`** is one app-level check over the generated key and every imported key
  (10.7 on the Face ID branch). It keeps its level: a change keeps every key and its public
  line, and starts no reconnect. It replaces the Face ID setting that 7.6 named for approvals
  and app open (20.6). The Mac app shows no row (the branch puts it on iOS only), so a level
  pick on the Mac leaves it alone.

### 20.4 Preferences: settings with no level

These differ by taste or by device. They show on the Settings page and in its search, but the
picker never changes them.

| Id | Title | Area | Type | Default | Stored | Source |
|---|---|---|---|---|---|---|
| `notify.done` | Notify when a turn finishes | Notifications | bool | Off | App; push kind `done` on each hab | 8.2, 18.5 |
| `notify.show_commands` | Show commands in notifications | Notifications | bool | On | App; `hide_commands` on `push.register` | 8.3 |
| `notify.command_done` | Notify when a long command finishes | Notifications | bool | Off | App; push kind `command` on each hab (not built) | 6.1, 8.2 |
| `notify.tap` | Open a notification in | Notifications | chat, terminal or smart | Smart | App | 9.1 |
| `usage.warnings` | Usage warnings, with thresholds | Usage | off, or a list of percentages from 50 to 100 | On, 80 and 95 | App; push kind `usage` with `usage_thresholds` on `push.register` (not built) | 14.3 |
| `widget.hide_titles` | Hide titles on the Lock Screen | Widgets | bool | Off | App Group | 15.2 |
| `mac.keep_connected` | Stay connected in the background | Connection | bool | Off | App (Mac) | 12.1 |
| `hab.permission_wait` | Answer permissions from the app (blocking hook) | Approvals | integer seconds, 0 = off, else 1 to 120 | 0; the switch sets 30 | Hab, this hab only | 7.4, `harness-adapters.md` (blocking hook) |
| `voice.*` | Engines (hab, Parakeet, Apple), summary request, spoken commands, confirm before send, barge-in, noisy room, pause | Voice | As 13.7 | As 13.7 | App | 13.7 |
| `voice.language` | Dictation language | Voice | a language tag, or system | System | App (not built) | 13.4 |

- The Capcom host picker (`capcom.md` 7.3) and each hab's push status are controls, not
  settings. They sit in Settings with no id.
- **`hab.permission_wait`** is per host: the app writes it to one hab. The row is a switch (0 or
  30) with a stepper for 1 to 120 s once on. A change applies to the next hook; a hook already
  waiting keeps its wait. The hook config always passes `-wait`, so turning it on needs no hook
  change. A `-permission-wait` flag given on the command line, or a non-empty
  `$GFF_HAB_PERMISSION_WAIT`, locks the value (20.5).
- **Pushed preferences reach the hab.** A preference that changes what a hab pushes while the
  app is closed goes to each hab on `push.register`: the kinds (`done`, `command`, `usage`),
  `hide_commands`, and the usage thresholds. The hab pushes a usage warning only at a threshold
  the device sent.

### 20.5 Hab-side settings and the API

A hab-side setting lives on the hab that enforces it. The app writes it through the hab API.

- **Where the hab keeps them.** Client writes go to `<state>/settings.json`, synced before its
  rename, with each setting's revision. A value set in the hab's config file, or by a flag given
  on the command line or a non-empty environment variable, locks the setting: the hab enforces
  that value and refuses writes. The app shows a locked row read-only, with "Set in the hab's
  config".
- **The config form.** The hab's config file (`hab.toml`) takes a `[settings]` table with quoted
  keys, since a bare dotted key would make nested tables:
  `"confirm.start_elsewhere" = false`. No existing key moves.
- **When the config is read.** At start, and again after a change to the file has held for one
  check, as the peer file is (`mesh.md` 4). A lock that appears, changes or goes raises the
  setting's revision and sends `settings.updated`.
- **One value everywhere.** For a setting stored on every hab, the app writes the same value to
  each hab. The app and `gff lb status` show a hab whose value differs.
- **The queue.** A write to an offline hab waits in a queue the app saves, so it survives a
  relaunch. It holds the value, the revision the app last saw and that hab's epoch. A later
  write to the same id on the same hab replaces the queued one. When the hab connects:
  - Same epoch: the app sends the write with its revision. A `conflict` (another device wrote
    since) or `locked` drops that row by the error's `data.id`, as for a level pick (below); the
    hab's value stands, and the level notice names the row.
  - New epoch: revisions are not compared across epochs (`hab-logic-api.md` 3.5), and a lost
    `settings.json` can restart them. The app reads the snapshot first. When the value there is
    not the one the app last saw, another write won and the queued write drops. Otherwise the app
    sends it with the snapshot's revision.

The API addition, by name and shape. It follows `hab-logic-api.md` 3.3 to 3.6; the schema comes
with the build stage:

| Call, event or field | Shape | Purpose |
|---|---|---|
| capability `settings` | — | The hab serves everything below |
| snapshot field `settings` | optional array of `{id, value, default, revision, source}` in a `host` scope snapshot, as `agents` is | The current hab-side settings, so the app needs no read after a (re)subscribe |
| `settings.get {ids?}` | → `{settings: [{id, value, default, revision, source}], seq}` | Read every hab-side setting, or the named ones, outside a subscription |
| `settings.set {values: [{id, value, revision}]}` | → `{settings: [{id, value, default, revision, source}], seq}` | Write one or more values, all or nothing on this hab. A level pick is one call per hab |
| event `settings.updated` | `type` `settings.updated`, `id` the setting id, `revision` the new revision, `data {value, source}` | A value changed: by a client, or by a config read. Sent to `host` scope subscribers only |
| error kind `locked` | `data.kind` `locked`, `data.id` the setting | The setting is set in the hab's config; the client shows it read-only |
| error field `data.id` | the id of the first refused row, on every `settings.set` error | Tells the client which row refused the call |

- **`source`** is a server-sent word set, so it is open: `default` (no client wrote it, no config
  set it), `client` (a client wrote it), `config` (locked by the config, a flag or the
  environment). An unknown word reads as `config`: read-only.
- **`revision`** is the setting's object revision (`hab-logic-api.md` 3.5): stored with it, raised
  by one on each change, compared only within an epoch. It starts at 1 when the hab first lists
  the setting.
- **Errors, by `kind`.** A `revision` that is not the current one is `conflict`. A locked id is
  `locked`. An unknown id is `not_found`, so a newer app can show "not on this hab" for a
  setting an older hab lacks. A value outside the setting's type is `invalid`. Any error refuses
  the whole call, and every `settings.set` error carries `data.id`, the first refused row.
- **A refused level pick.** On `conflict` or `locked`, the app drops the row `data.id` names and
  sends the rest once more, with their revisions unchanged. A second refusal ends the try on
  that hab; the rows left show their hab's value. A `not_found` or `invalid` drops that row the
  same way. The level notice names each dropped row.
- **`settings.updated` is not ephemeral:** the settings are in the `host` snapshot, so the event
  carries a revision and a client orders it against the snapshot like any other object.
- **The hab applies a change at once:** to the next start elsewhere, the next request's expiry,
  the next memory write, the next permission hook.
- **A hab without `settings`** keeps the values from its config and flags, and the app shows its
  rows read-only.

Built choices on the hab (task-yfmrj6yc, `internal/settings`):

- **What each setting drives.** `hab.permission_wait` sets the blocking hook's wait for the next
  hook; a hook already waiting keeps its wait. The other three are stored and served only: their
  consumers are not built (`confirm.start_elsewhere` waits for Capcom, `approval.max_age` for the
  request expiry of 7.4, `logbook.approval` for Logbook).
- **The flag lock.** `-permission-wait` given on the command line, even as 0, or a non-empty
  `$GFF_HAB_PERMISSION_WAIT`, locks `hab.permission_wait` in whole seconds, rounded. A flag wins
  over the config file.
- **The config file.** An entry of `[settings]` that names no hab-side setting, or holds a value
  outside its type, is logged and left out; it locks nothing. A file that does not parse stops
  gff-hab at start, since it cannot enforce what it cannot read; at a later read it keeps the
  locks from the last good read. A lock that appeared, changed or went while gff-hab was stopped
  raises the revision at the next start (`settings.json` keeps the last lock it saw).
- **Checks, in order per row:** unknown id, lock, revision, value. A row named twice in one call
  is `invalid`. A save that fails is `unavailable`, names the first row and changes nothing. A
  written row's source becomes `client` even when its value did not change, so the new-hab check
  leaves it alone. The result lists the written rows. Params that do not decode carry no
  `data.id`: no row was read.
- **`settings.get` with `ids`** leaves out an id the hab does not know, so a newer app reads
  absence as "not on this hab" without a failed call.
- **A `settings.json` that does not parse** is logged and the settings start from the defaults at
  revision 1; a new epoch orders nothing across it.
- **Integers** are whole JSON numbers in range; `30.0` and `1.5` are refused.

### 20.6 Dropped, not settings

Each candidate below is a fixed behaviour now.

| Candidate | Fixed behaviour | Why |
|---|---|---|
| Approve from the lock screen without unlocking | An answer from a banner or a Live Activity needs the device unlocked | The device key signs only while unlocked (`transport.md` 7.2), so the switch changed nothing (8.7) |
| Face ID before in-app approvals, app open and a voice Allow (7.6) | No Face ID gate of our own beyond the SSH keys | `ssh.require_face_id` covers each dial, and the passcode guards the device. iOS 18 and later can require Face ID for any app from the Home Screen *(gk)*, which covers a shared phone. Moshi puts Face ID on keys only |
| Worktree setup trust for rebased and cherry-picked commits | They count as own when their source does (`hab-logic-api.md` 3.10) | A false alarm costs one yes, and nobody wants more of them. Not levelled: it guards against someone else's code |
| Alert when free fires at once | A fresh reading with room fires the alert now (14.4) | The user asked to know when it is free |
| Offline cached images | Content the app already holds shows while its hab is offline (4.7) | Showing what is in hand costs nothing; Paseo shows cached rows offline |
| Changed-files scope | The list holds the session's own edits and the folder's git status, each marked (4.7) | The marks tell them apart. A filter in the list can come later; it is not a setting |
| Spoken summary shown in chat | The chat hides the "Spoken summary:" request on the user's message (13.7) | The transcript and the TUI keep it; the request itself has its switch |
| Notification prompt timing | The app asks for notification permission when the first hab with `push` connects (8.7) | Asked in context; a user with no push never sees the prompt |
| Keep photo location and camera data | Always stripped before upload (4.4) | No agent needs where a photo was taken |
| Send the original photo | Downscaled to what the harness accepts (4.4) | The harness takes no more; a larger file only costs time |
| Per-harness memory approval switches | One `logbook.approval` (20.3) | One rule for memory is easier to trust |
| Attachment retention (30 days) and size limit (25 MB) | Hab config only, no app row (4.4) | An operator limit for the host's disk, not a per-person choice |
| The long-command threshold (30 s) | Fixed, no app row (6.1) | Nobody tunes when a badge appears; the push for it is `notify.command_done` |
| The hab speech engine (`[speech] enabled`) | Hab config only, no app row (`voice.md`) | Whether a host serves speech is an install choice; it changes the hab's capabilities, which only a new hello reports. A device that does not want it turns off its hab engine (`voice.*`) |

### 20.7 The Settings page

- **One page, grouped by area:** Agents, Approvals, Notifications, Voice, Usage, Widgets,
  Connection, Security, and Habs (each hab's push status, keys and the Capcom host picker).
- **The level picker** heads the page, above the groups.
- **Search** filters rows by title and id across groups. Each row shows its id in small type, so
  a doc or an issue can name it.
- **A hab-side row** says where it is stored: "On every hab" or "On <hab>".
- **Today's pages** (Settings › Notifications, › Voice, › SSH and the usage warnings) become
  groups of this page. Their sections keep the wording of each row.
