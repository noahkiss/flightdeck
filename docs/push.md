# Push setup

Flightdeck pushes approvals, questions, errors and notices to your iPhone, iPad, Mac and Apple
Watch. There is no Flightdeck push service. Each hab sends to Apple's push service (APNs)
itself, with an APNs key you make in your own Apple Developer account (`docs/specs/app.md` 8).

Push is optional. Without it, the app works with live updates while it is open.

## What you need

- An Apple Developer Program membership.
- A Flightdeck app that **your own team signs**, with your own bundle id. An APNs key pushes only
  to apps of its own team. A build that someone else signed cannot receive your pushes
  (`app.md` 18.3).
- `gff-hab` running on each host that should push.

## 1. Make an APNs key

1. Sign in to the Apple Developer site. Open **Certificates, Identifiers & Profiles**, then
   **Keys**.
2. Add a key. Turn on **Apple Push Notifications service (APNs)**. Save it.
3. Download the key file. Its name is `AuthKey_<KEY ID>.p8`. Apple lets you download it once.
4. Note the **Key ID** (10 characters) and your **Team ID** (10 characters, under Membership).

One key works for every app of your team, in both the development and production
environments. Treat the file as a password: anyone with it can push to your apps.

## 2. Store the key where a command can print it

gff-hab does not read a key file. It runs a command that prints the key on stdout, and keeps
the key in memory only. Store the `.p8` file's contents in a password manager or keychain that
a command can read without a terminal prompt. Some examples:

| Store | Command |
|---|---|
| `pass` | `["pass", "show", "flightdeck/apns-key"]` |
| 1Password CLI | `["op", "read", "op://<vault>/<item>/<field>"]` |
| Secret Service (Linux) | `["secret-tool", "lookup", "service", "flightdeck-apns"]` |

The command must print the whole PEM text, from `-----BEGIN PRIVATE KEY-----` to
`-----END PRIVATE KEY-----`. Delete the downloaded file once the store holds it.

## 3. Write the push config

Write `push.json` in the hab's state directory. That is `~/.local/state/gff/hab/` on Linux (or
`$XDG_STATE_HOME/gff/hab/`), and `~/Library/Application Support/gff/hab/` on macOS. `-state` or
`$GFF_HAB_STATE` moves it.

```json
{
  "key_command": ["pass", "show", "flightdeck/apns-key"],
  "key_id": "ABCDE12345",
  "team_id": "TEAM123456",
  "topic": "com.example.flightdeck",
  "environment": "production"
}
```

| Field | Value |
|---|---|
| `key_command` | The command that prints the key, as an argv: the program, then each argument. No shell runs it, so pipes, `~` and `$VAR` do not work; wrap them in a script of your own if you need them. A program with no `/` is looked up on gff-hab's `PATH` |
| `key_id` | The key's 10-character id |
| `team_id` | Your 10-character team id |
| `topic` | The bundle id of your Flightdeck build |
| `environment` | `production` or `development`. Optional, default `production`. Used only for a Live Activity token whose device sent no device token |

Each device token carries its own environment, so one hab pushes to a development build from
Xcode and a TestFlight build at once.

The environment can set or override any field: `GFF_HAB_PUSH_KEY_COMMAND`,
`GFF_HAB_PUSH_KEY_ID`, `GFF_HAB_PUSH_TEAM_ID`, `GFF_HAB_PUSH_TOPIC`,
`GFF_HAB_PUSH_ENVIRONMENT`. `GFF_HAB_PUSH_KEY_COMMAND` is a JSON array of strings, such as
`["pass", "show", "flightdeck/apns-key"]`. `gff-hab serve -push-config <path>` reads another
file.

An older config with `key_file` (or `GFF_HAB_PUSH_KEY_FILE`) is refused: push stays off, and
the log says to set `key_command`.

### How gff-hab runs the command

- In the background after start. gff-hab never waits for it.
- With gff-hab's environment, stdin from `/dev/null` and no terminal, so it cannot wait on a
  password prompt. A store that asks with a dialog (a GUI agent) can still work.
- For at most 30 seconds. Past that, gff-hab kills it and everything it started.
- Stdout is capped at 16 KiB and must be one P-256 private key in PEM (`PRIVATE KEY`).
- The key is never logged and never written to disk. A failure logs the program, its exit
  status and at most the first line of stderr, cut to 120 bytes. A line that looks like key
  material (a `-----` marker or a long base64 run) is left out.

### Under the installed service

