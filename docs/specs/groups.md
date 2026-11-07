# Agent groups: behaviour spec

Status: draft for the owner. Part 15, stage B1 (task-5ntfv32q). Written 2026-11-06; the owner's
answers applied the same day (13).

An agent group is one task worked by several agents at once: a coordinator, leads, workers and a
reviewer, on any host and in any harness. Members are ordinary sessions. Flightdeck starts them,
wires them on channels, watches their context, usage and disk, reviews their work and lands it.
The user sees one group card. This spec covers the roles, the group record, the confirmation,
the merge lane, the heavy-run lock and the disk watch, context supervision, usage pacing, and the
build stages. It describes behaviour in our own words and copies no code from other tools.

Rules this spec works within:

- **Groups are first-class and optional** (master plan decision (o), 2026-11-06). The model is the
  coordinator workflow that builds Flightdeck, productised.
- **One confirmation per group plan; an app setting turns it off** (owner decision 6, (p),
  `swarm-study.md` 8).
- **The merge lane lands straight on main once review and CI pass** (owner decision 7). This
  replaces the study's "group branch" (`swarm-study.md` 3.2).
- **Members are ordinary sessions.** Each opens, resumes from a plain terminal and shows like any
  session (AGENTS.md, "Native data stays native").
- **Nothing types into a session in use.** Every action on a member goes through the rounds lock
  (`rounds.md` 6).
- **The bar is herdr, Rex and Paseo** for robustness; we add only cheap guards.
- **Hands-off defaults, few settings** (power-user rule). Supervision, pacing, the lock and the
  disk watch run by default.

Sections that settle a question end with a **Decision** line and its reason. The owner's
answers to this spec's questions (2026-11-06) are in [section 13](#13-owner-decisions).

## Contents

