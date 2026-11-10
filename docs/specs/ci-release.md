# CI and release: spec

Status: study for review. Part 10, stage 1. Written 2026-11-01.

This spec says what CI and release do for this repo, on which runners, and which shared
workflows from `noahkiss/workflows` they use. It covers the Go gates, the Apple builds, the
hygiene scan, the Go binary release, the Homebrew formula, and the app release (TestFlight for
iPhone and iPad, Developer ID for the Mac). Later stages of Part 10 build from it.

No workflow exists yet. Nothing here names a real machine, account, vault or item. Where a value
is needed, the spec uses a placeholder:

| Placeholder | Meaning |
|---|---|
| `<team-id>` | The Apple Developer team id of whoever signs the release |
| `<prefix>` | The bundle id prefix. The app reads it from `apple/Config/Names.xcconfig` (`APP_BUNDLE_PREFIX`) |
| `<version>` | A release version, `x.y.z`, from the tag |
| `<xcode>` | The pinned Xcode, selected by path: `/Applications/Xcode_<n>.app` |

Contents:

1. Settled decisions
2. Triggers
3. Jobs per path
4. Runners
5. Caching
6. Secrets and variables
7. Release
8. Tap bump
9. Hygiene scan
10. TestFlight and the Mac app
11. Shared workflows: fit and extensions
12. Saving CI minutes: each rule
13. The reference release workflow, described
14. Decisions
15. Sources

---

## 1. Settled decisions

These are decided. This spec states them; it does not reopen them.

- **The Mac app ships with Developer ID**, notarized, outside TestFlight. TestFlight carries the
  iPhone and iPad app only.
- **Export compliance.** The app uses only standard encryption (SSH, TLS), which is exempt. The
  iOS `Info.plist` answers it with `ITSAppUsesNonExemptEncryption` `false`
  (`apple/App/iOS/Info.plist`). The Mac app ships outside the App Store and needs no answer.
- **macOS runners run only when they are needed.** That means when `apple/` changes, when the API
  schema changes (the Swift package is generated from it), or on a tag. They also run when holder
  packages change, plus one nightly macOS end-to-end run: that is the owner's test strategy
  decision of 2026-10-31 (section 14, Q2). Section 3 lists the exact paths.
- **The hygiene scan** has generic checks built in (private IPv4, the CGNAT range `100.64/10`,
  `/Users/<name>` and `/home/<name>` paths). Real patterns come from a repo secret, or from a
  private file that an environment variable names. They are never in the repo. On a hit the scan
  prints the file, the line and the rule name, never the matched text.
- **Changes to `noahkiss/workflows` are reviewed by the owner** before merge, because every caller
  uses `@main`. Each extension in section 11 is a pull request there, backward compatible.
- **Release builds sign on a macOS CI runner, once** (`macos-identity.md` 5.1). No user's Mac
  signs anything.
- **Tags:** `v*` releases the Go binaries; `app-v*` releases the app (coordinator decision,
  section 14, Q1).
- **Homebrew:** the formula is `gff`. A `flightdeck` cask ships the Mac app from its first
  release. No Sparkle for now (section 14, Q3 to Q5).

---

## 2. Triggers

One workflow file per concern. Each row is one file in `.github/workflows/`.

| Workflow | Events | Path filter | Concurrency |
|---|---|---|---|
| `ci.yml` (Go, Linux) | `push` to `main`, `pull_request`, `workflow_dispatch` | `paths`: the Go group (3.1), written as `'**'` then negations, with `docs/examples/**` added back | Group per workflow and ref; cancel unless the ref is `main` |
| `apple.yml` (Swift and Xcode, macOS) | `push` to `main`, `pull_request`, `workflow_dispatch` | `paths`: the Apple group (section 3.1) | Same as `ci.yml` |
| `macos-go.yml` (Go on macOS) | `push` to `main`, `pull_request` on the holder group; `schedule` nightly | `paths`: the holder group (section 3.1) | Same; the nightly run never cancels. Holder group: owner decision, Q2 |
| `hygiene.yml` | `push` to any branch, `pull_request`, tags | None: docs are where leaks happen most | Group per ref; cancel superseded runs |
| `nightly.yml` | `schedule` (one daily slot, UTC), `workflow_dispatch` | None | One group; never cancels |
| `release.yml` (Go binaries) | `push` of a `v*` tag; `workflow_dispatch` with a `tag` input | None (path filters never apply to tags) | Group per tag; never cancels |
| `app-release.yml` (iOS and Mac app) | `push` of an `app-v*` tag, dry-run tags included (7.6); `workflow_dispatch` with `tag` and `dry_run` inputs | None | Group per tag; never cancels |

The caller block for `ci.yml` and `apple.yml` follows the shared one
(`saving-ci-minutes.md`, "Caller block"):

```yaml
concurrency:
  group: ci-${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.ref != 'refs/heads/main' && !startsWith(github.ref, 'refs/tags/') }}
```

