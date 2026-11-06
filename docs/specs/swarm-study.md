# Swarm study: several agents on one task, Capcom as an always-on service

A study, not a spec. The owner asked (2026-11-06) for three things: optional multi-agent work,
an omni-agent, and a texting agent. Later guidance narrowed them:

- The core is Capcom as an always-on, server-style agent that the owner reaches through the
  iPhone app and its notifications, and that never makes the owner think about context (section 4).
- Multi-agent work is a separate track (sections 1 to 3).
- An iMessage bridge is optional and minor, and a good Capcom may make it unneeded (section 5).
  SMS is out.

## 1. What Flightdeck already has

| Piece | What it gives | Where |
|---|---|---|
| Channels | Agent-to-agent text on any host, a disk outbox, dedup | `mesh.md` 6.1-6.3 |
| Deliver when idle | A message waits while the target works and is held while it is blocked | `mesh.md` 6.2 |
| `gff-hab wait` | A lead waits for a message or a worker's state without reading a screen | `mesh.md` 6.4 |
| `hand_off`, `start_session` | Work to the agent on a project, or a seeded session on any host | `capcom.md` 3.2, 3.3, 6.4 |
| Spawn verb (open) | A teammate in any harness by alias ("a codex reviewer"), result through channels | task-6iwat66b |
| Capcom | A standing, folder-free session that reads every host; not a router or supervisor | `capcom.md` 2.1, 7 |
| Rounds | Scheduled `fresh`, `resume` and `fork` runs that never type into a session in use | `rounds.md` 5, 6 |
| Logbook | Records on every host, `claim`, hand-over to another project | `logbook.md` 10.2, 12.2 |
| Catalog, `suggest_host` | Every host's tools, projects and agents; ranked hosts, no scores | `mesh.md` 5, 7 |
| Worktrees | One checkout per writer | `hab-logic-api.md` 3.10 |

Missing for multi-agent work: a record that says "these sessions are one task", one view of
them, and a rule for how their commits land.

## 2. Multi-agent patterns, with clear eyes

