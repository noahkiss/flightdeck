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
| update needed | The peer has no `mesh` capability, refuses the peer link's hello, or no revision fits |

`gff-hab peers` lists label, state, the endpoint in use, the short device id and last seen.

### 4.4 Built choices (task-phkmrjaw)

Package `internal/peers`, wired into `gff-hab serve`. Commands: `gff-hab peers` and
`gff-hab peers accept <label>`. Example file: `docs/examples/peers.toml`.

- **One entry format.** A `[[peers]]` entry is gff's `[[hosts]]` entry (`hosts.Entry`), parsed by
  the same code (`hosts.ParseEntries`). Endpoint forms are gff's: an absolute path is a local
  socket, `cmd:<command>` runs under `/bin/sh -c`, anything else is an SSH destination.
- **The path.** `-peers`, else `$GFF_HAB_PEERS`, else `gff/peers.toml` beside gff's
  `config.toml`.
- **Bad lines by line.** The TOML library gives no key positions, so each problem names the line
  of its entry's `[[peers]]` header. A file that does not parse keeps the last good list and
  shows the parse error, with its line, in `peers.list`.
- **Reload on change.** gff-hab checks the file's size and modification time every 2 s, and reads
  the file once a changed size and time held for one more check (2 to 4 s). A writer that edits
  in place is not read half way. No file watcher library.
- **The pin is a line edit.** It replaces the entry's `device_id` line, or adds one after its
  `label` line, at the same indent. Every other line stays, comments included. The edited text
  must parse with the new pin before it replaces the file, in one rename that keeps the mode. An
  entry whose label is not on a line of its own is not edited. When the pin cannot be written,
  `peers.list` shows a problem with the id to set by hand, and the row shows no device id:
  habconn pins for a connection's life only, so after a restart any id would be taken. The edit
  reads the file once more after its fsync, just before the rename: when the user saved it in
  between, nothing is renamed and the edit starts again from the saved file.
- **The first pin reconnects once.** Writing the pin changes the entry, and `hosts.Reload` gives
  a changed entry a new connection. The peer shows `connecting` for a moment after first contact,
  or `online` with no endpoint in use while its own link to this hab is open.
- **Endpoints in turn.** Each attempt uses one endpoint. After a failed attempt the next one is
  tried, wrapping round (`habconn.Failover`), spaced by the reconnect backoff. There is no
  parallel stagger: an `ssh` process always starts, and fails only at the hello. A working
  endpoint stays in use until an attempt on it fails. Each update names the endpoint it is
  about, so a row shows an error with the endpoint that gave it.
- **A different hab at one endpoint is one failed attempt.** An address can move to another
  machine (DHCP). The peer is `refused` only when every endpoint, in that many attempts in a
  row, presented the same other device id; then `peers accept` pins that id. A different id at
  one endpoint while another fails or presents a third id keeps the peer `offline`, with the
  different hab named in the reason, and there is nothing to accept. Accept also refuses an
  empty id, which would turn pinning off for the entry.
- **A refused hello is `update needed`.** A gff-hab from before peer links refuses the client
  kind `hab` with `invalid`; one with no shared revision answers `revision_unsupported`. Either
  shows `update needed`. The peer link keeps trying at the longest backoff (30 s), or at once on
  `Retry`, so the peer comes online once it is updated, with no restart here
  (`habconn.Config.RetryRefused`). gff's own host connections still stop on a refused hello.
- **The peer link.** Client kind `hab`, the hello's `peer` field, and no `events.subscribe`
  (`habconn.Config.NoSubscribe`). Liveness is habconn's: a ping every 15 s, dead after 45 s.
- **Online.** A peer is online while this hab's link to it is up, or while a link from it is
  open: a hello whose `peer.device_id` matches the pin. `refused` wins over an inbound link.
- **Self.** An entry pinned to this hab's own device id is skipped at load. An entry whose
  endpoint answers with this hab's id is dropped while its endpoints stay as they were, with a
  problem line in `peers.list`; an edit of its endpoints tries it again. Nothing is written to
  the file, so a shared file stays the same on every host.
