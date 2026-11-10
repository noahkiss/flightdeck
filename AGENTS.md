# flightdeck

One personal workspace for coding agents, and later for the rest of a homelab and daily life.
Agents run on several machines. Flightdeck shows them all in one place: a native chat view, a
terminal view, approvals and status. It does not replace any agent harness. It starts, hosts,
observes and presents them.

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
online and offline, session.

## How we build it

- **New code, one architecture.** We study how the best tools behave (herdr, Rex, Moshi and
  earlier terminal tools). We write down the behaviour, then write our own code from that spec.
  Do not paste chunks of other projects' code.
- **Spec first.** Each part starts with a study that writes a behaviour spec in `docs/specs/`
  (what the reference tools do, edge cases, why). Implementation works from the spec.
- **Harnesses.** Supported now: Claude Code, Codex, opencode. Agent detection comes from herdr's
  detection manifests, used as data. Each harness gets a thin adapter of our own: state events
  from hooks, a transcript reader for the chat view, input through the PTY, fixture transcripts.
- **Native data stays native.** Transcripts stay where each harness writes them. Resuming a
  session from a plain terminal must always work.
- **Sessions outlive updates.** Restarting `gff-hab` never kills a running program.
- **Many machines from day one.** The repo builds and runs on macOS and Linux. Nothing assumes
  one host.

## Layout

The repo layout, the API schema and the libghostty and speech-helper builds are in
`docs/layout.md`. Behaviour specs are in `docs/specs/`. Each CLI prints its flags with `-h`.

## Build and test

Go code builds on macOS or Linux; Swift and SwiftUI need a Mac with Xcode.

| Command | What it does |
|---|---|
| `make gen`, `make check-gen` | Regenerate the API schema, Go client and Swift package from `api/`; fail when one is stale |
| `make libghostty` | Build the pinned libghostty-vt with our patches into `build/` (needs zig; `docs/layout.md`) |
| `make test`, `make test-pure`, `make race` | Every Go test with libghostty; the tests that need no cgo; the race set three times (`RACE_COUNT`) |
| `make check-pure` | Fail when a package other than `internal/vt/ghostty` needs cgo |
| `make ci-full`, `make ci-fast` | What `ci.yml` runs after its gofmt and generate checks |
| `cd swift/HabAPI && swift test` | The generated Swift package |
| `cd apple/FlightdeckKit && swift test` | The app's shared package |
| `apple/scripts/generate.sh` | Generate `apple/Flightdeck.xcodeproj` with XcodeGen (not committed) |
| `actionlint` | Lint the workflows |
| `scripts/hygiene-scan.sh --staged` | The leak scan that `hygiene.yml` runs; `--install-hooks` adds it to git |

Which test covers what, the test layers, and the live checks: `docs/testing.md`.

### CI

GitHub Actions, through the shared workflows of `noahkiss/workflows`. What runs where, and why,
and the local command for each job, are in `docs/specs/ci-release.md` (3 and 7). Run the same
commands locally before you push; CI confirms. Release builds and signing run in CI.

## Conventions

- A behaviour change updates its spec in `docs/specs/` and, when it adds a test, `docs/testing.md`.
- Generated files change only through `make gen`; edit `api/` instead.

## Hygiene: the repo stays publishable

- The repo is public. Nothing homelab-specific goes in it: no real hostnames, IPs, tailnet
  names, usernames, home paths, or vault and item names. This covers code, docs, examples,
  commit messages and CI.
- Configuration comes from the environment or a config file. No baked-in default names a real
  machine.
- Test fixtures are written by hand or scrubbed. Never commit a real transcript.
- No agent footers in commits or PRs.
