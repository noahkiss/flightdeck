# Swarm study: several agents on one task, an omni-agent, a messaging front door

A study, not a spec. The owner asked (2026-11-06) for optional multi-agent work, an
omni-agent and a texting agent. The owner's later ruling (master plan, decision l): a messaging front
door is an iMessage bridge, never SMS, acts like a normal contact, and is a separate, minor track.

## 1. What Flightdeck already has

| Piece | What multi-agent work gets from it | Where |
|---|---|---|
| Channels | Agent-to-agent text on any host, a disk outbox, dedup | `mesh.md` 6.1-6.3 |
| Deliver when idle | A message waits while the target works and is held while it is blocked; never forced | `mesh.md` 6.2 step 5 |
| `gff-hab wait` | A lead waits for a message or a worker's state change without reading a screen | `mesh.md` 6.4 |
| `hand_off` | Sends work to the agent already on a project, no tap | `capcom.md` 3.2, 6.5 |
| `start_session` | Starts a seeded session on any host | `capcom.md` 3.3, 6.1, 6.4 |
| Spawn verb (open) | Start a teammate in any harness by alias ("a codex reviewer"); result back through channels | task-6iwat66b |
| Capcom | A standing, folder-free session that reads every host; not a router or supervisor | `capcom.md` 2.1, 7 |
| Rounds | Scheduled `fresh`, `resume` and `fork` runs that never type into a session in use | `rounds.md` 5, 6 |
| Logbook | Tasks on every host, `claim`, hand-over to another project | `logbook.md` 10.2, 12.2 |
| Catalog, `suggest_host` | Every host's tools, projects and agents; ranked hosts, no scores | `mesh.md` 5, 7 |
| Worktrees | One checkout per writer | `hab-logic-api.md` 3.10 |

Missing: a record that says "these sessions are one task", one view of them, and a rule for
how their commits land.

## 2. Patterns from the sources