- **Unlisted links.** A hab that links in and is not in the list shows in `peers.list` as
  `unlisted`, with its claimed device id and host id. The add action waits for `peers add`.
- **`peer.updated`.** Ephemeral, id is the label, host scope only. Sent when a peer's row
  changes; a moving `last_seen` alone sends nothing.
- **Accept goes through the running hab.** Only it holds the id the peer presented. The command
  prints both ids and asks no question: running it is the owner's deliberate act.
- **Last seen** is kept in memory. A gff-hab restart forgets it until the peer is seen again.
- **The installed service and ssh-agent.** Install does not write `SSH_AUTH_SOCK` into the units
  or the plist. Decision: document, not pass through. Reason: the installing shell's agent
  socket belongs to its login session. An agent started per login uses a new path each time and
  goes at logout, while the lingering unit runs on, so a written path would go stale with no
  message. gff-hab also passes its own environment to every pane, so a stale path would spread
  to agents and shells. On macOS, launchd gives jobs in the GUI domain the system agent's socket.
  On Linux, a systemd user unit has an agent socket only when the user manager has one: a
  desktop session that imports `SSH_AUTH_SOCK` into it, an `environment.d` file naming a fixed
  socket (such as an agent run as a user unit under `$XDG_RUNTIME_DIR`), or `systemctl --user
  import-environment SSH_AUTH_SOCK`, followed by a restart of the logic job. Without one, peers
  whose key needs an agent show `offline` with ssh's "Permission denied (publickey)". A key with
  no passphrase for the peer links also works. The references (1.1 to 1.4) record nothing on
  how a background process reaches an agent, so saying what works is at their bar.
- **Not built in this stage:** `peers add`, `rm` and `rename`; "known to <label>"; the
  `mesh.changed` wake-up; a retry on wake or network change; the "network blocks SSH" and "no hab
  at <endpoint>" hints (the row shows ssh's own last error line); the two-hosts-one-device-id
  check (10).

Limits, at the bar of herdr, Rex and Paseo:

- The hello's `peer` field is a claim. Over the system sshd the caller is already the owner
  (3.2), so an inbound link from a claimed id can mark that peer online. Nothing else trusts it.
- The pin protects the dialer only. A hab that dials a refused peer sends it nothing beyond the
  hello and closes.
- A peer link has full API access. The control server gates no method by client kind, so a link
  with kind `hab` may call `pane.input`, `session.create`, `approval.answer` and
  `peers.accept`. This is the 3.2 decision: over the system sshd the caller already passed as
  the owner, as in herdr, hc and Rex. When the embedded SSH server admits paired hab keys that
  are not the owner's (4.1), client kind `hab` needs an allowlist of methods first.

### 4.5 Built choices: the client merge (task-kbvoirca)

Package `internal/merge`, on top of `internal/hosts` (one `habconn.Conn` per hab, each with its
own seq, epoch, resync and backoff). Pure Go, so the app's Go library and gff share it. This is
the client side: a client connects to every hab itself; no hab relays (`hab-logic-api.md` 7.9).

- **The model.** `merge.Build` turns the hosts' models into one `Model`: every host with its
  status, projects, sessions and unfiled panes. Each session and pane carries a `Ref`: the host
  label plus its id on that hab, since ids are unique on one hab only.
- **Projects match by identity.** The key is the remote, else the root commit
  (`hab-logic-api.md` 2.2). A project with neither stays its own on its host. A merged project
  lists one location per host, in host order, each with that hab's own project record. The
  board's grouping uses the same key (`merge.ProjectKey`).
- **One hab, two labels.** Shown once, under the first online label, else the first label
  (`merge.Shown`, which the board also uses). The other label stays in the host list, not shown.
- **Stale.** A host that is not online but has held a snapshot keeps its last structure, marked
  stale on every session, pane and project location. A client shows it dimmed. A host that never
  connected has nothing to show. A host that failed for good (revision, changed device id) shows
  nothing: hidden, not stale, as the board already did.
- **Input off until fresh.** `Host.Input` is true only while the host is online, which is only
  after its snapshot is applied: habconn reports online and the snapshot in one update. Calls to
  a stale host already fail with `habconn.ErrOffline`; they are never queued.
