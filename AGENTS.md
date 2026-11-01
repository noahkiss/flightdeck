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
| `make race` | Run the holder, holderproto, holderclient, holderreg, structure, link, control, datastream, habconn, hosts, board, gffclient, panesize, termq, cmdmarks, harness, hook, gitstatus, transcript, service, grants and `cmd` tests with `-race -count=3` |
| `make fuzz [FUZZTIME=30s]` | Run each holderproto fuzz target for `FUZZTIME` in turn (nightly) |
| `make check-pure` | Fail when a package other than `internal/vt/ghostty` needs cgo |
| `make check-gen` | Fail when a generated file is stale |
| `go run ./cmd/gff-hab-holder [-socket path \| -dir dir]` | Run a holder; the socket directory defaults to `$GFF_HOLDER_DIR`, else `gff/holders` under `~/Library/Application Support` on macOS and under `$XDG_RUNTIME_DIR` elsewhere (no `XDG_RUNTIME_DIR`: it refuses to start) |
| `go test -race ./internal/holder/... ./cmd/...` | Holder end-to-end tests: real PTYs, a real Unix socket |
| `go run ./cmd/gff-hab serve [-socket path] [-hook-socket path] [-state dir] [-host-id id] [-permission-wait d]` | Run gff-hab with an empty pane source. `-permission-wait` (or `$GFF_HAB_PERMISSION_WAIT`) turns on the blocking permission hook: Claude's `PermissionRequest` waits up to `d` for the app's answer; 0, the default, is off. The hook endpoint listens on `hook.sock` beside the control socket (or `$GFF_HAB_HOOK_SOCKET`). Socket: `$GFF_HAB_SOCKET`, else `gff/hab.sock` under the user config directory on macOS and under `$XDG_RUNTIME_DIR` elsewhere. State: `$GFF_HAB_STATE`, else `gff/hab` under the user config directory on macOS and under `$XDG_STATE_HOME` (or `~/.local/state`) elsewhere. Claude Code transcripts for `chat.page` are read in place under `$CLAUDE_CONFIG_DIR`, else `~/.claude`. No TCP port |
| `gff-hab serve ... -init systemd\|launchd [-prefix p] [-unit-dir dir] [-build id]` | What the installed logic job runs: gff-hab starts each holder as its own init system job and removes a launchd holder job after a clean exit, or at its next start when the holder is gone, then each build directory no job names. With launchd, `-build` names the build directory whose `GFF Holder.app` its holders run from. `-init none` (the default, or `$GFF_HAB_INIT`) starts no holders |
| `gff-hab install [-prefix p] [-state dir] [-holders dir] [-socket path] [-unit-dir dir] [-hab-bin path] [-holder-bin path] [-dry-run]` | Linux: copy both binaries to `<state>/bin`. macOS: take the two helper bundles (`GFF Hab.app`, `GFF Holder.app` beside it), copy them with `ditto` into `<state>/builds/<hash of both>` through a temporary directory, verify each copy with `codesign`, rename it into place, point `<state>/current` at it, and remove each older build no job names. Then write the systemd user units (Linux) or the launchd plist (macOS, refuses with no GUI login) with PATH, the locale and `$GFF_MACOS_GRANTS` (when unset, the list the last install recorded; it warns on an unknown name), load and (re)start the logic job. Records what it writes in `<state>/install.json`; refuses a state directory another prefix installed into. Prefix: `$GFF_SERVICE_PREFIX`, else `flightdeck`. Prints `loginctl enable-linger` when lingering is off; never runs sudo |
| `gff-hab uninstall [same flags] [-yes]` | Ask before ending programs (`-yes` skips it), stop the logic job, shut down every holder job, remove what `<state>/install.json` lists and the holder plists and logs, then the directories install made, when empty |
| `go test ./internal/service/` | Unit and plist rendering (no user or host names, absolute paths, the locale, bundle paths and `AssociatedBundleIdentifiers`), install and uninstall file sets with a fake init system, a state directory the user already had, another prefix refused, holder retire and the start sweep, per-build bundle copies (never rewritten, kept while a holder job names them, removed after), a copy that fails `codesign` |
| `scripts/service-check.sh` | Core tests 2 and 13 on this machine's init system: install, 20 logic restarts (`GFF_SVC_LOOPS`), a reinstall of a newer build, the drained holder retired, uninstall; checks the child's pid, output and environment, one holder per restart, and a path list against the one from before the install. Uses its own job prefix and a temporary directory. On macOS it installs dev bundles from `scripts/macos-bundle.sh -dev` and checks that the drained build's bundles go |
| `scripts/macos-bundle.sh [-dev] [-bin-dir dir] [-build id] [-version x.y.z] [-no-notarize] -o out` | macOS: wrap gff-hab and gff-hab-holder into `GFF Hab.app` and `GFF Holder.app` (bundle ids `$GFF_BUNDLE_PREFIX.hab` and `.hab-holder`, usage strings, holder entitlements), sign inside out with hardened runtime, notarize one zip, staple, and run every check of `docs/specs/macos-identity.md` 5.3. Release: `$GFF_SIGN_IDENTITY`, `$GFF_TEAM_ID`, `$GFF_NOTARY_PROFILE`, optional `$GFF_SIGN_KEYCHAIN` and `$GFF_NOTARY_KEYCHAIN`. `-dev`: ad-hoc (or `$GFF_DEV_SIGN_IDENTITY`), `.dev` ids, no notarization |
| `gff-hab connect [-socket path]` | Bridge stdin and stdout to the local socket, for `ssh <host> gff-hab connect` |
| `gff-hab api call [-socket path \| -via command] <method> [params-json]` | Say hello, send one request, print its result as JSON (`hab.hello` prints the welcome). Exit 3 on an API error |
| `gff-hab events [-socket path \| -via command] [-scope host\|projects:id,...\|sessions:id,...]` | Subscribe and print the welcome, the snapshot and every notification, one JSON line each. Exit 1 when the hab ends the connection with no `closing` or with a reason other than shutdown |
| `gff-hab manifests refresh [-state dir] [-from url\|path] [-dry-run]` | Pull herdr's detection catalogue over https (default `https://herdr.dev/agent-detection/index.toml`; a local index or directory works offline), print a diff per newer manifest, and write them to `manifests/` in the state directory. Never downgrades; the vendored set stays the fallback |
| `gff-hab manifests list [-state dir]` | Print the active detection manifest per agent and where it came from. Exit 1 when a manifest could not be used (drift) |
| `gff-hab hook [-t ms] [-wait] <harness> [event]` | What each harness's hook config runs: posts the payload on stdin to the hook endpoint named by `$GFF_HAB`. Always exits 0, prints nothing, gives up after 900 ms. `-wait` (Claude `PermissionRequest` only): when gff-hab holds the hook, waits for the app's answer and prints it as Claude's hook output |
| `gff-hab hook -print <harness> [-command name]` | Print the hook config for `claude` or `codex` (JSON) or `opencode` (a plugin file). Prints only; it writes no settings |
| `go test -race ./internal/harness/... ./internal/hook/...` | Hook fixtures per harness to the exact `agent.state` stream, OSC 7501 and hook fixtures with each change's source, precedence and ordering rules, the send and answer checks, the endpoint over a real socket into a control connection |
| `go test ./internal/progstatus/` | OSC 7501 report parsing over hand-written byte fixtures cut at every chunk size; `-fuzz FuzzScanner` for a fuzz run |
| `go test -race -run 'TestClaudeAdapter\|TestBlockingPermissionHook' ./cmd/gff-hab/` | The Claude Code adapter end to end with the real binaries: OSC 7501 state, `session.send` refused while working and delivered as paste plus Enter, `approval.answer` by keys and through the blocking hook |
| `go test -race ./internal/gitstatus/` | Project git status with real git in temp folders (skips with no git): ahead and behind against a local bare upstream, a branch switch seen by the ref poll, a folder outside a repository, a hung git that times out, no event without a change |
| `go test -race ./internal/transcript/` | Claude Code transcript reader over hand-written fixtures (`testdata/claude-*.jsonl`, expected messages in `.chat.json`; `-update` rewrites them): each record type, unknown records as placeholders with a warning, version drift, a half-written last line, cursor resume, a rewritten file and its reset event, an idle read that allocates no buffer |
| `go test -race ./internal/cmdmarks/ && go test -race -run TestCommandMarks ./cmd/gff-hab` | OSC 133 commands: marks split byte by byte and at random, nested shells, fake, malformed and over-long marks, the bounds, the cost of a mark flood against plain text; then a real bash and zsh (skipped when absent) running three commands, each command's output read back from the ring, the list across a planned restart, a SIGKILL with output past the last checkpoint, and a rebuild from the ring alone |
| `go test ./internal/detect/...` | Detection tests: process and screen matching for Claude Code, Codex and opencode, refresh, version warnings |
| `go test -race ./internal/control/... ./cmd/gff-hab/...` | Control connection tests: seq and epoch rules, backlog close, the binary over a real socket and over `connect` |
| `go test ./internal/link/` | Holder link end-to-end tests: real holder, the link as its own process, restarted by plan and SIGKILL; `-tags ghostty` uses libghostty. `GFF_RESTART_LOOP=200` sets the restart count (nightly) |
| `go test -race ./internal/habconn/` | Connection library suite over the Unix socket, an in-process pipe, `SSHTransport` with a stand-in `ssh`, and the real `ssh` to localhost (`$GFF_TEST_SSH_HOST` overrides; skips without key-based `BatchMode` ssh): data drops resume from the held offset with no gap or repeat, a new epoch replaces the structure, a dead link reconnects after a transport reset, the ssh control directory checks |
| `gff [-config path] [-host label=endpoint[,endpoint...]]...` | The workspace: every host at once (config file `-config`, else `$GFF_CONFIG`, else `gff/config.toml` in the user config directory; `[[hosts]]` with `label`, `endpoints`, `device_id`), tabs, a sidebar of every session on every host, a status board. An endpoint is a socket path, `cmd:<command>` or an SSH destination; with no host, the `-socket`/`-via`/`-ssh` hab. Prefix Ctrl+\ then `w` sidebar, `g` board, `c` tab, `]` `[` `1`-`9` tabs, `x` close view, `b` bars |
| `gff [-socket path \| -via command \| -ssh host] [-new command]... [-split columns\|rows] [-observe] [-prefix ctrl-<key>] pane...` | The single-hab view: show the named panes and one new pane per `-new` in this terminal. One pane that fits passes through; several share the screen. Prefix Ctrl+\ then `o` focus, `z` zoom, `s` split, `r` redraw, `n` nudge, `f` fit, `d` detach. Build with `-tags ghostty` for libghostty |
| `go test -race ./internal/hosts/ ./internal/board/` | Many hosts over in-process fake habs: one down at start, offline and back with a new epoch, the board's order across hosts, two labels to one hab, reload, the config file |
| `go test -race ./internal/gffclient/` | The workspace over fake habs (the sidebar recovers after a hab restarts, the board puts blocked above working, start, rename and close from the sidebar) and tabs over the real hab. Client core against the real holder and gff-hab: an observer never sends a size, typing moves ownership, one debounced size after a burst of host resizes, no query reaches the host or gets an answer, detach; then the `gff` binary in a pty with vim and a full-screen script side by side across a resize (skips without vim) |
| `gff setup [--check [--json]] [--host h] [--grant g]... [--timeout 5m] [--no-open]` | Check the permission grants of the hab's processes through `hab.permissions`, or walk the missing ones: open each Settings pane, wait, verify. `--check` exits 0 when all are granted, 1 when one is missing, 2 when the check could not run, 64 on a usage error. Opt-in grants come from `$GFF_MACOS_GRANTS` on gff-hab. The walk asks again for a Local Network or Files and Folders answer it remembered. Ctrl-C stops the walk; a second Ctrl-C ends it at once |
| `go test -race ./internal/grants/ ./cmd/gff/ ./cmd/gff-hab/` | Permission check: probe protocol, checks with a fake system, signature rows, kept walk answers, exit codes and output, the walk; a real probe pane on a real holder |
| `cd swift/HabAPI && swift test` | Build and test the generated Swift package (Mac) |
| `apple/scripts/generate.sh` | Generate `apple/Flightdeck.xcodeproj` from `apple/project.yml` with XcodeGen (the pinned release is downloaded when `xcodegen` is not on PATH). The project is not committed |
| `xcodebuild -project apple/Flightdeck.xcodeproj -scheme FlightdeckiOS -destination 'generic/platform=iOS Simulator' build` | Build the iOS app for the simulator (no signing setup needed) |
| `xcodebuild -project apple/Flightdeck.xcodeproj -scheme FlightdeckMac -destination 'generic/platform=macOS' build` | Build the Mac app; ad hoc signed unless `apple/Config/Local.xcconfig` names an identity |
| `apple/scripts/build.sh [--install]` | Generate, then build the Mac app (Release) into `apple/build/`, and verify its signature |
| `cd apple/FlightdeckKit && swift test` | Test the app's shared package (it links `swift/HabAPI`) |
| `FLIGHTDECK_LIVE_SOCKET=<sock> [FLIGHTDECK_LIVE_SSH_HOST=<host> FLIGHTDECK_LIVE_SSH_COMMAND=... FLIGHTDECK_LIVE_SSH_OPTIONS=...] swift test --filter LiveTests` (in `apple/FlightdeckKit`) | Live check against a real hab: hello, subscribe, ping and a clean close over the Unix socket, and over `ssh <host> gff-hab connect`. Skipped when unset |
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
