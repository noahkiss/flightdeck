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

### 2.3 The holder link (logic side only)

gff-hab is a client of every live holder on its host: the current one and any that are draining.

- On start it connects to each holder, agrees a holder protocol version, and reads the pane list.
- It keeps one parser per pane. Holder output enters the parser in order, with its absolute byte
  offset.
- Measured cost of one parser: a 120x40 libghostty terminal holding 10,000 coloured 120-column
  lines of scrollback reports 10 MiB resident (libghostty's own memory figure, Linux amd64, release
  build). Its `snapshot-ghostty` is 4.6 MiB and its `snapshot-vt` 1.3 MiB, for 1.2 MiB of input.
  `TestMemoryLongScrollback` in `internal/vt/ghostty` reproduces it (`go test -v`).
- It writes a **checkpoint** per pane (a parser snapshot plus the offset it covers) when the pane
  goes idle and every N seconds. The holder spec owns the ring; this spec owns the checkpoint.
- **Planned restart:** gff-hab writes fresh checkpoints, tells each holder it is handing over with
  the offsets it saved, and exits. The next gff-hab loads the checkpoints and asks for bytes after
  each saved offset.
- **Crash:** the next gff-hab loads the latest checkpoint and replays the ring from that offset. If
  the ring no longer reaches back that far, it rebuilds from the ring alone and marks the pane's
  screen as **suspect** (see 6.7).
- gff-hab must speak every holder protocol version still running, because a drained holder can
  live for weeks.
- Input, resize and signal requests from clients go to the holder that owns the pane.

Decision: proposed. Details of the frames belong to the holder spec.

### 2.4 Backends

gff-hab talks to its terminals through a backend interface with two implementations:

| Backend | Structure and lifecycle | Raw bytes | Data streams |
|---|---|---|---|
| gff-hab-holder | Pane list, events from the holder | Yes | Yes |
| zellij (interim) | The fork's event stream plus a tree read on a gap | No | Not available; capability absent |

Under the zellij backend the terminal view attaches a real zellij client. Seq and epoch are minted
by gff-hab, never taken from zellij. Whether this interim backend exists at all is Decision: open
(Q1).

---

## 3. API rules

### 3.1 Protocol

- **JSON-RPC 2.0.** Requests, responses and notifications as the spec defines them.
- **Plain mode by default:** one JSON message per line, UTF-8, no embedded newlines. A socket tool
  can drive it.
- Every result is a JSON object, never a bare value, so fields can be added later.
- Requests on one connection may run side by side. A client must match responses by id.
- Method names are grouped by prefix: `hab.*`, `structure.*`, `pane.*`, `session.*`, `project.*`,
  `data.*`, `events.*`, `rpc.discover`.

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
| `seq` | Always 0: no event has been queued yet |

If no revision fits, gff-hab answers with an error of kind `revision_unsupported` that carries both
ranges, then closes. The client tells the user which side to update.

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
  `pane.owner` (6.5) and `pane.activity` (7.1).
- Word sets the server sends are open and may gain words; word sets the client sends are closed
  (see above). The compat test applies both rules.
- The compat test compares against the highest released revision. A higher current revision must
  be exactly one higher and have a translator (`api.Translators`) for every step down to
  `min_revision`.
- `rpc.discover` returns the schema document itself, so its result carries no `seq`.

### 3.4 Capabilities

Capability strings name optional features: `data-streams`, `snapshot-vt`, `snapshot-ghostty`,
`agent-state`, `transcripts`. A client checks for a capability before it uses the feature. The
zellij backend omits `data-streams`. Capabilities are for features that a host may lack. They are
not a second versioning scheme.

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
  with the object, so it survives restarts. A client ignores an event whose revision is not higher
  than the one it holds. Deleting an object emits a final event with a final revision.
- **Ephemeral events carry no revision.** `pane.owner` and `pane.activity` describe state that is
  not stored (owners are dropped on a new epoch). Their `revision` is absent, and the "ignore a
  revision not higher" rule does not apply: a client applies them in seq order.
