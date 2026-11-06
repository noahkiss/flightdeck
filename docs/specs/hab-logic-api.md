# gff-hab logic and API: behaviour spec

Status: draft for review. Part 2, stage 1.

This spec says how `gff-hab` behaves and how clients talk to it. Stages 2 to 9 of Part 2 build
from it. It describes behaviour in our own words. It copies no code from the reference projects.

The holder side of the link (wire format, ring size, drain, service install) belongs to the holder
spec, `docs/specs/hab-holder.md`. This spec covers only what `gff-hab` does on its side of that
link.

Each section that settles a question ends with a **Decision** line. "Decision: proposed" means this
spec recommends it and the build can start from it. The questions once open are all decided now;
their options and reasons stay in [Open questions](#8-open-questions).

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
- The header also holds the pane's newest root OSC 7501 report, as an optional `report` field,
  unless a shell prompt (OSC 133 `A`) came after it. The replay starts at the checkpoint's
  offset, past the bytes that held the report, so without it a restarted gff-hab would have no
  agent state for the pane until the next hook (`harness-adapters.md`, Edge cases). A checkpoint
  without it (an older build) hands on only a report found in the replayed bytes.
- A checkpoint is taken only where the output stream is in the parser's ground state: not inside
  an escape sequence, a control string or a UTF-8 character. A snapshot cannot carry a half-parsed
  sequence. A small scanner in the link tracks this; mid-sequence, the tick is skipped, and a
  planned stop keeps the older checkpoint.
- A checkpoint is restored only onto a pane of the holder that wrote it.
- Cadence: when a pane has printed nothing for 1 s, and at least every 10 s while it keeps
  printing; only when the screen changed. Both are configurable.
- One event loop applies every holder frame in the order the holder sent it, replay data and
  live output alike, so a pane's parser always sees its bytes in offset order.
- **A resize sizes the parser first.** The link resizes a pane's parser on the event loop, sends
  every subscriber a resize at the offset the parser has reached, and only then sends the resize
  to the holder. The holder has no frame that answers a resize with its output offset, so the
  link cannot place the resize exactly in the stream. Resizing the parser first, as terminal
  multiplexers do, means a program that redraws at once on SIGWINCH (vim) always has its redraw
  parsed at the new size. Resizing it on the holder's answer let that redraw be parsed at the old
  size, which left every client's screen wrong until the next redraw.
- **Limit:** output the program printed before the PTY resized, which the link had not yet
  parsed when it sent the resize, is parsed at the new size. Those are the few bytes in flight at
  that moment, and the program's redraw follows them.
- One resize of a pane runs at a time, from the parser's resize to the holder's answer, so the
  parser and the PTY end at the same size. When the holder refuses a resize (an error frame), the
  link puts the parser back to its old size and sends every subscriber a resize to it, unless the
  pane moved to another holder connection or took another size meanwhile. When the connection
  ends instead, the pane is lost, and its return resizes the parser to the size the holder
  reports. A resize whose caller gives up after it was sent stays: the holder still applies it.
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

gff-hab talks to its terminals through one backend: gff-hab-holder. It gives the pane list,
events from the holder, raw bytes and data streams.

An interim zellij backend was studied and dropped: there is no interim (Q1).

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
- A mark's payload is held up to 4 KiB. Once a payload starts `133;`, a longer one is cut there:
  the rest is followed to its end and not held, and the mark is kept, with its last param cut. A
  long command line therefore still makes a command, and its `D` still ends it. A byte below 0x20
  (or 0x7f) past the cut still makes the mark malformed. So any output, however hostile, costs
  the scanner at most 4 KiB.
- A payload with a byte below 0x20 or the byte 0x7f, a letter other than A to D, or a letter
  followed by something other than `;` or the end, is malformed and dropped.
- A gap in the stream (bytes that never arrived) resets the scanner, and a held candidate is
  dropped.
- Inside DCS, SOS, PM or APC, only an ESC can start an OSC, and that ESC (not followed by `\`)
  aborts the string first, as in a terminal's parser and the link's ground-state scanner. So
  tmux passthrough, which doubles the ESC, carries a mark to gff-hab as it would to a terminal.

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
- **text**: the command line from `cmdline_url`, decoded, when the shell sent it. It is at most
  1 KiB, cut at a UTF-8 character boundary; **text_truncated** says it was cut, at 1 KiB or by
  the payload cap. A percent escape that the payload cap cut in two is dropped.
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
- **A nonce is a hard gate.** Without one, a `cat`ted file can forge a command's text and exit
  status. Display may use them. No feature may act on a mark's `text` or `exit` (a rerun that
  types `text` into the pane, approvals keyed on an exit status) until marks carry a nonce that
  the console snippet sets and gff-hab checks.
- **A gap.** When bytes never arrived, a running command closes as `unknown` at the gap's start,
  and the state goes to idle.

**Bounds.** A pane keeps at most 1000 commands; past that the oldest is dropped. A command whose
end has left the holder's ring is dropped too: its bytes are gone. A running command is never
dropped by the ring, only by the count. A program that prints marks in a loop therefore costs at
most 1000 records of at most about 1.2 KiB each per pane. Dropping the oldest is O(1) over time,
so such a loop costs a small multiple of plain text to follow (27x to 41x, against about 200x
when each drop moved the whole list), not a stall of the link's event loop.

**Restarts.** The list is saved with the pane's checkpoint, so a restart that replays from the
checkpoint keeps it and goes on from the saved state. A screen rebuilt from the ring alone
rebuilds the list from the marks still in the ring.

**Shells.** zsh, and bash 4.4 or newer. bash sends `C` from `PS0`, which bash 4.4 added. An older
bash (macOS ships bash 3.2 as `/bin/bash`) ignores `PS0`, so it sends `A`, `B` and `D` but never
`C`. The tracker starts a command only at `C`, so such a pane lists no commands; this is not an
error. The console's bash lines say "bash 4.4 or newer" (`app.md` 6), and the end-to-end tests
skip below 4.4 and say so. A `DEBUG` trap could send `C` on bash 3.2, but it runs before every
simple command and clashes with the user's own trap. None of the reference tools ships one
(`landscape.md`: herdr ships no shell integration, Rex reads only `A`, Paseo uses OSC 633), so
Flightdeck does not either.

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
  time per connection. Field set 2 of `Command` adds the optional `text_truncated` and
  `dropped` (events only).
- **Times** are RFC 3339 with nanoseconds, in UTC, from gff-hab's clock when it read the mark as
  live output.
- **Events.** `command.updated` carries one `Command`; its id is the pane id, so a scope that
  holds the pane gets it. The link notes which commands changed; `gff-hab` reads the changes and
  sends them at most every 200 ms, at most 32 per pane per flush (the newest). The last event of
  a command is its final state; a client that missed some reads `command.list`. When a flush
  leaves a pane's older changes out, its first event for the pane carries `dropped`, the count
  left out, so the client knows to read `command.list`.
- **Saved.** The checkpoint header gains an optional `commands` field: the list and the
  tracker's state (`phase`, the pending `prompt` and `input`). A checkpoint without it (an older
  build) starts an empty list at its offset. A checkpoint is written only where the mark scanner
  holds no partial mark. With the 1 KiB text cap the largest header is under 7 MiB (1000 commands
  of text that JSON escapes up to 6 bytes a byte); gff-hab reads at most 8 MiB of header and
  refuses a longer one, and allocates the snapshot only when the file is exactly as long as the
  header says.
- **Checkpoint times.** `GFF_HAB_CHECKPOINT_IDLE` (default 1s) and `GFF_HAB_CHECKPOINT_EVERY`
  (default 10s), Go durations from 100ms to 24h, set when a pane's checkpoint is written. The
  end-to-end tests use them to SIGKILL gff-hab with output past the last checkpoint.
- **The ring.** The link drops a command once its end is at or before the pane's offset minus
  the holder's ring size (the welcome's ring size), and, after restoring a checkpoint, once its
  end is at or before the ring's oldest byte.
- **A range the ring lost completely.** The holder answers such a replay at its oldest byte,
  which can be past the command's end. `command.output` then returns the empty run at the
  command's end (`start` = `end` = the end), with `gap`, and `more` only while the command runs.
  It does not answer `not_found`: the command is still listed, and `gap` already means bytes
  before `start` are lost.
- **A read the ring overtakes.** When the holder skips bytes in the middle of a bounded replay
  (the ring moved on while it sent), the bytes cannot be placed, so gff-hab asks again, up to
  three times; the next replay starts after the lost bytes and says gap.
- **Test snippet.** `cmd/gff-hab/testdata/marks` holds a bash rcfile and a zsh `.zshrc` that emit
  the marks; they are test fixtures, not the console's snippet. zsh prints its partial-line mark
  (`PROMPT_SP`, `PROMPT_CR`) before `precmd`, so that mark would land inside a command's output
  range before `D`; the fixture turns both off. The shipped snippet has to deal with it. The
  bash tests pick their bash in a fixed order and log its path and version: `$GFF_TEST_BASH`
  (failing when older than 4.4), else the first bash 4.4 or newer of bash on PATH,
  `/opt/homebrew/bin/bash`, `/usr/local/bin/bash` and `/bin/bash`. With none, they skip.
- **Not built here:** text from bash (bash sends no command line in its marks without more
  setup), and a nonce against fake marks (see the hard gate above). zsh's `PROMPT_SP` mark
  landing inside an output range before `D` is fixed later in the console snippet (for example,
  send `D` first thing in `precmd`), not in the tracker.

#### Built choices: the working directory (task-3x22hva9)

- **OSC 7.** The scanner also keeps `ESC ] 7 ; <url>` reports, under the same payload cap and
  rules as a mark. The tracker keeps the last good report's path; a command takes it at its `C`
  mark, as the optional `cwd` (field set 3 of `Command`). A report counts when it is
  `file://<host>/<path>`. The path is not parsed as a URL, so `?` and `#` belong to it: each
  `%XX` with two hex digits is decoded and any other `%` is kept, since shells encode little.
  A cut report, one with no path, or a path that is not UTF-8, is over 1 KiB or decodes to a
  control byte leaves the last path in place. The host part is not checked: the field shows where the shell said it was,
  as a terminal's title does. The path is saved with the tracker's state in the checkpoint.
  Display only, under the nonce gate above.
- The test snippets send OSC 7 after `D` in their prompt hook.

---

## 3. API rules

### 3.1 Protocol

- **JSON-RPC 2.0.** Requests, responses and notifications as the spec defines them.
- **Plain mode by default:** one JSON message per line, UTF-8, no embedded newlines. A socket tool
  can drive it.
- Every result is a JSON object, never a bare value, so fields can be added later.
- Requests on one connection may run side by side. A client must match responses by id.
- Method names are grouped by prefix: `hab.*`, `structure.*`, `pane.*`, `session.*`, `project.*`,
  `data.*`, `events.*`, `devices.*`, `peers.*`, `approval.*`, `chat.*`, `attachment.*`, `push.*`, `usage.*`,
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
| `peer` | Optional; set by a hab that dials another hab (`client.kind` `hab`): its device id and host id (`mesh.md` 3.2) |

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
| `time` | The hab's clock as it answers the hello: RFC 3339 in UTC with milliseconds (`2026-03-01T08:30:00.007Z`). Clients measure reset times and ages against it, and may compute their clock's offset (`app.md` 14.5). Optional in the schema, like `device_id`; this build always sends it |
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

How long the floor stays: 90 days after a revision is replaced, and at least two revisions (Q4).

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
- Changed before any release, so with no new revision: `hab.stats` `cpu_pct` became optional
  (2026-11-01), so a host that cannot measure CPU leaves it out. No revision had been released, and
  no client read the field.

### 3.4 Capabilities

Capability strings name optional features: `data-streams`, `snapshot-vt`, `snapshot-ghostty`,
`agent-state`, `transcripts`, `devices`, `push`, `host-stats`, `mesh`, `idempotency-keys`, `usage`, `catalog`, `channels`,
`speech`, `capcom`. `capcom` means `capcom.open`, `capcom.attach`, `capcom.detach`, `capcom.pending`, `capcom.assign` and
`session.resume` of a Capcom session work, and the catalog carries the `capcom` section
(`capcom.md` 7.6, 8.4). `speech` means the hab's speech engine is installed and turned on, so `speech.prepare`
and `speech.transcribe` work, though the model may still need its download (`voice.md`).
`idempotency-keys` means the hab honours `idempotency_key` on `session.create` and
`session.send` (3.9). `confirm` means `confirm.request`, `confirm.answer`, `confirm.list`,
`confirm.get`, the `confirm.requested` and `confirm.closed` events and the open confirmations in
a `host` snapshot work, and that `session.create` takes `model` and `seed` (`capcom.md` 6.4). `mesh` means the hab keeps
a peer list and serves peer links, `peers.list`, `peers.accept` and `peer.updated` (`mesh.md` 4).
`catalog` means `catalog.get`, `catalog.put`, `catalog.list` and `catalog.updated` work (`mesh.md` 5).
`channels` means `channel.send`, `channel.status`, `channel.wait`, `mail.push`, `mail.pull` and
`channel.message` work, and that the hab sends `mesh.changed` to a peer link when mail waits for
its dialer (`mesh.md` 6.5). `mesh.changed` is the one event a connection gets without
`events.subscribe`: it goes only to a peer link, from the hab that link dialed.
`devices` means the embedded SSH
server is on and the `devices.*` methods work. `push` means the hab has working push config;
`host-stats` means `hab.stats` works (3.9). `settings` means the hab-side settings work:
`settings.get`, `settings.set`, `settings.updated`, and `settings` in a `host` snapshot
(`app.md` 20.5). `read-tools` means `transcript.list`, `transcript.search`, `transcript.read`,
`notice.get` and `file.read` work (`capcom.md` 4, 5.5); their `host` is a peer label or device
id, this hab's host id or device id, or `self`. `notice.get` reads the last 500 notices of the
last 7 days, saved across a restart (`capcom.md` 8.5). `logbook` means the hab has a Logbook
replica and the `logbook.*` methods and the `logbook.updated` and `logbook.conflict` events work
(`logbook.md` 10.3, 10.5); without it every `logbook.*` method answers `unavailable`. `rounds` means the `round.*` methods, the `round.updated` and
`round.run_updated` events, the rounds in a `host` snapshot and `Session.round` work (`rounds.md` 13).
A client checks for a capability before it uses the feature. Capabilities are
for features that a host may lack. They are not a second versioning scheme.

Built: with the holder link on, gff-hab sends `agent-state`, `data-streams`, `snapshot-vt`,
`transcripts`, `detect-explain` (`detect.explain` works) and `capcom`. It always sends `mesh`, `idempotency-keys`,
`catalog`, `channels` and `usage` (`usage.list` and `usage.updated` work; readings arrive only through panes with the
holder link), `settings`, `read-tools` and `confirm`. With the holder link on it also sends
`rounds`. It sends `logbook` when its Logbook file opens (`logbook.md` 10.5). It sends `push` only when its push config loads and validates
(3.9, Push), and `speech` only when the speech helper is found and the host setting leaves the
engine on (`voice.md`, Built choices).

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
  do the activity, chat, turn and usage events of 3.9. Ephemeral means never in the snapshot,
  with one exception: the open confirmations are in a `host` snapshot, and `confirm.requested`
  and `confirm.closed` carry no revision; the hab sends them in order (`capcom.md` 6.4).
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

Decision: proposed. A reconnect within the same epoch does not replay missed events; the client
reads state again (Q5).

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

With the `settings` capability: kind `locked`, a setting set in the hab's config. `data.id`
names the first refused row on every `settings.set` error the settings service raises; a
`settings.set` refused before any row is read (params that do not decode, no `settings`
capability) carries none. Kinds are a server-sent word set, so adding one needs no revision.

With the `read-tools` capability: kind `denied`, a read the host's read scope refused
(`capcom.md` 5.2). The hint names the host, and is the same for every reason: it never names the pattern or the list.

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
| `session.rename` | Give a session a new name; a taken name is `conflict`, one outside the name charset `invalid` | Yes (task-98qp7he5) |
| `session.close` | Remove a session record; a live pane keeps running, unfiled (2.2) | Yes (task-98qp7he5) |
| `session.resume` | Start an exited session's harness again in a new pane, with its resume id | Capcom sessions (task-aqw4iowl, `capcom.md` 7.6); every other session (task-4i9uv0ai, `gff-client.md` 6.2) |
| `session.interrupt` | Stop the current turn through the adapter (the chat view's Stop) | Part 4 stage 4 |
| `session.send` | Send a message through the adapter, under the input rules | Yes, Claude Code (Part 4 stage 4) |
| `approval.answer` | Answer a pending request by id; a closed or expired id is refused with `conflict` | Yes, Claude Code permissions (Part 4 stage 4) |
| `trust.answer` | Answer a harness's folder trust prompt, `trust` or `exit`, from the dialog on screen (capability `trust`; `app.md` 7.10) | Yes, Claude Code (task-8svbwckn) |
| `chat.page` | Read a page of chat messages from the transcript reader (capability `transcripts`) | Yes, Claude Code (Part 4 stage 5) |
| `attachment.put` | Start an upload of a file for a session (capability `attachments`; `app.md` 4.4) | Yes (task-mthck7r4) |
| `attachment.write` | Send the next run of an upload's bytes; the last run checks the hash (capability `attachments`; API addition) | Yes (task-mthck7r4) |
| `attachment.get` | Fetch a run of an attachment's bytes, or its thumbnail (capability `attachments`) | Yes (task-mthck7r4) |
| `content.show` | Store a reference to a file on the hab's host for a session to show (capability `content`; `app.md` 4.7) | Yes (task-btkd1ekc) |
| `content.list` | A session's shown files, images from tool calls and changed files (capability `content`) | Yes (task-btkd1ekc) |
| `content.read` | A run of one listed file's bytes, in bounded chunks (capability `content`) | Yes (task-btkd1ekc) |
| `content.diff` | A listed file's diff in unified format (capability `content`) | Yes (task-btkd1ekc) |
| `session.focus` | Report focus: marks `done` seen, and holds pushes while watched | Yes (task-oe7ioczy) |
| `push.register` | Store an APNs device token with the kinds the device wants (capability `push`) | Yes (task-oe7ioczy) |
| `push.register_activity` | Store a Live Activity token for the tracked session (capability `push`) | Yes (task-oe7ioczy) |
| `push.unregister` | Drop a token (capability `push`) | Yes (task-oe7ioczy) |
| `notice.raise` | Raise a Flightdeck notification from a hab event or a local script: a `notice.raised` event, and a push where push is set up | Yes (task-oe7ioczy) |
| `usage.list` | The latest usage reading of each harness account (capability `usage`) | Yes (task-6189lsjb) |
| `hab.stats` | CPU, memory, disk, battery and uptime of the host (capability `host-stats`) | Part 5 stage 5 |
| `command.list` | A page of a pane's shell commands, found by OSC 133 marks (capability `commands`, 2.5) | Yes (task-m845kpi9) |
| `command.output` | A run of one command's output, from a bounded replay of the holder's ring (capability `commands`, 2.5) | Yes (task-m845kpi9) |
| `detect.explain` | Why a pane shows its agent state: the source that won, the process match, and each screen rule's region and result (capability `detect-explain`; `harness-adapters.md`, Detection) | Yes (task-bgjbr5rn) |
| `worktree.list`, `worktree.create`, `worktree.setup`, `worktree.remove` | A project's git worktrees: list, make one and run its setup script, run a pending setup, remove one (3.10) | Yes (task-wvbuz085) |
| `speech.prepare` | Load the speech engine and start the model download if needed; returns the engine state at once (capability `speech`; `voice.md`, API addition) | Yes (task-h011ghy9) |
| `speech.transcribe` | Turn one recorded clip into text, one decode at a time in arrival order (capability `speech`; `voice.md`, API addition) | Yes (task-h011ghy9) |
| `settings.get`, `settings.set` | Read hab-side settings, and write them all or nothing with a revision check; event `settings.updated` (capability `settings`; `app.md` 20.5) | Yes (task-yfmrj6yc) |
| `capcom.open`, `capcom.assign` | Return or start the standing Capcom session; make a host the Capcom home host (capability `capcom`; `capcom.md` 7, 8.3) | Yes (task-aqw4iowl) |
| `capcom.attach`, `capcom.detach`, `capcom.pending`, `capcom.open`'s `context` | Attach a notification, session, file, console command or Logbook record to a Capcom session's next message, remove one, or read what waits (read-only); the next `session.send` delivers it as blocks before the user's text, or alone when the send has no text (capability `capcom`; `capcom.md` 8.1, 8.4) | Yes (task-ilse43i7) |

| Event | Purpose | Id | Sent from |
|---|---|---|---|
| `pane.bell` | A pane rang the bell; at most once per second per pane, with the count | Pane | Yes (task-98qp7he5) |
| `pane.output` | A pane printed; at most once per second per pane, with its offset | Pane | Yes (task-98qp7he5) |
| `chat.message` | A chat message was appended or changed in place | Message | Part 4 stage 5 (built for Claude Code) |
| `session.turn_end` | The agent's turn ended; names the final message | Session | Part 4 stage 4 |
| `usage.updated` | An account's usage reading changed | Account key | Yes (task-6189lsjb) |
| `command.updated` | A pane's command started or ended (2.5) | Pane | Yes (task-m845kpi9) |
| `notice.raised` | A notice was raised (`notice.raise`) | Notice | Yes (task-oe7ioczy) |
| `content.updated` | A session showed a file (`content.show`) | Content item | Yes (task-btkd1ekc) |

All eight events are ephemeral (3.5): they carry no revision, and a client applies them in seq
order. A chat message lives in the transcript, not in gff-hab, so it has no stored revision
either; an update repeats the message id and replaces the message. A scoped subscription gets a
`chat.message` when the message's `session` is in its scope.

Built: gff-hab sends no `revision` key on these events.

New capabilities: `push` (the hab has working push config, `app.md` 8.4), `host-stats`,
`commands` (the `command.*` methods and the `command.updated` event work; a hab with the holder
link sends it), and `content` (the `content.*` methods and the `content.updated` event work; a
hab with the transcript reader sends it).

Shapes the specs left open, and the choice taken (the smallest that serves both clients):

- **`session.create`** takes `name`, `project` or `project_dir`, `harness`, `argv`, `cwd`, `env`,
  `cols` and `rows`, all optional, and returns the `session` and `pane` ids. The objects arrive as
  events. Defaults: the hab's `$SHELL` (else `/bin/sh`); `cwd` is `project_dir`, else, with
  `project`, the project's most recently used folder that still exists (task-3x22hva9: a console
  beside a project's session starts there), else the hab's home; 80x24; the name is the
  program's base name, then `-2`, `-3` and so on. A name is 1 to 64 of `A-Z a-z 0-9 . _ -`
  (`mesh.md` 6.5): another name is `invalid` at create and rename, and a picked name has every
  other character made `-`. `env` entries are
  added to the hab's own environment and replace the hab's value for the same key; `GFF_HAB` is
  always the pane's own. `project_dir` finds the project by the repository's remote (`origin`,
  else the first), else its root commit, and creates the project, named after the repository
  folder, when it is new. `project` and `project_dir` together are `invalid`. `harness` needs
  the adapter: `claude`, `codex` and `opencode` are built (`harness-adapters.md`, Codex, built
  choices; `claude` starts `claude`, task-ph00sdci); any other value
  answers `unavailable`. Every input is checked first, the name
  rules and clashes (local and peer) among them. Then the hab files the session, and only then
  spawns, so the pane's `GFF_HAB` token names the session from the program's first byte: a
  program that runs `gff-hab show`, `send` or `wait` at once finds its session. The token
  counts as live while the spawn is in flight, before the holder link follows the pane
  (`harness-adapters.md`, Hook endpoint, Checks). Events: `session.updated` (and the project's
  `project.updated`) come first, naming a pane that is not in the structure yet; the pane's
  first `pane.updated` follows, with `session` already set, so a new session's pane is never
  seen unfiled. If the spawn fails, the hab closes the session again: `session.deleted` (and
  `project.updated`) follow its `session.updated`, the call answers the error, and the name is
  free. Only a definite refusal counts as a failed spawn: the holder's spawn-failed, no holder,
  or a uuid in use. When the answer is lost instead (a timeout, a closed link), the hab looks for
  the pane in the link and on the holders: if it exists, the program runs, so the session stays
  and the create succeeds; if not, the session closes as above. When the current holder lists
  the pane, the hab sends the same spawn again with a deadline of its own: the holder starts
  nothing and answers the existing pane, and the link follows it (its output, exit and token). A
  pane on another holder is not spawned again, which would start a second program; the link
  follows it after the next gff-hab start. The retry names the holder it checked and is refused
  when another holder became current meanwhile; a pane left unfollowed is logged once. Output
  the program wrote while its answer was lost reaches the link only through the frames it holds
  for a pane it does not follow yet, which are bounded: bytes past that bound are lost, and the
  screen marks the gap. A picked name (no `name`) that another create filed
  first is picked again, at most twice, before the call answers `conflict`. A `session.close`
  that lands during the spawn closes the record as usual: the create still answers success with
  the closed session's id, and the new pane is unfiled. The record carries a spawning mark,
  saved in the state file and not in the API, which the pane's first sight clears. A gff-hab
  that stops between the record and the spawn leaves a session still marked. The next start
  drops it (`session.deleted`, and the project's `project.updated`, so its name is free) only
  when the link has attached every registered holder and the pane is on none of them. A
  holder counts once the link has attached its panes, not when it connects. With a holder not
  reached, the session stays, exited with no status, and keeps its mark: its pane showing up
  makes it run again, and a later start with every holder reached drops it. A session without
  the mark is never dropped. `gff-client.md`
  proposed `command`; `argv` is that command, unparsed. The optional `idempotency_key` makes a
  retried create start one session (Idempotency keys, below). With `harness`, the optional
  `model` goes after the program's arguments as `--model <model>`, and the optional `seed` (at
  most 8000 characters) is queued in the hab's channels inbox once the session is filed: it
  reaches the agent as its first message when the agent can take one, in a start envelope that
  names `seed_from` as the writer, on the owner's behalf (`capcom.md` 6.4). A seed that cannot
  be queued after the spawn is logged, not answered, so a retry never starts a second session.
- **`cwd_of`** on `session.create` names a pane: with no `cwd` and no `project_dir`, the new
  pane starts in that pane's working folder (its program's folder, else its shell's last OSC 7
  report, else its session's start folder, else the default) and is filed in that pane's
  project unless `project` is given (`gff-client.md` 5.4).
- **`session.resume`** takes `{session}` and returns the same ids as `session.create`. The
  session keeps its id and gets a new pane; it must be exited (else `conflict`). A Capcom
  session resumes through Capcom (task-aqw4iowl, `capcom.md` 7.6). Any other session resumes
  from gff-hab's resume book (task-4i9uv0ai, `gff-client.md` 6.2): the harness last seen in it
  with its native session id (`claude --resume <id>`, or `--session-id <id>` while Claude Code
  has no transcript; `codex resume <id>`; `opencode --session <id>`), else the start
  `session.create` was asked for, in the session's folder. A session the book knows nothing of,
  or whose folder is not known, answers `unavailable`; so does one exited with no status while
  a holder is not reached yet, since its program may still run there.
  A session record carries the optional `kind` (`capcom`, an open word set) and `dir` (a Capcom
  session's folder).
- **`pane.close`** takes `{pane}` (the `PaneRef` of the size methods) and returns the seq only.
- **`session.send`** returns the seq only, once the writes reached the program. Whether the
  message may go follows from the pane's agent state, which the client already holds: a busy
  agent, an open permission request, or another message still starting its turn refuses it
  with `conflict`, a pane with no agent with `unavailable` (`harness-adapters.md`, Input
  delivery). A "not delivered" report is added as an event later. The optional
  `idempotency_key` makes a retried send deliver the message once (Idempotency keys, below).
- **`approval.answer`** `choice` is a closed set: `allow`, `deny`, or `answer` with the option or
  free text in `text`. `via` is `app`, `banner`, `watch`, `voice` or `terminal`. A request that
  is not the pane's open one, or a second answer to one request, is refused with `conflict`.
  The call returns once the request is resolved; an answer the hook could not take, an exit,
  or a request still open past the read-back answers `unavailable` (`harness-adapters.md`,
  Permission answers).
- **`trust.answer`** `{pane, choice}`: `choice` is `trust` or `exit`. The pane must show
  `blocked:trust` (`agent.state` field `trust: {folder}`), and the screen must still show the
  dialog with one cursor row and one row with the chosen label; otherwise `conflict`, and nothing
  is sent. Client input to the pane in the last second is `conflict` too. `exit` sends Esc;
  `trust` moves the cursor one key per write, reads the screen again, and sends Enter only with
  the cursor on "Yes, I trust this folder". The call returns once the outcome shows (5 s, else
  `unavailable`): the dialog gone with the agent running, or the session ended. The result is
  `{dropped}`: with `exit`, the channel messages that waited for the session (its start message
  among them), now refused with the reason "the owner did not trust the folder". A second call
  while one runs is `conflict`; a peer link's call is `invalid`. The keys are the hab's, so they
  are not a round's take over (`app.md` 7.10).
- All three run off the connection's read loop, so a slow pane holds up no other call.
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
- **`attachment.*`** (`app.md` 4.4, capability `attachments`). The bytes take the control
  connection in runs, as `content.read` serves them: one message is at most 16 MiB and the
  file limit is 25 MB, so one message cannot carry a file. `attachment.put {session, name, type,
  size, sha256}` checks the session, the size (at most 25 MB, else `invalid` naming the limit)
  and the hash's form, then returns `id`, the final `path`, `chunk` (1 MiB, the most one write
  takes) and `done` (true only for an empty file). `attachment.write {id, offset, data}` takes
  the runs in order: `offset` must equal the bytes received (else `conflict`, with the count in
  the message), and data more than `chunk` or past `size` is `invalid`. It returns `received`
  and `done`. The run that reaches `size` checks the SHA-256: a mismatch drops the upload
  (`invalid`); a match moves the file to `path`. A write for an id that is already done answers
  `done` again, so a retried last run is harmless. An upload with no write for 2 minutes is
  dropped with its partial file, as is every partial file when gff-hab starts; at most 8
  uploads are open at once. A put of the same file (same hash) for the same session drops that
  session's earlier open upload of it, so a retry never fills the limit. `attachment.get {id, thumbnail?, offset?, limit?}` returns `name`,
  `type`, `size`, `offset`, `data` and `eof`: a run of at most 1 MiB (default 256 KiB) of the
  file, or with `thumbnail` a JPEG of at most 320 px on its longest side (PNG, JPEG and GIF
  sources; any other type is `invalid`). `session.send` takes the ids of done uploads for that
  session (else `not_found`; an open upload is `conflict`) and the adapter delivers each path
  in the message text (`harness-adapters.md`, Input delivery). Where the files live, and the
  ids in `chat.page`, are in `app.md` 4.4. Golden files: `result.attachment.put@2.json`,
  `result.attachment.write.json`, `result.attachment.get@2.json`.
- **`content.*`** (`app.md` 4.7). `content.show` takes `path` (absolute, a regular file) and
  `session` or `pane_token` (the token in the calling pane's `GFF_HAB`), with the optional `kind`
  (`image`, `markdown`, `pdf`, `html`, `text`, `diff`, `file`; else from the file name) and
  `title` (at most 200 characters); it returns the item `id` and the `session`. The hab stores
  the path, never a copy, in `content.json` in its state directory, at most 200 per session.
  `content.list {session}` returns `items` oldest first (shown files; images a tool call read,
  wrote or viewed; files the session wrote or edited; git status of the session's folder),
  `folder` and `more` (at most 500). An item has `id` (a hash of session and path), `session`,
  `kind`, `path`, `name`, `type`, `at`, and the optional `title`, `size` (absent when the file
  is gone), `modified` (the file's modification time, as `content.read` reports it), `shown`, `change` (`added`, `modified`, `deleted`, `renamed`, `untracked`; open) and
  `message` (the tool call). `content.read {session, id, offset?, limit?}` serves only ids the
  list holds for that session (else `not_found`), in runs of at most 1 MiB (default 256 KiB),
  of regular files up to 32 MiB; each run carries `size` and `modified`. `content.diff
  {session, id}` returns `diff` with `source` `git` (against HEAD), `untracked`, `transcript`
  (the session's edit calls) or `none`, cut at 512 KiB with `truncated`. A git status path that
  leads out of the repository through a symlink is left out. The calls run off the read loop,
  at most 4 per connection. `content.updated` carries one item; a scoped subscription gets it
  when its session is in scope. `gff-hab show` is the command line of `content.show`.
- **`push.register`** `environment` is `development` or `production`; `kinds` are `permission`,
  `question`, `auth`, `error`, `done` and `usage`. Added by the push stage, optional:
  `hide_commands`, the app's "show details in notifications" turned off (`app.md` 8.3). Added
  by "Ask an agent", optional: `features`, words for what the app's categories can do; `ask`
  means it registered the Ask categories (`capcom.md` 8.5). Unknown words are kept and
  ignored.
- **`notice.raise`** takes `title`, and the optional `body`, `level` (`passive`, `active`, the
  default, or `time-sensitive`) and `session`. It returns the notice `id` and `pushed`, the
  number of devices the push goes to (0 without push). A `script` or `cli` client may call it;
  `gff-hab notify` is its command line. `notice.raised` carries `id`, `title`, `body`, `level`,
  `session` and `at`; a scoped subscription gets it when the notice names no session or a
  session in its scope.
- **`hab.stats`** is a method, not an event: a client asks while it shows the numbers. Sizes are
  bytes; `cpu_pct` is over all cores and absent where the host cannot measure it; `battery_pct`
  is absent with no battery.
- **`usage.list`** returns `accounts`, each with `key`, `label`, `harness`, `windows` (`name`,
  `used_pct` in whole percent, `resets_at`, `read_at`, `length_s`, `starts_at`) and `degraded`.
  `length_s` is the window's length in seconds and `starts_at` is `resets_at` less that length;
  each is absent when the harness's data does not define it, so a client never takes a length
  from a name it does not know. `degraded` is one line naming what the account's newest report
  could not read so that no window read, or a known window failed (`harness-adapters.md`, Usage);
  absent once a newer report's windows read. The three fields came after the first `usage`
  field set, so they are optional (`<message>@2.json`). `usage.updated` carries one account. Every
  subscription gets `usage.updated`, scoped or not: an account belongs to no session. Sources, key,
  names and the send rate are in `harness-adapters.md`, Usage.
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

**Idempotency keys** (task-uu40arhs; `internal/control/idem.go`; `landscape.md` idea 4.2,
after Paseo). A phone that loses its connection mid-call cannot tell whether the call ran. With
a key it sends the same call again, and the hab runs it at most once. Built choices:

- **Field.** `session.create` and `session.send` take an optional `idempotency_key`, a string
  of at most 128 bytes. A key must be unique on the hab, such as a random UUID made per
  request: two clients with counter-style keys would collide. A longer key answers `invalid`.
  No key: the call runs every time, as before. The field is additive (3.3); the results are
  unchanged, so no golden file changes.
- **Capability.** Every welcome lists `idempotency-keys` (3.4). A hab built before keys ignores
  the field as an unknown optional one (3.3), so a client sends keys, and retries on the
  strength of them, only to a hab that lists the capability.
- **Scope.** One table per hab, shared by every connection, because a client that reconnects
  after the grace period gets a new `client_id` (3.7). Keys are per method: the same string on
  `session.create` and `session.send` is two keys.
- **A repeat after success** runs nothing and answers the first call's result: the same
  `session` and `pane` ids for a create, success for a send. Its `seq` is the repeat's own
  (3.5). A create's size ownership (6.5) stays with the first caller.
- **A repeat while the first call runs** waits for it and gets its answer, result or error.
  Each caller gets its own copy of an error, since each reply carries its own `seq`. It waits
  at most the action timeout (30 s), then answers `unavailable`; the first call goes on.
- **A failed call is forgotten.** An error answer stores nothing, so a retry with the same
  key runs again. A send refused because the agent is busy (`conflict`) is the common case:
  the app sends the message again at the next `idle` (`app.md` 4.2).
- **Delivered in part.** A send whose paste was taken but whose Enter failed answers
  `unavailable` with a hint: the text is in the prompt, press Enter in the terminal or clear
  the prompt. The hab keeps this answer like a success, so a retry with the same key gets the
  same error and pastes nothing. To send the text anyway the client makes a new key. A paste
  write that fails after it reached the program is not told apart from one that never did;
  its error is forgotten like any other.
- **Same key, other request.** The hab keeps a SHA-256 hash of the params without the key and
  without the fields a retry may change: `cols` and `rows` on a create (the phone rotated)
  and `origin` on a send (typed after it was dictated). A repeat whose other params differ
  answers `conflict` and runs nothing.
- **How long.** A key is kept 24 hours after its call succeeded, at most 4096 keys; past that
  the oldest goes first. The app's queued "sends after this turn" go out within that time.
- **In memory only.** A gff-hab restart forgets every key. A restart lands between a call and
  its answer only rarely, and the client sees a new epoch and takes a fresh snapshot (3.5), so
  it can see a session its create made. A repeated send after a restart is refused with
  `conflict` while the agent still works on the first; once that turn has ended it is
  delivered again. Paseo's creation journal is the bar; persisting the table costs a write per
  send, and the window is the few seconds of a restart.
- **Not built here.** The app and `gff` do not send keys yet; each client adds them with its
  retry path.

**Push** (task-oe7ioczy; `internal/push`, `cmd/gff-hab/push.go`). Built choices:

- **Config.** `-push-config`, else `<state>/push.json`, with `GFF_HAB_PUSH_KEY_COMMAND` (a
  JSON array), `GFF_HAB_PUSH_KEY_ID`, `GFF_HAB_PUSH_TEAM_ID`, `GFF_HAB_PUSH_TOPIC` and
  `GFF_HAB_PUSH_ENVIRONMENT` over it, field by field. The file names a command that prints the
  key (`key_command`, an argv run with no shell), never the key. `key_file` and
  `GFF_HAB_PUSH_KEY_FILE` are refused with a message that names `key_command`. Key id and team
  id are 10 capital letters and digits; the topic is the app's bundle id. Nothing set: push is
  not set up. Set but wrong: gff-hab logs why and runs without push. Either way it leaves
  `push` out of the capabilities, and the `push.*` methods answer `unavailable` with a hint
  that names `docs/push.md`. `session.focus` and `notice.raise` work without push.
- **Key command** (task-sle59fdq). gff-hab runs it in the background after start, never
  waiting for it: gff-hab's environment, stdin from `/dev/null`, its own process group, 30 s
  timeout (the group is killed), 16 KiB stdout cap. Stdout must parse as a PKCS #8 P-256 PEM
  key. The key stays in memory, is never logged and never written to disk; the stdout buffer is
  overwritten after parsing. An error names the program and exit status, and at most the first
  stderr line cut to 120 bytes, dropped when it holds a PEM marker or a 32-character base64 run.
  Push is on, and the welcome lists `push` (checked at each hello), only once a key parsed. A
  failure logs one line per failure streak and retries after 30 s, doubling to 30 minutes, so a
  password manager locked at boot delays push only. APNs answering `InvalidProviderToken`, or a
  token that cannot be signed, starts the command again, at most once per 5 minutes, in the
  keyring's own context with the full 30 s (never the send's 15 s); a new key that arrives while
  the send still has time sends the message once more, a failure keeps the old key. The signer
  reads the keyring's key at each token, so racing sends never restore an older key. A program
  that exits 0 while a child it started holds stdout keeps its output. Closing gff-hab ends a
  run without a log line.
- **APNs.** HTTP/2 to `api.push.apple.com` or `api.sandbox.push.apple.com`, chosen by each
  token's environment. Token auth: an ES256 JWT with the key id and team id, reused for 40
  minutes (APNs refuses one older than an hour or refreshed within 20 minutes), signed again
  at once when APNs calls it expired. `apns-topic` is the bundle id, plus
  `.push-type.liveactivity` for a Live Activity. `apns-expiration` keeps an alert for a device
  that is offline: a permission banner until its request's `expires_at`, other banners and
  notices for an hour, and a withdrawal as long as the banner it replaces. Live Activity updates
  go with 0: APNs tries once and stores nothing. Alerts go at priority 10, passive ones and Live
  Activity progress at 5.
- **Tokens.** `<state>/push/tokens.json`, mode 0600 in a 0700 directory, written whole and
  renamed into place. One device token and one Live Activity token per device id: a second
  registration replaces the first. At most 64 devices; the oldest registration goes first.
  A token must be hex. `push.register_activity` needs a known session. `push.unregister` of a
  token the hab does not hold succeeds. APNs answering `BadDeviceToken`, `Unregistered`,
  `DeviceTokenNotForTopic`, `ExpiredToken` or 410 drops the token and logs once. A refused key
  or topic (`InvalidProviderToken`, `TopicDisallowed` and the like) is logged once as "push
  broken" until a push succeeds. A 429 backs off from 1 s, doubling to a minute: an urgent push
  waits once and retries, and every progress update is dropped until the back-off ends.
- **Shutdown.** Closing the notifier ends a push in flight and a back-off wait at once, and
  drops queued pushes that have not gone out, so a stop never waits on an unreachable APNs.
- **What pushes.** The notifier reads every `agent.state` change, keeping only the latest
  unsent state per pane. A new `blocked:permission`, `blocked:question`, `blocked:auth`,
  `error` or `done` (by state and request id) raises a banner to each device whose `kinds`
  hold it; `done` is opt-in by its kind. A repeat of the same state raises nothing. A pane
  first seen through a replayed OSC 7501 report (source `screen`, which the link sends for each
  pane after a gff-hab restart) raises no banner: its state is recorded and its Live Activity
  updated. Collapse
  ids: `r-<request id>` (a hash past 64 bytes), else `q-`, `a-`, `e-`, `d-` plus the pane id,
  and `n-<notice id>`.
- **Presence tiers** (`app.md` 8.3, `landscape.md` 4.1). A device hears of an event by push
  unless (1) some connected client reported the session in focus (`session.focus`, `active`
  true) within the last 60 s: then no device gets a banner; or (2) a connected client of that
  device sent `session.focus` with `active` true, for any session, within the last 180 s: the
  app is in use and shows its own notice from the live event. `active` false ends the focus on
  that session; it ends the foreground mark only when the client has no other session in focus
  within 60 s, so moving from one session to another keeps the app in the foreground.
  `active` false for a session that no longer exists succeeds.
  A client speaks for a device once it called `push.register` or `push.register_activity`
  with that device id. The app re-sends `active` true at least every 30 s while a session is
  on screen in an active window, and sends `active` false when it leaves or goes to the
  background. Live Activity updates ignore presence.
- **Answered elsewhere** (`app.md` 7.5). When an `approval.answer` reaches the harness, the
  notifier replaces the banner on every device that got it, same collapse id, passive, with
  "Answered on <label>" (the answering client's hello label, else its name) and "Allowed:
  <program>" (the harness's command summary when it sent one, `app.md` 16.4) or "Denied: …". It does this at once: Claude Code keeps the request open until
  the tool has run. The same blocked state arriving afterwards raises nothing. A request that
  closes with no answer through the hab (the TUI) is replaced with "Answered in the terminal".
  A request whose pane's program exits is replaced with "Session ended". A pane that closes with a banner open has it replaced with "Pane closed", and the notifier
  forgets the pane: it no longer counts in `others`.
  A second answer to one request stays refused with `conflict` (harness-adapters.md,
  Permission answers).
- **Live Activity** (`app.md` 9). Each device's activity token tracks one session. Every change
  of state goes out at once; repeated `working` at most every 30 s; other repeats not at all.
  An answer drops the buttons at once (state `working` after allow, `idle` after deny).
  `done`, `error` and `exited` end the activity with a dismissal ten minutes later, and the
  token is dropped. The content state is `state`, `kind`, `title`, `request`, `pane`,
  `buttons`, `since`, `others` (other blocked panes on this hab), `hab` and `harness`.
- **Payload.** `aps` holds `alert` (`title`, `subtitle`, `body`), `interruption-level`,
  `category` (`approval`: Approve and Deny; `approval-deny`: Deny and Open; `open`; while some
  host runs Capcom and the device registered the `ask` feature, `approval-ask`,
  `approval-deny-ask`, `open-ask` and `ask`, each with Ask last, `capcom.md` 8.5),
  `thread-id` (the session), `sound` except when passive, and `mutable-content` 1, so the
  notification service extension can add the expiry as a local clock time. A `flightdeck`
  object carries `v` (1), `kind`, `hab`, `session`, `pane`, `state`, `request`, `expires_at`
  and `notice`, and, while some host runs Capcom, `capcom_context` (`{kind: "notification",
  ref, host?}`: a notice id, and this hab's device id when Capcom runs elsewhere) and
  `capcom_home` (that host's device id). An agent-state banner's ref is a notice the hab keeps
  the banner as. Payloads stay under 4 KB.
- **Words** (`app.md` 16). Each field is fitted to its budget on word boundaries, with "…".
  The command is shortened (leading `cd`, environment assignments, the working directory part
  of paths, the middle of long quoted strings) and gets 13 words and 84 characters, leaving
  room for the phone's " · Expires 14:05". Approve is offered only when the whole shortened
  command fits. A permission request with no request id cannot be answered through
  `approval.answer`, so its banner uses `open`. The agent state carries no question or error text yet, so those bodies are
  fixed: "Open to answer." and "The agent stopped on an error."
- **Not built here.** Push-to-start of a Live Activity, actions on question banners (answering
  a question through `approval.answer` is not built), "push broken" in a status the app can
  read, and dropping tokens when a device is revoked (the `devices` methods are not built).

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

### 3.10 Worktrees

Parallel agents on one repository each want their own folder and branch. gff-hab makes, lists
and removes git worktrees for a project, and runs a project's setup and teardown scripts
(task-wvbuz085; `landscape.md` 2.13 and idea 4.5). Code: `internal/worktree` (git, files),
`cmd/gff-hab/worktrees.go` (the methods and `gff-hab worktree`).

**What the reference tools do.**

- **herdr.** `herdr worktree create`, `open` and `remove`. New checkouts go to
  `<directory>/<repo>/<branch slug>` (default directory `~/.herdr/worktrees`). With no branch
  it makes up a name. Remove refuses a checkout with modified or untracked files
  (`git status --porcelain --untracked-files=all`) unless forced. A forced remove of a checkout
  whose folder is half gone cleans up what is left. Open makes a workspace for a worktree that
  exists.
- **Paseo** (`public-docs/worktrees/index.md`, `worktreeinclude.md`). Worktrees under
  `~/.paseo/worktrees/<hash>/<slug>`. Modes: branch off a base, check out a branch, check out a
  pull request. `paseo.json` at the repository root names `worktree.setup` and
  `worktree.teardown`: a script or a list of commands, run with the worktree as the working
  folder and `$PASEO_SOURCE_CHECKOUT_PATH` set. It is read from the base's committed version.
  Setup runs once after creation; teardown runs before removal, and a failing teardown does not
  stop the removal. `.worktreeinclude` (gitignore syntax) copies ignored files such as `.env`
  from the source checkout before setup: only files git already ignores, an existing file in
  the worktree is kept, and a copy error removes the new worktree. Setup, terminals and scripts
  in a pull request from a fork wait for the user's approval. Archiving asks first when the
  worktree has uncommitted or unpushed work. Setup output streams into the workspace.
- **Rex.** No worktrees.

**Calls** (all take `project` or `project_dir`, exactly one, and run off the read loop):

| Method | What it does |
|---|---|
| `worktree.list` | The repository's working trees, the main one first: `path`, `branch`, `head`, `main`, `locked`, `prunable`, and `managed` (made by `worktree.create` on this hab) |
| `worktree.create` | Make a worktree, copy the files `.worktreeinclude` names, then start the setup script per `setup` (`auto`, the default; `run`; `skip`). Returns `project`, `worktree`, `copied` and `setup` |
| `worktree.setup` | Start a linked worktree's setup script now: the yes to a pending setup, or a rerun |
| `worktree.remove` | Run the teardown script, then remove a linked worktree. The branch stays. Uncommitted or untracked changes need `force` |

"Open" is `session.create` with the worktree folder as `project_dir`: the worktree has the
project's identity, so the session files into the same project. No method of its own is needed.

**Built choices.**

- **Which folder.** `project_dir` resolves the project as `session.create` does (3.9). With
  `project`, gff-hab uses the project's most recently used folder that still exists; a project
  with none answers `unavailable` and asks for `project_dir`. Listing does not record a folder.
- **Where new worktrees go.** `<root>/<main working tree folder>/<branch slug>`. The root is
  `serve -worktrees`, else `$GFF_HAB_WORKTREES`, else `~/.gff/worktrees` (`~/` expands). The
  slug is the branch in lower case, with one dash per run of other characters. `path` puts the
  worktree anywhere. A folder that exists is a `conflict`; gff-hab never reuses one.
- **Branches.** A local branch is checked out (with `base` too it is a `conflict`). With no
  `base`, a branch that exactly one remote has becomes a local branch that tracks it. Otherwise
  a new branch starts at `base`, else at the source checkout's HEAD, with `--no-track`, so its
  first push names its own branch, never the base. No branch: `wt/<word>-<word>-<4 hex>`. A
  branch checked out elsewhere, or on several remotes with no base, is a `conflict`; a bad name
  is `invalid`; an unknown base is `not_found`. gff-hab never fetches: `base: origin/main` is
  whatever the last fetch brought.
- **Included files.** `.worktreeinclude` is read from the source checkout. gff-hab asks git for
  the untracked files its patterns match (`ls-files --others --ignored --exclude-from`), keeps
  those git ignores anyway (`check-ignore --stdin`), and copies each: regular files with their
  mode, symbolic links as links, folders (nested repositories) skipped, a file the worktree has
  kept. Every write goes through an `os.Root` of the new worktree. So a folder the checkout holds
  as a symbolic link that leaves the worktree is refused: a foreign branch cannot steer the
  user's ignored files elsewhere. The copy stops past 10000 files or 1 GiB, or at the call's
  deadline. Any copy error removes the worktree again, and the branch when gff-hab made it.
- **The project file.** `flightdeck.json` at the repository root, read from the new worktree,
  so it is the version committed on the start point:
  `{"worktree": {"setup": "...", "teardown": "..."}}`, each a script or a list of commands.
  Unknown keys are ignored, so scripts and services can join later. A file that does not parse
  leaves the worktree in place with setup `error` and the reason.
- **Setup runs in a session.** The script runs as `/bin/sh -c 'set -e; ...'` in a new pane of
  the project, named `setup-<folder>`, working in the worktree. So it outlives a gff-hab
  restart, its output shows in the terminal view, and the session's exit status says how it
  ended. The caller owns the pane, as with `session.create`. `worktree.create` does not wait
  for it; a client that wants an agent after setup starts it once the session exits. Scripts
  get `GFF_SOURCE_CHECKOUT`, `GFF_WORKTREE` and `GFF_BRANCH`.
- **Setup from someone else waits for a yes.** A false alarm costs one yes; a miss runs a
  stranger's script. So the check leans to foreign. No setting or level turns this off, not even
  Bypass permissions (`app.md` 20.1). In `auto`, setup does not run when any of
  these holds:
  - the start ref, or a local branch's `branch.<b>.merge`, has a pull request segment: `pull`,
    `pr`, `pull-request`, `pull-requests`, `merge-requests`, `mr` or `changes`, in any case
    (`gh pr checkout`,
    a fetch of `pull/N/head`);
  - the start ref is a branch of a remote whose URL is not the project's remote;
  - a local branch's `branch.<b>.remote` is another remote or URL;
  - no branch of the project's remote contains the start commit, and the commits it lacks were
    not all made here (the reflog rule below). This catches a commit id, `FETCH_HEAD`,
    `^{commit}`, `~0` and a fetched branch with no upstream. A project with no remote checks
    against the source checkout's HEAD instead.

  **A branch of the project's remote** is a ref under `refs/remotes/<r>/`, for each remote whose
  URL is the project's, that passes two layers:
  1. The remote's fetch refspecs (`remote.<r>.fetch`; a leading `+` dropped, negative `^`
     refspecs skipped). The ref must match the destination of a refspec whose source starts
     with `refs/heads/`, and match the destination of no refspec whose source does not. The
     second clause matters: the default `+refs/heads/*:refs/remotes/origin/*` also matches
     `origin/pr/7`, so a refspec such as `+refs/pull/*/head:refs/remotes/origin/pr-*` (any
     name) keeps its refs out only through it. A destination has at most one `*`. A catch-all
     such as `+refs/*:refs/remotes/origin/*` trusts nothing: false alarms only.
  2. The name, for a one-off fetch into the remote's namespace that no refspec names: the part
     below `refs/remotes/<r>/` may have no pull request segment (as above).

  **The reflog rule** keeps the user's own unpushed work from waiting for a yes. gff-hab lists
  the commits in the start commit's history that no branch of the project's remote holds (at
  most 200; more is foreign). The start point counts as own only when each of them has a
  `commit:`, `commit (initial):` or `commit (merge):` entry in a reflog, or a
  `commit (amend):` entry whose replaced commit is itself own by this rule. The replaced
  commit is the entry's old value: the next older entry of the same ref, by the `n` of
  `ref@{n}` (git lists several refs' entries interleaved by date). gff-hab accepts that pair
  only when both commits have the same parents and the same author name, email and date, which
  `git commit --amend` keeps. The reason is gc: its expiry (30 days for an unreachable commit)
  can drop the entry before an amend, so that the next older entry is a different commit.
  `--reset-author` amends then give a false alarm only. One residual (H1): after gc's expiry, a
  pull request built to copy the parents and author of the commit the user had checked out
  just before it can pass as own. It needs the user to keep that pull request checked out for
  weeks around a gc, and the attacker to know that commit's parents and author date to the
  second.

  Rebased and cherry-picked commits count as own when their source does (owner decision
  2026-11-01). A pick entry names only the copy: a `cherry-pick:` entry, or a rebase step that
  keeps its changes, whose action (the part before the first `: `) starts with `rebase` or
  `pull` and ends in `(pick)`, `(reword)` or `(edit)`. That covers `git rebase`,
  `git rebase -i` and `git pull --rebase` with any arguments
  (`pull -q --rebase origin main (pick):`). gff-hab pairs the copy with a source: a commit some
  reflog entry names that has the same author name, email and date (which both keep, and which
  bounds the work) and the same patch id, the same changes. The patch id is `git patch-id
  --verbatim` over `git diff-tree -p --full-index --no-color --no-ext-diff --no-textconv
  -U3`: whitespace counts (in a script a space can change the meaning), a binary file is
  compared by its full blob ids, and user config cannot change the diff. A git without
  `--verbatim` pairs nothing: false alarms only. The copy then brings in nothing its source
  did not, and the source must itself be own by this rule. Patch ids are computed only for the
  pick commits the walk reaches, with their same-author candidates, in one batch per step, so
  the cost follows the commits under check, not the length of the reflog. `squash` and `fixup`
  steps, and a rebase that resolved a conflict (`rebase (continue):`), change the changes: they
  wait for a yes, a false alarm. So does a rebase of commits made on another machine, which no
  local entry names.

  The chain of amends, rebases and cherry-picks is followed back at most 8 steps, and a set of
  commits already visited (each marked foreign while its own check runs) ends a cycle at once:
  `--no-edit` amends in one second make the same commit again. A commit marked foreign that way
  can only cause a false alarm. So amending, rebasing or cherry-picking a fetched pull request
  (`gh pr checkout 7 && git commit --amend`) stays foreign. gff-hab reads the reflogs that
  `git commit` writes: each worktree's HEAD (`HEAD`, `main-worktree/HEAD`,
  `worktrees/<id>/HEAD`) and the local branches (`--branches`), never the remote-tracking
  reflogs, which can hold thousands of fetch entries. Commit ids go to git on stdin, so a long
  reflog never runs into the argument size limit.

  Only local git writes the reflog, so a fetched pull request commit has no such entry: only
  `fetch` and `checkout`. A local commit stacked on a pull request still fails, through its
  fetched parent. A start point with no reflog entry gives foreign, the safe side: for example
  a repository that never wrote logs, or an entry past `gc.reflogExpire`. (Setting
  `core.logAllRefUpdates=false` later changes nothing: git still appends to log files that
  exist.) Entries from `am` do not count.

  The reflog shows who made a commit, not whose changes it holds. A local `git commit` that
  takes in a pull request's changes counts as own: `git merge --squash` then `git commit`,
  `git cherry-pick -n` then `git commit`, and a conflicted cherry-pick finished with
  `git commit`. Code cannot see these.

  A known false alarm: a local `git merge` or `git pull` that makes a merge commit (reflog
  subjects `merge <x>:` and `pull:`) waits for a yes until it is pushed.

  gff-hab checks the commit the worktree really got: `git worktree add` resolves the start
  point again, so a fetch in between could move it. A worktree whose HEAD is not the checked
  commit is foreign.

  The result is `pending` with the reason and both scripts (`script`, `teardown_script`), so
  the client shows what the yes covers. `worktree.setup` (or `setup: run` on create) is the yes.
  Any control client may give it; the hab is single-user, as at the bar.
- **The mark.** gff-hab writes `gff-worktree.json` in the worktree's private git folder
  (`.git/worktrees/<name>/`). It holds the source checkout, when it was made, whether its scripts
  are approved, a SHA-256 digest of the setup and teardown scripts approved, and why setup
  waited. Git ignores the file and removes it with the worktree; the working tree stays clean.
  `managed` in the list and in the `worktree.remove` result is "this file exists".
- **Teardown.** Runs only when the mark approves exactly the scripts in the worktree now: one
  this hab made from the project's own code, or one the user said yes to, with no change to
  `flightdeck.json` since. Otherwise teardown is `skipped`, and `teardown_output` says why (no
  yes, or the scripts changed since). It runs in gff-hab, not in a pane, in its own process
  group, bounded to 2 minutes, with stdin empty. At the deadline the whole group is killed. A
  non-zero exit, a timeout or a start failure is `failed` with the last 4 KiB of output.
- **Removal.** Never the main working tree (`invalid`, even with force). A locked worktree is a
  `conflict`: unlock it with git. Without `force`, modified, staged or untracked files are a
  `conflict`; ignored files do not count. Without `force`, gff-hab checks again after teardown.
  A teardown that left files (a log, say) stops the removal with a `conflict` that names the
  teardown and the files; the worktree stays whole. A tree that changes otherwise between the
  check and the removal is refused by git. A worktree whose folder is gone loses only its own
  entry under `.git/worktrees/`; other entries stay, such as one on a volume that is not
  mounted now. git runs from the main working tree, so removing the folder the project resolved
  to works. The branch is never deleted, so unpushed commits are never lost; that is why
  gff-hab, unlike Paseo, needs no unpushed check. The client's confirm before `force` says when
  a worktree is not `managed`, and that ignored files (a local database, notes) go too.
- **One at a time.** Each project has one create, setup or remove running at a time. A call
  runs to its end if the client goes away, so git is never cut off halfway; each call has
  5 minutes.

**Limits** (at the bar, `landscape.md` 2.13):

- No pull request checkout by number: fetch it as a branch first (it then counts as foreign,
  above).
- A pull request fetched once, by hand, into the project remote's namespace under a name with
  no pull request segment (`git fetch origin pull/7/head:refs/remotes/origin/pr-7`) counts as a
  branch of the remote: no refspec names it, and the name check is a heuristic. A configured
  refspec closes this, whatever the name.
- A local `git commit` that takes in a pull request's changes counts as own:
  `merge --squash` or `cherry-pick -n` followed by `git commit`, and a conflicted cherry-pick
  finished with `git commit`. The reflog cannot tell these apart.
- H1: after gc's reflog expiry, a pull request built to copy the parents and author of the
  commit the user had checked out just before it can pass as own, once amended. Closing it
  fully means reading the files backend's log files, which keep each entry's true old id
  through expiry; reftable repositories would still need the metadata check.
- A rebase with a resolved conflict, a squash or fixup step, and a rebase of commits made on
  another machine wait for a yes.
- A patch id leaves out each hunk's line numbers, so a copy that puts a source's hunk at another
  place in the same file, with the same three lines of context on each side, pairs with it. It
  needs a file with repeated blocks.
- A local merge commit (`git merge`, `git pull`) waits for a yes until it is pushed.
- No scripts, services, ports or terminals from the project file yet (Paseo has them).
- A session whose program runs in a removed worktree keeps running in a folder that is gone.
  Removing an agent's worktree usually leaves it dirty, so the refusal without force catches
  most of these; the client confirms before force.
- No events: a client calls `worktree.list` again after a change. The project's branch fields
  follow the folder of the most recent session (3.9).
- A teardown script runs with gff-hab's own environment, not the pane environment setup gets.

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
mode 0700 and owned by the user; while it is open to other users, gff-hab waits and logs the fix
once (`hab-holder.md` 5.5). gff-hab peeks at the first byte of each connection: the hello
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
- Built so far: only the pairing window of `gff-hab pair` (`transport.md` 6.1), a separate
  short-lived listener that serves the `flightdeck-pair` subsystem and nothing else. The
  `devices.*` methods still answer `unavailable`.
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
  state. `snapshot-vt` is the baseline; `snapshot-ghostty` is offered as a capability (Q6).
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
  program's input as typed text. Programs that query use timeouts and carry on. The holder does not
  answer any query itself in that window (Q7).
- **Focus reports** (focus in and out, when a program asks for them) come only from the size owner's
  client. Other clients' focus changes are dropped.
- **Queries gff-hab does not recognise reach every client** (5.8 lists them: OSC 52 clipboard
  reads, `CSI 14 t` and `CSI 16 t`, a query with a C0 byte inside its CSI, a query cut by a gap).
  Each client's parser must discard its own replies to those queries, and the client stages must
  build this. Otherwise two clients on one pane answer twice, and an OSC 52 read could send a
  client's clipboard to the program. Built in gff (`gff-client.md` 3.4): its parsers answer
  nothing, and its pass-through view keeps every query away from the host terminal.

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
- **Resize frames.** The link resizes the parser and then the PTY (2.3), and every stream gets a
  resize frame at the offset the parser had reached. When the stream's scanner holds back the start of
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
  (`CSI ? u`), colour queries (OSC 10, 11 and 12 with `?`, and OSC 4 when every entry asks),
  DECRQSS and XTGETTCAP (answered "not supported"), and the program status query
  (`OSC 7501 ; ?`, answered with the supported states and kinds; `harness-adapters.md`, Claude
  Code). Status reports (`OSC 7501 ; state=...`) are not queries: they pass on to clients.
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

Each hab has its own seq and epoch. A client merges hosts by device id and never compares seq across
hosts. No hab relays another hab's events in this part. Hab-to-hab links carry only catalog, channel
messages and control items (`mesh.md` 4.2). The client merge is built in `internal/merge` (`mesh.md` 4.5).

---

## 8. Open questions

Each question has options, a recommendation and the reason. Each now ends with its decision.

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

Decision: C, no interim; the holder came first (owner). Stage 9 is dropped. The earlier GFF stays
the owner's daily driver until Flightdeck is stable.

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

Decision: B, 90 days and at least two revisions (coordinator, 2026-11-01). Not user-visible; the
compat test already enforces translators down to `MinRevision`.

### Q5. Event replay on reconnect

| Option | For | Against |
|---|---|---|
| A. No replay (Rex, herdr-core): the client re-reads state | Simple; the seq rule already closes the race | A phone on a bad link re-reads often |
| B. A per-process ring of recent events; a reconnect within the same epoch resumes from a seq | Cheaper reconnects | Needs a process-wide seq beside the per-connection seq; more to test |

Recommendation: **A** for the first release. Structure is small; a re-read is cheap. Revisit if the
phone's reconnect traffic measures high.

Decision: A, no replay (coordinator, 2026-11-01). Rex and herdr-core re-read state too
(`landscape.md` 2.18); none was built.

### Q6. Snapshot encoding baseline

| Option | For | Against |
|---|---|---|
| A. Plain VT bytes baseline, Ghostty format as a capability | Any client can draw it, including a pass-through Go view | Loses some state (exact scrollback attributes, some modes) |
| B. Ghostty format only (Rex) | Exact state; same parser everywhere | Every client must embed libghostty, including the Go client and scripts |

Recommendation: **A.** The Go client's single-pane view passes bytes to the host terminal, and the
CLI should not need libghostty. The app can ask for the Ghostty format.

Decision: A (coordinator, 2026-11-01). Built: gff-hab advertises `snapshot-vt` with data streams.

### Q7. Terminal queries while gff-hab is down

| Option | For | Against |
|---|---|---|
| A. Nobody answers; programs time out | The holder stays a pure byte pump | A program that waits without a timeout hangs until gff-hab returns |
| B. The holder answers a fixed set (primary device attributes only) while no gff-hab is connected | Covers the common startup query | The holder starts scanning bytes, against "parses nothing"; it must also tell gff-hab which queries it answered |

Recommendation: **A.** gff-hab restarts take seconds. Most programs query with a timeout. Revisit if
a supported harness hangs. Align with the holder spec's OSC 133 decision, which faces the same
"scan or not" question.

Decision: A, nobody answers (coordinator, 2026-11-01). The holder stays a byte pump, as it does for
OSC 133 (`hab-holder.md` Q1).

### Q8. Reconnect grace period

Proposed 60 s. Longer keeps ownership through a short tunnel or a lift; shorter frees panes from a
phone that will not return. Recommendation: **60 s**, configurable.

Built (stage 8): size ownership uses the reconnect grace (`control.DefaultReconnectGrace`, 60 s),
so a client keeps its sizes and its claim to restore exactly as long as its token. Not yet a
flag.

Decision: 60 s, configurable later (coordinator, 2026-11-01). Built as `control.DefaultReconnectGrace`.

---

## Notes for later stages

- Stage 2 (schema): the hello, welcome, error `kind` list, event envelope and data frame kinds above
  are the first types.
- Stage 4 (holder link): 2.3.
- Stage 5 (structure): 2.2.
- Stage 6 (control connection): 3 and 4.1, 4.2.
- Stage 7 (data connections): 5.
- Stage 8 (pane size): 6.
- Stage 9 (zellij backend): dropped (Q1).
