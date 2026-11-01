# Landscape: Paseo, herdr and Rex set the bar

Status: study for review. Researched 2026-11-01.

This study answers two questions for each area of Flightdeck:

1. What is "good enough"? The bar is what the popular tools in this space do. The owner's rule:
   Flightdeck matches them and adds only cheap guards. It need not be stricter or better.
2. What do they do well that we have not planned?

It describes behaviour in our own words. It copies no code. Claims marked *(unverified)* come
from a secondary source or from reading code that we did not run.

## Contents

1. [Tools studied](#1-tools-studied)
2. [Area by area](#2-area-by-area)
3. [Shared foundations](#3-shared-foundations)
4. [Smart ideas we have not planned](#4-smart-ideas-we-have-not-planned)
5. [Where we are below the bar](#5-where-we-are-below-the-bar)
6. [Where we are stricter than all three](#6-where-we-are-stricter-than-all-three)
7. [What to watch](#7-what-to-watch)
8. [Sources](#8-sources)

---

## 1. Tools studied

| Tool | What it is | Version and date | Licence |
|---|---|---|---|
| **Paseo** (getpaseo/paseo, paseo.sh) | A self-hosted daemon that runs coding agents, with a phone, desktop and web app. TypeScript daemon, Expo app for iOS, Android and web, Electron desktop, a CLI, an optional relay | v0.14.0, 2026-10-29 | Apache-2.0 |
| **herdr TUI** (herdrdev/herdr) | A terminal workspace for agents: a server owns the PTYs, screen manifests detect agent state, SSH machines | v0.9.5, 2026-10-14 | Apache-2.0 |
| **herdr-core** (`hc`) and **herdr-app** | herdr's new stack: a holder plus a core, OSC 7501 state only, and a desktop app built from JS mods | hc 0.1.16 and app 0.1.11, both 2026-10-31 | Apache-2.0 |
| **Rex** (Superlogical) | One Go server per account owns PTYs, with libghostty screens, a native Mac app, tailnet transport, and Lua scripts | 0.1.0, build 1698 (snapshot) | Proprietary |

**Which Rex build.** Rex claims come from the public docs (`llms-full.txt`, read 2026-11-01) and
from build 1698, inspected read-only: `Info.plist`, entitlements and `strings` on the server
binary. We never launched it. We also fetched the preview channel's `latest.dmg`. It holds build
1696, which is older than 1698, so 1698 stays the evidence build. The newest snapshot is about
1728 (per the docs' appcast), so a few claims may be about 30 builds stale. Each Rex row that rests
on the binary says "(1698)".

**What Paseo is.** We verified this from its repository. It is not a terminal multiplexer. It
drives agents through their structured APIs: the Claude Agent SDK, `codex app-server`, ACP for
Copilot, Cursor and Kimi, the opencode server and Pi's RPC mode. Plain terminals exist beside the
agents. A paid cloud "Hub" (GitHub, Slack and Linear triggers) is optional.

---

## 2. Area by area

Columns: what each tool does; our current choice and where it is written; the bar we adopt; a
flag. **Below** means we are under the bar. **Above** means we go well past it. **Even** means we
match it.

### 2.1 Session hosting and survival

| | |
|---|---|
| Paseo | SDK agents live in the daemon and die when it restarts. On start it resumes each agent that was running and sends "Resume working". That is best effort and can lose or repeat a turn. Terminals live in a detached terminal worker that survives daemon restarts and upgrades. It runs from its own copied bundle, and the daemon adopts a worker one protocol version older and lets it drain. Nothing survives a reboot |
| herdr TUI | A server owns the PTYs, and detaching keeps them running. A server restart restores the layout plus each agent's native resume command (18 agents). Handoff over SCM_RIGHTS is experimental |
| hc | A holder plus a core. A core update keeps the terminals; a new holder build drains the old one. `--keep` brings a lost program back as a fresh shell and never reruns its task |
| Rex | One process owns the PTYs. An upgrade of the shared server ends its sessions. A personal server keeps running the old build until it stops. Saved sessions come back as layouts, without their programs |
| Ours | Holder and logic split, with drain and per-build binary copies (`hab-holder.md` 2.1, 4.3, 4.4). After a reboot: Resume, never automatic (`gff-client.md` 6.2) |
| Bar | Match hc: terminals outlive a logic restart and an upgrade |
| Flag | **Even** with hc, **above** Paseo and Rex. The holder is core to the product; keep it |

### 2.2 Multi-host

| | |
|---|---|
| Paseo | The app keeps several saved hosts. A daemon can save other hosts and route API calls to them, one hop only and with no reverse access. Agents get MCP tools to list hosts and start an agent on another host |
| herdr TUI | SSH machines with one combined agent list, a shared SSH ControlMaster, and reconnect backoff up to 2 minutes. A lost machine shows its cached state dimmed, and input stays off until a fresh screen arrives |
| hc | `hc machine add` over SSH, or a custom connection command. Addresses look like `box/t3` |
| Rex | A home server keeps the host list. Each host has several endpoints, tried in turn. `hosts check` notices when an address now reaches a different machine |
| Ours | Peer list from config over SSH stdio (`transport.md` 4.1, task-phkmrjaw). The client merges hosts (task-kbvoirca). Device ids are pinned |
| Bar | Match herdr: SSH, a cached view of an offline host, input off while stale |
| Flag | **Even**. Add herdr's stale-input rule (idea 4.9) |

### 2.3 Transport, auth and pairing from the phone

| | |
|---|---|
| Paseo | Three ways in: a relay the daemon dials out to, a direct connection (for example a tailnet address), or an SSH tunnel (desktop and CLI only). Pairing is a QR code or link that carries the daemon's public key in the URL fragment. The relay session is Noise NK. A legacy NaCl fallback can be replayed unless a flag turns it off. An optional password. Grant presets: owner, operator, viewer |
| herdr | SSH only. No phone app; its docs suggest any SSH client |
| Rex | The transport is the only trust: Unix peer credentials, or tailnet identity. Pairing by QR code through the tailnet. SSH keys are "planned". Build 1698 already contains `openssh` and `sshconfig` packages *(unverified purpose)* |
| Ours | SSH everywhere. The system sshd comes first; an embedded SSH server, off by default, comes later. QR pairing, Secure Enclave keys, pinned host keys, no relay (`transport.md` 3, 4) |
| Bar | Match Paseo's pairing: one QR scan and the phone works |
| Flag | **Above** on auth. **Below** Paseo on reach: with no relay, a phone off the LAN needs a system VPN. The owner chose that (no hub); relay stays a later carrier (`transport.md` 3, option E) |

### 2.4 Agent detection and state

| | |
|---|---|
| Paseo | SDK agents report state through provider events. For plain CLIs in a Paseo terminal, opt-in global hooks post to the daemon with a per-terminal token. No screen reading. An Esc or Ctrl-C typed by the user clears "working", because Claude sends no Stop on interrupt |
| herdr TUI | Process name plus screen manifests on the live bottom of the buffer. Hooks now report only the session identity; 0.9.5 dropped Codex turn hooks. `herdr agent explain` prints the rule that matched |
| hc | OSC 7501 only, stored as reported. No hooks: hook processes have no TTY, and hc has no report API (herdr-core#42) |
| Rex | OSC 7501 only (it wrote the spec). Its hook setup installs SessionStart only. The 1698 binary does contain Claude hook event names (`PermissionRequest`, `UserPromptSubmit`) in its hook editor *(unverified use)* |
| Ours | Sources ranked spawn > hook > OSC 7501 > screen, with precedence per turn and ordering rules. Manifests are a fallback only (`harness-adapters.md`, Detection, State event model) |
| Bar | Match Paseo: tokened hooks as the main source; manifests as herdr's fallback |
| Flag | **Even** to slightly above: we combine both. Add idea 4.3 (interrupt and prompt-mark resets) and idea 4.7 (`explain`) |

### 2.5 Input delivery and send gating

| | |
|---|---|
| Paseo | SDK agents take no PTY input, so they cannot race a dialog. A send during a turn steers, interrupts or queues (long-press queues, since 0.14). An unclear interrupt blocks new work. Terminals reported by hooks have no gate *(unverified, from code)* |
| herdr TUI | `agent prompt` refuses with `agent_blocked` and sends nothing. It respects live bracketed-paste mode and sends text, then a delayed Enter, as one ordered submission. Its docs admit that an unknown dialog reads as idle; bug herdr#5667 (2026-10-29) shows an open Claude permission dialog read as idle |
| hc | `send` refuses while blocked (exit 9) unless `--force`, and waits for the person to stop typing. Its docs: blocked "is not an access boundary" |
| Rex | `rex send` writes raw bytes. No gate in the docs |
| Ours | Deliver when idle or done; queue in the hab while working; refuse when blocked; bracketed paste, then Enter; wait 2 s after anyone types; read back and show "not delivered" (`harness-adapters.md`, Input delivery) |
| Bar | Match herdr and hc: refuse while blocked, ordered paste, person first |
| Flag | **Even**, plus the read-back check, which is a cheap guard. Keep it |

**Can terminal bytes spoof state?**

| Tool | Answer |
|---|---|
| Paseo | No for SDK agents. Hook state needs the terminal's token |
| herdr TUI | Yes in principle: output that draws a dialog shape can match a manifest *(inferred)* |
| hc, Rex | Yes: any program in the pane can print OSC 7501. The Rex spec says reports carry no authentication and suggests that terminals label the source, rate-limit side effects, and drop `working` and `blocked` at an OSC 133 `A` prompt mark |
| Ours | OSC 7501 ranks below hooks. An answer button needs a hook or server source and a matching request id (`harness-adapters.md` Rules 2, Permission answers). Spoofed bytes can at worst show a wrong hint |

### 2.6 Answering permissions from the phone

| | |
|---|---|
| Paseo | Structured, through the SDK's tool-permission callback. The app also answers AskUserQuestion with real choices and approves a plan at plan-mode exit. CLI: `paseo permit allow/deny` |
| herdr | Keystrokes only |
| Rex | None built in. A community board types into the pane |
| Ours | Structured where the harness offers it; keystrokes as a fallback with a read-back. The blocking Claude hook is a per-host setting, off by default (owner, 2026-11-01). Requests expire after 60 minutes, need an unlocked phone, and are withdrawn everywhere once answered (`app.md` 7; `harness-adapters.md`, Permission answers) |
| Bar | Match Paseo: structured answers for permissions, questions and plan approval |
| Flag | **Below** on plan approval, which no spec covers yet (idea 4.4). **Above** on expiry, Watch fit and withdrawal |

### 2.7 Chat view

| | |
|---|---|
| Paseo | A native timeline built from provider events and held in memory. The harness transcript stays the durable record, and a resumed agent rebuilds from it. `paseo import` adopts a session started elsewhere |
| herdr, Rex | None |
| Ours | The hab tails the native transcript and sends chat messages (`app.md` 4; `harness-adapters.md` Rule 4) |
| Bar | Match Paseo |
| Flag | **Even**. Adopting a session started outside Flightdeck is not specified (idea 4.10) |

### 2.8 Terminal view

| | |
|---|---|
| Paseo | xterm.js 6 with WebGL. The last client to interact sets the PTY size; passive renders never resize |
| herdr | libghostty, a UI per client, Kitty graphics |
| Rex | libghostty with a snapshot then raw bytes; an explicit size owner with a reason on each change |
| Ours | libghostty, snapshot then raw bytes, Rex's owner model with typing as a take (`hab-logic-api.md` 5, 6) |
| Bar | Match Rex |
| Flag | **Even** |

### 2.9 Push and the Apple Watch

| | |
|---|---|
| Paseo | Expo's push service. Routing by presence: a visible client focused on the agent gets nothing; a client active in the last 180 s gets an in-app notice only; otherwise a push. Never for errors. The body holds a clear-text preview of the last message or the tool input. No Watch support found |
| herdr | Toast, outer-terminal or system notification, plus sounds. No push |
| Rex | Local Mac notifications. Push is on its roadmap |
| Ours | Each user brings an APNs key; the hab pushes directly. Holds banners while a client watches (60 s). Collapses by request id. Banners written for the Watch. A setting hides commands (`app.md` 8, 16) |
| Bar | Match Paseo's presence routing |
| Flag | **Above** on privacy and the Watch. **Below** on setup: Paseo push works with no Apple account. Add Paseo's in-app tier (idea 4.1) |

### 2.10 Voice

| | |
|---|---|
| Paseo | Runs on the daemon host: Parakeet TDT 0.6B through sherpa-onnx with Silero voice detection, and Kokoro for speech; OpenAI as an option. Voice mode runs a hidden agent session (Claude Haiku by default) with voice tools over MCP. Dictation is on by default |
| herdr, Rex | None. herdr#5718: dictation arrives as a run of `a` characters |
| Ours | Parakeet on the device through Core ML; Apple voices; approvals by a fixed grammar, never a model (`voice.md`; `app.md` 13) |
| Bar | Match Paseo |
| Flag | **Even**. Both picked Parakeet. Paseo's host-side engine avoids a 440 MB download on the phone (idea 4.12) |

### 2.11 Images and files to agents

| | |
|---|---|
| Paseo | `paseo send --image`, attachments in the composer, binary transfer in 256 KiB chunks. A sandboxed HTML preview with a documented gap |
| herdr | The desktop app pastes files and screenshots with Cmd+V (0.1.10). The TUI bridges clipboard images in remote mode |
| Rex | None documented |
| Ours | Upload to the hab, a folder kept out of version control, markup, metadata stripped (`app.md` 4.4) |
| Bar | Match Paseo |
| Flag | **Even**, with markup as an extra. herdr's clipboard bridge suits `gff` (idea 4.11) |

### 2.12 Usage and limits

| | |
|---|---|
| Paseo | `paseo usage` from plugins, cached for 5 minutes. Claude: reads the login from the Keychain. Codex: reads the auth file and calls a private usage endpoint. Also Copilot, Cursor and others |
| herdr, Rex | None |
| Ours | Documented interfaces only: hook payloads, status-line input, rollout records. Never credential files, never private endpoints (`app.md` 14.1) |
| Bar | Show 5-hour and weekly windows per account, as Paseo does |
| Flag | **Stricter** (section 6). We may show nothing for an account whose harness exposes no usage |

### 2.13 Worktrees and projects

| | |
|---|---|
| Paseo | Workspaces of three kinds: a folder, a checkout, or a worktree it creates. A project file defines setup, teardown, scripts and services. An include file copies ignored files (such as `.env`) into a new worktree. Setup from a PR out of another repository waits for the user to confirm. A diff and PR panel per workspace |
| herdr | `herdr worktree create/open/remove`, with groups |
| Rex | None |
| Ours | Projects keyed by repository identity, with the latest folder of several worktrees (`hab-logic-api.md` 3.9, project fields). No worktree creation planned |
| Bar | Match herdr: create, open and remove a worktree for a project |
| Flag | **Below** (idea 4.5) |

### 2.14 Scheduling

| | |
|---|---|
| Paseo | Cron schedules start a fresh agent in a fresh workspace and archive it after. Heartbeats re-prompt an existing agent. A missed run fires once, then skips ahead *(inferred from code)* |
| herdr | None |
| Rex | Lua timers (`after`, `every`) |
| Ours | Part 13, rounds (plan-vtsaeon9; study task-226cqosp) |
| Bar | Match Paseo: both kinds; catch up once |
| Flag | **Below** for now: not built. Add heartbeats (idea 4.6) |

### 2.15 Install, update and service management

| | |
|---|---|
| Paseo | npm, a desktop app that runs its own daemon, or a container. The daemon supervises itself; no launchd or systemd units found. Stable and beta channels; a stable desktop release rolls out over 36 h |
| herdr | curl, Homebrew, mise, nix; `herdr update`. hc checks an update against a SHA-256 manifest over HTTPS, with no separate signature |
| Rex | Sparkle checks at launch and daily and cannot be turned off. The 1698 binary contains a `launchd` package that writes plists and boots jobs in the GUI domain |
| Ours | `gff-hab install` writes systemd and launchd jobs; per-build copies; signed Mac bundles; Homebrew (`hab-holder.md` 5; task-ygmxs1lu) |
| Bar | Match herdr: one-line install, self-update, sessions untouched |
| Flag | **Above**. Fine; the service jobs serve the holder |

### 2.16 macOS permissions

| | |
|---|---|
| Paseo | Notarized desktop with three entitlements (JIT, unsigned executable memory, audio input). No TCC guidance found |
| herdr | Starts the server as a one-shot launchd job in the GUI domain, so quitting the terminal app does not kill it. Moves to the per-user bootstrap port so logout does not break name lookups. Listens for the login window's undocumented notifications to save 15 to 20 s before shutdown. herdr-app 0.1.9 fixed updates that dropped the signature and reset TCC grants |
| Rex (1698) | The server is a signed helper bundle (`Rex Server.rex`) inside the app, with its own bundle id. Its entitlements cover camera, microphone, contacts, calendars, photos, location and Apple Events. Every usage string says a program running in a Rex terminal wants the access, and that Rex itself does not use it. A `tcc` package names Full Disk Access and Screen Recording |
| Ours | Signed helper bundles per process, the `gff setup` walk, per-build copies keep grants (`macos-identity.md` 3.2, 6, 7) |
| Bar | Match Rex (1698): a signed helper with child-facing usage strings |
| Flag | **Even**. We reached Rex's design on our own. Take its usage-string wording and herdr's early save (ideas 4.8, 4.13) |

### 2.17 Security model

| | |
|---|---|
| Paseo | The relay is untrusted. Any connected client is a fully trusted operator. Workspace paths are no boundary: the file preview reads anything the daemon can read. Plugins run unsandboxed. Any process of the same user can read the local credential |
| herdr | Socket mode 0700, no password, SSH for remote trust. Blocked is not an access boundary. herdr-app sends telemetry by default |
| Rex | The transport is the only trust; any client can do anything the server can |
| Ours | `transport.md` 2: refuse unknown keys, pin host keys, revoke lost phones everywhere, no hosted service in the path |
| Bar | Match the shared assumption: a paired client is the owner. A same-user process is trusted |
| Flag | **Above**: revocation, pinned ids, Secure Enclave keys. Keep it; it costs little once built |

### 2.18 Offline behaviour

| | |
|---|---|
| Paseo | The app keeps a replica cache, shows cached rows offline, and reconciles on reconnect. A creation journal with idempotency keys stops a reconnecting client from sending the first prompt twice |
| herdr | Dimmed cached state; input off until a fresh screen arrives |
| Rex | Host-list changes made offline wait and are sent later |
| Ours | Reconnect tokens, epoch and seq, resume by offset (`app.md` 10.2; `hab-logic-api.md` 3.5, 3.7). A write that timed out is never sent (`hab-logic-api.md` 3.8) |
| Bar | Match herdr and Paseo |
| Flag | **Even**. Add idempotency keys (idea 4.2) |

---

## 3. Shared foundations

The tools converge on a few standards and libraries. They split on one question: drive the agent
through its API, or host its TUI in a PTY.

| Area | Common source or standard | Paseo | herdr TUI | hc / app | Rex | Flightdeck |
|---|---|---|---|---|---|---|
| Agent state in the terminal | OSC 7501, the program status protocol. Rex wrote the draft (0.1 on 2026-09-28; rev 0.4 on 2026-10-10). Claude Code emits it from 2.1.295; Pi and iTerm2 support it | No | No | Only source | Author | One source, ranked below hooks |
| Harness hooks | Each harness's own hook docs (Claude Code, Codex, opencode plugins) | Opt-in, state | Identity only | None | SessionStart only | Main state source |
| Structured agent APIs | Claude Agent SDK, Codex app-server, ACP, opencode server, Pi RPC | All of them | No | No | No | Not while the TUI owns the session (`harness-adapters.md` Rule 1) |
| Screen detection | herdr's detection manifests (TOML, served from herdr.dev) | No | Author | No | No | Vendored as a fallback |
| Terminal emulation | libghostty-vt / xterm.js | xterm.js and xterm-headless | libghostty | libghostty | libghostty | libghostty |
| Shell integration | OSC 133 / OSC 633 | OSC 633 (VS Code style) | — | — | OSC 133 `A` resets status (spec) | OSC 133 (`hab-logic-api.md` 2.5) |
| Hosting split | A holder that outlives the logic | Terminal worker, drain N-1 | Experimental fd handoff | Holder plus core, drain | None | Holder plus logic, drain |
| Remote transport | SSH / tailnet identity / relay | Noise relay, direct, SSH tunnel | SSH | SSH | tsnet, tailnet identity | SSH |
| Speech | NVIDIA Parakeet TDT 0.6B; Kokoro | Both, on the host | — | — | — | Parakeet on device; Kokoro later |
| Push | APNs | Through Expo | — | — | — | Direct, each user's key |
| Agent manual | The binary prints or installs its own skill | Not found | — | `hc --skill` | `rex skill setup` | Agent kit (task-6ho9972u) |

**Lineage.** herdr-core took OSC 7501 from Rex; herdr#5069 cites the Rex spec. Paseo names none
of the others and uses no herdr data or libghostty. herdr's phone story rests on third-party
apps (Moshi, Heeler). None of the three uses ACP, the Agent SDK or Codex app-server, except
Paseo. hc tells agents to run Codex with `--no-daemon`, which the owner also chose
(`harness-adapters.md`, open decisions).

**Where we sit.** We follow the PTY camp (herdr, Rex) for hosting and the terminal. We follow
Paseo for state (tokened hooks) and the chat view (native transcripts). No tool combines both
yet.

---

## 4. Smart ideas we have not planned

Each idea names its source, what it does, why it helps, and a proposed home. The coordinator turns
accepted ideas into tasks.

| # | Idea | Source | What it does | Why it helps | Proposed home |
|---|---|---|---|---|---|
| 4.1 | Presence tiers for push | Paseo | A client focused on the agent gets nothing. A client active in the last 180 s gets an in-app notice. Otherwise, a push | Fewer pushes while the owner is at the Mac, with no lost approvals. We have only the first tier | task-oe7ioczy |
| 4.2 | Idempotency keys on create and send | Paseo | Each session start and chat send carries a client key; the hab drops a repeat | A phone that reconnects mid-send never sends a prompt twice or starts two sessions | new task: "hab API: idempotency keys on session.create and chat send" (Part 2) |
| 4.3 | State resets from the owner's interrupt and prompt marks | Paseo, Rex spec | Esc or Ctrl-C typed into a working Claude pane clears `working`. An OSC 133 `A` mark (a shell prompt is back) drops OSC 7501 `working` and `blocked` | Claude sends no Stop on interrupt, so a pane can stick in `working`. A harness that exits leaves stale OSC 7501 state | new task: "Adapters: clear working on owner interrupt and on a prompt mark" (Part 4) |
| 4.4 | Plan approval and questions as structured answers | Paseo | The app shows a plan and answers plan-mode exit; AskUserQuestion shows real choices | The phone answers the common "approve this plan?" stop, not only tool permissions | task-oe7ioczy, with the hook side in the Claude stage of plan-9icxnz2m |
| 4.5 | Worktrees per project | Paseo, herdr | Create, open and remove a worktree for a project. A project file names setup and teardown; an include list copies ignored files. Setup from a foreign PR waits for a yes | Parallel agents on one repo is the common pattern; both tools ship it | new task: "Projects: worktree create, open and remove, with setup scripts" |
| 4.6 | Heartbeats beside cron | Paseo | A schedule that re-prompts an existing session, beside one that starts a fresh agent | "Check the deploy every hour" in the same conversation keeps context | task-226cqosp |
| 4.7 | `explain` for detection | herdr | Print, per pane, the rule that matched, the region it read and the source that won | Turns "why is this pane idle?" from a guess into one command | new task: "gff-hab detect explain <pane>" (Part 4) |
| 4.8 | Usage strings written for child programs | Rex (1698) | Every TCC prompt says a program in a terminal wants the access, not the hab | The owner reads a Mac prompt correctly; no grant by confusion | task-7eh7h89k |
| 4.9 | Stale remote input off | herdr | A host that dropped shows cached state dimmed; input stays off until fresh state arrives | Nobody types into a screen that is minutes old | task-kbvoirca |
| 4.10 | Adopt a session started elsewhere | Paseo | Bring a harness session started in a plain terminal into Flightdeck by its resume id | An owner moving in keeps their running work | new task: "Adopt an outside session by resume id" (Part 4) |
| 4.11 | Clipboard images over SSH | herdr | `gff` forwards a pasted image to the remote pane's agent | Screenshots reach a remote agent from the desktop client too | task-g1uwtlsb, or new task in Part 5 |
| 4.12 | Speech on the hab | Paseo | The phone streams audio; the hab transcribes with Parakeet | No 440 MB model on the phone; old phones get the good engine | task-h011ghy9 (an option beside on-device) |
| 4.13 | Save before logout and restart | herdr | Watch the Mac's logout and restart signals and checkpoint 15 to 20 s before agents are killed | The reboot restore (`gff-client.md` 6.2) has fresh layouts and resume ids | new task: "hab: checkpoint on Mac logout and shutdown" (Part 1) |
| 4.14 | Agents start agents on other hosts | Paseo | An MCP tool lets an agent start a session on a named host | Capcom and teammates can place work on the right machine | task-6iwat66b, task-ph00sdci |

**Top five**, by value for the cost: 4.2 (idempotency keys), 4.3 (state resets), 4.4 (plan
approval), 4.1 (presence tiers), 4.5 (worktrees).

**Seen and not proposed.** Paseo's "Resume working" after a daemon restart; our panes survive, so
we need none. Paseo's idle reaper, which frees an idle SDK process; ours is a TUI the owner may
look at. Staged release rollout; it is for a larger user base than ours.

---

## 5. Where we are below the bar

| Area | Gap | Cheapest fix |
|---|---|---|
| Worktrees (2.13) | No create or remove | Idea 4.5 |
| Plan approval (2.6) | Only tool permissions and questions | Idea 4.4 |
| Scheduling (2.14) | Not built | Part 13 as planned, plus 4.6 |
| Push setup (2.9) | Each user needs an Apple developer key; Paseo needs none | Owner decision stands; the setup guide must be short (`app.md` 8.4) |
| Reach off the LAN (2.3) | No relay; a system VPN is needed | Owner decision stands; relay stays a later carrier |
| Usage coverage (2.12) | May show nothing where a harness exposes no data | Accept; show "no data" plainly |

---

## 6. Where we are stricter than all three

The owner can drop any of these to the bar. Each line says what the bar is.

| Ours | The bar |
|---|---|
| No relay and no hosted service in any path, push included (`transport.md` 2; `app.md` 8.1) | Paseo uses its relay and Expo's push service |
| Usage from documented interfaces only (`app.md` 14.1) | Paseo reads Keychain logins and calls a private endpoint |
| An approval expires after 60 minutes; Allow needs an unlocked device (`app.md` 7.4, 7.6) | Paseo: no expiry found *(unverified)*; herdr and Rex: no remote approval |
| Approve from a Watch banner only when the whole command fits (`app.md` 16.1) | Nobody else has Watch approvals except Moshi |
| An answer button needs a hook or server source and a matching request id; screen text never answers (`harness-adapters.md` Rule 2) | herdr answers by keys from screen state; Rex and hc accept unauthenticated OSC 7501 |
| Read back after each send and show "not delivered" (`harness-adapters.md`, Input delivery) | herdr and hc: refuse while blocked, no read-back |
| Hook config is printed; a later opt-in install command writes it after showing the diff (owner, 2026-11-01; `harness-adapters.md`, open decisions) | Paseo, herdr and Rex install hooks or skills themselves |
| Secure Enclave keys, pinned device ids, revocation that reaches every hab (`transport.md` 4, 6.6) | herdr: plain SSH; Rex: tailnet identity; Paseo: an optional password |
| Signed per-build bundles and a guided grant walk (`macos-identity.md`) | Rex (1698) has a signed helper; others none |

The first two came from owner decisions; changing them is the owner's call. The rest are cheap
once built. We suggest keeping all of them, and making the hook print-only rule an install
prompt rather than a manual step.

---

## 7. What to watch

The owner prefers to follow these tools rather than reinvent. Our rule stays: we take behaviour,
not code.

| Tool | Licence and reuse | Cadence | Where changes appear | Paths worth watching | Feeds our areas |
|---|---|---|---|---|---|
| Paseo | Apache-2.0. Code reuse would be legal with attribution and NOTICE | About weekly: 0.12.0 (10-19), 0.13.0 (10-24), 0.13.1 and 0.13.2 (10-26), 0.14.0 (10-29), betas between | GitHub releases, `CHANGELOG.md` (with an unreleased section), its Discord, the author's posts | `docs/terminal-worker.md`, `docs/terminal-activity.md`, `docs/agent-lifecycle.md`, `docs/permissions.md`, `docs/connected-hosts.md`, `SECURITY.md`, `public-docs/voice.md`, `public-docs/schedules.md`, `public-docs/worktrees.md`, `packages/server/src/server/agent-attention-policy.ts`, `packages/server/src/server/agent/providers/` | 2.1, 2.4, 2.6, 2.9, 2.10, 2.13, 2.14 |
| herdr TUI | Apache-2.0 | Weekly to fortnightly: 0.9.0 (09-07), 0.9.1 (09-16), 0.9.2 and 0.9.3 (09-29), 0.9.4 (10-13), 0.9.5 (10-14); fixes only since | GitHub releases, `CHANGELOG.md`, the website docs | `distribution/agent-detection/*.toml` and `index.toml` (the catalogue we vendor), `docs/next/` (agents, integrations, agent automation, connecting machines), `src/platform/macos/` | 2.2, 2.4, 2.5, 2.16 |
| herdr-core (`hc`) | Apache-2.0 | Near daily: 0.1.10 (10-23) to 0.1.16 (10-31); no GitHub releases | `CHANGELOG.md` | `schema/released.openrpc.json`, `docs/security.md`, `crates/herdr-core/src/SKILL.md`, the holder sources | 2.1, 2.5, 2.17 |
| herdr-app | Apache-2.0 | Seven releases from 10-23 to 10-31 | `CHANGELOG.md` | `defaults/AGENTS.md`, `docs/security.md` | 2.11, 2.16 |
| Rex | Proprietary. Behaviour from public docs only; inspect builds read-only | Snapshot builds several times a day; previews every few days (1408 on 10-23 to 1696 on 10-30) | No public changelog. Notes go to its Discord channel. The appcast carries none | `llms-full.txt`, the program status spec page, the preview and snapshot appcasts | 2.4, 2.8, 2.16, OSC 7501 itself |
| herdrm (third party) | PolyForm Noncommercial; do not reuse beyond behaviour | — | — | — | 2.8 (Swift libghostty client) |

**How to watch.** The manifest refresh already pulls herdr's catalogue (`gff-hab manifests
refresh`). The harness drift job (task-hpte2o08) can also diff the listed doc paths and changelogs
each week and open one issue per change that touches an area above. That keeps watching cheap and
tied to our areas. The OSC 7501 spec deserves its own check: Rex revises it, and three of our
sources depend on it.

---

## 8. Sources

Paseo (read 2026-11-01, repository HEAD 8efb21f):

- github.com/getpaseo/paseo: `README.md`, `LICENSE`, `CHANGELOG.md`, `SECURITY.md`,
  `docs/architecture.md`, `docs/agent-lifecycle.md`, `docs/terminal-worker.md`,
  `docs/terminal-activity.md`, `docs/connected-hosts.md`, `docs/permissions.md`,
  `docs/timeline-sync.md`, `docs/hub.md`, `public-docs/connectivity.md`, `public-docs/cli.md`,
  `public-docs/voice.md`, `public-docs/schedules.md`, `public-docs/worktrees.md`,
  `public-docs/updates.md`.
- Code read to confirm behaviour: `packages/server/src/server/agent-attention-policy.ts`,
  `packages/server/src/server/push/push-service.ts`, `packages/protocol/src/agent-attention-notification.ts`,
  `plugins/claude-usage-source/`, `plugins/codex-usage-source/`, `packages/desktop/build/entitlements.mac*.plist`,
  the schedule service.
- Issues: #8069, #7019, #6725, #7664.

herdr (read 2026-11-01):

- github.com/herdrdev/herdr: `CHANGELOG.md`, website docs (`session-state`, `agents`,
  `integrations`, `add-herdr-support`, `agent-automation`, `connecting-machines`,
  `configuration`, `how-to-work`, `persistence-remote`), `src/platform/macos/`,
  `src/server/handoff.rs`. Issues #4100, #4920, #5069, #5667, #5718.
- github.com/herdrdev/herdr-core: `CHANGELOG.md`, `docs/security.md`, `schema/README.md`,
  `crates/herdr-core/src/SKILL.md`, the status and ghostty-vt sources. Issues #42, #47, #52.
- github.com/herdrdev/herdr-app: `README.md`, `CHANGELOG.md`, `docs/security.md`.

Rex:

- superlogical.com/rex/docs (`llms-full.txt`), the program status spec page, the Linux upgrading
  page, the privacy page. Read 2026-11-01.
- Rex Beta build 1698: the app's and the server helper's `Info.plist`, `codesign` entitlements,
  `strings` of the server binary. Build 1696 from the preview `latest.dmg`, mounted read-only to
  check its build number only.

Ours: `AGENTS.md`; `docs/specs/hab-holder.md`, `hab-logic-api.md`, `harness-adapters.md`,
`transport.md`, `app.md`, `gff-client.md`, `voice.md`, `macos-identity.md`; earlier research notes
on herdr and Rex (2026-10-30).

Secondary: the claim that OSC 7501 is supported by Pi and iTerm2 comes from an openai/codex issue
(#58136) *(unverified)*.
