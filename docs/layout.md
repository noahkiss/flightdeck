# Repo layout

One Go module, `github.com/noahkiss/flightdeck`, at the repo root. Swift packages live under
`swift/`. The app project is `apple/`.

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
| `internal/service/` | Service install: systemd user units and launchd plists rendered from a config with no baked-in names (PATH and the locale captured at install), `Install` and `Uninstall` with the exact file list (`Files`) from the install record `<state>/install.json` (`manifest.go`), and the holder starters (one systemd template instance or one launch agent per holder, retired after a clean exit, swept at start when gone). On macOS each build runs from its own copy of the helper bundles under `<state>/builds` (`bundle.go`). Commands run through an injected runner. Pure Go. Spec: `docs/specs/hab-holder.md` 5 |
| `scripts/service-check.sh` | Core tests 2 and 13 on a real init system: install, 20 logic restarts with one holder each time, a reinstall of a newer build, the drained holder's job retired, uninstall, then a path list compared with the one from before the install. Safe on a developer's machine (own job prefix, temporary directory) |
| `scripts/macos-bundle.sh` | macOS release and dev bundles: `GFF Holder.app` and `GFF Hab.app`, signing, notarization, stapling and the verification rows. Spec: `docs/specs/macos-identity.md` 3.3, 5 |
| `scripts/ios-build-check.sh` | iOS build check on a Mac: `internal/merge` and its imports as a C archive for the device or the simulator, through a probe main package it writes under `build/` and removes after |
| `cmd/gff-hab-holder/` | The holder binary: flags and environment for the socket path and registry directory, then `internal/holder` |
| `internal/structure/` | gff-hab's shared structure (projects, sessions, order, pane links), its JSON state file and restart mapping, plus `FakeHolder` for tests. Pure Go. Spec: `docs/specs/hab-logic-api.md` 2.2 |
| `internal/control/` | gff-hab's control connection: the Unix socket, hello, per-connection seq, scoped events, backlog close, reconnect tokens, and the client the CLI uses. It hands a connection that starts with a data hello to the data streams, and serves `data.ticket`, `pane.input`, `chat.page` and the pane size methods through injected services; `pane.input` runs off the read loop, in one ordered lane per pane, and each `chat.page` on its own goroutine. Pure Go. Spec: `docs/specs/hab-logic-api.md` 3, 4.1, 4.2 |
| `internal/datastream/` | Data connections: frame codec, single-use tickets, one stream per connection (welcome or reject, snapshot, bytes frames with holder offsets, resize and exited frames), credit flow and the fresh snapshot for a client too far behind, terminal queries left out of every frame, the owner in the welcome and every resize frame, which clients are observers of a pane, a fresh snapshot on one client's streams for a redraw, and the client side. Pure Go. Spec: `docs/specs/hab-logic-api.md` 5 |
| `internal/habconn/` | The client side of one hab, shared by `gff` and (through its own SSH) the app: a `Transport` seam that opens one byte stream per connection (Unix socket, a command's stdio, the system `ssh` running `gff-hab connect`, with connection sharing on and a checked control directory), the control connection (hello, the seq and revision rules, liveness, reconnect with the token and backoff, a fresh snapshot on a new epoch, the device id pin) and one data stream per pane (ticket, snapshot, bytes, resume by offset, credit; `Next` or a `Frames` channel). `Start` returns at once for a hab that may be down and keeps trying. Merging several hosts is a layer above it. Pure Go. Spec: `docs/specs/gff-client.md` 11.7 |
| `internal/hosts/` | Every configured hab at once for gff: one `habconn.Conn` per host started offline (`habconn.Start`), a model per host (connecting, online, offline, failed, since when, why), every host's updates on one channel, `Reload` for a changed list (a host whose endpoints or pin changed gets a new connection), and gff's config file (`config.go`: `[[hosts]]` with label, endpoints and device id, the shape of a mesh peer entry). The `Source` seam takes the Part 3 peer list later. Pure Go. Spec: `docs/specs/gff-client.md` 4.3 |
| `internal/merge/` | The multi-host merge of the shared client library: one model of every host (projects matched by remote across hosts with one location per host, sessions and unfiled panes tagged with their host, offline hosts stale with input off until a fresh snapshot) and a `Merger` that turns a `hosts.Set`'s updates into one ordered stream keyed by host and object id, with a resync per host. Pure Go; builds for iOS (`scripts/ios-build-check.sh`). Spec: `docs/specs/mesh.md` 4.5 |
| `internal/board/` | The rows gff's sidebar and status board show: one per session and unfiled pane on every host, ranked by need (`app.md` 3.1), an order where a row moves only when its rank changes, groups by project or host, the needs-you count, the filter. Pure Go. Spec: `docs/specs/gff-client.md` 4.3 |
| `internal/fakehab/` | A hab for tests of many-host clients: the real control server and structure store over a fake holder, agent states the test sets, session actions, stop and start with a new epoch. No data streams |
| `internal/gffclient/` | The gff client core: one or more panes of one hab in the host terminal. Pass-through for one pane that fits, else each pane's parser composed into the host screen (only changed cells written); the pass-through filter that keeps queries and host-wide settings from the host terminal; the prefix key reader; input as `pane.input` per pane, in order; desired sizes at open and after a 300 ms debounce; observer views; redraw, nudge and fit. The workspace (`workspace.go`): every host through `internal/hosts`, tabs whose hidden views close their streams and resume from their offsets, the tab bar, the sidebar (open, start, rename, close, filter, grouping), the status board and the bottom bar. The parser is injected. Pure Go. Spec: `docs/specs/gff-client.md` 3.4, 4.3 |
| `internal/panesize/` | Pane size ownership: one owner per pane, typing takes it after a debounce, take, request and disown, each client's desired sizes, the owner's size to the PTY at most every 50 ms, disconnect and restore within the grace period, observers kept out, and the redraw nudge. Time comes from an injected clock. Pure Go. Spec: `docs/specs/hab-logic-api.md` 6 |
| `internal/termq/` | Terminal queries: a small scanner that splits output into text and queries from any frame boundary, and the answers built from the parser's state. Pure Go. Spec: `docs/specs/hab-logic-api.md` 5.6, 5.8 |
| `internal/cmdmarks/` | Commands from OSC 133 prompt marks: a scanner that finds the four marks from any read split, with a bounded payload, and the per-pane tracker that keeps at most 1000 commands (offsets, state, exit status, times), saved with the checkpoint. Pure Go. Spec: `docs/specs/hab-logic-api.md` 2.5 |
| `internal/detect/` | Harness detection: herdr's manifests as data, our own process and screen matcher, the catalogue refresh, and the tested-version check. Pure Go. Spec: `docs/specs/harness-adapters.md`, Detection |
| `internal/detect/manifests/` | herdr's detection manifests, vendored unchanged, with `LICENSE` (Apache-2.0) and `SOURCE` (the pinned commit). Embedded in gff-hab |
| `internal/harness/` | Agent state: maps Claude Code, Codex and opencode hook events and OSC 7501 reports to states, applies precedence and ordering per pane, keeps the last state for snapshots, emits `agent.state`. Also the input rules (send and permission-answer checks, the paste and key bytes) and the gate of the blocking permission hook. Hand-written fixtures in `testdata/`. Pure Go. Spec: `docs/specs/harness-adapters.md`, State event model, Input delivery, Permission answers |
| `internal/progstatus/` | Program status reports (OSC 7501): the report parser and a scanner that finds reports in a pane's output, cut anywhere. Pure Go. Spec: `docs/specs/harness-adapters.md`, Claude Code |
| `internal/hook/` | The hook endpoint: HTTP on a local Unix socket, the pane tokens (a keyed hash of the pane uuid under `<state>/hook.key`), the `gff-hab hook` client, the channel route `POST /channel/<token>` with its client (`hook.Call`), and the printed hook configs. Pure Go. Spec: `docs/specs/harness-adapters.md`, Hook endpoint |
| `internal/transcript/` | The transcript reader for the chat view: tails a Claude Code session JSONL in place (complete lines only), maps records to chat messages with stable ids, shows unknown records as placeholders, reports version drift, and serves pages and byte-offset cursors. Hand-written fixtures in `testdata/`. Pure Go. Spec: `docs/specs/harness-adapters.md`, Claude Code |
| `internal/instructions/` | The instruction renderer: the `[instructions]` config table (sources by scope, host, harness and order; the opt-in `write` list), import expansion matched to Claude Code (fences, depth cap, cycles, one parent, missing and too-large files), the digest and header, one global target per harness with the user-loader and git work tree checks, the dry-run report and the atomic write with its state record under `<state>/instructions/`. Paths come from an `Env`, so tests use temporary directories only. Pure Go. Spec: `docs/specs/instructions-mcp-flight-rules.md` 5, 6 |
| `internal/gitstatus/` | Each project's repository state (branch, head, ahead, behind): read-only git runs with a timeout and a clean environment, and a watcher that reads on request (debounced), on a slow timer and when HEAD or the refs change. Pure Go. Spec: `docs/specs/hab-logic-api.md` 3.9 |
| `internal/peers/` | The hab's peer list and links (mesh.md 3, 4): the peer file (`$GFF_HAB_PEERS`, else `gff/peers.toml`; `[[peers]]` entries parsed by `internal/hosts`), the comment-keeping pin edit, and a `Manager` over `hosts.Set` that dials each peer as client kind `hab` with no subscription, skips this hab's own entries, pins on first link, refuses a changed id until `Accept`, tracks links other habs open to this one, and serves `peers.list`, `peers.accept` and `peer.updated`. Pure Go. Spec: `docs/specs/mesh.md` 3, 4 |
| `internal/catalog/` | The hab's catalog (mesh.md 5): detected facts (system, toolchains, harnesses), declared facts from the `[catalog]` section of the hab's config file, projects scanned from the project roots and the structure's folders, running agents, one saved generation counter, the peers' copies in `<state>/catalog.json`, the pull and put rounds over each peer link, the merged view with projects matched by `merge.ProjectKey`, and the always-on alert. Pure Go. Spec: `docs/specs/mesh.md` 5, 8 |
| `internal/channels/` | Channels (mesh.md 6): `channel.send` with address resolution (`label/name` through the peer list, a bare name through this hab's sessions and the catalog copies), one outbox per target device id, the inbox and the ledger of sent records and received ids under `<state>/channels/`, `mail.push` and `mail.pull` rounds over each peer link with backoff, dedup by message id, delivery when the agent may take it (through an injected `Local`), the envelope, status items back to the sender, and `channel.wait`. Pure Go. Spec: `docs/specs/mesh.md` 6.5 |
| `internal/habkey/` | The hab's own SSH host key and its fingerprint, the device id. Pure Go. Spec: `docs/specs/transport.md` 5 |
| `internal/secretfile/` | Reads and creates owner-only secret files (no symlink, owner and mode checked on the open file, atomic create); used by `hook.key`, the host key and the APNs key. Pure Go |
| `internal/push/` | Push straight to APNs with the user's own key: the push config, the ES256 provider token, the HTTP/2 client, the owner-only token store, banner words fitted to the Watch budgets, the payloads, and the notifier that turns agent state into banners and Live Activity updates under the presence tiers. Tests use an HTTP/2 fake APNs. Pure Go. Spec: `docs/specs/app.md` 8, `docs/specs/hab-logic-api.md` 3.9, Push; setup: `docs/push.md` |
| `internal/link/` | gff-hab's holder link: one parsed screen per pane on every holder, checkpoints in `<state>/checkpoints`, rebuild after a planned restart or a crash, panes marked lost when a holder dies, a holder started through the injected `Start` when none of this build is connected and `Retired` told when one says bye, ended panes released on a draining holder, panes released when a client closes them (`pane.close`), a subscriber API (output with absolute offsets, a snapshot, resizes tagged with the size owner from `SetOwner`), one ordered input queue per pane (`Input`, `Resize`), the answers to terminal queries in live output, bounded per pane, and each pane's commands from OSC 133 marks (`commands.go`: fed from the byte path, trimmed with the ring, a command's bytes by a bounded replay). The parser is injected, so it is pure Go. Spec: `docs/specs/hab-logic-api.md` 2.3 |
| `cmd/gff-hab/` | The logic binary: `serve`, `connect`, `api call`, `events`, `manifests`, `hook`, `instructions` (`instructions.go`), `install`, `uninstall` (`install.go`; `serve -init` picks the holder starter). `serve` runs the holder link; the parser is libghostty with `-tags ghostty`, else `vt.Fake` (`parser_*.go`), and the log says which. `spawn.go` starts panes through the link with `GFF_HAB` in their environment; `paneenv.go` builds the environment every pane starts from (TERM, COLORTERM, a UTF-8 locale, no init system job variables). `actions.go` serves `session.create` (spawn, then file the session) and `pane.close` (release through the link). `data.go` connects the data streams, `pane.input` and the pane size manager to the link. `commands.go` serves `command.list` and `command.output` and sends `command.updated`; `activity.go` sends `pane.output` and `pane.bell` at most once a second per pane, from its own link subscription per pane; `testdata/marks` holds the test shells' mark snippets |
| `internal/speech/` | gff-hab's side of the hab speech engine: the `[speech]` settings, the memory default and the helper lookup (`config.go`), the model download with its checksum (`model.go`), the helper process, one decode at a time in arrival order and the idle exit (`engine.go`, `helper.go`), and the session vocabulary fix-up (`vocab.go`). `speechtest/` is the fake helper the tests run. Pure Go. Spec: `docs/specs/voice.md`, Hab speech engine |
| `internal/speechproto/` | The gff-hab to speech helper wire protocol: frames over the helper's stdio. Pure Go. Spec: `docs/protocol/speech-helper.md` |
| `internal/oggopus/` | Reads an Ogg Opus file (RFC 7845) into its packets: pages, CRCs, the Opus header, the length from the granule position. No audio decoding. Pure Go |
| `cmd/gff-hab-speech/` | The speech helper: sherpa-onnx's C API and libopus through cgo. Builds only with the `speech` tag (`make speech-helper`) |
| `internal/vt/` | The terminal parser interface gff-hab and gff keep per pane (screen, snapshots, the cursor, modes and keyboard flags that answer queries, and the screen as cells for gff's composed view), and `Fake`, a pure-Go stand-in for tests |
| `cmd/gff/` | The terminal client: flags, the workspace's hosts (config file and `-host`), the single-hab transport (local socket, `-via`, `-ssh`), new panes through `session.create`, raw mode and SIGWINCH (`tty*.go`), then `internal/gffclient`. The parser is libghostty with `-tags ghostty`, else `vt.Fake` (`parser_*.go`) |
| `internal/vt/ghostty/` | The libghostty-vt parser through cgo. Builds only with the `ghostty` build tag |
| `internal/vt/vttest/` | The shared parser tests and their hand-written fixture |
| `build/` | Ignored by git. `make libghostty` puts the Ghostty source and the built library here |
| `swift/HabAPI/` | Swift package of generated `Codable` types for the app |
| `swift/Spikes/` | Throwaway device spikes, each an XcodeGen project with its own README. `ParakeetSpike/` measures on-device Parakeet against Apple SpeechAnalyzer (`docs/specs/voice.md`). `SSHSpike/` is a SwiftPM command-line tool instead: `swift-nio-ssh` with a Secure Enclave key against a throwaway sshd and `gff-hab connect` (`docs/specs/transport.md` 4.8) |
| `apple/` | The Flightdeck app: an XcodeGen spec (`project.yml`) with a macOS target and an iOS target (iPhone and iPad). The generated `.xcodeproj` is ignored by git |
| `apple/Config/` | `Names.xcconfig` (display name, bundle ID, App Group, version), `Base.xcconfig`, and `Local.example.xcconfig`. Signing goes in `Local.xcconfig`, which is ignored by git; with none, builds sign ad hoc |
| `apple/App/` | Each platform's entry point, `Info.plist` and entitlements. iOS: push, the App Group and time-sensitive notifications. macOS: no app sandbox (Developer ID, unsandboxed), so the app can reach the hab's local socket and run `ssh` |
| `apple/FlightdeckKit/` | The app's shared Swift package: everything that is not AppKit or UIKit. Depends on `swift/HabAPI`. Holds the hab transports (`Transport/`), the in-app SSH carrier with device keys, imported OpenSSH keys (`SSH/KeyImport/`), hab entries and pairing codes (`SSH/`), the JSON-RPC client (`RPC/`), the connection per hab (`Connection/`), the merged status board and lifecycle (`Board/`), the hab list config (`Config/`), the fake hab and demo mode (`Demo/`), the terminal view with its data-stream client (`Terminal/`), push registration, approvals, focus reports and the Live Activity tracker (`Push/`), usage readings, warnings and their views (`Usage/`) and the SwiftUI views (`Views/`). Also depends on libghostty-spm and swift-nio-ssh, both pinned exactly, and swift-nio. A second library, `FlightdeckShared`, holds what the extensions share with the app (push payload, expiry text, Live Activity attributes, request links, usage readings and the widget snapshot) and needs only Apple frameworks |
| `apple/NotificationService/`, `apple/Widgets/`, `apple/Shared/` | iOS extensions: the notification service extension (adds the expiry to permission banners) and the widgets (the Live Activity and the usage widget); `Shared/` holds the Live Activity intent, compiled into the app and the widget (app.md 8.7) |
| `apple/scripts/` | `generate.sh` (XcodeGen), `build.sh` (Mac Release build), `testflight.sh` and `notarize.sh` (release steps, from the app template), `ssh-live-check.sh` (the SSH carrier against a throwaway sshd), `gen-blowfish-tables.py` (Blowfish's tables from pi, for the key reader), `gen-key-fixtures.sh` (throwaway `ssh-keygen` keys for `KeyImportTests`) |
| `design/icons/AppIcon.icon` | The app icon (Icon Composer bundle); both app targets use it in place |
| `spikes/` | Throwaway host-side spikes (scripts, no Go module changes), each with its own README. `hab-parakeet/` measures Parakeet through the sherpa-onnx CLI on a hab host (`docs/specs/voice.md`). Models and binaries are downloaded outside the repo |
| `cmd/` | Binaries, as they land: `gff`, `gff-hab`, `gff-hab-holder`, `gff-hab-speech` |
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

gff-hab and gff parse pane output with libghostty-vt, Ghostty's terminal library, through cgo.
gff's release builds therefore need the libghostty build too (`gff-client.md` 15.1).

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
- **The app's copy.** The Flightdeck app does not use this build. It links libghostty-spm, a
  prebuilt Ghostty XCFramework for Apple platforms, pinned to one exact version in
  `apple/FlightdeckKit/Package.swift` (app.md 5.3).
- **Licences.** Ghostty and the bindings are MIT. `NOTICE` carries their notices for binaries.

## The speech helper (sherpa-onnx)

gff-hab transcribes speech through a separate helper, `gff-hab-speech`, because sherpa-onnx needs
cgo and gff-hab builds with `CGO_ENABLED=0` (`voice.md`, Hab speech engine).

- **Its own build tag.** Every file in `cmd/gff-hab-speech` carries the `speech` tag, the way
  `internal/vt/ghostty` carries `ghostty`. Without the tag the package has no files, so
  `make check-pure`, `make build`, `make vet` and `make test-pure` skip it. It stays in the one
  module: it adds nothing to `go.mod`, because it calls the C API directly, not sherpa-onnx's Go
  bindings.
- **Pinned, not vendored.** `make sherpa-onnx` downloads the static C library of sherpa-onnx 2.0.1
  for this platform (macOS arm64 and x86_64, Linux x86_64 and aarch64) and its `c-api.h` from the
  same tag into `build/sherpa-onnx`, checks both SHA-256 sums from the Makefile, and writes a
  pkg-config file for them. onnxruntime is linked into that library.
- **libopus** comes from the system: `pkg-config opus` (Homebrew `opus`, Debian `libopus-dev`).
- **Build and test.** `make speech-helper` writes `build/gff-hab-speech`. `make test-speech` runs
  its tests; set `$GFF_SPEECH_MODEL_DIR` to the extracted model archive to include real
  recognition. gff-hab finds the helper as `voice.md`, Built choices says.
- **The model** is never in the repo or a release: gff-hab downloads it on first use into its
  state folder.
- **Licences.** sherpa-onnx Apache-2.0, onnxruntime MIT, libopus BSD-3-Clause, and the libraries
  in sherpa-onnx's static release; `NOTICE` lists them. The Parakeet model is CC-BY-4.0, with the
  attribution line in the download log and in `ATTRIBUTION.txt` beside the model files.