- **One ordered stream.** A `Merger` takes over the Set's updates and hands them on as `Event`s
  with a client-side `Seq` from 1 with no gaps, which is never compared with a hab's seq. Each
  event has a `Key`: host label, object kind (`project`, `session`, `pane`, `agent`, or the
  event type for one the client keeps no copy of) and the object id. Within one host the
  stream keeps the hab's order.
- **Resync per host.** A host that comes online sends one `resync` event for that host alone,
  with `NewEpoch` set when the hab restarted. Only that host's objects are replaced; the others
  keep their values. A host added by `hosts.Set.Reload` joins the same way, with no reload of
  the others.
- **The model may run ahead.** `Merger.Model` is rebuilt from the Set's per-host copies, which
  can already hold a later change than the event just read. A client redraws from the model on
  each event; the next event says what changed again. An event that changed no copy
  (`pane.output`, an old revision) keeps the model as it was.
- **A replaced host keeps its cache.** When `hosts.Set.Reload` replaces a host's connection (new
  endpoints, pin or transport), the new one starts Connecting with the old one's last welcome
  and structure. A host that was down keeps its rows, stale, and an online one does not blink
  out (`gff-client.md` 4.2). Input stays off until the new connection's snapshot arrives. If
  the new pin refuses the hab, the host fails and its rows hide, as for any failed host.
- **Event status comes from the stream.** `Event.Status` is the host's status after that
  update, worked out from the updates alone: Connecting until the host first connects since it
  was added or replaced, then Offline while it is not online. `Event.Err` is the update's own
  error. The model may hold a later status already, so an offline event read after a fast
  reconnect still says offline. A host wrapped by `hosts.Wrap` while offline reports
  Connecting, not Offline, until its first drop or connect: a label only.
- **Close.** `Merger.Close` closes the Set and ends `Events` at once, dropping events not yet
  read, even when nobody reads them. The Merger still reads the Set's updates to their end, so
  the Set finishes too. `Done` closes when everything has stopped: after `Close`, or once the
  Set has closed and `Events` was read to its end. A client that drops a Merger calls `Close`.
- **iOS.** `scripts/ios-build-check.sh` builds the package and its imports as a C archive for
  iOS on a Mac (device, or `-sim`). The app links no Go yet; this only proves the build.

Limits, at the bar of herdr, Rex and Paseo:

- The model is rebuilt whole on each change: fine for a handful of hosts and hundreds of
  sessions, not tuned past that.
- gff's workspace still reads `hosts.Set` and `board` directly; it already dims offline rows and
  refuses their actions. Moving it onto `merge.Merger` is a later step.
- The event queue is unbounded: `pane.output` and `pane.activity` pile up while a client stops
  reading. Coalescing them per key is not done yet.

Left for the project-identity `/consensus` (`logbook.md` 3), not decided here:

- **Fork workflow.** A fork's clone (origin is the owner's fork, upstream is the original) and a
  clone of the original (origin is the original) get different keys, since the hab picks origin
  (`hab-logic-api.md` 2.2, 3.9). One project shows as two groups. Falling back to the root commit
  would merge them, and would also merge real forks that went their own way. herdr, Paseo and
  Rex do not group across hosts by repository identity, so they give no answer.
- **Identity frozen at creation.** A project made from its root commit alone, with an origin
  added later, keeps its `root:` key. The same repository on another host, made after the
  remote was added, has a `remote:` key, so the two do not merge. Remote-less clones that share
  a root commit do merge across hosts, as they do on one hab.

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

### 5.4 Built choices (task-dyagzrw0)

Package `internal/catalog`, wired into `gff-hab serve`. Capability `catalog`. Methods
`catalog.get`, `catalog.put` and `catalog.list`; event `catalog.updated`. Example config:
`docs/examples/hab.toml`. Until the agent kit exists, `gff-hab api call catalog.list` reads the
merged view.

