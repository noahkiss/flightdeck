# Swarm study: Capcom as an always-on service, agent groups, watchers

A study that new plan parts build on. Owner direction of 2026-11-06 (plan decisions (l) to (o))
sets the scope:

- **A. Capcom as an always-on service** is the core (section 2).
- **B. Agent groups** are a first-class, optional capability, modelled on the coordinator
  workflow that builds Flightdeck (section 3). Section 4 maps the friction that workflow has today
  to the pieces that remove it.
- **D. Watchers** are long-lived, event-driven rounds (section 5).
- **E. iMessage** is optional and minor (section 6).

## 1. What Flightdeck already has

| Piece | What it gives | Where |
|---|---|---|
| Channels | Agent-to-agent text on any host, deliver when idle, hold while blocked, outbox | `mesh.md` 6 |
| `hand_off`, `start_session`, confirmations | Work to a project's agent; a seeded session on any host | `capcom.md` 3, 6 |
| Capcom read tools, read scope | Every host's catalog, sessions, transcripts, notices, Logbook | `capcom.md` 4, 5 |
| Trust card | `blocked:trust` and `trust.answer` for a harness's folder trust prompt | `app.md` 7.10 |
| Usage readings, `context_pct` | Plan windows per harness; context use per session | `usage`, `hab-logic-api.md` |
| Rounds | `fresh`, `resume` and `fork` runs; the lock that never types into a session in use | `rounds.md` 5, 6 |
| Logbook | Records on every host, `claim`, proposals, a brief | `logbook.md` |
| Worktrees | One checkout per writer, setup and teardown | `hab-logic-api.md` 3.10 |
| App | Chat view, image and file attachments, speech input, push with actions | `app.md` 4, `voice.md` |
| Spawn verb (open) | A teammate in any harness by alias, result through channels | task-6iwat66b |

## 2. A. Capcom as an always-on service

### 2.1 Architecture

| Part | Choice |
|---|---|
| Process | `gff-hab-capcom`, an agent loop on the Claude Agent SDK, its own service job on the home host. The SDK is TypeScript or Python, so it is a sidecar, not part of gff-hab |
| Input | The SDK's streaming input: one long-lived loop; messages queue; an interrupt stops a turn |
| Tools | The agent kit through `gff-hab mcp`, plus the new tools in 2.3. No shell and no file writes |
| Approvals | `canUseTool` raises Flightdeck cards; the hab still runs confirmed starts |
| Channels | The engine holds a channel name, so agents it dispatched report back to it |
| Memory | Logbook. Facts and preferences become records; each fresh start reads the brief |
| Context | Managed by the engine (2.2) |
| Billing | The owner's plan login: the SDK "still draw[s] from your subscription limits" (support article, 2026-10-30). Guardrails in 2.6 |
| Later | A Codex SDK engine behind the same contract |

### 2.2 Context with no user effort

- **Rolling summary.** After each turn the engine updates a short summary of the thread.
- **Re-seed.** Past a size limit or a quiet hour, the engine ends the SDK session and starts a
  fresh one with the summary, the Logbook brief and the last few messages.
- **Compaction** stays on as a safety net; its `PreCompact` hook writes the summary first.
- **Memory writes** land in Logbook at once; tasks and findings stay proposals.
- **One thread to the user.** A quiet divider, "earlier messages summarised", opens the summary.

### 2.3 Tools: where each runs

The engine runs on a hab; the phone holds the person's context. The app sends a **device
context** block with each message: place (coarse location and place name), local time and time
zone, the next few calendar events. Nothing is tracked in the background.

| Tool | Runs on | Status |
|---|---|---|
| Read tools, `hand_off`, `start_session`, `logbook_write`, notices | Hab | Exists |
| Image input (a photo in the thread) | Phone sends, engine reads it as an image block | Upload exists (`app.md` 4.4); passing it to the engine is new |
| Device context | Phone | New: CoreLocation and EventKit read access in the app |
| Weather | Hab: WeatherKit REST, under the Apple Developer membership | New |
| Places nearby | Hab: Apple Maps Server API (same membership), or a Google Places key the user supplies | New |
| Web search and fetch | Hab: the SDK's own read-only web tools | New in Capcom's tool list |
| Add to calendar, set a reminder | Phone: a card; "Add" (in the app or the push action) writes through EventKit | New card kind |
| Message the user | Hab: a Capcom reply with push | New: the thread in 2.5 |
| Host checks | Hab: a seeded session on the named host through `start_session` | Exists; result back over channels is new |