- **Epoch.** A random id chosen at each gff-hab start, sent in the welcome. A client that reconnects
  and sees a new epoch drops everything ephemeral (owners, tickets, desired sizes) and takes a fresh
  snapshot. Object revisions stay comparable across epochs because they are stored.
- **Heartbeat.** A `heartbeat` notification with the current seq every 15 s when no other event was
  sent. A client that hears nothing for 45 s treats the connection as dead.
- **Client liveness.** A client sends `hab.ping` every 15 s when it has sent nothing else. gff-hab
  treats a control connection that sends nothing for 45 s as closed (reason `disconnect`).
- **Backlog limit.** If more than 8 MiB of events wait for one connection, gff-hab sends a final
  `closing {reason: "event_backlog"}` notification and closes. The client reconnects and reads
  state again.

Decision: proposed. Whether a reconnect within the same epoch can replay missed events is
Decision: open (Q5).

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

### 3.8 Input

- Input goes over the control connection: `pane.input {pane, data}`, where `data` is base64 bytes,
  or `text` for UTF-8 convenience.
- One `pane.input` call reaches the holder as one write. Calls from different clients never
  interleave inside a call. Calls are applied in the order gff-hab receives them.
- Input counts as activity for size ownership (6.2).
- gff-hab never writes to a program's input on its own, except for answers to terminal queries (5.6).

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

### 4.2 `gff-hab connect` over SSH stdio

- `gff-hab connect` bridges its stdin and stdout to the local socket. `ssh <host> gff-hab connect`
  reaches a remote hab with no new listener. SSH keys stay the authentication.
- The bridge is dumb: it copies bytes. It does not parse JSON-RPC.
- **How data connections ride it** is Decision: open (Q2). The options:
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
  opens a second SSH connection. B is the fallback if a client cannot open many channels.

### 4.3 Tailnet WebSocket (Part 3)

The tailnet WebSocket belongs to Part 3, including its authentication, which the owner must approve.
This spec only requires that it carries the same messages: one WebSocket text message per JSON-RPC
message on the control connection, and one binary message per data frame on a data connection.

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
in-band nonce is Decision: open (Q3); this spec recommends frames.

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

### 5.7 Backlog and flow

- A stream starts with 256 KiB of credit. The client returns credit as it applies bytes.
- gff-hab never queues more than the credit for a stream. A stalled stream never blocks the control
  connection or other streams.
- A client more than 4 MiB behind gets a fresh snapshot instead of the backlog.
- gff-hab keeps a per-pane buffer of recent output for resumes (proposed: 4 MiB, at least as large as
  the holder ring). A resume from before the buffer gets a snapshot.

Decision: proposed.

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

The bm task for stage 8 says "release"; this spec uses `disown` throughout, following Rex. One word
per concept: the stage 8 task should use `disown`.

### 6.4 Observers

- A data connection opened with `observe: true` (a thumbnail, a dashboard) never takes ownership,
  never sets a desired size and never sends input.
- An observer's own view size never reaches the PTY.

### 6.5 Owner changes

- Reasons: `create` (the client that created the pane), `take`, `request`, `typed` (the debounce in
  6.2), `disown`, `lost` (another client took it), `disconnect` (the owner's connection closed),
  `restore` (the owner reconnected within the grace period).
- **On disconnect** the pane keeps its size and has no owner. No resize happens, so the program does
  not reflow because a phone dropped.
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

Decision: open.

### Q3. Control messages in the data stream

| Option | For | Against |
|---|---|---|
| A. Frames on every transport (5.3) | A program cannot forge a frame; offsets are explicit; the same frames work on a socket, an SSH channel and a WebSocket | The client cannot pipe the stream straight into a terminal; it must unwrap frames |
| B. In-band escape sequences with a per-stream nonce (Rex) | The raw stream stays a valid terminal stream | Every client must scan for the sequence; offsets need their own message |

Recommendation: **A.** We need framing on the Unix socket and SSH anyway, and it makes the nonce
unnecessary. Stage 7's acceptance check "a program prints a fake control message; the client
ignores it" still applies and passes by construction.

Decision: open.

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