- **The hab's own config file.** `-config`, else `$GFF_HAB_CONFIG`, else `gff/hab.toml` in the
  user config folder, where the default peer file is too (`-peers` and `$GFF_HAB_PEERS` move only
  the peer file). Its `[catalog]` section holds `capabilities`, `project_roots` (absolute, or
  under `~/`), `always_on`, `always_on_alert` (a duration, default 30m), and `[[catalog.services]]`
  (`name`, `url`, optional `health`) and `[[catalog.actions]]` (`name`, `command`). gff-hab reads
  it at start and with every project scan (5 minutes). An entry it cannot use is left out and
  logged; a file that does not parse keeps the last good section, and at start, with none yet,
  the defaults (so `always_on_alert` stays 30m, never 0).
- **The whole catalog per generation.** `catalog.get {since}` returns the whole catalog when the
  generation differs from `since`, and no catalog when it is equal. A catalog is a few KB, so
  per-section answers would add a second shape for no real saving. A `since` above the
  generation (a hab that lost its state) gets the whole catalog too, so the puller replaces its
  copy.
- **The generation.** It rises on every change of the built catalog, compared field by field
  with the last one, not counting the generation and time. It is saved in `<state>/catalog.json`
  and rises once at each start, so a peer always pulls after a restart. Each new generation is
  one more than the last and never below the wall clock in milliseconds. A hab that lost or broke
  `catalog.json` therefore starts above every generation of its earlier life, and no puller
  takes its new catalog for the copy it holds. We chose the wall clock over an instance nonce:
  it needs no second field and no new compare rule. It fails only when the clock is set back
  past the earlier life, or when more than 1000 changes a second ran just before a restart.
- **`catalog.json`** holds the generation, the own catalog and the peers' copies. It is written to
  a temporary file, synced to disk, renamed into place, and the folder is synced. A file that
  does not parse is set aside as `catalog.json.bad`; one that cannot be read is logged. Either
  way the hab starts with no saved state rather than refusing to start.
- **A restart serves the last catalog.** At start the hab takes the detected facts and the
  folders read from the saved own catalog, so its first catalog (the new generation) already has
  its toolchains, harnesses and projects. Fresh detection and the scan replace them in the
  background. Peers never see a restarted host's projects vanish for a moment.
- **What is detected.** OS (`os-release` on Linux, `sw_vers` on macOS), arch, cores, memory;
  `git`, `go`, `node`, `python3`, `rustc`, `docker`, `podman`, and `xcodebuild` on a Mac, each by
  its own version flag with a 10 s limit; the harnesses `claude`, `codex` and `opencode` the same
  way. Detected words: `linux` or `macos`, `xcode`, `docker` (Docker or Podman), and `gpu` when
  `nvidia-smi` is on PATH. Read at start and every hour. Detection runs the programs that are
  first on the hab's PATH, with the hab's environment: the same trust as the user's own shell.
  `$GFF_HAB_DETECT=off` reads only OS, arch and cores from the Go runtime and runs no program;
  every test that starts gff-hab sets it.
- **Projects.** The scan walks each project root to depth 3, follows no symbolic link, skips
  hidden folders, stops at a folder with `.git`, and reads at most 500 folders. It also reads
  every folder of the structure's projects. Identity is gff-hab's own (`gitIdentity`): the
  normalized remote of origin, else of the first remote, else the root commit; a folder with
  neither is left out. Each folder carries branch, head, dirty (`git status --porcelain` with
  untracked files) and the commit time of HEAD, read with `internal/gitstatus`'s clean
  environment. Folders of one identity are one project; a structure project of that identity
  names it. A new project folder from the structure is read on the next change; the rest every
  5 minutes.
- **Agents.** Every pane whose agent state is not `exited`, named by its session (an unfiled
  pane by its pane id), with the project's name and the time its state began.
- **Peers in the catalog.** The catalog lists the device ids of this hab's peer list
  (`peers`), and of those the peers it watches (`watches`): each one whose copy carries the
  always-on mark. Only a watching hab can raise the alert, so the always-on check defers to
  watchers, not to every hab that lists the marked one. A hab that lists the marked host but has
  never reached it, or holds a copy from before the mark, would otherwise silence everyone.
