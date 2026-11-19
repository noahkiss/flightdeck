# Testing

Flightdeck keeps its tests slim. Each test guards a promise a user would notice. This note says
which tests we keep at each layer, what each must prove, and what we leave untested on purpose.
Every stage's acceptance checks follow it.

## Principles

- A test earns its place when a regression would kill a session, lose or repeat bytes, or break
  an older client or peer.
- Use real PTYs, sockets and processes for anything about survival. Fake only the far side of an
  interface we own.
- One test per behaviour, at the lowest layer that proves it. One e2e test per promise, not per
  feature. Use soak and fuzz runs for properties.
- Tests wait on conditions, never on fixed sleeps, and log any random seed. A flaky test is fixed
  or deleted in the same stage; we never retry it.
- In `cmd/gff-hab`, a top-level test calls `t.Parallel()` unless it sets the environment or a
  package variable, directly or through a helper. Those stay serial and run before the parallel
  ones. The package mostly waits on real processes, so parallel tests cut its run from about
  11 minutes to about 2 on a dev box.
- Every wait has an upper bound, `waitLong` in the holder test helpers. On a slow machine or CI
  runner, set `GFF_TEST_TIMEOUT_SCALE` (a positive number, such as `3`) to multiply it. Raise the
  bound; never add a sleep.
- A race between our own steps gets a test seam that widens its window, so the test fails every
  time without the fix. Seams compile only with the `testseam` build tag, and the e2e tests in
  `cmd/gff-hab` build their gff-hab with it. Each seam is off until its variable is set:
  `GFF_TEST_SPAWN_DELAY` (a duration) holds a spawn between the holder's answer and the link
  following the pane.
- Nothing a test makes outlives the test binary. A test puts its files in `t.TempDir()`. A
  short socket path comes from `testtmp.Dir`, which removes it when the test ends. What a
  `TestMain` builds once and shares goes in `testtmp.MainDir`, and the `TestMain` ends with
  `testtmp.Run(m, dir)`. Each process a test starts (a holder, a gff-hab, a fake harness) runs
  from one of these folders, so its command line names it, and `testtmp` finds it with `ps`.
  When the tests end, a process still running 5 s later prints a `FAIL: testtmp` line, is
  stopped, and fails the run; so does a folder still there. On SIGINT or SIGTERM, and shortly
  before `-test.timeout` would panic (which skips every cleanup), `testtmp.Run` stops the
  processes and removes the folders. After a SIGKILL or a crash, the next run's `MainDir` or
  `Dir` with the same prefix stops the dead binary's processes and removes its folders.
  `make clean-dev` removes whatever is left, with the build output.
- A test binary that runs git calls `testgit.Hermetic()` first in its `TestMain`. It points
  `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` at `/dev/null` and sets a test identity, so a
  developer's hooks path, filters or default branch never reach the tests or the gff-hab they
  start. A test that needs another author sets `GIT_AUTHOR_*` and `GIT_COMMITTER_*` on its command.
- No coverage targets and no benchmarks until a user-visible budget depends on one. Spec edge
  cases and review findings decide what gets a test.
- Core functionality also gets a fresh-eyes review before its stage closes.

## Layers

| Layer | Covers | Runs where | When | Budget |
|---|---|---|---|---|
| Unit | Frame codecs and goldens, byte ring, structure store, schema compatibility, adapter parsers over fixtures, Swift decoding | Linux CI; Swift on macOS CI when Swift or Apple code changes | Every push | Under 1 min |
| Integration | Holder with real PTYs and a real socket, control and data connections, gff-hab against a fake or real holder, libghostty, all with `-race` | Linux CI; macOS CI when holder packages or Apple code change | Every push | About 2 min on Linux, under 5 min on macOS |
| End to end | Real binaries as separate processes: holder, gff-hab, clients | Linux CI every push; macOS CI nightly | Push and nightly | About 12 tests, under 3 min |
| Soak and fuzz | Restart loop at 200 cycles, reconnect soak, `go test -fuzz` on decoders | Linux CI | Nightly | About 10 min |
| Smoke | Service install, restart and uninstall; signed update and permission grants; the app on a device | Linux CI for service checks; a Mac and a device by hand, from one script and a checklist | Release | About 30 min |

The macOS jobs run on holder changes because macOS PTYs differ from Linux. For example, macOS
cuts a background job off from its terminal when the session leader exits (spec `hab-holder.md`
6.3).

## Must-have end-to-end and smoke tests

The first five are the core set. If only five tests could exist, these would be the five.

| # | Behaviour | Pass condition |
|---|---|---|
| 1 | gff-hab restarts mid-stream, planned and by SIGKILL, 20 times per push and 200 nightly, with a full-screen program and a printing loop | Child PIDs unchanged; client byte stream has no gap and no repeat; screen dump equal |
| 2 | The service manager restarts the logic service (`systemctl --user restart`, `launchctl kickstart -k`) | Child PID unchanged and still writing |
| 3 | Drain from holder build A to build B | New panes land on B; A's children stay reachable; A exits and leaves the registry after its last release; versions N and N+1 work with one gff-hab |
| 4 | A client resumes from a stored offset, across a dropped connection and across a gff-hab restart | Bytes equal the holder's, with no gap and no repeat; an offset past the ring gives a snapshot |
| 5 | Cross-version golden messages for each released API revision, in Go and Swift, in both directions | An old client reads a new hab's messages and a new client reads an old hab's messages; unknown fields survive |
| 6 | Control connection after a gff-hab restart | New epoch; the structure matches; events arrive once and in order |
| 7 | Remote path: `ssh localhost gff-hab connect` | Carries a full session with the same bytes as the local socket; skips when passwordless ssh to localhost is not set up |
| 8 | Single machine | gff-hab opens no internet socket |
| 9 | Two clients on one pane | Same bytes; one answer reaches the program for a terminal query; an observer's resize leaves the PTY size alone |
| 10 | The holder dies by SIGKILL | Children get SIGHUP; gff-hab marks the panes lost within 5 s and never hangs; a new holder starts on the same path |
| 11 | Each harness adapter over its fixture transcripts | Exact `agent.state` stream; a scheduled job runs the newest harness versions |
| 12 | Signed update on a Mac (release smoke) | A pane reads the clipboard and a protected folder with no prompt; the setup check exits 0 |
| 13 | Uninstall (release smoke) | Every file the install wrote is gone |
| 14 | The app on a device (release smoke) | Connects over SSH, lists sessions, the terminal echoes typing, one approval round trip |

