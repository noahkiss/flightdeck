# macOS code identity and permission grants: behaviour spec

Status: draft, from the Part 1 study step of the signing task. Paper study, written on Linux.
Every claim marked **needs Mac proof** has not been confirmed on current macOS. Every question in
section 9 is decided; the proof steps in section 8 are approved, and 8.1 records which ran.
Sections 3, 5 and 6 note what was built where it differs from this paper spec.

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
11. [Study: an optional Accessibility grant for setup](#11-study-an-optional-accessibility-grant-for-setup)

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

Built: `scripts/macos-bundle.sh` writes the bundles. Choices it made beyond the paper spec:

- `Info.plist` also carries `CFBundlePackageType` `APPL`, `CFBundleName` (`GFF Holder`,
  `GFF Hab`), `CFBundleInfoDictionaryVersion` `6.0`, `CFBundleDevelopmentRegion` `en` and
  `LSMinimumSystemVersion` `12.0`. `CFBundleShortVersionString` and `CFBundleVersion` are both
  the `-version` value (numbers and dots only).
- Each usage string says "A program running in a Flightdeck pane wants to ..." and names the
  resource, nothing else.
- The prefix comes from `$GFF_BUNDLE_PREFIX`, with no default. The script refuses a prefix that
  is not reverse-DNS.
- The bare `gff` is signed only when `cmd/gff` exists. It does not exist yet, so a release today
  holds the two bundles only.

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

Built: `scripts/macos-bundle.sh -dev` makes the dev bundles: `.dev` ids, ad-hoc signature with
hardened runtime and the holder's entitlements, no notarization. It honours
`GFF_DEV_SIGN_IDENTITY`; `make` does not call it yet. `gff-hab install` on macOS takes bundles
only, so a local build goes through this script first (`scripts/service-check.sh` does).

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

### 5.2.1 Built: `scripts/macos-bundle.sh`

The script runs 5.1 to 5.3 in one pass. Settings come from the environment only:
`GFF_BUNDLE_PREFIX`, `GFF_SIGN_IDENTITY`, `GFF_TEAM_ID`, `GFF_NOTARY_PROFILE`, and the optional
`GFF_SIGN_KEYCHAIN` and `GFF_NOTARY_KEYCHAIN`. Without `-bin-dir` it builds both binaries with
`-tags ghostty` against `make libghostty`. Choices that differ from the paper spec, or add to it:

- **The submission uses a `notarytool` keychain profile** (`--keychain-profile`), not the key
  file flags. The profile is stored once with `xcrun notarytool store-credentials`.
- **A keychain of its own for signing and notarizing.** Over SSH, and in a CI job, the login
  keychain is locked: `codesign` fails with `errSecInternalComponent` and `notarytool` with
  `keychainLocked` (P1). The proof imported the Developer ID `.p12` into a temporary keychain with
  a random password, ran `security set-key-partition-list` for `codesign`, stored the notary
  profile in the same keychain, passed both as `GFF_SIGN_KEYCHAIN` and `GFF_NOTARY_KEYCHAIN`, and
  deleted the keychain after. CI does the same.
- **The requirement comparison ignores quotes.** `codesign -d -r-` prints the team as a bare word
  (`certificate leaf[subject.OU] = <team-id>`), where the template quotes it. The script strips
  `"` from both sides before it compares.
- **`codesign --verify -R=<requirement>` takes the requirement text with no leading `=`.** A
  leading `=` is a syntax error on macOS 27.
- An ad-hoc dev signature uses `--timestamp=none`; a release uses `--timestamp`.
- The bundles are zipped from one folder, `gff-macos/`, so the notary sees the same layout the
  release archive has. After stapling, the script zips that folder again as `gff-macos.zip`.

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
Built (`internal/service`, `bundle.go`). Choices beyond the paper spec:

- **`<build-id>` is a content hash**: the first 16 hex digits of a SHA-256 over both bundles'
  trees (paths, modes and bytes). The same bundles always land in the same directory, so a
  reinstall copies nothing; any changed byte, a staple included, gets a new directory. The build
  name a holder reports (`-X main.build`) is separate.
- **Install takes bundles only on macOS.** `-hab-bin` is `GFF Hab.app` or the executable inside
  it; `-holder-bin` defaults to the `GFF Holder.app` beside it. Install checks each
  `CFBundleExecutable`, that the ids end in `.hab` and `.hab-holder` (or both in `.dev`), and that
  both share one prefix. Bare binaries are refused.
- **The rows install runs.** For each copy: `codesign --verify --strict --verbose=2`, then
  `codesign --verify -R=<requirement>` with the designated requirement of the source bundle. Install
  does not know the team, so the template comparison (rows 2 and 3) stays with the release script
  and `gff setup --check`. A build directory that already exists is verified, never written; if it
  fails, install stops and says to reinstall.
- **`<state>/current` is set right after the rename**, before the plist that names the build, so a
  running gff-hab of an older build never prunes the new one.
- **Pruning.** A build directory goes when no plist of this install names it (the logic plist and
  every holder plist), it is not the build of the caller, and `current` does not name it. gff-hab
  prunes at install, after it retires a holder, and at its start sweep. A temporary copy directory
  goes once it is 10 minutes old. A process started by hand from a build directory, with no job,
  is not seen; that is a documented limit.
- **`gff-hab serve -build <build-id>`** tells the logic job which build it runs from, so it starts
  holders from the `GFF Holder.app` of its own build and reads both bundle ids from their
  `Info.plist`.
- **`AssociatedBundleIdentifiers`** holds the job's own bundle id only. Adding
  `<prefix>.flightdeck` waits for the Mac app.
- **Uninstall** removes every build directory by its name, after it has stopped every job.
- **LaunchServices registration.** After the rename, install runs `lsregister -f` on both bundle
  copies of the build. A prune and uninstall run `lsregister -u` on a build's copies before they
  remove them. System Settings and `tccutil` find a bundle by its id only through a registered
  copy (8.1). A launch agent's first start registers its bundle too, so a failed `lsregister -f`
  is a warning, not an error.
- **Linux is unchanged**: one copy of each binary in `<state>/bin` (`hab-holder.md` 4.4).

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
| Local Network | No preflight API is known. `--check` reports `unknown` unless a connection gff-hab made to a LAN address has succeeded or failed with the local-network error since start. The walk makes one connection to a LAN peer (or the gateway) from the probe and keeps the probe alive 30 s, because a short-lived process gets no alert (TN3179) | **No** | No anchor exists (8.1): open `x-apple.systempreferences:com.apple.preference.security` and name the Local Network row in words |
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

### 7.9 Built: `hab.permissions`, `gff-hab probe` and `gff setup`

Code: `internal/grants` (checks, probe protocol, signature rows, the service),
`cmd/gff-hab/permissions.go` (probe panes, wiring), `cmd/gff/setup.go` (the command). Choices
beyond the paper spec:

- **The API method is `hab.permissions`** (revision 1, additive; capability `permissions`).
  Params: `grants` limits the check to named grants; `request` names one grant the probe asks
  macOS for first (accessibility, screen-recording, local-network, files-and-folders,
  pasteboard). Only the walk sends `request`. The result is one row per grant and process:
  `process` (holder or hab), `instance`, `draining`, `grant`, `state` (granted, denied, unknown,
  unanswered, off, note, error), `required`, `detail`, `remembered` (the state is a kept
  answer; see Local Network below), `settings` (the URL) and `reveal` (the bundle to show in
  Finder). The Mac app's Permissions screen reads the same rows (7.8).
- **The client decides the exit code from the rows.** `error` on any row is exit 2; a required
  row that is not `granted` is exit 1. gff setup prints `ok`, `missing` (denied or unanswered),
  `unknown`, `off`, `note` or `error`. A draining holder's rows say `draining` in the first
  column. A granted domain row goes on the host line as `Aqua`. `--json` prints the rows with
  `exit` and `reason`.
- **The probe protocol.** gff-hab starts `<its own executable> probe -nonce <n> -checks <g,...>
  [-request <g>]` in a pane on the current holder, 80x24, cwd `/`, with the pane environment
  minus `GFF_HAB`. The probe prints one line, `GFFPROBE1 <nonce> <base64 JSON>`, and exits.
  gff-hab reads the pane's bytes from offset 0 through the link until that line, at most 256 KiB,
  then releases the pane. A line with another nonce, or a body that names another nonce, is not
  a result. The pane must start within 5 s (exit 2 past it); the result must come within 20 s,
  or 2.5 minutes with a request.
- **Probe panes are hidden.** gff-hab wraps the structure's view of the holders in a filter that
  drops the probe's uuid, so no client ever sees a probe pane. The link still follows it, and
  its uuid is retired after, like any closed pane. gff-hab writes each probe pane's uuid to
  `<state>/macos/probe-panes` before the spawn, and removes it once the pane is released or
  gone. A gff-hab that restarts mid-probe reads that list: the holder still has the pane, so the
  new gff-hab hides it and closes it as soon as the holder lists it. A release that failed (the
  holder connection dropped) leaves the uuid listed, and the pane is closed when the link sees it
  again. A listed uuid no holder shows within 24 hours is dropped.
- **A draining holder is not probed.** It refuses new panes (`hab-holder.md` 4.3 step 4), and
  the link spawns on the current holder only. gff-hab instead runs the signature rows on the
  draining holder's bundle (found from its pid) and compares its designated requirement with the
  current holder's. Same requirement: `granted`, "shares its grants" (6.2). Another requirement:
  `denied` and required, because its panes lack every grant made since. Whether a grant made
  after a holder started reaches it is P4's question; 6.3's restart is not built.
- **One check at a time** per hab; a second call waits. The call runs off the connection's read
  loop, bounded at 3 minutes.
- **The C checks** go through `purego` (`dlopen` of ApplicationServices, CoreGraphics,
  CoreServices and CoreFoundation) in `system_darwin.go`. `AEDesc` is passed as an opaque
  buffer, so its packing never matters. `AXIsProcessTrustedWithOptions` gets a one-key
  CFDictionary with the prompt key.
- **Local Network.** `--check` cannot connect without risking the alert, so it reports `unknown`
  until a walk's request ran: the probe connects to the default gateway's port 443. A connection,
  or a refusal from the gateway, is `granted`; no route is `denied`; it retries every 2 s and
  stays alive up to 30 s. A gateway outside the private and link-local ranges (a VPN or an exit
  node as the default route, public or CGNAT) is `unknown` with no connection, because reaching
  it needs no grant. **Limits:** a full-tunnel VPN whose gateway has a private address still
  reads as `granted` without the grant. An undecided grant gives no route, so it reads `denied`
  ("may be off"), not `unknown`. **The answer is kept** in `<state>/macos/seen.json`, keyed by process
  and a hash of the designated requirement, and later checks report it with its date. Keying on
  the requirement follows TCC: an update with the same requirement keeps it (P12 needs exit 0),
  a dev rebuild or a new team loses it. The paper spec said "since start"; that would fail every
  check after an update. Files and Folders answers (when Full Disk Access is dropped) are kept
  the same way. A row that reports a kept answer carries `remembered`: the user may have turned
  the grant off since, and no check sees that. `--check` still passes on a remembered `granted`
  (P12). The walk treats a remembered row as not granted and requests it again. A grant that is
  still on answers at once, with no dialog, and nothing opens; a grant that is gone is found and
  walked like any missing row, and the new answer replaces the kept one.
- **Pasteboard is not checked by default.** The round trip replaces the clipboard for a moment
  (the old text is put back; other content is lost), so it runs only on request or `--grant
  pasteboard`, and is never required.
- **Opt-in grants come from `$GFF_MACOS_GRANTS`** on gff-hab, because no Flightdeck config file
  exists yet: `accessibility`, `screen-recording`, `developer-tools`, `pasteboard`,
  `automation:<bundle-id>`, and `no-full-disk-access`, which drops the holder's Full Disk Access
  for Files and Folders (Q1). An opt-in that is off is reported `off` without a probe. It moves to
  `macos.grants` when the config file lands. `gff-hab install` passes the variable to the jobs
  and records it in `install.json`. A later install with the variable unset keeps the recorded
  list and says so; setting it, even empty, replaces the list. A name gff-hab does not know (a
  typo, or `automation:` with no bundle id) gets a warning at install and a required, `denied`
  `opt-in` row on gff-hab, so `--check` exits 1 instead of passing without the grant the user
  asked for. The walk cannot fix that row; it says to fix the variable and install again.
- **gff-hab's own Full Disk Access** is always checked, and required only when a project folder
  the store knows lies under Desktop, Documents, Downloads, iCloud Drive or `/Volumes`.
- **Background item.** For each launchd job (the logic job and each running holder's job):
  `launchctl print gui/<uid>/<label>` must succeed and `launchctl print-disabled` must not list
  it as disabled. gff-hab started with no init system gets a `note` row instead.
- **Signature rows** run 5.3 rows 1 to 4 on each running bundle and read `spctl` (reported, not
  gated). An ad-hoc signature (`cdhash` requirement) passes with a `dev` note; an Apple
  Development signature passes as a dev build; a Developer ID signature must carry the release
  requirement exactly.
- **Re-grant owed (6.4)** is read from `<state>/macos/regrant-owed`. A full check whose required
  rows are all granted removes it and writes `<state>/macos/last-pass`. Nothing writes the
  record yet: `gff-hab install` does not compare requirements across builds (owed).
- **The walk.** It runs the full check; with nothing missing it opens nothing (7.7). For each
  required row that is not granted (or each `--grant`): requestable holder grants are requested
  first and pass at once if the request answers granted; then it opens the row's URL (`open`,
  and `open -R <bundle>` for Full Disk Access), prints the menu path, and re-checks that one grant
  every 2 s. Enter re-checks, `s` then Enter skips, Ctrl-C stops and still prints the final check.
  After a stop, the hab may still run the probe the walk left (up to 2 minutes for a request),
  and the final check would queue behind it. So that check waits 5 s at most; past that, gff
  setup says to run `gff setup --check` and exits 2. Exiting closes the connection, which ends
  the probe. A second Ctrl-C ends gff setup at once.
  Local Network and Files and Folders re-send the request on each re-check. Developer Tools has
  no check: the walk opens its pane and goes on. A signature failure or a re-grant record cannot
  be fixed by the walk; it says what to do. Over `--host` or `--via` it prints the check and says
  to run the walk on that Mac. On Linux it prints `loginctl enable-linger` or `gff-hab install`.
- **Linux rows**: `lingering` (`loginctl show-user <uid> --property=Linger`) and `units`
  (`systemctl --user is-enabled <logic unit>`), both required under systemd.

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

Steps that need nothing (P0, P2, P8) can run at any time. The owner approved P1, P3 to P7 and
P9 to P12 (2026-11-01): Developer ID signing, TCC changes on a test Mac and a fresh user account.
They are approved; 8.1 says which ran.

### 8.1 Results so far

**P0 and P2 (macOS 27.2), passed.** A holder installed by `gff-hab install` as a launch agent in
`gui/<uid>`, with gff-hab started the same way; the commands ran in a pane made by
`session.create`, and the install was set up and removed over SSH:

- `launchctl managername` printed `Aqua` in the pane, and `Background` in the SSH shell.
- `pbcopy` of a random token, then `pbpaste`, returned the token at once, so no prompt held it
  (nobody watched the screen). The holder was an unsigned local build.

**P8, static part (macOS 27.2).** Checked on disk, without opening System Settings:

- The extension ids `com.apple.LoginItems-Settings.extension` and
  `com.apple.Notifications-Settings.extension` exist in `/System/Library/ExtensionKit/Extensions`.
- The Privacy & Security extension (`com.apple.settings.PrivacySecurity.extension`) still
  carries the legacy name `com.apple.preference.security`, and these anchors: `Privacy_AllFiles`,
  `Privacy_Accessibility`, `Privacy_ScreenCapture`, `Privacy_Automation`, `Privacy_DevTools`,
  `Privacy_Pasteboard`, `Privacy_FilesAndFolders`.
- It has **no `Privacy_LocalNetwork` anchor**. The Local Network walk must open the Privacy &
  Security pane without an anchor and name the row in words.

Still open for P8: that each URL opens the named pane. Confirming it needs a person at the screen,
or a screen capture, which needs Screen Recording (a TCC grant). **The visual part of P8 is still
owed.**

**P1 (macOS 27.2), passed.** Run over SSH with a proof prefix, not a product one:

- `scripts/macos-bundle.sh` built gff-hab and gff-hab-holder with `-tags ghostty` twice (builds A
  and B), wrapped both, signed them with the Developer ID identity from a temporary keychain,
  hardened runtime and a secure timestamp, and notarized each set: status `Accepted`. It stapled
  both bundles.
- Every 5.3 row passed on both bundles: `--strict` verify ("valid on disk", "satisfies its
  Designated Requirement"); `codesign -d -r-` equal to the 5.1 template; `TeamIdentifier` the
  team and `flags=0x10000(runtime)`; `-R=<template>` exit 0; `spctl` "accepted",
  `source=Notarized Developer ID`; `stapler validate` "The validate action worked!". The holder
  carries exactly the seven entitlements of 4.2. There is no `gff` binary yet, so its rows did
  not run.
- `gff-hab install` copied each build into `<state>/builds/<build-id>` and ran it as a launch
  agent. The hardened gff-hab started, logged `parser libghostty`, answered `hab.ping` and
  `hab.hello`, and served `session.create`. No entitlement beyond 4.2 was needed.
- Two blockers found and built around (5.2.1): the login keychain is locked over SSH, so
  `codesign` failed with `errSecInternalComponent` and `notarytool` with `keychainLocked`. A
  temporary keychain holds the identity and the notary profile for the run.

**P3, the part without a grant (macOS 27.2), passed.** Notarized builds A and B, one bundle id:

- Install A; a pane on A's holder. Install B over it: gff-hab of B started holder B from
  `builds/<B>/GFF Holder.app`; a new pane landed on B; A's pane stayed on A, whose holder kept
  running from `builds/<A>/GFF Holder.app`. Both copies verified, with one designated
  requirement. Each plist named its executable inside its own build's bundle and carried
  `AssociatedBundleIdentifiers`.
- In both panes: `launchctl managername` printed `Aqua`; a `pbcopy` and `pbpaste` round trip
  returned the token at once (P0 and P2 again, now under the signed holder); the Full Disk Access
  probe answered `denied` at once, as expected with no grant.
- Uninstall booted out every job and removed every build directory; no process was left.
- `scripts/service-check.sh` passes on the Mac with dev bundles: build A's copy goes once its
  holder retires.
- **Waits for the owner:** the grant itself. Add build A's `GFF Holder.app` to Full Disk Access,
  then list `~/Desktop` from a pane on A and on B, and check that System Settings shows one
  entry. The "other channel" half needs the Mac app, which does not exist; a byte-identical
  bundle lands in the same build directory, so there is nothing else to compare yet.

**P4: waits for the owner.** Every step is a System Settings toggle. The kit that runs the probes
around the toggles is prepared.

**P5, partly.** The pasteboard round trip under the signed holder returned the token at once,
with nothing waiting on a prompt. **Waits for the owner:** whether a prompt appeared, and whether
Privacy & Security lists "Paste from Other Apps" with GFF Holder in it.

**P6, partly.** From a pane of the signed holder, `nc -z` to the default gateway (port 443) and
to another LAN host (port 22) connected at once, on the first try, with no Local Network
decision recorded. So on macOS 27.2 an undecided Local Network state did not block a program in
a launch agent's pane. **Waits for the owner:** whether an alert appeared, under which name, and
whether Privacy & Security > Local Network lists GFF Holder. gff-hab makes no LAN connection of
its own yet (peer habs are not built), so that half is owed.

**`gff setup --check` on a notarized install (macOS 27.2), passed.** Two notarized release
builds (A, B) and two ad-hoc dev builds, all under a proof prefix, installed and checked over SSH
with no grant given:

- Every row the paper spec names appeared: `sig ok` with "Developer ID, notarized" on both
  bundles, `login ok` for each job, Aqua on the host line, holder `fda missing` with the Settings
  pane named, `lan unknown`, opt-ins `off`. Exit 1. A check took 0 to 1 s. `--json` gave the
  same rows.
- The probe pane never appeared in `structure.get` (0 panes before a session, 1 after).
- The first run found a bug that the Linux fakes hid: `codesign -dv` prints `flags=` inside the
  `CodeDirectory` line, so every release read as "no hardened runtime". Fixed and re-run.
- Over `--via` the walk printed the check and said to run it on that Mac; nothing opened.

**Local Network request: a finding for P6.** The walk's request (the probe, a Go program in a
pane of the signed holder, Local Network undecided) got "no route to host" from the gateway,
every 2 s for 30 s. Part A's `nc` from the same kind of pane connected at once. So the probe is
blocked where an Apple tool is not, which suggests macOS exempts its own tools, or that an alert
was waiting on the screen. **Waits for the owner:** was there an alert, naming what.

**P9, partly.** `launchctl disable gui/<uid>/<logic label>` made the logic job's row read
`login missing` ("switched off in Login Items") and the check exit 1; `launchctl enable` cleared
it. **Waits for the owner:** the real Login Items toggle, and whether gff-hab keeps running
after it (exit 2 if it stops).

**P10, the part without a grant, passed.** Dev build 1 (ad-hoc, `.dev` ids, its own job prefix)
next to the release: `sig ok ... ad-hoc signature (dev build)` and the `dev` note on both
processes. After a rebuild (dev build 2) installed over it, the drained dev-1 holder read
`draining sig missing: another signing identity than the current holder`, which is the lost dev
grant in requirement form. The release's check did not change. **Waits for the owner:** the
grant itself (P10 kit step).

**P12, the part without a grant, passed.** Release B installed over A: holder B started from its
own build directory, A's pane stayed on A, and `--check` gave a `draining sig ok` row "same
identity as the current holder, so it shares its grants". The kept Local Network answer carried
over to B (same requirement). **Waits for the owner:** the grants and "no prompt; exit 0; the
drained holder's pane still reads the protected folder" (P12 kit steps).

**P7 and P11 wait for the owner** and a fresh account. The kit runs each step inside that
account.

**Restoring TCC needs a bundle LaunchServices can see.** `tccutil reset All <bundle-id>` failed
with OSStatus -10814 (`kLSApplicationNotFoundErr`) while the only copies of the bundle were under
`/tmp`, even after `lsregister -f`. With a copy under the home directory registered, it worked.
The installed layout (`<state>` under `~/Library`) is not affected; a proof that installs under
`/tmp` registers a copy elsewhere before it resets.

**A build under `/tmp` cannot be granted (macOS 27.2).** An owner proof kit installed its state
under `/tmp`. Dragging that build's `GFF Holder.app` into Full Disk Access added no row. The
cause is LaunchServices, checked on the same Mac:

- `lsregister -f` on a copy under `/tmp` adds a record (the dump shows `/private/tmp/...`), but
  `tccutil reset All <id>` still fails with -10814 while that is the only copy. LaunchServices
  lookups by bundle id skip `/tmp`.
- `gff-hab install` with its state under `~/Library/Application Support` (dev bundles, a test
  prefix): both build copies were in the dump within 5 s, registered as each job started.
  `tccutil reset All` succeeded for both ids. No path component is hidden or a dot path.
  Finder hides `~/Library` by default; `open -R` still reveals a bundle in it.
- A plain `ditto` copy under `~/Library/Application Support` is not registered, and running its
  executable from an SSH shell does not register it: `tccutil` gave -10814. `lsregister -f`
  registered it, and `lsregister -u` removed it again.
- After an uninstall the dump kept records for the deleted build paths.

- That side effect is not reliable. In a later install of a notarized build from before this
  change, the hab copy was registered and the holder copy was still missing from the dump 20 s
  after install, while its holder ran a pane. `tccutil reset All` of the holder id failed with
  -10814.

So the product path was registered only as a side effect of a job's start, and not always. Install
now registers each build copy itself and unregisters it before the build goes (6.1). With that
change, `scripts/service-check.sh` checks the dump after install, after the retire, and after
uninstall. Proof kits keep every installed state under `~/Library/Application Support`.

---

## 9. Open questions

Q2 to Q5 are decided by the coordinator. Q1 and Q6 widen what the holder can reach, so the
owner decided them (2026-11-01).

### Q1. Holder Full Disk Access by default. **Decision: yes, FDA by default (owner, 2026-11-01)**

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

### Q6. The opt-in entitlements on the holder. **Decision: yes, carry them as terminals do (owner, 2026-11-01)**

Carrying camera, microphone, contacts and similar entitlements lets a pane program ask for them,
as in a terminal app. It also widens what the holder could be asked for. Recommendation: carry
them, as terminals do; the user still answers each prompt, and `--check` never requires them.

---

## 10. Proposed change to hab-holder.md

Applied: `hab-holder.md` 5.3 and Q2 now carry this decision. The text below is kept as the record of the change. It replaced the open block in 5.3 (from
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

---

## 11. Study: an optional Accessibility grant for setup

Status: paper study, read-only. Nothing on a Mac was granted, pressed or changed for it. Claims
marked **needs Mac proof** were not tried.

The question: could one opt-in Accessibility grant let Flightdeck click through its own Mac setup
for the user? Scope is our own setup only. The design never reads or presses another app's dialog.

### 11.1 The reference: dontallow

dontallow (<https://github.com/johncassil/dontallow>, MIT, version 0.1.1) is a small Swift tool
that answers macOS's Local Network alert with "Don't Allow". Read for behaviour, not copied:

- A per-user launch agent in `gui/<uid>`, kept alive. It needs only Accessibility.
- It finds the process that hosts privacy alerts (`com.apple.UserNotificationCenter`) and
  registers an `AXObserver` on it for "window created". It re-attaches when that process
  restarts. Idle, it uses no CPU.
- On a new window it waits 0.25 s, so the user can click first. It then walks the window's
  Accessibility tree (eight levels deep) and collects the text and the buttons.
- It acts only when the text contains "local network" and a button reads "Don't Allow". The
  match is on English text; it normalizes the curly apostrophe macOS uses.
- It sends the Accessibility press action. If macOS refuses the press, it posts a synthetic mouse
  click at the button's centre. Its source calls these "hardened dialogs that refuse AXPress".
- It never presses Allow. It has a pause command, an allowlist and a dry-run mode.
- Its README says it was tested on one Intel Mac on macOS 26.6. Every Homebrew upgrade drops the
  Accessibility grant, because the build is not Developer ID signed.

The Show HN thread (item 50294506) adds three points. TCC keys grants on the designated
requirement, not the path (as 1.1 found). MDM cannot preset Local Network. The trust cost is real:
one reply calls it "an unsandboxed unverified app that requests Accessibility". No comment shows
whether Apple refuses a synthetic press of Allow.

### 11.2 What Apple documents

| Fact | Source |
|---|---|
| `AXIsProcessTrustedWithOptions` with the prompt option prompts asynchronously and does not change the return value. Only the user can switch the grant on | Apple, `AXIsProcessTrustedWithOptions` |
| Local Network has no API to request or check it. Connecting a UDP socket to a LAN address raises the alert and sends no traffic | TN3179, "Trigger the local network alert", "Check for local network access" |
| macOS auto-allows Local Network for launchd daemons, for root, and for command-line tools run from Terminal or SSH, with their children. Launch agents are not exempt | TN3179, "macOS considerations" |
| A short-lived process gets no Local Network alert | TN3179 (FB16131937) |
| macOS 27.2 adds + and - buttons to the Local Network list, so a user can reset a decision | TN3179, revision 2026-10-06 |
| MDM cannot configure Local Network | TN3179 |
| A PPPC profile can allow Accessibility and Full Disk Access, through the device channel with user-approved MDM. On macOS 27.0 an Accessibility profile shows a notification, and the user can change the setting. A profile can only deny Screen Recording, camera and microphone | Apple, Device Management, `PrivacyPreferencesPolicyControl.Services` |

Not from Apple:

- macOS blocks synthetic input per UI component, not everywhere. High Sierra blocks synthetic
  clicks on the keychain prompt and on kernel extension approval (Jamf, "Synthetic Reality"). A
  2018 bypass that clicked Mojave's privacy alerts was reported as a vulnerability. A process that
  presses Allow on its own privacy alert is the class of behaviour Apple patches.
- macOS 26.1 had a bug where binaries did not appear in the Accessibility list. An Apple engineer
  called it serious; 26.3 betas seem to fix it (Apple Developer Forums thread 808897).
  `AXIsProcessTrusted` can return true while event taps fail, until the user removes and re-adds
  the row (MiddleClick issue 162).
- No source describes a macOS 27 change to Accessibility or synthetic input.

### 11.3 What Accessibility could automate in our setup

| Step | What AX would do | What still needs the user | Verdict |
|---|---|---|---|
| A. Our own Local Network alert (holder, gff-hab) | Watch the alert host, match our bundle name, press Allow | Nothing, if the press works | Pressing Allow for ourselves is self-consent. Apple fixes such paths when found; dontallow presses only Don't Allow. Fragile, and against the purpose of the alert. Whether it works at all **needs Mac proof**. Not proposed |
| B. Walk System Settings panes in `gff setup` | Find our row in Full Disk Access and press its switch, or press + and fill in the file picker | The admin password or Touch ID that each privacy switch asks for. A process cannot type into that sheet, and must never hold the password | Saves a scroll and a drag at most. System Settings is SwiftUI with no stable identifiers, and its tree changes each release (it has no Local Network anchor, 8.1) |
| C. Verify a switch is on | Read the switch's value in System Settings | Nothing | Worse than today. The probe in a pane (7.2, 7.3) tests the grant itself. A switch can read "on" while the grant does not match (1.1, and the stale-trust bug above) |
| D. Login Items | Press the switch for our job | The same authentication | On by default. Needed only after the user switched it off on purpose |

Accessibility is itself a manual grant. The user switches it on, with authentication, before AX can
do anything. Today the required walk has two manual grants: Full Disk Access and Local Network for
the holder. Local Network for gff-hab joins them only with a peer hab. An AX grant adds one switch
to save, at best, one alert click and one drag. The number of manual steps does not go down.

### 11.4 Privacy and trust cost

- Accessibility lets its holder read the text of every window and press anything in every app.
  The prompt and the Settings row say so in Apple's words. Users and reviewers read it as the
  most sensitive grant; the dontallow thread shows the reaction.
- The holder already has an opt-in Accessibility row (4.1) for agents that drive the UI. If setup
  used the holder's grant, every program in every pane would get it too. That widens the grant for
  a one-time convenience.
- Matching dialogs by text means English only, or a translation table per macOS language.
- A process that presses privacy buttons behaves like malware. Security tools may flag it, and a
  later macOS may refuse it without notice.

### 11.5 Which process would hold it

| Holder | For | Against |
|---|---|---|
| gff-hab-holder | Already has an opt-in AX row | Every pane program inherits it (11.4). Not acceptable for setup |
| gff | It runs the walk | It is not a TCC client. Its terminal is responsible (4.1), so the grant would land on the terminal app |
| gff-hab | Serves `hab.permissions` | A long-running agent would gain power over every app, all the time |
| A new helper bundle (`GFF Setup.app`) | Narrowest: runs only during the walk, then exits | A fifth signed identity, another Settings row, and another bundle in every release |
| The Flightdeck Mac app | A GUI the user already chose to trust; it owns the first-run screen (7.8) | The app does not exist yet. The hab must also set up without it (3.2) |

If it is ever built, the Mac app holds the grant. It uses the grant only while the first-run or
Permissions screen is open.

### 11.6 Opt-in and the setting

If built, it follows the owner rule for friction-adding things (a setting turns them off) and the
hardening bar (no stricter than herdr, Paseo and Rex):

- Off by default. The walk offers it once. Declining leaves today's walk unchanged.
- A setting in the Mac app turns it off at any time. Turning it off tells the user that the
  Accessibility row stays until they remove it.
- It acts only on our own bundle names, and only while a setup screen is open. It never presses
  Allow on any alert (11.3 A) and never reads another app's window.
- `gff setup --check` reports it as one opt-in row: `off` by default, never required.

### 11.7 What herdr, Paseo and Rex do

None of them automates setup with Accessibility (`landscape.md` 2.16). Paseo gives no TCC
guidance. herdr fixed updates that reset grants. Rex names Full Disk Access and Screen Recording in
a `tcc` package and leaves the switches to the user. Our walk (7) already meets that bar.

### 11.8 Recommendation: no, not now

Do not build it. Revisit only if one of these happens:

- A reference tool ships it and its users accept it.
- Apple documents an API to request Local Network or Full Disk Access, or a stable way to open a
  privacy pane with our row selected.
- The Mac app's first-run screen exists, and testers report the Full Disk Access drag as the main
  place they give up.

Cost if built later, in the Mac app: about 3 to 4 days for B and C on one macOS version (observer,
row finder, file-picker fill, the setting, the opt-in row). Add 1 to 2 days of Mac proof per macOS
release, because the System Settings tree changes. A is excluded at any cost.

Cheap gains that need no Accessibility, worth adding to the walk now:

- Raise the Local Network alert with a UDP connect to a LAN address, as TN3179 suggests, instead
  of a TCP connection to the gateway (7.9). It sends no traffic. The probe already stays alive 30 s.
- On macOS 27.2 and later, a user can remove a denied Local Network row with the new - button. The
  walk can say so.
- Keep `open -R` and the printed path for the Full Disk Access drag (7.4).

If this is revisited, three points **need Mac proof**: whether `UserNotificationCenter` refuses an
AX press of Allow on macOS 27; whether a System Settings privacy switch accepts an AX press before
authentication; and whether the + button's file picker accepts a path set through AX.