- **Exchange.** Every 15 s, and at once when a peer's row changes or the own catalog changes,
  this hab works each online link it dialed to a peer with `catalog`: it puts its own catalog
  when that link has not had this generation, then pulls with `since` the generation it holds.
  A link seen down puts again when it is back, and so does a link whose peer's catalog changed,
  since a restarted peer may have lost its copy. A pulled catalog must carry the link's device
  id, and a link with no device id pulls nothing. A link whose put or pull fails waits one poll
  period, then twice that for each failure in a row, up to 5 minutes; it logs only when the
  wait grows.
- **`since` comes from pulls only.** It is the generation this hab last pulled over its own
  link. A `catalog.put` for that peer clears it, so the next pull asks for everything. The hello
  on a put is a claim (3.2), so a put that copies the peer's current generation can never
  outlive the next pull. `catalog.put` is taken only from a peer link (client kind `hab` with the hello's `peer`)
  and only for that peer's own device id.
- **Copies.** One per device id, with the time this hab last confirmed it with the peer
  (`seen_at`), saved in `catalog.json`. A copy is stale while its peer is not online in
  `peers.list`. It is dropped when its device id is in neither the peer list nor an open link.
  A hab that links in without being listed has its copy shown, with no label, while it is linked.
- **The merged view.** `catalog.list` gives this hab's catalog first, then each listed peer's
  copy in file order, then those of unlisted linked habs; and the projects grouped by
  `merge.ProjectKey`, one location per host, each with its folders and the stale mark.
- **One hop by construction.** `catalog.get` serves only the hab's own catalog, and
  `catalog.put` refuses another hab's, so no copy travels on.
- **The always-on alert.** Each round, for each marked peer this hab watches: online clears the
  alert; otherwise the hab counts from the later of the peer's last seen and the moment this
  hab's own watch began, which is its start or the end of a sleep (two rounds further apart than
  three poll periods and than 30 s). Past `always_on_alert`, and when no online peer copy with a
  lower device id watches the marked hab, it raises one notice through `notice.raise` (level
  active, "<label> is offline"). One per outage. Every time in this check is wall time with the
  monotonic reading stripped: Linux and macOS stop the monotonic clock during sleep, so a gap
  measured with it never shows a sleep, and a laptop would alert the moment it woke.
- **`catalog.updated`.** Ephemeral, host scope only, id the device id. Sent when the own
  catalog or a copy changes, and when a copy is dropped (generation 0).

Not built in this stage:

- The `mesh.changed` wake-up (4.2). The dialer pulls every 15 s instead of 60 s, so a change on
  the dialed hab reaches it in at most 15 s; a change on the dialer goes out at once.
- The agent kit (`gff hosts`, `gff projects`, MCP tools; task-6ho9972u, Part 4).
- GPU models, battery state, Xcode simulators, attached devices, whether hooks are set up, and
  the embedded server's endpoints.
- A dirty read on a hook event in that project: dirty state is up to 5 minutes old.

Limits, at the bar of herdr, Rex and Paseo (none of them has a catalog):

- The alert's "online the whole time" is this hab's own process time and sleep gap. A restart of
  this hab starts the count again, so a host that stays down alerts once more 30 minutes after
  that restart.
- The lowest-id rule reads the copies this hab holds, which can be up to one round old. Two habs
  that each see the other offline (a network split) both alert, as section 8 accepts. So does
  a lowest-id hab that cannot reach the marked host while others can (a laptop on a network that
  blocks SSH): it alerts after 30 minutes.
- `always_on_alert` is each watcher's own setting, not the marked host's. The lowest-id watcher's
  value decides when the alert goes out.
- An unlisted hab that keeps dialing in after the user removed it stays visible, with no label,
  for as long as its link lasts (section 10 says its copy is dropped; it is, once the link ends).
- Paths and the summary line go to every peer as they are. The peers are the same owner's
  machines (3.2).

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

### 6.5 Built choices (task-z5pysi5t)

Package `internal/channels`, wired into `gff-hab serve`. Capability `channels`. Methods
`channel.send`, `channel.status`, `channel.wait`, `mail.push` and `mail.pull`; event
`channel.message`. Commands: `gff-hab send` and `gff-hab wait`. Channels own deliver-when-idle;
the agent kit (task-6ho9972u) calls the same service and may give the commands other names.

