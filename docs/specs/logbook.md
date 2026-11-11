# Logbook: behaviour spec

Status: study for review. Part 12, study stage. Written 2026-11-01.

Logbook is Flightdeck's built-in memory. It holds findings, tasks, plans, guides, issues and
proposals. Any agent and the user can read and write it, on every host, offline, with no hub.
This spec says what a record is, how writes merge across hosts, how agents and people use it, and
how the later Part 12 stages are built and checked. It describes behaviour in our own words and
copies no code.

Rules this spec works within (project decisions):

- Logbook lives in `gff-hab`. No sidecar, no hub, no Docker service.
- Every hab keeps a full local replica: SQLite plus an op log. A write always lands locally first.
- Habs swap ops peer to peer over the hab-to-hab link. One machine alone is a mesh of one.
- Conflicts: per-field last writer wins by hybrid logical clock. The losing value is kept and
  flagged.
- Issue is a record type. There is no inbox type: untriaged tasks show on the status board.
- Memories are added without approval by default. A setting makes them wait for the user.
- An optional always-on host may add backups, a git export and vector search. Nothing needs one.
- The bar is herdr, Paseo and Rex (`landscape.md`). None of them ships a memory, so the bar here
  is the public agent memory tools in section 1. We match them and add only cheap guards.

Logbook builds on other parts. This spec names their pieces and does not define them:

| Piece | Defined in |
|---|---|
| Peers, the peer link, the catalog, project identity in the catalog, the outbox, the always-on mark | `mesh.md` |
| Device ids, trust, revocation | `transport.md` |
| API rules: JSON-RPC, revisions, capabilities, events, errors | `hab-logic-api.md` 3 |
| The hook endpoint, `GFF_HAB`, the pane token, the agent kit | `harness-adapters.md` |
| Capcom's proposals and review | `capcom.md` 3.4, 6.2 |
| Memory proposals from harness memory writes, the flight rules ledger | `instructions-mcp-flight-rules.md` 11, 12 |
| The status board, approvals, the app's views | `app.md` |
| Which tests we keep | `docs/testing.md` |

