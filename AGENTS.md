# flightdeck

One personal workspace for coding agents, and later for the rest of a homelab and daily life.
Agents run on several machines. Flightdeck shows them all in one place: a native chat view, a
terminal view, approvals and status. It does not replace any agent harness. It starts, hosts,
observes and presents them.

The plan and its stages live in bm (project `flightdeck`). Start with the master plan:
`bm ls -t plan -p flightdeck`.

## Names

| Name | What it is |
|---|---|
| GFF (Go For Flight) | The system as a whole, and the `gff` terminal client (Go) |
| Flightdeck | The native iOS, iPadOS and macOS app (Swift, SwiftUI) |
| hab | The server on each host: `gff-hab` (logic, restarts any time) and `gff-hab-holder` (owns the PTYs, almost never restarts) |
| Capcom | The conversation that needs no project folder: it answers or routes anything |
| modules | Extensions: JS that returns native building blocks, rendered by the app and the Go client |

Use these plain words for the rest: console (a shell beside a session), approvals, flight rules
(the context ledger), status board, channels (agent messages), rounds (scheduled jobs),
online and offline, session. One word per concept; do not rotate synonyms.

## How we build it

- **New code, one architecture.** We study how the best tools behave (herdr, Rex, Moshi, the
  earlier GFF and the zellij fork). We write down the behaviour, then write our own code from
  that spec. Do not paste chunks of other projects' code.
- **Spec first.** Each part of the plan starts with a study task that writes a behaviour spec
  (what the reference tools do, edge cases, why). Implementation tasks work from the spec.
- **Harnesses.** Supported now: Claude Code, Codex, opencode. pi comes later; others after that.
  Agent detection comes from herdr's detection manifests, used as data and refreshed on a
  schedule. Each harness gets a thin adapter of our own: state events from hooks, a transcript
  reader for the chat view, input through the PTY. Each adapter ships fixture transcripts.
- **Native data stays native.** Transcripts stay where each harness writes them. Resuming a
  session from a plain terminal must always work.
- **Sessions outlive updates.** Restarting `gff-hab` never kills a running program.
- **Many machines from day one.** The repo builds and runs on macOS and Linux. Nothing assumes
  one host.

## Build and test

Local builds and tests are fine on any of the owner's machines for now. CI arrives later
(shared workflows from `noahkiss/workflows`; macOS jobs run when Apple code or holder packages
change, plus a nightly macOS end-to-end run). Which tests we keep, and why, is in
`docs/testing.md`.

- Go code (hab, `gff`, modules runtime) is developed mostly on the Linux dev box.
- Swift and SwiftUI work happens on a Mac, with Xcode and the simulator.
- Signing and release builds will move to CI once it exists.

Go is pinned in `.go-version` and Zig in `mise.toml` (mise). The repo layout and the libghostty
build are in `docs/layout.md`.

| Command | What it does |
|---|---|
| `make gen` | Regenerate the API schema, Go client and Swift package from `api/` |
| `make libghostty` | Fetch the pinned Ghostty commit and build libghostty-vt into `build/` (needs zig) |
| `make test` | Build libghostty, then run every Go test with `-tags ghostty` |
| `make build`, `make vet` | Build or vet every package with `-tags ghostty` |
| `make test-pure` | Run the Go tests without cgo or libghostty |
| `make race` | Run the holder, holderproto, holderclient, holderreg, structure and `cmd` tests with `-race -count=3` |
| `make fuzz [FUZZTIME=30s]` | Run each holderproto fuzz target for `FUZZTIME` in turn (nightly) |
| `make check-pure` | Fail when a package other than `internal/vt/ghostty` needs cgo |
| `make check-gen` | Fail when a generated file is stale |
| `go run ./cmd/gff-hab-holder [-socket path \| -dir dir]` | Run a holder; the socket directory defaults to `$GFF_HOLDER_DIR`, else `gff/holders` under `~/Library/Application Support` on macOS and under `$XDG_RUNTIME_DIR` elsewhere (no `XDG_RUNTIME_DIR`: it refuses to start) |
| `go test -race ./internal/holder/... ./cmd/...` | Holder end-to-end tests: real PTYs, a real Unix socket |
| `go run ./cmd/gff-hab serve [-socket path] [-state dir] [-host-id id]` | Run gff-hab with an empty pane source. Socket: `$GFF_HAB_SOCKET`, else `gff/hab.sock` under the user config directory on macOS and under `$XDG_RUNTIME_DIR` elsewhere. State: `$GFF_HAB_STATE`, else `gff/hab` under the user config directory on macOS and under `$XDG_STATE_HOME` (or `~/.local/state`) elsewhere. No TCP port |
| `gff-hab connect [-socket path]` | Bridge stdin and stdout to the local socket, for `ssh <host> gff-hab connect` |
| `gff-hab api call [-socket path \| -via command] <method> [params-json]` | Say hello, send one request, print its result as JSON (`hab.hello` prints the welcome). Exit 3 on an API error |
| `gff-hab events [-socket path \| -via command] [-scope host\|projects:id,...\|sessions:id,...]` | Subscribe and print the welcome, the snapshot and every notification, one JSON line each. Exit 1 when the hab ends the connection with no `closing` or with a reason other than shutdown |
| `gff-hab manifests refresh [-state dir] [-from url\|path] [-dry-run]` | Pull herdr's detection catalogue (default `https://herdr.dev/agent-detection/index.toml`; a local index or directory works offline), print a diff per newer manifest, and write them to `manifests/` in the state directory. Never downgrades; the vendored set stays the fallback |
| `gff-hab manifests list [-state dir]` | Print the active detection manifest per agent and where it came from. Exit 1 when a manifest could not be used (drift) |
| `go test ./internal/detect/...` | Detection tests: process and screen matching for Claude Code, Codex and opencode, refresh, version warnings |
| `go test -race ./internal/control/... ./cmd/gff-hab/...` | Control connection tests: seq and epoch rules, backlog close, the binary over a real socket and over `connect` |
| `go test ./internal/link/` | Holder link end-to-end tests: real holder, the link as its own process, restarted by plan and SIGKILL; `-tags ghostty` uses libghostty. `GFF_RESTART_LOOP=200` sets the restart count (nightly) |
| `cd swift/HabAPI && swift test` | Build and test the generated Swift package (Mac) |
| `go test ./api -run TestGoldens -update` | Rewrite the golden messages of the current API revision in `api/testdata/` |

## Delegation

- Split work into tasks one subagent can finish and verify: one outcome, clear acceptance.
- Use Opus subagents for code and research. Use Fable only for copywriting (README, site,
  store text), and sparingly.
- Run `/consensus` on key decisions with no ground truth.

## Hygiene: the repo stays publishable

- The repo is public. Nothing homelab-specific goes in it: no real hostnames, IPs, tailnet
  names, usernames, home paths, or vault and item names. This covers code, docs, examples,
  commit messages and CI.
- Configuration comes from the environment or a config file. No baked-in default names a real
  machine.
- Test fixtures are written by hand or scrubbed. Never commit a real transcript.
- No agent footers in commits or PRs.