| Pattern | Pays off when | Costs |
|---|---|---|
| Subagent in the harness (Claude Code, Codex, opencode) | Read-heavy side work; only a summary returns | Own tokens, same limit, lossy hand-back. Flightdeck adds nothing |
| Lead and teammates with a shared task list (Claude Code agent teams, experimental) | Independent modules, competing hypotheses, research, review | About 7x one session's tokens in plan mode. Same-file edits overwrite. Teammates do not resume |
| Sessions on one branch, a shared queue, a commit script that serialises commits, another session reviewing each change (HN 50380658) | Many small, separable work items | One user's report: 4,454 reviews of 4,471 commits; four in five found something. Reviewers "always find something": cap them to the top item |
| Separate-context reviewer, ideally another harness | Nearly always; the best-supported pattern | One extra session per change |
| Parent waits on children across hosts (herdr `hc wait --children`; Paseo's start-an-agent MCP tool) | A lead hands out work and collects results | Same as above; neither tool adds a merge rule |
| Orchestrator and workers; blackboard; pipeline (kadhar.dev) | Fixed, well-split work | Bottlenecks, race-like bugs, delegation loops. Needs a depth cap and a budget |

The skeptics: Anthropic's research system spends about 15x a chat's tokens and says coding has
"fewer truly parallelizable tasks". Cognition and OpenHands: parallel agents make conflicting
implicit decisions; keep one thread or share full context.

Usage is wasted when each worker re-reads the repo, when workers edit the same files, when
leads poll, and when review loops have no cap. What helps: three workers, not five (Claude Code
doc), separate files, a cheaper worker model, one reviewer.

## 3. Options

### (a) A session group

**Behaviour.** The user, Capcom or an agent starts a group: a lead and a few workers, each an
ordinary session on any harness and host. The app and gff show one card: the task, each
member's state, open approvals, the merge queue. Each member opens and resumes like any session.

**Build.**

1. A group record in gff-hab: id, task, lead, members, branch. `session.create` and the spawn
   verb take a group id.
2. A worktree per writer. A merge lane in gff-hab lands one change at a time (the HN commit
   script as a hab call); a conflict goes back to the worker.
3. The queue is Logbook tasks with `claim`, so it survives restarts.
4. An optional reviewer, another harness by default, reads each merged change.
5. The group card in the app and gff.

**Risks.** Usage multiplies by member count. Same-file work still conflicts. Harness-native
teams already run in one pane; do not rebuild them. Our value is cross-harness, cross-host and
visible.

**Size.** Spec plus four stages: record and spawn link, merge lane, reviewer, card.

### (b) Capcom as the omni-agent

**Behaviour.** "Start a team on the parser fix" starts a group; "what happened overnight" reads
every host; a `resume` round sends Capcom a morning digest prompt.

**Build.** A `start_group` action under the same start confirmation; a digest round template.

**Risks.** An omni-agent that watches and steers sessions breaks the owner's "no router, no
supervisor" rulings. Capcom starts and reports; it does not steer. A long standing
conversation also costs more per turn.

**Size.** One stage after (a).

### (c) A messaging front door (iMessage)

**Behaviour.** The user texts a contact. The text reaches the standing Capcom conversation,
marked as from a text; the reply comes back as a text. With more work: a typing bubble while
Capcom works and a read receipt when the hab takes the message.

**Which product the owner means.** Most likely Instinct (Spear Street Technology; iMessage and
WhatsApp, proactive, invite-only since August 2026): medium-high confidence, as no "Insight" agent
exists. Poke (iMessage, SMS, Telegram) is the runner-up.

**Build.** A bridge on a Mac that watches the Messages database and sends through Messages
automation, as the open-source `imsg` and OpenClaw do; BlueBubbles is the alternative.

**Risks.** Typing bubbles and read receipts need a private-API helper with System Integrity
Protection (SIP) off, in both `imsg` and BlueBubbles. Apple may break that path in any update.
The Mac must stay awake and signed in. A text is a weak identity.

**Size.** Spec plus two stages: plain text bridge, then typing and receipts.

## 4. The front door on the owner's kind of setup

**Where it runs.** iMessage needs a Mac signed in to Messages; a Linux box cannot run it and a
sleeping laptop is a poor host. Use an always-on Mac with a dedicated Apple ID, so the bridge
never reads personal threads.

**Inbound with no public port.**

| Channel | How messages arrive | Public endpoint? | Owner approval |
|---|---|---|---|
| iMessage bridge | Local file watch on the Mac | None | Only for turning SIP off |
| Telegram bot | Long polling, outbound only | None | No |
| Signal (signal-cli) | Outbound WebSocket | None | No |
| Twilio SMS or RCS, polled | The hab polls the Messages API | None; seconds of lag | No |
| Twilio by webhook | Cloudflare Tunnel, Tailscale Funnel, ngrok or a relay server | Yes, every one of them | Yes |

Twilio is out under decision (l), and needs US 10DLC or toll-free registration first. If it
returns, poll; a tunnel is public exposure and needs the owner's approval.

**Auth by sender.** An allow list of the owner's handles in the hab config; any other sender is
dropped unread, so a stranger's text never becomes a prompt. A handle is weak proof, so a text
gets the lowest trust of any client.

**Never possible from a text:**

- Answering a permission request or an approval.
- `confirm.answer`: a start that asks still asks, in the app. A start elsewhere always asks when it
  comes from a text, whatever `confirm.start_elsewhere` says.
- Approving Logbook proposals; changing settings, rounds, peers or devices.
- Typing into any session but Capcom, or reading outside a host's read scope (`capcom.md` 5).

Allowed: talking to Capcom, read tools, and hand-offs to an agent already on a project (marked
as from a text in the hand-off card).

## 5. Recommendation

1. No swarm engine. Finish the spawn verb (task-6iwat66b) with a reviewer alias: the one
   pattern every source supports.
2. Build the session group (a) as a thin layer: a record, a worktree per writer, a merge lane,
   one card. Leave in-pane teams to the harnesses.
3. Give Capcom `start_group` and a digest round (b). Keep it a starter and reporter, not a
   supervisor.
4. Build the iMessage bridge (c) last, as plain text first, on an always-on Mac.

## 6. Decisions for the owner

| # | Question | Suggested default |
|---|---|---|
| 1 | Typing bubbles and read receipts need SIP off on the bridge Mac. Accept that? | No at first: plain texts. Later, SIP off only on a Mac used for nothing else |
| 2 | Which Apple ID does the bridge use? | A dedicated one, on an always-on Mac |
| 3 | Can a text hand off work to a running agent? | Yes, marked as from a text; starts and approvals still need the app |
| 4 | How do group workers share code? | A worktree each; the lead merges one at a time |
| 5 | Largest group size | Three workers plus a lead; more on request |
| 6 | Reviewer in every group? | On, another harness than the writer's, top finding only |

## Sources

- Hacker News item 50380658 and https://kadhar.dev/blog/multi-agent-systems-from-scratch;
  https://www.cmu-agents.com/ (CMU 11-768).
- https://code.claude.com/docs/en/agent-teams, `sub-agents`, `costs`; Codex subagents docs;
  https://opencode.ai/docs/agents/.
- https://www.anthropic.com/engineering/multi-agent-research-system;
  https://cognition.com/blog/dont-build-multi-agents; OpenHands, "Don't sleep on single-agent
  systems".
- https://github.com/steipete/imsg; OpenClaw iMessage docs; https://docs.bluebubbles.app;
  Twilio webhook security and 10DLC support pages; https://core.telegram.org/bots/api;
  https://tailscale.com/kb/1223/funnel.
- Instinct and Poke: press coverage and reviews, August to October 2026.
- `landscape.md` 1, `mesh.md` 1.3-1.4 for herdr and Paseo.