**Two front doors, one service.**

- **From a pane.** The command reads `GFF_HAB` and posts `{"method", "params"}` to
  `POST /channel/<token>` on the hook endpoint. The token names the pane, and the pane's session
  is the sender (6.2 step 1). The answer is `{"result": ...}` or `{"error": {"kind", "message"}}`.
  The route takes `channel.send`, `channel.status` and `channel.wait`. A pane with no session
  cannot send.
- **From a client.** The control connection takes the same three methods. `channel.send` names
  the sending session in `from`; `channel.wait` names the waiting session in `session`. Any
  control client is already the owner (3.2), so naming a session is enough.
- **The command picks the door.** With `-socket` or `-via`, the control connection, and `-from`
  or `-session` is needed. Else, with `GFF_HAB` set, the hook endpoint. Else the default
  control socket.

**Messages and ids.**

- A message is: id (128 random bits, 32 hex digits), `from` (device id, host id, session name),
  `to` (device id, session name), text, optional `reply_to`, and the sender hab's time.
- Text is at most 64 KiB. Empty text is refused with `invalid`.
- `channel.send` returns the id, the resolved address and the state: `handed` for a target on
  this hab, else `queued`. The sender hab keeps one record per message for 7 days
  (`channel.status`), with its state, a reason for a refusal, and the time of the last change.
- A state moves only forward: `queued`, then `handed`, then one final state (`delivered`,
  `not_delivered`, `refused`, `expired`). A late or repeated update for an earlier state is
  ignored.
- `channel.message` is sent on the sender hab each time a record changes. Ephemeral, host scope,
  id the message id.

**Addresses and names.**

- `label/name`: `label` is a peer label in this hab's peer list, and the peer's device id must be
  known (pinned or seen). The sender does not check the name; the target hab does, and refuses an
  unknown one. So `label/name` works while the target is offline or the catalog copy is old.
- A bare `name`: this hab's sessions, then every catalog copy it holds, stale ones included. A
  copy names its agents (5.4), so a bare name finds only sessions with an agent running when the
  copy was pulled. No match is `not_found` with a hint to use `label/name`. Matches on two or
  more hosts are `conflict`, listing each full address.
- **The cache is the catalog.** The copies are the "who has this name" cache the research
  proposed. They refresh with each catalog pull (every 15 s, at once on a peer change). There
  is no second cache to invalidate.
- A name found only in an unlisted hab's copy is refused with "add <host id> as a peer": mail
  goes only to habs in the peer list (6.3, no forwarding).
- **Names unique across peers.** `session.create` with a name, and `session.rename`, refuse with
  `conflict` "name in use on <label>" when a catalog copy (stale included) lists an agent with
  that name. A name gff-hab picks itself (`claude`, then `claude-2`) also skips names in the
  copies. A clash made while a peer was offline stays (10); `label/name` reaches either agent.

**Outbox, handover and retry.**

- **One outbox per target device id**, in `<state>/channels/outbox-<id>.json`, where `<id>` is
  the device id with `/`, `+` and `:` made file-safe. It holds messages and status items, in
  order. At most 1000 items per target: a send beyond that is refused with `conflict` "outbox
  full for <label>". An item older than 7 days is dropped; a dropped message's record turns
  `expired`.
- **Every write reaches the disk before the answer.** Each file is written to a temporary file,
  synced, renamed into place, and the folder synced (as `catalog.json`). A file that does not
  parse is set aside as `.bad` and the hab starts with that part empty.
- **Push.** Every 15 s, and at once after a send or when a peer's row changes, the hab works
  each online link it dialed to a peer with `channels`. It pushes that peer's outbox in order,
  up to 100 items per `mail.push`. The answer has one result per item. A failed call keeps every
  item and waits 1 s, doubled for each failure in a row up to 60 s. A link that comes back
  online is tried at once.