1. [Words](#1-words)
2. [Model](#2-model)
3. [The group plan and its confirmation](#3-the-group-plan-and-its-confirmation)
4. [Starting members](#4-starting-members)
5. [The group record and the group card](#5-the-group-record-and-the-group-card)
6. [Review](#6-review)
7. [Worktrees, stacking and the merge lane](#7-worktrees-stacking-and-the-merge-lane)
8. [The heavy-run lock and the disk watch](#8-the-heavy-run-lock-and-the-disk-watch)
9. [Context supervision and sibling actions](#9-context-supervision-and-sibling-actions)
10. [Usage pacing](#10-usage-pacing)
11. [When a group is the wrong tool](#11-when-a-group-is-the-wrong-tool)
12. [Shared interfaces with other parts](#12-shared-interfaces-with-other-parts)
13. [Owner decisions](#13-owner-decisions)
14. [Edge cases](#14-edge-cases)
15. [API, CLI and settings](#15-api-cli-and-settings)
16. [Build stages and acceptance](#16-build-stages-and-acceptance)
17. [Sources](#17-sources)

---

## 1. Words

| Word | Meaning |
|---|---|
| Group | One task, its plan, its members and its items. Lives on its home hab |
| Home hab | The hab of the coordinator's session when the group was made. It keeps the group record and runs the merge lane |
| Coordinator | The session that made the plan and reads the results. Any session the user picks, or Capcom |
| Item | One separable piece of the task. One lead owns it. A Logbook task holds it |
| Lead | A member that owns one item, in its own worktree and branch |
| Worker | A member a lead starts for a sub-task |
| Reviewer | A member that reviews one item, in another harness than the item's lead |
| Member | Any session the group started: lead, worker or reviewer. The coordinator is not a member |
| Plan | The list of items and members the coordinator proposes. The confirmation covers it |
| Envelope | The limits a confirmed plan sets: hosts, harnesses, folders, member caps (3.3) |
| Lane | The merge lane: lands one item's branch at a time on main (7) |
| Heavy run | A build or test command that takes the host's heavy-run lock (8) |

One word per concept: "member", never "agent" or "teammate", for a session the group started.
"Item", never "sub-task", for what a lead owns.

## 2. Model

### 2.1 Roles

| Role | Does | Default | Count |
|---|---|---|---|
| Coordinator | Splits the task into items, proposes the plan, dispatches leads, reads verdicts and lane results, answers members' questions | The session that calls `group_create` | One per group |
| Lead | Works its item's Logbook task in its own worktree and branch, may start workers, submits its branch to the lane | One per item | At most 3 working at once (`group.max_leads`); more items wait |
| Worker | Does a sub-task for a lead and reports back to it over channels | The lead's choice | At most 2 per lead (`group.max_workers`) |
| Reviewer | Reviews one item's diff against its brief, reports a verdict and its top findings, may fix in place | On for every item (6.1) | One per item under review |

- **A role is a field of the member, not a kind of session.** The session record gains
  `group: {id, role, item}`. Everything else about the session is unchanged.
- **The coordinator stays the user's session.** Flightdeck never starts the coordinator. A
  coordinator that exits does not end the group (14).
- **Capcom can coordinate** through the same tools (12.1).
- **Harness sub-agents** (Claude Code's Task tool, Codex's own agents) run inside a member's
  process. They are not members, and the group does not see them.

Decision: three leads at once by default. Reason: the workflow this models runs well with three;
more leads mean more conflicts and more usage, and the study's research shows parallel agents make
conflicting implicit decisions.

### 2.2 What runs where

| Piece | Runs on | Why |
|---|---|---|
| Group record, card data, events | Home hab | One writer, no election (`mesh.md`) |
| Members | Any host the plan names | Each member is a session on its host's hab |
| Item queue | Logbook (tasks with `claim`), synced to every hab | It already exists, survives restarts and syncs (`logbook.md` 7) |
| Merge lane | Home hab, in a lane worktree of its own | One lane per repository per hab (7.3) |
| Heavy-run lock, disk watch | Every hab, for its own host | Disk and CPU are per host |
| Context supervision | Each member's own hab | It reads that hab's `context_pct` and types through that hab's lock |
| Envelope checks for later starts | The hab that starts the member | Each member's `session.create` carries the envelope (3.3), so a member hab decides alone while the home hab sleeps |
| Usage governor | Part 14 G (12.2) | Shared with Capcom |

- **How the home hab learns member state.** From the catalog copies it pulls from each peer every
  15 s (`mesh.md` 5): each member's agent state, `context_pct` and compact count. The card shows
  a member's state with the copy's age, as rounds do for a stale peer.
- **While the home hab sleeps or is offline,** the lane, new leads and reviewers on other hosts
  wait. Starts inside the envelope on a member's own host (workers, fresh sessions) go on, and
  so does supervision. A group meant to run overnight should have an always-on home: Capcom's
  home host, or a coordinator session on an always-on host. The user can move a group's home
  to another hab before a laptop sleeps (5.7).

Decision: the group record lives in gff-hab, and the items live in Logbook. Reason: members,
states and the lane queue change by the second and belong to one hab; items are durable records
that every agent reads with `gff lb`, and Logbook already gives `claim` and sync.

---

## 3. The group plan and its confirmation

### 3.1 What a plan holds

The coordinator writes the plan and calls `group_create` (15). A plan holds:

| Field | Meaning |
|---|---|
| `title` | The task, one line |
| `brief` | The task in full, the shared context every member's seed carries |
| `project` | The project (by id or remote). A group works on one repository |
| `items` | Each: title, brief, `touches` (paths or item ids it overlaps), `after` (an item whose branch it stacks on, 7.2), `review` (`on` or `off`, 6.1) |
| `members` | Each: role, item, host, harness alias, model, name. Folders come from the worktrees the hab makes (7.1) |
| `lane` | Target branch (the project's default branch, `main` in this spec), and whether the reviewer may fix in place |

The hab checks the plan before any card: a known harness and a reachable host for each member,
the project checked out on each member's host (the catalog), at most `group.max_workers` workers
per lead, names in the channel charset and free on every host (`mesh.md` 6.1), item ids unique,
no `after` cycle, and the `logbook` capability on the home hab. A check that fails refuses the
call with the item or member it names; a host without the project gets "set up the project on
<label> first". `group.max_leads` is not a plan limit: it caps how many leads work at once
(4.3). An automatic name follows `capcom.md` 6.4: harness and six hex digits of the group id,
then the role and the item's position in the plan, such as `codex-3fa9c2-review-2` for the
reviewer of the second item.

The hab files one Logbook plan record for the group and one task per item, linked as stages
(`logbook.md` 2.4). Each item's task carries the item brief. The group id is on the plan record
as a tag.

### 3.2 The confirmation

- **One confirmation for the whole plan.** The card shows the task, each item, each member (role,
  host, harness, model, folder or "new worktree", branch), the lane target, review on or off per
  item, and the trust rule: which new worktrees the confirmation answers the trust prompt for (4.2).
- **The setting.** `confirm.group_plan`, a levelled hab-side setting (`app.md` 20.3, 20.5): true
  asks, false starts at once and shows the card as a record. Safe true, Balanced true, Bypass
  false (owner, 2026-11-06; 13 Q1). Every plan asks while it is true, a plan on one host too.
  This is the one confirmation Balanced adds, an exception `app.md` 20.2 states ("no
  confirmation Flightdeck adds beyond approval expiry" and this card). Reason: the card also
  answers folder trust for every worktree the group makes (4.2).
- **With `confirm.start_elsewhere` on and `confirm.group_plan` off**, a plan with any member on
  another host still asks, once. The plan never asks once per member.
- **Mechanics are the confirmations of `capcom.md` 6.1 and 6.4**: kind `group`, on the home hab,
  first answer wins, expiry after `group.confirm_max_age` (default 60 minutes, as `capcom.md` 6.1;
  15.3), no tool answers, edits allowed. The user may edit hosts,
  harnesses, models and names, drop an item or a member, and turn review off for an item. The
  hab checks the edited plan again (3.1).
- **Nothing starts before the answer.** On `cancelled` or `expired` the plan record and its tasks
  are marked `dropped`, and the coordinator gets one message from Flightdeck.
- **The tool waits** as `start_session` does (45 s), then says "still waiting"; the answer
  reaches the coordinator later as a message from Flightdeck (`capcom.md` 6.4).
- **A gff-hab restart drops open confirmations** (`capcom.md` 6.4, "In memory"). At start, a
  group in `confirming` whose confirmation is gone becomes `dropped`, and the coordinator gets one
  message: "the hab restarted before you answered; propose the plan again".

Decision: one card for the whole plan. Reason: the friction was one prompt per start and per
folder; the owner asked for one (decision 6).

### 3.3 The envelope: starts after the plan

A confirmed plan sets an envelope: its project, its hosts, its harnesses, the worktrees the hab
makes for it, and the member caps. The home hab copies the envelope into every member's
`session.create`, so each member hab checks a later start on its own host with no call home.
Later starts inside the envelope ask nothing:

| Later start | Asks? |
|---|---|
| A worker a lead starts, within `group.max_workers`, on a plan host, in a plan harness | No |
| A reviewer for an item, in a plan harness, on a plan host | No |
| A fresh session that replaces a member (9.3) | No |
| A member started again after its host came back | No |
| A new item, a new host, a new harness, or another project | Yes: the coordinator calls `group_amend`, which raises one card for the change (same setting rules) |

A start outside the envelope that is not an amend follows the normal start rules
(`confirm.start_elsewhere`) and is not a member.

Built (B3): only a start from a member, inside the envelope, skips the card. A member's start
outside the envelope is refused with the reason, and the coordinator amends. A start that names
a group but comes from a session that is not one of its members is not refused: it follows the
normal start rules, asks while `confirm.group_plan` is on, and gets the trust cover of 12.6 as
B2 built it.

---

## 4. Starting members

### 4.1 The spawn verb

Members start through the spawn verb (task-6iwat66b, extended by stage B2). This spec needs from
it, and does not define its internals (12.3):

- **Arguments:** host, harness alias or harness, model, folder, name, seed, and the group fields
  `{group, role, item}`.
- **Returns:** the session id, its name and its channel address (`label/name` as the spawner's
  hab sees it).
- **The seed** goes through the channels inbox as a start envelope (`capcom.md` 6.4), which names
  the spawner's address. So both sides know each other's address with no extra step.
- **A start on a peer** goes over the link the home hab dialed, as `session.create` with an
  idempotency key, as confirmed starts do today.

### 4.2 Folder trust

A harness may ask to trust a new folder (`app.md` 7.10). A group starts most members in new
worktrees, so most starts would show the prompt.

- **What a confirmed plan covers.** Every worktree a hab makes for the group: on a plan host, of
  the group's project, on a `group/<group id>/` branch, for a harness in the plan. That includes
  reviewers' and workers' worktrees made later. The card states this rule and the hosts and
  harnesses; it does not list folders, since most do not exist yet. Confirming the plan is the
  user's trust answer for them.
- **Who answers.** The member's own hab, for its own pane. `trust.answer` never crosses a peer
  link (`app.md` 7.10). The home hab hands the cover to the member hab inside the spawn's
  `session.create`: `trust_cover {group, confirmation, harness, branch_prefix}` (12.6). The
  member hab answers only a folder that matches it.
- **No card for a covered folder.** While a covered pane shows the dialog, the member hab holds
  the `blocked:trust` report, its board row and its push for up to `[groups] trust_hold` (10 s;
  15.3) and answers with `trust`. All of `trust.answer`'s checks apply: it never answers blind,
  and a client typing in the pane refuses it. If the answer fails or the hold passes, the report goes out and the card
  shows as today.
- **A plan that started with no confirmation** (setting off) gets the same cover: the hab answers
  the trust prompt for every worktree it makes for the group, by the same rule (owner,
  2026-11-06; 13 Q2). The cover then names no confirmation id; it names the group and its
  `confirmation: none`. A folder outside the cover raises the trust card as today.
- **The seed waits** in the inbox while `blocked:trust`, as today, and goes in once the agent is
  idle.
- **A failed pre-answer** (`conflict`, `unavailable`) leaves the card for the user and tells the
  coordinator once.
- **Codex.** Built in B2 (`app.md` 7.10): Codex 0.167's prompt was drawn and checked live, and
  the card answers it with `trust` (arrows, then Enter on "Trust and continue"); `exit` is
  refused for Codex, since no key exits it from the dialog. **The cover does not answer Codex.**
  In a linked worktree Codex says "Trusting will apply to the repository root" and records the
  main checkout as trusted (checked live 2026-11-06 with a throwaway `CODEX_HOME`). That trusts
  every worktree of the repository, someone else's code too, which is wider than the rule above.
  So a Codex member's prompt shows the card, as today; the hab never answers it.

**Built choices (stage B2, task-7kzvfcna).**

- Until stage B3 brings the one plan card, each group start is its own confirmation: a spawn
  with `group` asks while `confirm.group_plan` is on, on any host, and the cover names that
  confirmation. B3 replaces it with the plan's confirmation and the envelope (3.3).
- The member hab checks the cover when the dialog first shows: the cover's harness (Claude Code;
  not Codex, above), a linked worktree it made (gff-hab's worktree mark) holding the start
  folder, on a branch under `branch_prefix`, and not a foreign checkout whose setup still waits
  for the owner's yes (`hab-logic-api.md` 3.10). "Of the group's project" holds because the
  session starts in that worktree; "a plan host" holds because only the plan's starts carry a
  cover. The cover comes only from the hab's own confirmations or a listed peer's link.
- "A listed peer's link" is not authentication by itself: a peer's hello names its device id,
  and the link is as trusted as the SSH access behind it (`mesh.md` 4). A hab that can open a
  link as a listed peer can already start sessions on this one.
- The hold: the hab tries the answer on each screen read until it lands or `trust_hold` passes,
  and stops trying once a client typed into the pane (in the last second before the dialog, or
  during the hold). The card then shows at the hold's end. A folder outside the cover shows the
  card at once.
- "Tells the coordinator once": in B2 the hab tells the member's spawner (its `report_to`), as
  a channel message from the member. B3 sends it to the group record's coordinator.
- The session record's `group: {id, role, item}` (2.1) comes with B3; B2 carries the fields on
  the confirmation and the cover only.

Decision: the confirmation is the trust answer; with confirmation off, the group itself is.
Reason: the user saw every folder on one card, and asking again per folder is the friction this
part removes. With the card off, the user chose a hands-off group (Bypass, or by hand), and the
owner decided the hab answers trust for every group worktree all the same (13 Q2). This is an
exception to `app.md` 20.2 "What Bypass never touches" (the harness's own prompts), limited to
worktrees the hab made for a group, and `app.md` 7.10 states it (12.6).

### 4.3 Order of starts

1. The hab picks items in plan order, up to `group.max_leads` working at once. A stacked item
   waits until the item it stacks on reaches `review` (7.2). Items beyond the cap wait as
   `queued`; a lead whose item lands or drops frees a slot, whether or not its session still
   runs (7.3 step 7).
2. On the lead's host, the hab fetches the project's remote, makes the worktree (7.1) and waits
   for its setup script to end. A setup that waits for the user's yes (`hab-logic-api.md` 3.10,
   a foreign checkout) keeps the item `queued` and shows on the card's waits line.
3. It starts the lead and writes the claim itself: assignee the lead's session, status `doing`.
   A hab write, not an agent write, so it lands under `logbook.approval = agents` too (owner,
   2026-11-06; 13 Q5).
4. Reviewers start when their item is submitted (6.2), not before: a reviewer idle for hours costs
   context and a pane.

A start that fails (host offline, harness missing, spawn refused, setup failed) marks the member
`failed` with the reason, tells the coordinator, and moves the item to `stalled`. The queue skips
a `stalled` item until the coordinator calls `group_retry` or edits the plan.

---

## 5. The group record and the group card

### 5.1 The record

The home hab keeps one record per group in `<state>/groups/<id>.json`, synced before its rename,
so a gff-hab restart loses nothing.

| Field | Content |
|---|---|
| `id` | Random, 64 bits, hex |
| `title`, `project`, `plan_record` | The task, its project, its Logbook plan record id |
| `home`, `generation` | The home hab's device id, and a number that rises by one at each move (5.7). 1 at creation |
| `coordinator` | Session id, name and address |
| `inbox` | Reports for the coordinator that wait while it is not running (5.5) |
| `state` | See 5.2 |
| `confirmation` | Id and outcome, or `none` when the setting was off |
| `items` | Each: id, Logbook task id, lead, branch, `after`, state (5.3), the reviewer's verdict and findings, lane attempts |
| `members` | Each: session id, name, host, harness, role, item, state (from `agent.state`), `context_pct`, compacts so far, the session it replaced |
| `lane` | The queue in order and the item landing now |
| `usage` | The governor's share and use for this group (10) |
| `pause` | `user`, `usage`, both, or none, each with since when (5.2) |
| `waits` | What holds the group now: usage, disk, a lock, an open trust card, an open approval, a setup that waits for a yes |
| `leftovers` | At the end: worktrees, branches and build folders left (7.5) |

The home hab publishes a short form (id, title, state, counts, the coordinator's address, `home`
and `generation`) in its catalog's `groups` section, as rounds do (`rounds.md` 9), so every hab and client can list every
group, and a round on any hab can address the coordinator (12.5).

### 5.2 Group states

| State | Meaning | Next |
|---|---|---|
| `confirming` | The plan waits for the user | `running`, or `dropped` on cancel or expiry |
| `running` | Members work; the lane lands | `paused`, `ending` |
| `paused` | The user paused it, or usage is at its cap (10). No new turns start; working members finish their turn | `running` when no pause reason is left; `ending` on close |
| `ending` | Every item is landed or dropped, or the coordinator or the user closed it; the hab cleans up (7.5) | `ended` |
| `ended` | Done. The record stays `[groups] keep_ended` (30 days; 15.3) | — |
| `dropped` | The confirmation was cancelled or expired | — |

### 5.3 Item states

| State | Meaning |
|---|---|
| `queued` | Waiting for a lead slot, for the item it stacks on to reach `review`, or for a setup yes |
| `stalled` | Its lead failed to start or was closed; skipped until `group_retry` (4.3) |
| `working` | The lead works |
| `review` | Submitted; the reviewer reviews |
| `fix` | The verdict was "fix needed", or the lane sent it back; the lead works |
| `ready` | Review passed; in the lane queue |
| `landing` | The lane rebases, checks and waits for CI |
| `landed` | On main |
| `dropped` | The coordinator or the user dropped it |

### 5.4 The card

The app and gff show one card per group: in the app on the status board and in the coordinator's
session view; in gff on the status board, below the sessions.

- **Header:** title, state, home hab, elapsed time, the waits line ("waiting: disk on <label>").
- **Items:** one row each: title, state, lead, branch, the verdict with up to `[groups]
  brief_findings` (3) findings (6.3), the
  lane result.
- **Members:** one row each, as a session row (state, host, harness, context use). A tap opens the
  session. Approvals and trust cards of members show here and in the approvals area, as for any
  session.
- **Lane:** the queue in order, the item landing now and its step.
- **Usage:** the group's share and use (10).
- **Actions:** pause, resume, close, move home (5.7); per item: drop, land now (skips nothing: it only moves the
  item to the front of the queue); per member: open, compact, hand off (9.4).

Members stay in the normal session list too. The list marks them with the group's title.

### 5.5 Reports to the coordinator

Members, the lane and supervision report to the coordinator through the home hab, not straight
over channels, because channels refuse a message to an exited session (`mesh.md` 6.2).

- **The `group_report` tool** sends one line or a short text to the coordinator. The lane,
  review verdicts and supervision use the same path.
- **A running coordinator** gets each report as a channel message from Flightdeck, delivered
  when idle like any message.
- **A coordinator that is not running** (exited, its host asleep) gets nothing typed. The reports
  wait in the record's `inbox` (at most `[groups] inbox_max`, 200, for `inbox_days`, 7 days; the
  oldest go first) and show on the card. At `session.resume`
  of the coordinator the hab delivers them as one message, oldest first.
- Members may still use `channel_send` to the coordinator's address; that follows the channel
  rules unchanged.

### 5.6 A pause

- **Two reasons, kept apart.** A user pause (card or `group.pause`) and a usage pause (10) are
  separate fields. A usage resume never lifts a user pause.
- **Where it holds.** The home hab publishes the pause in the group's catalog entry and pushes it
  over each member link at once. Each member's hab then holds, for members of that group: channel
  messages into them (from any sender), supervision's messages and new starts. Working members
  finish their turn. A member hab that has not heard of the pause yet delivers; the next catalog
  pull (15 s) corrects it.
- A paused group can be closed.

### 5.7 Moving the home hab

Owner decision, 2026-11-06 (13 Q6): a group's home hab is movable in the first build. The usual
reason is a laptop that will sleep: the user moves the group to an always-on hab first, so the
lane and new starts go on overnight.

**Who moves it.** The user, from the card ("Move home"), `gff group move <group> --host <label>`
or `group.move {group, host}`; and gff-hab itself when the Capcom mark moves (below). An agent
cannot: the coordinator's tools have no move. Reason: the home decides where the lane runs and
which hab must stay awake; that is the user's call, as moving a round is (`rounds.md` 9).

**What moves.**

| Moves to the new home | Stays where it is |
|---|---|
| The group record (5.1) with its inbox, item and member states, pause, waits, usage share and leftovers list | Members: each is a session on its own host and keeps running there |
| The lane: its queue, and the item landing now (after the wait below) | Worktrees and branches. The new home makes its own lane worktree (7.3); the old one's lane worktree is removed after the move |
| Publishing the group in the catalog `groups` section | The Logbook plan and tasks: they sync to every hab already |
| Confirmations the group raises from now on (`group_amend`) | The envelope in each member's `session.create`: it names hosts and harnesses, not the home |
| The governor consumer `group:<id>`, by id | The heavy-run lock: each host's own; the lane's checks take the new home's |

**The checks.** The current home refuses the move, naming the reason, when the new hab:
is offline or has no link from the current home; lacks the `groups` or `logbook` capability; does
not have the project checked out (the lane needs it); is the current home (a no-op). A group in
`confirming`, `ending`, `ended` or `dropped` does not move.

**The hand-over.** The current home runs it; one writer at every step.

1. **Freeze.** The record gets `moving {to, since}`. The lane takes no new item. Reports, submits
   and verdicts that arrive go into the record as usual and move with it.
2. **Drain the lane.** An item in `landing` before step 4 (the push of the rebased branch) stops
   and goes back to `ready` at the front of the queue. One past step 4 finishes its land, at most
   `[groups] move_drain` (10 minutes); at that limit it goes back to `ready` and the new home starts it again at step 1,
   which marks it `landed` when its saved head is already on the target (14).
3. **Adopt.** The current home sends `group.adopt {record, generation}` over its link to the
   new hab. Each attempt takes a new generation, one above any the old home has used for the
   group, so two attempts never share a number. The new hab checks the project again, writes the record (synced, then renamed) with
   itself as `home` and the mark `pending`, and answers. A pending copy is not published and runs
   nothing.
4. **Commit.** On the answer, the old home writes its copy as a tombstone, `moved {to,
   generation}`, kept `[groups] tombstone_keep` (30 days). The tombstone is the commit point. The old home then publishes
   only the tombstone in its catalog and sends `group.adopt_commit`. The new home clears
   `pending`, publishes the group with the new generation, starts its lane, and tells the
   coordinator one line: "group <title> moved home to <label>". The card shows the new home.
5. **No answer.** When the adopt gets no answer in `[groups] move_answer` (60 s), or a refusal, the old home clears
   `moving`, keeps the group and says why on the card. It writes no tombstone, so a late answer
   commits nothing.
6. **Commit from the catalog.** The tombstone is the commit, so the new hab does not need the
   message. When any catalog copy of the old home shows the tombstone `moved {to: <this hab>,
   generation}` matching its pending copy, the new hab commits by itself, as at step 4. A home
   that wrote its tombstone and then slept or crashed therefore never strands the group. The
   old home also sends `group.adopt_commit` again at its start for each tombstone the new home
   has not acknowledged.
7. **A stale pending copy.** The new hab drops a pending copy when a catalog copy shows the
   group live anywhere (no tombstone, `moving` cleared), or a tombstone whose `to` is another
   hab or whose generation is not the pending copy's.

**How members and the coordinator re-point.**

- Every hab finds a group's home by its catalog copies: the entry with the highest `generation`
  wins; a tombstone points at its `to`. So member habs, rounds in `group` mode (12.5) and clients
  follow a move at their next catalog pull (15 s).
- A call that reaches the old home after the move (`group.report`, `lane.submit`,
  `review.verdict`, a `group_status`) is forwarded over the old home's link to the new home when
  that link is up, or refused with `moved` and the new home's label. The member's hab sends it
  again to the new home, so the agent sees one call that succeeded. A pending copy (step 3)
  answers `unavailable`, and the member's hab tries again after `[groups] move_retry` (2 s).
- The coordinator's address does not change: it is the coordinator's own session. Reports to it
  now come from the new home.
- A member started later in the envelope needs nothing from the home (3.3). Its new member
  record reaches the new home with the member hab's next report.

**The sleeping laptop.**

- A move needs the current home awake: it holds the only writable record. Nothing moves a group
  off a home that is already asleep or offline. The move is then queued on the target hab,
  which says so: "<label> is offline; the group moves when it is back". The target hab keeps
  the request (one per group; a later request replaces it, and the user can cancel it from the
  card), watches its catalog copy of the old home, and sends `group.move` over its link once
  the old home is online. A queued move older than `[groups] queued_move_days` (7 days) is dropped
  with one notice. The mesh has no election (`mesh.md`),
  so no other hab may take the record over by itself.
- **At creation.** When the coordinator's hab is not marked `always_on` (`mesh.md` 8) and an
  always-on peer has the project, the plan's card says so in one line and offers that peer as
  the home. The user picks; the hab never moves a group on its own, Capcom's case aside.
- **While it sleeps,** 2.2 and 14 apply: the lane and starts that need the home wait; members on
  other hosts work on.

**A Capcom-coordinated group when the Capcom mark moves.** A group whose coordinator is Capcom
has Capcom's home host as its home (12.1). When the mark moves (`capcom.assign`):

- The old Capcom home moves each such group to the new Capcom home as part of losing the mark,
  after its engine finished its turn (`capcom-service.md` 3.2), by the hand-over above. The new
  Capcom thread gets one note per group, "Group <title> moved here", with the card. Capcom's
  conversation does not move (`capcom.md` 7.3); the new Capcom reads the group with
  `group_status`.
- **The old home asleep during the re-assignment** (the earlier open case of 12.1): the group
  stays home on the old hab while it sleeps, as any group with a sleeping home does (2.2, 14).
  Reports go through the home (5.5), so they wait in the member habs' outboxes, and the lane
  waits. Members keep working. A member's own `channel_send` to `capcom` reaches the new
  Capcom home, as any channel message does. When the old home links again it drops the mark
  (`capcom.md` 7.3) and, in the same step, moves its Capcom groups to the new home; the waiting
  reports then reach the new Capcom.
- **The new Capcom home lacks the project.** The move is refused by the check above. The group
  stays on the old home with Capcom as coordinator, and the new thread gets one note: "Group
  <title> stays on <label>: set up the project here to move it."

Decision: an explicit two-step hand-over by the current home, whose tombstone is the commit
point, with a generation number in the catalog. Reason: the mesh has one writer per record and no
election. The record is live in exactly one place at every step: on the old home until its
tombstone, on the new home after the commit. The generation lets every hab and client pick that
copy from the catalog with no extra protocol.

### 5.8 Built choices (stage B3, task-nz4aglbr)

`internal/groups` holds the plan checks, the record and its store; gff-hab's group service runs
it on the home hab.

- **One confirmation per plan** is a confirmation of kind `group` in the confirm service: the
  same cards, first answer wins, expiry by `group.confirm_max_age`, and notes. An edit on the
  card is a whole edited plan, checked again. With `confirm.group_plan` off a plan runs at once
  and the record names confirmation `none`; with it off, `confirm.start_elsewhere` on and a
  member on another host, the plan asks once.
- **The record** is `<state>/groups/<id>.json`, written by a synced rename; a file that does not
  parse is set aside. `[groups] keep_ended` removes an ended or dropped record at a start. A
  restart drops a group still confirming and tells the coordinator; a group starting goes back
  to planned.
- **Logbook.** The home hab files the plan and one task per item as the actor kind `hab`, so
  they land under `logbook.approval = agents` (13 Q5). It claims an item's task for its lead.
- **Automatic names** count the whole plan: an amend's members are named with the group's
  items and members in view, and a second member of one role on one item gets `-2`, `-3`.
- **Card edits.** An item the user adds on the card gets its Logbook task when the plan runs.
  The hab writes an edited title and brief to the plan record, and an item's edited title and
  brief to its task.
- **Worktrees.** A start tried again finds the branch's worktree. When the list marks its setup
  as waiting for the user's yes (`setup_pending`), the item waits again; when the worktree was
  removed, a new one goes on the existing branch.
- **A start that ends after the group ended** leaves the member listed as left over, and closes
  it when `group.close_members` is on.
- **`group.close`** closes the idle members, with `group.close_members` on: on this hab, and on
  a peer with `pane.close` over the link this hab dialed. A peer offline keeps its members,
  listed as left over.
- **The envelope** lives on each member's session record, so a member's hab decides a later
  start while the home hab sleeps.
- **Members.** The home hab sees members through the catalog copies (an agent's `group` field)
  every 5 s, also workers a lead started. An agent the record does not list counts only as a
  worker or reviewer on a plan item, on a plan host and in a plan harness; a start from a
  session that is not a member carries no `group` field. A lead whose session is closed stalls
  its item, and the coordinator hears it. On this hab the session record shows it; for a member
  on a peer, the home hab asks the peer (`structure.get` over the link it dialed) once per
  catalog copy that no longer lists the member's agent: a session gone is closed, one exited
  shows exited, and is asked about again every 2 minutes at most. Each call waits 5 s at most.
  A peer offline is not asked; its members stay as they were.
- **Reports.** From a pane, only a member of the group or its coordinator reports or notes; a
  member notes its own item only. A report goes to a running coordinator as a Flightdeck note, else to the inbox
  (`[groups] inbox_max`, `inbox_days`). Another hab forwards a report over the link to the home
  hab once, never twice. The inbox goes in as one message once the resumed coordinator's pane
  reports an agent state.
- **A pause** holds channel messages into members and new member starts.
- **The push to members.** When the envelope or the pause changes, the home hab sends
  `group.sync` (the record's revision, the envelope, the pause) over the link it dialed to
  each hab with a running member; members on the home hab get the envelope in their session
  records. A member's hab replaces the envelope of that group's members whose envelope names
  the sender as home, and takes the pause. It accepts a push only from the hab the catalog
  lists as the group's home, or the home a member's envelope names. The pause holds by the
  newer revision: the last push, or the catalog copy's group entry, which carries the revision
  too. A hab the push misses (offline, or linked only the other way) gets it at a later pass,
  every 5 s, and once more after a home restart; until then it reads the pause from the
  catalog copy. A hab that answers but refuses the push (an older build) is not asked again
  for that change, and reads the pause from the catalog copy.
- **The CLI** is `gff-hab group ls|show|pause|resume|close`. The agent kit has `group_create`,
  `group_status`, `group_amend`, `group_close`, `group_retry`, `group_report` and `group_note`.
- **Not built in B3.** The usage pause has its call but no governor
  (Part 14 G). Reviewers and plan workers stay planned (B7). The scheduler does not wait for a
  started worktree setup or fetch first (B4). The app's group card and gff's board section come
  later.

---

## 6. Review

### 6.1 When an item is reviewed

- Every item is reviewed unless the plan or the user set `review: off` for it. The card shows
  "no review" on such an item.
- The lane lands an item only with a `pass` verdict, or with review off.

Decision: review on for every item by default. Reason: owner decision 7 lands on main "once review
and CI pass", with no branch of its own to catch a bad merge; the study's "on for core changes" is
the coordinator's to narrow per item.

### 6.2 Choosing and briefing the reviewer

- **Another harness than the lead's, picked at plan time.** The hab picks, per item, the first
  harness in the fixed order Codex, Claude Code, opencode that is not the lead's and is installed
  on a plan host. The plan, and so the card and the envelope, name it. With none, it uses the
  lead's harness in a fresh session and the card says "same harness".
- **Started when the lead submits** (`lane_submit`): the hab fetches, makes a review worktree on
  the submitted head, on the reviewer's host, and starts the reviewer with the item brief, the
  diff against the merge base, and the review rules below.
- **One review at a time per item.** A second submit while a review runs restarts it on the new
  head and tells the reviewer.

### 6.3 The verdict

The reviewer ends its review with the `review_verdict` tool:

| Field | Content |
|---|---|
| `verdict` | `pass` or `fix_needed` |
| `findings` | At most `[groups] max_findings` (5), each `severity` (`high` or `medium`), title, place (file and line), one paragraph; more are refused. The brief asks for the top `brief_findings` (3) |
| `fixed` | Commits the reviewer pushed to the branch (6.4), if any |

- Low findings are not reported. The brief says so: reviewers nearly always find something
  (HN 50380658), so only high and medium findings reach the card.
- The card shows the verdict and the findings. The full review stays in the reviewer's
  transcript.
- `fix_needed` moves the item to `fix` and sends the findings to the lead over channels. After the
  lead submits again, the same reviewer reviews the new diff.
- A reviewer that exits or stays idle `[groups] reviewer_idle` (30 minutes) with no verdict is reported to the coordinator;
  the item stays in `review`.
- The verdict goes to the coordinator as one report line ("item 2: fix needed, 2 findings"), never
  the review text.

### 6.4 Fix in place

When the plan allows it (`lane.reviewer_fixes`, default true), the reviewer may fix its own
findings. The hab has no gate on the git remote, so the reviewer never pushes to the lead's
branch:

- The reviewer commits in its review worktree on top of the submitted head and pushes to its own
  branch, `group/<group id>/<item id>-review`.
- The verdict names the fix commits and the review branch head. The hab checks that head is a
  descendant of the submitted head; otherwise the fixes are ignored and the card says so.
- A `pass` with fixes lands the review branch head: the lane takes it in place of the submitted
  head. Its checks and CI run on that result, as for any land.
- The lead's next `lane_submit` waits until the verdict. After a pass with fixes, the hab tells
  the lead to fast-forward to the review branch before any new change.

---

## 7. Worktrees, stacking and the merge lane

### 7.1 Worktrees and branches

- **Each lead gets one worktree and one branch**, made with `worktree.create` (`hab-logic-api.md`
  3.10) on the lead's host. Branch name: `group/<group id>/<item id>`.
- **Workers** that write get their own worktree on a branch off the lead's; the lead merges their
  work. Read-only workers run in the lead's folder.
- **Leads push their branch to the project's remote** (the `origin` the catalog knows). The lane
  reads branches from the remote, so a lead on another host needs no file path the home hab can
  see.
- **gff-hab fetches before it makes a group worktree.** `worktree.create` does not fetch today
  (`hab-logic-api.md` 3.10); groups need a `fetch` option on it (12.7). A branch the hab made
  from the remote under `group/` is not foreign, so its setup needs no yes.

### 7.2 Stacking

When an item `touches` another item's paths, or the plan sets `after`, the later item stacks:

- It starts when the earlier item reaches `review`, so the earlier branch has its work. Its
  worktree starts from the earlier item's submitted head, not from main.
- The lane lands the earlier item first. The later item's lane run rebases on main, which
  already holds the earlier commits, so they drop out of its diff.
- If the earlier item goes back to `fix`, the later one waits in the queue behind it.
- If the earlier item is dropped, the later one is rebased on main by the lane; a conflict goes
  back to its lead.

### 7.3 The lane

One lane per repository per hab. It lands one item at a time, in the order items reach `ready`,
except that an item never lands before an item it stacks on.

For each item:

1. **Fetch** the item's branch and the target branch from the remote.
2. **Rebase** the item on the target in the lane's own worktree. A conflict stops here (7.4).
3. **Checks.** Run the project's `lane.checks` under the heavy-run lock (8). A failure stops here.
   The lane reads `lane.checks`, `lane.ci` and `lane.build_folders` from `flightdeck.json` as
   committed on the target branch it fetched in step 1, never from the item's branch, so an item
   cannot change its own gate.
4. **Push the rebased branch** to the item's branch with `--force-with-lease`, and save the
   rebased head in the item's lane attempts before step 5.
5. **CI.** Run the project's `lane.ci` command with the branch name and the commit id, and wait
   for its exit: 0 passes, any other code fails. The command is the project's own, such as one
   that watches the CI run for that commit. The project's CI must run on pushes to `group/**`
   branches; this repository's `ci.yml` runs only on pull requests and on main today and needs
   that trigger. A project with no `lane.ci` has no CI step: the item lands on its local checks
   alone, and the card says "no CI" on the item and the lane (owner, 2026-11-06; 13 Q3).
6. **Land.** Push the commit to the target as a fast-forward. If the target moved, start again
   at step 1. After `[groups] lane_tries` (3) tries the item goes back to the coordinator with
   "main keeps moving".
7. **After the land:** mark the item `landed` and the Logbook task `done`. The lead and the
   reviewer keep running (owner, 2026-11-06; 13 Q4): the coordinator or the user may still ask
   them about the work, and they hold no lead slot (4.3). Their worktrees stay while their
   sessions run, since a live session's folder must not vanish; the hab removes each worktree
   once its session has ended and the worktree has no uncommitted changes.
   **How they end.** At the group's end (7.5), which closes idle members; when the coordinator
   drops them with `group_amend` (closed when idle, 14); or when the user closes them from the
   card or the session list. Closing a member of a landed item never stalls the item.

- **The lane never lands without a pass.** A review verdict of `pass` (or review off) and a
  passing check and CI step are required for every land. No setting turns this off.
- **The lane does not merge with a merge commit** and does not squash. The item's commits land
  as the lead wrote them, rebased.
- **The lane runs on the home hab.** If the home hab sleeps, the lane waits. Members on other
  hosts keep working and submit; their items wait in `ready`.

Decision: land straight on the target, one at a time, after review and CI. Reason: owner decision
7; one at a time keeps every land tested on the exact tree it lands on.

### 7.4 Back to the lead

A conflict, a failed check, a failed CI or a refused push moves the item to `fix` and sends the
lead one channel message: the step, the first `[groups] fix_output_lines` (40) lines of output, and what to do ("rebase on main
and submit again"). The coordinator gets a one-line note. The lane goes on with the next item.

A push the remote refuses for a rule, not a race (a protected branch, no permission) stops the
lane for the whole repository and tells the coordinator, since every item would fail the same way.

### 7.5 The end of a group

When every item is landed or dropped, or the coordinator calls `group_close`:

- The hab closes idle members first (below), then removes the worktrees of landed items whose
  sessions have ended (7.3 step 7 did those that ended earlier).
- It lists the leftovers on the card and in one message to the coordinator: worktrees of dropped
  items (with uncommitted changes or not), branches not landed, and per-run build folders the
  project names in `flightdeck.json`. It removes nothing with uncommitted changes.
- It closes idle members (`group.close_members`, default true), the leads and reviewers of
  landed items among them. A working member is left running and listed, and its worktree with
  it.
- It marks the Logbook plan record `done` (or `dropped` when nothing landed).

---

## 8. The heavy-run lock and the disk watch

### 8.1 The lock

One lock per host for build and test runs. It stops three agents from running three full test
suites on one laptop at once.

- **How a run takes it.** The agent wraps its command: `gff heavy -- make test` (`gff-hab heavy`
  is the same command). The CLI asks its hab for the lock, waits in order, runs the command,
  and gives the lock back when the command exits. The command's output and exit code pass
  through unchanged.
- **Order.** First come, first served, per host. `heavy.slots` sets how many runs hold it at
  once (default 1).
- **Release.** On the command's exit, on the CLI's death (its connection to the hab drops), and on
  the pane's close. A process that outlives the CLI is not the hab's concern: the release follows
  the CLI.
- **gff-hab restart.** The lock lives in memory. A running CLI reconnects with its ticket and keeps
  its place (holding or waiting). For `[heavy] restart_hold` (10 s; 15.3) after a start gff-hab
  grants nothing, so earlier
  holders can come back first.
- **Long holds.** A hold past `heavy.long_hold` (60 minutes; 15.3) raises one notice and
  tells the coordinator of any group with a member waiting. The hab does not kill the run.
- **Who uses it.** Any session on the host, in a group or not, and the lane's checks. The group's
  seeds and the agent kit's manual tell members to wrap builds and tests. A run that does not
  wrap is not held back: the lock is a convention, as in the workflow it models.
- **Waiting shows.** A waiting CLI prints one line ("waiting for the heavy-run lock: 2 ahead") to
  stderr. The card's waits line and the status board show who holds it and who waits.

Decision: a wrapper command, not an MCP tool. Reason: the run is a shell command the agent's own
shell tool starts; only a wrapper can hold the lock for exactly the command's life and release it
when the command dies.

### 8.2 The disk watch

- **Free space per host** joins the catalog: the volume of the hab's state directory and the
  volume of the worktree root, each `{path, free_bytes, total_bytes}`, from the host stats the hab
  already reads.
- **Threshold.** `heavy.disk_min_free` (default 10 GiB). Below it on a host, that host's heavy
  runs wait: the lock grants nothing. Runs already holding it go on.
- **Recovery.** Grants start again when free space is `[heavy] disk_resume_margin` (2 GiB)
  above the threshold, so a run that
  frees a little does not flap the lock.
- **Who hears.** The coordinator of each group with a member on that host gets one channel message
  per low-disk episode: host, free space, the largest leftovers the hab knows (7.5). The card's
  waits line shows it. Hosts with no group get one notice.

### 8.3 Built choices (stage B5, task-aagbyg89)

`internal/heavy`, wired into gff-hab; `gff heavy` and `gff-hab heavy` share its CLI.

- **The calls.** `heavy.take {ticket?, holding?, since?, granted?, pane_token?, command?,
  wait_ms?}` answers `{ticket, state, ahead, since, granted?, reason?}` at once, or with
  `wait_ms` (at most 600 s) once the ticket holds or the wait ends. A ticket is 32 hex digits the
  hab makes. The connection that last took a ticket holds it; its close releases it. A
  `pane_token` (the pane's `GFF_HAB` token, as `content.show` takes it) names the pane: the pane's
  exit or close releases the ticket, and `heavy.status` names the pane's session. A token
  that names no pane yet (its holder not attached after a restart) is no error: the ticket goes
  on without a pane, and a later take with the token binds it.
  `heavy.release` of an unknown ticket is no error. Capability `heavy`.
- **A restart.** The CLI keeps the last answer and sends it back with its ticket on a new
  connection: a ticket the hab does not know holds again at once when it held, else it waits in
  the place its `since` gives it. A long hold counts from its old `granted`. A waiting CLI whose
  gff-hab stays away for 2 minutes, and a CLI that finds no gff-hab or one without the
  capability, runs the command without the lock and says so on stderr. A holding CLI dials
  again until the command ends.
- **Signals.** SIGINT and SIGQUIT reach the command from the terminal, as with any foreground
  job; the CLI passes SIGTERM and SIGHUP on. A signal while waiting releases the ticket and
  exits with 128 plus the signal, also while the CLI dials a gff-hab that is down. A command
  that is not found exits 127.
- **The disk watch** reads both volumes every 30 s. Any volume below the threshold makes the
  host low; grants come back when every volume has the threshold plus the margin free.
  `heavy.disk_min_free` 0 never holds runs. The catalog section `heavy` carries `slots`,
  `holding`, `waiting`, `disk_low` and the volumes; a volume's `free_bytes` there moves only when
  it changed by 1 GiB or more or the low state changed, so the catalog's generation does not
  follow every write. `heavy.status` has the latest reading.
- **Who hears, before groups.** No session coordinates a group until stage B3, so a low-disk
  episode raises one notice; the coordinator source is a hook B3 fills (the tests name a
  session through a test seam, and that session gets one channel message per episode). The
  message names the host, the free space, the volume and the free space that resumes grants;
  the leftovers of 7.5 come with B4. The long-hold notice is built; its message to coordinators
  with a member waiting comes with B3.
- **Where it shows.** gff's status board lists, under Heavy runs, each host whose lock is in use
  or whose disk is low, from the catalog. The group card's waits line comes with B3.

---

## 9. Context supervision and sibling actions

Supervision is hab-side and on by default for members (`swarm-study.md` 3.2). The coordinator does
not remind anyone.

### 9.1 Thresholds

| Value | Default | Kind | Meaning |
|---|---|---|---|
| `group.compact_at` | 40 | Setting | Compact a member at this `context_pct` |
| `group.fresh_after_compacts` | 4 | Setting | After this many compacts, the next trigger starts a fresh session |
| `group.fresh_transcript_mb` | 4 | Setting | A transcript this large, in MB, starts a fresh session at the next trigger |
| `group.handoff_at` | 60 | Setting | For a harness the hab cannot compact (9.2), the `context_pct` that starts a fresh session |

- **Where `context_pct` comes from.** `Session.context_pct` (`hab-logic-api.md` 2.2) is specified
  but no adapter fills it yet. Claude Code: the status-line input and the transcript's usage, as
  self-compact's nudge reads them. Codex: the rollout's `token_count` events, when they carry the
  model's window (to verify in B6). opencode: none. Stage B6 wires these into `context_pct`
  first.
- A member with no `context_pct` is supervised by transcript size only. That covers opencode,
  and Codex until its reading exists.
- SDK members manage their own context (`swarm-study.md` 2.2) and are not supervised.
- **The context nudge is off for members.** Self-compact's `agent.context_nudge` would tell a
  member to compact itself on top of supervision. Supervision suppresses it for group members,
  and counts every `agent.compact` of a member, its own included, toward
  `group.fresh_after_compacts`.

### 9.2 A compact

When a member reaches `group.compact_at`, its own hab:

1. Waits until the member is not in use (`rounds.md` 6.1). A member that stays in use is checked
   again at each of its turn ends; supervision never skips for good.
2. Sends one message from Flightdeck: "context is at N%: send your progress with
   `group_note`, then end your turn." The hab writes that text as a note on the item's task
   (13 Q5). The member's turn ends.
3. Runs a compaction for the member's pane with the follow-up "continue item <title>; your notes
   are on task <id>". This is self-compact's machinery started by the hab (12.4).
4. Counts the compact on the member's record.

Self-compact covers Claude Code only today. A member in another harness skips steps 2 and 3: it
goes straight to a fresh session at the hand-off level (9.3).

### 9.3 A fresh session

When a member passes `group.fresh_after_compacts` or the transcript size, or a harness the hab
cannot compact reaches the hand-off level:

1. Wait until not in use, as in 9.2.
2. Message: "send a hand-off with `group_note`: what is done, what is next, open questions; then
   end your turn." The hab writes it as a note on the item's task.
3. When the turn ends and the note is there (or after `[groups] handoff_note_wait`, 10 minutes,
   with no note, which the card flags), the hab holds new channel deliveries to the old session, renames it `<name>-<n>`,
   closes it, and starts a new member with the old name, in the same worktree, harness and
   model. Its seed is the item brief plus "read the notes on task <id> and continue".
4. **Messages carry over.** Messages held for the old session move to the new one in their order
   (12.8). Without that, channels would refuse them as sent to a renamed session (`mesh.md` 6.5).
5. The new member keeps the old one's address, so the coordinator and siblings need nothing new.
   The record keeps `replaced: <old session id>`. The old conversation stays resumable.

Decision: the hand-off is a note on the item's Logbook task, written by the hab from the member's
`group_note`. Reason: the note is durable, synced to every host, and sits beside the item the
fresh session reads anyway; no new record kind is needed. The hab writes it so that
`logbook.approval = agents` does not turn it into a proposal the fresh session cannot read
(owner, 2026-11-06; 13 Q5).

### 9.4 Sibling actions

Any member, the coordinator, or the user from the card can ask for a compact or a fresh session
for another member: tools `member_compact` and `member_handoff`.

- The member's hab runs it as in 9.2 or 9.3, under the rounds lock: never into a session in use.
  It waits up to `[groups] sibling_wait` (10 minutes), then ends as `skipped` with the reason, as a round run does
  (`rounds.md` 6.3), and tells the asker.
- One supervision action per member at a time. A second request while one runs gets `conflict`.
- An agent cannot act on a member outside its own group.

### 9.5 Built choices (stage B6, task-humtiqof)

gff-hab's supervisor (`cmd/gff-hab/supervise.go`) runs on every hab with the holder link, for
that hab's own members.

- **`context_pct`.** The supervisor reads each member every 2 s and stores the reading on its
  session (`Session.context_pct`), which the catalog copies carry to the home hab. Claude Code:
  the context nudge's reading (the status line, else the transcript). Codex: the last
  `token_count` of the rollout, `info.last_token_usage.total_tokens` (else input plus output)
  over `info.model_context_window`, else the window of the last `task_started`. Codex 0.166
  writes both; a `token_count` with `info` null is no reading. opencode: no reading and no
  transcript size yet, so an opencode member is not supervised.
- **Counting.** Every Claude Code `SessionStart` with source `compact` of a member counts on its
  session (`Session.compacts`), the member's own and the user's included; a compaction the hab
  ran counts once, at its end. A fresh session starts at 0. The home hab copies the count into
  the record.
- **Triggers.** Claude Code on a hab with self-compact: at `group.compact_at`, a fresh session
  when the member has `group.fresh_after_compacts` compacts or its transcript is
  `group.fresh_transcript_mb` or more, else a compact. `group.compact_at` 0 turns compacts off;
  the member then goes fresh at `group.handoff_at`, as other harnesses do. A member with no
  reading goes fresh at the transcript size. An automatic action that failed waits a minute
  before the next try.
- **The messages** are Flightdeck notes, sent through `session.send`'s path under the rounds
  lock's guard, never while the member is in use. The compact's ask names the percentage and
  the `group_note` arguments; the follow-up names the item, its title and its task. The turn
  counts as ended once the agent worked and came back to its prompt, at most
  `[groups] handoff_note_wait`. The compaction is self-compact's, started by the hab, with the
  same `agent.compact` events.
- **The swap.** The hab holds channel deliveries to the member from the moment the hand-off ask
  goes in: a message that arrives during or after the hand-off turn waits, so none starts a turn
  in a pane the swap closes. It refuses to swap when another turn started meanwhile. The hold
  keeps the member's name resolving to the held session while no session holds it, so a
  message sent to the name between the rename and the new session's start waits instead of
  being refused. The hab then writes a swap mark on the old session's record, renames it
  `<name>-<n>` (the first free n) and takes it out of the group (no group, no envelope), starts
  the new session under the old name in the old one's folder with its harness, model, group,
  envelope and a trust cover, records `Session.replaced` on it, moves the held messages, closes
  the old pane and clears the mark. A failed start puts the old name and group back and lets
  the held messages go to the old session. A start that fails after it made the session (a
  timeout) finds the new session by name in the group and goes on with it, so the group never
  has two sessions with the member's name. A gff-hab stopped mid-swap repairs it at its next
  start from the mark: with no new session it puts the old name and group back; with one, it
  holds the old session's deliveries from the channels' start, moves its messages, closes the
  old pane and clears the mark. The seed is the lead seed of the
  item from the group record (read from the home hab), then "read the notes on task <id> and
  continue". A hand-off with no note within `[groups] handoff_note_wait` still swaps, and the
  action's reason says "no hand-off note". The home hab's record follows the name to the new
  session and sets `replaced`.
- **Sibling actions** are `member.compact`, `member.handoff` and `member.action {id}`. The
  asker's hab finds the member by name in its own sessions, else in the catalog copies, and
  hands the call over the link; a forwarded call never forwards again. It answers at once with
  the action (`queued`, `running`, `done`, `skipped`, `failed`). The end reaches a pane's asker
  as a Flightdeck note: from the member's hab when the asker is local, else from the asker's own
  hab, which reads `member.action` every 2 s. The user reads `member.action`.
- **Not built.** Only Claude Code compacts. opencode has no reading. The card's flag for a
  hand-off with no note is the action's reason only, since the card is not built.

---

## 10. Usage pacing

Groups spend several times a chat's tokens (11). The usage governor (Part 14 G, task-k0ydtgae)
gives each group a share of the plan window. This spec needs from it (12.2):

- **A consumer per group,** `group:<id>`, with a share the governor's settings set.
- **A check before a new turn:** may a member of this group start a turn now? Answer: yes, or wait
  until a time, with the reason.
- **An event** when the answer changes.

What a group does with it:

- **At the cap the group is `paused`** with reason `usage` (5.6). Member habs hold what starts
  new turns: channel messages into members (from the coordinator, siblings, the lane and
  supervision) and new starts. Working members finish their current turn. The card says
  "paused: usage, until <time>".
- **The coordinator hears once** per pause, and once at resume.
- **A harness with no usage readings** (opencode) is unpaced, not blocked, as the governor task
  says. The card marks such members "unpaced".

---

## 11. When a group is the wrong tool

- **Use a group** for many separable items, or for work that needs a review in another harness.
  Its value is cross-host, cross-harness and visible.
- **Do not use a group for one change.** Multi-agent work costs several times a chat's tokens:
  Anthropic's research system spends about 15 times a chat; Claude Code agent teams about 7 times
  in plan mode (`swarm-study.md` 3.3).
- **The card warns.** A plan with one item shows "one item: a single session costs less" on its
  confirmation card. A plan whose items all touch the same paths shows "these items overlap and
  will land one after another".
- **The agent kit's manual** tells agents when to propose a group and when to work alone.

---

## 12. Shared interfaces with other parts

Groups depend on pieces other specs own. This section states what groups need. The owning spec
defines the internals. Each row is **shared**: a change on either side updates both specs.

### 12.1 Capcom as coordinator (Part 14 A1, A2; stage B8) — shared

- The Capcom service holds the channel address `capcom` (`capcom-service.md` 2.4), so members
  and the lane reach it as they reach any coordinator.
- **Capcom always counts as running.** Its thread takes messages while the engine is busy or
  down (`capcom-service.md` 9.1), so reports to a Capcom coordinator go in at once and never
  wait in the record's inbox (5.5).
- Capcom's tool list includes the coordinator's group tools of 15.2 (`capcom-service.md` 5.1).
  An event turn may not call them (`capcom-service.md` 9.4), except under a Capcom job's `group`
  action (`capcom-service.md` 9.6).
- The group card links from Capcom's thread, and Capcom sums up the result there.
- A group whose coordinator is Capcom has Capcom's home host as its home hab. When the Capcom
  mark moves, the group moves with it, also when the old home slept through the re-assignment
  (5.7).
- Capcom has no shell. The heavy-run lock and the lane do not need one from the coordinator.

### 12.2 The usage governor (Part 14 G) — shared

As in section 10: a consumer per group, a check before a new turn, an event on change, and
"unpaced" for a harness with no readings.

### 12.3 The spawn verb (task-6iwat66b, stage B2) — shared

As in 4.1: host, harness alias, model, folder, name, seed and the group fields in; session id,
name and address out; the seed as a start envelope naming the spawner.

Built (task-6iwat66b, `harness-adapters.md` Spawn): the `spawn` tool with host, harness or alias,
model, folder, name, seed and `mode` (teammate or detached); the `[aliases]` table; the address
in `ConfirmResult.address`; teammate reports through `session.create`'s `report_to`. B2 adds
`{group, role, item}` to `ConfirmStart` and the tool, and `trust_cover` to `session.create`,
both optional fields beside these. Built (task-7kzvfcna): both, as 4.2's built choices say.

### 12.4 Self-compact (task-f2fhhr6p) — shared

Self-compact compacts only the caller's own pane today. Supervision needs the same steps started
by the hab for a member's pane, with a follow-up the hab writes. The states and events
(`agent.compact`) stay as built. Supervision also needs the context nudge turned off per session
(group members), and `agent.compact` events for every compaction, so it can count them.

### 12.5 The `group` round mode (Part 13 D1) — shared

A round in `group` mode, with any trigger (a watcher's included), sends its prompt and event batch
as a message to the group's coordinator (`rounds.md` 5.2). Groups provide the coordinator's
address and the group's state, in the catalog `groups` section (5.1) and in `group.get`. A round
on any hab reads them from its catalog copies at the start of each run.

- The message is a channel message with the rounds `round` mark, delivered under the round lock
  on the coordinator's hab (`rounds.md` 5.2, 6). It is not a report (5.5): a coordinator that is
  not running makes the run `skipped`, and the next run carries the batch.
- A group in any state but `ended` and `dropped` counts as running. A group that is `ended` or
  `dropped` completes the round, with the `target_gone` notice, "Its group ended"
  (`rounds.md` 10.1, 10.2).
- A Capcom coordinator gets the message as a round message (`capcom-service.md` 9.5).

### 12.6 Trust cover on a start (`app.md` 7.10, B2) — shared

`session.create` from a peer link gains `trust_cover {group, confirmation, harness,
branch_prefix}`. The member hab answers its own pane's trust dialog only for a folder that
matches the cover (4.2), and holds the `blocked:trust` report up to `[groups] trust_hold` (10 s)
while it does. This is an
exception `app.md` 7.10 must state: the hab answers, the user does not, because the user
confirmed the plan, or turned the plan's confirmation off (`confirmation: none`, 13 Q2).
Built (task-7kzvfcna): `app.md` 7.10, Group members, states it.

### 12.7 Worktrees (`hab-logic-api.md` 3.10, B4) — shared

`worktree.create` gains a `fetch` option: fetch the project's remote before the branch is made.
A `group/` branch made from the remote is not foreign.

### 12.8 Channels across a member swap (`mesh.md` 6.5, B6) — shared

Today a held message to a session renamed or replaced since it arrived is refused. A group's
fresh-session swap (9.3) needs one exception: the hab moves held messages for the old session to
its named successor, in order, and the sender sees them delivered to the same address.

---

## 13. Owner decisions

The owner answered this spec's open questions on 2026-11-06 (plan-xagejg31 (q), and (r) for Q7).
None are open.

| # | Question | Decision (owner, 2026-11-06) | Where |
|---|---|---|---|
| Q1 | Is `confirm.group_plan` a levelled setting, and which plans ask? | Levelled: on at Safe and Balanced, off at Bypass. Every plan asks while it is on; `app.md` 20.2 gains this one exception for Balanced | 3.2, 15.3 |
| Q2 | With confirmation off, may the hab answer the trust prompt for a group's new worktrees? | Yes, always: the hab answers trust for every worktree it makes for the group | 4.2, 12.6 |
| Q3 | A project with no `lane.ci`: may the lane land on its local checks alone? | Yes; the card says "no CI" | 7.3 step 5 |
| Q4 | Does the lane close a lead and its reviewer after a land? | No. They keep running; they end at the group's end, when the coordinator drops them, or when the user closes them | 7.3 step 7, 7.5 |
| Q5 | Under `logbook.approval = agents`, may the hab write a group's claims and hand-off notes so they land at once? | Yes: the hab writes them as the group, with the member as the source session. Other member writes stay proposals | 4.3, 9.3 |
| Q6 | May the user move a group's home hab? | Yes, in the first build, including a Capcom-coordinated group when the Capcom mark moves | 5.7, stage B9 |
| Q7 | Are the spec's fixed limits settings? | Yes (plan-xagejg31 (r)): every cap is a setting with its earlier value as the default; only values that are not limits stay fixed | 15.3 |

---

## 14. Edge cases

| Case | Behaviour |
|---|---|
| The coordinator exits | The group runs on. Reports wait in the record's inbox (5.5) and go in when it is resumed; the card shows them meanwhile |
| The coordinator's host sleeps | The lane and the record wait. Members on other hosts keep working; their messages wait in outboxes (`mesh.md` 6.3) |
| A member's host goes offline | The member shows stale on the card. Its item stays in its state. Nothing is restarted elsewhere |
| A member's session is closed by the user | For an item not yet landed, the item goes to `stalled`; the coordinator is told and may call `group_retry` (inside the envelope). For a landed item, nothing changes but the member's state; its worktree goes once it has no uncommitted changes |
| The user types into a member | Allowed. The member is in use, so supervision and sibling actions wait (9) |
| A member asks a permission | An ordinary approval: card, push, answer. The group card lists it under waits |
| Two groups on one repository on one hab | One lane serves both, in `ready` order |
| Two habs land on one repository | The fast-forward push fails for the later one; it rebases and tries again (7.3 step 6) |
| A plan edit drops a member that already runs | `group_amend` asks once; the dropped member is closed when idle |
| gff-hab restarts mid-land | The lane starts the item again at step 1. When the saved rebased head (7.3 step 4) is already on the target, the item is marked `landed` |
| gff-hab restarts while the plan waits for its confirmation | The confirmation is gone; the group becomes `dropped` and the coordinator is told (3.2) |
| The user moves the home while an item is landing | Before its push the item goes back to the front of the queue; after it, the land finishes first, at most `[groups] move_drain` (10 minutes; 5.7) |
| The new home goes offline mid-move | No commit: the old home keeps the group after `[groups] move_answer` (60 s) and says why on the card (5.7) |
| The old home crashes or sleeps after its tombstone | The new home commits from the tombstone in its catalog copy; until then calls get `unavailable` and are retried (5.7) |
| A move asked while the home sleeps | Queued on the target hab, which runs it when the home is back (5.7) |
| The Capcom mark moves while a Capcom group's home sleeps | Reports wait in member outboxes and the lane waits; the group moves, and the reports reach the new Capcom, when the old home links again (5.7) |
| The home hab is offline when a lead submits | The submit waits in the member hab's outbox; the reviewer starts when the home hab is back |
| A heavy run's CLI is killed with SIGKILL | Its connection drops; the lock goes to the next waiter |
| Disk low while the lane's checks wait | The lane waits like any heavy run; the card shows it |
| The lead pushes while the reviewer fixes | The reviewer pushes to its own branch (6.4). A new lead commit after the submit makes the review branch no longer a fast-forward for the lead; the lead's next submit restarts the review on the new head |
| A Logbook write fails (no Logbook on the home hab) | `group_create` is refused with "groups need Logbook" before any card |

---

## 15. API, CLI and settings

Names and shapes; the schema comes with each build stage. Calls follow `hab-logic-api.md` 3.3
to 3.6.

### 15.1 Hab API

| Call or event | Shape | Stage |
|---|---|---|
| capability `groups` | — | B3 |
| `group.create {plan}` | → `{group, confirmation?}`; the plan as in 3.1 | B3 |
| `group.amend {group, change}` | → `{confirmation?}`; one card for the change | B3 |
| `group.get {group}`, `group.list {state?}` | → the record (5.1), or the short forms. The tool `group_status` calls `group.get` | B3 |
| `group.report {group, text}`, `group.note {group, item, text}` | A report to the coordinator (5.5); a note the hab writes on the item's task (9) | B3 |
| `group.pause`, `group.resume`, `group.close {group}` | → the record | B3 |
| `group.retry {group, member}` | Start a failed member again | B3 |
| `group.sync {group, revision, envelope?, pause?}` | Peer link only, from the home hab: the envelope after an amend and the pause, at once (3.3, 5.6) | B3 follow-up |
| `group.move {group, host}` | The user's move of the home hab (5.7); from a pane route `denied` | B9 |
| `group.adopt {record, generation}`, `group.adopt_commit {group, generation}` | Peer link only: the hand-over of 5.7 | B9 |
| event `group.updated` | `id`, `revision`, `data` the record; host scope | B3 |
| catalog section `groups` | The short form per group, with the coordinator's address (5.1) | B3 |
| `lane.submit {group, item}` | From the item's lead only; starts the review | B7 |
| `review.verdict {group, item, verdict, findings, fixed}` | From the reviewer's pane only | B7 |
| `heavy.take {ticket?}`, `heavy.release {ticket}` | The lock, on a held connection | B5 |
| `heavy.status` | Holders, waiters, disk state | B5 |
| `member.compact`, `member.handoff {group, member}` | Sibling actions; → the action (9.5) | B6 |
| `member.action {id}` | One action, by the member's hab (9.5) | B6 |

### 15.2 Agent kit

| Tool (MCP and `gff-hab act`) | Who | What |
|---|---|---|
| `group_create` | Coordinator | Propose the plan; waits 45 s for the confirmation |
| `group_status` | Any member, the coordinator | The record |
| `group_amend`, `group_close`, `group_retry` | Coordinator | As in 15.1 |
| `group_report` | Any member | A report to the coordinator (5.5) |
| `group_note` | Any member | A progress or hand-off note on its item (9) |
| `lane_submit` | Lead | Submit the item's branch |
| `review_verdict` | Reviewer | The verdict |
| `member_compact`, `member_handoff` | Any member, the coordinator | Sibling actions |

CLI: `gff group ls|show|pause|resume|close|move`, `gff heavy [--] <command...>`. Built in B3 as
`gff-hab group ls|show|pause|resume|close`; `move` comes with B9.

### 15.3 Settings

Owner, 2026-11-06 (plan-xagejg31 (r)): every cap is configurable. Each limit this spec names is a
setting, with its earlier fixed value as the default. Two homes, by who tunes it:

- **Hab-side settings** (`app.md` 20.5) for what the user tunes in the app. Stored on every hab
  unless the row says "this hab only". A value outside its range: `settings.set` refuses the
  call with `invalid` and `data.id`, and the stored value stays. A `[settings]` entry outside its
  range is logged and left out, and locks nothing (`app.md` 20.5, built choices).
- **`[groups]` and `[heavy]` keys** in the hab's config file for tuning with no row in the app,
  as `[rounds]` is (`rounds.md` 13). A value outside its range, or of the wrong type, is logged
  once and the default runs. The home hab's keys rule a group's record, lane and moves; each
  member hab's rule its own members.

**Hab-side settings**

| Id | Title | Type | Default |
|---|---|---|---|
| `confirm.group_plan` | Confirm a group's plan before it starts | bool, levelled | Safe true, Balanced true, Bypass false (13 Q1) |
| `group.confirm_max_age` | A plan's confirmation expires after | 1 minute to 24 hours | 60 minutes (3.2) |
| `group.max_leads` | Leads at once | integer 1 to 10 | 3 |
| `group.max_workers` | Workers per lead | integer 0 to 5 | 2 |
| `group.compact_at` | Compact members at context | percent, 0 = off | 40 |
| `group.fresh_after_compacts` | Start fresh after this many compacts | integer 1 to 20 | 4 |
| `group.fresh_transcript_mb` | Start fresh at this transcript size | integer 1 to 64 MB | 4 (9.1) |
| `group.handoff_at` | Start fresh at context, for a harness the hab cannot compact | percent 20 to 95 | 60 (9.1) |
| `heavy.slots` | Heavy runs at once on this host | integer 1 to 8 | 1; this hab only |
| `heavy.disk_min_free` | Hold heavy runs below this free space | integer GiB 0 to 4096, 0 = never | 10; this hab only |
| `heavy.long_hold` | Notify when a heavy run holds the lock longer than | integer minutes 5 to 1440 | 60 minutes; this hab only (8.1) |
| `group.close_members` | Close idle members at the end | bool | true |

**`[groups]` and `[heavy]` keys**

| Key | Default | Range | Section |
|---|---|---|---|
| `[groups] trust_hold` | 10 s | 1 s to 60 s | 4.2 |
| `[groups] keep_ended` | 30 days | 1 to 365 days | 5.2 |
| `[groups] inbox_max`, `inbox_days` | 200 reports, 7 days | 10 to 5,000; 1 to 90 days | 5.5 |
| `[groups] move_drain` | 10 minutes | 1 minute to 2 hours | 5.7 |
| `[groups] move_answer` | 60 s | 10 s to 10 minutes | 5.7 |
| `[groups] move_retry` | 2 s | 1 s to 60 s | 5.7 |
| `[groups] tombstone_keep` | 30 days | 7 to 365 days; a shorter one could lose a commit a sleeping hab has not seen | 5.7 |
| `[groups] queued_move_days` | 7 days | 1 to 90 days | 5.7 |
| `[groups] max_findings`, `brief_findings` | 5, 3 | 1 to 20; 1 to `max_findings` | 6.3, 5.4 |
| `[groups] reviewer_idle` | 30 minutes | 5 minutes to 24 hours | 6.3 |
| `[groups] lane_tries` | 3 | 1 to 20 | 7.3 |
| `[groups] fix_output_lines` | 40 | 0 to 500 | 7.4 |
| `[groups] handoff_note_wait` | 10 minutes | 1 minute to 2 hours | 9.3 |
| `[groups] sibling_wait` | 10 minutes | 1 minute to 24 hours | 9.4 |
| `[heavy] restart_hold` | 10 s | 0 to 60 s | 8.1 |
| `[heavy] disk_resume_margin` | 2 GiB | 0 to 100 GiB | 8.2 |

**Fixed values kept**

| Value | Why it is not a setting |
|---|---|
| `group_create` waits 45 s for the answer, then says "still waiting" (3.2) | Not a cap: the answer still comes later as a message. It is `start_session`'s wait (`capcom.md` 6.4), kept under the tool-call time a harness gives an MCP call |
| The reviewer harness order Codex, Claude Code, opencode (6.2) | Not a limit: an order. The plan names the reviewer, and the user edits it on the card |
| The catalog pull every 15 s (5.6, 5.7) | `mesh.md` 5 owns it; this spec does not change it |

**Changes to other specs.** Each stage edits `app.md` 20.3 to 20.5 as it builds a row. Stage
B2 built the hab's `confirm.group_plan` row (its group starts need the off case) and added it
to `app.md` 20.3, and built `[groups] trust_hold`; the app's row is not built yet. Stage B3 built the hab rows `group.confirm_max_age`, `group.max_leads`, `group.max_workers` and
`group.close_members` and the keys `[groups] keep_ended`, `inbox_max` and `inbox_days`; B5 adds the `heavy.*` rows; B6 built `group.compact_at`,
`group.fresh_after_compacts`, `group.fresh_transcript_mb` and `group.handoff_at`, and the keys
`[groups] handoff_note_wait` and `sibling_wait`. Each stage that
reads a `[groups]` or `[heavy]` key adds it to `docs/examples/hab.toml`.

The project's `flightdeck.json` gains `lane.checks`, `lane.ci` and `lane.build_folders`, read
from the target branch (7.3).

---

## 16. Build stages and acceptance

Each stage is a task in the Part 15 plan (plan-vu079jrc). Core stages (B3, B4, B6) get a
fresh-eyes reviewer before they close.

| # | Stage | Builds | Depends on | Task |
|---|---|---|---|---|
| B1 | This spec | — | Spawn verb spec | task-5ntfv32q |
| B2 | Spawn and trust | The spawn verb's group fields; `trust_cover` and the held trust report (12.6), also with `confirmation: none`; the Codex trust rule and its keys; addresses to both sides | task-6iwat66b | task-7kzvfcna |
| B3 | Record, card, confirmation | `group.*`, the record and its file, the Logbook plan and tasks with hab-written claims and notes, the envelope in each start, `confirm.group_plan`, reports and the inbox (5.5), pause (5.6), the card in the app and gff, the catalog section | B2 | task-nz4aglbr |
| B5 | Lock and disk | `gff heavy`, `heavy.*`, the disk fields in the catalog, the threshold and the messages | B1 | task-aagbyg89 |
| B7 | Review | `lane_submit` up to the verdict (item states `review`, `fix`, `ready`), reviewer choice, `review_verdict`, findings on the card, fix in place on the review branch | B2, B3 | task-3eez53r6 |
| B4 | Stacking and lane | Worktrees per lead with `fetch` (12.7), stacking, the lane from `ready` through its seven steps, back to the lead, the end of a group | B3, B7 | task-ur5zmuah |
| B6 | Supervision | `context_pct` from Claude Code and Codex; compact, fresh session, sibling actions, the settings; the hab-started compaction and the nudge off for members (12.4); messages across a swap (12.8) | B3, task-f2fhhr6p | task-humtiqof |
| B8 | Capcom coordinates | Group tools for Capcom, the card from its thread | Part 14 A2, B3 | task-itwl1sbg |
| B9 | Move the home hab | `group.move`, the hand-over (freeze, drain, adopt, commit, commit from the catalog, stale pending copies), the queued move on the target hab, `home` and `generation` in the record and catalog, forwarding and `moved` refusals, re-pointing by generation, the queued move, the always-on line at creation; moving Capcom groups with the Capcom mark | B3, B4; its Capcom rows after B8 | task-3vwhq3dn |

Order: B2 and B5; then B3; then B7 and B6; then B4; then B9; B8 after Part 14 A2, and B9's
Capcom rows after B8. Usage pacing (10) lands with
Part 14 G and wires into B3's `paused` state.

### 16.1 Acceptance: the friction table

The friction of `swarm-study.md` 4 is the acceptance of Part 15. Each row passes when the user
never opens a pane, answers a trust prompt or reminds an agent. Every row is an end-to-end test
with the real holder and gff-hab, two habs over the ssh stand-in, and fake harnesses that report
over OSC 7501 and hooks.

| Friction | Test passes when | Stage |
|---|---|---|
| The owner opened the Codex pane | A Claude Code coordinator's `group_create` with a Codex lead on the other hab starts it there after one confirmation; no client opens or types into any pane | B2, B3 |
| The owner approved Codex's folder trust | The fake Codex draws the trust prompt in its new worktree; gff-hab answers it; no trust card is raised; the seed goes in after | B2 |
| The owner told the coordinator where the pane was | The member's first reply reaches the coordinator by name, with no address typed by anyone | B2 |
| Reminders about context | A member at 41% gets one compact with no message from the coordinator; the fifth trigger starts a fresh session from the task's hand-off note, under the old name; a sibling's message held at the swap reaches the new session; the same at the Safe level (`logbook.approval = agents`) | B6 |
| Reminders about usage | With usage readings at the group's cap, the group is `paused`, no message goes into a member, and it resumes when the window moves (fake clock) | G, B3 |
| Reminders about disk | Free space below the threshold makes a heavy run wait and sends the coordinator exactly one message | B5 |
| Reviews read in full by the coordinator | A Claude Code lead gets a Codex reviewer; the card and the lane get the verdict and at most 5 findings; the coordinator receives the verdict line, not the review | B7 |
| Merges done by hand in order | Two items land on main in order with real git and a fake CI; a third that conflicts goes back to its lead; a failed CI blocks its land; nothing lands without a pass | B4 |

### 16.2 Acceptance: per stage

| Stage | Test |
|---|---|
| B2 | A spawn outside a group still raises the trust card. With `confirm.group_plan` off, a covered folder is answered all the same, and a folder outside the cover raises the card. A covered folder on the other hab is answered by that hab with no card and no push. A pre-answer that meets client input in the last second leaves the card after the hold; with `[groups] trust_hold` at 2 s the card shows after 2 s when the answer fails |
| B3 | A three-member plan raises one confirmation, a plan on one host too; nothing starts before it; with `confirm.group_plan` off it starts at once; the level picker writes true at Safe and Balanced and false at Bypass; with it off and `confirm.start_elsewhere` on, a plan with a member elsewhere asks once. A worker inside the envelope asks nothing, also with the home hab stopped; a new host asks once through `group_amend`. A restart during `confirming` drops the group and tells the coordinator. A plan confirmation expires at `group.confirm_max_age` (set to 2 minutes, fake clock), and `settings.set` refuses 0 with `invalid`. With `[groups] inbox_max` at 10, the eleventh report drops the oldest. A `[groups]` value outside its range is logged once and the default runs. Reports to an exited coordinator wait and arrive at its resume. A user pause outlives a usage resume. A member resumes from a plain terminal. The record survives a gff-hab restart. Swift and Go tests over a fake hab: the card lists items, members and states |
| B5 | Three runs on one host take the lock in arrival order; a killed CLI releases it; a CLI that reconnects after a gff-hab restart keeps its place; a hold past `heavy.long_hold` raises one notice, also with it set to 5 minutes; `[heavy] restart_hold` 0 grants at once after a start; `[heavy] disk_resume_margin` 0 resumes grants at the threshold |
| B7 | The reviewer is in another harness than the lead; with only one harness, the card says "same harness"; a `fix_needed` moves the item to `fix` and reaches the lead; a pass with fixes names a review head that descends from the submitted head, and one that does not is ignored; the coordinator gets one line, not the review; a verdict with more than `[groups] max_findings` findings is refused, and with `max_findings` 2 the third is; `[groups] reviewer_idle` at 5 minutes reports an idle reviewer at 5 minutes (fake clock) |
| B4 | After a land the lead and the reviewer keep running and hold no lead slot (a queued item starts), their worktrees stay until their sessions end, and the item does not stall when the user closes them; the group's end closes them when idle; a project with no `lane.ci` lands on its checks and the card says "no CI"; a `fix_needed` item never lands; a pass with fixes lands the review head; a stacked item lands after the item it stacks on with the earlier commits out of its diff; a target that moves during CI makes the lane start again; three moves send it back, and with `[groups] lane_tries` 1 one move does; a lane message to the lead carries `[groups] fix_output_lines` lines; a protected-branch refusal stops the lane; an item branch that empties `lane.checks` still runs the target's checks; a restart after the land push marks the item landed; the end lists leftovers and removes nothing with uncommitted changes |
| B6 | A sibling action against a session in use waits, then skips after 10 minutes, and after 2 with `[groups] sibling_wait` at 2 minutes; `group.fresh_transcript_mb` and `group.handoff_at` set lower start a fresh session sooner; a Codex member with a fake rollout reading goes straight to a fresh session at 60%; a member with no `context_pct` is supervised by size; a member gets no context nudge, and its own compaction counts |
| B9 | Two habs over the ssh stand-in, real git and fake harnesses: `gff group move` moves the record, the lane and the catalog entry; the coordinator gets one line; a member on a third hab sends its next report and `lane_submit` to the new home after one catalog pull, and one sent to the old home is forwarded; the old home's lane worktree goes. An item before its push goes back to the front of the queue; one after it lands first. The new home killed before its answer: the old home keeps the group after 60 s, and after 10 s with `[groups] move_answer` at 10 s. The old home killed after its tombstone and kept stopped past 10 minutes: the new home commits from the tombstone in its catalog copy and runs the lane; calls meanwhile are retried. A move to B that times out, then a move to C: B drops its pending copy when C's commit shows. A move to a hab without the project, a move of an `ended` group and a move from a pane are refused. A move asked while the home is stopped is queued on the target, runs once when the home is back, and a second request replaces the first. With Capcom as coordinator (a fake service): `capcom.assign` to the other hab moves the group with the mark, and the new thread gets the note; with the old home stopped during the assignment, reports wait in the member's outbox, and when the old home links again the group moves and the reports reach the new Capcom. Swift and Go over a fake hab: the card's "Move home" and the new home label |
| B8 | Live check of `swarm-study.md` 2.4 scenario 2: Capcom finds the project, starts a group across two hosts after one confirmation, and replies with the summary |

---

## 17. Sources

- `docs/specs/swarm-study.md` 2.6, 3, 4, 7 and 8; master plan decisions (o) and (p).
- `rounds.md` 5 and 6 (the lock); `capcom.md` 6 (confirmations); `app.md` 7.10 (trust card),
  20.2 to 20.5 (settings); `mesh.md` 6 (channels); `hab-logic-api.md` 3.10 (worktrees);
  `logbook.md` 2.2, 2.4 and 10 (records, links, `claim`); `harness-adapters.md`, Usage.
- Self-compact and the context nudge (task-f2fhhr6p).
- Hacker News item 50380658 (reviewers nearly always find something);
  https://www.anthropic.com/engineering/multi-agent-research-system;
  https://code.claude.com/docs/en/agent-teams; https://cognition.com/blog/dont-build-multi-agents.