| Pattern | Pays off when | Costs |
|---|---|---|
| Subagent in the harness (Claude Code, Codex, opencode) | Read-heavy side work; a summary returns | Own tokens, same limit. Flightdeck adds nothing |
| Lead and teammates, shared task list (Claude Code agent teams, experimental) | Independent modules, competing hypotheses, review | About 7x one session's tokens in plan mode. Same-file edits overwrite |
| Sessions on one branch, a shared queue, a script that serialises commits, a second session reviewing each change (HN 50380658) | Many small, separable items | One user's report: 4,454 reviews of 4,471 commits, four in five found something. Reviewers "always find something": cap them to the top item |
| Separate-context reviewer, ideally another harness | Nearly always; the best-supported pattern | One extra session per change |
| Parent waits on children across hosts (herdr `hc wait --children`, Paseo's start-an-agent tool) | A lead hands out work and collects it | No merge rule in either tool |
| Orchestrator, blackboard, pipeline (kadhar.dev) | Fixed, well-split work | Bottlenecks, race-like bugs, delegation loops; needs a depth cap and a budget |

The skeptics: Anthropic's research system spends about 15x a chat's tokens and says coding has
"fewer truly parallelizable tasks". Cognition and OpenHands: parallel agents make conflicting
implicit decisions. Usage is wasted when each worker re-reads the repo, when workers edit the
same files, when leads poll, and when review loops have no cap.

## 3. Multi-agent option: a session group

**Behaviour.** The user, Capcom or an agent starts a group: a lead and a few workers, each an
ordinary session on any harness and host. The app and gff show one card: the task, each member's
state, open approvals, the merge queue. Each member opens and resumes like any session.

**Build.** A group record in gff-hab (task, lead, members, branch), taken by `session.create`
and the spawn verb. A worktree per writer. A merge lane in gff-hab that lands one change at a
time; a conflict goes back to the worker. The queue as Logbook tasks with `claim`. An optional
reviewer of another harness. The group card.

**Risks.** Usage multiplies by member count. In-pane teams already exist in the harnesses; do
not rebuild them. Our value is cross-harness, cross-host and visible.

**Size.** Finish the spawn verb, then a spec and four stages: record, merge lane, reviewer, card.

## 4. Capcom as an always-on service

### 4.1 Today

Capcom is a Claude Code session (or Codex, or opencode) in a holder pane. gff-hab writes a
folder with a manual and MCP settings, starts the harness with `--session-id`, and resumes it
with `--resume` (`capcom.md` 7.1, 7.6). It has a terminal view, a chat view read from the
transcript, and one standing conversation plus "new conversation". Context is the harness's: it
grows until the harness compacts, and the user picks when to start fresh.

### 4.2 The architecture

A Capcom engine: an agent loop on the Claude Agent SDK, run as its own process beside gff-hab.

| Part | Choice |
|---|---|
| Process | `gff-hab-capcom`, its own service job on the home host, so a gff-hab restart never drops a turn. The SDK is TypeScript or Python, so it cannot live inside the Go binary |
| Input | The SDK's streaming input mode: one long-lived loop per conversation; new messages queue, an interrupt stops a turn |
| Tools | The agent kit through `gff-hab mcp`, unchanged: read tools, `hand_off`, `start_session`, `logbook_write`, notices. No shell or file-write tools |
| Approvals | The SDK's `canUseTool` callback raises Flightdeck's own cards; the hab still runs confirmed starts (`capcom.md` 6) |
| Memory | Logbook. Durable facts and preferences become records; the engine reads the brief at every fresh start |
| Context | The engine manages it (4.3); the user never sees a context limit |
| Conversations | One standing thread by default; topic threads on request. Each thread maps to SDK sessions over time |
| Proactive | Optional: the engine subscribes to hab events (a blocked agent, a failed round) and may write first. It reports; it never steers a session |

The manual becomes the system prompt. Attached context (`capcom.md` 8.1) becomes a structured
part of the user message instead of pasted text. The read scope stays in the read tools.

### 4.3 Context with no user effort

- **Rolling summary.** After each turn the engine keeps a short summary of the thread so far.
- **Re-seed.** Past a size limit, or after a quiet hour, the engine ends the SDK session and
  starts a fresh one seeded with the summary, the Logbook brief and the last few messages. The
  user sees one continuous thread with a quiet divider, "earlier messages summarised".
- **Compaction** (the SDK's own) stays on as a safety net, with its `PreCompact` hook writing the
  summary first.
- **Memory writes** land in Logbook at once, per the owner's rule that memories are added by
  default with a setting to require approval. Tasks and findings stay proposals (`capcom.md` 6.2).
- **The summary is visible.** A tap on the divider shows what the engine kept, so a lost fact is
  findable and fixable.

### 4.4 What changes from `capcom.md`

| Today | Service |
|---|---|
| D1: an ordinary session in a pane, kind `capcom` | A session record of kind `capcom` with no pane; the engine owns it |
| `[capcom] harness` = claude, codex, opencode | `[capcom] engine` = claude (Agent SDK); codex (Codex SDK) later; the pane form kept one release as a fallback |
| Terminal view, plain-terminal resume | No terminal view. The AGENTS.md rule that a plain terminal can resume a session needs an exception, or the engine keeps SDK sessions where `claude --resume` finds them (to verify) |
| User starts "new conversation" | The engine re-seeds by itself; "new conversation" stays for a clean start |
| Busy while working | Messages queue in the stream; Capcom is never busy to the user |
| Home host assignment, read scope, confirmations, hand-off rules | Unchanged |

### 4.5 Billing and terms

- **Claude.** Anthropic's support article "Use the Claude Agent SDK with your Claude plan"
  (updated 2026-10-30) says: "when you're signed in with your Claude plan, the Claude Agent SDK
  and claude -p still draw from your subscription limits." Plans with API credits can also use a
  Console key.
- **The limit on products.** The Agent SDK overview says: "Anthropic does not allow third party
  developers to offer claude.ai login or rate limits for their products, including agents built
  on the Claude Agent SDK." Usage limits "assume ordinary, individual usage". The owner running
  Capcom for personal use on a personal plan fits. Flightdeck shipped to others must not collect or handle
  plan credentials: the engine uses an API key the user supplies, or the login the user's own
  Claude Code already has. The line between those is not defined by Anthropic; read it before
  release.
- **Codex.** The Codex SDK starts, continues and resumes local threads; the app-server streams
  events and approvals. A ChatGPT login works for local use, but "App-server authentication has
  never been permitted for commercial or hosted services."
- **Cost of always on.** An idle engine costs nothing. Proactive triage costs a turn per event,
  so batch events and use a small model for the first look.

### 4.6 How the app presents it

The Capcom view (`app.md` 4.10) becomes a messages thread:

- **Typing.** The engine reports turn start and end; the app shows typing dots, then the reply.
  Tool calls fold into one quiet line ("read 3 transcripts").
- **Read state.** "Delivered" when the hab stores the message; "Read" when the engine takes it
  into a turn.
- **Push.** A reply while the app is closed is an iOS communication notification with Capcom's
  name and avatar, with an inline reply. Proactive messages use the same path.
- **Approvals** stay cards, never plain text.

### 4.7 Risks

- The SDK changes fast, and a Node or Python runtime joins every home host.
- Terms can change; the pane form is the fallback.
- Self-managed context can drop a fact; the visible summary and Logbook limit the damage.
- Proactive messages can become noise; off by default.
- With the home host offline there is no Capcom; an always-on host is the advice.

### 4.8 Stages

1. Spec: the engine contract, events, the resume question.
2. Engine with one standing thread and `gff-hab mcp` tools, behind `[capcom] engine`.
3. App: typing, read state, communication push, inline reply.
4. Context manager: summary, re-seed, Logbook memory, the divider.
5. Proactive events, opt-in.
6. Codex SDK engine.

## 5. Optional: an iMessage front door

**"Insight."** No product by that name was found. The likely match is Instinct (Spear Street
Technology), medium confidence; it does not say how it reaches iMessage. Poke runs on Apple
Messages for Business, which sends typing events but shows as a business, not a contact.

**How the contact feel is made.** Hosted iMessage lines (Sendblue, Linq, LoopMessage) run real
Apple IDs on Apple hardware, so the agent is an ordinary contact with native typing bubbles and
read receipts. They are paid and see every message.

**Self-hosted.** A Mac signed in to a dedicated Apple ID, with a bridge that watches the
Messages database and sends through Messages automation (the open-source `imsg`, BlueBubbles).
No webhook and no public port. Typing bubbles and read receipts need a private-API helper with
System Integrity Protection off on that Mac. Only the owner's own handles are let in, and a text
can never answer an approval or a confirmation.

**SMS and Twilio are out:** the owner does not want SMS, and US delivery needs carrier
registration and a public webhook.

## 6. Recommendation

1. Build Capcom as an always-on service (section 4) next in Part 9. It answers most of the ask.
2. Finish the spawn verb with a reviewer alias; build session groups after it.
3. Revisit iMessage after the app's Capcom thread ships.

## 7. Decisions for the owner

| # | Question | Suggested default |
|---|---|---|
| 1 | Replace the pane Capcom with the service? | Yes; keep the pane form one release as a fallback |
| 2 | Can Capcom message you first? | Off at first; then only for things that need you, batched |
| 3 | One standing thread, or threads by topic? | One thread; topic threads on request |
| 4 | Show "earlier messages summarised"? | Yes, as a quiet divider you can open |
| 5 | Session groups: how many workers, and a reviewer? | Three workers plus a lead, worktree each; a reviewer of another harness, top finding only |
| 6 | iMessage bridge | Not now. If wanted later: a dedicated Apple ID on an always-on Mac, plain text first; typing and read receipts only if you accept SIP off on that Mac |

## Sources

- Hacker News item 50380658; https://kadhar.dev/blog/multi-agent-systems-from-scratch;
  https://www.cmu-agents.com/.
- https://code.claude.com/docs/en/agent-teams, `sub-agents`, `costs`, `agent-sdk/overview`,
  `legal-and-compliance`; https://support.claude.com/en/articles/15036540 (read 2026-11-06).
- https://learn.chatgpt.com/docs/codex-sdk, `app-server`, `auth`; https://opencode.ai/docs/agents/.
- https://www.anthropic.com/engineering/multi-agent-research-system;
  https://cognition.com/blog/dont-build-multi-agents; OpenHands, "Don't sleep on single-agent
  systems".
- Apple Messages for Business REST common specs; Sendblue and Linq docs; https://github.com/steipete/imsg;
  https://docs.bluebubbles.app; press coverage of Poke and Instinct, June to October 2026.
- `landscape.md` 1, `mesh.md` 1.3-1.4 for herdr and Paseo.
