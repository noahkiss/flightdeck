# gff-hab logic and API: behaviour spec

Status: draft for review. Part 2, stage 1.

This spec says how `gff-hab` behaves and how clients talk to it. Stages 2 to 9 of Part 2 build
from it. It describes behaviour in our own words. It copies no code from the reference projects.

The holder side of the link (wire format, ring size, drain, service install) belongs to the holder
spec, `docs/specs/hab-holder.md`. This spec covers only what `gff-hab` does on its side of that
link.

Each section that settles a question ends with a **Decision** line. "Decision: proposed" means this
spec recommends it and the build can start from it. "Decision: open" means the owner decides; the
options and a recommendation are in [Open questions](#8-open-questions).

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [API rules](#3-api-rules)
4. [Transports](#4-transports)
5. [Data streams](#5-data-streams)
6. [Pane size](#6-pane-size)
7. [Edge cases](#7-edge-cases)
8. [Open questions](#8-open-questions)

---

## 1. References

### 1.1 Rex (control and data split)

Rex is one Go server per account. It links libghostty through cgo, one parser per terminal. It has
no holder: stopping the server ends every program. Its API is the closest match to what we want.

- **Control connection.** One WebSocket per client. Each message is one JSON text message of type
  hello, welcome, request, response or event.
- **Revisions.** The client's hello names the newest API revision it speaks and the oldest it
  accepts. The server picks the newest revision both speak. It translates messages to and from
  older revisions through a chain of history steps. Both sides skip unknown fields, types and
  events.
- **Seq rule.** Events count from 1 on each connection, with no gaps. Every response carries the
  seq of the last event queued before the request ran. The client applies the response, then only
  events with a higher seq. Each object carries a revision, so applying a change twice does no
  harm.
- **Backlog.** More than 8 MiB of queued events closes the connection ("event backlog exceeded").
  The client reconnects and reads state again.
- **Reconnect.** A reconnect token in the hello keeps the client's id and attachments for a grace
  period. Events from the gap are lost, and seq starts again at 1.
- **Data connections.** Each visible terminal gets its own WebSocket. The client asks the control
  connection for a single-use ticket, then opens the data connection with it. The stream starts with
  a snapshot of the screen in Ghostty's snapshot format, then carries the program's raw bytes.
  Output flows to the client only; input goes over the control connection. Rex's own messages
  (resize, theme, child exited) ride inside the byte stream as an escape sequence that carries a
  per-stream nonce, so a program cannot fake one. A dropped stream resumes from the exact count of
  bytes the client received. Too much backlog closes the stream with "reconnect required".
- **Size owner.** A terminal has at most one owner client, and it takes its size and colours from
  that owner. A client claims a terminal outright ("take") or only when it has no owner
  ("request"), and gives it up ("disown"). Each change carries a reason: create, take, request,
  disown, lost, disconnect, restore. The server keeps every client's last requested size, newest
  first. Each size change puts a resize message, with the owner, into the data stream.
- **Schema.** Generated from Go types and served by the running server.

### 1.2 herdr-core API

herdr-core is a Rust core plus a separate holder process that owns the PTYs. Its public API:

- **JSON-RPC 2.0, one message per line** ("plain mode") on a Unix socket. Any language or a socket
  tool can speak it. Errors carry a stable `kind` and a human `hint`.
- **Same protocol over stdio.** A `connect` subcommand bridges stdin and stdout to the socket, so an
  SSH command reaches a remote core with no new listener.
- **Framed mode.** A `hello` with `framed: true` switches the connection to frames after the reply.
  A frame has a channel number, a kind and a length. Channel 0 carries JSON-RPC; other channels
  carry attach streams. Frame kinds: data, credit, close, resize, focus, display facts.
- **Attach streams.** The core sends a first paint (screen, optional history, status, title), then
  raw program output with the terminal queries it already answered removed. The viewer sends keys
  on the same channel. A stream starts with 256 KiB of credit; the viewer returns credit as it
  writes bytes out. A viewer more than 4 MiB behind gets a fresh paint instead of the backlog. A
  stalled stream never blocks channel 0 or other streams.
- **Size.** The last active viewer sets the size. Active means it typed, resized, gained focus or
  sent display facts. An `observe` viewer never types or resizes.
- **Events.** Subscribing returns a snapshot with a seq. Each event raises seq by one. A heartbeat
  with the current seq arrives every 15 s when nothing else does. A subscriber that falls behind
  gets `missed` with a fresh snapshot. **Seq starts again with each core process**, and the client
  must take a fresh snapshot after reconnecting. There is no explicit epoch.
- **Versioning.** The schema (OpenRPC) is generated from the code and served by a `rpc.discover`
  method. The API only grows: a test compares the schema with the last released one and fails on
  any change that could break a client (a removed method or field, a new required input, an input
  that accepts less, a result that can carry a new kind of value). A real break needs a new
  "protocol generation", chosen on purpose.
- **Documents.** Small opaque JSON values with a version number; a write succeeds only if the
  version matches. This is a useful pattern for client-owned shared settings.

### 1.3 The earlier GFF server

The earlier GFF server is TypeScript on Node. It drives zellij through the zellij CLI.

- A **poller** reads the full pane list of each host on a timer and broadcasts the whole tree. A
  per-host circuit breaker stops polling a host that has gone to sleep.
- Clients receive the tree over **server-sent events**. Each broadcast carries an **epoch** (new on
  every server start) and a **seq** (one per event). A client that sees a gap in seq, or a new
  epoch, reads the tree and the selected pane again.
- Pane content comes from screen dumps on demand. Chat views read harness transcripts, not the
  screen.
- The current interim plan keeps the poll as the only tree read and adds the zellij fork's new
  event stream as a doorbell: an event triggers a read, it does not replace one.

What we keep: epoch plus seq, and "on a gap or a new epoch, read again". What we drop: polling as
the source of truth, and whole-tree broadcasts.

### 1.4 The zellij fork's watch and subscribe streams

- **`list-agents --watch`** prints one JSON line when an agent appears, changes or goes. The push
  is in the client: it re-reads the pane list every second and prints the difference. It carries no
  seq. A pane counts as gone only after two looks miss it.
- **A server-pushed event stream** (agent changes and pane lifecycle, with seq and epoch) is in
  progress in the fork, so the interim GFF can stop polling for most changes.
- **`subscribe`** pushes pane contents. **This is why composed screens failed for GFF:**
  - It delivers the rendered viewport text of each pane, not the program's raw bytes. A libghostty
    view cannot consume rendered text; it needs bytes.
  - Deliveries were driven by the render loop. A tab with no attached client did not render, so its
    panes went silent while a human looked at another tab. The fork later fixed the gap by querying
    missing panes directly, but the feed stays tied to zellij's render timing and composed size.
  - The grid itself stayed correct: a direct screen dump of an unrendered pane was fresh. The
    problem was the push trigger and the composed form, not the screen state.
  - GFF dropped subscribe mode and went back to polling.
- **Lesson for gff-hab:** stream raw bytes from the holder, keep one parser per pane in gff-hab, and
  never derive the event stream from a render loop.

### 1.5 What we take from each

| Topic | Taken from | Change we make |
|---|---|---|
| Control plus one data connection per pane | Rex | Same split over a Unix socket and SSH, not only WebSocket |
| JSON-RPC 2.0 lines, stdio bridge over SSH | herdr-core | None |
| Schema from code, additive-only test | herdr-core, Rex | Combined with whole-number revisions |
| Revisions with min and max, translation | Rex | Translation only down to a stated floor |
| Seq per connection, response carries last seq | Rex | Plus an epoch per gff-hab process |
| Epoch plus seq, resync on gap | Earlier GFF | Same rule, on every event |
| Snapshot then raw bytes, resume by byte count | Rex, herdr-core | Count is the holder's stream offset, see 5.4 |
| Credit flow, repaint when far behind | herdr-core | Kept as the backlog rule for data streams |
| Size owner with reasons | Rex | Typing takes ownership after a debounce |
| Observers never resize | herdr-core | Same |
| Queries answered once by the server | herdr-core | Same, with a rule for bytes that arrive while gff-hab is down |

---

## 2. Model

### 2.1 Processes on one host

| Process | Job | Restarts |
|---|---|---|
| `gff-hab-holder` | Owns every PTY, keeps a raw byte ring per pane, resizes, signals, reaps. Parses nothing. | Almost never; upgrades by drain |
| `gff-hab` | Parses each pane with libghostty, keeps the shared structure, runs harness adapters, serves the API | Any time; no program dies |

A host has one hab. Clients connect to every hab themselves. No hub sits between them.

### 2.2 Objects

| Object | Identity | Owned by | Survives a gff-hab restart |
|---|---|---|---|
| host | The hab's host id (from config), plus its build | gff-hab | Yes |
| holder | Holder build and process; one current, zero or more draining | Holder | Yes |
| pane | Holder pane id (never reused), absolute output offset, size, owner | Holder (bytes), gff-hab (parsed screen, owner) | Yes; owner may change, see 6.5 |
| session | Stable id; one running program in one pane; a name unique on the host | gff-hab | Yes |
| project | Git remote, or the root commit when there is no remote | gff-hab | Yes |
| order | The order of sessions inside a project | gff-hab | Yes |
| agent state | Harness, state, turn, from the harness adapter | gff-hab (Part 4) | Rebuilt from hooks and transcripts |
| client | Connection-scoped id, label, kind (app, terminal client, CLI, script) | gff-hab | Only through a reconnect token, see 3.7 |

Rules:

- **Ids are stable and never reused.** Events and references use ids, never names or positions.
- **gff-hab owns structure, not presentation.** It keeps projects, sessions, names and order. It
  keeps no layout, splits, tabs, focus or theme. Each client owns its own presentation.
- **A pane with no session record is "unfiled".** gff-hab never deletes a pane it cannot place. It
  shows it, unfiled, so the owner can file or close it.
- **Native data stays native.** gff-hab keeps a path to each harness transcript, never a copy.

Decisions taken in stage 5 (`internal/structure`):

- Ids are random, so a lost state file can never cause an id to be reused.
- Project identity is the remote when there is one, else the root commit. Remotes are stored as
  host/path (no scheme, user, port or `.git`; host in lower case). Project names need not be unique; session names are unique among all session records, exited ones included.
- A pane's revision is stored too. Linking or unlinking a session raises it, since
  `Pane.session` changed.
- Deleting a project keeps its sessions, with no project. Closing a session removes the record
  only; the pane, if live, becomes unfiled.
- A session whose pane is gone is marked exited with no exit status. If the pane comes back (its
  holder only dropped out), the session runs again. A gone pane keeps a tombstone of its final
  revision, so a returning pane's revision keeps rising.
- A corrupt or unreadable state file is renamed `structure.json.bad-<time>`; gff-hab starts empty
  with every live pane unfiled and reports the problem. A file from a newer build is left alone:
  gff-hab starts empty, saves nothing and reports it. A lock file stops two gff-hab processes
  sharing one state directory.

### 2.3 The holder link (logic side only)

gff-hab is a client of every live holder on its host: the current one and any that are draining.

- On start it connects to each holder, agrees a holder protocol version, and reads the pane list.
- It keeps one parser per pane. Holder output enters the parser in order, with its absolute byte
  offset.
- Measured cost of one parser: a 120x40 libghostty terminal holding 10,000 coloured 120-column
  lines of scrollback reports 10 MiB resident (libghostty's own memory figure, Linux amd64, release
  build). Its `snapshot-ghostty` is 4.6 MiB and its `snapshot-vt` 1.3 MiB, for 1.2 MiB of input.
  `TestMemoryLongScrollback` in `internal/vt/ghostty` reproduces it (`go test -v`).
- Cost by scrollback cap, from `TestScrollbackCost` in `internal/vt/ghostty`: a 120x40 pane fed
  twice the coloured 120-column lines its cap holds, so the scrollback is full. Linux amd64, a
  12th-generation desktop CPU, release libghostty, the checkpoint written and fsynced to an ext4
  SSD (`GFF_MEASURE_DIR` names the directory; `make libghostty`, then `go test -tags ghostty -v
  -run TestScrollbackCost ./internal/vt/ghostty` with the Makefile's `PKG_CONFIG_PATH`). Times are
  one call each, over two runs.

  | Cap | Scrollback rows | Resident | Checkpoint | Encode | Write and fsync | `snapshot-vt`, all | `snapshot-vt`, 1000 rows | 1000-row page |
  |---|---|---|---|---|---|---|---|---|
  | 1 MiB | 413 | 0.8 MiB | 0.2 MiB | 0.4 ms | 1-2 ms | 0.06 MiB, 0.2 ms | 0.06 MiB, 0.1 ms | 0.05 MiB, 0.2 ms |
  | 2 MiB | 1,639 | 1.9 MiB | 0.8 MiB | 1.1 ms | 1-2 ms | 0.22 MiB, 0.5 ms | 0.14 MiB, 0.3 ms | 0.13 MiB, 0.3 ms |
  | 4 MiB | 3,704 | 3.8 MiB | 1.7 MiB | 2.1 ms | 1.7-2.5 ms | 0.50 MiB, 1.1 ms | 0.14 MiB, 0.5 ms | 0.13 MiB, 0.3 ms |
  | 10 MiB | 9,899 | 10.0 MiB | 4.6 MiB | 6 ms | 3.4-4 ms | 1.32 MiB, 3.2 ms | 0.14 MiB, 0.3 ms | 0.13 MiB, 0.3 ms |

  Resident memory tracks the cap. A parser allocates scrollback pages as output arrives, so a
  pane that printed little costs little; only panes that printed past the cap cost the full cap.
- Each parser's scrollback is capped at 10 MiB by default, about the 10,000 lines above. `gff-hab
  serve -scrollback-mib N`, else `$GFF_HAB_SCROLLBACK_MIB`, sets another cap. A new parser and one
  restored from a checkpoint get the same cap, so a restored screen drops scrollback at the same
  points as one that parsed every byte. libghostty's own default (about 10 kB) is never used.
- **Scrollback budget.** All parsers together get at most 256 MiB of scrollback by default:
  25 panes at the full cap. With more panes, each parser's cap is the budget divided by the number
  of panes the link follows, never under 1 MiB, and never over the per-pane cap. The link sets
  the new cap on every parser when a pane is added or dropped; lowering a cap drops the oldest
  scrollback of every pane at once, and raising it again brings nothing back. `gff-hab serve
  -scrollback-budget-mib N`, else `$GFF_HAB_SCROLLBACK_BUDGET_MIB`, sets another budget. Without
  it, 50 busy panes would hold 500 MiB; with it they hold 256 MiB.
- **Checkpoints stay complete.** A checkpoint carries the whole scrollback (`snapshot-ghostty` has
  no bound). At the 10 MiB cap it costs about 10 ms of CPU and 4.6 MiB of disk writes, and only
  a pane that prints without a 1 s pause writes one every 10 s: about 0.5 MiB/s of disk writes
  per such pane, and nothing for a pane at rest. That is acceptable, so the cadence does not
  change with size, and a crash restore keeps the whole scrollback.
- It writes a **checkpoint** per pane (a parser snapshot plus the offset it covers) when the pane
  goes idle and every N seconds. The holder spec owns the ring; this spec owns the checkpoint.
- **Planned restart:** gff-hab writes fresh checkpoints, tells each holder it is handing over, and
  exits. Handover carries no offsets: the checkpoints hold them. The next gff-hab loads the
  checkpoints and asks each holder for a replay from each saved offset.
- **Crash:** the next gff-hab loads the latest checkpoint and replays the ring from that offset. If
  the ring no longer reaches back that far, it rebuilds from the ring alone and marks the pane's
  screen as **suspect** (see 6.7).
- gff-hab must speak every holder protocol version still running, because a drained holder can
  live for weeks.
- Input, resize and signal requests from clients go to the holder that owns the pane.

Decision: proposed. Details of the frames belong to the holder spec.

Decisions taken in stage 4 (`internal/link`):

- A checkpoint is one file per pane, `<state>/checkpoints/<pane uuid>.ckpt`: a JSON header line
  (format version, pane, holder, offset, encoding, size, suspect, time), then the raw snapshot.
  The header also holds the snapshot's length and CRC-32; a file that disagrees counts as no
  checkpoint. It is written to a temporary file, synced, renamed, and the directory synced, so a
  crash leaves the old or the new one. The encoding is `snapshot-ghostty` when the parser has it,
  else `snapshot-vt`.
- A checkpoint is taken only where the output stream is in the parser's ground state: not inside
  an escape sequence, a control string or a UTF-8 character. A snapshot cannot carry a half-parsed
  sequence. A small scanner in the link tracks this; mid-sequence, the tick is skipped, and a
  planned stop keeps the older checkpoint.
- A checkpoint is restored only onto a pane of the holder that wrote it.
- Cadence: when a pane has printed nothing for 1 s, and at least every 10 s while it keeps
  printing; only when the screen changed. Both are configurable.
- One event loop applies every holder frame in the order the holder sent it, replay data and
  live output alike, so a pane's parser always sees its bytes in offset order.
- A checkpoint the ring no longer reaches, one past the pane's byte count, or one in an encoding
  this parser lacks is ignored: the screen is rebuilt from the ring alone. It is suspect only when
  the ring has lost bytes (its oldest offset is above 0). A pane resized after its checkpoint is
  suspect too: where the resize fell in the stream is unknown. Suspect is kept in the checkpoint.
- A subscriber that resumes from before the link's buffer (for example from before the
  checkpoint the link restarted from) is served by a bounded replay from the ring. Only when the
  ring has lost those bytes does it get a snapshot instead (5.4).
- Every byte that did not arrive as live output carries no "live" mark. Only live bytes may get
  answers to terminal queries (5.6).
- When a holder connection ends, its panes are marked lost at once and their subscriptions end.
  The link rescans the registry then and every 2 s, and replays a pane that comes back from the
  offset its screen holds. A late end event from an older connection leaves a pane that already moved to a newer
  one alone.
- After a scan that reached every holder in the registry, the link drops panes no holder lists
  (a lost pane after 30 s), frees their parsers and removes their checkpoints. A spawn needs a
  new uuid: one that matches a live pane on the current holder is a retry of that spawn, and one
  that matches any other pane the link lists or has dropped is refused. If a pane with the uuid
  appears while the spawn runs, the new pane replaces it, and watchers get gone for the old pane
  before added for the new one.
- **Retired uuids outlive a restart.** A pane's uuid is retired when the pane goes gone (released,
  closed by a client, or its holder said bye) and when the prune drops it. The retired set lives
  in `<state>/checkpoints/retired.json`: `{"v": 1, "retired": {"<pane uuid>": "<time retired,
  RFC 3339>"}}`, written to a temporary file, synced, renamed and the directory synced, like a
  checkpoint. A spawn of a retired uuid is refused for 30 days after it was retired (`gff-hab
  serve -retired-days N`, else `$GFF_HAB_RETIRED_DAYS`). Loading the file drops the entries past
  that TTL and rewrites it; every save drops them too. An unreadable file is logged and treated as
  empty. The refusal holds the moment a uuid is retired; the file is written soon after by a
  writer of its own, off the event loop, so a slow fsync (about 1 ms on a Linux SSD, more on
  APFS) never delays a release or a prune. Requests made during a write coalesce into one more
  write, and a planned stop writes any change still pending. A crash in the moment between
  retire and write loses that one uuid's persistence. A lost pane's uuid is refused while the link lists the pane, and retired when the prune
  drops it.
- When output jumps past bytes that never arrived, the screen is suspect and every subscriber
  gets a snapshot at the new offset before the next bytes.
- A pane's notices keep their order: added, then exited, then gone. A spawned pane is added on
  the event loop before any frame the holder sent for it, even an exit that arrived before the
  spawn's answer.
- **Releasing ended panes on a draining holder.** The link releases a pane when all of these hold:
  its holder is draining; the exit is recorded (the exited frame, or an exited pane record at
  attach); the replay after attach has ended; and the parser holds every byte before the exit's
  final count. Subscribers have those bytes then too, because the link queues each byte to every
  subscriber as it parses it. The link checks after each exit and replay end, and on every scan,
  so a holder drained later releases its ended panes too. A failed release is tried again on the
  next scan. After the release the pane is gone: its subscriptions end, its checkpoint is
  removed, watchers get a gone notice, and its parsed screen stays readable until the prune
  drops it. The holder exits after its last release. A running pane on a draining holder is
  never released by this rule. Ended panes on the current holder stay until a client closes
  them with `pane.close` (3.9), which releases a pane on any holder.

### 2.4 Backends

gff-hab talks to its terminals through a backend interface with two implementations:

| Backend | Structure and lifecycle | Raw bytes | Data streams |
|---|---|---|---|
| gff-hab-holder | Pane list, events from the holder | Yes | Yes |
| zellij (interim) | The fork's event stream plus a tree read on a gap | No | Not available; capability absent |

Under the zellij backend the terminal view attaches a real zellij client. Seq and epoch are minted
by gff-hab, never taken from zellij. Whether this interim backend exists at all is Decision: open
(Q1).

### 2.5 Commands from prompt marks (OSC 133)

The console shows each shell command as a card (`app.md` 6). A shell that emits the four OSC 133
marks tells gff-hab where each command's prompt, typed line, output and end fall in the pane's
byte stream. gff-hab finds the marks; the holder parses nothing (holder spec Q1). A command's
bytes come from a bounded replay of the holder's ring.

**The marks.** Each is an OSC: `ESC ] 1 3 3 ; <letter> [; <params>]`, ended by BEL or by
`ESC \` (ST).

| Mark | Meaning | Params gff-hab reads |
|---|---|---|
| `A` | Prompt start | none |
| `B` | Command start: the typed line follows | none |
| `C` | Output start: the command runs | `cmdline_url=<percent-encoded command line>`, when present |
| `D` | Command end | the first param, when it is a decimal exit status |

Other letters and other params are ignored.

**Finding them.** A small scanner follows the stream byte by byte, without a VT parser, so a
mark split across any number of reads is found once, at the same offsets.

- It follows OSC strings as a terminal does: BEL or ST ends one; CAN or SUB aborts it; an ESC
  that is not followed by `\` aborts it and starts a new escape sequence. An OSC that is not
  OSC 133 is followed to its end and dropped.
- A mark's payload is held up to 4 KiB. A longer OSC 133 is malformed: it is followed to its end
  and dropped. So any output, however hostile, costs the scanner at most 4 KiB.
- A payload with a byte below 0x20 or the byte 0x7f, a letter other than A to D, or a letter
  followed by something other than `;` or the end, is malformed and dropped.
- A gap in the stream (bytes that never arrived) resets the scanner, and a held candidate is
  dropped.

**The command list.** A pane has a list of commands, oldest first. A command starts at a `C`
mark. Its fields:

- **id**: the offset of its `C` mark. It is unique in the pane and stays the same when gff-hab
  rebuilds the list from the ring after a restart.
- **prompt**: the offset of the `A` mark before it, and **input**: the offset just after the
  `B` mark, when the shell sent them for this command.
- **output**: the offset just after the `C` mark; the command's output starts there.
- **end**: where the output ends. A `D` mark ends it at the `D` mark's first byte, so the
  command's bytes, from output to end, hold its output only, without any mark.
- **state**: `running` until it ends; `done` after a `D`; `unknown` when something other than
  a `D` closed it (see below). **exit** is the `D` mark's status, when it had one.
- **text**: the command line from `cmdline_url`, decoded, at most 4 KiB, when the shell sent it.
- **started_at**, **ended_at**: when gff-hab read the `C` and the closing mark as live output.
  Marks found in replayed bytes (after a restart) carry no time: it is unknown, never guessed.

**Rules, one state per pane.** The tracker is in one of: idle, prompt (after `A`), typing (after
`B`) or running (after `C`).

| Mark | In state | Result |
|---|---|---|
| `A` | running | The running command closes with state `unknown` at the `A` mark (`app.md` 6.2: a missing end). Then a new prompt |
| `A` | any other | A new prompt; an earlier `A` or `B` with no `C` is forgotten |
| `B` | prompt | Typing; input starts after the mark |
| `B` | idle or typing | Typing, with no prompt (or the newer input) |
| `B` | running | Ignored |
| `C` | idle, prompt or typing | A new command starts, running |
| `C` | running | Ignored: one command runs at a time |
| `D` | running | The command ends: `done`, with the exit status if present |
| `D` | any other | Ignored. A shell sends one before its first prompt, and after an empty line |

- **Nested shells.** A marked shell started from a marked shell (`bash` inside `zsh`): the inner
  shell's first `A` closes the outer command (`bash`) as `unknown`, and the inner commands
  become commands of their own. When the inner shell exits, the outer shell's `D` finds no
  running command and is ignored. A nested shell without marks joins the running command until
  marks return (`app.md` 6.2).
- **Fake marks.** A program can print marks (`cat` of a file holding them). They are taken as
  real: there is no nonce. The rules keep the list sound whatever arrives: commands never
  overlap, their offsets only grow, and a stray `D` or `B` changes nothing. A fake `A` closes
  the running command as `unknown`, and the shell's real `D` that follows is then ignored.
- **A gap.** When bytes never arrived, a running command closes as `unknown` at the gap's start,
  and the state goes to idle.

**Bounds.** A pane keeps at most 1000 commands; past that the oldest is dropped. A command whose
end has left the holder's ring is dropped too: its bytes are gone. A running command is never
dropped by the ring, only by the count. A program that prints marks in a loop therefore costs at
most 1000 small records per pane.

**Restarts.** The list is saved with the pane's checkpoint, so a restart that replays from the
checkpoint keeps it and goes on from the saved state. A screen rebuilt from the ring alone
rebuilds the list from the marks still in the ring.

**Reading a command's bytes.** `command.output` asks the pane's holder for a bounded replay
(holder protocol 6, replay with *has to*) of the command's output range, at most 1 MiB per
call. A running command's range ends at the bytes gff-hab holds now. When the ring has lost part
of the range, the answer starts later and says so. The holder protocol does not change.

Decision: proposed (this stage). Calls and the event: 3.9.

#### Built choices (task-m845kpi9)

- **Where.** `internal/cmdmarks` holds the scanner and the per-pane tracker (pure Go). The holder
  link feeds each pane's tracker from one call in its byte path, after the parser, with live and
  replayed bytes alike (`internal/link/commands.go`). `cmd/gff-hab/commands.go` serves the calls
  and sends the events. The holder and its protocol are unchanged.
- **Shapes.** `command.list` takes `pane`, `before` (a command id) and `limit` (default 100, at
  most 1000), and returns `commands`, oldest first, and `more` when older ones exist: it pages
  backwards like `chat.page`. `command.output` takes `pane`, `command` (the id), `from` (an offset;
  default the output's start) and `limit` (bytes; default and most 1 MiB), and returns `command`
  as it stood, `data`, the range `[start, end)` it covers, `gap` when the ring had lost bytes
  before `start`, and `more` when output goes on past `end` (the limit cut it, or the command
  runs). A `from` outside the command's range is clamped to it. An unknown command answers
  `not_found`; a lost pane `unavailable`. `command.output` runs off the read loop, at most 4 at a
  time per connection.
- **Times** are RFC 3339 with nanoseconds, in UTC, from gff-hab's clock when it read the mark as
  live output.
- **Events.** `command.updated` carries one `Command`; its id is the pane id, so a scope that
  holds the pane gets it. The link notes which commands changed; `gff-hab` reads the changes and
  sends them at most every 200 ms, at most 32 per pane per flush (the newest). The last event of
  a command is its final state; a client that missed some reads `command.list`.
- **Saved.** The checkpoint header gains an optional `commands` field: the list and the
  tracker's state (`phase`, the pending `prompt` and `input`). A checkpoint without it (an older
  build) starts an empty list at its offset.
- **The ring.** The link drops a command once its end is at or before the pane's offset minus
  the holder's ring size (the welcome's ring size).
- **A read the ring overtakes.** When the holder skips bytes in the middle of a bounded replay
  (the ring moved on while it sent), the bytes cannot be placed, so gff-hab asks again, up to
  three times; the next replay starts after the lost bytes and says gap.
- **Test snippet.** `cmd/gff-hab/testdata/marks` holds a bash rcfile and a zsh `.zshrc` that emit
  the marks; they are test fixtures, not the console's snippet. zsh prints its partial-line mark
  (`PROMPT_SP`, `PROMPT_CR`) before `precmd`, so that mark would land inside a command's output
  range before `D`; the fixture turns both off. The shipped snippet has to deal with it.
- **Not built here:** the working directory of a command (OSC 7), text from bash (bash sends no
  command line in its marks without more setup), and a nonce against fake marks.

---

## 3. API rules

### 3.1 Protocol

- **JSON-RPC 2.0.** Requests, responses and notifications as the spec defines them.
- **Plain mode by default:** one JSON message per line, UTF-8, no embedded newlines. A socket tool
  can drive it.
- Every result is a JSON object, never a bare value, so fields can be added later.
- Requests on one connection may run side by side. A client must match responses by id.
- Method names are grouped by prefix: `hab.*`, `structure.*`, `pane.*`, `session.*`, `project.*`,
  `data.*`, `events.*`, `devices.*`, `approval.*`, `chat.*`, `attachment.*`, `push.*`, `usage.*`,
  `rpc.discover`. The calls the clients need are listed in 3.9.
- `devices.pair`, `devices.list` and `devices.revoke` manage the hab's trust store
  (`transport.md` 5, 6). They need the `devices` capability. Until pairing is built they answer
  `unavailable`. Rename and rotate (`transport.md` 6.5) are added the same way when they are built.

Decision: proposed.

### 3.2 Handshake

The first request on a control connection must be `hab.hello`.

| hello field | Meaning |
|---|---|
| `revision` | Newest API revision the client speaks |
| `min_revision` | Oldest API revision the client accepts |
| `client` | Name, version and kind of the client |
| `label` | Optional human label ("phone", "desk") |
| `reconnect_token` | Optional; from an earlier welcome, see 3.7 |

| welcome field | Meaning |
|---|---|
| `revision` | The revision this connection uses |
| `epoch` | Random id of this gff-hab process |
| `build` | gff-hab build string |
| `host` | Host id (capabilities are in `capabilities`) |
| `capabilities` | Strings for optional features, see 3.4 |
| `client_id` | This client's id |
| `reconnect_token` | Token to resume this client within the grace period |
| `device_id` | The hab's host-key fingerprint, `SHA256:` plus base64 (`transport.md` 5). Optional in the schema, so a newer client accepts an older hab's welcome; this build always sends it |
| `seq` | Always 0: no event has been queued yet |

If no revision fits, gff-hab answers with an error of kind `revision_unsupported` that carries both
ranges, then closes. The client tells the user which side to update.

Built choices (`internal/control`):

- The chosen revision is the lower of the two newest revisions. It must not fall below either
  side's minimum.
- A first request other than `hab.hello`, a second `hab.hello`, a `client.kind` outside its word
  set, or a `revision` below `min_revision` is refused with `invalid`. A refused hello closes the
  connection.
- The hab key is an Ed25519 key in the state directory, made on first start, mode 0600
  (`internal/habkey`).

### 3.3 Revisions

- **A revision is a whole number.** It starts at 1 and rises by one.
- **Additions need no new revision.** A new method, a new optional field, a new event type or a new
  capability is added within the current revision. Clients skip unknown fields, event types and
  capabilities; the server ignores unknown optional request fields.
- **A new revision marks a change an old client cannot skip:** a removed or renamed field, a field
  whose meaning changes, a new required input, a narrower input (a removed word, or a plain string
  that becomes a word set).
- **Word sets the server sends are open.** Error kinds, owner reasons, closing reasons, session
  states and the like may gain words within a revision. Every client maps an unknown word to a
  fallback (for an error kind, treat it as a generic failure; for a reason, show it as given).
  Word sets the client sends are closed: the server refuses an unknown word with `invalid`.
- **The server translates down to a floor.** Each revision step has a translator for requests,
  responses and events. The server keeps translators back to `min_revision`. It never translates
  up: a server older than the client simply speaks the older revision, and the client must accept
  it if it lies within the client's range.
- **The schema is generated from Go types.** It is committed, served by `rpc.discover`, and used to
  generate Swift types.
- **The compat test** compares the schema with the last released schema of the same revision and
  fails on any breaking change. A breaking change passes only together with a new revision and its
  translator.

How long the floor stays is Decision: open (Q4).

The schema lives in `api/` (layout and commands: `docs/layout.md`). Built choices:

- The document is OpenRPC 1.3.2. Notifications, event types, data frames and capabilities sit in
  `x-` extensions, because OpenRPC has no place for server-sent messages.
- Event types are `<object>.updated` and `<object>.deleted` for projects, sessions and panes, plus
  `pane.owner` (6.5), `pane.activity` (7.1) and `agent.state` (harness-adapters.md, State event
  model). `agent.state` carries a per-pane revision that restarts with the epoch; the snapshot
  holds the current states in the structure's optional `agents` array.
- Word sets the server sends are open and may gain words; word sets the client sends are closed
  (see above). The compat test applies both rules.
- The compat test compares against the highest released revision. A higher current revision must
  be exactly one higher and have a translator (`api.Translators`) for every step down to
  `min_revision`.
- `rpc.discover` returns the schema document itself, so its result carries no `seq`.

### 3.4 Capabilities

Capability strings name optional features: `data-streams`, `snapshot-vt`, `snapshot-ghostty`,
`agent-state`, `transcripts`, `devices`, `push`, `host-stats`. `devices` means the embedded SSH
server is on and the `devices.*` methods work. `push` means the hab has working push config;
`host-stats` means `hab.stats` works (3.9). A client checks for a capability before it uses the
feature. The zellij backend omits `data-streams`. Capabilities are for features that a host may
lack. They are not a second versioning scheme.

Built: with the holder link on, gff-hab sends `agent-state`, `data-streams`, `snapshot-vt` and
`transcripts`.

### 3.5 Events, seq and epoch

- **Subscribe.** `events.subscribe {scope}` returns a snapshot of the objects in scope and starts
  events. Scope is `host` (everything) or a set of projects or sessions.
- **Events are JSON-RPC notifications**, method `event`, with params:
  `seq`, `type`, `id` (the object), `revision` (the object's revision after the change) and `data`.
- **Seq counts per connection, from 1, with no gaps.** Every event queued for this connection raises
  it by exactly one. A gap is a bug, and the client handles it as a backlog close.
- **Every response carries the last queued seq.** It is the seq of the last event queued for this
  connection before the request ran. Results carry it as a `seq` field; errors carry it in
  `error.data.seq`. The client applies the response, then only events with a higher seq. This
  closes the race between reading and subscribing.
- **Every object carries a revision.** It rises by one on each change to that object and is stored
  with the object, so it survives restarts. Within one epoch, a client ignores an event whose
  revision is not higher than the one it holds. Deleting an object emits a final event with a final
  revision. Revisions order events only within an epoch; they are not compared across epochs.
- **Ephemeral events carry no revision.** `pane.activity` describes state that is not stored; so
  do the activity, chat, turn and usage events of 3.9. Ephemeral means never in the snapshot.
  `pane.owner` is not ephemeral: the owner is part of the pane in the snapshot, so its event
  carries the pane's revision, and a client orders it against the snapshot it holds like any
  other pane change. Owners are still not saved; a new epoch starts with none (6.8). Their `revision` is absent, and the "ignore a revision not higher" rule does not apply:
  a client applies them in seq order.
- **Epoch.** A random id chosen at each gff-hab start, sent in the welcome. A client that reconnects
  and sees a new epoch drops everything not saved (owners, tickets, desired sizes) and takes a fresh
  snapshot. It replaces all its objects with the snapshot's, whatever revisions it held: a lost or
  unreadable state file can restart revisions lower than before.
- **Heartbeat.** A `heartbeat` notification with the current seq every 15 s when no other event was
  sent. A client that hears nothing for 45 s treats the connection as dead.
- **Client liveness.** A client sends `hab.ping` every 15 s when it has sent nothing else. gff-hab
  treats a control connection that sends nothing for 45 s as closed (reason `disconnect`).
- **Backlog limit.** If more than 8 MiB of events wait for one connection, gff-hab sends a final
  `closing {reason: "event_backlog"}` notification and closes. The client reconnects and reads
  state again.

Decision: proposed. Whether a reconnect within the same epoch can replay missed events is
Decision: open (Q5).

Built choices (`internal/control`):

- One queue per connection carries responses and notifications, so the wire order is the queue
  order. Requests on one connection run one at a time, in order.
- `events.subscribe` marks the connection subscribed and reads the seq under one lock, then takes
  the snapshot. Events that arrive meanwhile wait and get seqs above the response's seq, after the
  response. Each event is emitted after its change is stored, so every event at or below that seq
  is already in the snapshot.
- A second `events.subscribe` replaces the scope and answers with a new snapshot. Seq keeps
  counting.
- Scope `projects` covers those projects, the sessions filed in them and the panes those sessions
  link. Scope `sessions` covers those sessions and their panes. An object that leaves the scope
  sends one last event.
- The backlog counts the bytes queued plus the bytes the writer is still writing.
- On a backlog close, gff-hab drops the queued events and sends `closing` alone, so the client is
  not first made to read 8 MiB it will throw away. The flush has 5 s. `closing
  {reason: "event_backlog"}` is best effort: if the client still does not read, the connection
  closes without it. A client treats a close with no `closing` the same way: it reconnects and
  reads state again.
- Heartbeats start after the welcome. A connection that sends nothing for 45 s is closed with no
  message.

### 3.6 Errors

JSON-RPC error objects with code `-32000` for application errors. `data` always carries:

| Field | Meaning |
|---|---|
| `kind` | Stable word: `not_found`, `conflict`, `unavailable`, `revision_unsupported`, `not_owner`, `ticket_invalid`, `invalid` |
| `hint` | A sentence for a human |
| `seq` | Last queued seq, as for results |

Clients branch on `kind`, never on `message`.

### 3.7 Reconnect

- A client that loses its control connection may reconnect with its `reconnect_token`.
- Within the grace period (proposed: 60 s) and the same epoch, gff-hab gives it the same `client_id`
  and keeps its size ownership, its desired sizes and its open data tickets.
- Events from the gap are not replayed (unless Q5 decides otherwise). Seq starts again at 1. The
  client re-reads state with the seq rule.
- After the grace period, or with a new epoch, the token is refused and the client starts fresh.
  A refused token is not an error: the welcome gives a new `client_id`.
- Each welcome issues a new token; the old one stops working. A token whose connection still looks
  open is accepted, and that older connection is closed as dead.

### 3.8 Input

- Input goes over the control connection: `pane.input {pane, data}`, where `data` is base64 bytes,
  or `text` for UTF-8 convenience.
- One `pane.input` call reaches the holder as one write. Calls from different clients never
  interleave inside a call. Calls are applied in the order gff-hab receives them.
- Input counts as activity for size ownership (6.2).
- gff-hab never writes to a program's input on its own, except for answers to terminal queries (5.6).

Built (stage 7, `internal/link`): `pane.input` writes through the holder link. Each pane has one
input queue: client input and gff-hab's answers to terminal queries reach the holder in the order
they were queued. gff-hab sends each write without waiting for the holder's answer to the one
before: the holder answers behind the pane's output, so under heavy output an answer comes seconds
after its write reached the program. A refused write makes the writes sent behind it fail on their
offset; gff-hab sends those again, in order, at the offset the holder expects. Two cases fail them
instead. A later write was taken (its offset happened to match the holder's count after other
input moved it), so a resend would land out of order. Or the write's deadline passed: 10 s from
when it was first sent. A write therefore lands within 10 s of first going out, or fails. A pane
that is gone or unknown answers `not_found`; a lost pane answers `unavailable`. Each call counts
as activity for size ownership (6.2) when the connection reads it (stage 8, 6.8).

- The control connection runs `pane.input` off its read loop, in one lane per pane: calls to one
  pane keep their order, and a pane that takes its input slowly holds up neither `hab.ping`, other
  calls, nor input to other panes. One connection may have 256 calls waiting; past that a call
  answers `unavailable`.
- A lane queues each call to the pane's input queue as soon as it reads it, and answers the calls
  in order as their results come. It never waits for one call's result before it queues the next:
  each keystroke is its own call, and a result comes behind the pane's output.
- A call's 30 s timeout runs from when gff-hab read it. A call that times out while it still waits
  is never written, so a client that tries again types it once. A call already being written
  answers with that write's result.
- Answers to terminal queries are bounded per pane: at most 64 answers or 4 KiB wait (one larger
  answer may wait alone). Past that, gff-hab drops new answers. While the holder refuses the
  pane's input as queue full, gff-hab drops the waiting answers and every new one, until the
  holder says it takes input again. A burst of drops is logged once at its start and once at its
  end. Client input therefore waits behind at most one bounded batch of answers. Answers next to
  each other in the queue go to the holder as one write.

### 3.9 Calls and events the clients need

`gff-client.md` 14 and `app.md` 17 list what the terminal client and the app need. Every call
and event below is in the revision 1 schema, added under the 3.3 rule: new methods, new event
types, new capabilities, new word sets, and new optional fields on existing types.
The owning stage builds the behaviour. Until then a method answers `unavailable` with a hint that
names the stage, and an event is defined but never sent.

| Method | Purpose | Built |
|---|---|---|
| `session.create` | Start a program in a new pane and file it as a session | Yes (this stage) |
| `pane.close` | Release a pane; a running program gets a hang-up first | Yes (this stage) |
| `session.resume` | Start an exited session's harness again in a new pane, with its resume id | Part 4 stage 4 |
| `session.interrupt` | Stop the current turn through the adapter (the chat view's Stop) | Part 4 stage 4 |
| `session.send` | Send a message through the adapter, under the input rules | Part 4 stage 4 |
| `approval.answer` | Answer a pending request by id; a closed or expired id is refused with `conflict` | Part 4 stage 4 |
| `chat.page` | Read a page of chat messages from the transcript reader (capability `transcripts`) | Yes, Claude Code (Part 4 stage 5) |
| `attachment.put` | Start an upload of a file for a session | Part 6 stage 5 |
| `attachment.get` | Fetch an attachment or its thumbnail | Part 6 stage 5 |
| `session.focus` | Report focus: marks `done` seen, and holds pushes while watched | Part 6 stage 7 |
| `push.register` | Store an APNs device token with the kinds the device wants (capability `push`) | Part 6 stage 7 |
| `push.register_activity` | Store a Live Activity token for the tracked session (capability `push`) | Part 6 stage 7 |
| `push.unregister` | Drop a token (capability `push`) | Part 6 stage 7 |
| `usage.list` | The latest usage reading of each harness account | Part 4 (usage readings) |
| `hab.stats` | CPU, memory, disk, battery and uptime of the host (capability `host-stats`) | Part 5 stage 5 |
| `command.list` | A page of a pane's shell commands, found by OSC 133 marks (capability `commands`, 2.5) | Yes (task-m845kpi9) |
| `command.output` | A run of one command's output, from a bounded replay of the holder's ring (capability `commands`, 2.5) | Yes (task-m845kpi9) |

| Event | Purpose | Id | Sent from |
|---|---|---|---|
| `pane.bell` | A pane rang the bell; at most once per second per pane, with the count | Pane | Part 2 stage 7 |
| `pane.output` | A pane printed; at most once per second per pane, with its offset | Pane | Part 2 stage 7 |
| `chat.message` | A chat message was appended or changed in place | Message | Part 4 stage 5 (built for Claude Code) |
| `session.turn_end` | The agent's turn ended; names the final message | Session | Part 4 stage 4 |
| `usage.updated` | An account's usage reading changed | Account key | Part 4 (usage readings) |
| `command.updated` | A pane's command started or ended (2.5) | Pane | Yes (task-m845kpi9) |

All six events are ephemeral (3.5): they carry no revision, and a client applies them in seq
order. A chat message lives in the transcript, not in gff-hab, so it has no stored revision
either; an update repeats the message id and replaces the message. A scoped subscription gets a
`chat.message` when the message's `session` is in its scope.

Built: gff-hab sends no `revision` key on these events.

New capabilities: `push` (the hab has working push config, `app.md` 8.4), `host-stats`, and
`commands` (the `command.*` methods and the `command.updated` event work; a hab with the holder
link sends it).

Shapes the specs left open, and the choice taken (the smallest that serves both clients):

- **`session.create`** takes `name`, `project` or `project_dir`, `harness`, `argv`, `cwd`, `env`,
  `cols` and `rows`, all optional, and returns the `session` and `pane` ids. The objects arrive as
  events. Defaults: the hab's `$SHELL` (else `/bin/sh`); `cwd` is `project_dir`, else the hab's
  home; 80x24; the name is the program's base name, then `-2`, `-3` and so on. `env` entries are
  added to the hab's own environment and replace the hab's value for the same key; `GFF_HAB` is
  always the pane's own. `project_dir` finds the project by the repository's remote (`origin`,
  else the first), else its root commit, and creates the project, named after the repository
  folder, when it is new. `project` and `project_dir` together are `invalid`. `harness` needs
  the adapter and answers `unavailable` until Part 4 stage 4. Every input is checked before the
  spawn; if filing the session fails after it, the new pane is closed again. `gff-client.md`
  proposed `command`; `argv` is that command, unparsed.
- **`session.resume`** takes `{session}` and returns the same ids as `session.create`.
- **`pane.close`** takes `{pane}` (the `PaneRef` of the size methods) and returns the seq only.
- **`session.send`** returns the seq only. Whether the message waits for the turn follows from
  the pane's agent state, which the client already holds. A "not delivered" report is added as
  an event when Part 4 builds delivery.
- **`approval.answer`** `choice` is a closed set: `allow`, `deny`, or `answer` with the option or
  free text in `text`. `via` is `app`, `banner`, `watch`, `voice` or `terminal`.
- **`chat.page`** pages backwards by message id (`before`); `limit` defaults to 50, at most 200.
  The result holds `messages`, oldest first, and `more`. A message has `id`, `session`, `kind`
  (`user`, `agent`, `tool`, `event`, `plan`; open), `at`, and the optional `text`, `sender`,
  `parent` (the subagent call), `tool` (name, target, status, input, the first part of the
  output) and `plan` (steps with `pending`, `in_progress` or `done`).
  Added by the transcript reader (Part 4 stage 5), all optional: the param `after` (a cursor;
  not with `before`) returns what was added or changed since the cursor, oldest first, with
  `more` set when further changes wait; the result's `cursor` is the value to pass next; a
  message's `event` says what a kind `event` line marks (`compaction`, `permission`,
  `interrupt`, `command`, `notice`, `reset`, `unknown`; open). `reset` comes only as a
  `chat.message` event, never in a page: the transcript was replaced, and the client takes the
  tail again. An empty `cursor` means there is nothing to resume from yet: the client pages
  again. A stale cursor answers `conflict`, an
  unknown `before` answers `not_found`. The rules are in `harness-adapters.md`, Claude Code,
  built choices for the transcript reader. Golden files: `result.chat.page@2.json` and
  `event.chat.message@2.json`.
- **`attachment.put`** returns `id` and `path`. The channel the bytes take is designed in its
  stage; it may add an optional field to the result. **`attachment.get`** returns the bytes in
  `data`, with `name` and `type`.
- **`push.register`** `environment` is `development` or `production`; `kinds` are `permission`,
  `question`, `auth`, `error`, `done` and `usage`.
- **`hab.stats`** is a method, not an event: a client asks while it shows the numbers. Sizes are
  bytes; `cpu_pct` is over all cores; `battery_pct` is absent with no battery.
- **`usage.list`** returns `accounts`, each with `key`, `label`, `harness` and `windows` (`name`,
  `used_pct` in whole percent, `resets_at`, `read_at`). `usage.updated` carries one account.
- The session, request and project fields that `app.md` 17 lists are in the schema as optional
  fields. Their golden messages are a second field set (`<message>@2.json`, `docs/testing.md`);
  the first field set stays as an older revision 1 hab sent it. Each field is defined now and
  filled by the stage named here; until then a hab leaves it out.

  | Type | Field | Shape and choice | Filled by |
  |---|---|---|---|
  | `Session` | `title` | String; absent when the harness gives none | Part 4 stage 5 (transcript reader), then each adapter |
  | `Session` | `model` | String, as the harness names the model | Part 4 stage 4 (Claude Code adapter), then each adapter |
  | `Session` | `context_pct` | Integer, whole percent of the current model's window; absent when unknown, never guessed | Part 4 stages 4 and 5 (adapter and transcript reader) |
  | `PermissionRequest` | `title` | String. `app.md` 17 lists it as always present; it is optional here, because a request from an older hab has none. A client builds a title from `tool` when it is absent | Part 4 stage 4 |
  | `PermissionRequest` | `description` | String, the harness's longer description of the call | Part 4 stage 4 |
  | `PermissionRequest` | `context_line` | String, the agent's last message before the request | Part 4 stage 5 (transcript reader) |
  | `PermissionRequest` | `expires_at` | RFC 3339 string, like `AgentState.at`; absent when no deadline applies (`app.md` 7.4) | Part 4 stage 4 |
  | `Project` | `branch` | String; absent on a detached HEAD or outside a repository | Part 2, project git status (`internal/gitstatus`); built |
  | `Project` | `head` | String, the full commit id of HEAD; the app shortens it | As `branch` |
  | `Project` | `ahead`, `behind` | Integers, pointers so 0 is sent; both absent with no upstream | As `branch` |

  **How gff-hab fills the project's repository fields** (`internal/gitstatus`):

  - **Which folder.** gff-hab keeps each project's working directories, most recently used
    first, at most 8, in the state file (not in the API). `session.create` adds the repository
    top folder of `project_dir`. With `project` and a `cwd`, it adds the cwd's top folder when
    that repository has the project's identity. A project with several worktrees or clones
    shows the one most recently used for a new session; a folder that no longer exists is
    skipped for the next. A project with no known folder has empty fields.
  - **The reads.** git runs in that folder with the C locale, `GIT_OPTIONAL_LOCKS=0`
    (`--no-optional-locks`), no pager, no prompt and only `PATH`, `HOME`, `XDG_CONFIG_HOME`
    and `TMPDIR` from the hab's environment. Commands: `rev-parse --absolute-git-dir
    --git-common-dir`, `symbolic-ref -q HEAD` (the branch; an unborn branch counts),
    `rev-parse -q --verify HEAD^{commit}` (the head), `rev-parse --symbolic-full-name
    @{upstream}`, and `rev-list --left-right --count HEAD...@{upstream}`. gff-hab never
    fetches and never writes to the repository; behind counts what the last fetch brought.
  - **Edge cases.** A detached HEAD has no branch and no counts. An empty repository has a
    branch and no head. No upstream, or an upstream ref that was never fetched, gives no counts.
    A folder outside a repository clears the fields.
  - **Limits.** One read (all its git runs) has 2 s. A timeout or a git failure keeps the last
    values and is logged once until a read succeeds. Each project has at most one read
    running; at most 4 run at once on the host. Reads run off the event loop; the store is
    locked only to look up the folder and to store the result.
  - **When.** On `session.create` into the project (so on project create), on any hook event
    from a pane of the project (debounced 500 ms), every 60 s (`-git-interval`, else
    `GFF_HAB_GIT_INTERVAL`), and when a poll every 2 s sees a change in the size or time of
    `HEAD`, the branch ref, the upstream ref or `packed-refs`. No file watcher is needed.
  - **Events.** A read that changes a value raises the project's revision and sends
    `project.updated`. An unchanged read sends nothing. The last values are kept in the state
    file, so a restarted hab shows them until its first read.

  The approval request is `PermissionRequest`, which `AgentState.request` carries. The other
  `app.md` 17 rows were already in place: chat kind `plan`, `approval.answer` field `via`, and
  push kind `usage`.

**`pane.close` on a running pane.** gff-hab asks the pane's holder to release it. The holder
hangs up the pane's session (SIGHUP) and closes the terminal; it keeps the child's waiter, so
the program is reaped whenever it ends (holder spec 2.7). Neither the holder nor gff-hab
escalates to TERM or KILL: a program that ignores the hang-up lives on with no terminal until it
exits, or until the holder shuts down (holder spec 6.1). An ended pane is released the same way,
on any holder, so ended panes on the current holder do not pile up. After the release the pane is
gone (2.3): its data streams end, its checkpoint is removed, a `pane.deleted` event goes out, and
its session is marked exited. The session record stays until a client closes it. Closing a pane
that is already gone succeeds. A lost pane (its holder is not connected) answers `unavailable`;
an unknown pane answers `not_found`. The client confirms with the owner before it calls.

---

## 4. Transports

### 4.1 Local Unix socket (first)

- gff-hab listens on one Unix socket in its runtime directory, owner-only permissions.
- It opens **no TCP port** by default. One machine works alone with this transport only.
- The control connection and each data connection are separate connections to the same socket. The
  first message tells which one it is: `hab.hello` for control, a data hello with a ticket for data.
- Authentication is the socket's file permissions plus a peer-credential check that the caller runs
  as the same user.

Decision: proposed.

Built: the socket is `gff/hab.sock` under the user config directory on macOS and under
`$XDG_RUNTIME_DIR` elsewhere (`$GFF_HAB_SOCKET` or `-socket` override it). Its directory must be
mode 0700 and owned by the user. gff-hab peeks at the first byte of each connection: the hello
frame's kind byte (1) makes it a data connection, anything else a control connection. With the
holder link off, a data hello gets a reject frame (`unavailable`).

### 4.2 `gff-hab connect` over SSH stdio

- `gff-hab connect` bridges its stdin and stdout to the local socket. `ssh <host> gff-hab connect`
  reaches a remote hab with no new listener. SSH keys stay the authentication.
- The bridge is dumb: it copies bytes. It does not parse JSON-RPC. When stdin ends it half-closes
  the socket; it exits when gff-hab closes the connection.
- `gff-hab api call -via <command>` and `gff-hab events -via <command>` run a command such as
  `ssh <host> gff-hab connect` and speak over its stdio.
- **How data connections ride it** is decided: option A (Q2). The options were:
  - **A. One SSH channel per connection.** The control connection is one `gff-hab connect`
    session. Each data connection is another session on the same SSH connection
    (`gff-hab connect --data`). The Go client opens several session channels on one SSH
    connection; OpenSSH users get the same through connection sharing.
  - **B. Framed mode on one channel.** After the welcome, the control connection switches to
    frames, as herdr-core does. Channel 0 carries JSON-RPC; each data stream gets its own channel,
    with credit flow so one stalled pane cannot block the rest.
  - **C. Forward the socket.** The client forwards the remote Unix socket to a local one, then
    opens ordinary connections to it.
- **Recommendation: A.** gff-hab keeps one connection model on every transport, the bridge stays
  dumb, and SSH gives each channel its own flow-control window. Its cost: OpenSSH allows 10
  sessions per SSH connection by default, so a client showing more than nine panes on one host
  opens a second SSH connection; the embedded server (4.3) lifts that cap. B is the fallback if a
  client cannot open many channels.
- **Built: A**, while Q2 stays open for the owner. Because the socket tells a data connection
  from a control connection by its first byte (4.1), the dumb bridge carries either. A data
  connection over SSH is one more `ssh <host> gff-hab connect` session; no `--data` flag is
  needed. `TestDataResumeAfterDrop` in `cmd/gff-hab` resumes a stream through `gff-hab connect`.
- The welcome carries the hab's host-key fingerprint as `device_id` (`transport.md` 5). The client
  pins it on first contact and refuses a changed id until the owner accepts it. A welcome with no
  `device_id` leaves the hab unpinned (`transport.md` 4.1).

### 4.3 Embedded gff-hab SSH server (Part 3)

- gff-hab can run its own SSH server, off by default, on one address the owner names. Its
  authentication, QR pairing and revocation live in `transport.md` (4.2, 6). The owner approves it
  before any real host enables it.
- It carries the same connections as 4.2: the control connection and each data connection are
  channels on one SSH connection, as in option A. It serves the API subsystems only.
- It sets its own channel limit, so one connection carries one channel per visible pane.
- The transport reports the authenticated device id to gff-hab, so events and input record who
  sent them. Data tickets are bound to that device.
- WebSocket stays possible only as a later carrier for a client that cannot speak SSH. If it comes,
  it carries one text message per JSON-RPC message on the control connection and one binary
  message per data frame on a data connection.

Decision: settled (`transport.md`, /consensus 3/3).

---

## 5. Data streams

### 5.1 Shape

- One data connection per visible pane per client. A client that shows no terminal opens none.
- Output flows to the client only. Input goes over the control connection (3.8).
- The stream starts with a snapshot, then carries the program's raw bytes.

### 5.2 Opening a stream

1. The client calls `data.ticket {pane, observe?}` on its control connection. The result is a
   single-use ticket, valid for 30 s, bound to this client.
2. The client opens a data connection and sends a data hello with the ticket and, to resume, the
   offset it holds (5.4).
3. gff-hab answers **welcome** (stream id, pane size, owner) or **reject** (`ticket_invalid`,
   `not_found`), then closes on reject.

### 5.3 Framing

Every message on a data connection is a frame: a kind byte, a length, and a payload. On a
WebSocket, one binary message is one frame.

| Kind | Direction | Payload |
|---|---|---|
| hello | client to hab | Ticket, resume offset |
| welcome / reject | hab to client | JSON |
| snapshot | hab to client | Offset it covers, encoding, screen bytes |
| bytes | hab to client | Offset of the first byte, then program output |
| resize | hab to client | Columns, rows, owner client id, reason |
| exited | hab to client | Exit code or signal |
| credit | client to hab | Bytes consumed |

Program output is always inside a bytes frame, so a program cannot forge a resize or exited
message. This replaces Rex's in-band escape sequence with a nonce. Whether to frame or to use an
in-band nonce is decided: frames (Q3).

### 5.4 Offsets and resume

- **The offset is the holder's absolute byte offset for the pane**, counted from the pane's first
  output. It is not the count of bytes the client received. gff-hab removes answered terminal
  queries (5.6), so the two counts differ; the holder offset does not.
- Every snapshot frame names the offset it covers. Every bytes frame names the offset of its first
  byte. The client keeps the offset after the last byte it applied.
- **Resume:** the client sends that offset in its data hello. If gff-hab can still serve bytes from
  that offset (from its own buffer or the holder's ring), it continues from there with no gap and no
  repeat. Otherwise it sends a fresh snapshot first.
- A resume never sends bytes the client already has.

### 5.5 Snapshot

- gff-hab builds the snapshot from the pane's parser: the screen, scrollback the client asked for,
  the title, the modes that matter (alternate screen, cursor style, bracketed paste, mouse modes,
  keyboard protocol).
- **Encoding.** `snapshot-vt` is plain VT bytes that redraw the screen in any terminal. It works for
  the Go client's pass-through view. `snapshot-ghostty` is libghostty's own format, which keeps more
  state. Which one is the baseline is Decision: open (Q6); this spec recommends `snapshot-vt` as the
  baseline, with `snapshot-ghostty` offered as a capability.
- A snapshot never contains a terminal query.
- A `snapshot-vt` never sets the palette (OSC 4) or the working directory (OSC 7). On a
  pass-through client those would change the user's own terminal.
- On the alternate screen, a `snapshot-vt` draws the primary screen first, then switches to the
  alternate screen, so a client that later leaves it sees the primary screen.
- The scrollback bound applies to `snapshot-vt`. A `snapshot-ghostty` always carries the whole
  scrollback, because libghostty's format has no bound.
- **Bounded by default.** A subscription's snapshot, and the snapshot a jump past lost bytes
  sends, carry the screen and at most 1000 scrollback rows (`link.Config.SnapshotScrollback`):
  about 0.14 MiB at 120 columns, where the whole 10 MiB scrollback is 1.3 MiB (2.3).
- **More scrollback on demand.** `Link.Scrollback(pane, skip, n)` returns up to n older rows
  (at most 10,000), oldest first, that end skip rows above the newest scrollback row, with the
  pane offset it read them at and the scrollback's row count. A client passes the number of
  scrollback rows it holds as skip. The rows are VT bytes: text, colours and styles, rows joined
  by CRLF, then an SGR reset; no cursor move, mode or screen switch. On the alternate screen the
  rows come from the primary screen's scrollback. If the client's offset differs from the
  returned one, output scrolled rows in meanwhile; the client asks again once caught up. The
  data connection stage exposes this as a client's "fetch more when scrolling".

### 5.6 Terminal queries

Programs ask the terminal questions: primary and secondary device attributes, device status, cursor
position, mode queries, terminal version, colour queries (foreground, background, cursor, palette),
keyboard protocol queries, termcap queries, window size reports.

- **gff-hab answers every query once.** It answers from its own parser (cursor position, modes,
  size) and from the owner client's display facts (colours). With no owner, it answers colours
  from a configured default.
- **gff-hab removes each answered query from the bytes it sends to clients.** No client ever sees
  one, so no client answers one. Two clients on one pane cannot send two answers.
- **Replayed bytes get no answer.** During a restart replay or a crash rebuild, gff-hab answers
  nothing: those queries were answered by the previous process or are stale.
- **Bytes that arrived while no gff-hab ran get no answer.** A late answer would land in the
  program's input as typed text. Programs that query use timeouts and carry on. Whether the holder
  should answer a fixed few queries itself in that window is Decision: open (Q7).
- **Focus reports** (focus in and out, when a program asks for them) come only from the size owner's
  client. Other clients' focus changes are dropped.
- **Queries gff-hab does not recognise reach every client** (5.8 lists them: OSC 52 clipboard
  reads, `CSI 14 t` and `CSI 16 t`, a query with a C0 byte inside its CSI, a query cut by a gap).
  Each client's parser must discard its own replies to those queries, and the client stages must
  build this. Otherwise two clients on one pane answer twice, and an OSC 52 read could send a
  client's clipboard to the program.

### 5.7 Backlog and flow

- A stream starts with 256 KiB of credit. The client returns credit as it applies bytes.
- gff-hab never queues more than the credit for a stream. A stalled stream never blocks the control
  connection or other streams.
- A client more than 4 MiB behind gets a fresh snapshot instead of the backlog.
- gff-hab keeps a per-pane buffer of recent output for resumes (proposed: 4 MiB, at least as large as
  the holder ring). A resume from before the buffer gets a snapshot.

Decision: proposed.

### 5.8 Built choices (stage 7)

Data streams (`internal/datastream`), query answering (`internal/termq`, `internal/link`):

- **Tickets.** `data.ticket` checks that the link follows the pane (else `not_found`) and returns
  a random 128-bit ticket. It works once, for 30 s, and records the client id and `observe`. A
  ticket that is unknown, used or expired is rejected with `ticket_invalid`.
- **Hello.** The hello frame must come within 10 s. A pane that is gone or unknown is rejected
  with `not_found`; a lost pane, or one whose replay does not end within 30 s, with
  `unavailable`. Any other bad first frame gets `invalid`. gff-hab closes after a reject.
- **Frame layouts confirmed as proposed** in `api/methods.go`: a snapshot payload is the 8-byte
  big-endian offset it covers, a 1-byte encoding name length, the name, the screen bytes; a bytes
  payload is the 8-byte big-endian offset of its first byte, then output. The encoding is always
  `snapshot-vt` for now. Payloads are at most 64 MiB; a bytes frame carries at most 64 KiB.
- **Each bytes frame covers one contiguous run of holder offsets.** A removed terminal query is a
  skip between two frames, never inside one. So the client's offset, the frame's offset plus its
  length, is always exact, and a resume from it neither repeats nor skips a byte.
- **Order.** Welcome (stream id, size, owner), then a snapshot unless the resume
  is served from bytes, then bytes, resize and exited frames in stream order. An `exited` frame
  comes after the program's last bytes, then gff-hab closes the connection (7.7).
- **Resume.** From the link's buffer, else a bounded replay from the holder's ring, else a
  snapshot (2.3). An offset past the pane's output (a client that kept an offset from another
  pane's life) gets a snapshot too.
- **Credit.** It counts the screen bytes of snapshot frames and the output of bytes frames, not
  their offsets and names (`CreditFrame.bytes`). A bytes frame is split to fit the credit. A
  snapshot cannot be split, so it may take the credit below zero once; nothing more is sent
  until the client returns enough. Resize, exited and reject frames cost nothing. The credit a
  stream holds is capped at 4 MiB (the behind limit): credit a client returns past the cap is
  ignored, so no client can make gff-hab queue more than that for it.
- **Behind.** Output that waits for credit counts towards 4 MiB. A snapshot does not count: it
  cannot be split or skipped, so a snapshot larger than 4 MiB is still sent. Past the limit,
  gff-hab drops the waiting frames and ends the stream's link subscription, so a stalled client
  costs nothing. When credit comes back,
  it subscribes again from a snapshot at the pane's current offset. A stream that falls behind
  the link's own queue does the same. The writer has its own goroutine and each data connection
  its own socket, so a stalled stream blocks neither the control connection nor another stream.
- **End without an exit.** When a stream cannot go on (its subscription ends because the pane
  is lost or gone, or a resubscribe after falling behind fails), gff-hab sends a reject frame
  (`not_found` for a pane that is gone, else `unavailable`) and closes. After an exited frame no
  reject follows.
- **Resize frames.** The link resizes the PTY and then the parser, and every stream gets a resize
  frame at the offset the parser had reached. When the stream's scanner holds back the start of
  a possible query from before that offset, the resize frame waits until those bytes go out, so
  it always follows every byte before its offset. Each resize frame names the owner and a reason
  (6.8).
- **Observers** (6.4) are recorded on the ticket. A data connection carries no input and no size,
  so an observer can do neither by construction; 6.8 keeps an observer's size out of
  ownership.
- **On a planned stop** gff-hab closes every data connection after the control connections, then
  checkpoints; clients resume by offset (7.4).

Terminal queries (5.6):

- **One scanner shape.** Every query starts with ESC, and an ESC ends any escape sequence or
  control string a terminal parser is in. A scanner that holds nothing back is therefore in the
  same state as a new one. A stream may restart at any frame boundary, and the link's scanner
  and each stream's scanner agree on what is a query. The scanner holds back the bytes of a
  sequence that may still be a query, at most 64 bytes of CSI or 512 of OSC and DCS.
- **Answered and removed:** DSR (`CSI 5 n`), cursor position (`CSI 6 n`, `CSI ? 6 n`), primary,
  secondary and tertiary device attributes, XTVERSION (`CSI > q`), mode queries (`CSI ? Ps $ p`,
  `CSI Ps $ p`), the text area size in cells (`CSI 18 t`), the keyboard protocol flags
  (`CSI ? u`), colour queries (OSC 10, 11 and 12 with `?`, and OSC 4 when every entry asks), and
  DECRQSS and XTGETTCAP (answered "not supported").
- **From the parser:** the cursor, the size, mode states and keyboard flags. `vt.Terminal` has
  `Cursor`, `Mode` and `KeyboardFlags` for this; libghostty supplies all three, the fake knows the
  alternate screen modes only. The link writes the parser up to the end of each query before it
  answers, so the cursor is where the query found it. Colours come from a default dark theme with
  the xterm palette (`link.Config.Colors`); the owner client's display facts are a later stage.
- **Live only.** The link answers queries in live output. Replayed bytes get no answer, but every
  stream removes every query, replayed or live, so a client never sees one.
- **Left to the clients:** pixel size reports (`CSI 14 t`, `CSI 16 t`), clipboard reads
  (OSC 52), a query with a control byte inside its CSI, and a query cut off by a gap.

---

## 6. Pane size

### 6.1 One owner

- A PTY has one size. Each pane has at most one **owner** client, and the PTY takes the owner's size.
- Every other client renders the pane at the owner's size itself: it scales, letterboxes or scrolls.
  With raw bytes and a local parser, that is local work.
- The chat view does not depend on pane size.

### 6.2 Typing takes ownership

- Input from a client that is not the owner starts a debounce of about 300 ms.
- If that client is still the latest source of input when the debounce ends, it becomes the owner
  and its last desired size applies.
- Each further input from the current owner does nothing to ownership.
- This stops the size from flapping when the owner types on the phone, then the Mac, then the phone.

### 6.3 Explicit control

| Method | Effect |
|---|---|
| `pane.take {pane}` | Become owner now (reason `take`) |
| `pane.request {pane}` | Become owner only if the pane has none (reason `request`) |
| `pane.disown {pane}` | Give up ownership; the pane keeps its size and has no owner (reason `disown`) |
| `pane.set_desired_size {pane, cols, rows}` | Store this client's wanted size; applied at once if this client is owner, or the pane has no owner |

This spec uses `disown` throughout, following Rex, not "release". One word per concept.

### 6.4 Observers

- A data connection opened with `observe: true` (a thumbnail, a dashboard) never takes ownership
  and never sets a desired size.
- Input from a client that follows a pane through observer streams only is written to the
  program like any input, but it never counts toward ownership (6.2).
- An observer's own view size never reaches the PTY.

### 6.5 Owner changes

- Reasons: `create` (the client that created the pane), `take`, `request`, `typed` (the debounce in
  6.2), `disown`, `lost` (reserved: in the word set, never sent; see 6.8), `disconnect` (the
  owner's connection closed), `restore` (the owner reconnected within the grace period).
- **On disconnect** the pane keeps its size and has no owner. No resize happens, so the program does
  not reflow because a phone dropped. A size the owner wanted that is still waiting in the
  throttle (6.6) is dropped too; the same holds for `disown`.
- **On restore** the owner gets the pane back, unless another client took it meanwhile.
- gff-hab keeps each client's last desired size, newest first.
- Every owner change emits one event with the reason. Every size change also reaches every data
  stream on the pane as a resize frame with the owner.

### 6.6 Owner resizes

A window drag sends many sizes. gff-hab applies the owner's latest desired size at most once per
50 ms, trailing, so the program gets the final size and not every step.

### 6.7 Redraw

`pane.redraw {pane, mode}`:

- **`snapshot`** (default): gff-hab sends a fresh snapshot on that client's data stream only. Nothing
  reaches the program.
- **`nudge`**: gff-hab first sends the snapshot, then resizes the PTY to one column fewer and back,
  about 100 ms apart. The kernel signals a size change only when the size really changes, and a
  program that reads the size late must see two different sizes to redraw. The final size is
  unchanged.
- **Never Ctrl+L.** A redraw writes no bytes to the program's input. Ctrl+L is input, and harness
  interfaces may bind it.
- gff-hab may nudge on its own once when a pane's screen is marked suspect after a crash rebuild
  (2.3), and only if the pane has an owner connected.

Decision: proposed.

### 6.8 Built choices (stage 8)

Pane size ownership (`internal/panesize`), wired to the holder link, the structure store and the
data streams in `cmd/gff-hab/data.go`:

- **One event per change.** Every owner change emits one `pane.owner` event and raises the
  pane's revision; `Pane.owner` carries the owner in the structure. The event's `reason` names
  how the change came about (`create`, `take`, `request`, `typed`, `disown`, `disconnect`,
  `restore`); its size is the size the pane gets. A takeover is one event with the new owner,
  so `lost` is reserved: it stays in the word set and is never sent. The client that lost the
  pane sees another owner in the event. The owner is not saved; a new gff-hab starts a new epoch with no owners.
- **Creator.** The client whose `session.create` made the pane owns it (`create`), with the
  pane's size as its desired size.
- **Debounce.** The 300 ms starts at the first input from a client that is not the owner;
  further input from that client does not restart it. Input from the owner stops it. Input
  from a third client starts that client's own debounce, so the 300 ms restarts from that
  input: while two clients other than the owner take turns typing, neither takes the pane.
  Input counts when the connection reads the `pane.input` call.
- **Explicit control.** `pane.take` and `pane.request` return the owner after the call; a take
  by the owner and a request while another client owns change nothing and emit nothing.
  `pane.disown` from a client that is not the owner answers `not_owner`.
- **Desired size.** `pane.set_desired_size` stores the size (1 to 65535) and answers
  `applied: true` when this client owns the pane or the pane has none: the PTY gets this size,
  or a newer one, within the throttle. Sizes go to the holder at most once per 50 ms, trailing.
- **Observers.** A client is an observer of a pane when its open data streams on the pane, the
  streams it is opening and its unexpired data tickets for the pane (5.2) are all observers
  (at least one). So a thumbnail is an observer from its `data.ticket` call on, not only once
  its stream is open. Its input is written to the program but does not count toward ownership,
  `pane.set_desired_size` stores nothing and answers `applied: false`, and `pane.take`,
  `pane.request` and a nudge answer `invalid`. A client with a full stream or full ticket for
  the pane, or with neither stream nor ticket, is not an observer.
- **Disconnect and restore.** A client's connections are counted, so an old connection closed
  by a reconnect token (3.7) does not end its ownership: the new connection is counted before
  the old one is closed. When its last connection closes, each pane it owns keeps its size and
  has no owner (`disconnect`). A client that says hello again with its token within the grace
  period (60 s, the reconnect grace) gets each such pane back (`restore`), unless any owner
  change happened on the pane meanwhile. After the grace period its desired sizes are dropped. A size waiting in the throttle when the pane loses its owner
  is dropped, so it never lands after a disconnect or a disown. A pane the holder link loses
  forgets its owner; when it comes back, it has none until a client takes it.
- **Resize frames.** Every resize frame carries the owner and the reason of the last owner
  change (`disown` for a pane that never had an owner). An owner change also sends a resize
  frame at the current size, so every stream learns the owner in band even when the size
  stays; when the new owner's size differs, a second frame follows with that size. The welcome
  carries the owner. A snapshot that is not the stream's first (a redraw, or a client that fell
  too far behind) is followed by a resize frame, since the frames it replaces may have held a
  size change. A size change also sends `pane.updated` with the new size.
- **Redraw.** `snapshot` drops the frames waiting for credit on that client's streams of the
  pane and sends a fresh snapshot on each; other clients' streams get nothing. `nudge` does
  the same, then resizes the PTY one column narrower (one wider at 1 column) and back after
  100 ms; a size wanted meanwhile is applied in place of the way back. A nudge goes through the
  same throttle as the owner's sizes. Neither writes to the program's input.
- **Not built here:** `pane.activity` (7.1), focus reports from the owner only (5.6), the
  owner's display facts for colour answers (5.8), and the nudge after a crash rebuild (6.7).
  The nudge after a crash rebuild is the first follow-up.
- **Not saved.** An owner change raises the pane's revision in memory and never writes the
  state file, so no caller waits on the disk; the revision reaches the file with the next
  saved change.

---

## 7. Edge cases

### 7.1 Two clients typing at once

- Both clients' input reaches the program, in the order gff-hab receives it. A single input call
  is never split.
- There is no input lock. Ownership follows 6.2; the debounce stops size flapping.
- Every input emits a throttled `pane.activity {client}` event (at most once per second per client),
  so each client can show who else is typing.
- Bracketed paste markers come from the client that pastes. gff-hab does not add or remove them.

### 7.2 A phone that drops for ten minutes

1. The phone's connections die without a close. gff-hab notices after 45 s with no ping (3.5), or
   sooner if the TCP or SSH connection fails.
2. The owner leaves with reason `disconnect`. The pane keeps its size.
3. The 60 s reconnect grace ends. The phone's client id, desired sizes and tickets are gone.
4. Ten minutes later the phone reconnects: new client id, maybe a new epoch if gff-hab restarted.
5. It reads state with the seq rule, then opens data streams with the offsets it holds.
6. If gff-hab still has bytes from those offsets, the streams continue; otherwise each starts with a
   snapshot. A busy agent pane will usually need a snapshot after ten minutes.
7. The phone gets ownership back only when the user types (6.2). Notifications during the gap went
   through the notification service, not this API.

### 7.3 A client older than the server

- If the client's newest revision is at or above the server's floor, the server translates every
  message to the client's revision. Fields and events the old revision lacks are dropped or mapped.
- If it is below the floor, the hello fails with `revision_unsupported` and both ranges. The app
  shows "update the app"; the CLI prints the same.
- A client newer than the server speaks the server's revision if it lies within the client's range.
  Features from later revisions stay off; capabilities say which.
- A TestFlight build can lag the server by weeks. The floor policy (Q4) must cover that.

### 7.4 gff-hab restarts while clients are connected

- Every connection closes. Clients reconnect, see a new epoch, and take a fresh snapshot.
- Data streams resume by holder offset. The new gff-hab rebuilt its parsers from checkpoints and can
  serve bytes from the holder ring, so a resume needs no snapshot while the ring still reaches the
  client's offset.
- Ownership is gone. The next typing takes it again.

### 7.5 A holder drains

- Panes on the old holder keep working. gff-hab routes their input and resizes to the old holder.
- New panes start on the new holder. Clients see no difference except a holder build field on the
  pane.

### 7.6 A program prints something that looks like a control message

It stays inside a bytes frame and reaches the client as program output. No frame boundary can be
forged from inside a payload.

### 7.7 A pane exits

- The holder reports the exit. gff-hab sends an `exited` frame on every data stream for the pane,
  then closes them, and emits a session event with the exit status.
- The session record stays, marked exited, until the owner closes it or a rule removes it.

### 7.8 Event backlog on a slow link

A client on a slow link that falls 8 MiB behind on events is closed with `event_backlog`. It
reconnects and reads state. Data streams are not affected, because they have their own credit.

### 7.9 Several hosts

Each hab has its own seq and epoch. A client merges hosts by host id and never compares seq across
hosts. No hab relays another hab's events in this part.

---

## 8. Open questions

Each question has options, a recommendation and the reason. All are **Decision: open** for the
owner.

### Q1. The interim path (blocks stage 9)

Run with the Part 1 holder decision; the two answers must agree. The plan calls for `/consensus`.

| Option | For | Against |
|---|---|---|
| A. The earlier GFF server (TypeScript) serves the final API over zellij | Works soon; already reads transcripts and projects | Two implementations of one API; 1 s polling floor; zellij's smallest-client size policy |
| B. A new Go gff-hab with a zellij backend (stage 9) | One implementation; the backend swaps to the holder by config | Slower to first use |
| C. No interim: build the holder first | No throwaway work | Nothing new is usable until Part 1 ships |

Recommendation: **B.** One Go codebase serves the final API from day one. The zellij backend has no
data streams, and the terminal view attaches a real zellij client, as the interim plans already
found. The earlier GFF server keeps running unchanged until gff-hab replaces it. The design doc's
"first version" (option A) predates the finding that GFF's server is TypeScript, not Go.

Decision: open.

### Q2. Data connections over SSH stdio (stage 1 of the plan)

Options and recommendation in 4.2. Recommendation: **A, one SSH channel per data connection**,
because gff-hab keeps one connection model and the bridge stays dumb. Fallback: B, framed mode.

Built: A (4.2), pending the owner's decision.

Decision: A, one SSH channel per data connection (owner, with the SSH-everywhere consensus: one SSH connection per host, one channel per visible pane).

### Q3. Control messages in the data stream

| Option | For | Against |
|---|---|---|
| A. Frames on every transport (5.3) | A program cannot forge a frame; offsets are explicit; the same frames work on a socket, an SSH channel and a WebSocket | The client cannot pipe the stream straight into a terminal; it must unwrap frames |
| B. In-band escape sequences with a per-stream nonce (Rex) | The raw stream stays a valid terminal stream | Every client must scan for the sequence; offsets need their own message |

Recommendation: **A.** We need framing on the Unix socket and SSH anyway, and it makes the nonce
unnecessary. Stage 7's acceptance check "a program prints a fake control message; the client
ignores it" still applies and passes by construction.

Built: A (5.8), pending the owner's decision. `TestDataFakeFrames` in `cmd/gff-hab` prints frame
headers and a JSON-RPC `closing` from a program; the client gets them as output.

Decision: A, frames on every transport (coordinator ruling): a program cannot forge a frame, offsets stay explicit, and one framing works on every transport.

### Q4. The revision floor

| Option | Meaning |
|---|---|
| A. Keep translators for the last N revisions (for example 3) | Simple to test |
| B. Keep translators for 90 days after a revision is replaced | Matches the TestFlight lag |
| C. Keep translators until no connected client has used the revision for 30 days | Measured, but needs usage records |

Recommendation: **B**, with a minimum of two revisions. The app on TestFlight is the oldest client
in practice, and days fit that better than counts.

Decision: open.

### Q5. Event replay on reconnect

| Option | For | Against |
|---|---|---|
| A. No replay (Rex, herdr-core): the client re-reads state | Simple; the seq rule already closes the race | A phone on a bad link re-reads often |
| B. A per-process ring of recent events; a reconnect within the same epoch resumes from a seq | Cheaper reconnects | Needs a process-wide seq beside the per-connection seq; more to test |

Recommendation: **A** for the first release. Structure is small; a re-read is cheap. Revisit if the
phone's reconnect traffic measures high.

Decision: open.

### Q6. Snapshot encoding baseline

| Option | For | Against |
|---|---|---|
| A. Plain VT bytes baseline, Ghostty format as a capability | Any client can draw it, including a pass-through Go view | Loses some state (exact scrollback attributes, some modes) |
| B. Ghostty format only (Rex) | Exact state; same parser everywhere | Every client must embed libghostty, including the Go client and scripts |

Recommendation: **A.** The Go client's single-pane view passes bytes to the host terminal, and the
CLI should not need libghostty. The app can ask for the Ghostty format.

Decision: open.

### Q7. Terminal queries while gff-hab is down

| Option | For | Against |
|---|---|---|
| A. Nobody answers; programs time out | The holder stays a pure byte pump | A program that waits without a timeout hangs until gff-hab returns |
| B. The holder answers a fixed set (primary device attributes only) while no gff-hab is connected | Covers the common startup query | The holder starts scanning bytes, against "parses nothing"; it must also tell gff-hab which queries it answered |

Recommendation: **A.** gff-hab restarts take seconds. Most programs query with a timeout. Revisit if
a supported harness hangs. Align with the holder spec's OSC 133 decision, which faces the same
"scan or not" question.

Decision: open.

### Q8. Reconnect grace period

Proposed 60 s. Longer keeps ownership through a short tunnel or a lift; shorter frees panes from a
phone that will not return. Recommendation: **60 s**, configurable.

Built (stage 8): size ownership uses the reconnect grace (`control.DefaultReconnectGrace`, 60 s),
so a client keeps its sizes and its claim to restore exactly as long as its token. Not yet a
flag.

Decision: open.

---

## Notes for later stages

- Stage 2 (schema): the hello, welcome, error `kind` list, event envelope and data frame kinds above
  are the first types.
- Stage 4 (holder link): 2.3.
- Stage 5 (structure): 2.2.
- Stage 6 (control connection): 3 and 4.1, 4.2.
- Stage 7 (data connections): 5.
- Stage 8 (pane size): 6.
- Stage 9 (zellij backend): 2.4 and Q1.
