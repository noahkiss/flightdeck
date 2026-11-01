# macOS code identity and permission grants: behaviour spec

Status: draft, from the Part 1 study step of the signing task. Paper study, written on Linux.
Every claim marked **needs Mac proof** has not been confirmed on current macOS. Open decisions are
marked **Decision: open**.

This spec says which code identity each Flightdeck process carries on macOS, which permission
grants each one needs, how releases are signed, and how one built-in command (`gff setup`) checks
and requests the grants. It states behaviour in our own words. No code from the reference projects
is copied here, and none may be copied into the implementation.

It builds on `hab-holder.md` 4.3 (drain), 4.4 (binary copies) and 5.3 (launchd agents, the
responsible process). It answers `hab-holder.md` Q2 and adds to the stage 6 permission check.

Placeholders used throughout:

| Placeholder | Meaning |
|---|---|
| `<prefix>` | The reverse-DNS prefix for bundle identifiers and launchd labels. Comes from the build configuration, never baked into the source |
| `<team-id>` | The Apple Developer team id of whoever signs the release |
| `<identity>` | The signing identity name, `Developer ID Application: <name> (<team-id>)` |
| `<state>` | The GFF state directory (`hab-holder.md` 5.3) |
| `<uid>` | The user's numeric id |

Credential storage for signing (the certificate, its password, the notary API key) is a CI secret
store. Which store, and the item names, stay out of this repo.

Contents:

