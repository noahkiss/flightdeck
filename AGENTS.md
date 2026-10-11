# flightdeck

flightdeck is a personal build of [rootshell](https://github.com/kitknox/rootshell), carried as
a patch series on top of upstream `main`. This section holds the fork's rules. The upstream notes
below it still apply to the code.

## What this repo is

- A new repository that carries upstream's full history. It is not a linked GitHub fork.
- Remotes: `origin` is this repo, `upstream` is `kitknox/rootshell`.
- **Upstreaming is not a goal.** Issues here are for the sync workflow and its fixer; report rootshell bugs upstream.
- `FLIGHTDECK.md` is the patch ledger. Every patch has an entry there. It also names the base.

## Patch rules

- Keep patches **surgical and additive**. Prefer a build setting, an xcconfig line or a new file
  over a rewrite of upstream code.
- One patch, one commit on top of upstream. A rebase replays them one at a time. A patch that
  upstream makes redundant is dropped whole.
- Identity changes go through `Configuration/Flightdeck.xcconfig`, never through literals in
  `project.pbxproj`, `.entitlements` or `Info.plist` (see the upstream section below).
- Update `FLIGHTDECK.md` in the same commit as the patch it describes.

## Branch, sync, release

1. **Branch per patch.** Never commit a patch to `main` directly. Land by squash-merge, so one
   ledger entry is one commit.
2. **Never merge upstream into `main`.** Move the base by rebasing:
   `git rebase --onto <new-base> <old-base> main`. Then run
   `git range-diff <old-base>..<old-tip> <new-base>..<new-tip>` and read every patch marked `!`.
3. The nightly sync workflow does step 2 on CI. On a conflict or a red build it opens an issue for
   an agent fixer instead of pushing.
4. Release tags are `v<upstream-version>-fd.<n>`. Upstream tags have no `v`, so the two never
   collide. Push tags by name; this repo sets `push.followTags false`.

## Conventions

- **Conventional commits** for our patches: `feat(identity):`, `ci:`, `docs:`, `chore:`.
- **Never add an AI or agent attribution footer** to a commit or pull request.
- **The repo is public.** Nothing tracked may name a private machine, network, account, path or
  credential location. Secrets reach CI only as GitHub Actions secrets.
- Linux cannot build this project. Every build and test runs on GitHub-hosted macOS runners.

---

# Notes for coding agents

Conventions in this repo that are not obvious from the code. See
[README.md](README.md) for what rootshell is and how to build it.

## Signing identity is centralized — never hardcode it

`Configuration/Identity.xcconfig` is the single source of truth for the
Apple Developer team, the bundle-identifier prefix, the App Groups, the
keychain group, and the iCloud container. No target sets `DEVELOPMENT_TEAM`
itself.

To build under a different Apple Developer account, run:

```bash
scripts/setup-dev-signing.sh
```

It writes the gitignored `Configuration/DeveloperSettings.xcconfig`, which
`Identity.xcconfig` includes optionally. Read
[docs/contributor-signing.md](docs/contributor-signing.md) before changing
anything about signing.

**Do not** edit `project.pbxproj`, an `.entitlements` file, or an `Info.plist`
to change the team or bundle identifier. Those files reference variables
(`$(ROOTSHELL_DEVELOPMENT_TEAM)`, `$(ROOTSHELL_DEFAULT_APP_GROUP)`, and
friends); replacing one with a literal breaks every other configuration
silently, because a target-level build setting shadows the whole xcconfig
chain.

Code that needs a resolved identifier at runtime reads it back from the
bundle's Info.plist rather than repeating the literal:
`rootshell/Core/Security/AppIdentifiers.swift` for the app group, keychain
group, and iCloud container; `rootshell-helper/Sources/SocketCommandServer.swift`
for the bundle identifier and team; `RootshellPushKit`'s `PushConfiguration`
for its app group.

## Check project.pbxproj before committing

`git diff` on `project.pbxproj` should be empty unless you meant to change the
project. A signing override resolves through variables, so a build under your
own team shouldn't touch it — if a hunk appears anyway, it likely carries your
org identifier or team, and shouldn't go into a PR.

## Building

Shared schemes: `rootshell-AppStore` (iOS/iPadOS/visionOS, sandboxed),
`rootshell-Standalone` (unsandboxed Mac Catalyst), `rootshell-China`,
`rootshell-helper` (the macOS background helper), and `rootshellvpn`
(the native macOS VPN host and system extension).

Build for a simulator by device id rather than
`-destination 'generic/platform=iOS Simulator'`; the generic destination builds
x86_64 too and fails with `cannot find type 'arm_thread_state64_t' in scope` in
`MainThreadStackSampler.swift`.

```bash
xcodebuild -project rootshell.xcodeproj -scheme rootshell-AppStore \
  -destination 'id=<simulator-udid>' build
```

Device and Mac Catalyst builds need `-allowProvisioningUpdates` from the
command line; without it every target fails with "Automatic signing is disabled
and unable to generate a profile". They also need an Apple ID signed in under
Xcode → Settings → Accounts. Note that a `No Accounts` error line is not
diagnostic on its own — Xcode emits it alongside unrelated capability failures
even with an account signed in and a valid team profile in use.

`rootshellvpn` has a shared native macOS scheme and embeds the `tunnel` system
extension. Both targets explicitly clear the terminal app's bridging header.
`rootshell-Standalone` bundles the prebuilt app in `rootshell/Resources`; it
does not rebuild these targets. Use `scripts/deploy-vpn-host.sh --build-only`
for an unsigned universal compile check, or the signed workflow in
[docs/contributor-signing.md](docs/contributor-signing.md) to refresh that
artifact. The latter requires Developer ID profiles and notarization.

To compare resolved build settings across configurations, pass `-alltargets`
rather than looping over targets — one invocation per configuration instead of
one per target.
