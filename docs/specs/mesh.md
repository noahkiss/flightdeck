# Mesh: behaviour spec

Status: study for review. Part 3, study task. Written 2026-11-01.

This spec says how habs find, trust and talk to each other with no hub. It covers the peer list,
the hab-to-hab link, the catalog, channels (agent messages), suggestions, the optional always-on
host and transcripts. The later Part 3 stages build from it. It describes behaviour in our own
words and copies no code from the reference projects.

Rules this spec works within (owner decisions in the master plan):

- No hub, no relay, no leader election. One machine alone is a mesh of one.
- Peers come from Flightdeck's own config, written by the user. No default names a real machine.
- SSH first. Trust follows `transport.md`. A VPN or tailnet is only a network underneath.
- No session router. Agents read the catalog and suggest where work runs; the user confirms on a
  card, which a setting can turn off (owner, 2026-11-01). A running agent stays on its host.
- The bar is herdr, Paseo and Rex (`landscape.md`). We match them and add only cheap guards.

Sections that settle a question end with a **Decision** line and its reason. The owner decides
only user-visible questions; those are in [section 11](#11-owner-decisions).

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [Peers and trust](#3-peers-and-trust)
4. [Transports and the peer link](#4-transports-and-the-peer-link)
5. [Catalog](#5-catalog)
6. [Channels](#6-channels)
7. [Suggestions (no router)](#7-suggestions-no-router)
8. [Optional always-on host](#8-optional-always-on-host)
9. [Transcripts](#9-transcripts)
10. [Edge cases](#10-edge-cases)
11. [Owner decisions](#11-owner-decisions)
12. [API additions](#12-api-additions)
13. [Sources](#13-sources)

---

## 1. References

### 1.1 Rex: a home server and hosts

- Each device follows one home server, set in a home file or an environment variable. The home
  keeps the shared host list.
- `rex hosts add LABEL ENDPOINT...` stores several endpoints per host. The client tries them in
  turn. A Unix socket endpoint works only where its path exists, for example the local end of an
  SSH forward.
- A host-list change made while the home is away waits locally and is sent later. On a label
  clash, the local machine's own hosts win.
- `rex hosts check` reports each host as reachable, not reachable from here, or down. It notices
  when an address now leads to a different machine.
- `-S <label|address>` picks the server per command. Scripts reach other hosts by label. The
  client connects to each host itself; no hub relays traffic.
- Trust comes from the transport only: Unix peer credentials or the tailnet's identity. A QR code
  pairs a device through the tailnet. The welcome may list other carriers.

What we take: a label plus several endpoints per host, tried in turn; a check that notices a
different machine behind an old address; the client connects to each host itself. What we leave:
one home that owns the list (a single point we do not want), and tailnet identity as trust.

### 1.2 herdr TUI: SSH machines

- A user adds SSH machines. herdr shows one combined agent list over all of them.
- It shares one SSH ControlMaster per machine and reconnects with backoff up to 2 minutes.
- A lost machine shows its cached state dimmed. Input stays off until a fresh screen arrives.

What we take: SSH as the transport, a cached and dimmed view of an offline host, input off while
the view is stale (`landscape.md` 2.2, idea 4.9).

### 1.3 herdr-core: `hc machine add` and `hc connect`

- `hc machine add <ssh target> --name box` registers a machine. The target is anything SSH takes,
  a host alias from the user's SSH config included. `--via '<command>'` takes any command that
  carries stdin and stdout unchanged and asks no questions.
- If the other side lacks `hc` or has a build too old to talk to, `add` asks before copying `hc`
  there. `hc machine update` copies a new build and restarts the remote core; its terminals keep
  running.
- `hc connect` speaks the protocol over stdio, so SSH reaches a remote core with no listener.
- The local core keeps the connection and merges terminal lists and events into one local stream.
  The remote core owns its programs, history and files.
- The local name (`box`) is the user's name for that connection, not the remote host name.
  Addresses look like `box/t3`. `hc machine rename` changes the local name; old addresses stop
  working.
- A lost connection is retried by the core. `hc machine ls` shows connection state and the last
  seen version. `hc machine rm` forgets the machine and leaves its terminals running.
- `hc send <terminal> <text>` pastes the text and presses Enter. It waits for a person to pause
  typing. `--if idle` types only in that state; `--force` types even while the program is blocked.
  `hc wait --children` waits for child agents, and a message typed to the waiting agent also ends
  the wait. There is no outbox: a send to an unreachable machine fails.

What we take: the local name as an alias, not an identity; `label/name` addresses; SSH stdio with
a custom-command escape hatch; "forget" leaves the remote work running; send waits for the person.
What we leave: one machine relaying the others' events into its own stream (our clients connect
to each hab, `hab-logic-api.md` 7.9).

### 1.4 Paseo: saved hosts and one hop

- The app keeps several saved hosts. A daemon can save other hosts and route API calls to them,
  one hop only, with no reverse access.
- Agents get MCP tools to list hosts and to start an agent on another host. No confirmation step
  was found beyond the harness's own tool permission.

What we take: agents can see other hosts and start work there through a tool (`landscape.md`
idea 4.14). What we leave: forwarding calls through a daemon. What we add: a confirmation card
before a start on another host, which the user can turn off (section 11).

### 1.5 QR pairing

Rex and Paseo pair a phone with one QR scan. `transport.md` 6.1 sets our flow: the hab shows an
offer with its host-key fingerprint, its endpoints and a one-time token; the owner confirms on the
hab. One QR per hab; introduction between habs is off by default (`transport.md` 6.2). The mesh
adds nothing to pairing. It only lets a paired client learn the other habs' device ids and
endpoints, which `transport.md` 6.2 already allows.

### 1.6 Ideas from the research, and what changed

The early research picked a static peer list from a personal machine table, tailnet identity as
trust, an owner host for scheduled jobs, and a router. Owner decisions since then replaced each of
these: Flightdeck's own config, SSH keys and pinned device ids, an optional always-on host, and
suggestions instead of a router. We keep the research's other picks: direct delivery with a
per-host outbox, a host-owned catalog pulled with `since`, detected versus declared facts (from
Nomad), and transcripts left in place (`r2-mesh.md` Q2, Q3, Q6).

---

## 2. Model

| Thing | What it is | Identity |
|---|---|---|
| hab | gff-hab on one host | Its device id: the fingerprint of its own host key (`transport.md` 5) |
| peer | Another hab this hab knows from its peer list | The pinned device id; the label is local |
| label | The user's name for a peer, in this hab's peer list | Local to one hab, like herdr's `box` |
| host id | The name a hab gives itself in the welcome (`-host-id`, else the system host name) | Display only. Two hosts can share one |
| peer link | A control connection from one hab to another, with client kind `hab` | One per pair at most, see 4.2 |
| catalog | What a hab publishes about its host: facts, projects, agents | Owned by that hab; keyed by device id |
| channel message | Text from one agent to another, on any host | A random 128-bit message id |
| outbox | Per-target queue of messages and control items a hab could not hand over yet | Per target device id |

- **The mesh keys hosts by device id.** The host id defaults to the system host name, so two
  laptops can both be called the same thing. Labels are local. Only the device id is the same
  everywhere.
- **Each hab owns its own data.** No hab stores or serves another hab's sessions. It keeps only
  copies of other habs' catalogs, and messages in transit.
- **Clients still connect to every hab.** The mesh does not change `hab-logic-api.md` 7.9. A
  client learns hosts from a hab's peer list and catalog, then opens its own connection to each.

Decision: device id as the mesh identity, labels local. Reason: host ids collide by default, and
herdr shows that a local alias is enough for addressing.

---

## 3. Peers and trust

### 3.1 The peer list

- **Where.** A file of its own: `$GFF_HAB_PEERS`, else `gff/peers.toml` beside the hab's config
  file (the user config directory on macOS, `$XDG_CONFIG_HOME` or `~/.config` elsewhere). Per-host
  settings (declared catalog facts, the always-on mark) stay in the hab's own config file.
- **Entry.** A label, one or more endpoints, and an optional pinned device id. An endpoint is the
  local socket path, an SSH destination (anything `ssh` takes, an alias from the user's SSH config
  included), or a custom command that carries stdio, as herdr's `--via`.
- **One file for every host.** A hab skips the entry whose pinned device id is its own, and an
  entry whose endpoint answers with its own device id. So a user can keep one peer file and copy
  it to every machine by any means they like.
- **Written by the user.** `gff-hab peers add <label> <endpoint>...`, `peers rm`, `peers rename`
  and `peers accept` edit the file and keep comments. The user may also edit it by hand. gff-hab
  reloads it on change.
- **Bad lines.** A duplicate label, an empty endpoint list, or an unreadable file is reported by
  line. The good entries still load.
- **No discovery.** No mDNS, no tailnet API, no scan. A hab may *show* the peers a peer knows (by
  device id and label) as "known to <label>", so a client can offer to add them. It never adds or
  trusts one on its own.

Decision: a separate peer file, usable unchanged on every host. Reason: the user writes it once;
Rex needs a home server for the same result, and we have none.

Decision: no automatic discovery. Reason: herdr and hc add machines by hand; discovery would
need a network service the owner ruled out.

### 3.2 Trust

Trust is exactly `transport.md`. The mesh adds no new credential.

| Path | Who the dialed hab trusts | Proof |
|---|---|---|
| Local socket | The same user | Socket permissions and peer credentials |
| SSH to the system sshd, `gff-hab connect` | Whoever logged in as the user | The user's SSH keys. Any process that reaches the socket is already the owner (`transport.md` 2) |
| The embedded SSH server, later | A paired hab key | Public-key auth against gff-hab's own authorized-keys file (`transport.md` 4.2, 6.3) |

- **The dialer pins the dialed hab.** The welcome carries `device_id`. The first link pins it in
  the peer file. A different id later is refused until `gff-hab peers accept <label>`, which shows
  both ids (`transport.md` 6.3).
- **The dialed hab learns the dialer.** A hab that dials sends its own device id in the hello
  (`peer` field, 12). Over the system sshd that is a claim, not a proof; that is enough, because
  the caller is already the owner. Over the embedded server the key proves it.
- **Unknown dialers.** A hab that receives a link from a device id not in its own peer list still
  serves it (the caller is the owner) and shows it as "linked from <host id>, not in your peer
  list" with an add action.
- **Revocation** travels as a control item in the outbox (6.4), as `transport.md` 6.6 says.

Decision: no peer-level allowlist beyond SSH. Reason: herdr, hc and Rex all treat a caller who
got through the transport as the owner (`landscape.md` 2.17).

---

## 4. Transports and the peer link

### 4.1 Transports

- **First build:** `ssh <endpoint> gff-hab connect` over the system sshd, the same transport as
  `gff` (`gff-client.md` 11.1). The hab uses `internal/habconn`: endpoints tried in order with a
  short stagger, connection sharing, reconnect with backoff and jitter up to 30 s, an immediate
  retry on wake or network change.
- **Later:** the embedded gff-hab SSH server, after the owner enables it (`transport.md` 4.2,
  4.5). The system sshd stays the bootstrap and the fallback.
- **A tailnet or VPN** is only a route to an endpoint. Online and offline come from the link, not
  from a VPN's status.
- **A peer link has no data connections.** Habs exchange control traffic only: catalog, channel
  messages, control items. Pane bytes go from a hab to a client, never hab to hab.

### 4.2 The peer link

A peer link is an ordinary control connection with `client.kind` `hab`. It subscribes to nothing
by default; it makes calls.

- **The dialer drives both directions.** A laptop can usually reach a dev box, but not the other
  way. So whichever hab holds a link does all the calls for both directions over it: it pulls the
  other's catalog and mail, and pushes its own catalog changes and mail (12).
- **One link per pair is enough.** If both habs can dial, both may hold a link; that is harmless,
  because every exchange is idempotent (generations and message ids). A hab that already holds a
  healthy link it dialed does not need the reverse one, but it does not tear down an incoming one.
- **Wake-ups.** Over a link, the dialed hab sends a small `mesh.changed` notification when its
  catalog generation moves or it has mail for the dialer. The dialer then pulls. A missed
  notification costs at most one poll period (60 s).
- **Capability.** A hab that supports the link says `mesh` in its capabilities. A peer without it
  shows "update needed" and is used for nothing else.

Decision: the dialer drives both directions. Reason: a sleeping laptop behind NAT must still
send and receive, and a pull model needs no inbound path to it.

### 4.3 Online and offline

| State | Meaning |
|---|---|
| online | A link is up, either direction, and pings answer |
| connecting | The hab is trying endpoints |
| offline | No link; last seen at a time. Normal for a laptop that sleeps |
| refused | The endpoint answered with a different device id, or with no hab |
| update needed | The peer has no `mesh` capability, or no revision fits |

`gff-hab peers` lists label, state, the endpoint in use, the short device id and last seen.

---

## 5. Catalog

### 5.1 What a hab publishes

| Section | Fields | Source |
|---|---|---|
| host | device id, host id, OS and version, arch, cores, memory, GPU where cheap, on battery or not | Detected |
| toolchains | Name and version of what is on PATH: git, Go, Node, Python, Rust, Docker or Podman, Xcode and its simulators on a Mac | Detected at start and every hour |
| harnesses | Installed harnesses and versions (from `internal/detect`), whether hooks are set up | Detected |
| capabilities | Short strings: detected ones (`macos`, `linux`, `xcode`, `docker`, `gpu`) plus the user's own (`signing`, `printer`, any word) | Detected plus declared |
| services | Name, URL, optional health URL | Declared |
| projects | Identity (remote, else root commit), name, the folders on this host, branch, head, dirty or clean, last commit time, the first line of `AGENTS.md` or `README` | Scanned from configured project roots and from the structure model |
| agents | Session name, harness, project, state, since | The structure model and agent state |
| always on | true or absent | Declared (8) |
| endpoints | The embedded server's address, when it is on | gff-hab |
| generation | A per-hab counter, raised on every change; saved across restarts | gff-hab |

- **Detected versus declared.** Declared facts add to detected ones. A declared capability with
  the same word as a detected one shows once, marked with both sources. A declared fact never
  hides a detected one.
- **Declared facts** live in the hab's own config file, in a `catalog` section: extra
  capabilities, services and the always-on mark. They are host-specific, so they are not in the
  shared peer file.
- **Project roots** are folders the user names in the config. With none set, the catalog lists
  only projects the structure model already knows. A scan follows no symbolic links and stops at a
  depth of 3. Git reads use the `internal/gitstatus` rules: read-only, clean environment, time
  limit. Dirty state comes from `git status --porcelain` with untracked files counted, run at most
  every 5 minutes per project, plus on a hook event in that project.
- **Nothing secret.** The catalog carries no environment, no file contents beyond one summary
  line, no tokens. A service URL is what the user wrote; Flightdeck does not add credentials to it.

Decision: section-level facts with one generation counter. Reason: it is the smallest shape that
supports `since` pulls; Nomad's attribute and meta split gives detected versus declared.

### 5.2 Pull and merge

1. `catalog.get {since}` returns the sections that changed after generation `since`, each whole,
   plus the new generation. `since` 0 returns everything. An unchanged catalog returns no sections.
2. The puller keeps the last copy per device id, with `seen_at`. A new epoch of the peer does not
   reset the copy; the generation is saved across restarts. A generation lower than the one held
   (a hab with a lost state directory) means "replace everything".
3. **Each hab is authoritative for its own sections.** There are no conflicts to merge.
4. **Offline peers stay, marked stale,** with their age. They are dropped only when the user
   removes the peer from the list.
5. **Catalogs travel one hop only.** A hab serves its own catalog and the copies it pulled from its
   direct peers, each marked with who it came from and its age. A hab never re-serves a copy it
   got second-hand.
6. **The merged view** (`catalog.list`) shows one project with several locations when its identity
   matches on several hosts. It is what clients and agents read.

Decision: one hop. Reason: Paseo routes one hop; deeper gossip needs loop and age rules we do not
need for a handful of machines.

### 5.3 Who reads it

- **Clients** read `catalog.list` from any hab to show hosts, projects with their locations, and
  hosts they could add. Each host's live state still comes from the client's own connection to it.
- **Agents** read it through the agent kit (Part 4, task-6ho9972u): `gff hosts`, `gff hosts
  <label>`, `gff projects`, the same as MCP tools, and the printed skill (7).

---

## 6. Channels

Channels carry text from one agent to another, on any host. Part 4 (agent kit) owns the local
surface: the env contract, the CLI and MCP verbs, the envelope text, and delivery into the PTY.
Part 3 owns names across hosts, direct delivery, the outbox and dedup.

### 6.1 Names and addresses

- **An agent's name** is its session name (`hab-logic-api.md` 2.2). It is unique on its host.
- **Across hosts,** naming an agent checks the name against every peer catalog the hab holds,
  stale ones included, and refuses a clash with "name in use on <label>". Offline peers may still
  hold a clash this check cannot see.
- **An address** is `name` or `label/name`, as in hc. `label` is the sender hab's label for the
  target host. `label/name` always works. A bare `name` resolves through the catalog copies; if it
  matches on more than one host, the send is refused and lists the full addresses.
- **Resolution order:** the local host, then online peers, then stale copies. A name found only in
  a stale copy is still a valid target; the message waits in the outbox (6.3).

Decision: refuse a clash at naming time, and accept `label/name` always. Reason: the stage
acceptance asks for the cheap check; hc shows that full addresses settle what slips through.

### 6.2 Send and deliver

1. The agent runs `gff send <address> <text>` (or the MCP tool). The CLI reaches its own hab
   through `GFF_HAB` (`harness-adapters.md`, environment contract). The hab knows the sender pane
   from the token.
2. The sender hab makes a message: id (random, 128 bits), sender (host device id, session name),
   target (device id, session name), text, created at, and an optional `reply_to` id.
3. **Target on this host:** hand it to the local input queue (step 5).
4. **Target on a peer:** if a link is up, push it now (`mail.push`). Otherwise put it in the
   outbox for that device id (6.3). The send returns at once with the message id and the state
   `queued` or `handed`.
5. **The target hab delivers** through Part 4's input queue, under `harness-adapters.md`, Input
   delivery: deliver on `idle` or `done`; wait while `working`; hold while the person typed in the
   last two seconds. Agent messages add two rules:
   - **While `blocked`:** hold, never force. The sender is not watching; a forced send could
     answer a dialog. herdr also refuses blocked unless forced.
   - **While `none` or `exited`:** refuse back to the sender with "agent not running".
6. **Envelope.** The text arrives wrapped in a short envelope: it says the message is from an
   agent, names the sender address (`label/name` as the target's hab sees it, else the host id),
   and gives the reply command. Part 4 owns the exact text. The chat view marks it as an agent
   message (`app.md` 4.3).
7. **Read back.** Part 4's read-back applies. A message the adapter reports "not delivered" is
   not retried on its own; its state says so.

| Message state | Meaning |
|---|---|
| `queued` | In the sender's outbox |
| `handed` | The target hab accepted it; it waits in the agent's input queue |
| `delivered` | Written into the target PTY and read back |
| `not_delivered` | Written, but no sign the agent took it |
| `refused` | The target is not running, or does not exist on that host |
| `expired` | Not handed over before the time limit (6.3) |

The sender hab keeps each message's state for 7 days. `gff send --wait` waits for `delivered` or
a final state. Status changes for a message travel back to the sender hab as control items.

Decision: Part 4's input queue owns deliver-when-idle; channels call it. Reason: one queue per
pane means the person's own input and agent messages cannot interleave mid-message.

Decision: hold while blocked. Reason: an asynchronous sender cannot judge a dialog; the bar
(herdr) types into a blocked program only with an explicit force, which we do not expose across
hosts.

### 6.3 Outbox and dedup

- **One outbox per target device id,** in the sender hab's state directory, saved to disk, so a
  restart loses nothing.
- **Handover.** When a link to the target comes up, the hab that holds the link pushes or pulls
  the outbox in order (4.2). The receiver answers with the ids it accepted; the sender removes
  them.
- **Dedup.** The receiver keeps every accepted message id for 7 days and drops a repeat, answering
  as if accepted. So a retry after a lost answer never delivers twice.
- **Order.** Messages from one sender to one target are handed over in send order. No order is
  promised between different senders.
- **Limits.** A message is at most 64 KiB of text. An outbox holds at most 1000 items per target;
  a send beyond that is refused with "outbox full for <label>". An item expires after 7 days and
  its sender sees `expired`.
- **Control items** use the same outbox: message state updates and device revocations
  (`transport.md` 6.6). They have no size or expiry limit beyond the item cap, and revocations are
  never dropped.
- **No forwarding.** A hab only hands mail to the target itself. If the sender hab has no peer
  entry for the target's host, the send is refused with "add <host id> as a peer".

Decision: per-target outbox on disk with 7-day ids and expiry. Reason: a laptop may sleep over a
weekend; a week covers it without unbounded growth. hc has no outbox, so this is above the bar
only in a way the plan already asked for.

### 6.4 `gff wait`

- `gff wait` returns when a channel message reaches the calling agent, or when a named agent's
  state changes (`--for idle|done|blocked|exit`), or on timeout (default 180 s, as hc).
- It is a hab call, so no client polls a screen (`gff-client.md` 15.10).

---

## 7. Suggestions (no router)

- **Agents see the catalog** through the agent kit: hosts with OS, toolchains, harnesses,
  capabilities, services, projects with their locations, and online or stale.
- **The printed skill tells agents how to use it:** when a task needs something this host lacks
  (Xcode on a Linux box, a GPU, a project not checked out here), name the hosts that fit, say why
  in one line each, and ask the user before acting.
- **Acting needs the user's yes.** Setting up a project on another host (task-j600w23a) and
  starting a session there (`session.create` on that hab) are separate agent kit actions. Each
  raises a Flightdeck confirmation card before it runs: the same card, expiry and
  first-answer-wins rules as Capcom's (`capcom.md` 6.1). The harness's own tool-permission prompt
  still applies to the command. The setting `confirm.start_elsewhere` (default on) turns the card
  off; the action then runs at once and its card shows as a record (owner, 2026-11-01;
  section 11).
- **No scoring, no automatic choice.** Flightdeck ranks nothing. `gff hosts --has <capability>`
  filters; the agent reasons.
- **A running agent stays on its host.** Nothing moves a session.
- **Set up elsewhere** (task-j600w23a): the target hab clones or fetches by the project's remote,
  checks out a ref, runs the project's declared bootstrap, and its catalog then shows the project
  at that folder. Credentials come from the target host's own git setup.

Decision: filters, no scores. Reason: Paseo gives agents a host list and nothing more; a score
would be a router in disguise.

---

## 8. Optional always-on host

- **The mark.** `always_on = true` in a hab's own config, `catalog` section. The catalog
  publishes it. Any number of habs may carry it, or none.
- **What it adds.** Part 12 (Logbook) extras that need a machine that stays up: backups, a git
  export, vector search. Part 12 decides which extras run on every marked hab and which run on
  one. Transcript backups may also target it later (9.2).
- **Nothing requires one.** Core features (sessions, chat, approvals, channels, catalog, Logbook
  replicas) keep working with no always-on hab and while one is down. A feature that needs one
  says "no always-on host" and does nothing else.
- **No election.** The mark is the user's choice. No hab promotes itself.
- **Down alert.** When a marked hab has been offline for 30 minutes (configurable), one Flightdeck
  notification goes out through the Part 6 notification path:
  - Every online hab that has the marked hab as a peer checks the catalogs it holds. Only the one
    with the lowest device id among online habs that list the marked hab as a peer raises it.
  - A hab counts the 30 minutes only while it has been online itself the whole time, so a laptop
    that just woke does not alert about a host it could not see while asleep.
  - One notification per outage. Back online clears it.

Decision: the lowest device id raises the alert. Reason: it gives one notification with no
election and no shared state; during a network split two habs may each alert, which is
acceptable.

---

## 9. Transcripts

### 9.1 In place

- **Transcripts stay where each harness writes them** (AGENTS.md, native data stays native).
  Resuming from a plain terminal keeps working.
- **Each hab indexes its own** with the Part 4 readers: harness, session id, project identity,
  folder, start, end, title, path. The index is Flightdeck's own and is shared with Capcom (Part 9).
- **Peers read the index,** not the files: `transcript.index` over a link or a client connection.
  A client reads a transcript's content from the hab that holds it (`chat.page`).
- **No moving sessions.** The first build never copies a transcript to another host to resume it
  there. A running agent stays on its host; an ended one resumes on its own host.

### 9.2 Backup

- **One-way copy** of each harness's transcript folders to a target in the hab's config: a local
  path (a mounted share counts), one folder per host by device id. A peer marked always on is a
  later target.
- **Only changed files** are copied, by size and modification time. Nothing is deleted at the
  target when the source deletes; a separate retention setting prunes.
- **Laptop-safe.** The job runs at most hourly. After a sleep it runs once, not once per missed
  interval. A failed run retries with backoff when the network returns.
- **Off by default.** No target, no backup. Credentials for any remote target come from the
  environment or the user's secret store, never a file in the repo.

Decision: a path target first. Reason: a mounted share or a synced folder covers the common case
with the least code; a peer target needs the link to carry bulk data, which it does not yet.

---

## 10. Edge cases

| Case | Behaviour |
|---|---|
| A laptop sleeps | Its links drop; peers show it offline, its catalog stale, never removed. Mail for it waits in their outboxes. On wake, `habconn` retries at once; the laptop's links pull its mail and push its own. Nothing alerts unless it is marked always on |
| A laptop wakes on a network that drops SSH | Every endpoint fails fast; the peer shows "network blocks SSH" (`gff-client.md` 11.6). Outboxes keep their mail |
| An address now leads to another machine | The welcome's device id differs from the pin. The link is refused and the peer shows "different hab at this address", with both ids, until `gff-hab peers accept`. Mail is never handed to the new machine |
| An address leads to a machine with no hab | The peer shows "no hab at <endpoint>" with the install hint |
| A hab was rebuilt (new key) | As a different machine: refused until accepted (`transport.md` 7.4). After accept, its old outbox items are handed over; ids it already accepted were lost with its disk, so a duplicate is possible once |
| Two hosts with the same host id | Fine: the mesh keys by device id. Rows show the local label, and the host id only when no label exists |
| Two peers with the same label in one file | The second is refused at load, by line |
| One state directory copied to a second host (a cloned VM) | Both claim one device id. A hab that sees one device id on two live links with different epochs refuses the second and shows "two hosts share a device id"; the fix is to remove the copied key on one host so it makes a new one |
| The peer file lists the hab itself | Skipped (3.1) |
| Two agents named the same on two hosts | A bare name is refused with the full addresses; `label/name` works |
| A name clash made while a peer was offline | Found when its catalog arrives; both agents keep their names; the status board shows the clash once |
| A message to an agent that exits before delivery | `refused` with "agent not running"; the text is not kept |
| A message to a working agent for hours | It waits in the target's input queue; its state stays `handed`. It expires with the outbox limit only if never handed over |
| A sender agent that exits | Its messages still deliver; replies to it are refused at its host |
| The same message pushed twice after a lost answer | The receiver's dedup drops the repeat |
| A peer with an older hab | No `mesh` capability: "update needed"; clients can still connect to it for sessions |
| A peer removed from the list | Its link closes, its catalog copy is dropped, its outbox items expire as `refused` |
| An agent starts a session on another host | With `confirm.start_elsewhere` on, a card waits for the user; nothing runs until confirm, and an expired card never runs. With it off, the start runs at once and the card shows as a record |
| An agent messages an agent already on a project | Sent at once through channels, no card (owner, 2026-11-01). Only starts and set-ups take a confirmation |
| Clocks differ between hosts | The mesh orders by generations and message ids, never by wall time. Times shown come from the hab that made them |
| A one-way network (laptop reaches the dev box, not back) | The laptop holds the only link and drives both directions (4.2) |
| A hab restarts | Outboxes, the dedup list and the catalog generation are on disk. Links reconnect with the new epoch |

---

## 11. Owner decisions

Only user-visible questions came to the owner. Every internal call above is decided.

### Q1. Should an agent need a confirmation in Flightdeck before it starts work on another host?

The study recommended no extra gate (the harness's own tool prompt only, as in Paseo).

**Decision: yes, a confirmation card, with a setting to turn it off (owner, 2026-11-01).**

- Before `session.create` or a project set-up on a host other than the agent's own, the hab
  raises a confirmation card. It uses Capcom's mechanism (`capcom.md` 6.1, 6.3).
- `confirm.start_elsewhere` in Flightdeck's config, default on, also in the app's settings,
  turns the card off. Off, the action runs at once and the card shows as a record.
- The owner's general rule: a confirmation that adds friction gets a setting to turn it off
  (`app.md`, owner rules).
- Handing work to an agent already on a project needs no card (owner, 2026-11-01).

---

## 12. API additions

All are additive under `hab-logic-api.md` 3.3. Each answers `unavailable` until its stage builds
it.

| Item | Kind | Purpose | Stage |
|---|---|---|---|
| `client.kind` `hab` | Word | A peer link | task-phkmrjaw |
| hello `peer` | Optional field | The dialer's device id and host id | task-phkmrjaw |
| `mesh` | Capability | The hab supports peer links | task-phkmrjaw |
| `peers.list` | Method | Peers with label, state, endpoint in use, device id, last seen, and peers known to each (3.1) | task-phkmrjaw |
| `peers.accept` | Method | Accept a changed device id (the CLI's `peers accept`) | task-phkmrjaw |
| `peer.updated` | Event | A peer's state changed | task-phkmrjaw |
| `catalog.get` | Method | This hab's own catalog since a generation | task-dyagzrw0 |
| `catalog.put` | Method | The dialer pushes its own catalog sections | task-dyagzrw0 |
| `catalog.list` | Method | The merged view: own catalog plus direct peers' copies, with age and stale marks | task-dyagzrw0 |
| `catalog.updated` | Event | A copy or the own catalog changed (generation and device id only) | task-dyagzrw0 |
| `mesh.changed` | Event | To a peer link: catalog or mail waiting (4.2) | task-dyagzrw0 |
| `mail.push` | Method | Hand over messages and control items; returns accepted ids | task-z5pysi5t |
| `mail.pull` | Method | Take items waiting for the caller's device id | task-z5pysi5t |
| `channel.send` | Method | An agent's send through its own hab; returns id and state | task-z5pysi5t |
| `channel.status` | Method | A message's state | task-z5pysi5t |
| `channel.wait` | Method | Wait for a message to the caller or an agent state change (6.4) | task-z5pysi5t |
| `channel.message` | Event | A message state changed (sender side) | task-z5pysi5t |
| `transcript.index` | Method | A page of this hab's transcript index | task-syy38i23 |

---

## 13. Sources

- herdr-core: `docs/machines.md` and `docs/commands.md` (`hc machine`, `hc send`, `hc wait`),
  github.com/herdrdev/herdr-core, read 2026-11-01.
- herdr TUI, Paseo and Rex: `landscape.md` 1, 2.2, 2.3, 2.17, ideas 4.9 and 4.14.
- Rex hosts and home: superlogical.com/rex/docs (`llms-full.txt`), via the earlier research.
- `transport.md` (trust, pairing, revocation, device ids), `hab-logic-api.md` 2.2, 3.2, 3.9,
  4.2, 7.9, `gff-client.md` 6 and 11, `harness-adapters.md` (Input delivery, environment
  contract), `app.md` 4.3 and 8.
- Nomad constraints (`attr` detected, `meta` declared): developer.hashicorp.com/nomad/docs/job-specification/constraint.
- Earlier research reports on mesh, sessions and Rex (Q1 to Q6), read as ideas only.
