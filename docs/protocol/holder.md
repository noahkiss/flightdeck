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
7. [Handover, crash and drain](#7-handover-crash-and-drain)
8. [How versions evolve](#8-how-versions-evolve)
9. [Tests and fixtures](#9-tests-and-fixtures)

---

## 1. Connection

- gff-hab connects to the holder's Unix socket (spec 2.10). The stream carries frames, nothing
  else.
- gff-hab sends hello first. The holder answers with welcome or refuse. No other frame may cross
  before that.
- After the handshake both sides send frames at any time. Each side writes each frame in one
  write, under one lock, so frames never interleave.
- gff-hab keeps one connection per live holder. Each connection has its own negotiated version.
  During a drain, one gff-hab holds connections at two versions at once (section 7).

## 2. Frame header

Every frame is a 10-byte header followed by the payload. All integers are big-endian.

| Offset | Size | Field | Meaning |
|---|---|---|---|
| 0 | 2 | kind | Frame kind (section 5) |
| 2 | 4 | pane | Pane handle, or 0 when the frame concerns the connection |
| 6 | 4 | length | Payload length in bytes, at most 16 MiB (16 777 216) |
| 10 | length | payload | Fields of the kind, at the negotiated version |

Rules:

- **Too long.** A length over 16 MiB ends the connection. The receiver cannot tell a corrupt
  length from a real one.
- **Unknown kind.** A receiver skips, by length, a frame whose kind it does not know. It also
  skips a kind it knows that is newer than the negotiated version. A skip is not an error.
- **Bad payload.** A payload of a known kind that is too short for its fields is a protocol error.
  The receiver drops the connection.
- **Extra bytes.** A receiver ignores payload bytes after the last field it knows (section 8).

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
| record | 4 + n | u32 byte count, then the record's fields. Used for list items, so items can grow (section 8) |
| rest | n | Raw bytes to the end of the payload. Only the last field may be rest |

## 4. Version negotiation

- The protocol version is a u16, separate from the build id. Version 1 is the current version.
- hello carries the version range gff-hab speaks (min..max) and its build id.
- The holder picks the highest version in both ranges. It answers welcome with that version.
- With no common version, the holder answers refuse with its own range and a reason, then
  closes. gff-hab then knows both ranges. It starts a holder of its own build (spec 4.3).
- hello, welcome and refuse are the handshake. Their layout is frozen: any version reads them.
  welcome is laid out at the version it names, so a receiver reads the version field first.
- From welcome on, each side encodes and decodes every frame at the negotiated version. A side
  must not send a kind newer than that version; the Go codec refuses it with `ErrNotInVersion`.

In Go, a `Registry` holds the kinds and the range one side speaks. A `Conn` holds the version it
negotiated. `Conn.Hello` runs the gff-hab side; `Conn.AcceptHello` runs the holder side up to the
welcome, which the holder then sends with `Version` set to `Conn.Version()`.

## 5. Frames

Direction: L is gff-hab (logic), H is the holder. "pane" is the header pane field. A frame with
no payload fields has length 0.

### 5.1 Handshake and connection

| Kind | Name | Dir | Header pane | Payload, in order |
|---|---|---|---|---|
| 0x0001 | hello | L→H | 0 | min u16, max u16, build string |
| 0x0002 | welcome | H→L | 0 | version u16, instance string, pid u32, build string, ring size u64, draining bool, previous u8, pane count u32, then pane count × pane record |
| 0x0003 | refuse | H→L | 0 | min u16, max u16 (the holder's range), reason string |
| 0x0004 | ping | either | 0 | token u64 |
| 0x0005 | pong | either | 0 | token u64 (the ping's token) |
| 0x0006 | handover | L→H | 0 | none |
| 0x0007 | drain | L→H | 0 | none |
| 0x0008 | shutdown | L→H | 0 | none |
| 0x0009 | bye | H→L | 0 | none. The last frame before the holder closes |
| 0x000a | error | H→L | pane or 0 | ref kind u16, code u16, message string |

**previous** (welcome): how the last logic process left. 0 none (first start), 1 handover,
2 crash (the connection ended without handover, hung, or a newer hello replaced it).

**Pane record** (welcome), 50 bytes at version 1, inside a record:

| Field | Type | Meaning |
|---|---|---|
| uuid | uuid | Pane uuid |
| handle | u32 | Pane handle for frame headers |
| pid | u32 | Child pid |
| cols, rows | u16, u16 | Current size |
| count | u64 | Bytes the pane has produced since spawn |
| oldest | u64 | Offset of the oldest byte still in the ring |
| exited | bool | The child has been reaped |
| signaled | bool | The child ended by a signal (zero unless exited) |
| code | i32 | Exit code, or the host's signal number when signaled (zero unless exited) |

**error**: the holder refused or failed a request. *ref* is the kind of that request. "Input
refused" (spec 3.1) is an error with ref 0x0103 (input). Codes, also used by spawn failed:

| Code | Name | Meaning |
|---|---|---|
| 0 | unknown | Anything else; read the message. A receiver treats an unknown code as 0 |
| 1 | queue full | The pane's input queue is full |
| 2 | no pane | No pane has this handle |
| 3 | draining | The holder is draining and takes no new panes |
| 4 | bad request | A field is out of range, or the request makes no sense now |
| 5 | exited | The pane's child was already reaped |
| 6 | start | The program could not start |

### 5.2 Panes

| Kind | Name | Dir | Header pane | Payload, in order |
|---|---|---|---|---|
| 0x0100 | spawn | L→H | 0 | uuid, cols u16, rows u16, cwd string, argv strings, env strings (complete, `KEY=value`) |
| 0x0101 | spawned | H→L | new handle | uuid, pid u32, existing bool |
| 0x0102 | spawn failed | H→L | 0 | uuid, code u16, message string |
| 0x0103 | input | L→H | handle | rest: the bytes to write to the PTY |
| 0x0104 | output | H→L | handle | offset u64, rest: the bytes |
| 0x0105 | resize | L→H | handle | cols u16, rows u16 |
| 0x0106 | signal | L→H | handle | target u8, signal u8 |
| 0x0107 | foreground query | L→H | handle | none |
| 0x0108 | foreground | H→L | handle | pgid i32 (0: no foreground group) |
| 0x0109 | exited | H→L | handle | signaled bool, code i32, final u64 |
| 0x010a | release | L→H | handle | none |
| 0x010b | replay | L→H | handle | token u32, from u64, has to bool, to u64 |
| 0x010c | replay data | H→L | handle | token u32, offset u64, rest: the bytes |
| 0x010d | replay end | H→L | handle | token u32, start u64, end u64, gap bool |

Notes:

- **spawn / spawned.** gff-hab matches the answer by uuid. A spawn for a uuid the holder already
  runs answers spawned with existing set, so a retry is harmless (spec 2.2).
- **signal.** *target* 0 is the child (the session leader), 1 the PTY's foreground process group.
  *signal* uses the protocol's own numbers, because Linux and macOS number some signals
  differently: 1 HUP, 2 INT, 3 QUIT, 4 TERM, 5 KILL, 6 WINCH, 7 CONT, 8 STOP, 9 USR1, 10 USR2.
  The holder answers any other number with error code 4.
- **exited.** *code* is as in the pane record. *final* is the pane's byte count when the holder
  sent the frame: gff-hab holds all output once it holds every byte before *final* (spec 2.7).
- **No mark frames.** gff-hab finds OSC 133 prompt marks itself (spec Q1, option 2). A command's
  bytes are a bounded replay.

## 6. Offsets and replay

- Each pane has a byte count: a u64 offset from spawn that never resets.
- **output** carries the offset of its first byte. The next output for the pane starts at
  offset + length. gff-hab detects a gap or a duplicate by comparing offsets.
- **replay** asks for the ring bytes in [from, to). With *has to* false the range is open, and
  *to* is ignored. gff-hab picks *token*; every replay data and the replay end carry it back.
- The holder answers with zero or more replay data frames in offset order, then one replay end.
  Replay end says the holder sent exactly [start, end).
  - *start* is from, or the oldest kept offset if from is older. Then *gap* is true: bytes in
    [from, start) left the ring (spec 6.4).
  - *end* is to, or the pane's count if that is smaller, or the count for an open replay.
  - A from beyond the count gives start = end = count.
  - An unknown handle gives an error with ref 0x010b and code 2, and no replay end.
- **Live output after a handshake.** After welcome the holder sends no output on its own. An open
  replay subscribes the pane: after its replay end, live output follows from *end* with no gap and
  no overlap. A pane spawned on this connection is subscribed from offset 0 at once.
- **Bounded replays** do not change the live stream. Their replay data may arrive between live
  output frames; the token and the frame kind tell them apart.
- The holder never discards ring bytes at handover. Only the ring size limits how far back a
  replay reaches.

## 7. Handover, crash and drain

**Planned restart (handover).**

1. gff-hab saves each screen checkpoint with the byte offset it covers.
2. gff-hab sends handover and closes the connection.
3. The holder records "handover". It keeps every pane and every ring byte, and keeps pumping.
4. The next gff-hab sends hello. welcome says previous = 1 and lists every pane with its count
   and oldest kept offset.
5. For each pane, gff-hab loads its checkpoint and sends an open replay from the checkpoint's
   offset. It parses the replay into the screen, then follows live output.

**Crash.** The connection ends without handover, the holder drops a hung connection (spec 2.6),
or a newer hello arrives while a connection is open. The holder records "crash"; the next welcome
says previous = 2. Recovery is the same as step 5. The only difference is how fresh the
checkpoint is. The holder keeps no state about gff-hab's saves.

**Drain (upgrade).**

1. A new gff-hab build connects to the old holder H1 at the highest common version, and to the
   new holder H2 at the current version.
2. gff-hab sends drain to H1. H1 refuses new spawns with spawn failed, code 3, and keeps serving
   its panes. Later welcomes from H1 say draining.
3. New panes go to H2. gff-hab serves both connections, each at its own version.
4. When H1's last pane is released, H1 sends bye and exits.

**Shutdown.** gff-hab sends shutdown. The holder ends every pane (spec 2.8), sends bye and
closes.

## 8. How versions evolve

- **Every wire change takes a new version number.** A peer at version N expects every field of
  version N, so a field added without a bump would fail as a short payload.
- **A new version may:**
  - add a frame kind, with a new kind number. Older peers never see it: a sender does not send a
    kind newer than the negotiated version.
  - append fields to the end of a payload or a record. At the old version the sender leaves
    them out. An old receiver that gets extra bytes ignores them.
- **A new version must not:** reorder, resize or remove an existing field; change the meaning of
  a field or a code; reuse a kind number; change the 10-byte header; change the handshake layout
  except by appending.
- **Keeping old versions.** gff-hab speaks every version a live holder on the host may still use
  (spec 3.2). A drained holder can live for weeks, so a version leaves gff-hab only when no
  holder of that version can remain.
- **In Go.** Each kind is registered with the first version that carries it. A frame type encodes
  by the version it is given, and its decoder reads the fields that exist at that version. A
  frame type that gains fields at version N checks `v >= N` on both paths.
- **Kinds 0x7f00 to 0x7fff** are reserved for tests.

## 9. Tests and fixtures

- `internal/holderproto/testdata/v1/` holds one golden file per version 1 frame kind. Each file
  is hex: the 10-byte header on the first line, then the payload 32 bytes a line.
- The golden test encodes each sample frame and compares it with its file. It also decodes the
  file and compares the result with the sample. Either mismatch fails. A test also fails when a
  registered kind has no sample.
- `testdata/v2/` holds the same for a test-only version 2. It appends pixel sizes to resize and
  adds a stats kind (0x7f01). It exists to prove two versions work; it is not a real version.
- The cross-version tests run, over `net.Pipe`, a v1 client against a v2 holder, a v2 client
  against a v1 holder, and v2 against v2. A further test holds a v1 and a v2 connection open from
  one client at once.
- `go test ./internal/holderproto/ -update` rewrites the golden files. Only do that on purpose:
  a changed version 1 golden means version 1 broke.
- `FuzzDecodeFrame` feeds the decoder random bytes (spec 6.6).
