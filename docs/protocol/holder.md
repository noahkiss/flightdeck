# Holder protocol

The wire protocol between gff-hab (the logic process) and gff-hab-holder (the holder). The
behaviour behind it is in [`docs/specs/hab-holder.md`](../specs/hab-holder.md), sections 2.6, 3
and 4. The code is `internal/holderproto`. This document and the code must agree; the golden
fixtures in `internal/holderproto/testdata/` are the byte-exact reference.

Contents:

1. [Connection](#1-connection)
2. [Frame header](#2-frame-header)
3. [Field encoding](#3-field-encoding)
4. [Version negotiation](#4-version-negotiation)
5. [Frames](#5-frames)
6. [Offsets and replay](#6-offsets-and-replay)
7. [Input](#7-input)
8. [Limits and pacing](#8-limits-and-pacing)
9. [Handover, crash and drain](#9-handover-crash-and-drain)
10. [How versions evolve](#10-how-versions-evolve)
11. [Tests and fixtures](#11-tests-and-fixtures)

---

## 1. Connection

- gff-hab connects to the holder's Unix socket (spec 2.10). The stream carries frames, nothing
  else.
- gff-hab sends hello first. The holder answers with welcome or refuse. No other frame may cross
  before that.
- After the handshake both sides send frames at any time. Each side writes each frame in one
  write, under one lock, so frames never interleave.
- The holder writes through one queue and one writer (spec 2.6). Its reader never waits on a
  write; a reader that did could deadlock against a peer that is itself blocked writing.
- gff-hab keeps one connection per live holder. Each connection has its own negotiated version.
  During a drain, one gff-hab holds connections at two versions at once (section 9).

## 2. Frame header

Every frame is a 14-byte header followed by the payload. All integers are big-endian.

| Offset | Size | Field | Meaning |
|---|---|---|---|
| 0 | 2 | kind | Frame kind (section 5) |
| 2 | 4 | pane | Pane handle, or 0 when the frame concerns the connection |
| 6 | 4 | token | Request token, or 0 (below) |
| 10 | 4 | length | Payload length in bytes (section 8) |
| 14 | length | payload | Fields of the kind, at the negotiated version |

**Token.** gff-hab picks a token for each request. The holder copies it into every reply to that
request: the answer, the replay frames, or an error. Events (output, exited, input ready, bye)
and the handshake carry 0. The token sits in the header, not the payload, so the holder can echo it even
for a request whose kind it does not know. gff-hab should not reuse a token while a request with
it is open; 0 is a legal token but cannot be told apart from an event.

Rules:

- **Too long.** A length over a limit (section 8) ends the connection. The receiver cannot tell a
  corrupt length from a real one.
- **Unknown kind.** A receiver skips, by length, a frame whose kind it does not know. It also
  skips a kind it knows that is newer than the negotiated version. The holder answers each
  skipped frame with an error, code 7 (unknown kind), carrying the frame's pane, token and kind.
  gff-hab skips silently.
- **Bad payload.** A payload of a known kind that is too short for its fields is a protocol error.
  The receiver drops the connection.
- **Extra bytes.** A receiver ignores payload bytes after the last field it knows. Only the
  handshake relies on this between versions (section 10).

Pane handles are small numbers the holder assigns at spawn. They are unique within one holder
instance. The pane uuid is unique everywhere.

## 3. Field encoding

Control payloads are compact binary, not JSON. One encoding for every frame keeps the decoder
small and fuzzable, gives byte-exact golden fixtures, and puts no JSON work on the output path.

| Type | Bytes | Encoding |
|---|---|---|
| u8, u16, u32, u64 | 1, 2, 4, 8 | Unsigned, big-endian |
| i32 | 4 | Two's complement, big-endian |
| bool | 1 | 0 is false; any other value is true. Senders write 1 |
| uuid | 16 | The uuid's 16 bytes |
| string | 4 + n | u32 byte count, then UTF-8 bytes |
| strings | 4 + … | u32 count, then that many strings |
| record | 4 + n | u32 byte count, then the record's fields. Used for list items, so items can grow |
| fixed | 2 + n | u16 byte count, then fixed fields. Used before rest |
| rest | n | Raw bytes to the end of the payload |

**Kinds that end in raw bytes** (input, output, replay data) start with a *fixed* part: a u16
length, then their fixed fields. The raw bytes follow it. A later version can append fixed fields
and every receiver still finds the raw bytes.

**Exit status**, used in exited and in the pane record:

| Field | Type | Meaning |
|---|---|---|
| signaled | bool | A signal ended the child |
| code | i32 | Exit code; 0 when signaled |
| signal | u8 | The signal in protocol numbers (5.2); 255 (other) when the protocol has no number for it; 0 when not signaled |
| host signal | i32 | The signal as the holder's host numbers it, for logs only. Never compare it: Linux and macOS number signals differently |

## 4. Version negotiation

- The protocol version is a u16, separate from the build id. Version 1 is the current version.
- hello carries the version range gff-hab speaks (min..max) and its build id.
- The holder picks the highest version in both ranges. It answers welcome with that version.
- With no common version, the holder answers refuse with its own range and a reason, then
  closes. gff-hab then knows both ranges. It starts a holder of its own build (spec 4.3).
- hello, welcome and refuse are the handshake. Any version reads them. welcome is laid out at the
  version it names, so a receiver reads the version field first.
- From welcome on, each side encodes and decodes every frame at the negotiated version. A side
  must not send a kind newer than that version; the Go codec refuses it with `ErrNotInVersion`.

In Go, a `Registry` holds the kinds and the range one side speaks. A `Conn` holds the version it
negotiated. `Conn.Hello` runs the gff-hab side; `Conn.AcceptHello` runs the holder side up to the
welcome, which the holder then sends with `Version` set to `Conn.Version()`. The holder sets
`Conn.OnSkip` to queue `UnknownKind(header)` for every skipped frame.

## 5. Frames

Direction: L is gff-hab (logic), H is the holder. Pane and token are header fields. A frame with
no payload fields has length 0.

### 5.1 Handshake and connection

| Kind | Name | Dir | Pane | Token | Payload, in order |
|---|---|---|---|---|---|
| 0x0001 | hello | L→H | 0 | 0 | min u16, max u16, build string |
| 0x0002 | welcome | H→L | 0 | 0 | version u16, instance string, pid u32, build string, ring size u64, draining bool, previous u8, pane count u32, then pane count × pane record |
| 0x0003 | refuse | H→L | 0 | 0 | min u16, max u16 (the holder's range), reason string |
| 0x0004 | ping | either | 0 | yes | none |
| 0x0005 | pong | either | 0 | ping's | none |
| 0x0006 | handover | L→H | 0 | yes | none |
| 0x0007 | drain | L→H | 0 | yes | none |
| 0x0008 | shutdown | L→H | 0 | yes | none |
| 0x0009 | bye | H→L | 0 | 0 | none. The last frame before the holder closes |
| 0x000a | error | H→L | request's | request's | ref kind u16, code u16, offset u64, message string |

**previous** (welcome): how the last logic process left. 0 none (first start), 1 handover,
2 crash (the connection ended without handover, hung, or a newer hello replaced it).

**Pane record** (welcome), 63 bytes at version 1, inside a record:

| Field | Type | Meaning |
|---|---|---|
| uuid | uuid | Pane uuid |
| handle | u32 | Pane handle for frame headers |
| pid | u32 | Child pid |
| cols, rows | u16, u16 | Current size |
| count | u64 | Bytes the pane has produced since spawn |
| oldest | u64 | Offset of the oldest byte still in the ring |
| input count | u64 | Input bytes the holder has accepted for the pane (section 7) |
| exited | bool | The child has been reaped |
| exit status | 10 bytes | Section 3; zero unless exited |

**error**: the holder refused or failed a request. *ref* is the request's kind; the header
carries its pane and token. *offset* is the expected input offset for codes 1 and 8, and 0
otherwise. "Input refused" (spec 3.1) is an error with ref 0x0103. Codes, also used by spawn
failed:

| Code | Name | Meaning |
|---|---|---|
| 0 | unknown | Anything else; read the message. A receiver treats an unknown code as 0 |
| 1 | queue full | The pane's input queue cannot take the whole input frame |
| 2 | no pane | No pane has this handle |
| 3 | draining | The holder is draining and takes no new panes |
| 4 | bad request | A field is out of range, or the request makes no sense now |
| 5 | exited | The pane's child was already reaped |
| 6 | start | The program could not start |
| 7 | unknown kind | The holder skipped the request: unknown kind, or newer than the version |
| 8 | input offset | The input frame's offset is not the pane's input count |

### 5.2 Panes

| Kind | Name | Dir | Pane | Token | Payload, in order |
|---|---|---|---|---|---|
| 0x0100 | spawn | L→H | 0 | yes | uuid, cols u16, rows u16, cwd string, argv strings, env strings (complete, `KEY=value`) |
| 0x0101 | spawned | H→L | new handle | spawn's | uuid, pid u32, existing bool |
| 0x0102 | spawn failed | H→L | 0 | spawn's | uuid, code u16, message string |
| 0x0103 | input | L→H | handle | yes | fixed {offset u64}, rest: the bytes to write to the PTY |
| 0x0104 | output | H→L | handle | 0 | fixed {offset u64}, rest: the bytes |
| 0x0105 | resize | L→H | handle | yes | cols u16, rows u16 |
| 0x0106 | signal | L→H | handle | yes | target u8, signal u8 |
| 0x0107 | foreground query | L→H | handle | yes | none |
| 0x0108 | foreground | H→L | handle | query's | pgid i32 (0: no foreground group) |
| 0x0109 | exited | H→L | handle | 0 | exit status (section 3), final u64 |
| 0x010a | release | L→H | handle | yes | none |
| 0x010b | replay | L→H | handle | yes | from u64, has to bool, to u64 |
| 0x010c | replay data | H→L | handle | replay's | fixed {offset u64}, rest: the bytes |
| 0x010d | replay end | H→L | handle | replay's | start u64, end u64, gap bool |
| 0x010e | input ready | H→L | handle | 0 | offset u64 (the input offset the holder now expects) |

Notes:

- **spawn / spawned.** A spawn for a uuid the holder already runs answers spawned with existing
  set, so a retry is harmless (spec 2.2).
- **Requests with no success reply** (input, resize, signal, release, handover, drain,
  shutdown) get an error only when they fail. To know that every earlier request was handled,
  send ping and wait for its pong: the holder handles a connection's frames in order.
- **signal.** *target* 0 is the child (the session leader), 1 the PTY's foreground process group.
  *signal* uses the protocol's own numbers: 1 HUP, 2 INT, 3 QUIT, 4 TERM, 5 KILL, 6 WINCH,
  7 CONT, 8 STOP, 9 USR1, 10 USR2. gff-hab may send only these. The holder answers any other
  number with error code 4. Exit status uses the same table, extended for signals a child can die
  of: 11 PIPE, 12 ALRM, 13 SEGV, 14 ABRT, 15 BUS, 16 FPE, 17 ILL, 18 TRAP, 19 SYS, 20 TSTP,
  21 TTIN, 22 TTOU, 23 XCPU, 24 XFSZ, 25 VTALRM, 26 PROF, 255 other.
- **exited.** The holder always sends exited when it reaps a pane's child while a logic process
  is connected, whether or not the pane is subscribed (section 6). A child reaped while no logic
  process is connected shows in the next welcome's pane record. For a child reaped during the
  handshake, exactly one of the two reports it, never both and never neither: if the welcome's
  pane record says exited, no exited frame follows for that pane on this connection; otherwise an
  exited frame follows the welcome. *final* is the pane's byte count
  when the holder sent the frame: gff-hab holds all output once it holds every byte before
  *final* (spec 2.7).
- **No mark frames.** gff-hab finds OSC 133 prompt marks itself (spec Q1). A command's bytes are
  a bounded replay.

## 6. Offsets and replay

- Each pane has a byte count: a u64 offset from spawn that never resets.
- **output** carries the offset of its first byte. The next output for the pane starts at
  offset + length. gff-hab detects a gap or a duplicate by comparing offsets.
- **replay** asks for the ring bytes in [from, to). With *has to* false the range is open, and
  *to* is ignored. Every replay data and the replay end carry the replay's token.
- The holder answers with zero or more replay data frames in offset order, then one replay end.
  Replay end says the holder sent exactly [start, end).
  - *start* is from, or the oldest kept offset if from is older. Then *gap* is true: bytes in
    [from, start) left the ring (spec 6.4).
  - *end* is to, or the pane's count if that is smaller, or the count for an open replay. The
    count is the one when the request arrives, so every replay ends, however fast the program
    writes.
  - If the ring overwrote bytes in [start, end) before the holder sent them, the holder skips
    them: the next replay data starts at a later offset, and *gap* is true as well.
  - A from beyond the count gives start = end = count.
  - An unknown handle gives an error with ref 0x010b and code 2, and no replay end.
- **Subscribing.** After welcome the holder sends no output on its own. An open replay
  subscribes the pane: after its replay end, live output follows from *end*. A pane spawned on
  this connection is subscribed from offset 0 at once.
- **Re-opening.** gff-hab may send an open replay for a pane that is already subscribed. What it
  observes:
  - Live output queued before the re-open arrives before the replay.
  - No live output for the pane arrives between the re-open and its replay end.
  - The pane's reader keeps filling the ring the whole time; programs never wait on a replay.
  - *end* is the pane's count when the re-open arrives. Live output from *end* on queues behind
    the replay and counts against the backpressure limit (section 8). It resumes after the
    replay end at *end*, with no overlap.
  - If the ring overwrote bytes before the holder sent them, the next replay data frame starts
    at a later offset than the previous frame ended, and the replay end says gap. gff-hab treats
    the screen as it would after a gap at the start.
- **Bounded replays** do not change the live stream. Their replay data may arrive between live
  output frames; the token and the frame kind tell them apart.
- The holder never discards ring bytes at handover. Only the ring size limits how far back a
  replay reaches.

## 7. Input

- Each pane has an **input count**: the input bytes the holder has accepted since spawn. Welcome
  reports it in each pane record.
- Each input frame carries *offset*: the input count gff-hab believes comes before this frame.
- The holder accepts an input frame only when *offset* equals the pane's input count. It then
  queues all of the frame's bytes, and the count grows by their length. It never queues part of a
  frame.
- Otherwise the holder refuses the whole frame with an error: code 8 (input offset) when the
  offset is wrong, code 1 (queue full) when the pane's 1 MiB input queue cannot take all of it
  (spec 2.5). The error's *offset* is the input count the holder expects.
- After code 8, gff-hab resends from that offset at once.
- After code 1, gff-hab waits for input ready before it resends. The holder sends input ready
  once, when the pane's input queue is empty again after a queue-full refusal. It carries the
  input offset the holder now expects, and gff-hab resends from there. A full queue therefore
  costs no retry loop.
- After a restart, gff-hab starts from the input count in the welcome. Input that a crashed
  gff-hab sent but the holder never accepted is not written twice.

## 8. Limits and pacing

| Limit | Value | On a violation |
|---|---|---|
| Any payload | 16 MiB | Protocol error: the receiver drops the connection |
| Bytes in one output or replay data frame | 64 KiB | Protocol error. The codec refuses to send it, and a receiver drops the connection |
| Bytes in one input frame | 1 MiB | Protocol error, as above. gff-hab splits larger input over several frames |

- **Replay pacing.** The holder produces replay data as its writer drains, one frame at a time,
  never the whole range up front. A large replay therefore holds no more memory than the writer
  queue allows, and counts against the same 8 MiB backpressure limit as live output (spec 2.6).
- **Hung.** The holder marks a connection hung when its writer makes no progress for 5 s while
  frames are queued: not one byte reaches the socket. It writes with 1 s deadlines, so it finds a
  hung connection 5 to 6 s after the last byte moved. A slow but moving reader is not hung. The
  holder then drops the connection and records "crash" (spec 2.6).

## 9. Handover, crash and drain

**Planned restart (handover).**

1. gff-hab saves each screen checkpoint with the byte offset it covers.
2. gff-hab sends handover and closes the connection. Handover carries no offsets: the
   checkpoints hold them.
3. The holder records "handover". It keeps every pane and every ring byte, and keeps pumping.
   The holder reads the frames sent before the close even when a write to the closing
   connection fails first, so a handover followed by a close is recorded as handover. The
   exception: if the holder cannot read the handover within the hang timeout after such a
   write failure, it drops the connection and records crash.
4. The next gff-hab sends hello. welcome says previous = 1 and lists every pane with its count,
   oldest kept offset and input count.
5. For each pane, gff-hab loads its checkpoint and sends an open replay from the checkpoint's
   offset. It parses the replay into the screen, then follows live output.

**Crash.** The connection ends without handover, the holder drops a hung connection (section 8),
or a newer hello arrives while a connection that has not sent handover is open. The holder
records "crash"; the next welcome says previous = 2. A newer hello that replaces a connection
which already sent handover records "handover": a gff-hab may still be closing when its
successor says hello. Before it decides, the holder lets the old connection's reader take
what is already in the socket, for up to 100 ms, so a handover sent just before the newer
hello counts even if the holder has not read it yet. The newest hello wins: an older hello still
waiting for that reader when a newer one arrives ends without a welcome. Recovery is the same as step 5. The only
difference is how fresh the checkpoint is. The holder keeps no state about gff-hab's saves.

**Drain (upgrade).**

1. A new gff-hab build connects to the old holder H1 at the highest common version, and to the
   new holder H2 at the current version.
2. gff-hab sends drain to H1. H1 refuses new spawns with spawn failed, code 3, and keeps serving
   its panes. Later welcomes from H1 say draining.
3. New panes go to H2. gff-hab serves both connections, each at its own version.
4. When H1's last pane is released, H1 sends bye and exits. A released program that ignores the
   hang-up is killed first, after the shutdown grace (spec 4.3 step 6).

**Shutdown.** gff-hab sends shutdown. The holder ends every pane (spec 2.8), sends bye and
closes.

## 10. How versions evolve

- **Every wire change takes a new version number.** A peer at version N expects every field of
  version N, so a field added without a bump would fail as a short payload.
- **A new version may:**
  - add a frame kind, with a new kind number. Older peers never see it: a sender does not send a
    kind newer than the negotiated version.
  - append fields to the end of a payload, a record, or a fixed part. At the old version the
    sender leaves them out.
  - append fields to hello, welcome or refuse. These are read before a version is agreed, so an
    older receiver ignores the extra bytes.
- **A new version must not:** reorder, resize or remove an existing field; change the meaning of
  a field or a code; reuse a kind number; change the header; change the handshake except by
  appending.
- **Keeping old versions.** gff-hab speaks every version a live holder on the host may still use
  (spec 3.2). A drained holder can live for weeks, so a version leaves gff-hab only when no
  holder of that version can remain.
- **In Go.** Each kind is registered with the first version that carries it. A frame type encodes
  by the version it is given, and its decoder reads the fields that exist at that version. A
  frame type that gains fields at version N checks `v >= N` on both paths.
- **Kinds 0x7f00 to 0x7fff** are reserved for tests.
- **Version 1 is not frozen yet.** Until the first holder release, version 1 may change in place
  with its golden files regenerated on purpose. After that release it is frozen.

## 11. Tests and fixtures

- `internal/holderproto/testdata/v1/` holds one golden file per version 1 frame kind. Each file
  is hex: the 14-byte header on the first line, then the payload 32 bytes a line.
- The golden test encodes each sample frame and compares it with its file. It also decodes the
  file and compares the result with the sample. Either mismatch fails. A test also fails when a
  registered kind has no sample.
- `testdata/v2/` holds the same for a test-only version 2. It appends pixel sizes to resize,
  appends a sequence number to output's fixed part, and adds a stats kind (0x7f01). It exists to
  prove two versions work; it is not a real version.
- The cross-version tests run, over `net.Pipe`, a v1 client against a v2 holder, a v2 client
  against a v1 holder, and v2 against v2. A further test holds a v1 and a v2 connection open from
  one client at once. Another sends a v2 hello with appended fields to a v1 holder.
- Other tests cover input offsets and resend, a full input queue and input ready, the
  unknown-kind reply, size limits, the fixed
  part, and protocol signal numbers.
- `go test ./internal/holderproto/ -update` rewrites the golden files. Only do that on purpose:
  once version 1 is frozen, a changed version 1 golden means version 1 broke.
- `FuzzDecodeFrame` feeds the frame decoder random bytes; `FuzzReadFrame` feeds `Conn.ReadFrame`
  a random stream at version 2 (spec 6.6).