A phone tool is a card, never a silent call: iOS does not run the app on demand in the
background, and the card is the guardrail.

### 2.4 The acceptance scenarios

| # | Ask | What happens | New pieces |
|---|---|---|---|
| 1 | "What can I do around here?" | Reads device context, calls weather and places, ranks by Logbook preferences, replies; an "Add" card per pick writes to the calendar | Device context, weather, places, calendar card |
| 2 | "Look at what's going on in app X we run." | Finds the project in the catalog, hands off to its agent or starts a session on its host, waits on channels, sums up | Channel endpoint for the engine; group start for larger work (section 3) |
| 3 | A photo of a flyer | Reads the image, searches the event, replies with the details and an "Add" card | Image input, web search, calendar card |
| 4 | "Check security updates on host X." | Starts a seeded session on X (confirmation per `confirm.start_elsewhere`), gets the report over channels, replies | Channel endpoint only |

### 2.5 How the app presents it

- **Typing.** The engine reports turn start and end; the app shows typing dots. Tool calls fold
  into one quiet line ("read 3 transcripts").
- **Read state.** "Delivered" when the hab stores a message; "Read" when a turn takes it.
- **Push.** A reply while the app is closed is a communication notification with Capcom's name
  and avatar, and an inline reply.
- **Approvals and phone tools** stay cards.

### 2.6 Guardrails and terms

- **Pacing.** A usage governor in gff-hab gives the engine and every group a share of the plan
  window: by default a daily share of the weekly budget (about 15% per day, cumulative), from
  the usage readings. At the cap the engine answers with a small model or says it waits.
- **Proactive turns** (section 5) are batched, use a small model for the first look, and have a
  daily turn budget.
- **Terms.** Personal use on the owner's own plan fits "ordinary, individual usage". Flightdeck
  shipped to others must never collect plan credentials: it uses the user's own login or an API
  key. Recheck before release.

### 2.7 What changes in `capcom.md`

| Today | Service |
|---|---|
| Kind `capcom` session in a pane, `[capcom] harness` | Kind `capcom` record with no pane; `[capcom] engine`; the pane form kept one release as fallback |
| Plain-terminal resume | The engine keeps SDK sessions where `claude --resume` finds them; verify, else an AGENTS.md exception |
| "New conversation" by the user | Re-seed by the engine; "new conversation" stays for a clean start |
| Busy while working | Never busy: messages queue |
| Home host, read scope, confirmations, hand-off rules | Unchanged |

**Risks.** The SDK moves fast and adds a Node or Python runtime. Self-managed context can drop a
fact; the visible summary limits that. Capcom needs its home host online.

## 3. B. Agent groups

### 3.1 The model

A group is a task with a coordinator and members. Any session the user picks, or Capcom, can
coordinate. Members are ordinary sessions on any host and harness: Claude Code, Codex, opencode
or SDK agents. Each opens, resumes and shows like any session; the app and gff show one group
card with the task, each member's state and context, approvals, the merge queue and usage.

| Role | Does | Default |
|---|---|---|
| Coordinator | Splits the task, dispatches leads, reads verdicts, gates | The starting session |
| Lead | Owns one item in its own worktree and branch; may run workers | Up to 3 at once |
| Worker | A sub-task under a lead | The lead's choice |
| Reviewer | Another harness than the author's; reports only high and medium findings (about 3); may fix in place | On for core changes |

### 3.2 The mechanisms

