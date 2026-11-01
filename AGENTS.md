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

The repo layout and the libghostty build are in `docs/layout.md`.

| Command | What it does |
|---|---|
| `make gen` | Regenerate the API schema, Go client and Swift package from `api/` |
| `make libghostty` | Fetch the pinned Ghostty commit and build libghostty-vt into `build/` (needs zig) |
| `make test` | Build libghostty, then run every Go test with `-tags ghostty` |
| `make build`, `make vet` | Build or vet every package with `-tags ghostty` |
| `make test-pure` | Run the Go tests without cgo or libghostty |
| `make race` | Run the holder, holderproto, holderclient, holderreg, structure, link, control, datastream, habconn, panesize, termq, cmdmarks, harness, hook, gitstatus, transcript, service and `cmd` tests with `-race -count=3` |
| `make fuzz [FUZZTIME=30s]` | Run each holderproto fuzz target for `FUZZTIME` in turn (nightly) |
| `make check-pure` | Fail when a package other than `internal/vt/ghostty` needs cgo |
| `make check-gen` | Fail when a generated file is stale |
| `go run ./cmd/gff-hab-holder [-socket path \| -dir dir]` | Run a holder; the socket directory defaults to `$GFF_HOLDER_DIR`, else `gff/holders` under `~/Library/Application Support` on macOS and under `$XDG_RUNTIME_DIR` elsewhere (no `XDG_RUNTIME_DIR`: it refuses to start) |
| `go test -race ./internal/holder/... ./cmd/...` | Holder end-to-end tests: real PTYs, a real Unix socket |
| `go run ./cmd/gff-hab serve [-socket path] [-hook-socket path] [-state dir] [-host-id id]` | Run gff-hab with an empty pane source. The hook endpoint listens on `hook.sock` beside the control socket (or `$GFF_HAB_HOOK_SOCKET`). Socket: `$GFF_HAB_SOCKET`, else `gff/hab.sock` under the user config directory on macOS and under `$XDG_RUNTIME_DIR` elsewhere. State: `$GFF_HAB_STATE`, else `gff/hab` under the user config directory on macOS and under `$XDG_STATE_HOME` (or `~/.local/state`) elsewhere. Claude Code transcripts for `chat.page` are read in place under `$CLAUDE_CONFIG_DIR`, else `~/.claude`. No TCP port |
| `gff-hab serve ... -init systemd\|launchd [-prefix p] [-unit-dir dir]` | What the installed logic job runs: gff-hab starts each holder as its own init system job and removes a launchd holder job after a clean exit, or at its next start when the holder is gone. `-init none` (the default, or `$GFF_HAB_INIT`) starts no holders |
| `gff-hab install [-prefix p] [-state dir] [-holders dir] [-socket path] [-unit-dir dir] [-hab-bin path] [-holder-bin path] [-dry-run]` | Copy both binaries to `<state>/bin`, write the systemd user units (Linux) or the launchd plist (macOS, refuses with no GUI login) with PATH and the locale, load and (re)start the logic job. Records what it writes in `<state>/install.json`; refuses a state directory another prefix installed into. Prefix: `$GFF_SERVICE_PREFIX`, else `flightdeck`. Prints `loginctl enable-linger` when lingering is off; never runs sudo |
| `gff-hab uninstall [same flags] [-yes]` | Ask before ending programs (`-yes` skips it), stop the logic job, shut down every holder job, remove what `<state>/install.json` lists and the holder plists and logs, then the directories install made, when empty |
| `go test ./internal/service/` | Unit and plist rendering (no user or host names, absolute paths, the locale), install and uninstall file sets with a fake init system, a state directory the user already had, another prefix refused, holder retire and the start sweep |
| `scripts/service-check.sh` | Core tests 2 and 13 on this machine's init system: install, 20 logic restarts (`GFF_SVC_LOOPS`), a reinstall of a newer build, the drained holder retired, uninstall; checks the child's pid, output and environment, one holder per restart, and a path list against the one from before the install. Uses its own job prefix and a temporary directory |
| `gff-hab connect [-socket path]` | Bridge stdin and stdout to the local socket, for `ssh <host> gff-hab connect` |
| `gff-hab api call [-socket path \| -via command] <method> [params-json]` | Say hello, send one request, print its result as JSON (`hab.hello` prints the welcome). Exit 3 on an API error |
| `gff-hab events [-socket path \| -via command] [-scope host\|projects:id,...\|sessions:id,...]` | Subscribe and print the welcome, the snapshot and every notification, one JSON line each. Exit 1 when the hab ends the connection with no `closing` or with a reason other than shutdown |
| `gff-hab manifests refresh [-state dir] [-from url\|path] [-dry-run]` | Pull herdr's detection catalogue over https (default `https://herdr.dev/agent-detection/index.toml`; a local index or directory works offline), print a diff per newer manifest, and write them to `manifests/` in the state directory. Never downgrades; the vendored set stays the fallback |
| `gff-hab manifests list [-state dir]` | Print the active detection manifest per agent and where it came from. Exit 1 when a manifest could not be used (drift) |
| `gff-hab hook [-t ms] <harness> [event]` | What each harness's hook config runs: posts the payload on stdin to the hook endpoint named by `$GFF_HAB`. Always exits 0, prints nothing, gives up after 900 ms |
| `gff-hab hook -print <harness> [-command name]` | Print the hook config for `claude` or `codex` (JSON) or `opencode` (a plugin file). Prints only; it writes no settings |
| `go test -race ./internal/harness/... ./internal/hook/...` | Hook fixtures per harness to the exact `agent.state` stream, precedence and ordering rules, the endpoint over a real socket into a control connection |
| `go test -race ./internal/gitstatus/` | Project git status with real git in temp folders (skips with no git): ahead and behind against a local bare upstream, a branch switch seen by the ref poll, a folder outside a repository, a hung git that times out, no event without a change |
| `go test -race ./internal/transcript/` | Claude Code transcript reader over hand-written fixtures (`testdata/claude-*.jsonl`, expected messages in `.chat.json`; `-update` rewrites them): each record type, unknown records as placeholders with a warning, version drift, a half-written last line, cursor resume, a rewritten file and its reset event, an idle read that allocates no buffer |
| `go test -race ./internal/cmdmarks/ && go test -race -run TestCommandMarks ./cmd/gff-hab` | OSC 133 commands: marks split byte by byte and at random, nested shells, fake, malformed and over-long marks, the bounds, the cost of a mark flood against plain text; then a real bash and zsh (skipped when absent) running three commands, each command's output read back from the ring, the list across a planned restart, a SIGKILL with output past the last checkpoint, and a rebuild from the ring alone |
| `go test ./internal/detect/...` | Detection tests: process and screen matching for Claude Code, Codex and opencode, refresh, version warnings |
| `go test -race ./internal/control/... ./cmd/gff-hab/...` | Control connection tests: seq and epoch rules, backlog close, the binary over a real socket and over `connect` |
| `go test ./internal/link/` | Holder link end-to-end tests: real holder, the link as its own process, restarted by plan and SIGKILL; `-tags ghostty` uses libghostty. `GFF_RESTART_LOOP=200` sets the restart count (nightly) |
| `go test -race ./internal/habconn/` | Connection library suite over the Unix socket, an in-process pipe, `SSHTransport` with a stand-in `ssh`, and the real `ssh` to localhost (`$GFF_TEST_SSH_HOST` overrides; skips without key-based `BatchMode` ssh): data drops resume from the held offset with no gap or repeat, a new epoch replaces the structure, a dead link reconnects after a transport reset, the ssh control directory checks |
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