**go-ci's own guard.** go-ci also holds a job-level group, today
`go-ci-<repo>-<working-directory>-<ref>`, with `cancel-in-progress: true` fixed. That group has no
workflow name, so `ci.yml` on `main`, `nightly.yml` (a schedule runs on `main`) and
`macos-go.yml` all land in the same group and cancel each other's go-ci jobs, `main` included.
Within one workflow the caller's own group queues runs on `main`, so two pushes never meet in
go-ci's group; the cancel is between workflows. Extension E4 (11.2) fixes this: the group gains the calling workflow's name and the runner OS,
and a `cancel-in-progress` input (default `true`, today's behaviour) lets a caller pass
`${{ github.ref != 'refs/heads/main' }}`. Every go-ci caller here passes that expression. Until E4
merges, these workflows run go-ci's steps inline instead.

**The `ci.yml` path filter.** A plain `paths-ignore: docs/**` would skip runs that change
`docs/examples/`, whose config files the settings, catalog, peers, speech and gff client tests
load (rule 2: never ignore a file the build or tests use). So `ci.yml` lists `paths` with negations, in this
order (a later pattern wins):

```yaml
paths: ['**', '!**/*.md', '!docs/**', 'docs/examples/**', '!apple/**', 'apple/App/Acknowledgements.txt', 'apple/FlightdeckKit/Package.resolved', '!design/**', '!LICENSE']
```

**Required checks.** No check is required on `main` (the default). `ci.yml`, `apple.yml` and
`macos-go.yml` are path-filtered, so a pull request that touches none of their paths never starts
them, and a required check would wait forever. If branch protection ever requires checks, it
requires `hygiene.yml` only (it has no path filter), or each filtered workflow gains a job that
always runs and reports.

**The nightly guard.** `nightly.yml` and the nightly leg of `macos-go.yml` start with a step that
compares `main`'s head with the head of the last successful nightly run (`gh run list --workflow
nightly.yml --status success --limit 1 --json headSha`). When they match, the run ends there. A
night with no new commit costs one short job.

**The harness drift check** (a later Part 10 task: the newest Claude Code, Codex and opencode
against the adapter fixtures) is its own scheduled workflow. Its spec belongs to that task. This
spec reserves its secret names in section 6 only.

---

## 3. Jobs per path

### 3.1 Path groups

| Group | Paths | Why |
|---|---|---|
| Docs | `**/*.md`, `docs/**` except `docs/examples/**`, `LICENSE` | Nothing builds them. Only the hygiene scan runs |
| Go | everything not in Docs, `apple/` or `design/`; `docs/examples/**`, `NOTICE`, `apple/App/Acknowledgements.txt` and `apple/FlightdeckKit/Package.resolved` included | `ci.yml`. Go tests load the example config files in `docs/examples/`; `make check-notices` compares the two notice files with what `Package.resolved` and the rest produce (7.8) |
| Apple | `apple/**`, `swift/HabAPI/**`, `api/**`, `design/icons/**`, `api/testdata/**`, `internal/transcript/testdata/**`, `cmd/gff-hab/testdata/console/**`, `.github/workflows/apple.yml` | The Swift package is generated from `api/`. `GoldenTests`, `ChatSnapshotTests` and `ConsoleTests` read those Go testdata folders. The project uses the app icon in `design/icons/` |
| Holder | `internal/holder/**`, `internal/holderproto/**`, `internal/holderclient/**`, `internal/holderreg/**`, `internal/link/**`, `internal/service/**`, `internal/grants/**`, `cmd/gff-hab-holder/**`, `scripts/service-check.sh`, `scripts/macos-bundle.sh`, `.github/workflows/macos-go.yml` | macOS PTYs and launchd differ from Linux (`testing.md`, Layers) |

`swift/` stays in the Go group too: `make check-gen` diffs `api` and `swift`, so a hand edit to
the generated Swift package must fail the Go gate.

### 3.2 `ci.yml`: the Go gate

Two jobs.

1. **`plan`** (ubuntu, inline, a few seconds). Decides `full` or `fast` (3.3). Reads
   `make ghostty-id` (`GHOSTTY_COMMIT` and the id of our Ghostty patches) and the zig version
   from `mise.toml`. Outputs: `full`, and `ghostty_key`,
   `libghostty-<GHOSTTY_COMMIT>-<patch id>-zig<version>`. The `go` job's make commands pass
   the platform's release `GHOSTTY_ZIGFLAGS` (7.3), so the cached library is the one a release
   links.
2. **`go`** calls `go-ci.yml` with the extensions of section 11 (`mise`, `cache-path`,
   `cache-key`):

| Input | Value |
|---|---|
| `fmt` | `true` |
| `generate-check` | `true` (the same check as `make check-gen`) |
| `mise` | `true`: installs zig from `mise.toml` |
| `cache-path` | `build/libghostty-vt` |
| `cache-key` | `${{ needs.plan.outputs.ghostty_key }}-Linux-X64-baseline` (section 5) |
| `cancel-in-progress` | `${{ github.ref != 'refs/heads/main' }}` (E4) |
| `commands`, full | `GFF_TEST_TIMEOUT_SCALE=3 make ci-full GHOSTTY_ZIGFLAGS='-Dtarget=x86_64-linux-gnu.2.31 -Dcpu=baseline'` (`check-pure`, `vet`, `test`, `race`) |
| `commands`, fast | `GFF_TEST_TIMEOUT_SCALE=3 make ci-fast GHOSTTY_ZIGFLAGS='-Dtarget=x86_64-linux-gnu.2.31 -Dcpu=baseline'` (`check-pure`, `build`) |
| job name | `go (full)` or `go (fast)`, from the `plan` output (7.4) |

**`GHOSTTY_ZIGFLAGS` sits inline in every command that can build libghostty.** go-ci has no `env`
input. Without the flags, a cache miss would build a native-CPU libghostty and save it under the
`-baseline` key, and every later run and release would restore the wrong build.
| `timeout-minutes` | `30` |

`make test` runs every Go test with `-tags ghostty`, including the end-to-end tests over real
binaries. `make race` runs the race list three times. Both run with `GFF_TEST_TIMEOUT_SCALE=3`
(a hosted runner is slower than a dev box; `testing.md`, Principles). go-ci has no `env` input,
so the commands set it inline (`GFF_TEST_TIMEOUT_SCALE=3 make test`).

**Every libghostty build goes through make, or sets its environment.** The bindings find the
library with `#cgo pkg-config: --static libghostty-vt-static`, and only the Makefile's
`GHOSTTY_ENV` points `PKG_CONFIG_PATH` at `build/libghostty-vt/share/pkgconfig`. A bare
`go test -tags ghostty` fails to find it. So CI uses make targets, and a raw `go` command with
`-tags ghostty` starts with
`PKG_CONFIG_PATH="$PWD/build/libghostty-vt/share/pkgconfig" CGO_ENABLED=1`.

**What go-ci runs as it is, and what it needs.** go-ci sets up Go from `go.mod` (the
`toolchain` line), caches modules keyed on `go.sum`, checks `gofmt`, checks `go generate`, and runs
commands in order. That covers `make test-pure`, `make check-pure`, `make race` and the generate
check unchanged. The libghostty half (`make test`, `make vet`, `make build`) needs zig 0.17.0 and
the built library. Without a cache it rebuilds libghostty every run, a few minutes. Extensions
E1 and E2 (11.2) close the gap. Until they merge, the job stays inline, with the same
steps in the same order.

### 3.3 Fast and full runs

- A **pull request** runs the full set. So does a **dispatch**, on any ref.
- A **push to `main`** runs the full set, unless the pushed head is the merge of a pull request
  whose own run passed. Then it runs the fast set. The `plan` job asks
  `GET /repos/{repo}/commits/{sha}/pulls` and looks for a merged pull request whose `ci.yml` run
  on its head succeeded.
- Most changes here land as direct pushes to `main`, so most runs are full. That is rule 3's
  "direct push runs the full suite".
- A **Renovate** pull request runs the full set. The bot rule of rule 3 is for private repos; this
  repo is public.

### 3.4 `apple.yml`: the Apple build

One job on macOS, a call of the shared `xcode-ci.yml` (E5, section 11.2), which runs these
steps from its inputs:

1. Check out. Select `/Applications/Xcode_27.1.app` by path (`xcode-app`, through
   `DEVELOPER_DIR`); fail when it is missing (never the image default, never a beta or release
   candidate). The runner is `xcode-27` (4).
2. Write a `~/.netrc` for `api.github.com` with the job token (`netrc`, on by default). Callers
   with SwiftPM binary targets from GitHub (here libghostty-spm and FluidAudio) saw 403s from the
   anonymous rate limit without it. Removed at the end.
3. Restore the SwiftPM cache (section 5).
4. `cd swift/HabAPI && swift test` (the Swift half of core test 5).
5. `cd apple/FlightdeckKit && swift test`.
6. `apple/scripts/generate.sh` (XcodeGen, pinned release; it copies the Kit's `Package.resolved`
   into the project, section 5).
7. `xcodebuild -project apple/Flightdeck.xcodeproj -scheme FlightdeckiOS -destination 'generic/platform=iOS Simulator' build`.
8. `xcodebuild ... -scheme FlightdeckMac -destination 'generic/platform=macOS' build` (ad hoc
   signed; no `Local.xcconfig` in CI).
9. `xcodebuild -scheme FlightdeckKit-Package -destination <iPhone simulator> test` in
   `apple/FlightdeckKit`: the Kit's tests on an iOS Simulator: the first iPhone, by name, of
   the newest iOS runtime no newer than the selected Xcode's simulator SDK. Runtimes are
   system-wide, and the image's 27.2 RC can bring a runtime Xcode 27.1 cannot test on. Step 5 runs them on macOS; this catches iOS-only breaks.
10. Each `xcodebuild` log is filtered to errors, signing warnings and failed tests; the full logs
    are uploaded as an artifact on failure.

Every `xcodebuild` passes `-clonedSourcePackagesDirPath`, so the cache serves them. No signing, no
secrets. Timeout 60 minutes. The live tests (`LiveTests`, `ssh-live-check.sh`) do
not run in CI: they need a real hab or an sshd, and they skip when unset.

### 3.5 `macos-go.yml`: Go on macOS (owner decision, Q2)

A `plan` job as in `ci.yml` (the guard and the cache key), then `go-ci.yml` with the `runner`
extension set to the macOS label, `mise`, and the libghostty cache keyed `<ghostty_key>-macOS-ARM64-baseline`. Commands on a holder change:
`go test -race -count=3` over the holder group's packages, then `make test`. First the job runs
`brew install bash` and sets `GFF_TEST_BASH` to it: the image's `/bin/bash` is 3.2 and it ships
no Homebrew bash, while the marked shell tests need bash 4.4 (`hab-logic-api.md` 2.5). With
`GFF_TEST_BASH` set, those tests fail on an old bash instead of skipping. The nightly leg and a
dispatch add `scripts/service-check.sh` (core tests 2 and 13 under launchd). The darwin build
passes no `GHOSTTY_ZIGFLAGS` (7.3: Ghostty's build sets its own macOS target). The holder group's
paths also cover `internal/vt/**`, `Makefile`, `go.mod`, `go.sum` and `mise.toml`, so a parser or
pin change runs here the same day. A hosted macOS runner has a GUI
login, which launchd user agents need; stage 2 proves this before it relies on it.

### 3.6 `nightly.yml`

One Linux job (go-ci with `mise` and the cache), after the guard:

- `make ci-full GHOSTTY_ZIGFLAGS='-Dtarget=x86_64-linux-gnu.2.31 -Dcpu=baseline'`: the full set on
  `main` as it stands. The push that merges a passed pull request runs only the fast set (3.3),
  and that pull request ran against an older `main`; this catches a break from merge skew by the
  next morning. It also builds the baseline libghostty on a cache miss, for the commands below.
- `make fuzz FUZZTIME=30s`
- `PKG_CONFIG_PATH="$PWD/build/libghostty-vt/share/pkgconfig" CGO_ENABLED=1 GFF_RESTART_LOOP=200 go test -tags ghostty -run TestRestartLoop ./internal/link/`
  (3.2: a bare `-tags ghostty` command cannot find the library)
- `scripts/service-check.sh` under systemd. The runner user needs a user manager:
  `sudo loginctl enable-linger "$USER"` first; the job waits up to 30 s for the user bus and fails
  with an error when it never appears. Stage 2 proves it on the hosted image.

A scheduled run skips everything when `main` has not moved since the last passing run; a
dispatch always runs everything.

A second small job on `ubuntu-24.04-arm` runs only
`make libghostty GHOSTTY_ZIGFLAGS='-Dtarget=aarch64-linux-gnu.2.31 -Dcpu=baseline'` with the
cache, so the linux arm64 release leg starts warm (5).

Timeout 75 minutes: the full set takes most of it. The slow suites themselves budget about 10 minutes (`testing.md`, Layers).

---

## 4. Runners

The repo is public, so every GitHub-hosted runner is free, macOS and arm64 included. Rule 4 still
holds: macOS runs only what needs it.

| Job | Runner | Why |
|---|---|---|
| `ci.yml`, `hygiene.yml`, `nightly.yml`, every `plan`, `verify` and `publish` job | `ubuntu-latest` | Default. go-ci runs here |
| `apple.yml` | `xcode-27` (arm64) | Xcode 27.1, Swift |
| `macos-go.yml` | `macos-26` (arm64) | launchd, macOS PTYs; needs only the command line clang |
| `release.yml` linux amd64 leg | `ubuntu-24.04` | Native cgo build (7.3) |
| `release.yml` linux arm64 leg | `ubuntu-24.04-arm` | Native cgo build; free on a public repo |
| `release.yml` darwin arm64 leg | `macos-26` | Native cgo build, then sign and notarize; no Xcode project |
| `app-release.yml` | `xcode-27` | Archive, sign, upload, notarize |
| Tap bump | `ubuntu-latest` (the dispatch); the tap's own runners verify | `dispatch-and-wait.yml` |

**Which Xcode.** The project builds with Xcode 27 (the dev Macs run 27.2). GitHub's `macos-26`
image carries Xcode 26.x only. The `xcode-27` image (GitHub-hosted, macOS 27, a public preview)
carries Xcode 27.1 as its default and 27.2 as a release candidate (checked in the runner-images
repo, 2026-11-01). The reference release ran on the same label with Xcode 27.1. So every Xcode
job runs on `xcode-27` and selects `/Applications/Xcode_27.1.app` by path; it moves to 27.2 when
the image carries the final release, after a dry run (7.6). The Go-only macOS jobs stay on the GA
`macos-26` image. If GitHub ends the preview label, the Xcode jobs move to the GA image that
carries Xcode 27.

Pinned labels, not `-latest`, on every release leg: a new image must not change a release
unannounced. Renovate does not bump runner labels; a person does, with a dry run (7.6).

Intel Macs are not a product target (`testing.md`, Deliberately not tested). No x86_64 darwin
build.

---

## 5. Caching

| Cache | Key | Where | Note |
|---|---|---|---|
| Go modules and build cache | `go.sum` | Built into go-ci (`setup-go`) | go-ci saves it on every run |
| libghostty-vt | `libghostty-<GHOSTTY_COMMIT>-<patch id>-zig<version>-<os>-<arch>-baseline`, built by a `plan` step | `build/libghostty-vt` | Keyed on the two pins and the id of `third_party/ghostty/patches/` (`make ghostty-id`), not on a hash of the whole Makefile, which changes often. A pin or patch change misses the cache, as it should. Needs the go-ci `cache-path` and `cache-key` inputs (11.2) |
| zig | `mise.toml` | `jdx/mise-action` cache | Needs the go-ci `mise` input |
| SwiftPM | Runner OS and arch, the Xcode build version, and `hashFiles('apple/FlightdeckKit/Package.resolved')` | `~/Library/Caches/org.swift.swiftpm` (for `swift test`) and the `-clonedSourcePackagesDirPath` folder (for `xcodebuild`) | Built into `xcode-ci` (`spm-cache-key-file`). Restore key without the hash, as the reference does. Saved on a miss once the builds and tests ran, also when one failed; never after an earlier step failed (the cloned packages folder can be empty, and an exact-key entry is not saved again), never from a tag. The project's only package is the Kit, so `generate.sh` copies the Kit's `Package.resolved` into the generated project (`project.xcworkspace/xcshareddata/swiftpm/`): `xcodebuild` resolves the same pins as `swift test`, and the Kit's file is a true key. Every `xcodebuild` passes `-onlyUsePackageVersionsFromResolvedFile`, so a graph that drifts from the pins fails instead of floating |
| XcodeGen | Its pinned version in `generate.sh` | The download folder | Small; optional |
| fastlane gems | None | `bundle install` in the job | Release jobs only. The Gemfile and its lock live under `.github/fastlane/`. No cache: a cache saved on a tag is visible to that tag only, and no `main` run installs these gems, so every release would save an entry nobody reads. A cold install costs about a minute |

**Tests and releases share one libghostty build.** Every CI job builds libghostty with its
platform's release `GHOSTTY_ZIGFLAGS` (7.3), so one cache entry per platform serves the tests and
the release, and the tests run the library that ships. The `-baseline` suffix keeps the entry
apart from a native build. Design call: a separate release build, with its own prefix and keys,
would double the entries and still test a library that never ships.

**Caches saved on a tag are visible to that tag only.** A release leg restores its platform's
entry saved from the default branch and never saves its own. `ci.yml` on `main` keeps the linux
amd64 entry warm, and `macos-go.yml` the darwin one. `nightly.yml` keeps the linux arm64 entry
warm with a small job on `ubuntu-24.04-arm` that only runs `make libghostty` with its flags. A
cold entry costs a few minutes and a warm one nothing, so a release starts warm.

---

## 6. Secrets and variables

Every value lives first in the maintainer's password manager (1Password). CI copies it with
`gh secret set`, piped, never through a file or an argument. The repo and its docs never name
the vault or the item. 1Password references written to any pushed file use ids only.

### 6.1 The `release` environment

Limited to the release tag patterns (7.1), which also admit the dry-run tags `v*-dry.*` and
`app-v*-dry.*` (7.6): the deployment rules are `v*` and `app-v*`, so they do. Only the jobs that
sign, or upload to App Store Connect, name it. `publish` uses the job token.

**Both deployment rules are of type `tag`. No branch rule exists.** `verify` refuses a run that is
not on a tag, but that check lives in the workflow file of the ref being run. A branch named
`v-test`, dispatched with an edited `release.yml` that drops `verify`, meets only the
environment's rule; a branch-type `v*` rule would admit it. The environment is set once by hand:

```sh
gh api -X PUT repos/<owner>/<repo>/environments/release \
  -F 'deployment_branch_policy[protected_branches]=false' \
  -F 'deployment_branch_policy[custom_branch_policies]=true'
gh api -X POST repos/<owner>/<repo>/environments/release/deployment-branch-policies -f name='v*' -f type=tag
gh api -X POST repos/<owner>/<repo>/environments/release/deployment-branch-policies -f name='app-v*' -f type=tag
gh api repos/<owner>/<repo>/environments/release/deployment-branch-policies --jq '.branch_policies[] | "\(.type) \(.name)"'
```

The last command must print exactly `tag v*` and `tag app-v*`.

| Name | Kind | Value lives in | Used by |
|---|---|---|---|
| `MATCH_PASSWORD` | Secret | 1Password, the signing store item, its match password field | Every signing job: decrypts the store |
| `MATCH_DEPLOY_KEY` | Secret | 1Password, the signing store item, its read-only deploy key section | Every signing job: clones the store over SSH |
| `MATCH_GIT_URL` | **Secret** | 1Password, the signing store item, its repo field | Every signing job |
| `UPLOAD_KEY_P8` | Secret | 1Password, the App Store Connect API key item (Developer role), the key file | Upload, notarize |
| `UPLOAD_KEY_ID` | Secret | Same item, the key id field | Upload, notarize |
| `ASC_ISSUER_ID` | Secret | Same item, the issuer id field | Upload, notarize |
| `APPLE_TEAM_ID` | Variable | The Apple Developer account's team id | `scripts/macos-bundle.sh` (`GFF_TEAM_ID`), the signing xcconfig, the export plist |

`MATCH_GIT_URL` is a secret here, not a variable as in the reference workflow. Actions prints a
variable unmasked in the run log, and the logs of a public repo are public. The URL names a
private repository, which is local context (AGENTS.md, Hygiene).

`APPLE_TEAM_ID` may be a variable. Every signed binary carries the team id in its signature, so it
is public once a release ships. It still stays out of the repo, as `macos-identity.md` says.

### 6.2 Repo secrets

| Name | Kind | Value lives in | Used by |
|---|---|---|---|
| `HOMEBREW_TAP_TOKEN` | Secret | 1Password, the GitHub account item, its fine-grained tap publisher token field. A fine-grained PAT with access to `noahkiss/homebrew-tap` only, with the Actions permission read and write (what `dispatch-and-wait.yml` needs to start and watch the run), and nothing else | `release.yml` and `app-release.yml` tap bumps |
| `HYGIENE_PATTERNS` | Secret | 1Password, the hygiene patterns item (a note field) | `hygiene.yml` |

**1Password references.** A reference in a pushed file uses ids only,
`op://<vault-uuid>/<item-uuid>/<field-id>`, never a vault or item name. Known today:

| Secret | Reference |
|---|---|
| `HOMEBREW_TAP_TOKEN` | `op://ha5nenhhlvb7jinwatoxqgxeue/4e3rdqgo7fathngevdrilb34zq/ctrd5d2bhqyzk7kdekdorrx5ji` |

The ids of the other items (the signing store, the App Store Connect key, the hygiene patterns)
live in the release environment's setup notes, outside the repo. Each row is added here once its
id is at hand. Set a secret by piping the read into `gh secret set <NAME> -R noahkiss/flightdeck`
(add `--env release` for 6.1).

`HOMEBREW_TAP_TOKEN` is a repo secret, not an environment secret. A job that calls a reusable
workflow cannot name an environment, and `dispatch-and-wait.yml` is a reusable workflow. The
token's narrow scope makes that acceptable. A pull request from a fork never receives it.

### 6.3 Reserved

| Name | Kind | For |
|---|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` | Secret | The harness drift check (its own task decides which) |

### 6.4 Values that need no secret

| Value | Comes from |
|---|---|
| Bundle id prefix | `apple/Config/Names.xcconfig` (`APP_BUNDLE_PREFIX`), already public |
| Signing identity names | Constants: `Apple Distribution`, `Developer ID Application`; the keychain picks the one certificate of each kind |
| Notary credentials for `macos-bundle.sh` | A `notarytool store-credentials` profile written into the job's throwaway keychain from the three upload values |
| `GITHUB_TOKEN` | The job token: release create and upload, the SwiftPM netrc, run lookups |

---

## 7. Release

### 7.1 Tags (coordinator decision, Q1)

**Decision: two lines** (coordinator, 2026-11-01; section 14, Q1).

| Line | Tag | Ships | Workflow |
|---|---|---|---|
| Go binaries | `v<version>`, `v<version>-rc.<n>` | `gff`, `gff-hab`, `gff-hab-holder` (and the macOS helper bundles) | `release.yml` |
| App | `app-v<version>`, `app-v<version>-rc.<n>` | The iOS app to TestFlight, the Mac app with Developer ID | `app-release.yml` |

Reasons:

- Logic, client and app ship on separate cycles (plan principle 9). A hab fix should not force an
  App Store version, and an app fix should not force a formula bump.
- The three Go binaries share one version. They are built from one commit, the holder protocol
  negotiates versions between them, and Homebrew installs them as one formula.
- The app's marketing version follows App Store rules (rising, three parts). Its own tag line
  keeps that free of the Go line.
- `v*` stays the conventional Go tag, so `go install ...@v<version>` and the module proxy work.
- Cost: the tap's `bump.yml` strips a leading `v` only, so `app-v*` needs one input there (8.2).
  The `release` environment lists both patterns.

The rejected option was one `v*` line for everything. It is simpler, but every release then carries
both halves, and the App Store version must rise with every hab fix.

### 7.2 What a `v*` release ships

| Asset | Holds |
|---|---|
| `gff_<version>_linux_amd64.tar.gz` | `gff`, `gff-hab`, `gff-hab-holder`, `LICENSE`, `NOTICE` |
| `gff_<version>_linux_arm64.tar.gz` | The same |
| `gff_<version>_darwin_arm64.tar.gz` | `GFF Hab.app`, `GFF Holder.app` (signed, notarized, stapled; each holds `LICENSE` and `NOTICE` in `Contents/Resources`), `gff` (signed, notarized), `LICENSE`, `NOTICE` |
| `<asset>.sha256` for each | One line, `sha256sum` format. The tap reads these (8.2) |
| `SHA256SUMS` | Every asset's line |

`NOTICE` is the generated third-party notice file (7.8). Each archive holds one folder named as
the archive without `.tar.gz`, and the files inside it.
Homebrew strips a single top folder, so the formula sees the files directly. The darwin archive
is written with `--no-xattrs --no-mac-metadata` and `COPYFILE_DISABLE=1`, so it carries no
extended attribute headers and no `._` files.

The speech helper (`gff-hab-speech`) is not in a release yet. `gff-hab install` does not copy it,
and its libopus link is dynamic (`voice.md`). It joins when that is settled.

### 7.3 Build path per OS and arch

`gff-hab` and `gff` parse pane output with libghostty-vt through cgo. The Go bindings link the
static library by default (`go.mitchellh.com/libghostty`, the `!dynamic` build file uses
`libghostty-vt-static`). So a release binary carries libghostty inside and needs no shared
library at run time. `gff-hab-holder` is pure Go.

**Decision: a native runner per target, no cross-build.**

| Target | Runner | `gff-hab`, `gff` | `gff-hab-holder` |
|---|---|---|---|
| linux/amd64 | `ubuntu-24.04` | `make release-build` (below) | `CGO_ENABLED=0 go build` (fully static) |
| linux/arm64 | `ubuntu-24.04-arm` | The same | The same |
| darwin/arm64 | `macos-26` | The same, with the command line clang | `CGO_ENABLED=0 go build` |

**`make release-build`** runs `make libghostty`, then builds `gff-hab` and `gff` with the
Makefile's `GHOSTTY_ENV` and `-tags ghostty`, so cgo finds the library (3.2), and
`gff-hab-holder` with `CGO_ENABLED=0`. It writes all three into `RELEASE_DIR` (default
`build/release`), and links in `RELEASE_VERSION` (default `dev`) and `RELEASE_BUILD` (default the
short commit id). The Makefile variable `GHOSTTY_ZIGFLAGS`, empty by default, goes to `zig build`
in the libghostty rule. The release values:

| Target | `GHOSTTY_ZIGFLAGS` | Go side |
|---|---|---|
| linux/amd64 | `-Dtarget=x86_64-linux-gnu.2.31 -Dcpu=baseline` | `CC="zig cc -target x86_64-linux-gnu.2.31"` |
| linux/arm64 | `-Dtarget=aarch64-linux-gnu.2.31 -Dcpu=baseline` | `CC="zig cc -target aarch64-linux-gnu.2.31"` |
| darwin/arm64 | none | `MACOSX_DEPLOYMENT_TARGET=13.0`, and `-mmacosx-version-min=13.0` in `CGO_CFLAGS` and `CGO_LDFLAGS`, all set by `make release-build` |

- **Why fixed targets.** With no `-Dtarget`, zig builds for the host, CPU features included. A
  release built on a new runner CPU could then crash with an illegal instruction on an older
  machine. `baseline` keeps to the architecture's base instruction set. Ghostty's build reads the
  standard zig `-Dtarget` and `-Dcpu` options.
- **The Linux floor.** glibc 2.31 is Ubuntu 20.04 and Debian 11. The libghostty objects and the Go
  link follow the same floor; a floor set on `CC` alone would leave libghostty built for the
  runner's glibc.
- **The macOS floor is 13.0.** When Ghostty's build targets macOS on a Mac, it replaces the target
  with its own: a generic CPU and macOS 13.0 as the minimum, the oldest macOS Apple still
  supports. A `-Dtarget=aarch64-macos.12.0` would be replaced the same way, so the darwin leg
  passes no flags, and 13.0 is the true floor. On macOS, `make release-build` itself exports
  `MACOSX_DEPLOYMENT_TARGET=13.0` for cgo and the link, and the same minimum in `CGO_CFLAGS` and
  `CGO_LDFLAGS` (target-specific variables), so a local build matches the release and the job
  sets nothing. Found while building:
  the Go build cache does not key on `MACOSX_DEPLOYMENT_TARGET`, so cgo objects compiled earlier
  for the host's macOS were linked in, and the linker warned that they were built for a newer
  macOS. The `CGO_` flags are part of the cache key. `scripts/macos-bundle.sh` declares
  `LSMinimumSystemVersion` 13.0 to match. The tap verifies on a newer macOS.

- **Release binaries carry the `ghostty` tag.** Without it gff-hab runs `vt.Fake`, which is for
  tests only. Each leg checks the tag took: `go version -m <binary>` must list `-tags=ghostty`.
- **Every leg uses** `-trimpath` and `-ldflags "-s -w -X main.version=<version> -X
  main.build=<build>"`. `<build>` is the tagged commit's 12-character id; `main.build` is the
  build id a holder and gff-hab already report. Each binary answers `version`, `-version` and
  `--version` with one line, `<name> <version>` (`internal/version`); an unset version reads
  `dev`.
- **Release jobs use no Go build cache** (`setup-go` with `cache: false`). A release compiles
  every package from scratch, so nothing a branch or a test run cached reaches it. libghostty
  still comes from the cache that `main` saves (5).
- **The Linux glibc floor.** A cgo binary links glibc dynamically, so it needs a glibc at least as
  new as the build image's (2.39 on Ubuntu 24.04). Older distributions then fail to start it.
  Design call: the Linux legs set the 2.31 floor for libghostty and for the Go link (the table
  above). zig is already on the runner for libghostty. The native runner still builds and runs
  the binary, so the smoke test (7.4) runs what ships. If `zig cc` fails with cgo in stage 6,
  fall back to the plain toolchain for both halves and state the 2.39 floor in the README.
- **Why not cross-build.** zig can build libghostty for another target, and `zig cc` can drive cgo
  for it. That would let one Linux runner build all three. But the darwin leg must sign and
  notarize on macOS anyway, native arm64 Linux runners are free here, and a native build is
  smoke-tested where it was built. The cross path stays a fallback, not the plan.

### 7.4 `release.yml` jobs

1. **`verify`** (ubuntu, `actions: write`). The tag matches
   `^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$` (or the dry-run form, 7.6). The checkout is the tag's
   commit. The tagged commit is on `main`. No release exists for the tag (not on a dry run). The
   tagged code has passed `ci.yml` (the CI rule below). `make release-schema` leaves `api/released/` unchanged, so the schema of this revision
   is recorded (not on a dry run). Outputs `version` and `tap_inputs`. Each failure says which check failed and what
   to do.
2. **`linux`** (matrix: amd64, arm64; `needs: verify`). Restore the libghostty cache, build (7.3),
   pack the tarball and its `.sha256`, then smoke-test the unpacked tarball with
   `scripts/release-smoke.sh <dir> <version>`: each binary's `version` prints `<name> <version>`;
   `go version -m` lists `-tags=ghostty` for `gff-hab` and `gff`; `ldd gff-hab` lists no
   libghostty; no glibc symbol version above 2.31 (`objdump -T`); `gff-hab-holder` is static; a
   holder and gff-hab start in a temporary directory and `gff-hab api call hab.hello` answers.
   Upload as a workflow artifact. Timeout 30 minutes.
3. **`darwin`** (`needs: verify`, environment `release`). Build (7.3). Throwaway keychain;
   `fastlane match developer_id --readonly --skip_provisioning_profiles` with the store flags of
   10.2 (`--git_url`, `--git_branch main`, `--skip_docs`) installs the Developer ID certificate
   into it. Store a notary profile in the same keychain. Run
   `scripts/macos-bundle.sh -bin-dir <dir> -version <x.y.z> -build <short sha> -o <out>` (the
   bundle version of 7.7, no suffix) with
   `GFF_BUNDLE_PREFIX`, `GFF_SIGN_IDENTITY`, `GFF_TEAM_ID`, `GFF_NOTARY_PROFILE`,
   `GFF_SIGN_KEYCHAIN` and `GFF_NOTARY_KEYCHAIN` from the job. The script signs inside out,
   notarizes one zip, staples, and runs every check of `macos-identity.md` 5.3, including the
   requirement comparison that refuses a release which would drop every grant. Pack, `.sha256`,
   then smoke the unpacked tarball: `codesign --verify --strict`, `stapler validate` and `spctl`
   on each bundle, then `scripts/release-smoke.sh`, which on macOS checks that `gff-hab` and `gff`
   link only system libraries and name macOS 13.0 as their minimum. Upload as an artifact. Delete
   the keychain and keys in an `always()` step. Timeout 45 minutes.

   Details the build settled: fastlane comes from `.github/fastlane/Gemfile` (pinned, with its
   `Gemfile.lock`), installed with `bundle install` (frozen) after `ruby/setup-ruby`, no cache (5). match gets `--app_identifier
   <prefix>.hab`, `--team_id`, and `--keychain_name` and `--keychain_password` for the throwaway
   keychain; it clones the store with `GIT_SSH_COMMAND` naming the deploy key and GitHub's
   published host key, and the key file is removed right after. `GFF_SIGN_IDENTITY` is the
   identity's SHA-1 hash from `security find-identity`, so no certificate name reaches the log.
   The notary profile is `gff-release`, stored in the throwaway keychain. The bundle prefix comes
   from `apple/Config/Names.xcconfig`.
4. **`publish`** (ubuntu, `needs: [linux, darwin]`, `contents: write`; skipped on a dry run). Download the artifacts,
   check each `.sha256`, write `SHA256SUMS`, create the GitHub release as a draft, attach every
   asset, then publish it. When a release for the tag already exists, draft or not, `publish`
   fails and touches nothing, so no asset is ever replaced. `verify` makes the same check first,
   on any run that is not a dry run, so an existing release fails in seconds rather than after
   the builds and the notary. Before it creates the release, `publish` resolves the tag again
   (`gh api repos/<repo>/commits/<tag>`) and refuses when it no longer names the commit the run
   built: the release attaches to wherever the tag points now. An `-rc.` tag publishes as a prerelease. Release notes come
   from `--generate-notes`.
5. **`bump-tap`** (`needs: [verify, publish]`, not on an `-rc.` tag or a dry run):
   `dispatch-and-wait.yml` (section 8). A red `bump-tap` is retried with "Re-run failed jobs" on
   the run that published. A new dispatch would stop at `verify`: the release exists.

**Order before the first final tag.** `bump-tap` dispatches the tap's `bump.yml` with
`formula: gff`. E7 and E8 are merged in the tap (noahkiss/homebrew-tap#10). The `gff` formula is
in noahkiss/homebrew-tap#11 (8.4), and the secret `HOMEBREW_TAP_TOKEN` (6.2) is not set yet.
Both must be in place first. Until then, release only `-rc.` tags, which skip `bump-tap`. A final
tag before that publishes the release, then fails at `bump-tap` with the release already public.

**The CI rule.** Path filters often leave the tagged commit with no run of its own (a docs-only
commit gets no `ci.yml` run; most commits get no `apple.yml` run). So `verify` accepts either:

- A successful **full** run of the workflow on `main` (a push or a dispatch) on commit C, when C
  is an ancestor of the tag and no file in `git diff --name-only C <tag>` falls in the
  workflow's path group (3.1): nothing the workflow tests changed since C. It looks through the
  workflow's recent successful runs (`gh run list --workflow <file> --branch main --status
  success --json headSha,databaseId`), newest first.
- A successful full run dispatched on the tag ref itself.

**Only a full run counts.** The fast run of 3.3 (`make check-pure`, `make build`) runs no tests.
`ci.yml` names its go job `go (full)` or `go (fast)` from the `plan` output, and `verify` reads a
run's jobs (`gh run view <id> --json jobs`) and requires a successful job whose name starts with
`go (full)`: a job that calls a reusable workflow shows as `go (full) / <called job>`. A dispatch
always runs the full set. Every `apple.yml` run is full.

**The path test needs its own matcher** for GitHub's path patterns. A git pathspec cannot do it:
it cannot take `docs/examples/**` back in after it excludes `docs/**`. `verify` reads the group
from the workflow file's own `paths` list, so the two never drift, and matches each changed file
against the globs in order, last match wins, as GitHub does. The matcher is
`internal/pathfilter` (run as `go run ./internal/pathfilter/cmd -patterns <file>`); `verify`
reads the list with `yq '.on.push.paths[]'` and diffs with `--no-renames`, so a moved file counts
at both its old and its new path, with `core.quotePath=false`, so a path with non-ASCII bytes is
not quoted past the negations. A dispatched run is looked up from a minute before the dispatch, so
a runner clock ahead of GitHub's still finds it. `verify` has a 75-minute timeout, room for one dispatched full
run. `verify` checks out the full history
(`fetch-depth: 0`), so the ancestor test and the diff see every commit.

With no qualifying run, `verify` dispatches the workflow on the tag (`gh workflow run <file>
--ref <tag>`; the job token may start a `workflow_dispatch` run), waits for it, and fails unless
it succeeds. On a failure it prints that command, so a person can run it again by hand and then
dispatch the release. That costs seconds when a run qualifies; the other way, always running the
checks inside the release, costs a full Go run or up to an hour of macOS.

**Re-running a release.** Dispatch `release.yml` on the tag (`--ref v<version>`, input `tag`). The
`release` environment refuses `main`. The run uses the workflow file as it was at that tag.
`publish` refuses to replace an asset; delete the draft by hand first if a rebuild is meant.

### 7.5 What is not in a release

No container image: Flightdeck runs on the user's machine, not as a service. No Windows build.
No source tarball beyond GitHub's own.

### 7.6 Dry runs

`release.yml` and `app-release.yml` take a dispatch input `dry_run`. A dry run builds, signs and
checks everything, attaches nothing, uploads nothing, and bumps nothing. Use it after a runner
label, Xcode or signing change.

A dry run must run on a tag ref, because the `release` environment admits tags only and the
signing jobs need its secrets. Two ways:

- **On an existing release tag:** dispatch with `--ref v<version>` (or `app-v<version>`) and
  `dry_run: true`. It rebuilds that release and publishes nothing.
- **On untagged work:** push a tag `v<version>-dry.<n>` (or `app-v<version>-dry.<n>`) at the
  commit, dispatch with that ref and `dry_run: true`, then delete the tag. The push trigger
  excludes these tags (`tags: ['v*', '!v*-dry.*']`), so the push starts nothing. `verify` accepts
  a `-dry.` tag only with `dry_run: true`, and then skips the on-`main` and CI checks.

Every dry run skips two `verify` checks: no release exists for the tag, and the released schema
is recorded. A dry run may build a revision whose schema `api/released/` does not hold yet. A real
release of that revision needs `make release-schema` run and `api/released/` committed on `main`
first.

**The app line differs (as built).** `app-release.yml` starts on the push of an
`app-v<version>-dry.<n>` tag and runs it as a dry run at once: its push trigger is `app-v*`, and
`verify` makes any `-dry.` tag a dry run whatever the dispatch says. Push the tag, watch the run,
delete the tag. Reason: the dry run is how an app signing change is proven, and one push is one
fewer step to get wrong. A `-dry.` tag can never upload, so starting on its push risks nothing.
An app dry run sends the Mac app to Apple's notary and staples it, as `release.yml`'s dry run
notarizes the helper bundles. It uploads nothing to TestFlight, uploads no Mac artifact, and
skips `publish` (10.1). The `release.yml` behaviour above is unchanged.

Design call: dry runs use `-dry.` tags, not a branch the `release` environment admits. The
environment stays limited to tags, so a dry run reaches the signing secrets only the way a release
does. A dry run still signs and notarizes, so it proves a signing change. The cost is a tag pushed
and deleted by hand.

### 7.7 Version forms

`scripts/macos-bundle.sh` accepts `-version` as `x`, `x.y` or `x.y.z` only (its version check),
and Apple's `CFBundleShortVersionString` is up to three integers. So a suffix never reaches a
bundle's short version or the app's marketing version. It stays in the tag, the Go binaries'
`version` and the asset names.

| Tag | Go `version`, asset names | Bundle `-version` | App marketing version | App build number |
|---|---|---|---|---|
| `v1.2.3`, `app-v1.2.3` | `1.2.3` | `1.2.3` | `1.2.3` | UTC minute (10.3) |
| `v1.2.3-rc.<n>`, `app-v1.2.3-rc.<n>` | `1.2.3-rc.<n>` | `1.2.3` | `1.2.3` | UTC minute |
| `v1.2.3-dry.<n>`, `app-v1.2.3-dry.<n>` | `1.2.3-dry.<n>` | `1.2.3` | `1.2.3` | UTC minute |

- An `-rc` and its final share the marketing version. TestFlight takes several builds of one
  version, and the build number keeps them apart. An `-rc` or dry-run Go build never reaches the
  tap, so its bundles carrying `1.2.3` collide with nothing a user installs.
- Stage 6 adjusts the script's version check if a later need breaks this table. This spec does
  not change the script.

### 7.8 Third-party notices

Every artifact we publish carries the notices of the third-party code inside it. One generator,
`internal/notices` (`make notices`), writes two committed files:

| File | Covers | Ships in |
|---|---|---|
| `NOTICE` | The Go standard library; every Go module `gff`, `gff-hab`, `gff-hab-holder` or `gff-hab-speech` links on linux/amd64, linux/arm64 or darwin/arm64 (from `go list -deps` with each binary's tags); libghostty-vt and what it contains (Ghostty, the Zig standard library, uucode, simdutf, Highway, Wuffs); herdr's detection manifests; and the speech helper's sherpa-onnx static library with what it contains (ONNX Runtime with its own notices, kaldi-native-fbank, KISS FFT, kaldi-decoder, OpenFst, simple-sentencepiece, utf8proc, PortAudio, nlohmann/json, hclust-cpp) and libopus | Each release archive, beside the binaries, and `Contents/Resources` of `GFF Hab.app` and `GFF Holder.app` |
| `apple/App/Acknowledgements.txt` | Every Swift package pinned in `apple/FlightdeckKit/Package.resolved`, and what GhosttyKit contains (Ghostty, the Zig standard library, uucode, simdutf, Highway, Wuffs, Oniguruma, libxev, zig-objc, z2d, JetBrains Mono, Nerd Fonts) | The Flightdeck app on iPhone, iPad and Mac (10.5) |

Inputs, all pinned:

- `third_party/notices.toml`: one `[[component]]` per piece that is not a Go module or a Swift
  package, with its URL, licence, licence files and where it sits in each set; its version comes
  from the Makefile (`make:GHOSTTY_COMMIT`, `make:SHERPA_VERSION`), `mise.toml` (`mise:zig`) or
  is written out. One `[[swift]]` entry per pinned Swift package, with its licence.
- `third_party/licenses/`: the licence texts of each component, copied from the source at the
  pinned version; `third_party/licenses/swift/<identity>/` holds each Swift package's
  `LICENSE`, `NOTICE` and third-party licence files at the pinned revision.
- `go.mod` and the module cache: each Go module's top-level `LICENSE`, `COPYING`, `NOTICE` and
  `PATENTS` files, read where `go mod download` puts them. Only the module path names them in the
  output, never a local path. The standard library's text comes from GOROOT; its version is the
  `toolchain` line.

Each entry prints its name, version, source URL, licence and the artifacts that hold it, then its
licence files. A text identical (ignoring whitespace) to one printed earlier is named instead of
repeated.

**What keeps it current.** The generator fails when a Go module has no licence file, a pinned
Swift package has no `[[swift]]` entry or no texts, or an entry names a package that is no longer
pinned. `make check-notices`, part of `make check-gen`, regenerates both files in memory and
fails when either differs from the committed copy; `ci.yml` runs it, and its path filter includes
both files and `Package.resolved` (2). `go test ./internal/notices/` checks the app file against
the repository too, and `AcknowledgementsTests` in the Kit (`apple.yml`) checks that it names
every pinned package. A Renovate bump of a Go module or a Swift package therefore fails CI until
someone runs `make notices` and commits the result.

**Where it is checked in an artifact.** `make release-build` copies `LICENSE` and `NOTICE` into
the release folder; `scripts/macos-bundle.sh` puts both in each bundle's `Contents/Resources`
before signing and beside the bundles; `scripts/release-smoke.sh` fails when either is missing
beside the binaries or in a bundle, or when `NOTICE` has no entry for libghostty, Ghostty and
`golang.org/x/sys`.

**Adding a dependency.** A new Go module needs nothing by hand unless it has no licence file. A
new Swift package needs its texts under `third_party/licenses/swift/<identity>/` and a
`[[swift]]` entry. A new C library, Zig package, font or data set needs its texts and a
`[[component]]` entry. Then `make notices`.

---

## 8. Tap bump

### 8.1 The formula and the cask

| Tap entry | Installs | Bumped by |
|---|---|---|
| Formula `gff` (Q4) | Linux: the three binaries into `bin`. macOS: `GFF Hab.app`, `GFF Holder.app` and `gff` into `libexec`, and links `gff` and `gff-hab` in `bin` | `release.yml`, on a final `v*` tag |
| Cask `flightdeck` | The Mac app | `app-release.yml`, on a final `app-v*` tag |

On macOS the hab installs from the formula's bundles: `gff-hab install` copies them into
`<state>/builds` and never runs them from the Homebrew prefix (`macos-identity.md` 3.4). The
formula's `test do` block runs `--version` on all three binaries (on macOS, the holder inside
its bundle) and requires `<name> <version>`, so a release whose binaries report another version
fails the tap's verify.

**As built (macOS layout).** `gff-hab install` resolves its own executable through links
(`filepath.EvalSymlinks`), takes `GFF Holder.app` beside the `GFF Hab.app` it is in, and takes
the `gff` beside `GFF Hab.app` as the client to copy (`cmd/gff-hab/install.go`, `sources` and
`clientSource`). So the formula keeps the darwin archive's three items together in `libexec`,
as the archive holds them, and `bin` holds links only: `gff` to `libexec/gff`, and `gff-hab` to
the executable inside its bundle. `gff` installed straight into `bin` would leave install with no
client to copy. The holder gets no link: on macOS install always uses launchd and takes the
holder bundle beside `GFF Hab.app`, and only the systemd path looks for `gff-hab-holder` on PATH.
Homebrew changes nothing inside the bundles: their signatures still verify after `brew install`
(proved with dev-signed bundles, 8.4).

**Upgrade and removal.** The service runs the copies in the state directory, not the keg. So
after `brew upgrade gff` the hab keeps running the old build until `gff-hab install` runs again.
That reinstall copies the new build, restarts the logic job, and drains the old holder
(`scripts/service-check.sh` covers it). `brew uninstall gff` leaves the jobs running, so
`gff-hab uninstall` goes first. The formula's caveats say all three: run `gff-hab install`, run
it again after each upgrade, and run `gff-hab uninstall` before `brew uninstall gff`. A
`post_install` cannot do the reinstall: it runs sandboxed and cannot reach launchd.

### 8.2 `bump.yml` and the formula's shape

The tap's `bump.yml` takes `{formula, tag}`, rewrites one `url` and one `sha256`, hashes the asset
it downloads, proves the result with `brew install` and `brew test` on macOS and Linux, then
commits. Its contract with `dispatch-and-wait.yml` is the `request_id` echoed in `run-name`.

| Entry | Fits as it is? | Gap |
|---|---|---|
| Cask `flightdeck` | Yes, for the url and sha | The tag `app-v<version>` gives the version `app-v<version>` (it strips `v` only) |
| Formula `gff` | No | Three urls, one per platform (the shape below); bump.yml handles exactly one |

**The formula's shape.** Homebrew 7 loads every formula for every OS and arch, and the tap's
verify runs `brew readall`. A formula with no `url` for one of them fails to load ("formula
requires at least a URL") and takes the whole tap down with it. We ship no macOS Intel build
(section 4), so `gff` needs a `url` that holds everywhere. An existing tap formula with
per-platform urls solves this the same way. `gff` takes this shape:

- A top-level `version`, then a top-level `url` and `sha256`: the `linux_amd64` asset.
- `on_macos` → `on_arm`: the `darwin_arm64` asset's `url` and `sha256`.
- `on_linux` → `on_arm`: the `linux_arm64` asset's `url` and `sha256`.
- `on_macos`: `depends_on arch: :arm64`, so `brew install` on an Intel Mac stops with Homebrew's
  own message instead of pouring Linux binaries. The formula only loads there.

Every `url` interpolates only `#{version}` and is a release asset with its own `.sha256` (7.2),
the top-level one included. Design call: a top-level source archive with an `install` that stops
would work too, but needs a hash of a GitHub-generated archive and a stop message of our own.
`depends_on arch:` alone is not enough: it gives the formula no `url`. The tap's verify legs run
macOS arm64 and Linux amd64; the Linux arm64 branch is loaded, not installed.

**Extension 1: `tag_prefix`.** A new optional input, default `v`. bump.yml strips it from the tag
to get the version. Existing callers pass nothing and see no change. `app-release.yml` passes
`app-v`.

**Extension 2: per-platform formula.** When a formula holds several `url` lines, each interpolating
only `#{version}`, bump.yml rewrites the one `version` line and every `sha256`. The top-level url
is downloaded and hashed directly, as a single-url formula is today. Each url inside an `on_*`
block takes its sha from the matching published `<asset>.sha256`; bump.yml also downloads that
asset and refuses the bump when the two hashes differ, because the verify legs install on two
platforms only. Every asset and its `.sha256` must be attached before anything is rewritten. A
nested url that names no release asset (a `resource` block) is left alone. Built in
noahkiss/homebrew-tap#10.

A formula with one `url` takes today's path unchanged. The verify legs stay as they are:
`brew readall`, then `brew install` and `brew test` on both platforms.

**As built (the formula).** The shape above, with two changes the built `bump.yml` and `brew
audit --strict` called for:

- **Literal urls, no `version` line.** The built `bump.yml` reads the old tag off the top-level
  url and swaps the old tag and old version as literal strings in every url, as the zellij
  formulae work. So the urls name the version outright instead of interpolating `#{version}`.
  Homebrew reads the version from a GitHub release url (`0.1.0`, `1.2.3-rc.1`), and `brew audit
  --strict` refuses a `version` line that repeats it.
- **No version in a comment.** `bump.yml` refuses a rewrite that leaves the old version anywhere
  in the file. The local proof (8.4) caught a comment that named it.
- **Whole-version swap and check** (noahkiss/homebrew-tap#12, not yet merged). `bump.yml`
  swapped the old version everywhere after swapping the tag, so `v0.0.0` to `v10.0.0` gave
  `v110.0.0`. Its old-version check also matched substrings (`0.0.0` inside `10.0.0`). #12 fixes
  both. A first tag of `v0.x` bumps correctly without it.

The tap had no rule for a formula with no release yet. The formula went in with `v0.0.0` urls and
zero shas, so `brew install` fails until the first bump rewrites it. `bump.yml` downloads the new
assets only, never the old ones, so a placeholder bumps like any other version.

The tap's other bump workflows are not a fit: each is tied to one producer's formulas.

### 8.3 The caller

```yaml
  bump-tap:
    name: Bump the tap
    needs: [verify, publish]
    if: ${{ needs.verify.outputs.prerelease != 'true' && needs.verify.outputs.dry_run != 'true' }}
    permissions: {}
    uses: noahkiss/workflows/.github/workflows/dispatch-and-wait.yml@main
    with:
      repo: noahkiss/homebrew-tap
      workflow: bump.yml
      inputs_json: ${{ needs.verify.outputs.tap_inputs }}
      timeout_minutes: 45
    secrets:
      token: ${{ secrets.HOMEBREW_TAP_TOKEN }}
```

`verify` builds `tap_inputs` with `jq -cn`, so the `uses:` job has no quoting to get wrong. The
condition reads `verify`'s outputs, so a dispatch and a push decide the same way. 7.4 says what
must land in the tap before the first final tag. A red
bump fails the release run, with the tap run's URL in the step summary.

### 8.4 Local proof of the formula

No release existed when the formula was written, so it was proved on an arm64 Mac against a
local stand-in of a `v0.1.0` release:

1. `make release-build RELEASE_VERSION=0.1.0`, then `scripts/macos-bundle.sh -dev -bin-dir
   build/release -version 0.1.0`, then the darwin `Pack` step of `release.yml` (`ditto`, `tar
   --no-xattrs --no-mac-metadata`, `shasum -a 256`). Linux archives were shell stand-ins in the
   release layout, for the rewrite only.
2. The archives served from a local HTTP server on 127.0.0.1, at the
   `<owner>/<repo>/releases/download/<tag>/` path GitHub uses.
3. `bump.yml`'s own "Rewrite the formula" step (extracted with `yq`, GNU sed on PATH), run with
   `TAG=v0.1.0` on a copy of the formula whose urls name the local server. It rewrote all three
   urls and shas and checked each asset against its `.sha256`.
4. In a temporary local tap: `brew readall`, `brew style` and `brew audit --strict` on the
   placeholder formula; `brew audit --strict`, `brew install --build-from-source` and `brew test`
   on the rewritten one. Then `codesign --verify --strict` on both bundles and `gff` inside the
   keg, and `gff-hab install -dry-run` through the `bin` link, which resolved the keg's bundles
   and wrote nothing.
5. The same rewrite step from #12, with `TAG=v10.0.0`: today's `bump.yml` fails (url
   `v110.0.0`) and #12 passes. #12 still refuses a leftover `0.0.0` in a comment.

One stand-in difference: Homebrew reads the version from a `github.com` release url, but from a
`127.0.0.1` url it reads `64` (from `arm64`). The stand-in copy alone carried a `version` line.

Not proved here: an install on Linux. The tap's `ubuntu-latest` verify leg installs the real
`linux_amd64` asset at the first bump; the `linux_arm64` branch is loaded, not installed.

---

## 9. Hygiene scan

### 9.1 What it guards

The repo is public. Nothing pushed may name the owner's machines, network, accounts or private
projects (AGENTS.md, Hygiene). The scan is a cheap guard for that rule, on every push and pull
request, docs included.

### 9.2 Where it runs

- **CI:** `hygiene.yml`, one ubuntu job, timeout 5 minutes. It installs gawk and runs
  `scripts/hygiene-scan.sh` with it (`$GFF_HYGIENE_AWK`) on what the event adds (9.3), with the
  private rules from the secret. gawk, not the image's default awk, so a private rule behaves the
  same in CI as on a dev box with gawk; the script's tests require BSD awk, gawk and dash to agree.
  On a push it never cancels a running scan, so no pushed range goes unscanned; a newer push to a
  pull request may cancel the older run, because every run scans the pull request's whole range.
  It also runs when a pull request's title or body is edited.
- **Locally:** the same script, from a pre-commit and a commit-msg hook that
  `scripts/hygiene-scan.sh --install-hooks` installs (the README says how). Rule 10 says the
  checks pass on the dev machine before a push.

The stage task asked for the scan inside the first job of `ci.yml` (rule 11). It is a workflow of
its own instead, because `ci.yml` skips docs-only changes and docs are where leaks happen most.
On a public repo the extra job costs no minutes, and it runs in parallel.

### 9.3 What it scans

| Mode | Scans | Used by |
|---|---|---|
| `--range <base>..<head>` | Every commit in the range on its own (`git log -p`, merges left out): the lines and file paths it adds, its message, its author and committer (name and email); then `$GFF_HYGIENE_REF`, `$GFF_HYGIENE_PR_TITLE` and `$GFF_HYGIENE_PR_BODY` when set | CI |
| `--staged` | Lines and file paths the index adds | The pre-commit hook |
| `--message <file>` | One commit message: git's comment lines (by `core.commentChar`) dropped, and nothing below the scissors line of `git commit -v` | The commit-msg hook |
| `--tree` | Every tracked text file and every tracked path at `HEAD`, then `$GFF_HYGIENE_REF` | A full check by hand, and CI on a new branch or tag |

Per commit, not the net diff: a leak one commit adds and a later commit removes is still public
in the pushed history, so the range fails on it. Deleted lines and binary contents are never
scanned; a binary file's path is. Commit messages, identities, ref names and pull request text
are pushed text too, so they are scanned like files.

The CI job picks the range from the event:

| Event | Range | Pull request text |
|---|---|---|
| `pull_request` (opened, synchronize, reopened, edited) | `<base sha>..<head sha>` | Title and body, passed through `env` (never interpolated into the script) |
| `push` to a branch | `<before>..<after>`, and the branch name | None |
| `push` that creates a branch or a tag (`before` is all zeros) | `--tree`, then the commits not on `main` with `--range origin/main..<after>` | None |

### 9.4 Rules

Built-in rules, public, in the script:

| Rule name | Matches |
|---|---|
| `private-ipv4` | `10/8`, `172.16/12` and `192.168/16`, as dotted quads |
| `cgnat-ipv4` | `100.64/10` as dotted quads (the range a tailnet uses) |
| `tailnet-ipv6` | An address in the Tailscale IPv6 /48 that starts `fd7a`, `115c`, `a1e0`, any case |
| `tailnet-name` | A `<label>.ts.net` name |
| `home-path` | `/Users/<name>` and `/home/<name>`, where `<name>` is not a placeholder |

This spec writes the ranges in prefix form (`10/8`), not as dotted quads, so it needs no entry in
the allow file (9.5).

A placeholder name is any name the `[A-Za-z0-9._-]` class cannot start (`<user>`, `$USER`), or one
of `user`, `u`, `x`, `other`, `runner`, `you`, `linuxbrew`, `Shared`. Today's tree uses
`/home/user`, `/home/u`, `/Users/u`, `/home/x` and `/Users/other` in tests. A path counts only
where it starts: `/srv/home/x` is not a home path.

Private rules come from outside the repo, in this order:

1. `$GFF_HYGIENE_PATTERNS`, the rules themselves. CI sets it from the secret `HYGIENE_PATTERNS`.
2. Else the file that `$GFF_HYGIENE_PATTERNS_FILE` names (local runs and hooks).
3. Else none: the script says so on one line and runs the built-in rules only.

The format is one rule per line: `<rule name><TAB><extended regex>`, matched by POSIX `awk`
(so no `\b` and no Perl classes). Lines starting with `#` are comments. **A rule name must say
nothing private by itself** (for example `host-name`, `vault`, `tailnet`), because the log prints
it; it holds letters, digits, dots, dashes and underscores only. The rules reach `awk` through
the environment only: never a command-line argument (visible in the process list), never a file
the script writes.

**A bad rule stops the scan.** Before scanning, the script checks each rule in its own `awk` run
with stderr silenced: a line with no tab, a name outside the set above, a regex that does not
compile, or one that matches an empty line (it would hit every line). Any such rule exits 2 with
an error that names only the rule's number, never its text: `awk` prints a regex that fails to
compile in its own message, so that message must never reach a log. A silently skipped rule
would weaken the scan, so none is skipped. The scanner itself also runs with stderr silenced.

**What the secret holds.** The built-in rules cannot know the maintainer's own names. The
`HYGIENE_PATTERNS` secret holds one rule per class below; this spec names the classes, never a
value:

| Rule name | Class |
|---|---|
| `host-name` | The names of the maintainer's machines and servers, short and fully qualified |
| `lan-domain` | The private DNS suffix of the home network |
| `tailnet` | The tailnet's own name (the label in its `ts.net` domain) |
| `user-name` | The maintainer's login names, where they appear outside a home path (`name@host`, `~name`) |
| `public-ip` | Public addresses of the home and offsite networks, which the private ranges miss |
| `vault` | Credential-store vault and item names (references by id are allowed, 6.2) |
| `private-name` | Names of private repositories, projects and default session names |

Each rule is a POSIX extended regex, anchored at word edges with `(^|[^A-Za-z0-9])...([^A-Za-z0-9]|$)`
where a short name could match inside a longer word. The same file serves local runs through
`$GFF_HYGIENE_PATTERNS_FILE`.

### 9.5 Allowed hits

Some fixtures need a private address on purpose: today, `internal/grants/grants_test.go`
(a gateway address and the CGNAT edge cases) and `internal/opencode/client_test.go`. An entry
covers a whole file, so it is kept for test fixtures only; prose, such as this spec, writes ranges
in prefix form instead. The script's own regexes and its tests hold no dotted quad (the tests
build theirs from parts), so they need no entry. The file `.github/hygiene-allow` lists
`<path><TAB><rule name>` pairs. It holds no matched text. A
built-in rule only may be allowed this way; a private rule hit is never allowed by a file in the
repo.

### 9.6 Output

On a hit the script prints one line per hit and exits 1:

```
internal/example.go:42: private-ipv4
internal/example.go (commit 1a2b3c4):42: private-ipv4
commit 1a2b3c4:1: host-name
commit 1a2b3c4 author:1: host-name
<path withheld>: host-name (file path)
pull request body:3: host-name
```

- Never the matched text, never the line's content, never the pattern. One line per rule per
  scanned line. Exit 0 clean, 1 on a hit, 2 on a usage or setup error (a bad rule included).
- A file path that itself hits prints as `<path withheld>`, on the path's own hit and on every
  hit inside that file.
- The scanner (`awk`) prints only the source name, the line number and the rule name. `set -x` is
  off.
- Git output goes to files in a private temporary directory, removed at exit, and each git
  command's status is checked; no pipe hides a failing git command.
- Patterns are not printed when loaded. The secret is masked by Actions anyway; the script must
  not rely on that.
- The workflow reads the branch name and the pull request's title and body inside the step
  (`$GITHUB_REF_NAME`, `jq` over `$GITHUB_EVENT_PATH`), never in the step's `env:` block: the log
  prints that block, so a hit in the body would show there.
- The script's tests (`scripts/hygiene/`, a Go test that runs the script in temporary
  repositories) plant invented values, check each hit's file, line and rule, and check that the
  output holds none of the planted text. Where dash or gawk is installed they run the script under
  each too and require the same output.

### 9.7 Forks

A pull request from a fork receives no secrets. The scan then runs the built-in rules only and
says so. The push to `main` after the merge runs the private rules. A hit there means a history
rewrite of a public commit, so the maintainer reviews fork pull requests with that in mind.

---

## 10. TestFlight and the Mac app

### 10.1 `app-release.yml` jobs

As built (stage 8). The jobs:

1. **`verify`** (ubuntu, `contents: read`, 5 minutes). The run is on the tag it names. The tag
   matches `^app-v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`, or the dry-run form
   `^app-v[0-9]+\.[0-9]+\.[0-9]+-dry\.[0-9]+$` (7.6), which makes the run a dry run. The checkout is
   the tagged commit. Unless the tag is `-dry.`, the commit is on `main`. Unless the run is a dry
   run, the version is not below the last non-dry `app-v*` tag's (10.3). Outputs the marketing
   version, the build number (10.3), `dry_run` and the bundle id prefix from
   `apple/Config/Names.xcconfig`.
2. **`test`** (`needs: verify`). Calls `apple.yml` (3.4) through `workflow_call`, so xcode-ci
   runs the Swift package tests, both Xcode builds and the Kit's simulator tests on the tagged
   commit, with the same inputs as on `main`. No secrets.
3. **`ios`** (`needs: [verify, test]`, `xcode-27`, environment `release`, 120 minutes). Steps
   below. Uploads to TestFlight; on a dry run it exports the `.ipa` to disk and uploads nothing.
4. **`mac`** (`needs: [verify, test]`, `xcode-27`, environment `release`, 90 minutes). Steps
   below. Signs with Developer ID, notarizes and staples, checks the stapled app, and zips it
   as `Flightdeck-<version>-macos-arm64.zip` with its `.sha256`. Uploads the two files as the
   artifact `app-release-mac` (7 days). A dry run notarizes too, but uploads no artifact.
5. **`publish`** (ubuntu, `needs: [verify, mac]`, `contents: write`, 10 minutes; skipped on a
   dry run). Downloads the artifact and checks the `.sha256`. Refuses when the tag no longer
   names the commit the run built, as `release.yml` does (7.4). Creates the GitHub release for
   the tag as a draft when it does not exist (`gh release create --draft --verify-tag
   --generate-notes`, `--prerelease` on an `-rc.` tag, `--latest=false`). The notes start at
   the last non-dry `app-v*` tag before this commit (`--notes-start-tag`, from `git describe`),
   so a `v*` tag between them does not cut them short. Then it attaches the zip and its
   `.sha256` and publishes the draft.

The `ios` and `mac` jobs run in parallel. Each fails alone: a failed TestFlight upload does not
stop the Mac build, and the reverse. `publish` waits for `mac` only.

Not built yet: **`bump-tap`** (`needs: [verify, publish]`, final tags only, not on a dry run).
`dispatch-and-wait.yml` with `inputs_json` from `verify`:
`{"formula": "flightdeck", "tag": "app-v<version>", "tag_prefix": "app-v"}`. It waits for a
Flightdeck cask in the tap and the secret `HOMEBREW_TAP_TOKEN` (6.2).

**Design calls, as built:**

- **xcode-ci's "release mode" is the two composite actions.** `xcode-ci.yml` stays build-only. A
  job that calls a reusable workflow cannot name an environment, and environment secrets do not
  reach a reusable workflow in another repo (11.2, E6). So the signing jobs are the caller's own
  steps, through `actions/xcode-match-signing` and `actions/xcode-archive-upload` at `@main`.
- **The tests run on the tag, through `apple.yml`.** This replaces the CI rule of 7.4 for the
  app line. App tags are rare, macOS minutes are free on a public repo, and the run proves the
  exact tagged commit with no run lookup code. `apple.yml` takes `workflow_call`, so its xcode-ci
  inputs exist once. The cost is the test run's wall time before the archive starts.
- **The App Store Connect key reaches the export step only when it uploads.** On a dry run the
  three key inputs are empty, and `xcode-archive-upload` needs none of them to export to disk.
- **No bundler cache and no SwiftPM cache save.** A tag's cache is visible to that tag only (5).
  The signing jobs restore the SwiftPM cache that xcode-ci saves from `main`, by its key and
  paths; a miss resolves from scratch.
- **Logs upload on failure only.** The archive logs of a passing run hold nothing to act on, and
  the artifacts of a public repo are public.
- **The Mac job notarizes in the action, on a dry run too.** One `xcode-archive-upload` call
  exports, notarizes and staples. The checks then run on the stapled app, the bytes that ship,
  not on an earlier export. A dry run notarizes because notarization is the step a signing
  change most often breaks. Apple sees the app; nobody else does. So the App Store Connect key
  reaches the `mac` job on every run, while `ios` gets it only when it uploads.
- **A dry run keeps the Mac zip on the runner.** The artifacts of a public repo are public, so
  a dry run builds and hashes the zip but uploads no artifact.
- **`publish` is safe to run again.** A draft that a failed run left is reused. An asset already
  attached is kept when its bytes match and refused when they do not, so no asset is ever
  replaced. This differs from `release.yml`, which refuses any existing release (7.4): there,
  `verify` checks first, so an existing release fails in seconds. Here, a re-run of the failed
  `publish` job must finish the release that run started.
- **An app release is never marked latest.** GitHub's "latest" mark stays on the `gff` release
  (`v*`), whose assets the tap and any `releases/latest` link expect.

### 10.2 Steps of the signing jobs

As built. Steps 4 and 5 are `actions/xcode-match-signing`; steps 7 and 9 are `actions/xcode-archive-upload`,
called twice (archive only, then export from its `archive-path` output) so step 8 runs between
them; step 10 is `xcode-match-signing` with `cleanup: 'true'` (E6, section 11.2).

The `ios` job:

1. Check out the tag. Select `/Applications/Xcode_27.1.app` by path (4); fail when missing.
2. Ruby and fastlane from `.github/fastlane/Gemfile` (no bundler cache, as `release.yml`).
3. `~/.netrc` for SwiftPM (3.4, step 2); restore the SwiftPM cache that xcode-ci saved from
   `main`.
4. A throwaway keychain with a random password. The API key goes to a `0600` file under
   `$RUNNER_TEMP` only for the export step, which deletes it.
5. `fastlane match appstore --readonly --git_url "$MATCH_GIT_URL" --git_branch main --skip_docs
   --team_id "$APPLE_TEAM_ID"` for the app, the
   notification service and the widget bundle ids (`<prefix>.flightdeck`, `.notification-service`,
   `.widgets`), into the throwaway keychain, over SSH with the deploy key and GitHub's published
   host key. Remove the deploy key file at once. **Every match call passes the store flags**
   `--readonly --git_url --git_branch main --skip_docs`. Without `--git_branch main`, match reads
   `master`, finds an empty store and tries to create a certificate.
6. `apple/scripts/generate.sh`.
7. Archive `FlightdeckiOS` (Release) with the generated signing xcconfig: `CODE_SIGN_STYLE =
   Manual`, `DEVELOPMENT_TEAM`, `Apple Distribution`, the keychain in `OTHER_CODE_SIGN_FLAGS`, and
   `PROVISIONING_PROFILE_SPECIFIER` looked up by each target's bundle id from match's profile
   names. A `-xcconfig` file outranks the project's settings, so it supersedes the
   `APP_IOS_PROFILE`, `APP_NSE_PROFILE` and `APP_WIDGET_PROFILE` settings that a hand build sets in
   `Local.xcconfig`. Pass `MARKETING_VERSION` and `CURRENT_PROJECT_VERSION`. Filter the log to
   errors and signing warnings.
8. Check the archive: the app's `ITSAppUsesNonExemptEncryption` is `false` (export compliance,
   section 1); each bundle embeds the profile that match installed for its bundle id, and the
   archive signs exactly the bundles the profiles line names; `codesign -dv` shows the team; the
   marketing version and build number are the run's.
9. Export with `method` `app-store-connect`, `destination` `upload`, manual signing and a
   `provisioningProfiles` map per bundle id; authenticate with the API key. On a dry run (a
   `-dry.` tag, or the dispatch input `dry_run`), export with `destination` `export` instead,
   pass no API key, and upload nothing.
10. Always: delete the keychain, the key files and `~/.netrc`. On failure, upload the logs (the
    action's `log-dir`) as an artifact.

The internal TestFlight group receives every build without a step here. Its setup is a one-off
owner step in the signing store's portal tool, outside the repo.

The `mac` job:

1-4. As `ios`.
5. `fastlane match developer_id --readonly --skip_provisioning_profiles`, with the store flags
   of step 5 above, installs the Developer ID certificate (the action's profiles line
   `developer_id macos <prefix>.flightdeck certificates-only`). The Mac app needs no provisioning profile today: its only entitlement is audio
   input. A profile joins when an entitlement needs one (`Local.example.xcconfig`, step 3).
6. Generate, then archive `FlightdeckMac` (Release) with `Developer ID Application`,
   `--timestamp` and `ARCHS=arm64`.
7. Export with `method` `developer-id` and `notarize: 'true'`, with the App Store Connect key
   (`UPLOAD_KEY_P8`, `UPLOAD_KEY_ID`, `ASC_ISSUER_ID`) on every run, a dry run included. The
   action runs `codesign --verify --deep --strict`, then step 8. After it, the workflow
   verifies the stapled app: the Developer ID authority and the team, the hardened runtime, a
   secure timestamp, no `get-task-allow`, every Mach-O arm64 only (Apple's embedded
   `libswift*.dylib` back-deployment libraries ship universal and need only an arm64 slice)
   (`lipo -archs`), and the bundle id and versions.
8. Notarize in the action: zip with `ditto`, `notarytool submit`, then `wait`; on any status but
   `Accepted`, print `notarytool log` and fail. Staple; `stapler validate`; `spctl -a -vv` must
   say `source=Notarized Developer ID`. The workflow runs `stapler validate` and the `spctl`
   check again in its own check step, so the job proves them even if the action changes.
9. Zip the stapled app with `ditto -c -k --keepParent` as
   `Flightdeck-<version>-macos-arm64.zip`; write its `.sha256` (`shasum -a 256`, the file name
   only). Upload both as the artifact `app-release-mac`, except on a dry run.
10. Cleanup as `ios`.

The Mac app embeds the helper bundles later (`macos-identity.md` 3.4). That job will take the
signed bundles from the matching `v*` release's darwin asset, unchanged, and sign the app last
(`macos-identity.md` 5.1, step 5).

### 10.3 Version and build number

- The marketing version comes from the tag, without an `-rc.` or `-dry.` suffix (7.7). `verify`
  refuses a tag whose version is lower than the last non-dry `app-v*` tag's. An `-rc` and its
  final share a version, so equal passes. A dry run skips the check: it uploads nothing.
- One build number per run, from `verify`, so the iOS and the Mac build of one tag match. A
  re-run of a failed job reuses `verify`'s outputs, so on any attempt after the first each signing
  job takes the UTC minute at its own start instead (`github.run_attempt`). That number is later,
  so still higher, and never the number of a build an earlier attempt may have uploaded. A re-run
  of both jobs may then give the iOS and the Mac build different numbers; nothing depends on them
  matching.
- The 12-digit form (`YYYYMMDDHHMM`) is unproven against App Store Connect from CI. The first real
  `app-v*` tag is its first check there; a dry run exports without contacting App Store Connect.
  `apple/scripts/testflight.sh` uses the same form for a hand upload.
- The build number is the UTC time to the minute, `YYYYMMDDHHMM`, the same rule as
  `apple/scripts/testflight.sh`. A CI upload and a hand upload therefore never collide, and each
  is higher than the last. The run number is not used: it restarts per workflow and would fall
  below a hand upload.

### 10.4 Before the first upload

The first CI upload to App Store Connect faces outward, so the owner approves it (a plan
decision). The workflow has no switch for this: pushing the first non-dry `app-v*` tag is the
approval. Before it, prove the pipeline with an `app-v<version>-dry.<n>` tag (7.6), which signs
and exports everything and uploads nothing. The manual path (`apple/scripts/testflight.sh`, the
README) stays as the fallback, for a build that must go out while CI is down.

### 10.5 The app's acknowledgements

`apple/App/Acknowledgements.txt` (7.8) is a resource of both app targets (`apple/project.yml`),
so it sits in `Contents/Resources` of the Mac app and at the top of the iOS app. Settings ends
with an Acknowledgements row that opens the file as text.

---

## 11. Shared workflows: fit and extensions

### 11.1 Map

| Need | Shared piece | Fit |
|---|---|---|
| Go gate on Linux | `go-ci.yml` | Fits for pure Go. Needs `mise`, `cache-path`, `cache-key` for libghostty |
| Go on macOS | `go-ci.yml` | Needs `runner` too |
| Apple build (Swift tests, Xcode builds) | `xcode-ci.yml` (E5) | Built in stage 4; `apple.yml` calls it |
| TestFlight upload and Mac notarization | `actions/xcode-match-signing`, `actions/xcode-archive-upload` (E6) | Built in stage 4; composite, not a reusable workflow. `app-release.yml` (stage 8) uses them |
| Sign and notarize the helper bundles | `actions/macos-sign-notarize` | Not used: it signs one item with a `.p12` secret, while `macos-bundle.sh` signs two bundles and a binary, compares requirements and staples. The script stays the one copy |
| Tap bump | `dispatch-and-wait.yml` | Fits as it is |
| Tap side | the tap's `bump.yml` | Fits since `tag_prefix` and the per-platform formula landed (8.2) |
| Release attach | `actions/release-attach` | Not used: it is shaped for one Tauri app and waits for CI by itself. `publish` (7.4) is a few lines of `gh release` |
| Hygiene scan | None | Stays inline: a script in this repo, no workflow fits even with an input. A shared `hygiene-scan` could come later, once a second repo wants it |

### 11.2 Proposed extensions

Each is a pull request to `noahkiss/workflows` (or the tap), backward compatible: the default
keeps today's behaviour, so no existing caller changes. The owner reviews each before merge.

| # | Where | Change | Default |
|---|---|---|---|
| E1 | `go-ci.yml` | `mise` (boolean): run `jdx/mise-action` (pinned by SHA) before the commands, installing the tools in `mise.toml`, with its cache | `false` |
| E2 | `go-ci.yml` | `cache-path` and `cache-key` (strings): an `actions/cache` restore before the commands and a save after, on the default branch only (as `rust-ci` saves) | `''` (no step) |
| E3 | `go-ci.yml` | `runner` (string), as `rust-ci` has | `ubuntu-latest` |
| E4 | `go-ci.yml` | The job's concurrency group gains `${{ github.workflow }}` and the runner OS, so two workflows (or a Linux and a macOS call) on one ref no longer cancel each other; and a `cancel-in-progress` input (boolean expression), so a caller keeps `main` and scheduled runs from being cancelled (2) | `cancel-in-progress: true`, today's behaviour. The group name changes for every caller, but each caller's runs still share one group, so nothing else changes |
| E5 | new `xcode-ci.yml` | Reusable workflow: Xcode by path (`xcode-app`), optional `generate` command, `project`, `swift-test-dirs` (one per line), `builds` and `tests` (`scheme \| destination [\| package folder]` per line; the destination `ios-simulator` picks an iPhone), `xcodebuild-args`, `spm-cache-key-file`, `netrc` (default on), `logs-name`, `runner` (default `macos-26`; this repo passes `xcode-27`), `timeout-minutes`, `cancel-in-progress`. No secrets | New file |
| E6 | new composite actions | `actions/xcode-match-signing` (throwaway keychain, match read-only per type, platform and id list, a check that each profile arrived, a manual-signing xcconfig that looks up each target's profile by bundle id, outputs `xcconfig` and `profiles`; `cleanup: 'true'` in an `if: always()` step deletes the keychain and keys, since a composite action has no post step) and `actions/xcode-archive-upload` (archive with that xcconfig and the versions, export with a `provisioningProfiles` map for `app-store-connect` upload or export, or for `developer-id` with notarize and staple; it writes and deletes the API key file itself). Composite, run as steps in the caller's job, because environment secrets do not reach a reusable workflow from another repo | New files |
| E7 | tap `bump.yml` | `tag_prefix` input (8.2) | `v` |
| E8 | tap `bump.yml` | Per-platform formula: every url's sha from its `.sha256` asset, the top-level fallback included; a url with no such asset hashed directly (8.2) | Single-url path unchanged |

E4 is needed for `ci.yml`, `nightly.yml` and `macos-go.yml` alike: without it they cancel each
other's go-ci jobs on `main` (2). E5 and E6 are plan stage 4: noahkiss/workflows#16, with a
selftest that builds a small XcodeGen fixture through `xcode-ci` and runs both actions with a
stand-in fastlane. All eight are approved (section 14).

---

## 12. Saving CI minutes: each rule

From `saving-ci-minutes.md` in the shared-workflows guide.

| # | Rule | Status | How, or why not |
|---|---|---|---|
| 1 | Cancel superseded runs | Applied once E4 merges; partly applied until then | Workflow-level group per workflow and ref in `ci.yml`, `apple.yml`, `macos-go.yml`, `hygiene.yml`; `cancel-in-progress` off for `main` and tags. Release and nightly workflows never cancel. go-ci's own job-level guard cancels `main` and nightly runs across workflows today (2); E4 fixes it, and until then those workflows run go-ci's steps inline |
| 2 | Skip runs that test nothing | Applied | `ci.yml` lists `paths` with negations: docs, `apple/` and `design/` are left out, `docs/examples/**` stays in because tests read it (2). `apple.yml` and `macos-go.yml` run on their paths only. `hygiene.yml` has no filter on purpose: docs are what it guards. Tags get their runs |
| 3 | No double runs for one change | Applied | Pull requests run the full set; a push to `main` that merges a passed pull request runs the fast set (3.3); a direct push runs the full set. The bot clause is for private repos: not applicable, this repo is public |
| 4 | macOS only for jobs that need macOS | Applied | Only Xcode, Swift, launchd and macOS PTY jobs and the darwin release leg run on macOS; each is path- or tag-gated |
| 5 | Releases and slow suites on tags or a schedule | Applied | Releases on `v*` and `app-v*` tags; fuzz, the 200-cycle restart loop and the service check run nightly, skipped when `main` has not moved |
| 6 | Cache dependencies keyed on the lockfile | Applied | Go on `go.sum` (go-ci), libghostty on the Makefile pin and `mise.toml`, SwiftPM on `Package.resolved`, gems on `Gemfile.lock` (section 5) |
| 7 | `timeout-minutes` on every job | Applied | Every job states one (sections 3, 7, 9, 10); go-ci and the new workflows take it as an input |
| 8 | Prefer public repos for CI-heavy projects | Applied | The repo is public; the hygiene scan keeps it publishable |
| 9 | Dependency-bot cadence | Applied | No github-actions Dependabot entry here (the workflows repo covers callers at `@main`). `renovate.json` extends `github>noahkiss/renovate-config` (plan stage 2). One repo rule: Renovate ignores `go.mitchellh.com/libghostty`, because it must move with `GHOSTTY_COMMIT` in the Makefile, by hand. Security updates stay on |
| 10 | Run CI's checks locally first | Applied | Every CI command is a `make` target or a repo script that runs the same on a dev box (AGENTS.md, Build and test). The hygiene scan runs locally with a pattern file |
| 11 | Fold tiny jobs into the job before them | Applied, with stated exceptions | The tap bump uses `dispatch-and-wait` as its own job: the reusable workflow cannot be a step. `plan` and `verify` are separate because their outputs gate the matrix. On a public repo the whole-minute rounding costs nothing; the reason is wall time, and each of these adds seconds |
| 12 | Agents batch pushes | Applied | Agents push once per finished change, after local checks. A push to `main` here builds no deploy, but each still runs the full Go set |
| 13 | Pick the runner by visibility and load | Applied | Public repo: GitHub-hosted runners only, arm64 natively on `ubuntu-24.04-arm`, never QEMU. No self-hosted runner |
| 14 | Listener URL from the repo secret | Not applicable | Nothing here deploys through Komodo. The principle behind it applies: no private URL in a variable or a log, which is why `MATCH_GIT_URL` is a secret (6.1) |

---

## 13. The reference release workflow, described

The archived rootshell FlightDeck repo's `release.yml` proved match-based signing and the
TestFlight upload end to end. This spec takes its steps, not its code. What it did, in order:

**Shape.** Triggered on `v*` tags and by dispatch with a tag input; `contents: write`; one
concurrency group per tag that never cancels. Three jobs: iOS, macOS, and the cask bump. Both
build jobs named the `release` environment, ran on the `xcode-27` runner with a 120-minute cap, and
pinned Xcode by path in a workflow-level variable.

**Shared setup (both build jobs).**

1. Check out the tag.
2. Select the pinned Xcode by path; fail with the list of installed Xcodes when it is missing.
3. Install the Metal toolchain when absent, and wait for it to mount.
4. Ruby and fastlane through Bundler, cached.
5. Read the team, bundle id prefix and display name from the project's one xcconfig; derive the
   marketing version from the tag; use the run number as the build number.
6. Write a `~/.netrc` with the job token for the GitHub API (SwiftPM binary downloads).
7. Cache the SwiftPM checkout folder, keyed on `Package.resolved`.
8. Create a throwaway keychain with a random, masked password; write the API key to a private
   file.

**iOS job.** match read-only (`appstore`) for the app and each extension, over SSH with the
deploy key and a pinned GitHub host key; delete the key file. Archive with a generated manual
signing xcconfig whose profile name is derived per target. Check the archive: print each bundle's
`ITSAppUsesNonExemptEncryption`, require `false` on the app, print each embedded profile and
signing authority. Export with `app-store-connect` and `upload`, with an explicit
`provisioningProfiles` map and the API key. Always delete the keychain and keys; upload logs.

**macOS job.** match read-only (`developer_id`) for the Catalyst targets and a native helper.
Archive for arm64 only. Export with `developer-id`, rename the bundle to the display name, verify
the signature deeply and that every Mach-O is arm64 only. Notarize, wait, print the log on
failure, staple, validate, and require `source=Notarized Developer ID` from `spctl`. Then it
signed a zip for Sparkle (with an independent check of the signature against the app's public
key), attached the zip to the GitHub release (prerelease for `-rc`), and published an appcast on
an orphan branch for the newest tag only.

**Cask bump job.** Inline dispatch of the tap's `bump.yml` and a wait for the run found by its
`request_id`, because the token lived in the `release` environment and a job that calls a
reusable workflow cannot name one.

**What this spec changes from it.**

- The Mac app is a native macOS target, not Catalyst. No Metal toolchain step (no shaders).
- No Sparkle and no appcast for now. The coordinator decided against in-app updates for now (Q5).
- `MATCH_GIT_URL` becomes a secret (6.1).
- The build number is the UTC minute, not the run number (10.3).
- The tap token is a repo secret, so the bump uses the shared `dispatch-and-wait.yml` (6.2).

---

## 14. Decisions

All five questions are decided. Each keeps its number, because other sections cite it.

**Q1. Tag scheme. Decision: two lines, `v*` and `app-v*` (coordinator, 2026-11-01).** `v*` tags
release gff, gff-hab and gff-hab-holder. `app-v*` tags release the app (7.1). The other option was
one `v*` line. No `/consensus` run was needed.

**Q2. macOS Go jobs. Decision: run on holder package changes, plus a nightly macOS end-to-end run
(owner, 2026-10-31).** This was not a conflict. The owner settled it in the test strategy decision
of the master plan (2026-10-31), and that decision overrides the broader "macOS only on `apple/`,
the API schema or tags" line in the Part 10 plan text. `AGENTS.md` (Build and test) and
`docs/testing.md` (Layers) already say the same. `macos-go.yml` (3.5) and extension E4 stay. The
macOS minutes are free on a public repo, and the holder is where macOS differs from Linux.

**Q3. The Mac app cask. Decision: publish a `flightdeck` cask at the first Mac release
(coordinator, 2026-11-01).** The tap and its bump workflow already exist (8.1).

**Q4. Formula name. Decision: `gff` (coordinator, 2026-11-01).** `brew install noahkiss/tap/gff`
installs the terminal client and the hab. `flightdeck` stays the cask name, for the app.

**Q5. In-app updates for the Mac app. Decision: no Sparkle for now (coordinator, 2026-11-01).**
The cask updates the app.

**Extensions E1 to E8 (11.2). Decision: approved (coordinator, 2026-11-01).** Each must stay
backward compatible, with defaults that keep current callers unchanged. Before pushing a change to
a shared workflow, check it against one existing caller of that workflow (`noahkiss/heraldo` for
`go-ci.yml`; a cask and a single-url formula for the tap's `bump.yml`).

Design calls this spec makes (not owner questions): a native runner per release target (7.3);
the glibc floor through `zig cc` (7.3); `MATCH_GIT_URL` as a secret (6.1); the tap token as a
repo secret (6.2); the UTC-minute build number (10.3); the nightly skip when `main` has not
moved (2); the hygiene rule names printed but never the match (9.6); one baseline libghostty
build shared by tests and releases, and the macOS 13.0 floor (5, 7.3); the release CI rule
that accepts a full ancestor run (7.4); dry runs on `-dry.` tags (7.6); suffix-free bundle and
marketing versions (7.7); the formula's top-level asset url (8.2); the app line's dry run on the
tag push, its tests through `apple.yml` on the tag, and the Mac job that notarizes on every run,
dry runs included (7.6, 10.1).

---

## 15. Sources

Read while writing this spec, 2026-11-01.

- This repo: `AGENTS.md`, `Makefile`, `mise.toml`, `docs/layout.md`, `docs/testing.md`,
  `docs/specs/macos-identity.md` (3.3, 3.4, 5), `docs/specs/gff-client.md` 15.1,
  `docs/specs/voice.md`, `apple/project.yml`, `apple/Config/*.xcconfig`,
  `apple/App/*/Info.plist`, `apple/scripts/testflight.sh`, `scripts/macos-bundle.sh`.
- `go.mitchellh.com/libghostty` at the version in `go.mod`: `cgo_static.go` and `cgo_shared.go`.
- `noahkiss/workflows`: `go-ci.yml`, `dispatch-and-wait.yml`, the README's "Saving CI minutes
  and time".
- `noahkiss/homebrew-tap`: `.github/workflows/bump.yml` and its per-producer bump workflows.
- `noahkiss/heraldo`: `.github/workflows/ci.yml` (a go-ci, ghcr-build-push and
  dispatch-and-wait caller).
- The archived rootshell FlightDeck repo: `.github/workflows/release.yml` (described in 13, not
  copied).