| Mechanism | Flightdeck piece | Status |
|---|---|---|
| Start a member on a host, in a harness, with a brief | Spawn verb with host and alias; seed through the adapter | Open (task-6iwat66b); extend |
| Folder trust on first start | Trust card; the group's start confirmation may pre-answer it for the group's folders | Card exists; pre-answer new |
| Messaging | The spawn verb names each member on channels and tells both sides the address | Channels exist; wiring new |
| Queue | Logbook tasks with `claim`, one per item | Exists |
| Isolation | Worktree per lead; stacked branches, each lead on the branch before it when items touch | Worktrees exist; stacking new |
| Gate and merge lane | gff-hab lands one branch at a time on the group branch after its checks and review pass; a conflict goes back to its lead | New |
| Heavy-run lock | One host-wide lock that build and test runs take through the agent kit; others wait in order | New |
| Cleanup | Remove a worktree after its merge; per-run build folders; the group's leftovers listed at its end | Teardown exists; group rule new |
| Disk watch | Free space per host in the catalog; below a threshold, heavy runs wait and the coordinator hears of it | New |
| Usage pacing | The governor (2.6) across members of every harness that reports usage | New |
| Context supervision | Compact at about 40% `context_pct`; after 4 compacts or a 4 MB transcript, the member writes a hand-off record and a fresh session continues from it | `context_pct` exists; supervisor new |
| Sibling actions | Any member, or the supervisor, can compact or clear-and-hand-off another; typed only through the rounds lock, never into a session in use | Lock exists; actions new |

Supervision is a hab-side default with settings, not a coordinator's chore. SDK members manage
their own context as in 2.2.

### 3.3 When to use it

Use a group for many separable items, or a task that needs a cross-harness review; not for one
change. Anthropic's research system spends about 15x a chat's tokens; Claude Code agent teams
about 7x in plan mode. Parallel agents make conflicting implicit decisions (Cognition,
OpenHands). Reviewers nearly always find something (HN 50380658), so they report only the top
findings. A group's value is cross-host, cross-harness and visible.

## 4. C. Tonight's friction, mapped

| Friction | Piece that removes it |
|---|---|
| The owner opened the Codex pane | Spawn verb: the coordinator starts it on the right host and harness |
| The owner approved Codex's folder trust | Trust card, pre-answered by the group's start confirmation |
| The owner told the coordinator the pane existed and how to reach it | Spawn returns the member's channel name; replies route by name |
| Reminders about context and compaction | Context supervision (3.2), on by default |
| Reminders about usage pacing | The usage governor (2.6) |
| Reminders about disk | Disk watch and the heavy-run lock |
| Reviews read in full by the coordinator | Reviewer reports verdicts and top findings; the card shows them |
| Merges done by hand in order | The gate and merge lane |

## 5. D. Watchers: always-on rounds

Rounds 9 (task-awkgh5h1) adds a fixed list of hab events with debounce and cooldown. A watcher
goes further: a long-lived program the hab supervises like a server, whose output wakes a target.

| Change to task-awkgh5h1 | Detail |
|---|---|
| New trigger kind `watch` | The round names a command or module that runs at all times. gff-hab restarts it with backoff, stops it on sleep and starts it at wake, and logs it like a run |
| Event lines | The watcher prints one JSON line per event; debounce, cooldown and coalescing apply as for hab events |
| New targets | `capcom` (the event lands in Capcom's thread as a proactive turn) and `group` (a message to a group's coordinator), beside today's `fresh`, `resume`, `fork` |
| More hab events | `notice.raised` and Logbook record changes, beside the four in `rounds.md` 4.3 |
| Unchanged | No webhooks and no TCP port: a watcher dials out (IMAP IDLE for mail, a feed, a status page) |

Example: a mail watcher holds IMAP IDLE and prints a line per new message from a sender list;
Capcom writes to the owner only when a mail needs them.

## 6. E. iMessage (optional)

Instinct is the reference. Its plumbing is not public. A self-hosted bridge runs on an always-on
Mac: it watches the Messages database and sends through Messages automation (`imsg`,
BlueBubbles). Typing bubbles and read receipts need a private-API helper with SIP off, which the
owner accepts.

| Identity for the bridge | Works? | Cost |
|---|---|---|
| Apple ID with an email address only | Yes: an email handle sends and receives iMessage over data | Free |
| Prepaid SIM with SMS in a spare iPhone | Yes: activation needs Apple's verification SMS | Small monthly plan, a spare phone |
| Data-only eSIM | No: it cannot receive the verification SMS | - |
| Google Voice number | No: Apple does not register forwarding numbers | - |
| P2P line (Sendblue, Linq, LoopMessage) | Yes, a real number, native typing and read receipts | Paid; the vendor sees every message |