`gff-hab install` runs gff-hab as a launchd or systemd user job. That job gets a bare
environment: `PATH` (the installing shell's), the locale and `$GFF_MACOS_GRANTS`, nothing
else. So:

- `GFF_HAB_PUSH_KEY_COMMAND` set in your shell never reaches the job. Use `push.json`.
- A password manager's CLI may lack the variables or the agent it needs, such as a session
  token, `GNUPGHOME`, `PASSWORD_STORE_DIR`, `DBUS_SESSION_BUS_ADDRESS` or an agent socket.
- Give the program as an absolute path, or point `key_command` at a wrapper script of your own
  that sets those variables and then runs the store's command.

Test the command the way the job runs it, with an empty environment:

```sh
env -i PATH="$PATH" LANG="$LANG" pass show flightdeck/apns-key </dev/null
```

It must print the key with no prompt.

### When the command fails

A failure leaves push off and logs one line. gff-hab tries again after 30 seconds, then waits
twice as long after each failure, up to 30 minutes. A password manager that is locked at boot
only delays push: unlock it, and push turns on at the next try.

## 4. Restart gff-hab

gff-hab reads the config at start. Its log says which way it went:

- `push is on: topic com.example.flightdeck, ...`: the command printed a key and push works.
  The welcome carries the `push` capability.
- `push is not set up ...`: no config file and no environment. Clients get live updates only.
- `push is off: <reason>`: the config is incomplete or malformed, or the key command failed.
  gff-hab still starts. A bad config needs a fix and a restart; a failed command is tried
  again, as above.

A client that connected before push turned on sees the `push` capability at its next
connection. An agent that was already waiting on an approval or a question before push turned
on gets no banner for it; the next change of its state pushes as usual.

Then open the app. It registers its push token with each paired hab, every time it connects.
Connections › Notifications shows each hab's push status. A Debug build from Xcode registers
a development token, a Release or TestFlight build a production one. The iPhone app needs its
notification service extension and widget extension signed by the same team (`APP_NSE_PROFILE`
and `APP_WIDGET_PROFILE` in `Local.xcconfig`).

## When push is not set up

- The hab leaves out the `push` capability. `push.register` answers `unavailable`.
- The app shows "Push is not set up on <hab>" in Connections › Notifications, with a link to
  this guide.
- Approvals, questions and notices still reach the app while it is open.
- `gff-hab notify` still works: open clients see the notice.

## When push breaks

- **APNs refuses the key or the topic** (a revoked key, the wrong team, a bundle id the key does
  not cover): the hab logs `push broken` once and keeps trying with the next push. Check the
  config.
- **APNs calls the provider token invalid** (`InvalidProviderToken`, such as after you replace
  the key in your store): the hab runs the key command again, at most once every 5 minutes. A
  new key sends the push again. If the command fails, the hab keeps the key it had.
- **A device token is no longer valid**: the hab drops it and logs once. The app sends a new one
  when it next connects.
- **APNs rate-limits the hab**: the hab backs off. Approvals wait out the back-off; Live
  Activity progress updates are dropped until it ends.
- **A device is offline**: APNs keeps an approval until the request expires, and other
  notifications for an hour. Live Activity updates are not kept.
- **gff-hab restarts**: a pane's last state, replayed after the restart, raises no new
  banner. Pushes still queued when gff-hab stops are dropped.

## Notices from scripts

Any local script can raise a Flightdeck notification through the hab:

```sh
gff-hab notify "Backup finished" "All 3 snapshots verified."
gff-hab notify -level time-sensitive -session <session-id> "Deploy needs you"
```

`-level` is `passive`, `active` (the default) or `time-sensitive`. The title keeps to 8 words
and the body to 16; longer text is shortened with "…". The hab sends a `notice.raised` event to
open clients, and a push to each registered device that is not in use right now.

## Round notifications

Rounds (`docs/specs/rounds.md` 10.2) raise their notices the same way, with push kind `round`:
only a device that names `round` in `push.register` gets the push. One round's notifications
share one collapse id, so a failing round shows one banner, updated. The banner says the
round's name and what happened: a run failed or timed out, succeeded (with `notify = always`),
the round paused itself, did not run for 5 slots, or completed because its target session is
gone. A banner about a run opens its session. A round's result is never in the push; it is in
the kept notice that `notice.get` and "Ask an agent" read, where the read scope allows it.

## What a push holds

A push passes through Apple. In this release its text is plain: the title, the project and hab,
and what the agent asks for, shortened. That is a command, file names, the start of a plan, a
question and its options, or a fetched URL. The app's setting "Show details in notifications"
turns all of that off: banners then say "Approval needed" (or "Question waiting") and offer no
Approve. Sealed payloads are a later step (`app.md` 18.9).
