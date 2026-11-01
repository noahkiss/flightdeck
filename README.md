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

## License

MIT. See [LICENSE](LICENSE).
