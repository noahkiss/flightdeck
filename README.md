# Flightdeck

One place to run and watch coding agents across all of your machines.

Flightdeck is a native iPhone, iPad and Mac app with a terminal client and a small server on
each host. It works with Claude Code, Codex and opencode. Agents keep running when you close
the app, and every session stays resumable from a plain terminal.

**Status:** early design. Nothing here runs yet.

## Build the app

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
ignores that file.

## Ship an iPhone build to TestFlight

Until CI uploads app builds, a Mac uploads them. You need the Apple Distribution identity and an
App Store profile for each iOS bundle ID (the app, the notification service, the widgets),
installed on that Mac.

1. In `apple/Config/Local.xcconfig`, set `DEVELOPMENT_TEAM`, the iOS identity, and
   `APP_IOS_PROFILE`, `APP_NSE_PROFILE` and `APP_WIDGET_PROFILE` (step 2 of
   `Local.example.xcconfig`).
2. Copy `apple/Config/Local.example.env` to `apple/Config/Local.env`. Set the App Store
   Connect API key ID, and a command that prints each secret. Git ignores both files.
3. Run `apple/scripts/testflight.sh`. It archives FlightdeckiOS with the UTC time as the build
   number, then uploads it.
4. App Store Connect processes the build in a few minutes. Testers in the internal group can
   then install it from the TestFlight app.

The app declares only standard encryption (SSH and TLS), so `ITSAppUsesNonExemptEncryption`
is `false` and no upload asks the export question. Sale on the App Store in France needs a
French encryption declaration first. The Mac app ships outside TestFlight, signed with
Developer ID and notarized (`apple/scripts/notarize.sh`).

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
the matched text. `scripts/hygiene-scan.sh --tree` checks every tracked file.

## License

MIT. See [LICENSE](LICENSE).