1. [References](#1-references)
2. [How macOS decides](#2-how-macos-decides)
3. [Identity per process](#3-identity-per-process)
4. [Grants per process](#4-grants-per-process)
5. [Signing pipeline](#5-signing-pipeline)
6. [Install layout and drain](#6-install-layout-and-drain)
7. [The `gff setup` command](#7-the-gff-setup-command)
8. [Proof steps on a Mac](#8-proof-steps-on-a-mac)
9. [Open questions](#9-open-questions)
10. [Proposed change to hab-holder.md](#10-proposed-change-to-hab-holdermd)

---

## 1. References

### 1.1 The zellij fork (studied, not copied)

The zellij fork (the earlier GFF terminal layer) runs its server as a launchd agent and has met
every problem in this spec in practice. What it found:

- macOS charges a pane's protected access to the server binary, the **responsible process**, not to
  the shell or the program the user ran. The denial record names the server's path.
- For a program that is not in a bundle, TCC keys the grant on the **absolute path** plus a
  recorded **code requirement**. A symlink never holds a grant: TCC records the resolved target.
  The grant survived a `tccd` restart and a reboot, so it is not cached state.
- An unsigned or ad-hoc-signed binary has a designated requirement that is its own code hash
  (`cdhash H"..."`). Every new build voids every grant, silently.
- A Developer ID signature with a fixed identifier gives a requirement that names the identifier
  and the team. Every later release at the same path satisfies it, so grants survive upgrades.
- Changing the identifier, the team, or the kind of certificate (Developer ID versus Apple
  Development) changes the requirement. Every grant is lost. The fork records such a switch as
  "re-grant owed" and keeps asking until the user confirms the re-grant.
- Writing a new build over a running signed binary in place broke the next launch: the kernel kept
  the old code hash for that file and killed the process for a code-signing failure, while
  `codesign --verify` called the file valid. Write a temp file, then rename it into place.
- `codesign -v` returned 0 on a binary that failed its designated requirement. Only
  `codesign --verify --strict --verbose=2` reported it. A valid signature says nothing about
  whether a grant survived; compare the requirement text.
- The fork first signed builds on each Mac with whatever identity the keychain held. That needed
  the login keychain (locked over SSH), and mixed teams dropped grants. It now signs once, in CI,
  with Developer ID, and nothing on a Mac signs. A local build holds no grant, and the fork says so.
- Full Disk Access cannot be requested by any API, and attempting access did not reliably add the
  program to the Settings list. The user adds the exact path by hand.
- Files and Folders (Desktop, Documents, Downloads) prompts once. A dismissed prompt is a permanent,
  silent denial until the entry is removed. The fork opens each protected folder once at server
  start, on its own thread (the open blocks until the user answers), so the prompt appears while
  the user is looking at the upgrade, not weeks later in an unrelated program.
- The Full Disk Access check is an `open` of `/Library/Application Support/com.apple.TCC`.
  `access(2)` reads only permission bits and answers "readable" without a grant. The per-user
  `TCC.db` is gone on the newest macOS. A grant answers at once; a refusal took about 5.5 s, so a
  check that has not answered in 10 s reads as "not granted".
- Checks must run **inside a pane**. A check run from the user's terminal is judged against the
  terminal's grants.
- The fork's notes disagree on one point: one says a Full Disk Access toggle reaches a running
  server; its setup script says a grant reaches only a server started after it. **Needs Mac
  proof** (8, step P4).
- The fork's grants are Full Disk Access, Accessibility and Screen Recording, all on the server.
- The notary service takes a zip, never a bare Mach-O, and a ticket cannot be stapled to a bare
  binary. The fork's CI prints `spctl`'s verdict but gates only on `codesign`.

### 1.2 Terminal apps

A terminal app is the responsible process for everything its shells run, which is the holder's
position too. Public terminal projects (Ghostty, Alacritty) ship with:

- hardened runtime entitlements for Apple events, audio input, camera, contacts, calendars,
  location and photos, so that a program in a shell can ask for them;
- usage description strings for each of those in the app's `Info.plist`, including local network.

They carry these because a program in a shell asks under the terminal's identity. A responsible
process with hardened runtime and no matching entitlement or usage string cannot pass the request
on. The holder needs the same treatment (4.2).

### 1.3 Apple documentation relied on

| Topic | Source |
|---|---|
| Designated requirements; Developer ID default DR; ad-hoc DR is a cdhash; TCC remembers code by its DR | TN3127, <https://developer.apple.com/documentation/technotes/tn3127-inside-code-signing-requirements> |
| Signing order inside out; no `--deep`; `-i` for non-bundled code; `--timestamp`; `-o runtime` | <https://developer.apple.com/documentation/xcode/creating-distribution-signed-code-for-the-mac> |
| An app-like bundle as a launchd job | <https://developer.apple.com/documentation/xcode/signing-a-daemon-with-a-restricted-entitlement> |
| Where nested code goes in a bundle | <https://developer.apple.com/documentation/bundleresources/placing-content-in-a-bundle> |
| Hardened runtime | <https://developer.apple.com/documentation/security/hardened-runtime> |
| Apple events entitlement | <https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.security.automation.apple-events> |
| `NSAppleEventsUsageDescription` | <https://developer.apple.com/documentation/bundleresources/information-property-list/nsappleeventsusagedescription> |
| `LSUIElement` (no Dock icon) | <https://developer.apple.com/documentation/bundleresources/information-property-list/lsuielement> |
| Notarization; zip, dmg and pkg accepted; tickets cannot be stapled to standalone binaries | <https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution>, <https://developer.apple.com/documentation/security/customizing-the-notarization-workflow> |
| `notarytool` | TN3147, <https://developer.apple.com/documentation/technotes/tn3147-migrating-to-the-latest-notarization-tool> |
| Local network privacy: launch agents are not exempt; `AssociatedBundleIdentifiers` | TN3179, <https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy> |
| `SMAppService` (macOS 13+) for agents inside an app bundle | <https://developer.apple.com/documentation/servicemanagement/smappservice> |
| `AXIsProcessTrusted` (no prompt) | <https://developer.apple.com/documentation/applicationservices/1460720-axisprocesstrusted> |
| `CGPreflightScreenCaptureAccess` (no prompt) | <https://developer.apple.com/documentation/coregraphics/cgpreflightscreencaptureaccess()> |
| Pasteboard access behaviour, macOS 15.4+ | <https://developer.apple.com/documentation/appkit/nspasteboard/accessbehavior-swift.enum/default> |
| Notification settings (needs a bundle) | <https://developer.apple.com/documentation/usernotifications/unusernotificationcenter> |

Not from Apple, and so **needs Mac proof**:

- The System Settings URLs (`x-apple.systempreferences:...?Privacy_AllFiles` and the rest). Apple
  does not document them. Community lists:
  <https://github.com/bvanpeski/SystemPreferences/blob/main/macos_preferencepanes-Ventura.md>.
- `AEDeterminePermissionToAutomateTarget` with `askUserIfNeeded` false returns -1744 both when no
  decision exists and after a denial, and -600 when the target is not running:
  <https://mjtsai.com/blog/2018/08/31/aedeterminepermissiontoautomatetarget-added-but-aepocalyse-still-looms/>.
- The macOS 15.4 pasteboard privacy preview and its Settings entry "Paste from Other Apps":
  <https://mjtsai.com/blog/2025/05/12/pasteboard-privacy-preview-in-macos-15-4/>.

---

## 2. How macOS decides

These rules hold for every section below.

1. **Domain.** Only a process in the graphical `Aqua` domain reaches TCC-gated resources, the
   pasteboard and notifications (`hab-holder.md` 5.3). The holder and gff-hab are launch agents in
   `gui/<uid>`.
2. **Responsible process.** A protected access is charged to the responsible process. launchd
   starts the holder, so the holder is responsible for every program in its panes. launchd starts
   gff-hab, so gff-hab is responsible for what it runs itself (git, ssh). A program the user runs
   in a terminal app is charged to that terminal app.
3. **Client key.** TCC records a grant against a client and a code requirement:
   - a **bundled** program is keyed by its bundle identifier;
   - a **non-bundled** program is keyed by its absolute, resolved path.
   A later process matches the grant only if it satisfies the recorded requirement.
4. **Requirement.** A Developer ID signature with a fixed identifier yields a requirement that
   every later build of the same identifier and team satisfies. An ad-hoc signature yields a code
   hash that only that exact build satisfies. The Go linker ad-hoc signs every arm64 binary it
   writes, so an unsigned local build is in practice an ad-hoc build.
5. **Silent failure.** A grant that no longer matches is not reported. The next protected access
   either prompts from a background process, at a random moment, or is denied silently.

---

## 3. Identity per process

### 3.1 Options

| Option | How | Drained older holder | Local network, Apple events, camera | Cost |
|---|---|---|---|---|
| A. Helper bundles with fixed bundle ids | Each long-running process is a minimal app-like bundle (`Info.plist`, `Contents/MacOS/<exe>`), signed with Developer ID and a fixed identifier | Keeps every grant: TCC keys on the bundle id and the team requirement, so a copy at any path matches (**needs Mac proof**, P3) | The bundle's `Info.plist` carries the usage strings | Build step to assemble bundles; signing in every release |
| B. Standalone signed binaries at pinned paths | Bare Mach-O signed with Developer ID and `-i <prefix>.<name>`, installed at one fixed path per process, replaced by rename | Loses grants: a drained build runs from its own copy (`hab-holder.md` 4.4), which is a different path and so a different client. Pinning one path for every build would make drain overwrite a running holder | Needs an `Info.plist` embedded in the binary's `__TEXT,__info_plist` section; TN3179 allows this for local network, other services **need Mac proof** | No bundles; same signing |
| C. Both: bundles for the holder and gff-hab, a bare binary for `gff` | A for processes that are responsible for anything; B-style signing for the terminal client | As A | As A | As A, plus one bare binary |

### 3.2 Recommendation: option C

| Process | Identity | Form | Why |
|---|---|---|---|
| gff-hab-holder | Bundle id `<prefix>.hab-holder` | Helper bundle `GFF Holder.app`, executable `Contents/MacOS/gff-hab-holder` | Responsible for every pane. Several builds run at once during drain, each from its own directory. Only a bundle id lets them share grants |
| gff-hab | Bundle id `<prefix>.hab` | Helper bundle `GFF Hab.app`, executable `Contents/MacOS/gff-hab` | Responsible for its own git and ssh children: Files and Folders for projects in protected folders, Local Network for hab-to-hab ssh. Restarts at every update; a bundle id keeps its grants whatever path the update uses |
| gff | Identifier `<prefix>.gff` | Bare binary, Developer ID, hardened runtime, notarized | Runs from the user's terminal, so the terminal is responsible. Needs no TCC identity of its own. Signing is only for Gatekeeper |
| Flightdeck Mac app | Bundle id `<prefix>.flightdeck` | App bundle, Developer ID, notarized and stapled (`app.md` 1) | Its own grants: notifications, Local Network for SSH to habs on the LAN. Embeds the two helper bundles (3.4) |

Why not B: drain and path keys fight. Drain keeps an older holder build running from its own copy
for days. Under B that copy is a client TCC has never seen, so any new grant it needs prompts
from a background process, and a dismissed prompt is a permanent denial. Under A both copies are
one client.

Why not `SMAppService`: it ties each agent to a plist inside one app bundle, at the app's path,
with one label. Drain needs a label per holder instance and a binary copy per build
(`hab-holder.md` 4.4, 5.4). The hab must also install without the Mac app (Homebrew or a tarball).
So we keep plain plists in the user's LaunchAgents directory, and set
`AssociatedBundleIdentifiers` in each plist so macOS attributes them (TN3179). Q3 decides no
`SMAppService` for now; revisit when the Mac app lands.

### 3.3 What a bundle holds

```
GFF Holder.app/
  Contents/
    Info.plist                 CFBundleIdentifier <prefix>.hab-holder, CFBundleExecutable,
                               CFBundleShortVersionString, CFBundleVersion, LSUIElement true,
                               usage strings (4.2)
    MacOS/gff-hab-holder       the Go binary, unchanged
    _CodeSignature/CodeResources
```

- No resources, no icon, no frameworks. The Go binary is the same one Linux builds produce for
  darwin, wrapped.
- `LSUIElement` keeps it out of the Dock if anything ever touches AppKit. The holder does not.
- The bundle is never written after signing. Any change breaks the seal.
- The executable inside the bundle runs as a plain command too. The hook command
  (`gff-hab hook`) and `gff-hab install` run from `GFF Hab.app/Contents/MacOS/gff-hab` through a
  symlink on `PATH`. A symlink is fine for a CLI: it needs no TCC identity.

### 3.4 Where the bundles come from

Two channels ship byte-identical, identically signed bundles:

1. A release archive for the hab: `GFF Holder.app`, `GFF Hab.app` and the `gff` binary. Homebrew
   installs from it.
2. The Mac app embeds the same two bundles in `Contents/Helpers/`.

Either way, `gff-hab install` copies the bundles into the per-build directories of 6.1 and runs
them from there. The launchd plist never names a path inside a package manager's directory or
inside the Mac app. One bundle id and one requirement means a grant made for a copy from either
channel covers both (**needs Mac proof**, P3).

### 3.5 An unsigned local dev build

A local `go build` is ad-hoc signed. Its requirement is a code hash. Rules:

- A dev build uses **different bundle ids**: `<prefix>.hab-holder.dev` and `<prefix>.hab.dev`.
  It never shares an id with a release. A dev build granting itself under the release id would
  overwrite the release's recorded requirement, and the release would lose the grant.
- A dev build holds grants only until its next rebuild. `gff setup --check` reports this as its
  own line (7.4) instead of a vague failure.
- A developer with an Apple Development or Developer ID identity may set `GFF_DEV_SIGN_IDENTITY`.
  `make` then signs the dev bundles with it, still under the `.dev` ids. Its requirement names the
  team, so dev grants survive rebuilds. Nothing signs by default, and nothing in the build reads a
  keychain unless that variable is set.
- On Linux none of this applies. `make` builds bare binaries as today.

---

## 4. Grants per process

### 4.1 Table

"Required" means `gff setup --check` fails without it. "Opt-in" means the user turns it on in the
Flightdeck config (`macos.grants`), and only then does `--check` require it.

| Grant (TCC service) | Holder | gff-hab | gff | Mac app | Why |
|---|---|---|---|---|---|
| Graphical domain (`Aqua`) | Required | Required | — | — | Without it no grant below can apply (`hab-holder.md` 5.3) |
| Background item allowed (Login Items) | Required | Required | — | — | macOS 13+ lists every LaunchAgents plist; the user can switch it off, and then the job does not load (**needs Mac proof**) |
| Full Disk Access (`SystemPolicyAllFiles`) | Required | Required if a project lives in a protected location, else not needed | — | — | Agents read and write anywhere the user works. Covers every Files and Folders location, so no per-folder prompt can land later |
| Files and Folders (Desktop, Documents, Downloads, removable and network volumes) | Covered by Full Disk Access | Covered by Full Disk Access, or the single folder if the user declines FDA | — | — | Promptable; a dismissed prompt is a permanent denial (1.1) |
| Local Network | Required | Required when a peer hab or project remote is on the LAN | — | Required when a hab is on the LAN | Launch agents are not exempt (TN3179). Pane programs (ssh, curl to a LAN host) and gff-hab's ssh to peer habs ask under the holder or gff-hab |
| Accessibility | Opt-in | — | — | — | Agents that drive the UI (computer use, UI scripting) |
| Screen Recording | Opt-in | — | — | — | Agents that take screenshots |
| Automation (Apple events), per target app | Opt-in, per target | — | — | — | Scripts that tell another app to act. Per pair, so there is no single grant to check |
| Developer Tools | Opt-in | — | — | — | Lets programs in panes skip the Gatekeeper scan of freshly built binaries, as terminal apps do (**needs Mac proof**) |
| Notifications | — | — | — | Required for local alerts | Flightdeck posts notifications from the app only. A bare process cannot use `UNUserNotificationCenter` |
| Pasteboard | None today | — | — | — | `pbcopy` and `pbpaste` in a pane work in the `Aqua` domain with no grant. macOS 15.4 added a pasteboard access behaviour whose default is "ask" for programmatic reads; if a later macOS enforces it, pane reads ask under the holder (**needs Mac proof**, P5) |
| Camera, microphone, contacts, calendars, location, photos | Passed through | — | — | Microphone for voice mode (`voice.md`) | The holder carries the entitlements and usage strings (4.2) so a pane program can ask. `--check` does not require them |

`gff` needs nothing: its responsible process is the user's terminal app. `gff setup` therefore
never checks `gff`'s own grants. It asks gff-hab to check from inside a pane (7.2).

### 4.2 Entitlements and usage strings

Hardened runtime blocks a request the signed code has no entitlement for, and the request of a
child is made under the responsible process (1.2). So:

| Bundle | Hardened runtime entitlements | `Info.plist` usage strings |
|---|---|---|
| GFF Holder.app | `com.apple.security.automation.apple-events`, `com.apple.security.device.audio-input`, `com.apple.security.device.camera`, `com.apple.security.personal-information.addressbook`, `...calendars`, `...location`, `...photos-library` | `NSAppleEventsUsageDescription`, `NSLocalNetworkUsageDescription`, `NSMicrophoneUsageDescription`, `NSCameraUsageDescription`, `NSContactsUsageDescription`, `NSCalendarsUsageDescription`, `NSRemindersUsageDescription`, `NSLocationUsageDescription`, `NSPhotoLibraryUsageDescription`. Each says "A program running in a Flightdeck pane wants to ..." |
| GFF Hab.app | None | `NSLocalNetworkUsageDescription` |
| Flightdeck.app | `com.apple.security.device.audio-input` (voice mode) | `NSLocalNetworkUsageDescription`, `NSMicrophoneUsageDescription`, `NSSpeechRecognitionUsageDescription` if voice uses on-device speech |
| gff | None | None |

No bundle needs `cs.allow-jit`, `cs.allow-unsigned-executable-memory` or
`cs.disable-library-validation`. Go needs none of them. libghostty-vt is linked statically. A
`purego` call into a system framework (7.3) loads Apple-signed code, which library validation
allows. **Needs Mac proof** (P1): a signed, hardened gff-hab with libghostty runs.

The usage strings are UI text. They say what the request is for, nothing more.

---

## 5. Signing pipeline

### 5.1 What gets signed, in order

Release builds sign on a macOS CI runner, once. Nothing on a user's Mac signs anything (1.1).

1. `gff-hab-holder`, `gff-hab` and `gff` are built for `darwin/arm64` with `-tags ghostty`.
2. The holder and gff-hab binaries are placed into their bundles, with `Info.plist` filled in from
   the build (`<prefix>`, version).
3. Sign each helper bundle (inside out, no `--deep`):
   ```
   codesign --force --timestamp --options runtime \
     --entitlements holder.entitlements --sign "<identity>" "GFF Holder.app"
   codesign --force --timestamp --options runtime \
     --sign "<identity>" "GFF Hab.app"
   ```
   The identifier comes from `CFBundleIdentifier`. It is a constant per bundle: changing it voids
   every grant on every Mac.
4. Sign the bare client with an explicit identifier:
   ```
   codesign --force --timestamp --options runtime -i <prefix>.gff --sign "<identity>" gff
   ```
5. The Mac app (Xcode archive, later): copy the two signed helper bundles into
   `Flightdeck.app/Contents/Helpers/` unchanged, then sign the app last. Do not re-sign the helpers.

The default designated requirement is kept. For Developer ID it reads:

```
designated => identifier "<prefix>.hab-holder" and anchor apple generic
  and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */
  and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */
  and certificate leaf[subject.OU] = "<team-id>"
```

CI compares each signed item's `codesign -d -r-` output with this template, filled in. A
difference fails the build: a changed requirement is a release that silently drops every grant.

### 5.2 Notarization and stapling

| Item | Submit | Staple | Offline first run |
|---|---|---|---|
| `GFF Holder.app`, `GFF Hab.app` | In one zip (`ditto -c -k --keepParent`) | Yes, each bundle, then re-zip or tar for distribution | Works |
| `gff` | In the same zip | Not possible for a bare binary | Gatekeeper fetches the ticket online once. A binary fetched by a tool that sets no quarantine flag (`curl`, a Homebrew formula) is not assessed at all |
| `Flightdeck.app` | zip or dmg | Yes | Works |

```
xcrun notarytool submit hab-macos.zip --key <key.p8> --key-id <id> --issuer <issuer> \
  --wait --output-format json        # gate on status "Accepted"
xcrun stapler staple "GFF Holder.app"
xcrun stapler staple "GFF Hab.app"
```

On a non-Accepted status, CI prints `notarytool log <id>` and fails.

### 5.3 Verification

Run in CI after signing, and again by `gff setup --check` on the installed copies:

| Check | Command | Pass |
|---|---|---|
| Signature and seal intact, DR satisfied | `codesign --verify --strict --verbose=2 <item>` | Exit 0 and no "does not satisfy its designated Requirement" |
| Requirement is the release one | `codesign -d -r- <item>` | Equals the 5.1 template |
| Team and runtime | `codesign -dv --verbose=4 <item>` | `TeamIdentifier=<team-id>`, `flags=0x10000(runtime)` |
| Stated requirement, explicit | `codesign --verify -R='<template>' <item>` | Exit 0 |
| Gatekeeper, bundles | `spctl --assess --type execute -vv <bundle>` | `accepted`, `source=Notarized Developer ID` |
| Gatekeeper, bare `gff` | `spctl --assess --type install -vv gff` | Printed, not gated (1.1) |
| Stapled ticket | `xcrun stapler validate <bundle>` | "The validate action worked" |

`codesign --verify` alone passing proves nothing about grants (1.1). The requirement check is what
answers "will the grants still match".

---

## 6. Install layout and drain

### 6.1 Layout

```
<state>/builds/<build-id>/GFF Holder.app     one copy per holder build (hab-holder.md 4.4)
<state>/builds/<build-id>/GFF Hab.app        the gff-hab bundle of that build
<state>/current -> builds/<build-id>          used only by the CLI symlink, never by launchd
```

- `gff-hab install` copies a bundle with `ditto` into a temp directory beside its target, verifies
  it (5.3, first four rows), then renames it into place. It never writes into an existing bundle
  (1.1: in-place writes broke launch).
- The holder plist and the gff-hab plist name the executable inside the per-build bundle, by its
  real path, never through `current`.
- Each plist sets `AssociatedBundleIdentifiers` to its own bundle id, and to `<prefix>.flightdeck`
  when the Mac app is installed.
- A build directory is removed only when no job names it and no process runs from it
  (`hab-holder.md` 4.3 step 7).

### 6.2 Drain keeps grants

Under option A, holder builds H1 and H2 run from two directories with one bundle id and one
requirement. TCC sees one client. A grant given while H1 ran covers H2, and the reverse. Nothing
re-prompts at an update. This is the promise P3 must prove.

### 6.3 A fresh grant and a running holder

If a grant does not reach a running process (1.1 open point; P4), a holder started before the grant
never gets it, and restarting it would end every pane. Drain answers this without loss:
`gff setup` asks gff-hab to start a new holder instance of the **same** build and drain the old
one. New panes get the grant; old panes keep running and finish on the old holder. The setup output
says so.

### 6.4 Requirement change (a re-grant owed)

If a release ever changes the requirement (new team, new identifier, a certificate of another
kind), every grant is lost. CI refuses that by default (5.1). If it is done on purpose, `gff-hab
install` records the old requirement in `<state>/macos/regrant-owed`. `gff setup --check` reports
"re-grant owed" until a probe sees every required grant again under the new requirement. That
probe is the proof; no flag is needed to clear it.

---

## 7. The `gff setup` command

### 7.1 Shape

```
gff setup [--check] [--host <hab>] [--grant <name>]... [--timeout 5m] [--no-open]
```

- `gff setup` walks every required grant (4.1) that is missing, one at a time: say what is
  missing and why, open the exact Settings pane, wait, verify, go on.
- `gff setup --check` only reports. It never opens Settings and never causes a prompt.
- `--grant` limits the walk to named grants (`full-disk-access`, `local-network`,
  `accessibility`, `screen-recording`, `automation:<bundle-id>`, `developer-tools`).
- `--host` runs it against another Mac's hab over the normal transport. Opening Settings then
  happens on that Mac, so only `--check` makes sense remotely; the walk says so and stops.
- No dotfile or shell profile step. It touches only what `gff-hab install` owns.
- On Linux it checks lingering (`hab-holder.md` 5.2) and the systemd units, and prints
  "macOS grants: not applicable".

### 7.2 Where the checks run

A check is about the process that will be responsible, so it must run as that process's child:

- **Holder checks** run in a hidden probe pane, spawned through gff-hab in **every running holder**
  (current and draining). The probe program is `gff-hab probe`, so the probe's code is in the hab
  build, but its responsible process is the holder.
- **gff-hab checks** run in gff-hab itself, on their own goroutine with a deadline.
- gff-hab exposes the result as one API method (a later revision of `api/`; name to be set there,
  for example `hab.permissions`). `gff setup` and the Mac app both call it. Neither checks
  anything in its own process.

### 7.3 How each grant is checked

"No prompt" means the check never shows a dialog. Each check returns `granted`, `denied`,
`unknown` or `unanswered`.

| Grant | Check | No prompt? | Settings pane opened by the walk |
|---|---|---|---|
| Domain | `launchctl managername` in the probe prints `Aqua` | Yes | None; tell the user to log in graphically (`hab-holder.md` Q5) |
| Background item | `launchctl print gui/<uid>/<label>` succeeds for every job | Yes | `x-apple.systempreferences:com.apple.LoginItems-Settings.extension` (**needs Mac proof**) |
| Full Disk Access | `opendir("/Library/Application Support/com.apple.TCC")`; 10 s deadline; no answer reads `unanswered`, counted as missing | Yes | `x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles` |
| Files and Folders | If FDA is granted: `granted` without probing. Else **cannot be checked without risking a prompt**: an `open` of an undecided folder raises the dialog. `--check` reports `unknown`. The walk probes each folder on its own thread while the user watches | **No** | `...?Privacy_FilesAndFolders` |
| Local Network | No preflight API is known. `--check` reports `unknown` unless a connection gff-hab made to a LAN address has succeeded or failed with the local-network error since start. The walk makes one connection to a LAN peer (or the gateway) from the probe and keeps the probe alive 30 s, because a short-lived process gets no alert (TN3179) | **No** | `...?Privacy_LocalNetwork` (**needs Mac proof**) |
| Accessibility | `AXIsProcessTrusted()` in the probe | Yes | `...?Privacy_Accessibility`. The walk first calls `AXIsProcessTrustedWithOptions` with the prompt option once, which lists the holder in the pane, so the user only toggles it |
| Screen Recording | `CGPreflightScreenCaptureAccess()` in the probe | Yes | `...?Privacy_ScreenCapture`. The walk first calls `CGRequestScreenCaptureAccess()` once, which lists the holder |
| Automation | `AEDeterminePermissionToAutomateTarget(target, any, any, false)` per configured target. -1744 reads `unknown` (it means undecided or denied), -600 reads `unknown` (target not running) | Yes, but cannot tell undecided from denied | `...?Privacy_Automation` |
| Developer Tools | No API known. `--check` reports `unknown` | — | `...?Privacy_DevTools` (**needs Mac proof**) |
| Pasteboard | `pbcopy` then `pbpaste` of a random token in the probe | Yes today; **needs Mac proof** on current macOS (P5) | `...?Privacy_Pasteboard` if it exists |
| Signature | 5.3 rows 1 to 4 on each installed bundle | Yes | None; tell the user to reinstall the release |
| Mac app notifications | `UNUserNotificationCenter.getNotificationSettings` in the app | Yes | `x-apple.systempreferences:com.apple.Notifications-Settings.extension?id=<prefix>.flightdeck` (**needs Mac proof**) |

The C calls (`AXIsProcessTrusted`, `CGPreflightScreenCaptureAccess`,
`AEDeterminePermissionToAutomateTarget`) need either cgo or a dlopen-based call. `make check-pure`
allows cgo only in `internal/vt/ghostty`. Recommendation: a darwin-only package using `purego` to
call the system frameworks, so no new cgo package appears. See Q4.

Every Settings URL is opened with `open <url>`. If `open` fails or the pane does not exist, the
walk prints the menu path in words ("System Settings > Privacy & Security > Full Disk Access")
instead.

### 7.4 What the walk shows for Full Disk Access

Full Disk Access cannot be requested, and the holder may not be in the list. The walk:

1. Opens the Full Disk Access pane.
2. Runs `open -R "<state>/builds/<build-id>/GFF Holder.app"`, which shows the bundle selected in
   Finder, so the user can drag it into the list. It also prints the path for Cmd-Shift-G.
3. Waits (7.5). When the probe answers `granted`, it says so and goes on.
4. If the probe in a **new** holder answers `granted` and the old holder's probe does not, it
   starts the drain of 6.3 and says why.

The same steps apply to gff-hab when its Full Disk Access is required.

### 7.5 Waiting and verifying

- After opening a pane, the walk re-runs that one check every 2 s, up to `--timeout` (default
  5 min). It prints one line when it starts waiting and one when the state changes. Enter re-checks
  at once; `s` skips this grant; Ctrl-C stops the walk and prints the summary so far.
- A grant counts as done only when the probe in a pane says so. A toggle the user reports is not
  proof.
- At the end the walk runs the full `--check` and prints its output.

### 7.6 `--check` output and exit codes

One line per grant and process, then a summary. Plain text; `--json` gives the same rows for
scripts. Example:

```
host          this Mac (macOS 27.1, Aqua)
holder  sig   ok      <prefix>.hab-holder, team <team-id>, Developer ID, notarized
holder  fda   ok
holder  lan   unknown no local connection seen yet; run gff setup to check
holder  ax    off     not turned on in macos.grants
hab     sig   ok      <prefix>.hab, team <team-id>, Developer ID, notarized
hab     fda   missing a project is in ~/Documents; gff setup opens Full Disk Access
holder  dev   note    dev build: grants last until the next rebuild

1 missing, 1 unknown. Run `gff setup` to fix.
```

| Exit | Meaning |
|---|---|
| 0 | Every required grant is `granted` on every running holder and on gff-hab, and every signature verifies. `unknown` on an opt-in grant does not fail |
| 1 | At least one required grant is `denied`, `unanswered` or missing, a signature fails, or a re-grant is owed. Each one is named |
| 2 | The check could not run: no hab, no `Aqua` domain, or the probe pane did not start within 5 s |
| 64 | Usage error |

`unknown` on a **required** grant (Local Network, Files and Folders without FDA) fails `--check`
with exit 1, because a check that could not clear the machine must not pass it (1.1). The line
says it is unknown, not denied. Testing smoke 12 (`docs/testing.md`) expects exit 0, so the smoke
script runs the walk once before the check.

### 7.7 Idempotence

- Running `gff setup` on a set-up Mac opens nothing and changes nothing. It prints the `--check`
  output and exits 0.
- It never removes a TCC entry (`tccutil reset`). If a Files and Folders entry is denied, it says
  that only the user can remove it, and opens the pane.
- It writes only `<state>/macos/` records (the re-grant record of 6.4, and the time of the last
  full pass).

### 7.8 The same flow from the Mac app

- A "Permissions" screen lists the same rows as `--check`, from the same API method.
- Each missing row has one button that opens the same Settings pane and, for Full Disk Access,
  reveals the bundle in Finder. The screen re-checks every 2 s while it is visible.
- The app checks its own notification and local network grants in its own process; it is
  responsible for itself.
- The app's first-run screen runs this walk once. The app never edits TCC itself.

---

## 8. Proof steps on a Mac

These extend the three steps of `hab-holder.md` 5.3. Each needs a Mac with Xcode. Column
"Needs" names what the coordinator must approve: **DevID** means signing with the owner's
Developer ID (in CI or locally); **TCC** means granting, denying or resetting a privacy entry on
that Mac (`tccutil reset` included); **Account** means creating a fresh macOS user account.

| # | Step | Pass | Needs |
|---|---|---|---|
| P0 | `launchctl managername` in a holder pane prints `Aqua`; over SSH it prints `Background` (5.3 step 1) | As stated | — |
| P1 | Build, wrap and sign both bundles and `gff` per 5.1; notarize and staple per 5.2; run every 5.3 row | All pass; a hardened gff-hab with libghostty starts and serves | DevID |
| P2 | Pasteboard round trip in a holder pane (5.3 step 2) | Same text back, no prompt | — |
| P3 | Grant Desktop (or FDA) to holder build A. Start build B from another directory with the same bundle id; drain A to B. List `~/Desktop` from a B pane and from an A pane (5.3 step 3, now with bundles). Then repeat with a copy of the bundle that came from the other channel (3.4) | Both list with no prompt; System Settings shows one entry | DevID, TCC |
| P4 | Revoke FDA for the holder while it runs, then grant it again. Probe from an existing pane, then from a new pane, then from a new holder instance of the same build | Records whether a toggle reaches a running process; 6.3 depends on the answer | TCC |
| P5 | On the newest macOS, read the pasteboard from a pane with no user action (`pbpaste`); check for a "Paste from Other Apps" entry | Records whether a prompt appears and under which identity | TCC (to reset the pasteboard entry after) |
| P6 | From a holder pane, `ssh` to a LAN host; from gff-hab, connect to a LAN peer hab | Records whether a Local Network alert appears, under which name, and that `AssociatedBundleIdentifiers` names it | TCC |
| P7 | Run each 7.3 check on a Mac with no decisions recorded, and confirm which ones show a dialog | Only the rows marked "No" may prompt | TCC, Account |
| P8 | Open each Settings URL in 7.3 | Each opens the named pane; record the working form | — |
| P9 | Switch off the hab in Login Items, then run `--check` | Background item reads `missing`, exit 1 | TCC (Login Items is a background-task setting, not a TCC entry, but it changes the Mac's privacy state) |
| P10 | Install a dev build (ad-hoc, `.dev` ids) next to the release; grant it; rebuild; check both | Release grants unchanged; dev grant lost after rebuild; `--check` prints the dev note | TCC |
| P11 | Run `gff setup` from start to finish on a fresh user account (task acceptance) | Ends with `--check` exit 0; a pane reads a protected folder and the clipboard with no prompt | DevID, TCC, Account |
| P12 | Update to a second signed release (smoke 12) | No prompt; `--check` exit 0; the drained holder's panes still read the protected folder | DevID |

Steps that need nothing (P0, P2, P8) can run at any time. The others wait for approval.

---

## 9. Open questions

Q2 to Q5 are decided by the coordinator. Q1 and Q6 widen what the holder can reach, so they
wait for the owner.

### Q1. Holder Full Disk Access by default. **Decision: open (owner)**

Full Disk Access is broad. The alternative is per-folder Files and Folders grants. Recommendation:
require FDA for the holder. Agents work anywhere, every terminal user grants FDA to their terminal
for the same reason, and per-folder grants leave a silent permanent denial waiting in every folder
nobody has opened yet (1.1). A user who declines can set `macos.grants` to drop it; `--check` then
checks the listed folders instead.

### Q2. Local Network as a required grant. **Decision: required for the holder; for gff-hab only with a peer hab (coordinator)**

TN3179 says launch agents are not exempt. If P6 confirms it, every pane `ssh` to a LAN host fails
until the holder holds the grant. Recommendation: required for the holder; required for gff-hab
only when a peer hab is configured. `--check` cannot verify it without a connection, so the walk
makes one.

### Q3. `SMAppService` for the Mac app. **Decision: no `SMAppService` for now (coordinator)**

Recommendation: no, for now. Drain needs a label per holder instance and a copy per build, and the
hab installs without the app. Revisit if macOS starts to treat LaunchAgents plists outside
`SMAppService` as second-class (Login Items attribution already needs
`AssociatedBundleIdentifiers`).

### Q4. How the probe calls the C checks. **Decision: option A, `purego` (coordinator)**

| Option | For | Against |
|---|---|---|
| A. `purego` (dlopen, no cgo) in a darwin-only package | `make check-pure` stays as it is; pure Go cross-compiles | A new dependency; hand-written function signatures |
| B. cgo in a second allowed package | Plain and typed | Changes the cgo rule in AGENTS.md; needs a macOS toolchain to build darwin |
| C. Run `osascript -l JavaScript` with the ObjC bridge in the probe | No Go change | Slow, harder to test, depends on JXA staying available |

Recommendation: A.

### Q5. A Developer ID signature for local dev builds. **Decision: no (coordinator)**

Recommendation: no. Release builds sign in CI only; dev builds use `.dev` ids and may opt in to a
local identity through `GFF_DEV_SIGN_IDENTITY` (3.5). This matches what the zellij fork learned:
signing on each Mac needs an unlocked keychain and invites team mix-ups.

### Q6. The opt-in entitlements on the holder. **Decision: open (owner)**

Carrying camera, microphone, contacts and similar entitlements lets a pane program ask for them,
as in a terminal app. It also widens what the holder could be asked for. Recommendation: carry
them, as terminals do; the user still answers each prompt, and `--check` never requires them.

---

## 10. Proposed change to hab-holder.md

Proposed for the coordinator to apply. Replace the **Decision: open** block in 5.3 (from
"**Decision: open — how the holder keeps one TCC identity across builds.**" through
"Recommendation: **option 1**, ... before stage 5 relies on it.") and the body of Q2 with:

> **Decision: option 1, signed helper bundles; detail in `macos-identity.md`.** The holder ships as
> a helper bundle with the fixed bundle id `<prefix>.hab-holder`, and gff-hab as one with
> `<prefix>.hab`, both signed in CI with Developer ID, hardened runtime and a constant identifier,
> then notarized and stapled. Each holder build runs from its own bundle copy under the state
> directory (4.4); TCC keys bundled code on its bundle id and requirement, not its path, so a
> drained older build keeps every grant. Local dev builds use `.dev` bundle ids and hold grants
> only until their next rebuild. Path pinning (option 2) is dropped, because drain runs builds
> from different paths. Grants per process, the signing pipeline, `gff setup` and the Mac proof
> steps P0 to P12 are in `macos-identity.md`; stage 6 runs P0, P2 and P3 before stage 5 relies
> on this.

And change the Q2 heading to: `### Q2. TCC identity for holder builds on macOS. **Decision:
option 1 (spec), pending Mac proof P3**`.
