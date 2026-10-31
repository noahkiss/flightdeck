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
- Every wait has an upper bound, `waitLong` in the holder test helpers. On a slow machine or CI
  runner, set `GFF_TEST_TIMEOUT_SCALE` (a positive number, such as `3`) to multiply it. Raise the
  bound; never add a sleep.
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

## Until CI exists

Each holder stage close records a passing `make race` run (`go test -race -count=3` of the holder
packages) on macOS as well as Linux. `make fuzz` runs each holderproto fuzz target for `FUZZTIME`
(default 30s); the nightly job will run it. `GFF_RESTART_LOOP=200 go test -run TestRestartLoop
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

Core test 4 lives in `cmd/gff-hab/data_test.go`, with the real holder and gff-hab binaries and a
test client that speaks the data frames. Each resume test compares the client's bytes with the
pane's ring, read from the holder after gff-hab stops, with the terminal queries left out: same
bytes at the same offsets. The file also holds the first two checks of test 9 (two clients, one
answer to a query) and the stalled-client and fake-frame checks of spec 5.