**Recommendation:** a new Apple ID with an email address only, on the always-on Mac, saved as a
contact named Capcom. A text never answers an approval or a confirmation.

Built well, A makes E unnecessary: the app thread already gives typing, read state and push.
SMS and Twilio are out.

## 7. Plan sketch

| Stage | One line | Kind | Depends on |
|---|---|---|---|
| A1 | Capcom service spec: engine contract, device context, phone cards, resume | Spec | Part 9 built stages |
| A2 | `gff-hab-capcom` on the Agent SDK: one thread, agent kit tools, cards, channel name | Build | A1, Logbook stage 4 |
| A3 | App thread: typing, read state, communication push, inline reply, images | Build | A2, `app.md` 4.10 |
| A4 | Context manager: summary, re-seed, Logbook memory, divider | Build | A2 |
| A5 | Device context and personal tools: weather, places, web, calendar card | Build | A3 |
| G | Usage governor: daily pacing for Capcom and groups | Build | Usage readings |
| A6 | Codex SDK engine | Build | A2 |
| B1 | Groups spec: roles, record, supervision, merge lane, lock | Spec | Spawn verb spec |
| B2 | Spawn verb with trust pre-answer and channel wiring | Build | task-6iwat66b, trust card |
| B3 | Group record and group card in the app and gff | Build | B2 |
| B4 | Stacked worktrees, gate and merge lane | Build | B3, worktrees |
| B5 | Heavy-run lock and disk watch | Build | B1 |
| B6 | Context supervision and sibling actions | Build | B3, rounds lock, `context_pct` |
| B7 | Reviewer role, top findings, fix in place | Build | B2 |
| B8 | Capcom starts and runs a group | Build | A2, B3 |
| D1 | `rounds.md` 4.3 update: `watch`, new targets and events | Spec | Rounds 1-6 |
| D2 | Rounds 9 with the new events and targets | Build | D1, A2 for `capcom` |
| D3 | Watchers: supervised programs, event lines, a mail example | Build | D2 |

Order: A1, B1 and D1 together; then A2, B2, G; then A3, A4, B3, B5; then A5, B4, B6, B7, D2;
then B8, D3; A6 last.

## 8. Decisions for the owner

Decided by the owner on 2026-11-06.

| # | Question | Decision |
|---|---|---|
| 1 | Replace the pane Capcom with the service? | Yes, outright: the pane form goes when the service ships |
| 2 | May Capcom write first? | Yes, about anything useful, with quiet hours |
| 3 | Location shared with Capcom | The app shares location in the background; Capcom queries it when a question needs it, and it never enters the context by default |
| 4 | Places and weather source | Apple (WeatherKit, Apple Maps Server API); Google only with the owner's own key |
| 5 | Calendar writes | A card with one "Add" tap, never silent |
| 6 | A group's starts on other hosts | One confirmation for the group plan; an app setting turns confirmation off |
| 7 | Where the merge lane lands | On main, once review and CI pass |
| 8 | Show "earlier messages summarised" | Yes, as a quiet divider |
| 9 | iMessage bridge | Not now; kept as a future idea |

## Sources

- Hacker News item 50380658; https://kadhar.dev/blog/multi-agent-systems-from-scratch.
- https://code.claude.com/docs/en/agent-teams, `agent-sdk/overview`, `agent-sdk/agent-loop`,
  `agent-sdk/streaming-vs-single-mode`, `legal-and-compliance`;
  https://support.claude.com/en/articles/15036540 (read 2026-11-06).
- https://learn.chatgpt.com/docs/codex-sdk, `app-server`, `auth`.
- https://www.anthropic.com/engineering/multi-agent-research-system;
  https://cognition.com/blog/dont-build-multi-agents; OpenHands, "Don't sleep on single-agent
  systems".
- Sendblue and Linq docs; https://github.com/steipete/imsg; https://docs.bluebubbles.app;
  Apple Community threads 252046064 and 254800806 (Google Voice); eSIM vendor guides on iMessage
  activation; press coverage of Poke and Instinct, June to October 2026.