- **Pull.** On the same rounds the hab calls `mail.pull` over each such link. The answer holds
  up to 100 items waiting for the caller's device id. The next pull carries the results of those
  items, and the peer then removes them; the hab pulls again at once while items come. So a hab
  that only the other side can dial (4.2) still sends and receives. Results that a lost answer
  dropped cost one repeat, which dedup absorbs.
- **No forwarding.** `mail.push` and `mail.pull` need a peer link: client kind `hab` with the
  hello's `peer` field. A message whose `from` device is not the link's, or whose `to` device is
  not this hab, is refused.

**Receive and dedup.**

- For each message the receiving hab checks, in order: a repeated id gets its first result
  again; a session name this hab does not hold is `refused` "no session named <name> on this
  host"; else the message joins the inbox and is `handed`. The id is kept 7 days from its first
  arrival, with its result, in `<state>/channels/ledger.json`. The inbox
  (`<state>/channels/inbox.json`) and the ledger are on disk before the answer goes back.
- At most 1000 messages per session wait in the inbox; one more is `refused` "inbox full".
- A message to this hab's own session takes the same path at once, with no outbox.

**Delivery.**

- The hab tries the oldest message for each session when any agent state changes, after a new
  message arrives, and once a second. Messages to one session go one at a time, in arrival order.
- It delivers through `session.send`'s own path (`harness-adapters.md`, Input delivery):
  bracketed paste, then Enter; only while `idle`, `done` or `error`; never with a permission
  request open; one message per turn. Busy (`working`, `blocked`, an open request, a message
  still starting its turn) holds the message. `error` delivers, as `session.send` does: the turn
  stopped and the harness is at its prompt again.
- **Refused:** the session is gone or exited, the pane shows `none` or `exited`, or the harness
  takes no messages yet (opencode). A pane with no agent state at all is held for 30 s after the
  message arrived or the hab started, whichever is later, so a hab that just restarted can
  learn the state again; after that it is refused "agent not running".
- **`not_delivered`:** the paste reached the prompt but Enter was not sent. It is not retried.
- **`delivered`** means both writes reached the program. The read back (a `working` signal
  within a few seconds) is Part 4's and is not built yet.
- **`channel.wait` also delivers.** An agent that runs `gff-hab wait` is `working`, so the PTY
  path would hold its messages until the wait ends. So a wait takes the oldest held message for
  its session, marks it `delivered`, and returns it. A message being written to the PTY at that
  moment is not taken.
- **Status back to the sender.** Each final state goes into the outbox for the sender's device
  id as a status item, and travels by push or pull like a message. The sender applies it only to
  a record whose target is the device that sent it.

**The envelope** (provisional; Part 4 may change the words):

```
[Flightdeck: message from agent <address>, not from the owner. Reply with: gff-hab send <address> "<text>"]
<text>
```

`<address>` is the sender as the receiving hab would address it: `label/name` with the label of
the sender's device in the receiving hab's peer list, else `<host id>/name`. A message from the
same hab gives the bare name. It is one paste, so a multi-line message stays one message.

**`channel.wait`** (6.4):

- Waits for a message to `session`, or, with `agent`, for that agent on this hab to reach one of
  the states in `for` (any change when `for` is empty; `exited` also when the session goes).
  Whichever comes first returns. `timeout_ms` defaults to 180 000 and is at most 600 000; the
  end of the time returns `timed_out`, not an error.
- `channel.status` with `wait_ms` waits the same way for the record's final state.

Not built in this stage:

- `gff-hab wait -agent` for an agent on another host; `-for` reads this hab's agents only.
- The read back and its `not_delivered`; the two-second typing pause (both Part 4).
- The `mesh.changed` wake-up: a hab that only pulls sees new mail within one round (15 s).
- An MCP tool and the printed skill (agent kit, task-6ho9972u).
- Revocations as control items (`transport.md` 6.6): the outbox takes only messages and status
  items.

Limits, at the bar of herdr, Rex and Paseo (hc has no outbox and no dedup):

- A hab that lost its state directory has lost its ledger: a message pushed again after that is
  delivered twice (10).
- Clocks: expiry counts from the sender hab's own clock; the dedup window from the receiver's.
- A name clash made while a peer was offline is not shown on the status board yet.

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
