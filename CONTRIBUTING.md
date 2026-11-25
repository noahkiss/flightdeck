# Contributing to Flightdeck

This guide covers building from source, running the tests, cutting a release and the leak scan.
The repo layout, the API schema and the native library builds are in
[docs/layout.md](docs/layout.md). Behaviour specs are in [docs/specs/](docs/specs/). Each part
starts from its spec, and a behaviour change updates that spec.

## Build from source

### The hab and `gff` (Go)

The Go code builds on macOS or Linux. The `make` targets and what each one does are in the
"Build and test" table of [AGENTS.md](AGENTS.md#build-and-test). Each CLI prints its flags with
`-h`.

### The app (Swift)

You need a Mac with Xcode. The app project lives in `apple/` and is generated with XcodeGen.

```sh
apple/scripts/generate.sh
xcodebuild -project apple/Flightdeck.xcodeproj -scheme FlightdeckiOS \
  -destination 'generic/platform=iOS Simulator' build
xcodebuild -project apple/Flightdeck.xcodeproj -scheme FlightdeckMac \
  -destination 'generic/platform=macOS' build
```

Builds sign ad hoc by default. To sign with your own team, copy
`apple/Config/Local.example.xcconfig` to `apple/Config/Local.xcconfig` and fill it in. Git
ignores that file. Then open `apple/Flightdeck.xcodeproj` in Xcode, pick your iPhone, iPad or
Mac as the run destination, and run the `FlightdeckiOS` or `FlightdeckMac` scheme.

To look around without a hab, launch the app with `-FlightdeckDemo YES`, or tap "Look
around with demo data" on the first screen. Demo mode loads two invented hosts
([docs/specs/app.md](docs/specs/app.md), 10.5).

## Tests

Which test covers what, the test layers and the live checks are in
[docs/testing.md](docs/testing.md). Run the same commands locally before you push; CI confirms.
What CI runs where, and the local command for each job, are in
[docs/specs/ci-release.md](docs/specs/ci-release.md) (sections 3 and 7).

## Releasing

Release builds sign in CI, never on a contributor's Mac. The full pipeline is in
[docs/specs/ci-release.md](docs/specs/ci-release.md).

### The hab and `gff`

Push a tag `v<x.y.z>`. `release.yml` builds the Linux and macOS archives, signs and notarizes
the macOS bundles, publishes the GitHub release and bumps the Homebrew formula (sections 7
and 8).

### The app

Push a tag `app-v<x.y.z>` (or `app-v<x.y.z>-rc.<n>`) on a commit on `main`. The
`app-release.yml` workflow runs the app tests on that commit. It then archives FlightdeckiOS with
the tag's version and the UTC time as the build number, signs it and uploads it to TestFlight.
App Store Connect processes the build in a few minutes. Testers in the internal group can then
install it from the TestFlight app.

On the same tag CI builds the Mac app, signs it with Developer ID, notarizes and staples it. It
publishes `Flightdeck-<version>-macos-arm64.zip` and its `.sha256` on the GitHub release
(section 10).

To prove the pipeline without an upload, push `app-v<x.y.z>-dry.<n>` instead. That run signs,
notarizes and exports everything, uploads nothing, and may run on a commit off `main`. Delete the
tag after.

The signing jobs read their secrets from the repo's `release` environment, listed in
[docs/specs/ci-release.md](docs/specs/ci-release.md) (sections 6 and 10).

### From a Mac, when CI cannot

You need the Apple Distribution identity and an App Store profile for each iOS bundle ID (the
app, the notification service, the widgets), installed on that Mac.

1. In `apple/Config/Local.xcconfig`, set `DEVELOPMENT_TEAM`, the iOS identity, and
   `APP_IOS_PROFILE`, `APP_NSE_PROFILE` and `APP_WIDGET_PROFILE` (step 2 of
   `Local.example.xcconfig`).
2. Copy `apple/Config/Local.example.env` to `apple/Config/Local.env`. Set the App Store
   Connect API key ID, and a command that prints each secret. Git ignores both files.
3. Run `apple/scripts/testflight.sh`. It archives FlightdeckiOS with the UTC time as the build
   number, then uploads it.

To notarize a local Mac build, run `apple/scripts/notarize.sh`.

The app declares only standard encryption (SSH and TLS), so `ITSAppUsesNonExemptEncryption`
is `false` and no upload asks the export question. Sale on the App Store in France needs a
French encryption declaration first.

## Check for leaks before you commit

The repo is public. `scripts/hygiene-scan.sh` refuses private addresses, tailnet addresses and
names, home paths, and any private patterns you give it. CI runs it with gawk on every push and
pull request, commit by commit. It uses your `awk` locally; set `GFF_HYGIENE_AWK=gawk` to match
CI. To run it on each commit, install its hooks:

```sh
scripts/hygiene-scan.sh --install-hooks
```

If git's `core.hooksPath` is set, the installer prints the two lines to add to those hooks
instead. Keep your own patterns outside the repo, one `<name><TAB><regex>` per line, and point
`GFF_HYGIENE_PATTERNS_FILE` at that file. A hit prints the file, the line and the rule name, never
the matched text. `scripts/hygiene-scan.sh --tree` checks every tracked file. Section 9 of
[docs/specs/ci-release.md](docs/specs/ci-release.md) has the full rules.

Test fixtures are written by hand or scrubbed. Never commit a real transcript.
