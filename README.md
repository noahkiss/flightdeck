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

## Check for leaks before you commit

The repo is public. `scripts/hygiene-scan.sh` refuses private addresses, home paths and any
private patterns you give it. CI runs it on every push and pull request. To run it on each
commit, install its hooks:

```sh
scripts/hygiene-scan.sh --install-hooks
```

If git's `core.hooksPath` is set, the installer prints the two lines to add to those hooks
instead. Keep your own patterns outside the repo, one `<name><TAB><regex>` per line, and point
`GFF_HYGIENE_PATTERNS_FILE` at that file. A hit prints the file, the line and the rule name, never
the matched text. `scripts/hygiene-scan.sh --tree` checks every tracked file.

## License

MIT. See [LICENSE](LICENSE).