Test 5 lives in three places. `api/api_test.go` writes the messages of the current revision to
`api/testdata/rev<N>/`, with every field set and extra `x_unknown_*` fields in every object. The
apigen tests check every golden against the schemas in both directions. The Swift `GoldenTests`
decode every golden.

Golden files are frozen once written, because they are what a hab of that revision sends:

- The goldens of a released revision are never regenerated.
- New messages go into the next revision's set.
- Revision 1 is still unreleased (`api/released/1.json` is the pre-release baseline). New
  revision 1 messages get new files beside the existing ones; existing files never change.
- `-update` only adds files. It never rewrites or removes a file.

An old golden is a message an older hab really sent, so it checks only the fields that existed
when it was written:

- A message can have several files, one per field set. `<message>.json` is the first field set;
  `<message>@<n>.json` is field set n. A new optional field on an existing type goes into a new
  field set; the older files stay byte for byte as they are.
- The newest field set of each message must match the current sample, which fills every field.
  When it no longer matches, `-update` adds the next field set.
- An older field set must decode, and its round trip must give back exactly its own fields. A
  field it lacks must therefore be optional: this is the compatibility promise that a new client
  reads an older hab. Making such a field required fails these files.
- Any other change to an existing message (a removed, renamed or retyped field) fails the older
  files too, and needs a new revision (spec `hab-logic-api.md` 3.3).

## Deliberately not tested

| Not tested | Why |
|---|---|
| SwiftUI snapshots and XCUITest suites | High churn and costly macOS minutes; the device smoke covers the app |
| libghostty parsing beyond our wrapper | Upstream owns it; we test our snapshot round trip |
| Programs surviving a holder crash | Impossible by design (spec `hab-holder.md` 6.6); test 10 checks the clean failure |
| Generated Go and Swift code line by line | `make check-gen` and the cross-version goldens cover it |
| Real harness sessions on every push | Auth, privacy and cost; fixtures per push, newest versions on a schedule |
| Coverage targets and benchmarks | No promise depends on them yet |
| Intel Macs, Windows, more than one Linux distribution | Not product targets |
| Transports not yet built (embedded SSH server, pairing) | Their own stages test them |

## Core tests in detail

`make race` runs the race set (`go test -race -count=3 -parallel 2`; `RACE_COUNT` and
`RACE_PARALLEL` set them) on Linux in
CI, once on a pull request and three times on a direct push to `main` and nightly. The holder packages run three
times on macOS when they change. `make fuzz` runs each holderproto fuzz target for `FUZZTIME` (default 30s); the
nightly job runs it. `GFF_RESTART_LOOP=200 go test -run TestRestartLoop
./internal/link/` is the nightly restart loop of core test 1; the push run does 20.

Core test 1 runs its holder with a ring smaller than the program's output, so a rebuild from the
ring alone cannot pass. Each restart asserts that the link restored the pane's checkpoint, and the
loop compares the rebuilt screen with a reference parser fed the same stream to the same offset.
The parsers in these tests use a 16 MiB scrollback cap: near libghostty's default cap (about
10 kB) a restored terminal drops scrollback at other points than one that parsed every byte.
gff-hab itself defaults to 10 MiB (spec 2.3); `TestScrollbackRestoreMatches` in `cmd/gff-hab`
checks that a restored terminal keeps the same scrollback as a continuous one with that cap.
The restart loop compares the full plain text, and the bounded `snapshot-vt` a client gets (1000
scrollback rows). `TestSubscribeScrollbackBound` in `internal/link` checks that bound and that
`Link.Scrollback` returns the older rows page by page. `TestScrollbackCost` in
`internal/vt/ghostty` logs the cost table in spec 2.3 (`-v`; set `GFF_MEASURE_DIR` to a directory
on a real disk to time the fsync there).

Core tests 2 and 13 are `scripts/service-check.sh`. On macOS it runs only with `GFF_SVC_RUN=1`,
which CI sets: each launch agent it loads is a new background item, and macOS notifies the user
of each one. Without the opt-in it prints why and exits 3. On Linux it needs no opt-in. One run
goes at a time per user. At start it removes what an earlier run left: every `gffsvctest-*` job
or unit, the processes from an old work folder, and the folder. At every exit, pass, failure or
signal, it stops its jobs and processes and removes its work folder. Its logic job label is the
same on every run (`gffsvctest-run`); a holder's label still carries the holder's instance id.
It installs with its own job prefix and a
temporary state directory, starts a child in a pane, restarts the logic job 20 times through the
init system, reinstalls a newer build once, then uninstalls. After each restart it waits for a new
gff-hab epoch, then checks that the child has the same pid, that its own counter still rises, that
its pane's output offset still grows, and that exactly one holder is registered. The child also
checks its environment: TERM, COLORTERM and a locale are set, and no variable of the init
system's job is. After the reinstall it ends the child and waits until the drained holder exits
and its job is gone (on macOS: booted out, plist deleted, and its build's bundle copies
unregistered from LaunchServices and removed). On macOS it also reads `lsregister -dump` after the
install, after the retire and after the uninstall: it must list exactly the installed builds. On macOS both builds are dev helper bundles from `scripts/macos-bundle.sh -dev`, so
the test runs the per-build layout of `docs/specs/macos-identity.md` 6.1. After the uninstall it checks that the
second pane's program ended, that no job is loaded (on macOS `launchctl print gui/<uid>/<label>`
for the logic job and every holder seen, which works over SSH too), and that no process of the
test is left. It lists every path under the unit directory and the work directory before the
install and again after the uninstall: nothing may be gone, and only gff-hab's own state may be
new. A failure uninstalls and removes everything too. It installs with the system's PATH only
(`/usr/bin:/bin:/usr/sbin:/sbin`), so the test hab never finds a program of an installed
Flightdeck or of the user's own PATH. It runs on Linux (systemd) and macOS
(launchd); `internal/service` holds the unit-level check of test 13 with a fake init system.