Sections that settle a question end with a **Decision** line and its reason. Items marked
**key call** are decisions with no ground truth, settled before the stage that builds them.
The owner decides only user-visible questions; those are in
[section 15](#15-open-questions-for-the-owner).

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [Projects](#3-projects)
4. [Storage](#4-storage)
5. [Merge rules](#5-merge-rules)
6. [Worked conflict examples](#6-worked-conflict-examples)
7. [Sync over the peer link](#7-sync-over-the-peer-link)
8. [One machine alone](#8-one-machine-alone)
9. [Approval](#9-approval)
10. [Surface: CLI, API and MCP](#10-surface-cli-api-and-mcp)
11. [Recording habits through the hook endpoint](#11-recording-habits-through-the-hook-endpoint)
12. [Other projects: what agents see and how they work together](#12-other-projects-what-agents-see-and-how-they-work-together)
13. [The optional always-on host](#13-the-optional-always-on-host)
14. [Edge cases](#14-edge-cases)
15. [Open questions for the owner](#15-open-questions-for-the-owner)
16. [Decisions](#16-decisions)
17. [Required property tests](#17-required-property-tests)
18. [Later stages](#18-later-stages)
19. [Sources](#19-sources)

---

## 1. References

### 1.1 The need: a record store that agents keep up

A common way to give many agents a memory is one record store that every session reads at start
and writes as it works. A study of one such long-running personal setup, used across dozens of
projects for over a year, gave these facts. They are ideas only; Flightdeck depends on no part of
that setup.

- **Findings dominate.** About 60% of records are findings (something learned), 31% tasks. A few
  hundred guides. Other types are barely used.
- **Findings are rarely edited.** When a finding goes stale, a new one supersedes it. The
  supersede chain is used hundreds of times.
- **An inbox type never drains.** Captured notes pile up in it. Untriaged tasks on a board work
  better.
- **Offline writes fail.** The store lives on one server. A laptop on a train cannot write. This
  was the top pain.
- **Hooks keep the habit.** A brief at session start, a reminder, and a closeout check keep agents
  recording. Without them, recording drops.
- **Agents use the CLI.** No harness had the store's MCP server registered. A short CLI with clear
  output is what agents call.
- **Ids are quoted outside the store,** in commits, docs and other records. An id must never
  change.
- **Write rate is low:** about 1,000 to 1,500 records a month across all projects.

### 1.2 Claude Code auto memory

From `instructions-mcp-flight-rules.md` 9:

- On by default. A `MEMORY.md` index plus one file per memory, per project, shared by every
  worktree of a repository.
- The first 200 lines or 25 KB of the index load at every session start; topic files are read on
  demand.
- The agent writes memory with its ordinary Write and Edit tools. No review.

### 1.3 Codex memories

- Off by default. A background job writes memories after sessions into a folder plus a SQLite
  store. No hook sees the write.

### 1.4 Beads

Beads is a public issue tracker built for coding agents (README, read 2026-11-01):

- Storage is Dolt, a versioned SQL database, embedded with one writer or as a server for several.
  Machines sync by pushing and pulling the database through a git remote. A JSONL file is an
  export, not the source of truth.
- Ids are short hashes (`bd-a1b2`), so two agents or branches never mint the same id. Epics have
  child ids (`bd-a3f8.1`).
- Links: blocks, related, parent-child, plus graph links such as supersedes and duplicates.
- Agent verbs: `ready` (tasks with no open blocker), a claim that sets assignee and status in one
  step, `prime` (prints workflow context and stored memories for injection at session start),
  `remember` (stores a project memory that `prime` injects later), and a setup command that
  installs hooks per harness.
- Old closed tasks can be summarised to save context ("memory decay").
- Merge is cell-level, through Dolt. No user-facing conflict procedure is described.

### 1.5 Op-based stores

*(gk)* marks facts from general knowledge, not from a page opened for this spec.

- **git-bug** *(gk)* stores each bug as a chain of operations in git objects, ordered by a Lamport
  clock per bug. Two clones merge by taking the union of operations and replaying them in clock
  order. Nothing needs a server.
- **Taskwarrior's sync** *(gk)* keeps a local replica and sends operations to a sync server, which
  orders them. A local write never waits for the network.
- **Hybrid logical clocks** *(gk)* (Kulkarni and others, 2014) pair wall time with a counter. They
  stay close to wall time, never run backwards, and order an event after every event its host has
  seen.
- **Version vectors and anti-entropy** *(gk)*: each replica keeps the highest sequence it holds per
  origin. Two replicas swap vectors and send what the other lacks. Any path between replicas is
  enough; loops are harmless because ops are deduplicated by id.

### 1.6 What we take

| From | We take | We change |
|---|---|---|
| The personal setup (1.1) | Finding, task, plan, guide; supersede for findings; a brief at start; hooks for the habit; a short CLI | Local-first writes; no inbox type; habits in the hab's hook endpoint for every harness |
| Claude auto memory | The brief loads at every start, capped in size | The brief is built from records, not a hand-kept index file |
| Beads | Random short ids; `ready`; a claim in one step; `prime`-style injection; setup per harness | Our own store and sync, no Dolt and no git remote; conflicts are visible |
| git-bug, Taskwarrior | Records as a fold of operations; local writes first | Peer exchange with no server |
| HLC, version vectors | Field order by HLC; sync by vectors | No general CRDT library: a few field kinds with fixed rules |

---

## 2. Model

### 2.1 Words

| Word | Meaning |
|---|---|
| record | One entry in Logbook: a finding, task, plan, guide, issue or proposal |
| op | One change to one record, made on one hab. Immutable once written |
| origin | The hab that made an op, by its device id (`transport.md` 5) |
| seq | An op's number within its origin, from 1 with no gaps |
| op id | `origin` plus `seq`. Unique everywhere |
| HLC | The op's hybrid logical clock stamp: wall time in ms, a counter, and the origin as tie-break |
| replica | One hab's copy: every op it holds, and the records folded from them |
| version vector | Per origin, the highest seq a replica holds with no gap below it |
| conflict | Two ops on one field where neither writer had seen the other's value |
| actor | Who asked for the change: a person (by client device id) or an agent (harness, session, host) |
| brief | The text a session gets at start: what the agent should know about its project now |
| untriaged | A task no person has looked at yet. Shown on the status board |

### 2.2 Record types

Every record has these fields:

| Field | Kind | Notes |
|---|---|---|
| `id` | Fixed at create | See 2.3 |
| `type` | Scalar | One of the types below |
| `project` | Scalar | A project id (3), or empty for a record that belongs to no project |
| `title` | Scalar | One line, at most 200 characters |
| `body` | Scalar | Markdown, at most 256 KiB |
| `tags` | Set | Short words |
| `links` | Set | Typed links to other records (2.4) |
| `notes` | Append log | Dated entries: progress, a decision, a comment |
| `deleted` | Scalar | true or false |
| `created`, `created_by` | Fixed at create | HLC, actor, origin |
| `source` | Fixed at create | Optional: the session, message and quote the record came from |

Type-specific fields:

| Type | Statuses | Extra fields | Editing rule |
|---|---|---|---|
| finding | none of its own; `superseded` is derived from links | none | Title and body are fixed at create. A change is a new finding that supersedes it |
| task | `open`, `doing`, `blocked`, `shelved`, `done`, `dropped` | `assignee` (session or person), `triage` (`new` or empty), `priority` (1 to 3, optional) | All fields editable |
| plan | `open`, `done`, `dropped` | ordered stages: `stage` links to tasks, each with a position | All fields editable |
| guide | `current`, `retired` | `pinned` (true puts it in every brief for its project) | All fields editable |
| issue | as task | `component` (free text: the tool, file or service that misbehaves) | All fields editable |
| proposal | `pending`, `approved`, `edited`, `rejected`, `expired` | `target` (`record`, `memory`, `flight_rules`), `draft` (the full content), `decided_by` | Only the decision fields change (9) |

- **Issue is a task with a component.** It keeps its own type so a board can show "what is broken"
  apart from "what is planned". Retyping between task and issue is a plain field change.
- **The memory proposal of `instructions-mcp-flight-rules.md` 12.3 is a proposal record** with
  target `memory`. Its fields map one to one: `text` is the draft, `quote` the source quote.
- **No profile, rule or state types.** Rules for agents belong to Part 8 (instructions and flight
  rules). A short-lived state note is a guide or a task note.

Decision: six types. Reason: the study in 1.1 shows four carry nearly all use; issue and proposal
are owner decisions and a Part 8 need.

### 2.3 Ids

- **Shape.** `<type>-<8 characters>` in lower-case base 32 without look-alike letters, for example
  `task-k3m9q2xz`. The prefix is the type at create time. 40 random bits per id.
- **Minted locally.** Any hab, and the CLI fallback (4.3), mints ids with no coordination.
- **Never changes.** Not on retype, not on project move, not on import. A task retyped to issue
  keeps `task-…`; the prefix is a hint, the `type` field is the truth.
- **Imported ids.** An import may carry ids verbatim when they match `[a-z]+-[a-z0-9]{3,16}`, so
  references written elsewhere still resolve.
- **Collision.** At 1,500 records a month, two equal random ids are not expected in the life of
  the store. If a create arrives for an id that already exists with a different create op, the
  create with the lower HLC keeps the id. The other record gets the id plus `-2` and is flagged
  "id collision". This is the one case where an id changes, and the user sees it.

Decision: random ids with a type prefix. Reason: Beads shows random short ids work for many agents
writing at once; the prefix keeps ids readable in commits and chat.

### 2.4 Links

| Link | From, to | Meaning |
|---|---|---|
| `supersedes` | finding to finding | The new finding replaces the old. The old shows `superseded` |
| `stage` | plan to task | The task is a stage of the plan, with a position |
| `part_of` | any to plan | Belongs to a plan, without a stage position |
| `blocks` | task or issue to task or issue | The target cannot start until this one is done |
| `relates` | any to any | Loose reference |
| `from` | any to any | Made because of the target (a hand-off, a proposal's result) |
| `duplicates` | any to any | Same as the target; the source is usually dropped |

- A link is stored on the source record and shown on both. `ready` (10.2) uses `blocks`.
- A link to an id this replica does not hold is kept and shown as "not here yet". It resolves when
  the op arrives.

### 2.5 Actors and provenance

- **Agents write through the pane token.** A write from inside a pane reaches the hab with the
  pane's `GFF_HAB` token, so the hab records harness, session id, pane and host. The agent cannot
  claim to be another session.
- **People write through a client.** The app and `gff` send the client's device id.
- **Source.** An agent write may carry a source: the session and message it came from, and a quote
  of the user's words. Capcom always sets it (`capcom.md` 3.4).
- **Origin is always a hab.** Clients never make ops of their own. A client's write is made into
  an op by the hab it talks to.

Decision: origins are habs only. Reason: a phone that made its own ops would need its own clock,
seq and storage, and it is not always reachable for sync. The hab it talks to is.

---

## 3. Projects

**Key call** (project identity). The client merge adds two items to it
(`mesh.md` 4.5): the fork workflow and identity frozen at creation.

### 3.1 Identity

A Logbook project is a record of its own kind (`proj-xxxxxxxx`) with a name and a set of
identities. Records point at the project id, never at an identity string.

| Source of identity | Form | Notes |
|---|---|---|
| Git remote | Normalised URL of the `origin` remote, else the first remote by name | Same rule as the catalog (`mesh.md` 5.1) |
| Root commit | `root:<hash>` of the first parent-less commit on the default branch | For a repository with no remote |
| Folder | `folder:<host device id>:<path>` | For a folder that is not a repository |

Normalising a remote: lower-case the host, drop the scheme, user, port, a trailing `.git` and a
trailing slash; turn `git@host:owner/repo` into `host/owner/repo`. So the SSH and HTTPS forms of
one repository match, and every clone and worktree of it on every host is one project.

- **First sight.** When a session starts in a folder whose identity matches no project, the hab
  makes a project with that identity and the repository's name. Two hosts that do this while
  apart make two projects with one identity; 5.6 merges them.
- **A remote changes** (a rename, a move to another forge): the catalog reports the new identity
  for a known folder. The hab adds it to the project's identity set. Old records stay with the
  project.
- **A fork** has a different remote, so it is a different project. The user can join two projects
  into one (`gff lb project join`), which adds every identity of one to the other and marks the
  emptied one as joined.
- **A monorepo** is one project. Sub-projects are tags, not projects, in the first build.

Decision: projects are records with an identity set, matched first by git remote. Reason: worktrees
and clones on every host land in one project; a remote rename does not orphan records; the catalog
uses the same identity, so Capcom and agents can go from a catalog project to its records.

### 3.2 Records with no project

A record may have no project: a general note, a household task, a finding about a tool used
everywhere. Capcom files these when nothing fits (`capcom.md` 3.4). They appear in `ls --general`,
in search, and in every brief only when tagged `everywhere`.

### 3.3 Built (stage 5)

The store side is in `internal/logbook` (`project.go`). First sight, remote changes and the
private rule are in `internal/lbsvc` (`projects.go`). gff-hab files projects in `logbookSighter`
and finds a pane's project in `logbookProject` (`cmd/gff-hab/logbook_open.go`). The verbs are in
`internal/lbcli`.

| Item | Built |
|---|---|
| Identity | `lbsvc.Identity`: the remote in the catalog's form (`structure.NormalizeRemote`, `mesh.md` 5.1), else `root:<hash>` of the root commit. The remote and the root commit come from the catalog's read of the folder (`catalog.ReadFolder`): `origin`, else the first remote. So the SSH and HTTPS forms of one repository, every clone and every worktree are one identity |
| First sight | The catalog hands gff-hab every folder it reads, after each scan and after each read of a new project folder (`catalog.Config.Scanned`). gff-hab files each folder a session works in (the structure's project folders) and each folder it filed before. A folder whose identity no project holds gets a new project with that identity. Its name is the last part of the remote, else the folder's name. A pane's call files its session's project at once when the catalog has not yet |
| The writer | A project filed at first sight, and an identity a remote change adds, are the hab's own writes: actor `{kind: hab, host}` |
| Folders | A `folders` table in `logbook.db` keeps each filed folder's project and the identity it had. It is this replica's own, like the idempotency keys: sync never sends it, and a rebuild keeps it |
| Remote change | A filed folder whose identity changed, to one no project holds, adds the new identity to the folder's project. Its records stay. The catalog sees the change at its next scan: at start, then every hour. A folder whose new identity another project holds is filed under that project, and nothing is joined |
| Join | `gff lb project join <a> <b>` adds every identity of b to a, and the identity `project:<b>` to both, so a project with no identity joins too. The fold then joins them (5.6): the older keeps its id, the other shows `joined_into`. Records of both list under either id |
| Rename | `gff lb project rename <p> <name>` sets the title |
| Private | The project field `private` (true or false), set by `gff lb project private <p> [on\|off]`. A project is private when it, or a project joined with it, is marked. `off` clears the mark on every project of the group. `logbook.projects` shows `private` (the group), `marked` (this project) and `joined_into`; `logbook.get` of a project shows the same |
| What agents see | A pane's reads leave out every private project except the pane's own, and their records: `find`, `list`, `ready` and `projects` skip them, and `get` and `history` answer `not_found`, as for a record this replica lacks. A pane's write that touches one, or files a record under one, answers `not_found` too. The pane's project is its session's project, by identity. A pane with no session or no project sees no private project. While gff-hab is down, the CLI in a pane reads the file under the same rule, with the working folder's identity as the pane's project. A person sees everything |
| Pane rules | An agent never files a project, never adds or removes an identity, and never sets or resolves `private`: each answers `invalid` with a hint that the user does it outside a pane. An agent may rename a project |
| API | Additive: a change's `identity` (add, remove); a record's and a project's `private`, `marked` and `joined_into`. No new method or event. The fold version is 4, so an older file is rebuilt at open. Version 5 adds the actor's `session_name`, which a build before it left out of folded rows |

Where this spec was silent, stage 5 decided. Each is a default the owner may revisit:

| Point | Decision |
|---|---|
| Cross-project reads (15.3) | Option A: agents read every project; a private project is hidden from agents outside it |
| Private is not a lock | Private hides; it does not protect. An agent runs as the user, so it can open `logbook.db` itself, unset `$GFF_HAB`, or read over `--socket`, which answers as a person. The rule keeps private records out of agents' ordinary reads and search |
| Which side of a join wins | The older project by create stamp, as 5.6 joins any shared identity, whichever order the user names them in. Reason: one rule for every join; a join the user makes and one sync makes agree |
| Private in a join | One mark makes the whole group private. Reason: joining must never show an agent what the user hid |
| Deleted projects | A deleted project still holds its identities and its mark, so a new first sight finds it rather than making a second project |
| Folder identities | Not built: a session outside a repository has no project in the structure, so Logbook files none for it (`folder:` of 3.1) |
| A different repository in a filed folder | Counts as a remote change when its identity is new: the new identity joins the old project. There is no split. A user who reuses a folder for another repository can rename the folder first |
| Peer-link reads | Unfiltered: no agent read goes over a peer link in this build. A read over one is a hab's |
| Not built | Unjoining. Stage 6 built the current project as the default view of `ls`, `ready` and the brief (12.1, 10.2), and the CLI's current project outside a pane (11.4) |

---

## 4. Storage

### 4.1 One package, one file per hab

- **Package.** `internal/logbook`, pure Go, no cgo (the repo's `check-pure` rule holds). It uses a
  pure-Go SQLite driver with FTS5.
- **File.** `logbook.db` in the hab state directory, WAL mode, owner-only permissions.
- **Tables.**
  - `ops`: the op log. Append-only, keyed by op id. Each op holds origin, seq, HLC, actor, record
    id, kind, field, value, the op id it saw for that field (`prev`), and a format version.
  - Folded tables: records, field values with the winning op, set members, notes, links, conflicts.
  - An FTS5 index over title, body, notes and tags.
  - `meta`: this hab's next seq, its last HLC, the version vector, the schema version.
- **The fold is a pure function of the ops.** `gff-hab logbook rebuild` drops the folded tables and
  replays every op. Two replicas with the same ops always fold to the same records.

### 4.2 Op kinds

| Kind | Effect |
|---|---|
| `create` | Makes a record with its id, type and initial fields |
| `set` | Sets one scalar field; carries `prev`, the op id of the value the writer saw |
| `add`, `remove` | Adds to or removes from a set field (tags, links, project identities); a remove names the adds it saw |
| `append` | Adds one entry to a record's notes |
| `forget` | Redacts the value of a named op on every replica (5.7) |

A change the user makes in one step (a new task with tags and a link) is one batch of ops with one
HLC, written in one transaction. A batch is applied all or nothing on every replica.

### 4.3 Writers

- **gff-hab** writes for the API, the MCP server and the hook endpoint.
- **The CLI fallback.** When gff-hab is down or restarting, the CLI opens `logbook.db` itself and
  writes with the same package. So memory keeps working through a hab restart or update. Inside a
  pane, only reads use the file; a write waits for gff-hab (10.5).
- **Two writers, one file.** A write takes SQLite's write lock (`BEGIN IMMEDIATE`), reads the next
  seq and the last HLC from `meta`, writes its batch and moves both on, then commits. A busy file
  waits up to 5 s. gff-hab notices ops it did not write (a WAL change) and emits events for them.
- **Nothing else writes the file.** A peer's ops arrive through gff-hab's sync.

Decision: the CLI opens the file directly when the hab is down. Reason: memory must work while
gff-hab restarts; SQLite's lock makes two writers on one host safe.

### 4.4 Format changes

- Every op carries a format version. A hab stores an op of a newer version, or an unknown kind or
  field, as it came, and forwards it in sync. It does not fold it. After an update the rebuild
  folds it.
- A change to the folded tables needs no migration: the new build rebuilds them from the ops.

Decision: the op log is the only durable truth. Reason: folding again is cheaper than migrations,
and it lets an older hab carry a newer hab's ops without losing them.

### 4.5 Built (stage 2)

`internal/logbook`. The driver is `modernc.org/sqlite`, pure Go, with FTS5 built
in. The file opens with a 5 s busy timeout, WAL, `synchronous=FULL`, `secure_delete` and `BEGIN
IMMEDIATE` for every write. FULL, because in WAL mode NORMAL can lose a commit after a power loss,
and the next write would then reuse a seq that peers already hold. `secure_delete` zeroes freed
space, so a forgotten value leaves no copy in the file (Forget below).

Where this spec was silent, stage 2 decided:

| Point | Decision |
|---|---|
| Op envelope | Every op carries `v`, `o` (origin), `s` (seq), `w` and `c` (the stamp), `bl`, `k` (kind), `r` (record id) and `root`. These keys never change between versions, so an older build can order, apply and forward any op |
| `root` | Every op names its record's create op. Two creates of one id (2.3) stay apart, and later ops reach the right record. The second record shows as `<id>-2`, then `-3`, in create stamp order |
| Claimed ids | A create, local or from a peer, claims an id of 2.3's shape, which has one dash. A collision id has two, so no create can claim an id a fold gives another record, and folded ids stay unique |
| Refused ops | `Apply` refuses an op, stores nothing for it and counts it in `Refused` when: its envelope is unusable; a seq it carries or names (its own, its batch end, its root, its target, a `prev`) is above 2^63-1, which SQLite cannot store; a create claims an id of another shape; its origin is shared; it is this replica's own origin past a gap (Own ops back); or the file's constraints refuse it, undone by a savepoint. The rest of the page lands. A refused op is not remembered, so a peer may send it again |
| Batches | Every op carries `bl`, the last seq of its batch. A replica folds an origin's ops up to the end of the last whole batch in its gap-free run. The version vector counts the gap-free run |
| `prev` | A local `set` names every head of the field: the set ops no other set op on it saw. With no flag that is one op; where a conflict shows, it is every side |
| Conflict flag | A field is flagged when it has two or more heads. So a set written where the flag shows clears it (5.2: resolve, or a later set that saw the winner), and a set written apart, that saw one side only, keeps it. P6 tests this rule. The create's value counts as the field's first set |
| Delete | Local writes to a deleted record are refused, except a restore (`deleted` false) and `forget`. "Edited after delete" flags the record when `deleted` is true and another folded op on it has a later stamp than the winning delete |
| Proposals | After create, only `status`, `decided_by` and `deleted` change. Decisions are `approved`, `edited`, `rejected` and `expired`. `decided_by` follows the batch of the decision that stands. These two fields raise "decided twice", never a last-writer conflict. A writer that sees a decision cannot write `status` or `decided_by` again (`invalid`), so only decisions made apart raise "decided twice" |
| Findings | `title`, `body` and `type` are fixed. An incoming set of them is kept, not folded, and logged once when it is applied |
| Retype | Only a task and an issue change type, into each other |
| Superseded twice | Derived when the record is read, from the `supersedes` links of records not deleted. Deleting one successor clears it |
| Projects | Type `project`, ids `proj-<8>`, the title is its name, set field `identities`. On fold, projects that share an identity, directly or through others, join: the oldest by create stamp is canonical. `Record.Project` is the canonical id, and a list by either id finds both. Stage 5 adds identity detection, first sight, `project join` and the private flag (3.3) |
| Forget | Targets a create (its text fields and source), a set of a text field, an append or an add. The value becomes `forgotten sha256:<hex of its JSON>`. Redaction is idempotent; an op that a held forget names is redacted before it is stored; the FTS index is optimised after a redaction. A repeat compares after redaction. A forget from a peer that names any other op (a set of a field with no text, a remove, a forget) changes nothing; an op of a kind this build does not know is redacted whole. Each forget of a record's op moves the record's revision. After a forget lands, a `TRUNCATE` checkpoint empties the WAL, whose older frames still hold the text; with `secure_delete` the file holds no copy either. A reader that holds the WAL open makes the checkpoint stop short, and the store logs it |
| Shared origin | A second, different op with a held op id marks its origin shared; its further ops are refused. `Status.Shared` lists it. When the origin is this replica's own, `Write` refuses with `ErrSharedOrigin` until the host gets a new device id |
| Clock ahead | An op stamped over 1 hour past the receiver's clock marks its origin, logs once, and shows in `Status.Ahead` |
| Counter top | A local stamp whose counter would pass 2^32-1 carries into the wall: wall + 1, counter 0. So a write still orders after a peer's op stamped with the top counter (5.1) |
| Own ops back | This replica's own ops come back from a peer only after its file lost them (a restore, 14). One that extends the gap-free run of the origin is stored, and the next seq moves past it, so no seq is used twice. One past a gap is refused: stored, it would hold a seq a later local write needs, and local writes would stop folding behind the gap. The peer sends it again once the ops before it are here |
| Limits | Title one line of 1 to 200 characters; body and note at most 256 KiB; a tag one word of at most 40 characters; other text fields at most 4 KiB; priority 0 (none) to 3; triage `new` or empty; an identity one line of at most 1 KiB |
| Property test counts | Stage 2 has no P1, so P2 runs after each of its own random histories: 3 to 5 replicas writing every op kind and exchanging pages that drop ops. The histories also hold creates of one id on two replicas, project fields, ops of a later build, stale unredacted copies of ops that may land after their forget, forged ops under a replica's own origin past a gap, and a replica that loses its file and gets its ops back. P2 compares the folded tables and the stored op bytes. Per push: P2 100 runs, P3 50 runs of 3 orders, P6 500 histories, P10 2 processes of 2 goroutines with 100 writes each. P2 runs 100 times, not 500, to keep the package under 15 s: a run's cost is mostly SQLite's file setup, and the runs go as parallel subtests. Nightly (`GFF_LOGBOOK_NIGHTLY=1`): 20,000, 5,000, 20,000 and 2,000. Under `-race` the run counts drop to a twentieth: the race build checks for data races on the same paths at a tenth of the speed, and `make test` runs the full counts |
| Fold version | The file records the fold version. A build with another opens it with a rebuild, which also re-checks which ops it can fold (4.4) |

Interfaces for the next stages:

- **Stage 3 (sync).** `Vector()` returns the gap-free seq per origin. `OpsAfter(have, maxOps,
  maxBytes)` returns the stored bytes of the ops `have` lacks, per origin in seq order, and whether
  more remain: a pull's answer and a push's source. `Apply(ops)` stores ops as their bytes came,
  folds what it can, and returns the counts stored, repeated and refused and the changed record
  ids, sorted: the ids before and after, so a record an id collision renames shows under both.
- **Stage 4 (CLI, API, MCP).** `Open(path, Options{Origin: ""})` takes the origin from the file,
  so the CLI writes while gff-hab is down. `Write(Batch{Actor, Steps})` returns op ids and record
  ids; a refusal is an `*InvalidError` with a hint (the API's `invalid`), a missing record
  `ErrNotFound`. Reads: `Get`, `History`, `List`, `Find`, `Status`. `Changes(cursor)` returns the
  records whose ops arrived after a cursor, by any writer of the file, so gff-hab can emit events
  for the CLI's writes; a create also names every record that claims its id. Idempotency keys belong in stage 4, in a table of the same file, written
  in the write's transaction.

---

## 5. Merge rules

### 5.1 The clock

- Each op gets an HLC stamp `(wall ms, counter, origin)`. Order is wall, then counter, then origin
  device id as bytes.
- On a local write: `wall = max(now, last wall)`; the counter rises when wall did not move, else
  resets to 0.
- On receiving ops: the hab's last HLC moves up to the highest one received, so its next write
  orders after everything it has seen.
- **A clock far ahead.** An op stamped more than 1 hour ahead of the receiver's wall clock is still
  accepted (refusing it would stop convergence). The receiver shows "clock ahead on <host>" once
  per peer. The HLC keeps every later write ordered correctly; only the shown times are off.

### 5.2 Scalar fields

Title, body, status, project, type, assignee, triage, priority, component, pinned, deleted, and a
proposal's decision fields.

- **The op with the highest HLC wins.**
- **A conflict** is two `set` ops on one field where neither op's `prev` is the other op, or a
  successor of it. That is: the second writer had not seen the first value. The record is flagged.
  The flag lists both values, both actors and both hosts.
- **The loser is kept** in the op log. The record's history shows it, and `resolve` can pick it.
- **Resolving** is a normal `set` whose `prev` names both ops. It clears the flag on every
  replica.
- A later `set` that saw the winning value clears the flag too: the user moved on.

### 5.3 Body

The body is a scalar: the whole body wins or loses. No text merge.

Decision: whole-body last writer wins, loser kept and flagged. Reason: a text merge is a large,
error-prone piece of code; the owner chose per-field last writer wins; notes (5.4) take most
concurrent writing away from the body.

### 5.4 Notes

- `append` entries are never in conflict. Every replica shows the union, in HLC order.
- Agents are taught to append progress to `notes`, not to rewrite the body. That keeps the body for
  the durable summary and keeps conflicts rare.

### 5.5 Sets: tags, links, project identities

- **Add wins.** A `remove` removes only the adds it names. An add made concurrently with a remove
  survives. Two adds of the same member are one member.
- Set changes never raise a conflict flag.

### 5.6 Rules for record types

- **Findings.** A local `set` on a finding's title or body is refused with `invalid`. An incoming
  one (from a faulty peer) is kept in the log, not folded, and logged once. Tags, links, notes,
  project and `deleted` may change.
- **Supersede** is a `supersedes` link on the new finding. The old one shows `superseded` because
  the link exists; there is no status field to fight over. Two findings that supersede one finding
  while apart both stand. The old one shows two successors and is flagged "superseded twice".
- **Proposal decisions are first writer wins,** not last: the decision with the *lowest* HLC
  stands, as approvals do in `app.md` 7.5. The later decision is flagged and shown as "decided
  twice". See 9.3 for what this does to an applied proposal.
- **Projects with one identity.** Two projects that share an identity are joined on fold: the one
  with the lower create HLC keeps its id, and records of the other show under it. The joined
  project keeps its id as an alias, so links and filters by either id work.
- **Delete** is `deleted = true`, a scalar like the others. A record that is deleted while another
  host edits it stays deleted; the edit is applied but hidden, and the record is flagged "edited
  after delete" so the user can restore it.

### 5.7 Forget

Ops are never removed in the normal course. One case needs it: a secret or private text written by
mistake.

- `forget <op id>` writes a `forget` op. Every replica that holds or later receives the named op
  replaces its value with a hash and the words "forgotten". The op id, actor and HLC stay, so sync
  and the fold still work.
- A forget also clears the FTS index for that text and any export made after it (13).
- Backups and git exports made before the forget still hold the text. The CLI says so.

Decision: a replicated redaction op. Reason: an append-only, replicated log otherwise keeps a
leaked secret on every host forever; the cost is one op kind.

---

## 6. Worked conflict examples

Hosts A and B are two habs. "Apart" means no link between them while the writes happen. Stamps are
written `wall.counter@origin`, with minutes for wall.

**E1. Status on both hosts.** Task T is `open`, set by op `o1`. Apart: on A at 10:00 an agent sets
`doing` (`prev o1`); on B at 10:05 the user sets `done` (`prev o1`). They link at 11:00. B's stamp
is higher, so T is `done` on both. Neither op saw the other: T is flagged. The flag shows "doing on
A, by claude in session s1; done on B, by phone". The user can leave it or resolve.

**E2. Different fields.** Apart: A changes T's title; B changes T's status. No conflict. Both
changes stand on both hosts.

**E3. Two body edits.** Guide G. Apart: A rewrites the body at 09:00; B rewrites it at 09:30. B's
body wins; A's is kept as a revision; G is flagged. Notes appended on either side both show.

**E4. Tag add and remove.** T has tag `ui` from op `a1`. Apart: A removes `ui` (naming `a1`); B adds
`ui` again (op `a2`). After the link, `ui` is present: the remove named only `a1`, and `a2` is a new
add. No flag.

**E5. Supersede twice.** Finding F. Apart: A writes F2 that supersedes F; B writes F3 that
supersedes F. After the link, F shows `superseded` with two successors and is flagged "superseded
twice". F2 and F3 both stand. The user drops one or links them.

**E6. Delete and edit.** Apart: A deletes T at 10:00; B appends a note and sets `doing` at 10:10.
After the link, `deleted` is true (only A set it). B's changes are folded but hidden. T is flagged
"edited after delete". Restoring T shows B's note and status.

**E7. A clock ahead.** B's wall clock runs 10 minutes fast. B sets T's title at what B thinks is
10:10 (really 10:00). A sets it at 10:05 real time, after it has seen B's op. A's HLC is already
past B's stamp (5.1), so A's op orders after B's and wins. Its `prev` names B's op, so there is no
conflict. Only the shown time of B's op is off by 10 minutes.

**E8. A clock far ahead.** B's clock is a day fast. B's op arrives at A. A accepts it and shows
"clock ahead on B" once. A's next writes still order after it (5.1). Nothing is lost.

**E9. Same identity, two projects.** Apart: a session on A starts in a fresh clone of repository R
and makes project P1; a session on B does the same and makes P2. After the link, P1 and P2 share
the identity. P1 has the lower create stamp, so P1 keeps its id, P2 becomes an alias, and records
filed under either show under P1.

**E10. Proposal decided twice.** Proposal X is pending. Apart: the user approves X on the phone
through A at 10:00; on the Mac through B, rejects it at 10:02. After the link, the approve (lower
stamp) stands, and the reject is flagged "decided twice". The record A made from X stays.

**E11. An unknown field.** A runs a newer build and sets T's new field `estimate`. B runs the older
build. B stores and forwards the op but does not fold it. When B updates, the rebuild folds it.

---

## 7. Sync over the peer link

### 7.1 Exchange

- **Who drives.** The hab that holds the link drives both directions, as for the catalog
  (`mesh.md` 4.2).
- **Pull.** `logbook.pull {have}` sends the caller's version vector. The callee answers with the ops
  the caller lacks, in per-origin seq order, at most 2,000 ops or 1 MiB per page, plus its own
  vector and whether more pages follow.
- **Push.** The caller compares vectors and sends what the callee lacks with `logbook.push {ops}`.
  The callee answers with its new vector.
- **Wake-ups.** When a hab writes, it sends `mesh.changed` with the `logbook` reason over every live
  link. A peer that missed it catches up within the 60 s poll period (`mesh.md` 4.2).
- **Apply order.** A replica applies ops of one origin in seq order, all of a batch at once. A gap
  waits for the missing ops; the version vector counts only the contiguous run.

### 7.2 Ops travel more than one hop

Catalogs travel one hop (`mesh.md` 5.2). Logbook ops do not: a hab serves every op it holds, from
every origin. Two laptops that never link to each other converge through any hab both reach.

Decision: multi-hop exchange by version vector. Reason: every replica must hold every op; ops are
immutable and keyed by op id, so loops and repeats are harmless, and no age or loop rules are
needed.

### 7.3 No outbox items

The op log already holds every op, and the version vector says what each peer lacks. So Logbook
puts nothing in the mesh outbox (`mesh.md` 6.3). A peer that sleeps for a month gets everything on
its next link.

### 7.4 Peers that change

| Case | Behaviour |
|---|---|
| A new hab joins | Its first link pulls every op, page by page. Reads and writes work locally from the first second; search shows "syncing, N of M" until done |
| A hab is rebuilt with a new key | It is a new origin. Its old origin's ops come back to it from any peer. Nothing is lost that any peer held |
| A hab's disk dies before it synced | Its unsynced ops are lost. This is the one way to lose a write. `gff lb status` shows unsynced op counts per peer so the user can see the risk |
| A peer is removed from the peer list | Its ops already received stay: they are the user's data |
| A device is revoked | Clients make no ops, so nothing changes in Logbook. A revoked hab's ops stay; new ones stop arriving because the link is refused |
| Two habs share one device id (a cloned VM) | Their seqs collide. The mesh refuses the second link (`mesh.md` 10). A replica that receives two different ops with one op id keeps the first, refuses the origin's further ops and shows "two hosts share a device id" |

### 7.5 Clients and offline clients

- Clients write through `logbook.write` on any hab they reach. Each write carries an idempotency
  key (`landscape.md` idea 4.2). The hab keeps keys for 7 days and answers a repeat with the first
  result.
- The app keeps a read cache of the records it showed and queues writes made offline, with their
  keys. It sends them when a hab answers. A queued write shows as "waiting to send".
- `gff` and the app may write through any hab. The record reaches every other hab by sync.

### 7.6 Built (stage 3)

`internal/logbook` (`sync.go`: `Store.Sync`, `ServePull`, `ServePush`) and `internal/lbsync`,
wired into `gff-hab serve`. Capability `logbook`. Methods `logbook.pull` and
`logbook.push`; `mesh.changed` gains the reason `logbook`. Until stage 4, `logbook` names the
peer methods only; the client methods of 10.3 come with stage 4.

Where this spec was silent, stage 3 decided:

| Point | Decision |
|---|---|
| The file | gff-hab opens `<state>/logbook.db` at start, with its device id as the origin. A corrupt file (SQLite says it is not a database, or damaged) is renamed `logbook.db.corrupt-<unix seconds>`, with its WAL, and gff-hab starts empty and logs it; the peers send a full copy (14). A file of another origin (the host got a new device id and kept its state folder) is renamed `logbook.db.other-origin-<unix seconds>`, and its ops are copied into the new replica as the old origin's ops, so nothing it held is lost. Neither file is deleted |
| Setting aside | The WAL moves first, then the shared-memory file, then the file, with the folder synced after each rename. SQLite does not check that a WAL belongs to its file: moved first, the file would leave its WAL beside the new replica, which would replay the old pages. A crash after the WAL moved leaves the file at `logbook.db` and its WAL under the aside name; the next start sees a WAL with no file of its name and moves the file there before anything opens it |
| Copying an older origin | At every start, gff-hab copies the ops of each `logbook.db.other-origin-*` file that has no `.copied` marker beside it, page by page; when a copy completes it writes the marker. A crash before that, or a copy that fails, means the next start copies again: ops are keyed by op id, so a repeat changes nothing. A failure is logged and listed in `Status.Uncopied` (as `<file>: <why>`) until a copy completes. An origin whose next op the new replica refuses is passed over, so the others still copy; the error names it |
| Peer links only | Both methods need a peer link (client kind `hab` with the hello's `peer`) and refuse one that claims this hab's own device id, with `invalid`. A client gets the same refusal |
| Wire shape | `logbook.pull {have, skip, max_ops, max_bytes}` answers `{ops, vector, more, skip}`; `logbook.push {ops}` answers `{vector, skip}`. A vector is a list of `{origin, seq}`. Ops travel as base64 of their stored bytes, not as JSON values: a JSON encoder may compact or escape a value, and a replica compares op bytes (4.5, Shared origin), so an op must arrive byte for byte |
| Pages | 2,000 ops or 1 MiB, one larger op alone (7.1). A pull may ask for less with `max_ops` and `max_bytes`; more is never sent, except one redacted op's group in an otherwise empty page (Redacted ops). A push of more is refused with `invalid` and stores nothing. 1 MiB of ops is about 1.4 MiB of base64: one page moves in 60 s on a link down to about 0.2 Mbit/s, far under the control connection's 8 MiB backlog |
| Slow links | One pull or push call may take 60 s. A call slower than 15 s halves the link's page size, down to 16 KiB; one faster than about 4 s doubles it, up to 1 MiB. Past 60 s the exchange gives up on the call, halves the page and fails, with the usual backoff. The call itself goes on: the hab at the other end writes every answer it makes, so the link's next call waits until that answer has come or the link has ended. So at most one answer is on the way per link, and a retry never stacks a second page in the other hab's backlog, which would close the whole peer link. A push's request is written by the dialer under the control client's 30 s write limit; a page too slow for it ends the connection once, and the next page is half the size |
| One exchange | `Store.Sync`: pull page after page until the answer says no more, then push, page after page, what the peer's last answered vector lacks. A page that moves no vector and passes over no new origin ends that direction, logged; so does the 1,000th page, and the next exchange carries on. Pull before push, so a push never sends ops the peer is about to send back |
| Redacted ops | A redacted op never travels without a forget that names it. When the caller holds no forget of it by its vector, a page carries the op with one forget this replica holds and the ops of the forget's origin from where the caller leaves off up to it, so every origin's run stays gap-free. Of several forgets, the one with the shortest run goes. An op in that run that is itself redacted brings its own forget the same way. The group goes whole into one page. An empty page takes it even past the limits the caller asked for, never past 2,000 ops or 1 MiB; on a link shrunk to 16 KiB such a page takes longer, within the 60 s call limit down to about 0.2 Mbit/s. A group that does not fit stops its origin's run for that page, and the other origins go on, the forget's among them, so the group shrinks on later pages. Without the group, a replica that lacked the op could store the redacted bytes while the forget was cut off by the page bound, a lost answer or a dead disk; a replica that already held the text is never sent the op again, so it would keep the text for good (P1, P9). The op travels alone only where no group can: every forget of it lies past a gap here, or no origin of the page can move. That happens when the forget lies more than a page ahead on the op's own origin, or two origins' forgets need each other across more than a page. If the forget's writer then dies, no replica will ever send the forget, and a replica that held the text keeps it. So a replica that stores a redacted op while it holds no forget of it records the op as an orphan, local to the file and found again from the log when the file is first opened. A stored forget of the op clears it. An orphan that outlives its chance gets a forget written by the replica itself, with the actor kind `hab`, and `gff lb history` shows it as the hab repairing a forget that never arrived. The chance is 10 minutes, ten times the 60 s call limit, so a forget on a later page of this link or another arrives first: gff-hab's sync round (every 60 s) repairs older orphans, also on a hab that only answers links. `Sync` repairs nothing, since another link may still be bringing the forget. A repair that comes too early only redacts what is already redacted, and history shows it beside the forget that came later. Repairs run one at a time, and each first checks that its orphan is still there. Answering a pull or a push repairs nothing, so a repeated request still changes nothing (P5). The forget travels like any op and redacts the text holder's copy, so every replica ends on the redacted form and no text travels. No repair runs during the start hold; a failed write is logged and tried again. Only bytes a redaction made count as redacted: a redaction writes the op's keys sorted, which an op as written never has, so a value that only looks like a forgotten one is never taken for one. An op a forget leaves alone, such as a remove, is never an orphan. An orphan whose op this build cannot forget (a kind or field it does not know) is dropped. A forget in an origin the caller skips counts as held, since the caller would refuse it; a passed-over origin comes back at the next exchange |
| Refused origins | Each side lists in `skip` the origins it refuses (two hosts share them, 7.4), and the other side sends none of their ops. Without it, a refused origin's ops would fill every page and starve the origins after it. An origin whose next op the receiving side refuses for another reason (its constraints, an envelope a newer build wrote) moves no vector, and every later page would carry the same ops again. So when a page moves no seq of an origin it carried, the exchange passes over that origin until it ends: it joins `skip` on pulls and is left out of pushes. The next exchange tries it again |
| When the dialer syncs | When the link comes up, every 60 s (7.1), at once when this replica's vector grows, and at once on `mesh.changed` with `logbook` from that link, past any backoff. One exchange per link at a time: a request during one runs it once more after. A failed exchange waits 1 s, doubled for each failure in a row up to 60 s; only the first failure in a row is logged. A wake-up during an exchange that then fails, or during the wait, runs the next exchange at once; a wake-up an exchange already served does not cut a later wait short |
| Seeing new ops | gff-hab acts when the vector grows: after a pull or push, after `Changed()`, and after another connection commits. Every 500 ms, a pinned read-only SQLite connection checks `PRAGMA data_version`. This catches CLI commits in WAL mode even when file sizes and timestamps stay unchanged. An initial vector check covers commits during watcher startup. A watcher error falls back to checking the vector each tick. |
| Wake-ups | A hab that dialed this one and pulled from it at least once is woken with `mesh.changed` (`reasons: ["logbook"]`) when this replica holds ops it lacks: what its last pull named, plus the page that pull was served, plus every op it has pushed since. So its own pushes never wake it back. Once until its next pull, as for mail (`mesh.md` 6.5). None when this hab holds an online link to it that is not in backoff: this hab pushes instead. A wake-up no link took is tried again at the next change |
| Multi-hop | Ops stored from a peer count as growth, so they go on to every other link at once, by push or by wake-up (7.2) |
| A new key | A hab rebuilt with a new key is a new origin with an empty file. Its old origin's ops are, to it, another origin's ops: it pulls them from any peer in seq order, folds and serves them like any others. The own-origin rule of 4.5 (Own ops back) applies only to a replica's current origin, so it never meets the old one; and the old origin never writes again, since its key is gone, so no seq of it is used twice. Ops the old disk held that no peer held are lost (7.4) |
| No outbox | Logbook puts nothing in the mesh outbox (7.3); the op log and the vectors are the outbox |
| Restored file | A file restored from an older copy with the same device id holds fewer of its own ops than the peers. A write would reuse a seq a peer holds under another op; the peers would mark the origin shared, and replicas would keep whichever op came first, apart for good. Two guards, both local and cheap. First, every pull answer, push answer, incoming pull's `have` and incoming push of own ops names how far the peer holds this replica's origin; the file keeps the highest (meta `own_seen`). While it is above the replica's own gap-free run, `Write` refuses with `ErrRestoring`, `Status.Restoring` names the seq, and the log says `RESTORED FILE` once per rise. The pull brings the own ops back in seq order (4.5, Own ops back), and writes go on with the next seq. Second, the start hold: when the peer list names a hab other than this one, gff-hab holds writes to the file for up to 10 s after it starts (meta `hold_until`, a lease every writer of the file honours, the CLI too), and lifts it when the first exchange over any link ends. A write in the hold waits, then goes on. A mesh of one never waits; a hab whose peers are all offline waits at most the 10 s once |
| What the guards leave | A restored file can still reuse a seq when it writes before any peer answered: its peers are all offline for longer than the hold, or the first link up is to a peer that never held the lost ops while one that did is away. That hab then refuses its own origin's later ops from the others, and they refuse its new ones: "two hosts share a device id" (7.4). Recovery is a new device id. The guard never blocks a mesh of one or offline peers for good: it waits only on a seq a peer said it holds. If that peer never comes back, writes stay refused, with the error saying to give the host a new device id |

Not built in stage 3:

- "syncing, N of M" in search, and `gff lb status` unsynced counts per peer (7.4): stage 4.
- The nightly soak of 1 million ops over 5 replicas (17).

Property test counts: P1 to P9 run in one process over `Store.Sync`, with a fake link that drops a
request, holds one and delivers it after others, repeats one (at once, checked to change nothing,
or later), or serves one and loses the answer. In half the runs each link asks for pages of 3 ops
or 2 KiB, so exchanges page, cut an origin's run mid-page and pass the byte bound. A replica restart or rebuild kills the requests in
flight to or from it. Per push: P1 40 runs, P4 30, P5, P7, P8 and P9 20 each, of 60 steps; nightly
(`GFF_LOGBOOK_NIGHTLY=1`) P1 20,000 and the rest 5,000; under `-race` a twentieth. Fewer than 17's
500 for P1, for the same reason as P2 in 4.5: the package stays near 15 s. P1 also checks P2 on
every tenth run. P4 rebuilds replicas with a new key; it excuses only ops that no other replica
held when the disk died. P8 sets clocks up to a day apart with drift and, after each sync that
stored ops, checks that a new write stamps after every op the replica holds.

---

## 8. One machine alone

- The same package, file and API. The peer list is empty, so sync has nothing to do.
- No network listener, no extra process. Core test 8 (`docs/testing.md`: no internet socket on a
  single machine) still holds.
- Adding a peer later starts sync with no migration: the replica already has an origin, seqs and
  HLCs.

Decision: no single-node mode. Reason: the owner ruled out duplicated code; a mesh of one is the
general case with no peers.

---

## 9. Approval

### 9.1 The setting

| `logbook.approval` | Agent writes | The user's own writes |
|---|---|---|
| `off` (default) | Land at once | Land at once |
| `agents` | Become proposals | Land at once |

- It is a hab-side setting (`app.md` 20.5): the app writes the same value to every hab, and a
  value in the hab's config file locks it. `gff lb status` shows a hab whose value differs.
- Under `agents`, an agent's write returns `proposed` and a proposal id. The agent is told the
  record waits for the user's approval and should not ask again.
- `logbook.approval` also covers harness memory: under `agents`, each harness's native memory
  writes become proposals too (`instructions-mcp-flight-rules.md` 12). They are stored as
  proposal records here. One switch replaces the per-harness switches (`app.md` 20.3).
- It is a levelled setting: `agents` at Safe, `off` at Balanced and Bypass permissions
  (`app.md` 20.3). The app writes it through `settings.set` (`app.md` 20.5).
- Built: gff-hab stores and serves the setting, and the app writes it. Stage 7
  reads it at each write on a pane's route (9.4). Harness memory is not covered yet: the Part 8
  hook that files memory proposals is not built.
- Capcom files proposals by its own rule (`capcom.md` 6.2), whatever this setting says. See open
  question 15.1.

### 9.2 Proposals

- A proposal record holds the full draft (the ops it would make), the source quote, the actor, and
  where it came from.
- Proposals show in the app's approvals area and with `gff lb proposals`. Each can be approved,
  edited and approved, or rejected. Pending proposals never expire on their own (`capcom.md` D7),
  except memory proposals, which keep their 30-day expiry.
- Approving applies the draft as the agent's write, with `approved_by` set to the deciding device.
  The new record links to the proposal with `from`.

### 9.3 A decision that loses

If two hosts decide one proposal while apart, the first decision stands (5.6). When a reject wins
over an approve that already made a record, that record is hidden like a deleted one and flagged
"approval overruled". When an approve wins over a reject, the record appears when the approving
host's ops arrive. Nothing is applied twice: the record's create is part of the approve batch.
A proposal's `status` and `decided_by` are never overruled: only the records and changes of an
approve that lost are (9.4, Fold order).

### 9.4 Built (stage 7)

Task-y7ne09xq. The fold is in `internal/logbook` (`refold` and `overruled` in `apply.go`, `fold`
in `fold.go`); `logbook.decide` is `Decide` in `internal/lbsvc`; the setting is read in
`logbookHook` (`cmd/gff-hab/logbook_open.go`).

| Item | Built |
|---|---|
| Setting | Under `agents`, every write on a pane's route is a proposal, as Capcom's always are. A write on the control connection, and the CLI outside a pane, land at once. gff-hab reads the setting at each pane call, so a change holds from the next write. Under `agents` a pane's `logbook.resolve` is refused, as Capcom's is |
| Answer | A proposed write's answer carries `note`: nothing landed, the change waits for the user's approval as proposal `<id>`, the user sees it with their other approvals, do not ask again. The CLI prints it; `logbook_write` says the same |
| `logbook.decide` | `{id, decision: approve\|reject, edits?, idempotency_key}`. Approve applies the draft's changes, or the edits in their place, in one batch with the decision; with edits the status is `edited`. Each create gets a `from` link to the proposal. Reject sets the status and `decided_by` only. The answer is `logbook.write`'s, with state `applied` or `rejected` and `proposal` set |
| Actor | An approve batch's actor is the proposing agent (the proposal's `created_by`) with `approved_by` set to the deciding device; the API's `LogbookActor` gains `approved_by`. `decided_by` is the same device. A reject's actor is the person |
| Who decides | Only a person. The pane route answers `logbook.decide` with `invalid` and names who can decide; the service refuses an agent actor and a pane too. Over a peer link it is a write, so `invalid` (10.5) |
| Only a decision decides | A plain write or resolve that sets a proposal's `status` or `decided_by` is refused with `invalid`, from anyone, the CLI's `mark` included. The answer names `gff lb approve`, `gff lb reject` and `logbook.decide`. So every decision goes through `logbook.decide`, with its draft applied and `approved_by` set. Ops from a peer still fold as they come (first writer wins); the rule guards local writes |
| Draft checks | A proposal's draft from a pane is checked strictly at filing: a forget of an op this hab does not hold, and a set of the status of a record it does not hold, are refused with `invalid`. An approve of a proposal an agent filed runs the pane rules (10.5) again, as the agent's session, against this hab now, over the draft or the person's edits. A failure answers `invalid` and applies nothing. An approve never applies a forget the agent could not write itself |
| First decision | The writer refuses a second decision it can see (5.6). Decisions made apart fold first writer wins, the later flagged `decided_twice` (E10) |
| Overruled approve | An approve batch that is not its proposal's standing decision is overruled. A record it created is hidden like a deleted one (`deleted` reads true) and flagged `approval_overruled` with the decision that stood. Only a person's restore stamped after the approve shows the record again, still flagged; an agent's restore leaves it hidden. A change it made to another record is not folded, and that record is flagged too. The record's revision goes up by one, so it never goes down |
| Fold order | A record whose ops carry `approved_by` folds against its proposal's standing decision. The standing decision is the first decision among the proposal's own status sets, read from its ops, never from folded rows. So no record waits for another to fold, and two approves that each touch the other's proposal fold the same on every replica. A proposal's `status` and `decided_by` are never overruled. A refolded proposal refolds what its approve batches touched, and `Changes` names those records, so events reach clients. A proposal this replica does not hold yet has no standing decision: its approves stand until it arrives. The fold version is 3, so an older file is rebuilt at open |
| Events | `logbook.updated` for the proposal when it is filed and when it is decided, and for each record the decision changes; `logbook.conflict` when a record gains `decided_twice` or `approval_overruled`. No new event: a client reads the proposal's status with `logbook.get` |
| CLI | `proposals [filters]` (pending unless `--status` names another), `approve <id> [--title t] [--body text\|-] [--changes json\|-]`, `reject <id>`. `mark <proposal> <status>` is refused (Only a decision decides). `--title` and `--body` edit the draft's first create; `--changes` replaces the changes whole. In a pane both verbs go to the pane route, which refuses them, and never to the file |
| Memory proposals | The store side only: a proposal's `expires` (RFC 3339, fixed at create), and `logbook.decide` refuses a pending proposal past it. A memory or flight rules proposal's approve records the decision and applies nothing: Part 8 writes the memory |

Where this spec was silent, stage 7 decided. Each is a default the owner may revisit:

| Point | Decision |
|---|---|
| Restore of an overruled record | Only a person's restore stamped after the overruled approve shows the record again: the standing `deleted = false` must be a person's. A person's restore made before the overrule arrived counts too, since it is stamped after the approve. An agent's restore, under `off` too, leaves the record hidden. The flag stays. Reason: the user can keep work that lost a race, as with "edited after delete" (5.6), and an agent cannot undo a person's decision |
| A forget in a draft | A person who wants a forget writes it with `gff lb forget`. An approve never applies a forget the proposing agent could not write directly. Reason: op ids (`<seq>@<origin>`) are easy to guess, so a draft filed now could name the person's next write |
| Plain writes of a proposal's status | Refused for every local writer, not only panes. Reason: a status set with no applied draft and no `approved_by`, made apart, could beat a real decision and hide its records |
| An overruled change to another record | Not folded, rather than undone by a new op, so every replica agrees without a writer. The record keeps the ops and shows the flag |
| Two approves made apart | The later approve is overruled like a reject's loser: its records are hidden, so nothing is applied twice |
| Forget in an overruled approve | A forget in the draft still redacts its target: a redaction is never undone (5.7) |
| `decided_by` from the app | The client's label, as the actor's device is until pairing gives clients device ids (10.5, Actor) |
| Edits | `edits` replaces the draft's changes whole; the CLI's `--title` and `--body` build that list from the draft |
| Resolve under `agents` | Refused from a pane, as Capcom's is: a resolve is a write, and a proposal of it would need its own draft shape |
| Not built | Filing memory proposals (the Part 8 hook, `instructions-mcp-flight-rules.md` 12), marking a memory proposal `expired` when its time passes, `gff lb status` showing a hab whose setting differs (9.1), and the app's approvals view (stage 8) |

---

## 10. Surface: CLI, API and MCP

### 10.1 One verb set, three doors

| Door | Who uses it | How |
|---|---|---|
| CLI: `gff lb <verb>` (long form `gff logbook`) | Agents in panes, people in a shell | Through the local hab's API; through the file when the hab is down (4.3) |
| API: `logbook.*` on the control connection | The app, `gff`, peers | `hab-logic-api.md` 3 rules; capability `logbook` |
| MCP: tools in the agent kit's MCP server | Harnesses that register MCP servers | `gff-hab mcp serve`, stdio, bridged to the hab |

- The CLI is the main agent door. The study in 1.1 found agents use a CLI and skip MCP when both
  exist.
- **`gff-hab mcp serve`** is the agent kit's one MCP server, with Logbook tools beside the catalog
  and channel tools. `gff-hab mcp launch` and `gff-hab mcp header` stay Part 8's subcommands.
- Every CLI verb prints short human lines by default and JSON with `--json`.
- On a host with no hab, `gff lb --host <label>` uses that hab's API over SSH.

### 10.2 Verbs

| Verb | Does |
|---|---|
| `brief [--project p]` | The session brief (11.2), for the current project by default |
| `find <words> [filters]` | Full-text search, newest and best first. Filters: type, status, project, tag, host, author, since |
| `ls [filters]` | List records: id, type, status, title |
| `ready [--project p]` | Open tasks and issues with no open blocker |
| `show <id>` | The record, its links both ways, notes, and any conflict flag |
| `history <id>` | Every op on the record, losers included |
| `new <type> <title> [--body … --tag … --link …]` | Create. The body may come from stdin |
| `edit <id> [--title … --body … --tag … --untag …]` | Change fields |
| `note <id> <text>` | Append a note |
| `mark <id> <status>`, `done <id>` | Set a status. `done` is `mark <id> done` |
| `claim <id>` | Set assignee to the calling session and status `doing` in one batch |
| `link <id> <kind> <id>`, `unlink …` | Change links |
| `supersede <id> <title> [--body …]` | Write a finding that supersedes this one |
| `delete <id>`, `restore <id>` | Set or clear `deleted` |
| `forget <op id>` | Redact a value everywhere (5.7). Asks first; `-yes` skips |
| `conflicts`, `resolve <id> <field> <value or op id>` | List and settle conflict flags |
| `proposals`, `approve <id>`, `reject <id>` | Proposal review (9.2) |
| `projects`, `project join <a> <b>`, `project rename <p> <name>`, `project private <p> [on\|off]` | Projects (3, 3.3) |
| `status` | Replica size, version vector, unsynced ops per peer, last sync per peer |
| `export [--format jsonl\|markdown] <path>`, `import <jsonl>` | Escape hatch and one-off import (13.1) |

Inside a pane, the current project comes from the pane's session. Outside one, from the working
folder's identity, else `--project`.

### 10.3 API

All additive under `hab-logic-api.md` 3.3. Each answers `unavailable` until its stage builds it.

| Item | Kind | Purpose |
|---|---|---|
| `logbook` | Capability | The hab has a Logbook replica |
| `logbook.brief`, `logbook.find`, `logbook.list`, `logbook.ready`, `logbook.get`, `logbook.history` | Methods | Reads |
| `logbook.write` | Method | One batch of changes with an idempotency key; returns ids and state (`applied` or `proposed`). `propose: true` makes the batch a pending proposal whatever `logbook.approval` says; Capcom's `file_record` sends it (`capcom.md` 6.5, which gives the shape it sends). The pane route must take it |
| `logbook.resolve`, `logbook.decide` | Methods | Settle a conflict; decide a proposal |
| `logbook.projects`, `logbook.status` | Methods | Projects; replica status |
| `logbook.pull`, `logbook.push` | Methods | Peer sync (7.1). Only on a peer link |
| `logbook.updated` | Event | A record changed (id, revision, type, project) |
| `logbook.conflict` | Event | A record gained or lost a conflict flag |

Subscribers to the `host` scope get Logbook events. A record's event revision is its count of
folded ops, so it never goes down.

### 10.4 MCP tools

Few tools, each with a clear job:

| Tool | Does |
|---|---|
| `logbook_brief` | The brief for the current or a named project |
| `logbook_search` | `find` and `ls` in one tool, with filters |
| `logbook_read` | One record with links and notes |
| `logbook_write` | Create, edit, note, mark, claim, link, supersede, by an `action` field |

Decision: four MCP tools. Reason: a small surface is easier for an agent to pick from; the CLI keeps
the full verb set for people and scripts.

### 10.5 Built (stage 4)

Task-0m8zy1we. Package `internal/lbsvc` serves the verbs over one store. gff-hab opens the replica
once (`openLogbook` in `cmd/gff-hab/logbook.go`, 7.6), and that one store serves sync and the
verbs (`startLogbookVerbs` in `cmd/gff-hab/logbook_open.go`). `internal/lbcli` is the CLI. The
shapes are in `api/logbook.go`.

| Item | Built |
|---|---|
| Methods | `logbook.find`, `.get`, `.list`, `.ready`, `.history`, `.write`, `.resolve`, `.projects`, `.status`. `logbook.pull` and `logbook.push` are stage 3's (7.6), `logbook.decide` stage 7's (9.4), `logbook.brief` stage 6's (11.4) |
| Events | `logbook.updated {id, revision, type, project, deleted}` and `logbook.conflict {id, revision, flags}`, host scope only, not in the snapshot. The revision is the record's count of folded ops |
| Capability | `logbook`, one capability for sync and the verbs, sent when `<state>/logbook.db` opens with the hab's device id as origin. A corrupt file, or one of another origin, is set aside first (7.6); a file that still does not open stops gff-hab with the reason |
| Sync | Each write through the verbs nudges sync, which sends the new ops at once (7.2). Writes wait out the start hold, and a restored file refuses them with `unavailable` (7.6); the CLI's own store honours both, since both live in the file |
| Pane route | Every method above. `logbook.find` and `logbook.get` moved from the read methods to `hook.LogbookMethods`, whose handler knows the pane |
| CLI | `gff lb` (long form `gff logbook`), and `gff-hab lb`, the same code. `export` and `import` are stage 9's (13.2); `brief` is stage 6's (11.4). `approve` and `reject` are stage 7's (9.4); `project join`, `project rename` and `project private` stage 5's (3.3) |
| MCP | `logbook_search` and `logbook_read` (read tools, built with Capcom's tools), `logbook_write` (an action tool). `logbook_brief` is stage 6's (11.4) |

Where this spec was silent, stage 4 decided:

| Point | Decision |
|---|---|
| Which binary | Both `gff lb` and `gff-hab lb`. The spec names `gff lb`; every pane already has `gff-hab` on its path (its hooks run it), and a Linux install copies only gff-hab and the holder. One package, two entry words |
| Write shape | `logbook.write {idempotency_key, propose, changes, source}`. A change is `{op: create\|set\|add\|remove\|note\|forget, id, ...}`: a create takes the record fields and tags and links added in the same batch; set takes `field` and `value`; add and remove take `tag` or `link`; note takes `text`; forget takes `op_id`. The answer is `{state: applied\|proposed, ids, ops, proposal, repeated}`. Capcom's `file_record` shape is a subset |
| Idempotency keys | In a `keys` table of the same file, written in the write's transaction, so a key holds across gff-hab and the CLI fallback. A repeat returns the first write's ids and ops with `repeated`; the same key with another request is `conflict`; a refused write keeps no key. Keys stay 90 days, for a phone that queued writes offline (14). At most 128 bytes, as on `session.create`. Keys never sync. The CLI sends a fresh key with each write, so a write the hab took just before it went away is not applied again from the file. `logbook_write` sends a fresh key with each call, so mark doing, mark blocked, mark doing are three writes; a caller that retries a call whose answer it lost passes the same `key` argument. A key older than 90 days is not found, though no write has dropped it from the table yet. The key's hash is of the request as its writer sent it, before gff-hab fills in the pane's session, so a key means one request whichever door it came through |
| Actor | Pane route: `{agent, harness, session, session_name, pane, host}`: the Flightdeck session id and its name at the write (the CLI shows the name; an op without one shows the id), the harness from the pane's agent state, this hab's host id. The source's session is always set to the pane's. Control connection: `{person, device}` with the hello's label (else the client name), until pairing gives clients device ids. CLI fallback: always `{person, device: cli}`, with `$GFF_HAB_HOST_ID` or the system host name, since a pane never writes the file |
| Which door the CLI takes | In a pane (`$GFF_HAB` set), the pane route; else the control socket. The CLI opens the file only when the socket does not answer: nothing listens on it (`ECONNREFUSED`) or no socket file is there (`ENOENT`). Any other error is the hab's answer and the CLI reports it: an HTTP status from the pane route (an unknown token, a body over 1 MiB) exits 3; a `$GFF_HAB` that does not parse exits 1 |
| The file from a pane | A pane never writes the file. Its reads may use it. A write tries gff-hab again for up to 15 s (a gff-hab restart is short, and the holder keeps the pane running), then fails with an error that says nothing was written. Outside a pane the CLI writes to the file as a person. Reason: nothing can vouch for a pane's session or its Capcom rule while gff-hab is down, and a file write from a Capcom pane would skip review |
| `--socket` in a pane | Reads only. A write over the control socket would land as a person, so the CLI refuses it while `$GFF_HAB` is set, as `confirm answer` does. Unsetting `$GFF_HAB` is outside what the CLI can see |
| The state folder in a pane | gff-hab sets `$GFF_HAB_STATE` in every pane it starts to its own state folder, `-state` included, so the CLI's reads find the file while gff-hab is down. Outside a pane the CLI looks at `--state`, then `$GFF_HAB_STATE`, then the default; a hab installed with another `-state` needs one of the first two |
| Proposals from a pane | An agent never decides a proposal: the pane route refuses a create of type `proposal` (a pane files one with `propose: true`), a set of `decided_by`, and a set or resolve of a proposal's `status`, with `invalid`. A person decides through the control connection or the CLI outside a pane, with `logbook.decide` (9.4). A pane's proposal draft is checked strictly at filing, and again at the approve (9.4, Draft checks) |
| Forget from a pane | A pane forgets only ops its own session wrote; a pane outside a session forgets nothing. Any other forget is refused with `invalid` and a hint that a person can forget it. A default the owner may revisit (5.7) |
| Writes over a peer link | Refused with `invalid`: a client writes on the hab it talks to, and sync carries the ops (2.5, D3). Reads over a peer link work |
| Proposals | `propose: true` stores one pending proposal record: target `record`, the write's changes and source as its `draft`, the first create's project, and a title made from the first change. Nothing else lands. `logbook.decide` applies the draft on approve (9.4) |
| Capcom | A write from a Capcom session's pane is stored as a proposal even without `propose`, and `logbook.resolve` from it is refused: Capcom's records wait for review (`capcom.md` D3, D14) |
| `claim` | Sets `assignee` to the word `self` and status `doing`; the hab replaces `self` with the writer's session, else its device. A writer with neither, such as a pane outside a session, is refused with `invalid` |
| Change caps | One `logbook.write` takes at most 200 changes, from any writer; more answers `invalid` with a hint to split it |
| Pane write limit | 60 writes (`logbook.write` and `logbook.resolve`) and 600 changes per pane in a sliding minute; the write past either answers `invalid` with a hint and writes nothing; gff-hab logs it once a minute per pane. A person is never limited. Not built yet: raising it in the hab's config, and the status board mark (14), which waits for the app's Logbook views (stage 8) |
| Lists | `logbook.list` and `find` without words go newest change first; `find` with words best match first. Filters: type, status, project (through joins), tag, host (the actor's host or the create's origin), author (the actor's session, device or harness), session (the actor's or the source's: Capcom lists one conversation's proposals with it), since, general, flagged, deleted. Default 50, at most 500, with `more` |
| Events for other writers | gff-hab reads `Changes` after each of its own writes and every second, so the CLI's writes to the file reach clients within a second. Events start from the file's end at gff-hab's start: a client takes a fresh read on a new epoch |
| `logbook.conflict` | Sent when a record's set of flag kinds changes, any kind; an empty list means the last one went |
| Not built | `--host <label>` over SSH (10.1). Stage 5 finds a pane's project for the private rule (3.3); stage 6 makes it the default view (11.4) |

---

## 11. Recording habits through the hook endpoint

The habits that keep agents recording live in gff-hab's hook endpoint, so every harness gets them
from one place, with no extra process per hook.

### 11.1 What reaches the agent

| Habit | When | What the agent gets | Default |
|---|---|---|---|
| Brief | Session start, and after a compaction | The brief (11.2) as added context | On |
| Reminder | On a prompt submit, when the session has changed files for 30 minutes or 20 turns with no Logbook write | One line: "Nothing recorded yet this session. Record findings and task changes with `gff lb`." At most once an hour | On |
| Closeout | The first stop after the session made commits or file edits and wrote nothing to Logbook | A one-time request to record what it learned or say there is nothing to record. Never twice per session | See 15.2 |

- Each habit has its own setting: `logbook.habits.brief`, `.reminder`, `.closeout`, per hab and per
  project.
- **Everything shows in flight rules.** Each injection is a `hook` entry in the session's ledger
  (`instructions-mcp-flight-rules.md` 11), with its text on tap. Nothing steers an agent
  invisibly.
- **The printed agent skill** (agent kit) tells agents what to record: findings when they learn
  something non-obvious, task status as it changes, notes for progress, supersede instead of
  editing a finding.

### 11.2 The brief

- For the session's project: tasks and issues in `doing` and `blocked`, then `ready` ones by
  priority, findings from the last 7 days, pinned guides, plans with open stages, untriaged count,
  conflict count, and tasks handed over from other projects (12.2).
- Records with no project tagged `everywhere`.
- Capped at 8 KB. Lines past the cap are dropped by rank, and the brief ends with "more:
  `gff lb ready`".
- One line per record: id, status, title. The agent reads a record with `show` when it needs it.

### 11.3 How each harness gets it

*(gk)* marks harness facts the hooks stage must confirm.

| Harness | Brief | Reminder | Closeout |
|---|---|---|---|
| Claude Code | `SessionStart` hook output as added context (also fires on compact and resume) | `UserPromptSubmit` added context | `Stop` hook with a block decision and a reason, once |
| Codex | `SessionStart` added context *(gk)* | `UserPromptSubmit` added context *(gk)* | `Stop` with a continue reason *(gk)* |
| opencode | gff-hab adds the brief at `session.created` through the pane's opencode server (11.4) | Not supported in the first build | Not supported in the first build |

- `gff-hab hook` gains a context mode: for the brief, reminder and closeout events it prints the
  hab's answer on stdout in the harness's shape. It keeps the 900 ms budget. When gff-hab is down,
  the command builds the brief from the local file itself (4.3); reminder and closeout are skipped.
- `gff-hab hook -print` adds these events to the printed config.

Decision: habits in the hook endpoint, each with a setting. Reason: the study in 1.1 shows hooks
carry the habit; one endpoint serves all three harnesses; the owner's rule gives every friction a
setting.

### 11.4 Built (stage 6)

The brief's query is `Store.Brief` in `internal/logbook` (`brief.go`), its text `BriefText` in
`internal/lbsvc` (`brief.go`). The habits' rules are in `internal/habits`. gff-hab answers them in
`habitService` (`cmd/gff-hab/habits.go`). The hook side is in `internal/hook` (`ContextEvent`,
`ContextOutput`, the `?context=1` post). The same path carries the context nudge
(`harness-adapters.md`, Context nudge) on Claude Code's `UserPromptSubmit` and `PostToolUse`:
gff-hab's one context handler asks the habit first, then the nudge, and joins both texts by a
blank line when both are due.

| Item | Built |
|---|---|
| Brief | `logbook.brief {project}` answers `{project, name, text, records, dropped}`. The text opens with the project's name, id, untriaged and flagged counts, and how to read a record. Then one section per kind, in rank order: doing and blocked (doing first), handed over, ready, pinned guides, findings from the last 7 days, open plans, records for every project. One line per record: id, status, title; a record with no status shows id and title. A record shows once, in its highest section |
| What each section holds | Handed over: an open task or issue with triage `new` and a `from` link. Ready: as `ready`, priority 1 first, no priority after 3, then newest. Pinned guides: `current` ones. Findings: created in the last 7 days, not superseded. Open plans: `open` with a stage that is not done or dropped. For every project: no project, tagged `everywhere`, not closed, not superseded. Untriaged counts open tasks and issues with triage `new`; flagged counts records with a flag |
| The cap | 8 KB. Lines go in by rank until the next would pass the cap with room kept for the more line; every line after is dropped. The text then ends with "more: `gff lb ready`". Each section reads at most 500 records, enough for any cap |
| No project | A brief with no project holds only the records for every project, under a line that says the folder has no project |
| Default view | On the pane route, `logbook.brief`, `logbook.list` and `logbook.ready` with no `project` cover the pane's project. `all: true` (new on list and ready) covers every project; `general` and a named project work as before. A pane with no project, and a person on the control connection, see every project. `find` always searches every project. The CLI's `conflicts` and `proposals` ask for every project |
| Current project outside a pane | `gff lb brief`, `ls` and `ready` take the project that holds the working folder's identity (3.1), found through `logbook.projects`, unless `--project`, `--general` or `--all`. Outside a repository there is none: `ls` and `ready` cover every project |
| Private projects | A pane's brief of a private project outside its own answers `not_found`, as a read of it (3.3). Its own brief shows its own private project |
| Context mode | `gff-hab hook` posts a habit's event with `?context=1`. gff-hab's sink takes the envelope first, then the habit answers: 200 with `{"text"}`, or 204 for nothing. The command prints the text in the harness's shape. It keeps the 900 ms budget |
| Claude Code and Codex | One shape for both. `SessionStart` (every source: startup, resume, compact, clear) prints `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":<brief>}}`. `UserPromptSubmit` prints the same shape with the reminder. `Stop` prints `{"decision":"block","reason":<closeout>}`. Codex is taken to read Claude Code's shapes, as its PermissionRequest output already does; that waits for a check against a real Codex (11.3 *(gk)*) |
| opencode | gff-hab adds the brief itself, through the pane's opencode server that it already follows: at a top-level session's `session.created` event it posts the brief to `POST /api/session/{id}/synthetic` with `resume: false`, so opencode keeps it in the session's inbox for the next prompt and starts no turn. Once per session: the pane's record keeps the sessions briefed or tried, so a replaced follower or a gff-hab restart never posts again, and a brief it cannot record is skipped; a failed post is logged and not tried again; none for a round's run session, and none when the habit is off for the project. The printed plugin only posts events and no longer touches the session. Checked against opencode 2.2.5: the plugin's old shape (a named export taking `client`, `client.session.prompt` with `noReply`) does not load there, and `noReply` is gone; the synthetic message lands in the inbox and no turn starts. No reminder and no closeout (11.3) |
| gff-hab down | At the brief's event, when nothing answers at the socket, the command opens `logbook.db` in `$GFF_HAB_STATE` and builds the brief itself: the payload's `cwd` gives the project by identity, and private projects but that one stay hidden. It honours the same switches as gff-hab, through `habits.On`: the brief's setting from `<state>/settings.json` under the `[settings]` locks, and `[logbook.habits_off]`, both from the config file in `$GFF_HAB_CONFIG`, else the default; a missing or broken file counts as none. gff-hab run with `-config` sets `$GFF_HAB_CONFIG` to that file, absolute, in every pane it starts, as it sets `$GFF_HAB_STATE`. The reminder and the closeout print nothing |
| Reminder | Counted per Flightdeck session (a pane outside a session counts as its own). A file change starts the clock: a PostToolUse of Write, Edit, MultiEdit, NotebookEdit or apply_patch, or a shell tool (Bash, shell, exec_command, local_shell) whose command runs `git commit`. A prompt 30 minutes after that change, or the 20th prompt since it, gets the reminder, at most once an hour. A Logbook write from the session's pane (`logbook.write` or `logbook.resolve`, proposals included) starts the count again |
| Closeout | The first `Stop` after a file change or a commit, in a session that made no Logbook write, gets the closeout. Never twice per session, whatever happens after. Not for a round's run session: its run ends at the turn's end (rounds.md 7) |
| Settings | Hab-side settings `logbook.habits.brief`, `.reminder` and `.closeout`, bools, on by default (15.2 option A). `[settings]` in the hab's config file can lock them (`app.md` 20.5) |
| Per project | `[logbook.habits_off]` in the hab's config file maps a project, by its Logbook id or an identity, to the habits it turns off: `"github.com/owner/repo" = ["closeout"]`. gff-hab reads the table again when the file changes; a table that does not parse keeps the last good one. An unknown habit is refused by name |
| MCP | `logbook_brief` (a read tool) with an optional `project` |
| Prompt counting | A prompt counts toward the reminder whether or not the reminder is on, so turning it on later does not reset the count |

Where this spec was silent, stage 6 decided. Each is a default the owner may revisit:

| Point | Decision |
|---|---|
| Per-project setting | In the hab's config file, not in Logbook. Reason: a setting is a hab's, like the others; a Logbook field would sync a host's preference to every host |
| What counts as a change | Edit tools and `git commit` through a shell tool, read from the PostToolUse payload. A file written by a shell command is not seen |
| "No Logbook write" | For the reminder, since the last write; for the closeout, in the whole session |
| Rank across sections | Sections in the order above; within one, its own order. A lower section loses all its lines before a higher one loses any |
| The tracker | In memory: a gff-hab restart starts each session's counts again, and a session may get a second closeout after one |
| A blocked stop | The `Stop` that carries the closeout still reaches the agent state, so the session shows done, then working, as the agent takes its extra turn |
| Not built | Ledger entries for each injection (11.1): the flight rules ledger (`instructions-mcp-flight-rules.md` 11) is not built yet. Claude Code records hook context in its transcript, where the ledger's reader will find it. The printed agent skill's recording advice (11.1) |

---

## 12. Other projects: what agents see and how they work together

**Key call** (how agents see and interoperate with other projects).

### 12.1 Seeing

- **Every replica holds every record,** so any agent can read any project's records with `find`,
  `ls` and `show --project`.
- **The default view is the current project.** `ls`, `ready` and the brief cover the current
  project unless asked. `find` searches every project and marks each hit with its project.
- **The catalog links in.** `gff projects` (`mesh.md` 5.3) shows each project's open task count, so
  an agent can go from "which hosts have this project" to "what is open there".
- **Private projects.** A project marked `private` is left out of every agent's reads outside that
  project. The user still sees it in the app and the CLI.

### 12.2 Working together

- **Hand a task to another project.** An agent files a task in project B with `--project B` and a
  `from` link to its own record. The task is `triage: new`. It appears in B's next brief under
  "handed over" and on the status board as untriaged. No message is sent and no agent is woken.
- **Ask a running agent now.** That is a channel message (`mesh.md` 6), not a record. An agent can
  do both: file the task and send a one-line message that names its id.
- **Shared findings.** A finding about a tool used everywhere has no project and the tag
  `everywhere`, so every brief shows it.
- **Cross-project links** work like any link. `show` lists records in other projects that point
  here.

Decision: read everything, default to the current project, hand over by filing a task. Reason:
every replica already holds everything, so hiding costs effort and buys little; a filed task needs
no running agent on the other side; channels stay for the urgent case.

---

## 13. The optional always-on host

`mesh.md` 8 leaves the choice of extras to this part.

| Extra | Runs on | Config | Behaviour |
|---|---|---|---|
| Backup | Every hab marked always on | `logbook.backup.path`, `logbook.backup.keep` (default 30) | A nightly SQLite backup of `logbook.db` plus a JSONL export to the path. After a sleep, once, not once per missed night |
| Git export | The one hab where the user sets `logbook.export.git` | A local repository path | One Markdown file per record, by project, committed after changes, at most hourly. One way: edits in the export are not read back |
| Vector search | Each hab where the user sets `logbook.vectors` | Model and index path | Embeds record text and answers `logbook.find {semantic: true}`. Other habs forward the query to an online hab that has it, else answer "no always-on host with vector search" |

- Git export and vectors need explicit config, not just the mark: two habs writing one export would
  fight, and embeddings cost memory.
- **Every hab can export by hand** with `gff lb export`, always-on or not. That is the escape hatch.

### 13.1 Import

- `gff lb import <file.jsonl>` reads one record per line in the export format, ids verbatim (2.3).
  It writes each record as a create batch from the importing hab, with the original dates kept as
  `created` and marked imported.
- Converting another tool's records to that format is a one-off step outside the product, written
  by whoever owns that data. Flightdeck ships no converter for any particular tool.

Decision: extras by explicit config, backup on every marked hab. Reason: backups are safe to run
twice; an export or an index is not, and the user knows which machine should hold them.

### 13.2 Built (stage 9)

The extras are in `internal/lbextra`. gff-hab runs them in `cmd/gff-hab/logbook_extra.go`. The
store side (`Export`, `Import`, `Backup`) is in `internal/logbook` (`export.go`, `query.go`).
The verbs are in `internal/lbcli`.

| Item | Built |
|---|---|
| Config | `[logbook.backup]` (`path`, `keep`), `[logbook.export]` (`git`) and `[logbook.vectors]` (`base_url`, `model`, `key_command`, `index`) in the hab's config file. The vectors section has the shape and rules of `[summaries]` (`app.md` 16.5): off with neither `base_url` nor `model`, an error with only one, `base_url` http or https with no user or password in it. The old `url` key is refused, naming `base_url`. Paths are absolute or start with `~/`. gff-hab reads the backup and export sections every minute, and the vectors section at start. The index defaults to `logbook-vectors.gob` in the state folder |
| Backup | On a hab whose own catalog carries the always-on mark (`mesh.md` 8). The slot is 03:00 local time. A backup is due when the newest one is older than the last slot. So a hab that slept through several nights makes one backup on waking, and a first start makes one at once. Each backup is `logbook-<UTC time>.db`, a `VACUUM INTO` copy, and `logbook-<UTC time>.jsonl`, the export. The folder is 0700 and the files 0600. Default folder `<state>/logbook-backups`. The oldest past `keep` go |
| Restore | Stop gff-hab and copy a backup's `.db` to `<state>/logbook.db`. It opens as the same origin. Peers send back the own ops it lacks, and its next write uses a seq past them (4.5, Own ops back) |
| Export format | One JSON object per line: `id`, `type`, `title`, `body`, `status`, `project`, `fields` (the other scalars), `tags`, `identities`, `links`, `notes` (`time`, `text`, `actor`), `created` (RFC 3339, UTC), `created_by`, `source`, `deleted`, `imported`. Projects first, then the rest, each by id. Deleted records are included |
| Import | One create batch per line, from the importing hab, with the id verbatim. The batch sets the fields `created` (the line's date, else the import's time) and `imported` (true), adds tags, identities and links, and appends the notes' text. Both fields are fixed after create. The fold version is 6: an older file is rebuilt at open, and an older build stores these creates but does not fold them (4.4). The API's record shows `imported`, and `created` is the original date |
| Import rules | An id the replica holds is skipped and counted, so a second run writes nothing. A proposal is skipped: it is the filing hab's review item. The import stops at the first line it cannot write, and names the line; the lines before it stay written. A line is at most 16 MiB |
| CLI | `gff lb export [--format jsonl\|markdown] <path\|->` and `gff lb import <file\|->` open `logbook.db` as a person, whether or not gff-hab runs. gff-hab sees the import's ops as another writer's: events within a second, and sync sends them. Both refuse to run in a pane. JSONL goes to a new file, never over one. `forget` asks with "Older backups and git exports keep it" |
| Markdown tree | One file per record that is not deleted: `<project id>/<record id>.md`, `general/` for no project, a project's own file in its folder. A front matter of JSON values (valid YAML), then the title, body, links and notes. A project id that is not a plain id goes to `general/`. The tree writes a `.logbook-export` marker and refuses a folder that holds other files without it. A file whose record is gone, moved or deleted is removed. An unchanged file is not rewritten |
| Git export | Where `[logbook.export] git` names a folder. gff-hab makes the folder and runs `git init` when needed, writes the tree, then `git add -A` and one commit, when anything changed and the last commit is an hour old. The commit's author is "Flightdeck Logbook", with signing and hooks off, so it needs no git identity. The repository has no remote; pushing it is the user's choice |
| Forget in the git export | The next export commit holds no forgotten text in its files. History is not rewritten: commits made before the forget keep it, as 5.7 says of exports made before a forget |
| Vector search | `logbook.find {words, semantic: true}`. A hab with `[logbook.vectors]` keeps an index of every record that is not deleted (title, body, tags and notes) and ranks by cosine similarity. It sends the welcome capability `logbook-vectors`. A hab without one forwards the call to the first online peer whose welcome lists it, in peer file order. A call that came over a peer link is never forwarded. The ranked ids then pass this replica's filters and private rule (3.3), so a record not synced here yet is left out. With no hab to ask: `unavailable`, "no always-on host with vector search". Words are required |
| The engine | No model ships with Flightdeck. The engine is an embeddings server the user runs. gff-hab posts the OpenAI-compatible `{"model", "input": [...]}` to `<base_url>/embeddings` and reads `data[].embedding`. With `key_command`, gff-hab runs the command in the background as `[summaries]` does: an argv with no shell, the key in memory only, sent as `Authorization: Bearer <key>`. A failed run waits 30 s, doubling to 30 minutes, and logs one line per streak, without the command's output. A 401 or 403 runs the command again, at most once in 5 minutes after a good run. No key reaches a log or an error. The index is a file (`index`, default `<state>/logbook-vectors.gob`). It embeds the records whose text changed, 32 per call, every 30 s. The text is at most 8,000 bytes, cut on a character boundary. A record whose text changed loses its vector at once, from memory and the file, before the new one is asked for: a forget leaves the index even while the server is down, and the vector is made again from what is left. When the server refuses a batch as bad input (400, 413 or 422), each text goes alone, and a record still refused is marked and asked for again only when its text changes. A semantic find never waits for the index's embeds |

Where this spec was silent, stage 9 decided. Each is a default the owner may revisit:

| Point | Decision |
|---|---|
| Backup without a path | The mark alone turns backups on, into `<state>/logbook-backups`. Reason: 13 says backups run on every marked hab |
| The nightly time | 03:00 local, not configurable |
| Export and import door | The file, as a person, not the API. Reason: no new method; the file is the replica, and gff-hab already serves another writer's ops |
| Export from a pane | Refused, with import. Reason: an export holds every private project (3.3) |
| Proposals in an import | Skipped |
| Git history after a forget | Kept. Rewriting it would break the user's clones of the export |
| Which peer runs a semantic find | Any online peer with the capability, not only one marked always on. The answer's words stay those of 13 |
| Vector model | An `HTTPEngine` against an OpenAI-compatible embeddings server the user runs, off unless configured. The config shape is that of `[summaries]`. The sample is Ollama with `nomic-embed-text`, an example, not a dependency. No model ships, because common local servers serve embeddings; speech bundles a model only because no such server exists for it. The owner may revisit this. The tests use a fake engine of hashed words |
| Not built | A size or time cap per backup, a restore command, a backup on a hab without the mark, pushing the export repository, and a property test that an export round trip folds to the same records |

---

## 14. Edge cases

| Case | Behaviour |
|---|---|
| gff-hab restarts during a write from the API | The write commits or it does not; SQLite gives one or the other. A client retries with its idempotency key |
| gff-hab is down and an agent writes | The CLI in the agent's pane tries gff-hab for 15 s, then fails and writes nothing (10.5). A person's CLI outside a pane writes to the file (4.3); gff-hab sees the new ops on start and syncs them |
| Two agents on one host write at once | SQLite's lock orders them. Each gets its own seq |
| A laptop writes for a week offline | Every write lands locally. On its next link, sync sends them; conflicts, if any, are flagged |
| A record is linked to an id the replica lacks | Shown as "not here yet"; resolves when the op arrives |
| An agent sets a finding's body | Refused with `invalid` and a hint to supersede |
| An agent writes a secret into a note | `forget` redacts it on every replica; older backups keep it (5.7) |
| An agent floods writes (a loop) | Writes from one pane above 60 a minute, or above 600 changes a minute, are refused with `invalid` and a hint, and the status board shows it. A cheap guard; the user can raise it |
| A body over 256 KiB | Refused; the hint suggests a file in the repo and a link |
| The brief would be larger than 8 KB | Cut by rank (11.2) |
| A session runs in a folder with no project identity | Its records go to a folder project (3.1). Moving them later is a `project` field change |
| A project's repository is deleted on every host | Its records stay. The project shows "no checkout on any host" |
| The phone writes while no hab answers | Queued in the app with a key, shown as "waiting to send" |
| A peer runs an older build | It stores and forwards newer ops without folding them (4.4) |
| `logbook.db` is restored from a backup, same device id | Writes wait or are refused until sync brings the newer own ops back from a peer (7.6, Restored file) |
| `logbook.db` is corrupt on one hab | gff-hab refuses to open it, moves it aside, and pulls a full copy from a peer. With no peer it starts empty and says so. Local ops not yet synced are lost |
| Clocks differ | HLC order (5.1); shown times come from the origin's clock |
| Two hosts decide one proposal while apart | The first decision stands on both; the later is flagged "decided twice". An approve that lost has its record hidden and flagged "approval overruled" (9.3, 9.4) |
| An agent tries to decide a proposal | Refused with `invalid`: the pane route never takes `logbook.decide`, and a set of a proposal's status from a pane is refused (10.5) |

---

## 15. Open questions for the owner

Only questions a user sees.

### 15.1 Do Capcom's records wait for review?

`capcom.md` says every record Capcom files waits for review (D3, D14). The owner later chose that
memories are added without approval by default.

| Option | For | Against |
|---|---|---|
| A. Capcom keeps its review list; other agents' writes land at once | Capcom writes for the user, often across projects, from a loose conversation; a review at the end is cheap | Two rules for one store |
| B. Capcom follows `logbook.approval` like every agent | One rule | A misheard "note that" lands without a look |

Recommendation: **A.** The Capcom review is one batch at the end, not a prompt per record.

Settled for memory (owner, 2026-11-06; `capcom-service.md` 6.4, E20): the Capcom service's
`capcom` guides (no project, tag `capcom`) land at once with a "Noted" line and Undo, except at
`logbook.approval = agents`, where they wait like every agent's write. Every other Capcom record
stays a proposal (A). Built in Capcom stage A4.

### 15.2 Closeout default

| Option | For | Against |
|---|---|---|
| A. On: once per session, the first stop after changes with nothing recorded asks the agent to record or say there is nothing | Recording stays high; 1.1 shows the habit drops without it | One extra agent turn per such session |
| B. Off; the reminder alone | No extra turn | Fewer records |

Recommendation: **A**, with the per-project setting to turn it off.

Stage 6 builds A, the default, with the per-project switch (11.4).

### 15.3 Cross-project reads

| Option | For | Against |
|---|---|---|
| A. Agents may read every project; a project marked private is hidden from agents outside it | "What did we learn about X elsewhere?" works; one flag for the exceptions | A private project must be marked |
| B. Agents read only their own project and general records unless the user allows more | Tighter | Cross-project learning needs setup |

Recommendation: **A.**

Stage 5 builds A, the default, until the owner decides otherwise (3.3).

### 15.4 Import from other record stores

The product ships `gff lb import` for a generic JSONL file (13.1). Converting records from another
tool into that file is a one-off step outside the repo; Flightdeck ships no converter for any
particular tool.

---

## 16. Decisions

| # | Decision | Reason |
|---|---|---|
| D1 | Six record types: finding, task, plan, guide, issue, proposal | Usage shows four carry the load; issue and proposal are owner and Part 8 needs |
| D2 | Random ids `<type>-<8>`, never changed; imports keep ids | Coordination-free minting; ids quoted elsewhere keep resolving |
| D3 | Origins are habs only; clients write through a hab | A phone cannot be a reliable sync peer |
| D4 | Projects are records with an identity set, matched by git remote, then root commit, then folder (key call) | Worktrees and clones join; renames do not orphan records; same identity as the catalog |
| D5 | One package, `logbook.db` per hab, op log as the only truth, folded tables rebuildable | Pure fold makes convergence testable; no migrations |
| D6 | The CLI writes the file directly when gff-hab is down | Memory works through hab restarts |
| D7 | Scalar fields last writer wins by HLC; conflict means neither saw the other, by `prev` | The owner's rule, with a precise flag that avoids false alarms |
| D8 | Body is a whole scalar; notes are an append log | No text merge; notes carry concurrent writing |
| D9 | Sets are add-wins | Standard, simple, never loses an add |
| D10 | Findings fixed at create; supersede by link | Matches use; supersede never conflicts |
| D11 | Proposal decisions first writer wins | Matches approvals' first-answer rule |
| D12 | Replicated `forget` op | A leaked secret must be removable |
| D13 | Sync by version vector, multi-hop, no outbox items | Every replica holds every op; repeats and loops are harmless |
| D14 | A far-ahead clock is accepted and shown | Refusing would break convergence |
| D15 | Approval setting `off` by default, `agents` to hold agent writes | Owner decision |
| D16 | CLI `gff lb` is the main agent door; four MCP tools in `gff-hab mcp serve` | Agents use the CLI; small MCP surface; no clash with Part 8's `mcp` subcommands |
| D17 | Habits in the hook endpoint with one setting each; injections shown in flight rules | Every harness gets them; nothing invisible |
| D18 | Agents read every project by default, view the current one, hand over by filing a task (key call) | Replicas hold everything; filing needs no running agent |
| D19 | Always-on extras by explicit config; backup on every marked hab | An export or index must have one writer |
| D20 | A pane write limit of 60 a minute | A cheap guard against a looping agent |

---

## 17. Required property tests

These run in Go with simulated replicas in one process: real `internal/logbook` stores on temp
files, a fake link that can drop, delay, repeat and reorder messages, and fake clocks per replica.
Each run logs its seed (`docs/testing.md`).

| # | Property | Setup |
|---|---|---|
| P1 | **Convergence.** After every partition heals and sync goes quiet, all replicas have the same ops, the same folded records and the same conflict flags | 3 to 5 replicas, random writes of every op kind, random partitions, heal, compare. 500 runs per push, 20,000 nightly |
| P2 | **Fold equals rebuild.** Folding ops as they arrive gives the same tables as `rebuild` from the log | After every P1 run. Stage 2, before P1 exists, runs it after its own random histories: 100 runs per push, 20,000 nightly (4.5) |
| P3 | **Order does not matter.** Any arrival order of the same ops, respecting per-origin seq, folds to the same records | Shuffle per-origin streams; compare |
| P4 | **No lost write.** Every op a replica wrote is on every replica after heal, unless its disk was wiped before any sync | P1 with random crashes; check against a written-op ledger |
| P5 | **Repeats are harmless.** Delivering any op or page twice changes nothing | Fake link repeats 10% of messages |
| P6 | **Conflicts are exact.** A flag appears if and only if two `set` ops on one field are concurrent by `prev` | Generate known causal histories; compare flags to the expected set |
| P7 | **Multi-hop.** Replicas A and C that never link converge through B | Line topology A-B-C with B often down |
| P8 | **Clock skew.** Replicas with clocks ±1 day off still converge, and a write made after seeing an op orders after it | Fake clocks with offsets and drift |
| P9 | **Forget reaches everyone.** After heal, no replica's store or FTS index holds a forgotten value | Forget during a partition |
| P10 | **Two writers on one file.** gff-hab and the CLI writing at once never reuse a seq or lose a write | Two processes, `-race`, real file |

P1 to P9 are fast enough for every push at the counts above. The nightly run raises the counts and
adds a soak of 1 million ops over 5 replicas.

---

## 18. Later stages

Each stage gets a fresh-eyes review before it closes (core functionality).

| # | Stage | Builds | Acceptance checks |
|---|---|---|---|
| 1 | Study (this task) | This spec | Exists; later stages listed with checks; no dependency on any personal tool |
| 2 | Store package | `internal/logbook`: ops, HLC, fold, FTS, ids, record types and their rules (2, 4, 5), `rebuild`, two writers (4.3) | P2, P3, P6, P10 pass; every example in 6 is a unit test; a finding body `set` is refused; a newer op is kept and forwarded but not folded; `check-pure` passes |
| 3 | Sync | `logbook.pull` and `logbook.push` on the peer link, version vectors, paging, wake-ups (7) | P1, P4, P5, P7, P8, P9 pass; an end-to-end test with two real habs over `ssh localhost` (skips without it) syncs 10,000 records and then a write in under 2 s; a hab rebuilt with a new key gets its old ops back |
| 4 | CLI, API and MCP | `gff lb` verbs (10.2), `logbook.*` methods and events (10.3), the four MCP tools in `gff-hab mcp serve`, idempotency keys, the pane write limit. Built: 10.5 | Golden messages for every new method and event (core test 5); the CLI writes while gff-hab is stopped and the hab syncs those ops after start; a repeated write key applies once; a write through a pane token records harness, session and host |
| 5 | Projects | Identity set, first sight, join, remote change, private flag (3, 12.1). Built: 3.3 | SSH and HTTPS remotes of one repository match; two worktrees share a project; a remote rename keeps records; E9 holds across two habs |
| 6 | Habits | Context mode in `gff-hab hook`, brief, reminder, closeout per harness (11), ledger entries. Built: 11.4 | Fixture tests per harness give the exact injected text; the brief stays under 8 KB with 1,000 open tasks; with gff-hab stopped the brief still prints from the file within the 900 ms budget; closeout fires at most once per session; each habit's setting turns it off |
| 7 | Approval | `logbook.approval`, proposals, decide, first-writer-wins, overruled approvals (9); memory proposals from Part 8 stored here. Built: 9.4 | Under `agents`, an agent write returns `proposed` and lands only on approval; E10 holds across two habs; a reject that wins hides the approved record and flags it |
| 8 | App views (Part 6) | A Logbook view: projects, lists with filters, record view with history and conflict banner, resolve; proposals in approvals; untriaged count on the status board; quick capture | The device smoke adds: create a task on the phone offline, see it on the Mac after reconnect; resolve a seeded conflict from the phone |
| 9 | Always-on extras and export | Backup, git export, vector search (13); `export` and `import`. Built: 13.2 | A backup restores to a working replica; the git export has one file per record and no forgotten text after a forget; with no vector hab the semantic flag answers the stated message; an import keeps ids |

Order: 2, then 3 and 4 in parallel, then 5, 6 and 7 in parallel, then 8 and 9. Capcom's tools stage
answers `unavailable` for Logbook until stage 4 lands (`capcom.md` 12).

---

## 19. Sources

- Beads README, github.com/steveyegge/beads, read 2026-11-01.
- Claude Code memory and Codex memories, as summarised in `instructions-mcp-flight-rules.md` 9 and
  its sources.
- Kulkarni, Demirbas and others, "Logical Physical Clocks and Consistent Snapshots in Globally
  Distributed Databases" (2014), for HLC *(gk)*.
- git-bug and Taskwarrior sync, from general knowledge *(gk)*.
- `mesh.md` 4, 5, 6.3, 8, 10; `transport.md` 5, 6.6, 7.4; `hab-logic-api.md` 3; `harness-adapters.md`
  (Environment contract, Hook endpoint); `capcom.md` 3.4, 6.2, 10, 12;
  `instructions-mcp-flight-rules.md` 9, 11, 12; `docs/testing.md`; `landscape.md` 2.18, idea 4.2.
- The project's plan for Part 12, its owner decisions and an earlier three-reviewer study of the
  memory architecture. A study of one personal record setup (1.1) supplied usage facts; none of its
  private detail is reproduced here.
