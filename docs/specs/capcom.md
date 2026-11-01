# Capcom: behaviour spec

Status: draft for review. Part 9, study stage.

Capcom is the conversation that needs no project folder. A user opens it from any client and
asks anything: a question about any project, "where should this run?", "tell the agent on the
API repo to rebase", "write that down as a task". Capcom answers, hands off, suggests or files.
It hands work to a running agent at once and shows what it sent. It starts work on a host only
after the user's yes, unless the user turns that confirmation off.

This spec says what Capcom does, what it may read, what limits it, and how the user approves
its writes. Later Part 9 stages build from it. It describes behaviour in our own words and copies
no code.

Capcom builds on other parts. This spec names their pieces and does not define them:

| Piece | Defined in |
|---|---|
| Peers, the catalog of hosts and running agents, channels and the outbox | the mesh spec (Part 3) |
| Sessions, the API rules, `session.create`, `session.send` | `hab-logic-api.md` |
| Agent state, input delivery, the agent kit, `GFF_HAB` | `harness-adapters.md` |
| The Capcom view, approvals, notifications | `app.md` |
| Logbook records and verbs | the Logbook spec (Part 12, not yet written) |
| Memory and flight rules proposals | the Part 8 spec (not yet written) |
| The bar we match | `landscape.md` |

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [The four jobs](#3-the-four-jobs)
4. [What Capcom reads](#4-what-capcom-reads)
5. [Read scope](#5-read-scope)
6. [Confirmations and approval](#6-confirmations-and-approval)
7. [The session host](#7-the-session-host)
8. [Attached context and the entry API](#8-attached-context-and-the-entry-api)
9. [Edge cases](#9-edge-cases)
10. [Decisions](#10-decisions)
11. [Owner decisions](#11-owner-decisions)
12. [Later stages](#12-later-stages)
13. [Sources](#13-sources)

---

## 1. References

### 1.1 The need: a standing "front door" session

A common way to work with many agents today is one long-lived agent session opened in the
parent folder of every project. The user keeps it open beside the project sessions and uses it
to:

- ask questions that cross projects ("which repos still pin the old library?");
- decide where new work belongs, then open a session in that project by hand;
- pass a message to an agent in another terminal tab, by typing or copying;
- write notes and tasks into whatever memory tool the user keeps;
- check what is running on which machine.

It works, but each step is manual. The session sees only the files on its own machine. It has
no list of running agents. It cannot tell which machine has the project checked out. Anything it
writes to memory lands with no review. Capcom does the same jobs with the system's knowledge and
with review.

### 1.2 Paseo: agents place work on other hosts

Paseo is the only tool in `landscape.md` with a related feature (idea 4.14). From its connected
hosts doc:

- Agents get MCP tools to list hosts and to create a workspace, create an agent, send it a
  prompt, and read its status, each with an optional host id. Without the id, a call goes to the
  agent's own daemon.
- A remote create needs an explicit destination workspace, provider and model. The caller's
  folder does not carry over.
- One hop only: a routed call cannot route again. Saving a host grants no access back.
- A lost response leaves the outcome unknown: the caller inspects the destination before it
  retries. Mutations are never replayed on their own.
- No user confirmation step is described.

herdr and Rex have no such feature. None of the three has a folder-free conversation.

### 1.3 What we take

| From | We take | We change |
|---|---|---|
| Paseo | Agents see hosts and start sessions on them through tools; one hop; inspect before retry | The user confirms every start on another host, with a setting to turn that off (owner, 2026-11-01) |
| Paseo idea 4.2 | An idempotency key on each start | The confirmation id is the key |
| The front door session | One standing conversation, open beside the project sessions | It reads the catalog, Logbook and past transcripts on every host, and its writes wait for review |
| The design doc | Answer, hand off, start seeded, file; approval at the end | "Off the record" mode stays deferred: nothing is written without approval anyway |

---

## 2. Model

### 2.1 What Capcom is

Capcom is an ordinary Flightdeck session with three differences:

| Difference | Detail |
|---|---|
| Folder | A hab-owned empty folder, not a project. See 7.1 |
| Manual | A printed Capcom manual in that folder tells the agent its jobs and limits |
| Label | The session record has kind `capcom`. Clients label it "Capcom" and pin it |

Everything else is the same as any session. A supported harness runs in a holder pane. The
adapter reports its state. The chat view reads its native transcript. It survives `gff-hab`
restarts. A plain terminal can resume it.

Capcom is not a router and not a supervisor. It watches no session. It acts only when the user
talks to it, and a running agent stays where it is.

### 2.2 Words

| Word | Meaning |
|---|---|
| Capcom | The folder-free session and its conversation |
| home host | The host whose hab runs the Capcom session |
| read tools | Agent kit tools that read the catalog, transcripts, Logbook and notifications. They never write |
| action | Something that changes the world outside the conversation now: a hand-off, a session start or a project set-up |
| confirmation | The user's yes or no to a session start or a project set-up, asked when Capcom proposes it (6.1) |
| proposal | A pending Logbook record, memory rule or flight rules change |
| review | The step where the user approves, edits or drops proposals |
| seed | The summary a new session starts with, written by Capcom and shown to the user first |

### 2.3 The tools are the agent kit's

The read tools and the action tools belong to the agent kit (Part 4), served by `gff-hab` as a
CLI and as MCP tools. Every agent gets them, not only Capcom. The owner asked that every agent
see the connected machines and suggest where work runs best. Capcom is the session whose manual
says to use them first.

The same read scope (5) and the same confirmation rule (6) apply to every agent that calls
them. The mesh spec (Part 3) uses this confirmation for any agent that starts work on another
host.

---

## 3. The four jobs

Capcom picks one job per request. When unsure, it asks the user.

### 3.1 Answer

Capcom answers from what it can read (4). It names its sources: the host, project, session or
record each fact came from. Answers write nothing and need no confirmation.

Examples: "what did the agent on the docs repo decide about the cache yesterday?" (transcript
search), "which hosts have Xcode?" (catalog), "what tasks are open on the app?" (Logbook).

### 3.2 Hand off to the agent already on a project

The user wants work done in a project where an agent already runs.

1. Capcom finds the project's running agents in the catalog: name, host, harness, state, last
   activity.
2. It picks one, by these rules in order: an agent the user named; an idle or done agent; the
   most recently active one. With two or more equal candidates it lists them and asks.
3. It drafts the message. The draft holds the request in the user's words plus any context
   Capcom gathered, and names Capcom as the sender.
4. It sends at once, with no confirmation (owner, 2026-11-01). `gff-hab` sends through
   channels, wrapped in the agent envelope (the mesh spec, `harness-adapters.md` Input
   delivery). Delivery follows the normal rules: now when the target is idle or done; queued
   until then when it is working or blocked; held in the outbox when its host is offline.
5. The conversation shows a hand-off card: the target agent, its host and state, how delivery
   went, and the full message text. The card records what was sent; it asks nothing.
6. Capcom tells the user what happened: delivered, queued (and why), or held for an offline
   host. The target may reply by name; the reply arrives in Capcom as an agent message.

A hand-off moves no conversation. The target keeps its own context and its project's rules.

### 3.3 Suggest a host and start a seeded session

The user wants new work started, and no agent is on the project, or the user wants a fresh one.

1. Capcom reads the catalog and ranks candidate hosts. It filters out hosts that are offline or
   lack a capability the work needs, and then prefers hosts where the project is present and
   ready on the right branch, and then a host the user named or used last for that project.
2. It shows the top suggestion with its reasons, and the next candidates. When no host fits, it
   says why (missing capability, project not set up, every fitting host offline) and offers the
   set-up action (the mesh spec, set up a project on another machine), which takes its own
   confirmation under the same setting.
3. It writes the seed: the goal, decisions so far, open questions, and links back to the Capcom
   messages it came from. At most 8,000 characters.
4. It asks for a confirmation (6.1). The card shows the host, project, folder, harness, model if
   set, and the full seed. The user may edit the seed or change the host or harness. When the
   user has turned the confirmation off, Capcom starts at once and shows the same card as a
   record.
5. On yes (or at once), the target hab runs `session.create` in the project folder with the
   harness, then sends the seed as the first message, marked as from Capcom on the user's
   behalf. The confirmation id is the idempotency key: a repeated create with the same id starts
   nothing new. With the confirmation off, the hab still issues the id, for the record card.
6. Capcom reports the new session's name and host. Clients show it like any session.

The new session follows its project's rules from then on: the harness loads the project's
instruction files natively because it starts in the project folder. Capcom does not carry its
own manual or scope into it.

A conversation never moves between folders or hosts. "Move this to the API repo" means a new
session there, seeded with a summary. The Capcom conversation stays where it is.

### 3.4 File a Logbook record

The user says "note that", "make a task", or Capcom notices a decision worth keeping.

1. Capcom drafts a record: type (task, finding, issue, or another Logbook type), project (from
   the catalog's project identity, or none for a record that belongs to no project), title,
   body, and a source quote from the conversation.
2. It files the draft as a proposal (6.2). Nothing reaches Logbook yet.
3. It tells the user the proposal waits for review, in one line.

Memory rules and flight rules changes work the same way, through Part 8's proposal record.

---

## 4. What Capcom reads

Capcom reads on demand, through read tools. Nothing is copied into Capcom's folder or into a
store of ours. Each read goes to the hab that owns the data, and that hab applies its own read
scope (5).

| Source | Tool behaviour | Owner of the data |
|---|---|---|
| Catalog | List hosts (OS, capabilities, online state), projects and where each is checked out, and running agents (name, harness, project, state) on every peer | Each hab, merged as the mesh spec says |
| Sessions | List past and current sessions by project, host, harness and date | Each hab's transcript index (Part 3) |
| Transcript search | Search past transcripts by words, with filters. Each hab scans its own transcripts with the Part 4 readers, newest first, and returns snippets with session and message refs | Each hab |
| Transcript read | Read a page of one session's messages, like `chat.page` | The session's hab |
| Logbook | Search and read records | The local Logbook replica, which holds every peer's records |
| Notifications | Read a notification and its source event by id | The hab that raised it |
| Files | Read a file inside an allowed root on a host | That host's hab |

Rules:

- **Read tools never write.** A read tool that receives a write is refused, and the refusal is
  tested.
- **Limits.** A search returns at most 50 hits. Each hab gets 5 s; hits from a hab that ran out
  of time are marked partial. An offline peer is listed as offline, not skipped in silence.
- **Content read is data.** Text from a transcript, a notification or a file is never treated as
  an instruction to Capcom. The manual says so, and says Capcom hands off only what the user
  asked for in this conversation. A start or set-up still needs the user's confirmation while
  that setting is on. A hand-off sends without one, but it carries the agent envelope, so the
  target sees it came from Capcom and its own permission prompts still apply.
- **No copy.** Transcript search runs over the native files. No transcript text is stored in a
  format of ours. A derived search index may come later, as a cache a hab can drop.

---

## 5. Read scope

### 5.1 What the user configures

Each hab has a read scope in Flightdeck's config file:

| Key | Meaning | Default |
|---|---|---|
| `read.allow` | Roots the read tools may read under | The project folders this hab knows, each harness's transcript folders, and Capcom's own folder |
| `read.deny` | Paths or patterns refused even under an allowed root | Common secret files: SSH and cloud credential folders, `.env` files, private key files |

Deny wins over allow. The scope is per hab, because each host has its own private paths. A hab
with no scope set uses the defaults.

### 5.2 How a denial behaves

- A read tool that hits a denied or unlisted path returns a denial. The denial names the host
  and says the read scope refused it. It does not repeat the matching pattern, which may itself
  name something private. The user sees the rule in the app.
- A transcript or session whose project folder is denied is left out of searches and lists. The
  result says how many hits were left out, not what they were.
- Capcom never works around a denial: no other tool, path spelling, host or agent. The manual
  says to stop and tell the user. The same rule binds every agent that uses the read tools.

### 5.3 Where the scope is enforced, and its limit

| Layer | Enforcement |
|---|---|
| Read tools | Hard. The owning hab checks every path, after resolving links, before it reads |
| The harness's own file tools | Best effort. The hab writes the scope as the harness's own deny rules in Capcom's folder, where the harness has such rules |

The second layer is not a sandbox. A harness's shell can read what the user's account can read.
Paseo treats workspace paths as no boundary, and hc says its blocked state "is not an access
boundary" (`landscape.md` 2.17). We match that bar: the read tools are the boundary, and the
harness rules are a cheap guard on top.

### 5.4 Across hosts

Capcom on one host reads another host's data only through that host's hab, under that host's
scope. The home host never applies its own scope to a peer's data, and never keeps a peer's
content past the conversation. Capcom reads every connected peer, within each peer's own scope,
with the default deny list in 5.1 (owner, 2026-11-01; 11, O2). Logbook is readable everywhere,
since every hab holds a replica.

---

## 6. Confirmations and approval

Capcom has two kinds of writes, with two timings.

| Kind | Examples | When the user decides |
|---|---|---|
| Hand-off | A message to an agent already on a project | Never asked. It sends at once; its card records what went (3.2) |
| Start | A session start, a project set-up | Now, before anything happens (6.1), unless the user turned the confirmation off |
| Proposal | A Logbook record, a memory rule, a flight rules change | At review, at the end or on demand (6.2) |

Starts are confirmed now because they are about the present: a start approved later is often
useless. Hand-offs need no tap because the target already works on the project and keeps its
own context and rules (owner, 2026-11-01). Records are reviewed together because a conversation
produces several, and a list is faster to judge than a string of interruptions.

### 6.1 Confirmations

- **The setting.** `confirm.start_elsewhere` in Flightdeck's config, default on, also shown in
  the app's settings. It covers every session start and project set-up an agent asks for on a
  host other than its own, and every start Capcom asks for, since Capcom never works in a
  project folder. Off: the hab runs the action at once and shows the card as a record. This
  follows the general rule that a confirmation that adds friction gets a setting to turn it off
  (owner, 2026-11-01; `app.md`, owner rules).
- With the setting on, a start or set-up tool does not act. It asks the hab for a confirmation
  and returns its id.
- The hab raises a confirmation card in the Capcom view on every client (for another agent, in
  that agent's session view and the approvals area). The card shows exactly
  what will happen (6.3). Buttons: confirm, edit, cancel.
- The tool waits up to 120 s for the answer. If none comes, it returns "still waiting"; the
  answer reaches Capcom later as a message marked as from Flightdeck.
- The first answer wins. A second client's answer gets `conflict`, and every card withdraws, as
  approvals do (`app.md` 7.5).
- An open confirmation expires after 60 minutes, like an approval (`app.md` 7.4). An expired one
  never runs.
- The hab, not the agent, runs a confirmed action, with the values on the card as the user left
  it. An agent cannot confirm its own action: no tool answers a confirmation.
- A confirmation the hab could not finish (a lost peer mid-start) is reported as unknown. Capcom
  inspects the target before it offers a retry, as Paseo does.

### 6.2 Proposals and review

- A proposal tool stores a pending proposal: kind, target (Logbook record, memory rule, flight
  rules change), the full content, the source quote, and the Capcom session and message it came
  from. Logbook (Part 12) and Part 8 own the stored shape.
- Review opens when the user ends the conversation ("done", closing the Capcom view's thread), or
  any time on demand.
- The review list shows each proposal with its source quote. For each: approve, edit, or drop.
- An approved proposal is applied, with who approved it (device id), when, and from which
  client.
- Closing a conversation without review leaves every proposal pending. Pending proposals show in
  the app's approvals area until the user reviews them. They never expire on their own.
- Until the app view exists, a CLI lists and answers proposals.

### 6.3 What every card shows

| Card | Shows |
|---|---|
| Hand-off (a record, not a question) | Target agent, host, harness, state, how delivery went (now, queued, outbox), the message |
| Session start | Host, project, folder, branch, harness, model if set, the seed |
| Project set-up | Host, repository, branch, the bootstrap command the project declares |
| Proposal | Kind, project, title, body, source quote |

---

## 7. The session host

### 7.1 The folder

The home hab makes one folder per Capcom session under its state directory. The folder holds:

- the Capcom manual, written as the harness's native instruction file;
- the harness's project-level settings: the agent kit's MCP tools, and the read scope as deny
  rules (5.3);
- attachments the user sends to Capcom (`app.md` 4.4).

It is not a repository and holds no project files. The hab owns the folder, so writing settings
there needs no opt-in (Part 8's opt-in covers the user's own files).

The harness also loads the user's own user-level instructions, as it does anywhere. Capcom adds
nothing on top beyond the manual.

### 7.2 One standing conversation

- One Capcom session is the standing conversation. Clients open it by default.
- "New conversation" starts a fresh Capcom session in a new folder. The old one stays in the
  list and can be resumed.
- Each Capcom session is a normal session record, kind `capcom`, so it shows in the sidebar, the
  status board and search, labelled Capcom.

### 7.3 Harness and host

- Any supported harness can run Capcom: it needs MCP tools, a resume command and our hooks.
  Claude Code, Codex and opencode all qualify. Which one is the default is a `/consensus` item
  in the Part 9 plan; this spec does not depend on the answer.
- **The user assigns the home host** and may re-assign it at any time (owner, 2026-11-01). Two
  ways, same effect: `gff-hab capcom assign <peer label>` (or `self`), and a host picker in the
  app's Capcom settings. At first run the app asks, and suggests a host marked always on, else
  the hab it paired with first.
- The assignment is a mark in the chosen hab's catalog, with a counter one higher than the last
  assignment it has seen. The highest counter wins, so a host that was offline during a
  re-assignment drops its old mark when it next links.
- **Re-assigning moves no conversation** (3.3). The old standing conversation stays on its host,
  in the list and resumable. The next open starts a fresh standing conversation on the new host.
- Clients find Capcom through the catalog: it lists the home host and each Capcom session with
  its host.

### 7.4 Survival and resume

- A `gff-hab` restart leaves the Capcom pane running, as for every session.
- After an exit or a reboot, `session.resume` starts it again with its resume id.
- A plain terminal resumes it: change to the Capcom folder and run the harness's resume command.
  The folder path is shown in the session's details.

### 7.5 First start

A harness may ask to trust a new folder on its first start in it *(gk)*. The pane shows as
blocked, and the user answers once in the terminal view. Flightdeck does not write the
harness's global trust file to skip it; that is Part 8's question for user-level files.

---

## 8. Attached context and the entry API

### 8.1 Attached context

A client can attach context to a Capcom message: a notification, a session, a file, a Logbook
record, or a console command's output. The hab delivers it as a structured block before the
user's text, never as pasted screen text:

- a header line names the kind, the id and the host;
- a short body: the notification text, the session's name and state, the record's title, or the
  command line and exit status;
- a hint that the read tools fetch the rest by id.

Files go through `attachment.put` into the Capcom folder, as for any session.

### 8.2 "Ask an agent" from a notification

A Flightdeck notification's "ask an agent" action opens the standing Capcom conversation with
that notification attached (owner, 2026-11-01). The aim is no friction and no lost context:

- One tap from the notification lands in the standing conversation, with the event attached to
  the composer (8.1). Earlier context in that conversation stays available to Capcom.
- A "New conversation" action sits beside the attached event. One tap moves the event, and any
  text typed so far, into a fresh Capcom conversation (`capcom.open {new: true, context}`).
- When the standing conversation is busy, the message queues as for any session. The view says
  so, and the "New conversation" action stays one tap away.

The app calls the entry API with the notification id; the hab resolves it. Without a reachable
home hab, the action fails with a clear message. Anything that posts through the hab's
notification API gets the action.

### 8.3 Calls

Each is added under the API revision rules (`hab-logic-api.md` 3.3) and answers `unavailable`
until its stage.

| Call or event | Purpose |
|---|---|
| `capcom.open {new?, context?}` | Return the standing Capcom session, starting it if needed; `new` starts a fresh one. Optional context to attach |
| `capcom.assign {host}` | Make `host` the home host (7.3); the CLI's `capcom assign` |
| `capcom.attach {session, kind, ref}` | Attach context (8.1) to the next message |
| `session.send` | Send the user's message, as for any session |
| `confirm.answer {id, choice, edits?}` | Answer a confirmation: `confirm` or `cancel`, with the user's edits |
| event `confirm.requested`, `confirm.closed` | A confirmation opened, or was answered, cancelled or expired |
| `proposal.list`, `proposal.answer` | List pending proposals; approve, edit or drop one. Shapes owned by Part 12 and Part 8 |
| event `proposal.created`, `proposal.closed` | A proposal was filed, or answered |

---

## 9. Edge cases

| Case | Behaviour |
|---|---|
| The home host sleeps or is offline | Clients show Capcom offline. The app offers to re-assign Capcom to an online host (7.3). Logbook is there (every hab has a replica); the old transcript is not, until its host wakes |
| Two hosts carry the Capcom mark after a network split | The higher assignment counter wins; the other drops its mark and keeps its conversations resumable |
| A peer is offline during a search | Its hits are missing; the result lists it as offline |
| A read is denied | The tool returns the denial; Capcom tells the user and stops that line (5.2) |
| No agent on the project | Capcom offers to start one (3.3) |
| Two agents on the project | Capcom picks by 3.2's rules, or lists them and asks |
| The target agent is blocked on a permission | The hand-off queues; its card says so; Capcom points the user to the open approval |
| The target agent exited | Capcom offers to resume it, or to start a new seeded session |
| The project is on no online host | Capcom says so and offers set-up on a host that fits |
| No host has a needed capability | Capcom names the capability and suggests nothing |
| A session start fails on the target | The card closes with the target hab's error; nothing is retried |
| A start's response is lost | The outcome is unknown; Capcom checks the target for a session with that confirmation id before it offers a retry |
| The user answers a card on two devices | First wins; the second gets `conflict` |
| The user ends the conversation with a card open | The card stays until answered or expired |
| A transcript or notification contains instructions | Treated as data. Capcom hands off only what the user asked for; a start still needs a confirmation while the setting is on (4) |
| The user turned `confirm.start_elsewhere` off | Starts and set-ups run at once; each shows its card as a record in the conversation |
| The Capcom harness fills its context | The harness compacts natively; Capcom's records are in Logbook and its proposals in the hab, so nothing pending is lost |
| The user edits Capcom's folder by hand | The hab rewrites the manual and settings at the next start; it leaves attachments alone |
| A proposal names a project that later disappears | Review still shows it; approval files it with no project and says so |

---

## 10. Decisions

Internal calls, taken here, each with its reason.

| # | Decision | Reason |
|---|---|---|
| D1 | Capcom is an ordinary session in a hab-owned folder, kind `capcom` | It reuses holder, adapters, chat view and resume. No new process to keep alive |
| D2 | The read and action tools are agent kit tools for every agent | The owner asked that all agents see the catalog and suggest hosts. One set of tools, one scope check |
| D3 | Starts are confirmed now; proposals are reviewed at the end; hand-offs send at once | A start is time-bound; records batch well; a hand-off reaches an agent already at work (owner, 2026-11-01) |
| D4 | A hab-side confirmation card, not the harness's permission prompt | Works the same in every harness, cannot be pre-allowed by a harness setting, and carries the user's edits |
| D5 | The hab runs the confirmed start from the card's values | The agent cannot change the action after the user saw it |
| D6 | The confirmation id is the idempotency key for starts | Paseo idea 4.2: a reconnect never starts two sessions |
| D7 | Confirmations expire after 60 minutes; proposals never expire | Matches approvals. A stale action is risky; a stale record is only stale |
| D8 | Read tools are the hard boundary; harness deny rules are best effort | Matches Paseo and hc, plus a cheap guard. A real sandbox is stricter than the bar |
| D9 | A denial does not echo the matching pattern | The pattern may name a private path |
| D10 | Transcript search scans native files on each hab; no index of text | Principle: data stays native. A droppable cache may come later |
| D11 | Search limits: 50 hits, 5 s per hab, partial results marked | A slow or sleeping peer never stalls a conversation |
| D12 | Seeds are at most 8,000 characters and shown in full on the card | Long enough for a goal and decisions; short enough to read on a phone |
| D13 | One standing conversation, plus "new conversation" | Matches how a front door session is used; old ones stay resumable |
| D14 | No "off the record" mode | Nothing is written without review, so it adds nothing (design doc, deferred) |
| D15 | The hab does not write the harness's global trust file | Part 8 owns writes to user-level harness files |

---

## 11. Owner decisions

The owner settled the study's questions on 2026-11-01. Each row cites "owner, 2026-11-01".

| # | Question | Decision | Where |
|---|---|---|---|
| O1 | Where Capcom runs | The user assigns the home host and re-assigns it at any time, through `gff-hab capcom assign` and an app setting. First run suggests a host marked always on, else the first paired hab (owner, 2026-11-01) | 7.3 |
| O2 | Reading every peer | Capcom reads every connected host, within each host's own read scope. No separate list of peers for Capcom (owner, 2026-11-01) | 5.4 |
| O3 | "Ask an agent" from a notification | It continues the standing conversation with the event attached. A "New conversation" action is one tap away. The aim: no friction, and context kept where it matters (owner, 2026-11-01) | 8.2 |
| O4 | Confirming hand-offs | A hand-off to an agent already on a project needs no tap. Its card records what was sent (owner, 2026-11-01) | 3.2, 6 |
| O5 | Starting work on another host | A confirmation card, with a setting (`confirm.start_elsewhere`) to turn it off. General rule: a confirmation that adds friction gets a setting to turn it off (owner, 2026-11-01) | 6.1 |

---

## 12. Later stages

The Part 9 stages, with what this spec adds to each.

| Stage | Builds | Acceptance checks |
|---|---|---|
| Tools (task-m31qyr6v) | Read tools in the agent kit (4), the read scope (5), the action and proposal tools that create confirmations and proposals | Tests on fake catalog, index and Logbook; a write through a read tool is refused; a read outside scope is denied without the pattern; a search with an offline and a slow peer returns partial results marked |
| Session host (task-aqw4iowl) | The Capcom folder, manual and settings (7.1), kind `capcom`, standing and new conversations, home host assignment (7.3) | A `gff-hab` restart keeps Capcom running; a plain-terminal resume works from the folder; the manual and settings are rewritten at start; `gff-hab capcom assign` moves the mark, the old conversation stays resumable, and a host offline during the change drops its mark on its next link |
| Hand off and file (task-9n47i6l8) | 3.2 and 3.4, the hand-off card as a record | Hand-off to an idle fake agent delivers at once with no confirmation; to a working one queues; the card shows the exact text sent; a record lands as a proposal |
| Suggest and start (task-ph00sdci) | 3.3, ranking, seed, start by confirmation id, `confirm.start_elsewhere` | A host without a needed capability is not suggested and the reason names it; with the setting on, no session starts without a confirm; with it off, the start runs at once and its card shows as a record; a repeated confirm id starts one session |
| Approval (task-3387vxca) | Review (6.2), confirmations' expiry and first-answer-wins (6.1), CLI fallback | Three proposals, one approved: only it applies; closing without review leaves all pending; an expired confirmation never runs; a second answer gets `conflict` |
| Entry API (task-ilse43i7) | 8.3 calls and events, attached context blocks (8.1) | Open, send, attach and confirm against a stub Capcom; the schema compat test passes |
| "Ask an agent" (task-3687ti3z) | 8.2 | The action payload carries the notification id and the hab resolves it; it lands in the standing conversation with the event attached; "New conversation" moves the event and draft to a fresh one; no reachable hab gives a clear message |

Notes for those stages:

- The confirmation mechanism (6.1) is shared by start and set-up, for Capcom and for any agent
  that starts work on another host (the mesh spec). Build it once, with
  `confirm.start_elsewhere`, in the tools stage, and let later stages add card kinds. Hand-off
  cards use the same card shape as a record, with no buttons.
- The tools stage depends on the transcript index (Part 3) and Logbook (Part 12). Until Logbook
  exists, the Logbook tools answer `unavailable`.
- The set-up card belongs to the mesh's set-up stage; Capcom only calls it.

---

## 13. Sources

- Paseo, `docs/connected-hosts.md` on the main branch, read 2026-11-01.
- `landscape.md` 2.2, 2.13, 2.17, idea 4.2 and idea 4.14.
- `hab-logic-api.md` 2, 3.1, 3.3, 3.9; `harness-adapters.md`, Input delivery and Environment
  contract; `app.md` 3.6, 4.4, 7, 17.
- The project's design notes and plan for Part 9 (the folder-free conversation, its jobs and the
  approval rule). None of their private detail is reproduced here.

Facts marked *(gk)* come from general knowledge of the harnesses, not from a page opened for
this spec. The session host stage must confirm them.