Core test 4 lives in `cmd/gff-hab/data_test.go`, with the real holder and gff-hab binaries and a
test client that speaks the data frames. Each resume test compares the client's bytes with the
pane's ring, read from the holder after gff-hab stops, with the terminal queries left out: same
bytes at the same offsets. The file also holds the first two checks of test 9 (two clients, one
answer to a query) and the stalled-client and fake-frame checks of spec 5.
`cmd/gff-hab/size_test.go` holds the third check of test 9 (an observer's size leaves the PTY
alone), typing moving ownership between two clients with one event per change, and a redraw
that writes nothing to the program's input. `internal/panesize` proves the same ownership rules
with a fake clock: debounce, throttle, nudge gap and grace period without sleeping.

## Test commands

One row per test command: what it covers, and the spec section it proves where one applies. Run a single test with `-run`; the test names say the rest.

| Command | What it covers |
|---|---|
| `go test ./internal/version/ ./internal/pathfilter/...` | The version request forms and output, release version order |
| `go test ./internal/notices/` | The notice generator on a hand-made repository |
| `go test -race ./internal/procscope/ && go test -race -run TestEscaped ./cmd/gff-hab` | Escaped-process cleanup (`hab-holder.md` Q4) |
| `go test -race ./internal/holder/... ./cmd/...` | Holder end-to-end tests |
| `go test ./internal/service/` | Unit and plist rendering |
| `go test -race ./internal/logfile/ ./internal/health/ && go test -race -run TestFailedRequestLine ./internal/control/ && go test -race -run TestPeerStateLines ./internal/peers/ && go test -race -run TestOutboxExpiryLine ./internal/channels/ && go test -race -run TestLostHolderTold ./internal/link/ && go test -run TestSystemdOutputFiles ./internal/service/ && go test -race -run 'TestHealthRecords\|TestHookPanicRecovered\|TestHookServerDown\|TestSweep\|TestMarkerWriteFails\|TestFlushedExit\|TestStatusLiveness\|TestStatusJSONToWriter' ./cmd/gff-hab` | Diagnostics stage 1 (`diagnostics.md` 15, tests 1 to 5). The log writer rotates at a small cap, reopens a removed file and never blocks on a full queue; a copy-truncated file another process appends to gets its next lines at its start; `gff-hab logs` merges hab and holder files by time and follows across a rename and a truncate. A gff-hab killed by SIGKILL leaves an `unclean` record and a panic (the `GFF_TEST_HAB_PANIC` seam) a `crash` record, both read at the next start, with one crash notice an hour; `gff-hab status` and `gff-hab logs` read them. A hook handler panic is a `recovered` record and the server goes on; a hook server that stops logs a line and repeats it. A failed request, a peer going offline, a message expired unsent and a lost holder each give one line or record, with no request or message text. The sweep caps `hab.stderr` and holder files, keeps 20 crash files, and removes no holder's file when the registry cannot be listed; the caps refuse a symlink. A run marker that cannot be written is tried once a minute and logged once. The shutdown watchdog's exit writes the queued lines first. Concurrent health records all land. `logs -f` prints a rotated file's last lines once and nothing twice after the merge. `status` does not take a reused pid for gff-hab. The systemd units append to the files launchd's jobs write |
| `GFF_INIT_APPEND_TEST=1 go test -run TestCopyTruncateUnderInit ./internal/service/` | Diagnostics test 2 under the real init system: a launchd job (macOS) or a systemd user unit through the units' output shell (Linux) writes its output to a file, gff-hab's copy-truncate empties it, and the job's next lines land at its start. On Linux the log folder is missing at the first start and removed before the second; the job makes it each time. It loads a job, so it runs in CI only: each macOS Go run and the nightly Linux job |
| `go test -race ./internal/peers/ && go test -race -run 'TestPeer' ./cmd/gff-hab` | Peer file path, bad entries by line, the pin edit that keeps comments, two habs sharing one file |
| `go test -race ./internal/pairing/ ./internal/devicestore/ && go test -race -run TestPair ./cmd/gff-hab` | Pairing with a Go SSH client as the phone |
| `go test -run '^$' -fuzz FuzzParseRequest -fuzztime 30s ./internal/pairing` | Fuzz the pairing request parser, which reads bytes from any client before the token check |
| `go test -race ./internal/catalog/ && go test -race -run 'TestCatalog' ./cmd/gff-hab` | Two habs sharing a project show one project with two locations |
| `go test -race ./internal/capcomthread/ ./internal/capcomengine/ && go test -race -run TestResolveCapcom ./internal/channels/ && go test -race -run 'TestCapcomService\|TestUninstallStopsEngine\|TestMCPClearsCredential' ./cmd/gff-hab` | The Capcom service (`capcom-service.md` 18, A2) |
| `go test -race -run 'TestContext' ./internal/capcomengine/ && go test -race -run 'TestServiceConfig' ./internal/capcom/ && go test -race -run TestCapcomServiceContext ./cmd/gff-hab` | The Capcom context manager and memory (`capcom-service.md` 6, 18 A4) |
| `go test -race -run 'TestSupervisorCodexCredential\|TestLink\|TestContextReseedEarly' ./internal/capcomengine/ && go test -race -run TestServiceConfigCodex ./internal/capcom/ && go test -race -run 'TestCapcomService' ./cmd/gff-hab` | The Codex engine's hab half (`capcom-service.md` 4.1, 4.5, 18 A6) |
| `cd capcom-engine && npm ci && npm run typecheck && npm run build && npm test` | The Capcom engine against a fake SDK and a fake hab |
| `cd capcom-engine && node --import tsx --test test/codex-home-cli.test.ts` | The Codex engine's own home (`capcom-service.md` 4.5) with the pinned Codex CLI and a loopback endpoint: the user's global `AGENTS.md` and skills and the CLI's bundled system skills stay out of the request, a plain `codex resume` finds the thread, a login writes through the `auth.json` link. Part of `npm test` |
| `go test -race ./internal/governor/ && go test -race -run 'TestServiceConfigPace' ./internal/capcom/ && go test -race -run 'TestLinkPace' ./internal/capcomengine/ && go test -race -run TestSDKReading ./internal/usage/ && go test -race -run 'TestCapcomServicePace' ./cmd/gff-hab && go test -race -run 'TestGovernorText\|TestBoardGovernor' ./internal/gffclient/` | The usage governor (`capcom-service.md` 10.1, stage G) on a fake clock with hand-written readings |
| `go test -race ./internal/capcomevents/ ./internal/settings/ && go test -race -run TestLinkEventBatches ./internal/capcomengine/ && go test -race -run 'TestDeliverRound\|TestObserveNotices\|TestEventLimitsFromSettings\|TestCapcomWritesFirst' ./cmd/gff-hab` | Capcom writes first (`capcom-service.md` 9, 9.7): the event queue on a fake clock (batches, quiet hours, the daily turn budget, outcomes), round messages and notices into the thread; then the real gff-hab with the stub engine: an event turn whose reply is marked wrote first, its acting calls refused, a capcom round's outcome |
| `go test -race -run 'TestRoundActions\|TestRoundCLIActions\|TestCapcomJobs\|TestCardAnswerNoteIsData' ./cmd/gff-hab && go test -race -run 'TestChannelRouteGuard' ./internal/hook/ && go test -race -run 'TestSoloBatch' ./internal/capcomevents/ && go test -race -run 'TestLinkEventGate\|TestLinkUserTurnFailsClosed\|TestLinkOneBatchPerTurn\|TestDataNoteInput' ./internal/capcomengine/ && go test -race -run TestBoardCapcomJobs ./internal/gffclient/ && (cd capcom-engine && npm test) && (cd apple/FlightdeckKit && swift test --filter testCapcomEventActionsLevels)` | Capcom jobs (`capcom-service.md` 9.6, 18 A9): only a person grants a round's words, an agent route may only remove them, and a grant clears `created_by`; words reach a turn only from a round runner's message, never from a forged round mark, an agent's message or event text; `allow`, `ask` (one confirmation naming the job, nothing before the tap, expiry), `off`, the per-turn limit, the union of two jobs, a notice-only turn, outcome `acted`; the guard and Dispatch on the route; the `WebFetch` gate; gff's `-actions`, export and apply, and the board's `m` filter; the level row in the app; end to end with the stub engine and the hab's own runner |
| `go test -run 'TestContext' ./internal/capcom/ && go test -race -run 'TestCapcomContext' ./cmd/gff-hab` | Capcom context (`capcom.md` 8.4) |
| `go test -race -run 'TestNotifierAsk\|TestAskCategory' ./internal/push/ && go test -race -run 'TestNoticeStore\|TestPushAskContext\|TestCapcomAskAfterRestart\|TestCapcomDetachByDeviceID' ./cmd/gff-hab` | "Ask an agent", hab half (`capcom.md` 8.5) |
| `go test ./internal/capcom/ && go test -race -run 'TestCapcom\|TestResumeSession' ./cmd/gff-hab ./internal/structure` | Capcom host |
| `go test -race ./internal/channels/ && go test -race -run 'TestChannels\|TestChannelTools' ./cmd/gff-hab` | Channels over fake peers |
| `go test -race ./internal/personal/ && go test -race -run 'TestServiceConfigPersonal' ./internal/capcom/ && go test -race -run 'TestCapcomPersonalTools' ./cmd/gff-hab` | The personal tools against fake WeatherKit, Apple Maps and Google servers (never the real services) |
| `go test -race ./internal/rounds/ && go test -race -run 'TestWatch\|TestSecretValues\|TestPaneSafe' ./cmd/gff-hab && go test -race ./docs/examples/mailwatch` | Watchers (`rounds.md` 4.4, 4.7, 14.1 stage 11) |
| `go test -race ./internal/confirm/ ./internal/suggest/ && go test -race -run 'TestConfirm\|TestActionToolText\|TestHarnessStart\|TestMCPCancelled\|TestSnapshotOpenOnly' ./cmd/gff-hab && go test -race -run 'TestSeedAndNote\|TestChannelRoute' ./internal/channels ./internal/hook` | Confirmations against a fake runner |
| `go test -race ./internal/aliases/ && go test -race -run TestDeliveringAndSendTo ./internal/channels/ && go test -race -run 'TestSpawnAliases\|TestSpawnTeammate\|TestSpawnHarnessEdit\|TestReportToUnlistedPeer\|TestSpawnTool\|TestSpawnFolders\|TestTeammates\|TestSpawnEndToEnd\|TestSpawnOnPeer' ./cmd/gff-hab` | The spawn verb (`harness-adapters.md`, Spawn) |
| `go test -race ./internal/suggest/ && go test -race -run 'TestLocalSend' ./internal/channels && go test -race -run 'TestHandOff\|TestFileRecord' ./cmd/gff-hab` | Hand-off and file (`capcom.md` 6.5) on a fake catalog |
| `go test -race ./internal/readtools/ && go test -race -run 'TestReadTool\|TestMCP\|TestReadRoute' ./cmd/gff-hab ./internal/hook` | Read tools over hand-written transcripts in temporary folders |
| `go test -race ./internal/content/` | Rich content |
| `go test -race ./internal/habits/ ./internal/hook/ && go test -race -run 'TestBrief\|TestDefaultView' ./internal/lbsvc/ && go test -race -run 'TestCurrentProject\|TestFileBriefHabits' ./internal/lbcli/ && go test -race -run TestRead ./internal/settings/ && go test -race -run TestLogbookHabits ./cmd/gff-hab` | Logbook habits (`logbook.md` 11, 11.4) |
| `go test -race ./internal/usage/ && go test -race -run 'TestUsage\|TestStatusline' ./cmd/gff-hab` | Usage readings from hand-written fixtures (`testdata/`, expected readings in `.usage.json`; `-update` rewrites them) |
| `go test -race ./internal/mcpreg/ && go test -race -run TestMCPRenderCLI ./cmd/gff-hab` | MCP registry over temporary folders, with a stand-in for Claude's CLI |
| `go test -race -run 'TestLauncher\|TestShellJoin\|TestShellRoundTrip\|TestSecretNotQuoted\|TestHeadersParallel\|TestResolver\|TestLaunchEnvNames' ./internal/mcpreg/ && go test -race -run TestMCPLaunchCLI ./cmd/gff-hab` | Launch-time secrets (8.5) with invented values that no file holds (the fake provider prints rot13 of its reference) |
| `go test -race ./internal/instructions/ && go test -race -run TestInstructions ./cmd/gff-hab` | Instruction renderer over a hand-written fake source tree in a temporary directory |
| `go test -race ./internal/flightrules/ && go test -race -run TestFlightRules ./internal/control ./cmd/gff-hab` | Flight rules ledger over a hand-written Claude Code transcript: each attachment kind, context and ignored attachment types, repeating sources collapsed with a count, a compaction, removals, placeholders, unknown attribution, incremental reads, the configured-source match, and both methods. Over a hand-written Codex 0.169.0 rollout and a scratch file tree: the AGENTS.md block split into matched files and unknown text, the rendered global file known by its header, a replacing block, the removal notice and the skill listing. Over a hand-written opencode 2.3.2 fixture served by a test server: nested files and skills from messages, a change notice and a nameless skill as unknown, computed files from the discovery, the skill and MCP lists, changes between polls, and a server that stopped. The stale flag for all three: an older render digest is stale, the current one is not. Changes between polls keep a recorded entry, and a failing server leaves a warning. The hab service opens a Codex session and an opencode session with no server, and sets the answer's stale flag for a Claude session |
| `go test -race ./internal/hook/ ./internal/agentcli/ && go test -race -run 'TestCompact\|TestNudge\|TestJoinContext' ./cmd/gff-hab` | Self-compact and the context nudge (`harness-adapters.md`) |
| `go test -race -run TestRunExportsSession ./internal/hook/ && go test -race ./internal/agentcli/ && go test -race -run 'TestAgentName\|TestSkillShellMatchesHelp\|TestMCPSkillMatchesTools' ./cmd/gff-hab` | The agent kit (`harness-adapters.md`, Agent kit): Claude Code's start hook exports `AGENT_KIND` and `AGENT_SESSION_ID`; an agent names its own session from a stand-in pane and its next message reaches an idle pane in the envelope from the new name; a taken or bad name is refused; every shell command in the printed manual is in `gff-hab -h` |
| `go test -race ./internal/harness/... ./internal/hook/...` | Hook fixtures per harness to the exact `agent.state` stream |
| `go test ./internal/progstatus/` | OSC 7501 report parsing over hand-written byte fixtures cut at every chunk size |
| `go test -race -run 'TestClaudeAdapter\|TestBlockingPermissionHook' ./cmd/gff-hab/` | The Claude Code adapter end to end with the real binaries |
| `go test -race -run 'TestTrust\|TestSetTrust' ./internal/harness/ && go test -race -run 'TestTrust' ./cmd/gff-hab` | The folder trust card (`app.md` 7.10) |
| `go test -race ./internal/confirm/ && go test -race -run 'TestGroupPlanRow' ./internal/settings/ && go test -race -run 'TestCover\|TestTrustHoldConfig\|TestGroupTrust' ./cmd/gff-hab` | Group starts and the trust cover (`groups.md` 4.2, 12.6, stage B2) |
| `go test -race -run 'TestCodexAdapter\|TestChatCodex\|TestHarnessStart' ./cmd/gff-hab` | Codex adapter end to end |
| `go test -race ./internal/speech/... ./internal/speechproto/ ./internal/oggopus/ && go test -race -run TestSpeech ./internal/control/` | The speech engine against a fake helper (the test binary run as the helper) |
| `go test -run '^$' -fuzz FuzzParse -fuzztime 30s ./internal/oggopus` | Fuzz the Ogg Opus reader, which parses client bytes, seeded with the hand-made streams |
| `GFF_SPEECH_HELPER_BIN=build/gff-hab-speech GFF_SPEECH_MODEL_DIR=<model> go test -run TestRealHelper ./internal/speech` | The engine end to end with the real helper and model, with the vocabulary fix-up on a spoken clip (macOS `say`) |
| `go test -race -run 'TestSpeech\|TestServeSpeech' ./cmd/gff-hab` | Speech setup (the memory default, the config switch, no helper), the vocabulary from git in a session's project folder, then `gff-hab serve` with a fake helper |
| `go test -race ./internal/attach/ && go test -race -run 'TestAttachment\|TestChatAttachments' ./cmd/gff-hab` | Attachments (`app.md` 4.4) |
| `go test -race ./internal/lbsvc/ ./internal/lbcli/ && go test -race -run 'TestLogbook' ./cmd/gff-hab ./internal/control` | The Logbook surface (`logbook.md` 10, 18 rows 4 and 7) |
| `go test -race ./internal/logbook/` | Logbook's store (`logbook.md` 2, 4, 5) on real files with fake clocks, and `Store.Sync` units (7.6), among them a redacted op that travels only with a forget of it, and the hab's forget of one whose forget never arrived (after the grace, once, retried, kept for a kind this build does not know) |
| `go test -race ./internal/lbextra/ && go test -race -run 'TestExport\|TestImport\|TestBackup' ./internal/logbook/ && go test -race -run 'TestSemantic\|TestExportImport' ./internal/lbsvc/ ./internal/lbcli/ && go test -race -run 'TestLogbookSemantic' ./cmd/gff-hab` | Logbook's always-on extras (`logbook.md` 13.2): export and import, backups, the git export, vector search |
| `go test -race -run 'TestIdentityForms\|TestFirstSight\|TestRemoteChange\|TestJoin\|TestPrivateHidden\|TestPaneProjectRules\|TestE9TwoReplicas' ./internal/lbsvc/ && go test -race -run 'TestProjectReads' ./internal/logbook/ && go test -race -run 'TestProjectVerbs\|TestPaneFileHidesPrivate' ./internal/lbcli/ && go test -race -run 'TestLogbookProjects' ./cmd/gff-hab` | Logbook projects (`logbook.md` 3.3, 18 row 5) |
| `go test -race -run 'TestP1\|TestP4\|TestP5\|TestP7\|TestP8\|TestP9' ./internal/logbook/` | Logbook sync's properties (`logbook.md` 7.6, 17) over `Store.Sync` in one process |
| `go test -race ./internal/lbsync/ && go test -race -run 'TestLogbook\|TestOpenLogbook' ./cmd/gff-hab` | Logbook sync in gff-hab (`logbook.md` 7.6) |
| `go test -race ./internal/push/` | Push against an HTTP/2 fake APNs (never Apple) |
| `go test -race ./internal/summary/ && go test -race -run 'TestSummary' ./internal/push/ && go test -race -run TestPushSummaries ./cmd/gff-hab` | Model-written approval titles (`app.md` 16.5) |
| `go test -race -run 'TestAnswerWithdrawsBanner\|TestServePush\|TestPushKeyCommand' ./cmd/gff-hab/` | gff-hab with push |
| `go test -race ./internal/settings/ && go test -race -run 'TestSettings\|TestPermissionWaitLock' ./cmd/gff-hab` | Hab-side settings (`app.md` 20.5) |
| `go test -race ./internal/gitstatus/` | Project git status with real git in temp folders (skips with no git) |
| `go test -race -run 'TestEvent\|TestHabWoke\|TestOutcome' ./internal/rounds/ && go test -race -run 'TestRound\|TestEventBlock' ./internal/channels/ && go test -race -run TestFold ./internal/logbook/` | Event rounds in the store and scheduler (`rounds.md` 4.3, 4.5, 5.2) on a fake clock |
| `go test -race -run 'TestRoundMatcher\|TestRoundMessageRuns\|TestRoundBatchCarried\|TestRoundsExampleSettings\|TestRoundMoveStage9\|TestRoundLimitSettings\|TestRoundEvents$\|TestRoundCapcomPeers\|TestRoundCapcomOneWay\|TestRoundGroupPeers' ./cmd/gff-hab` | Event, capcom and group rounds in gff-hab |
| `go test -race ./internal/rounds/` | Round definitions and the rounds store (`docs/specs/rounds.md` 3, 4.1, 7.6, 7.7) |
| `go test -race -run 'TestRound\|TestSessionRound' ./cmd/gff-hab ./internal/control ./internal/structure` | Rounds in gff-hab |
| `go test -race ./internal/confirm/ && go test -race -run 'TestRoundAgentKit\|TestRoundElsewhere\|TestRoundConfirmation\|TestRoundToolNulls\|TestRoundToolEvents\|TestRoundHostFromPane\|TestMCPProtocol' ./cmd/gff-hab && go test -run TestFindHab ./cmd/gff` | Rounds in the agent kit (`rounds.md` 13, stage 10) |
| `go test -race -run 'TestRoundLock\|TestRoundFork\|TestLock\|TestAgentUse\|TestForkRefused\|TestForkMemo\|TestRoundEnvelope\|TestSendGuarded\|TestClientSendInUse\|TestTargetMemo\|TestTakeOverBeforePrompt\|TestAdoptedTurnMarker\|TestAdoptResumeNotDelivered' ./cmd/gff-hab` | Resume and fork runs |
| `go test -race -run 'TestRoundHarness' ./cmd/gff-hab` | Harness runs (`docs/specs/rounds.md` 7, stage 4) with the real gff-hab and holder and fake `claude` and `codex` programs on `PATH` that report over OSC 7501 |
| `go test -race -run 'TestHarness\|TestTrustDialog\|TestAdoptedTurn' ./cmd/gff-hab` | Harness run units |
| `go test -race ./internal/channels/ && go test -race -run 'TestSupervise\|TestSibling\|TestNudgeOff\|TestCodexContext\|TestGroupSupervision' ./cmd/gff-hab` | Context supervision of group members (`groups.md` 9, stage B6) against fakes |
| `go test -race ./internal/heavy/ ./internal/hoststats/ && go test -race -run TestHeavy ./internal/control/ ./internal/settings/ && go test -race -run 'TestHeavy' ./cmd/gff-hab && go test -race -run 'TestBoardHeavy\|TestKeepHeavy' ./internal/gffclient/` | The heavy-run lock (`groups.md` 8, stage B5) on a fake clock |
| `go test -race ./internal/groups/ ./internal/confirm/ && go test -race -run 'TestGroup\|TestCover\|TestTrustHold' ./cmd/gff-hab` | Agent groups (`groups.md` 3, 5, stage B3) |
| `go test -race -run 'TestAddReviewers\|TestCheckVerdict\|TestSubmitAndVerdict\|TestStuckReviews\|TestReviewKeys' ./internal/groups/ && go test -race -run 'TestGroupReview\|TestGroupPlanOnPeer' ./cmd/gff-hab` | Review (`groups.md` 6, stage B7): the reviewer at plan time, the verdict on the head it reviewed |
| `go test -race ./internal/lane/ && go test -race -run 'TestMayLand\|TestNextLand\|TestUpstream\|TestStackOn\|TestNextLeadsAfterLand\|TestLaneKeys' ./internal/groups/ && go test -race -tags ghostty -run 'TestLane' ./cmd/gff-hab` | The merge lane (`groups.md` 7, stage B4) on throwaway bare remotes with real git |
| `go test -race -run 'TestFind\|TestMovable\|TestMoveKeys\|TestQueued\|TestShortCopiesMoveRefs' ./internal/groups/ && go test -race -run 'TestPendingVerdict\|TestGroupMove' ./cmd/gff-hab` | Moving a group's home (`groups.md` 5.7, 5.9, stage B9) |
| `go test -race -run 'TestBoardGroups\|TestGroupText' ./internal/gffclient/ && go test -race -run TestGroupsKept ./internal/habconn/ && (cd apple/FlightdeckKit && swift test --filter GroupTests)` | Group cards (`groups.md` 5.9) in gff and the app |
| `go test ./internal/capcom/ && go test -race -run TestLinkUserJoinsEventTurn ./internal/capcomengine/ && go test -race -run 'TestCapcomGroup$\|TestCapcomCoordinatorFollowsService' ./cmd/gff-hab && (cd capcom-engine && npm test)` | Capcom as a group's coordinator (`groups.md` 12.1, stage B8) |
| `go test -race ./internal/worktree/ && go test -race -run TestWorktrees ./cmd/gff-hab` | Worktrees with real git in temp folders (skips with no git) |
| `go test -race ./internal/transcript/` | Claude Code transcript and Codex rollout readers over hand-written fixtures (`testdata/claude-*.jsonl` and `testdata/codex-*.jsonl`, expected messages in `.chat.json`; `-update` rewrites them) |
| `go test -race ./internal/cmdmarks/ && go test -race -run TestCommandMarks ./cmd/gff-hab` | OSC 133 commands |
| `go test -race ./internal/opencode/ ./internal/harness/ ./internal/transcript/ && go test -race -run Opencode ./cmd/gff-hab` | opencode 2.x |
| `go test ./internal/detect/...` | Detection tests |
| `go test -race -run 'TestDetectExplain\|TestExplainVerdict\|TestExplainPartial\|TestSetProcessErr\|TestRegionTail' ./cmd/gff-hab` | `detect.explain` end to end |
| `go test -race ./internal/control/... ./cmd/gff-hab/...` | Control connection tests |
| `go test ./internal/link/` | Holder link end-to-end tests |
| `go test -race ./internal/habconn/` | Connection library suite over the Unix socket, an in-process pipe, `SSHTransport` with a stand-in `ssh`, and the real `ssh` to localhost |
| `go test -race ./internal/merge/` | Multi-host merge over three fake habs |
| `go test ./scripts/hygiene/` | The scan in temporary repositories with invented values |
| `go test ./internal/testgit/` | `testgit.Hermetic` keeps a failing global post-checkout hook and a global identity out of git |
| `go test ./internal/testtmp/` | A dead binary's folders and processes are swept, a live one's stay; the process guard finds a process by its folder and stops it |
| `go test -race ./internal/hosts/ ./internal/board/` | Many hosts over in-process fake habs |
| `go test -race ./internal/hoststats/ ./internal/control/ && go test -race -run TestHabStats ./cmd/gff-hab` | Shared host stats |
| `go test -race ./internal/gffclient/` | The workspace over fake habs |
| `go test -race -run 'TestWindowTitleFocus\|TestTitleBeforeAttach\|TestTitleTemplateAndSanitizing\|TestLongPaneTitle' ./internal/gffclient/` | Window titles (`gff-client.md` 3.4) |
| `go test -race -run TestCanvasLinks ./internal/gffclient/ && go test -race -tags ghostty -run 'TestLinkSurvivesComposing' ./internal/gffclient/ && go test -tags ghostty -run TestReadScreenLinks ./internal/vt/ghostty/` | OSC 8 links in the composed view (`gff-client.md` 3.4) |
| `go test -race -run 'TestKittyHost\|TestLegacyKeyGoldens\|TestLegacyCSIGoldens\|TestKittyInTranslate\|TestKittyReplies' ./internal/gffclient/ && go test -race -tags ghostty -run TestKittyPaneEncoding ./internal/gffclient/ && go test -tags ghostty -run 'TestEncodeKeyGoldens\|TestKeyboardFlagsInSnapshot' ./internal/vt/ghostty/` | The kitty keyboard protocol in the composed view (`gff-client.md` 3.4, 8.2) |
| `go test -tags ghostty -run TestSnapshotMidSequence ./internal/vt/ghostty/ && go test -race -tags ghostty -run TestDumpAcross ./internal/link/` | A snapshot of a libghostty terminal whose output stops inside an escape sequence or a UTF-8 character, on either screen and after a restore (`hab-logic-api.md`, checkpoints) |
| `go test -race -run 'TestResume' ./internal/gffclient/ && go test -race -run 'TestResume\|TestSessionResume' ./cmd/gff-hab` | Resume (`gff-client.md` 6.4) |
| `go test -race -run 'TestLogoutCheckpoint\|TestShutdownWatchdog\|TestResumeBookFlush' ./cmd/gff-hab` | Logout, restart and shutdown (`hab-holder.md` 2.11) |
| `go test -race -run 'TestOutside' ./internal/transcript/ && go test -race -run 'TestAdopt\|TestSessionAdopt' ./cmd/gff-hab` | Adopt (`gff-client.md` 6.5) over hand-written transcripts |
| `go test -race -run 'TestSetStale' ./internal/structure/ && go test -race -run 'TestRestart\|TestSessionRestart\|TestStaleWatch' ./cmd/gff-hab` | Restart through resume (`instructions-mcp-flight-rules.md` 13.1). Stub agent states and signals: a restart while the agent works or waits on a request is refused, as are one with no harness or no id, one under a shell, one of an exited or watcher session, one that could not start again, one whose agent takes a prompt before the hang-up, and a second while one runs; none sends a signal. An idle agent gets a hang-up and starts again only after its program ended; one that ignores the hang-up gets a TERM; one that outlives both keeps running; a start that fails after the exit names the untouched conversation. Real holder, a stand-in `claude` and a hand-written transcript: the restart starts `claude --resume <id>` in a new pane of the same session and the transcript is byte-identical. The stale sweep over the hand-written Claude Code transcript: nothing is read without a render record, an older render flags the session, opencode is never flagged, a list agrees, after a restart only a load since then counts, an ended session loses the flag |
| `go test -race -run 'TestLayoutTree\|TestStackArrange\|TestFocusAcrossNestedLayout\|TestNewPane' ./internal/gffclient/ && go test -race -run 'TestCreateCwdOf\|TestLsofName' ./cmd/gff-hab` | New panes, splits and stacks (`gff-client.md` 5.4) |
| `go test -race -run 'TestTabRenameCloseLast\|TestWorkspaceConfig' ./internal/gffclient/` | Tab actions and the workspace name (`gff-client.md` 5.5) |
| `go test -race -run 'TestWorkspaceSaved\|TestStartupTabs\|TestWorkspaceFile' ./internal/gffclient/` | The saved workspace (`gff-client.md` 5.6) |
| `go test -race -run 'TestWheelScrollsLocally\|TestParseMouse' ./internal/gffclient/` | Scrollback and the mouse (`gff-client.md` 5.7) |
| `go test -race -run 'TestLayoutReopen\|TestReopen\|TestShellAfterExit\|TestShellKeyRebound\|TestExitedKeymapConfig' ./internal/gffclient/` | Reopen a closed view and a shell after a command exits (`gff-client.md` 5.8); `TestShellAfterExitInFolder` runs the real holder and gff-hab |
| `go test -race ./internal/grants/ ./cmd/gff/ ./cmd/gff-hab/` | Permission check |
| `go test ./api/modules/` | Block vocabulary and manifest (`docs/specs/modules.md` 6.5) |
| `go test -race ./internal/modhost/` | The module runtime (`docs/specs/modules.md` 7.3, 7.4, 8.4) over the hand-written bundles in `internal/modhost/testdata/bundles/` |
| `go test -race -run TestModuleHostProcess ./cmd/gff-hab` | The real `gff-hab module-host` under `modhost.Host` |
| `go test -race ./internal/modview/` | The terminal renderer of module block trees (`docs/specs/modules.md` 6.6) |
| `go test -race -run 'TestModule\|TestModStyle\|TestCanvasText' ./internal/gffclient/` | Module cards on gff's status board |
| `cd swift/HabAPI && swift test` | Build and test the generated Swift package (Mac), with the block fixtures decoded and encoded back |
| `cd apple/FlightdeckKit && swift test` | Test the app's shared package (it links `swift/HabAPI`) |
| `go test -run TestRoundActionsCapability ./cmd/gff-hab && (cd apple/FlightdeckKit && swift test --filter CapcomJobTests)` | The Capcom jobs list in the app (`app.md` 21.5) over a fake hab: only `capcom` rounds, none from a hab without `round-actions`; a grant shows the prompt and sends the revision shown; a conflict reloads and does not grant; a switch off sends the removal; the action card keeps the whole call; the job card; the two limit rows |
| `cd apple/FlightdeckKit && swift test --filter CapcomThreadTests` | The Capcom service thread in the app (`capcom-service.md` 7.6) over a fake hab |
| `cd apple/FlightdeckKit && swift test --filter 'GovernorTests\|testSendNowOnlyOnTheCapcomThread'` | The usage governor in the app (`capcom-service.md` 10.1) over a fake hab: the pacing line for each basis and pace, no line without the catalog section, and Send now |
| `go test -race -run 'TestLinkAllowSite\|TestCapcomMessage\|TestCapcomPushesMessage\|TestThreadLines' ./internal/capcomengine/ ./internal/push/ ./cmd/gff-hab/ ./internal/gffclient/` | Capcom A3, hab and gff half (`capcom-service.md` 5.3, 7.6, 7.7) |
| `FLIGHTDECK_LIVE_SOCKET=<sock> [FLIGHTDECK_LIVE_SSH_HOST=<host> FLIGHTDECK_LIVE_SSH_COMMAND=... FLIGHTDECK_LIVE_SSH_OPTIONS=...] swift test --filter LiveTests` (in `apple/FlightdeckKit`) | Live check against a real hab |
| `FLIGHTDECK_LIVE_SOCKET=<sock> [FLIGHTDECK_LIVE_PROGRAM='vim -u NONE -N'] swift test --filter LiveTerminalTests` (in `apple/FlightdeckKit`) | Live terminal check against a real hab with a holder (`-tags ghostty` for real snapshots) |
| `FLIGHTDECK_LIVE_SOCKET=<sock> FLIGHTDECK_LIVE_CHAT_SESSION=<session> swift test --filter LiveTests.testLiveChatSendAndReply` (in `apple/FlightdeckKit`) | Live chat check |
| `FLIGHTDECK_LIVE_SOCKET=<sock> FLIGHTDECK_LIVE_ROUNDS=<path to gff-hab> swift test --filter LiveTests.testLiveRounds` (in `apple/FlightdeckKit`) | Live rounds check against a real hab that runs rounds and holds one (a `-every 1m` command round) |
| `apple/scripts/usage-live-check.sh` | Live usage check |
| `FLIGHTDECK_UPDATE_SNAPSHOTS=1 swift test --filter ChatSnapshotTests` (in `apple/FlightdeckKit`) | Rewrite the chat view's text snapshots from `internal/transcript/testdata/*.chat.json` |
| `apple/scripts/ssh-live-check.sh [serve]` | Live check of the app's SSH carrier (swift-nio-ssh) |
| `apple/scripts/pair-live-check.sh` | Live check of QR pairing from the app |
| `cd apple/FlightdeckKit && swift test --filter PairingFlowTests` | The app's half of QR pairing and the password setup |
| `go test ./api -run TestGoldens -update` | Rewrite the golden messages of the current API revision in `api/testdata/` |
