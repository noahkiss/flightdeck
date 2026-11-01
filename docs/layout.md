# Repo layout

One Go module, `github.com/noahkiss/flightdeck`, at the repo root. Swift packages live under
`swift/`. The app project (Part 6) will sit beside them.

| Path | What it holds |
|---|---|
| `api/` | The gff-hab API as Go types: the source of truth for the schema |
| `api/schema.json` | Generated OpenRPC document; gff-hab serves it from `rpc.discover` |
| `api/client_gen.go` | Generated Go word constants, event decoding and client stubs |
| `api/released/<revision>.json` | The schema as released for each revision; the compat test compares against it |
| `internal/apigen/` | Schema builder, Go and Swift generators, compat checker |
| `internal/holderproto/` | The gff-hab to holder wire protocol: frames, codec, version negotiation. Pure Go. Spec: `docs/protocol/holder.md` |
| `internal/holder/` | The holder core: panes in PTYs, byte rings, pump, reaper, input queues, and the protocol server with one writer queue per connection. Pure Go. Spec: `docs/specs/hab-holder.md` 2 |
| `internal/holderreg/` | The holder registry: one `<instance>.json` per live holder in the holder directory, atomic writes, the stale rule and pruning, and the trust checks the holder and gff-hab share (socket directory, socket file, peer uid). Pure Go. Spec: `docs/specs/hab-holder.md` 4.2 |
| `internal/holderclient/` | gff-hab's side of the holders: finds them through the registry, keeps one connection per holder at its own protocol version, checks each registry file before it trusts it, puts new panes on the newest holder of its build, drains the rest, reports where each pane lives. Starting a holder is an injected function (the init system's job, `internal/service`); with one set, `Upgrade` makes the holder directory first. Pure Go. Spec: `docs/specs/hab-holder.md` 4.3 |
| `internal/service/` | Service install: systemd user units and launchd plists rendered from a config with no baked-in names (PATH and the locale captured at install), `Install` and `Uninstall` with the exact file list (`Files`) from the install record `<state>/install.json` (`manifest.go`), and the holder starters (one systemd template instance or one launch agent per holder, retired after a clean exit, swept at start when gone). Commands run through an injected runner. Pure Go. Spec: `docs/specs/hab-holder.md` 5 |
| `scripts/service-check.sh` | Core tests 2 and 13 on a real init system: install, 20 logic restarts with one holder each time, a reinstall of a newer build, the drained holder's job retired, uninstall, then a path list compared with the one from before the install. Safe on a developer's machine (own job prefix, temporary directory) |
| `cmd/gff-hab-holder/` | The holder binary: flags and environment for the socket path and registry directory, then `internal/holder` |
| `internal/structure/` | gff-hab's shared structure (projects, sessions, order, pane links), its JSON state file and restart mapping, plus `FakeHolder` for tests. Pure Go. Spec: `docs/specs/hab-logic-api.md` 2.2 |
| `internal/control/` | gff-hab's control connection: the Unix socket, hello, per-connection seq, scoped events, backlog close, reconnect tokens, and the client the CLI uses. It hands a connection that starts with a data hello to the data streams, and serves `data.ticket`, `pane.input`, `chat.page` and the pane size methods through injected services; `pane.input` runs off the read loop, in one ordered lane per pane, and each `chat.page` on its own goroutine. Pure Go. Spec: `docs/specs/hab-logic-api.md` 3, 4.1, 4.2 |
| `internal/datastream/` | Data connections: frame codec, single-use tickets, one stream per connection (welcome or reject, snapshot, bytes frames with holder offsets, resize and exited frames), credit flow and the fresh snapshot for a client too far behind, terminal queries left out of every frame, the owner in the welcome and every resize frame, which clients are observers of a pane, a fresh snapshot on one client's streams for a redraw, and the client side. Pure Go. Spec: `docs/specs/hab-logic-api.md` 5 |
| `internal/habconn/` | The client side of one hab, shared by `gff` and (through its own SSH) the app: a `Transport` seam that opens one byte stream per connection (Unix socket, a command's stdio, the system `ssh` running `gff-hab connect`), the control connection (hello, the seq and revision rules, liveness, reconnect with the token and backoff, a fresh snapshot on a new epoch, the device id pin) and one data stream per pane (ticket, snapshot, bytes, resume by offset, credit). Merging several hosts is a layer above it. Pure Go. Spec: `docs/specs/gff-client.md` 11.7 |
| `internal/panesize/` | Pane size ownership: one owner per pane, typing takes it after a debounce, take, request and disown, each client's desired sizes, the owner's size to the PTY at most every 50 ms, disconnect and restore within the grace period, observers kept out, and the redraw nudge. Time comes from an injected clock. Pure Go. Spec: `docs/specs/hab-logic-api.md` 6 |
| `internal/termq/` | Terminal queries: a small scanner that splits output into text and queries from any frame boundary, and the answers built from the parser's state. Pure Go. Spec: `docs/specs/hab-logic-api.md` 5.6, 5.8 |
| `internal/cmdmarks/` | Commands from OSC 133 prompt marks: a scanner that finds the four marks from any read split, with a bounded payload, and the per-pane tracker that keeps at most 1000 commands (offsets, state, exit status, times), saved with the checkpoint. Pure Go. Spec: `docs/specs/hab-logic-api.md` 2.5 |
| `internal/detect/` | Harness detection: herdr's manifests as data, our own process and screen matcher, the catalogue refresh, and the tested-version check. Pure Go. Spec: `docs/specs/harness-adapters.md`, Detection |
| `internal/detect/manifests/` | herdr's detection manifests, vendored unchanged, with `LICENSE` (Apache-2.0) and `SOURCE` (the pinned commit). Embedded in gff-hab |
| `internal/harness/` | Agent state: maps Claude Code, Codex and opencode hook events to states, applies precedence and ordering per pane, keeps the last state for snapshots, emits `agent.state`. Hand-written fixtures in `testdata/`. Pure Go. Spec: `docs/specs/harness-adapters.md`, State event model |
| `internal/hook/` | The hook endpoint: HTTP on a local Unix socket, the pane tokens (a keyed hash of the pane uuid under `<state>/hook.key`), the `gff-hab hook` client and the printed hook configs. Pure Go. Spec: `docs/specs/harness-adapters.md`, Hook endpoint |
| `internal/transcript/` | The transcript reader for the chat view: tails a Claude Code session JSONL in place (complete lines only), maps records to chat messages with stable ids, shows unknown records as placeholders, reports version drift, and serves pages and byte-offset cursors. Hand-written fixtures in `testdata/`. Pure Go. Spec: `docs/specs/harness-adapters.md`, Claude Code |
| `internal/gitstatus/` | Each project's repository state (branch, head, ahead, behind): read-only git runs with a timeout and a clean environment, and a watcher that reads on request (debounced), on a slow timer and when HEAD or the refs change. Pure Go. Spec: `docs/specs/hab-logic-api.md` 3.9 |
| `internal/habkey/` | The hab's own SSH host key and its fingerprint, the device id. Pure Go. Spec: `docs/specs/transport.md` 5 |
| `internal/secretfile/` | Reads and creates owner-only secret files (no symlink, owner and mode checked on the open file, atomic create); used by `hook.key` and the host key. Pure Go |
| `internal/link/` | gff-hab's holder link: one parsed screen per pane on every holder, checkpoints in `<state>/checkpoints`, rebuild after a planned restart or a crash, panes marked lost when a holder dies, a holder started through the injected `Start` when none of this build is connected and `Retired` told when one says bye, ended panes released on a draining holder, panes released when a client closes them (`pane.close`), a subscriber API (output with absolute offsets, a snapshot, resizes tagged with the size owner from `SetOwner`), one ordered input queue per pane (`Input`, `Resize`), the answers to terminal queries in live output, bounded per pane, and each pane's commands from OSC 133 marks (`commands.go`: fed from the byte path, trimmed with the ring, a command's bytes by a bounded replay). The parser is injected, so it is pure Go. Spec: `docs/specs/hab-logic-api.md` 2.3 |
| `cmd/gff-hab/` | The logic binary: `serve`, `connect`, `api call`, `events`, `manifests`, `hook`, `install`, `uninstall` (`install.go`; `serve -init` picks the holder starter). `serve` runs the holder link; the parser is libghostty with `-tags ghostty`, else `vt.Fake` (`parser_*.go`), and the log says which. `spawn.go` starts panes through the link with `GFF_HAB` in their environment; `paneenv.go` builds the environment every pane starts from (TERM, COLORTERM, a UTF-8 locale, no init system job variables). `actions.go` serves `session.create` (spawn, then file the session) and `pane.close` (release through the link). `data.go` connects the data streams, `pane.input` and the pane size manager to the link. `commands.go` serves `command.list` and `command.output` and sends `command.updated`; `testdata/marks` holds the test shells' mark snippets |
| `internal/vt/` | The terminal parser interface gff-hab keeps per pane (screen, snapshots, and the cursor, modes and keyboard flags that answer queries), and `Fake`, a pure-Go stand-in for tests |
| `internal/vt/ghostty/` | The libghostty-vt parser through cgo. Builds only with the `ghostty` build tag |
| `internal/vt/vttest/` | The shared parser tests and their hand-written fixture |
| `build/` | Ignored by git. `make libghostty` puts the Ghostty source and the built library here |
| `swift/HabAPI/` | Swift package of generated `Codable` types for the app |
| `swift/Spikes/` | Throwaway device spikes, each an XcodeGen project with its own README. `ParakeetSpike/` measures on-device Parakeet against Apple SpeechAnalyzer (`docs/specs/voice.md`) |
| `cmd/` | Binaries, as they land: `gff`, `gff-hab`, `gff-hab-holder` |
| `docs/specs/` | Behaviour specs |
| `docs/protocol/` | Wire protocol documents |

Why one module: the binaries and the API ship together, and one `go.mod` keeps versions in step.
Split a module out only when something outside this repo must import it on its own schedule.

## The API schema

- Edit the Go types in `api/types.go` and the tables in `api/methods.go`. Never edit generated
  files.
- `make gen` (or `go generate ./...`) regenerates the schema, the Go client and the Swift package.
- `make test` runs the compat test. It fails when the schema breaks the released schema of the same
  revision (spec 3.3). A breaking change needs a new revision and its translator.
- `make release-schema` records the current schema in `api/released/`. Run it when a revision ships.
  Until the first release, `1.json` is a baseline that may be rewritten on purpose.
- Golden messages (`api/testdata/rev<N>/`, `docs/testing.md` core test 5) are frozen:
  - The goldens of a released revision are never regenerated.
  - New messages go into the next revision's set.
  - Revision 1 is still unreleased (`api/released/1.json` is the pre-release baseline). New
    revision 1 messages get new files beside the existing ones; existing files never change.
  - `go test ./api -run TestGoldens -update` only adds files: a new message's first file, or
    the next field set (`<message>@<n>.json`) of a message that gained optional fields. Only
    the newest field set must match the current sample; older ones must still decode and round
    trip. Any other change to an existing message needs the next revision. The full rule is in
    `docs/testing.md`.

Rules the generator follows:

- A pointer field, or a field tagged `omitempty`, is optional. Every other field is required.
- A named string type listed in `api.Enums` is a word set. A set the server sends is open:
  clients map unknown words to a fallback, so adding a word is compatible. A set the client sends
  is closed: removing a word, or turning a plain string into a word set, is breaking (spec 3.3).
- A required array must go out as `[]`, never `null`. Go marshals a nil slice as `null`, so the
  server must make every required slice non-nil.

## The terminal parser (libghostty)

gff-hab parses pane output with libghostty-vt, Ghostty's terminal library, through cgo.

- **Pinned, not vendored.** `go.mod` pins the Go bindings, `go.mitchellh.com/libghostty`, to one
  commit. The Makefile pins the Ghostty commit those bindings were built against
  (`GHOSTTY_COMMIT`). Bump both together. No Ghostty source is committed here.
- **Zig 0.17.0** builds libghostty-vt (pinned in `mise.toml`; the Ghostty commit's
  `minimum_zig_version`). `make libghostty` fetches the pinned commit into `build/ghostty-src` and
  runs `zig build -Demit-lib-vt` into `build/libghostty-vt`. The first build takes a few minutes;
  later builds are skipped by a stamp file. CMake is not needed.
- **The `ghostty` build tag.** Only `internal/vt/ghostty` uses cgo, and only with `-tags ghostty`.
  Without the tag, `go build ./...` and `go test ./...` skip that package and work on any machine,
  with or without the library. `make build`, `make vet` and `make test` build the library first,
  then pass the tag and `PKG_CONFIG_PATH`. Linking is static, so binaries need no shared library.
- **cgo stays in one package.** Every other package, the holder included, must build with
  `CGO_ENABLED=0`. `make check-pure` proves it.
- **Snapshot encodings.** `snapshot-vt` (the baseline, spec Q6) is libghostty's VT formatter output
  with scrollback and modes. `snapshot-ghostty` is libghostty's own snapshot format. The fake
  supports `snapshot-vt` only.
- **Golden files.** `go test ./internal/vt/... -update` rewrites `vttest/testdata/fixture.golden`.
  The fake and libghostty must produce the same dump.
- **Licences.** Ghostty and the bindings are MIT. `NOTICE` carries their notices for binaries.
