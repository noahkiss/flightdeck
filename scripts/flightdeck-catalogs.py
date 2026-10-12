#!/usr/bin/env python3
"""Rewrite the rootshell name to FlightDeck in a checkout, for a build.

CI runs this on its checkout just before xcodebuild (ci.yml and release.yml).
It never runs on a tracked tree: the files in git stay upstream's, so the
nightly rebase never conflicts on them, and text that upstream adds is renamed
on the next build.

It rewrites three kinds of text:

- String catalogs: every value, in every language. A key is renamed exactly
  when the Swift literal that looks it up is renamed (below), so the two stay
  paired. A key that has no value in the source language gets one, because
  that language otherwise shows the key itself.
- Swift string literals in the app, extension, package and test sources. A
  literal is renamed only when the name stands as a word in text with
  whitespace (a sentence, a label, terminal output). Literals without
  whitespace are identifiers (bundle IDs, keychain services, defaults keys,
  URL schemes, file names) and stay. Tests are rewritten too, so their
  expectations match.
- A few bundled text resources that users read (RESOURCES).

What stays is listed below with a reason: PROTECTED names, KEEP_PHRASES,
KEEP_LITERALS, BARE_KEEP and the herdr fork rule. REPLACE renames a few
identifier-like literals that users see (export file names, the terminal
banner). scripts/flightdeck-brandcheck.py imports these lists to check the
built app.

Usage:
  scripts/flightdeck-catalogs.py            rewrite the checkout
  scripts/flightdeck-catalogs.py --dry-run  report what would change
  scripts/flightdeck-catalogs.py --check    fail when a list entry no longer
                                            matches the sources, or a literal
                                            names rootshell and no rule
                                            classifies it
  scripts/flightdeck-catalogs.py --diff     print each Swift and resource
                                            change (implies --dry-run)
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
NAME = "FlightDeck"
NAME_LOWER = "flightdeck"

CATALOGS = [
    "Localizable.xcstrings",
    "PushNotificationService/Localizable.xcstrings",
    "SessionActivityWidget/Localizable.xcstrings",
    "rootshell/AppShortcuts.xcstrings",
]

# Swift sources that ship or test what ships.
SWIFT_ROOTS = [
    "rootshell",
    "rootshell-helper",
    "rootshellvpn",
    "rootshellTests",
    "rootshellvpnTests",
    "PushNotificationService",
    "SessionActivityWidget",
    "VPNTunnelExtension",
    "CoreWLANPlugin",
    "tunnel",
    "Packages",
]
# Test sources: rewritten, but not required to be classified by --check.
TEST_PARTS = ("Tests/", "Tests.swift", "UITests/")

# Bundled text that users read.
RESOURCES = {
    "rootshell/Resources/Rootshell.sdef": "AppleScript dictionary shown in Script Editor",
    "rootshell/Resources/promptrc_example.toml": "copied into the user's ~/.promptrc.toml",
    "rootshell/Features/MCP/README.md": "bundled MCP notes",
    "Resources/Wasm/wasm-host.html": "page title of the WebAssembly host web view",
}

# Names that contain rootshell but are not the app's name. They are masked
# before any rewrite and restored after it.
PROTECTED = [
    ".rootshellrc",              # the local shell's startup file; user scripts and docs name it
    "rootshell-askpass",         # remote helper command
    "rootshell-channel",         # upstream herdr release channel
    "rootshell-helper",          # Mac helper app bundle and executable name
    "rootshell-notify",          # remote push CLI installed from push.rootshell.com
    "rootshell-open-clipboard",  # remote helper command
    "rootshell-open",            # remote helper command
    "rootshell.com",             # upstream's hosts (push relay, downloads)
    "rootshellbackup",           # backup file extension, bound to the exported UTI
    "rootshellvpn",              # Mac VPN host app bundle name
]

# Text kept as written wherever it appears.
KEEP_PHRASES = {
    "Based on rootshell by Kit Knox": "attribution, kept on purpose",
}

# Whole literals (and catalog keys) kept as written.
KEEP_LITERALS = {
    "rootshell VPN": "VPN profile name; VPNManager finds existing profiles by it",
    "rootshell Capture CA": "Capture CA name; CaptureCAManager finds the installed CA by it",
    r"rootshell Capture CA (\(device) \(date))": "Capture CA certificate common name",
}

# Upstream's herdr fork is a separate project named after rootshell
# (kitknox/herdr, channel rootshell-channel). A mention with "herdr" and a
# word for "fork" within a few characters names that project, in English and
# in translations ("rootshell herdr fork", "rootshell’s herdr fork",
# "herdr-Fork von rootshell", "rootshell の herdr フォーク").
HERDR_WINDOW = 24
FORK_WORDS = re.compile(r"fork|furca|haarakopio|פיצול|форк|分支|フォーク|포크|نسخة", re.IGNORECASE)

# Bare literals that are exactly the name. They stay unless REPLACE lists
# them; --check fails on a new one, so each is reviewed.
BARE_KEEP = {
    ("rootshell-helper/Sources/EnvironmentBuilder.swift", "rootshell"): "LC_TERMINAL; remote hosts and zellij-nkmk key on it",
    ("rootshell/Core/Terminal/TerminalIdentity.swift", "rootshell"): "terminal identity (LC_TERMINAL, XTVERSION); remote hosts key on it",
    ("rootshell/App/RootShellApp.swift", "rootshell"): "rootshell:// URL scheme",
    ("rootshell/Features/LiveActivity/SessionActivityAttributes.swift", "rootshell"): "rootshell:// URL scheme",
    ("rootshell/Features/Push/PushNotificationRouter.swift", "rootshell"): "rootshell:// URL scheme",
    ("rootshell/Features/Cloud/OAuth/OAuthFlowManager.swift", "rootshell"): "OAuth callback URL scheme",
    ("rootshell/Features/SSH/OpenPubkey/OpenPubkeyClient.swift", "rootshell"): "OpenPubkey redirect URL scheme",
    ("rootshell/Features/VPN/Tailnet/TailnetLoginCoordinator.swift", "rootshell"): "Tailscale login callback URL scheme",
    ("rootshell/Core/SettingsSync/ConfigOverlay/ConfigOverlayLocation.swift", "rootshell"): "legacy Application Support folder an earlier upstream build used",
    ("rootshell/Core/Terminal/CursorManager.swift", "Rootshell"): "catalog key of the cursor style name; the catalog value is renamed",
    ("rootshell/Core/Terminal/SelectionManager.swift", "rootshell"): "catalog key of the selection preset name; the catalog value is renamed",
    ("rootshell/Features/AIAgent/Providers/ChatGPT/ChatGPTOAuth.swift", "rootshell"): "OAuth client registration name (AI agents are off)",
    ("rootshell/Features/HTTPCapture/CA/CaptureCAManager.swift", "rootshell"): "Capture CA organization (HTTPS capture is off)",
    ("rootshell/Features/MCP/Protocol/MCPTypes.swift", "rootshell"): "MCP server protocol name",
    ("rootshell/Features/Transfer/Attachments/PasteAttachment.swift", "rootshell"): "log subsystem fallback",
}

# Literals renamed in full: identifiers that users see. Keyed by
# (file, literal as written in the source).
REPLACE = {
    ("rootshell/Features/LocalShell/LocalShellBanner.swift", "rootshell"): NAME,
    ("rootshell/Features/Cloud/SSO/AWSSSOOIDCClient.swift", "rootshell"): NAME,
    ("rootshell/Features/AIAgent/Providers/OpenRouter/OpenRouterProvider.swift", "Rootshell"): NAME,
    ("rootshell/Features/HTTPCapture/Export/CaptureExporters.swift", "rootshell"): NAME,
    ("rootshell/UI/Settings/Sync/ConfigFileSettingsView.swift", "rootshell.conf"): "flightdeck.conf",
    ("rootshell/UI/Settings/SettingsSearchCatalog.swift", "rootshell.conf"): "flightdeck.conf",
    ("rootshell/Core/Backup/BackupManager.swift", r"rootshell-backup-\(dateString).rootshellbackup"):
        r"flightdeck-backup-\(dateString).rootshellbackup",
}

# A mention right after one of these is a command argument: lowercase it.
LOWERCASE_AFTER = re.compile(r"mcp add $", re.IGNORECASE)

# The name as a word: not part of an identifier, path, host, flag or
# compound command (rootshell_x, .rootshell, rootshell-x, rootshell/x,
# rootshell.com, rootshell://, $ROOTSHELL).
WORD = re.compile(r"(?<![A-Za-z0-9_.\-/$~@\\])rootshell(?![A-Za-z0-9_\-/]|\.[A-Za-z0-9]|:[^\s])", re.IGNORECASE)
# In translations: no boundary after the name, because translations inflect
# it (rootshells, rootshellissä, rootshellben) and put CJK text right next to
# it. PROTECTED keeps the compound names.
BRAND = re.compile(r"(?<![A-Za-z0-9_])rootshell", re.IGNORECASE)
# Case-sensitive: these names are lowercase. "Rootshell-Helper" in a
# translation is the app's helper, not the executable.
PROTECTED_RE = re.compile("|".join(re.escape(t) for t in PROTECTED))
PHRASES_RE = re.compile("|".join(re.escape(t) for t in KEEP_PHRASES), re.IGNORECASE)
HERDR = re.compile(r"herdr", re.IGNORECASE)
# Entries whose translations are all kept: the attribution.
SKIP_KEY = re.compile(r"Based on rootshell", re.IGNORECASE)
FORMAT_SPEC = re.compile(r"%(?:\d+\$)?(?:ll|l|h|hh|z|t|j)?[@dDiuUxXoOfFeEgGcCsSpaA]")


# ---------------------------------------------------------------- text rules

def _masked(text: str) -> list[tuple[int, int]]:
    spans = [m.span() for m in PROTECTED_RE.finditer(text)]
    spans += [m.span() for m in PHRASES_RE.finditer(text)]
    return spans


def _near_herdr(text: str, start: int, end: int) -> bool:
    for window in (text[end:end + HERDR_WINDOW], text[max(0, start - HERDR_WINDOW):start]):
        if HERDR.search(window) and FORK_WORDS.search(window):
            return True
    return False


def kept_mention(text: str, start: int, end: int) -> bool:
    """True when the mention at text[start:end] must stay rootshell."""
    if any(s <= start and end <= e for s, e in _masked(text)):
        return True
    return _near_herdr(text, start, end)


def mentions(text: str, pattern: re.Pattern = WORD) -> list[tuple[int, int, str]]:
    """(start, end, replacement) for each mention of the name to rename."""
    out = []
    for m in pattern.finditer(text):
        if kept_mention(text, m.start(), m.end()):
            continue
        repl = NAME_LOWER if LOWERCASE_AFTER.search(text[:m.start()]) else NAME
        out.append((m.start(), m.end(), repl))
    return out


def apply(text: str, edits: list[tuple[int, int, str]]) -> str:
    for start, end, repl in sorted(edits, reverse=True):
        text = text[:start] + repl + text[end:]
    return text


def is_prose(text: str) -> bool:
    return bool(re.search(r"\s", text))


def rename_key(key: str) -> str:
    """The new catalog key, or the key itself. Mirrors the Swift literal rule."""
    if key in KEEP_LITERALS or not is_prose(key):
        return key
    return apply(key, mentions(key))


def rename_translation(text: str) -> str:
    return apply(text, mentions(text, BRAND))


# ---------------------------------------------------------- Swift literals

STR_OPEN = re.compile(r'(#*)("""|")')


@dataclass
class Literal:
    start: int                     # first content character
    end: int                       # closing delimiter
    segments: list[tuple[int, int]] = field(default_factory=list)  # own text, outside interpolations


def _skip_code(src: str, i: int, lits: list[Literal], until_paren: bool) -> int:
    n = len(src)
    depth = 0
    while i < n:
        c = src[i]
        if c == "/" and src.startswith("//", i):
            j = src.find("\n", i)
            i = n if j < 0 else j
            continue
        if c == "/" and src.startswith("/*", i):
            nest = 0
            while i < n:
                if src.startswith("/*", i):
                    nest += 1
                    i += 2
                elif src.startswith("*/", i):
                    nest -= 1
                    i += 2
                    if nest == 0:
                        break
                else:
                    i += 1
            continue
        if c in '#"':
            m = STR_OPEN.match(src, i)
            if m and (c == '"' or m.group(1)):
                i = _read_string(src, m.end(), len(m.group(1)), m.group(2) == '"""', lits)
                continue
        if until_paren:
            if c == "(":
                depth += 1
            elif c == ")":
                if depth == 0:
                    return i + 1
                depth -= 1
        i += 1
    return i


def _read_string(src: str, i: int, hashes: int, multi: bool, lits: list[Literal]) -> int:
    close = ('"""' if multi else '"') + "#" * hashes
    esc = "\\" + "#" * hashes
    lit = Literal(start=i, end=i)
    lits.append(lit)
    seg = i
    n = len(src)
    while i < n:
        if src.startswith(esc, i):
            j = i + len(esc)
            if j < n and src[j] == "(":
                lit.segments.append((seg, i))
                i = _skip_code(src, j + 1, lits, True)
                seg = i
                continue
            i = j + 1
            continue
        if src.startswith(close, i):
            lit.segments.append((seg, i))
            lit.end = i
            return i + len(close)
        if not multi and src[i] == "\n":
            break
        i += 1
    lit.segments.append((seg, i))
    lit.end = i
    return i


def swift_literals(src: str) -> list[Literal]:
    lits: list[Literal] = []
    _skip_code(src, 0, lits, False)
    return lits


_ESCAPES = {"n": "\n", "t": "\t", "r": "\r", '"': '"', "\\": "\\", "'": "'", "0": "\0"}


def normalize(src: str, lit: Literal) -> tuple[str, list[int]]:
    """The literal as its runtime/catalog-key text, with a map back to source offsets.

    Interpolations become %@ (mapped to -1); simple escapes are decoded.
    """
    out: list[str] = []
    pos: list[int] = []
    for k, (s, e) in enumerate(lit.segments):
        i = s
        while i < e:
            if src[i] == "\\" and i + 1 < e and src[i + 1] in _ESCAPES:
                out.append(_ESCAPES[src[i + 1]])
                pos.append(-1)
                i += 2
                continue
            out.append(src[i])
            pos.append(i)
            i += 1
        if k < len(lit.segments) - 1:
            out.extend("%@")
            pos.extend([-1, -1])
    return "".join(out), pos


def literal_edits(path: str, src: str, lit: Literal) -> tuple[list[tuple[int, int, str]], str]:
    """Source edits for one literal and its class: replace, keep, bare, ident, prose, plain."""
    raw = src[lit.start:lit.end]
    if (path, raw) in REPLACE:
        return [(lit.start, lit.end, REPLACE[(path, raw)])], "replace"
    text, pos = normalize(src, lit)
    if not BRAND.search(text):
        return [], "plain"
    if raw in KEEP_LITERALS or text in KEEP_LITERALS:
        return [], "keep"
    found = mentions(text)
    if not is_prose(text):
        has_word = any(True for _ in WORD.finditer(text))
        return [], ("bare" if has_word else "ident")
    edits = []
    for s, e, repl in found:
        rs, re_ = pos[s], pos[e - 1]
        if rs < 0 or re_ < 0 or re_ - rs != e - 1 - s:
            continue
        edits.append((rs, re_ + 1, repl))
    return edits, "prose"


def swift_files(root: Path) -> list[str]:
    result = subprocess.run(["git", "-C", str(root), "ls-files", "--", *SWIFT_ROOTS],
                            capture_output=True, text=True)
    if result.returncode == 0:
        return [p for p in result.stdout.split("\n") if p.endswith(".swift")]
    # Not a git checkout (an exported tree): walk the same roots.
    return sorted(str(p.relative_to(root)) for top in SWIFT_ROOTS
                  for p in (root / top).rglob("*.swift") if p.is_file())


def is_test(path: str) -> bool:
    return any(part in path for part in TEST_PARTS)


# ------------------------------------------------------------------ catalogs

def rewrite_units(node, counter: list) -> None:
    if isinstance(node, dict):
        unit = node.get("stringUnit")
        if isinstance(unit, dict) and isinstance(unit.get("value"), str):
            new = rename_translation(unit["value"])
            if new != unit["value"]:
                unit["value"] = new
                counter[0] += 1
        for value in node.values():
            rewrite_units(value, counter)
    elif isinstance(node, list):
        for value in node:
            rewrite_units(value, counter)


def process_catalog(path: Path, dry_run: bool) -> tuple[int, int, int, int, list[str]]:
    doc = json.loads(path.read_text())
    source = doc.get("sourceLanguage", "en")
    values = [0]
    keys = skipped = renamed_keys = 0
    errors: list[str] = []
    strings = doc.get("strings", {})
    new_strings: dict = {}
    for key, entry in strings.items():
        if not BRAND.search(key) and not BRAND.search(json.dumps(entry, ensure_ascii=False)):
            new_strings[key] = entry
            continue
        if SKIP_KEY.search(key) or key in KEEP_LITERALS:
            skipped += 1
            new_strings[key] = entry
            continue
        before = values[0]
        localizations = entry.setdefault("localizations", {})
        rewrite_units(localizations, values)
        if source not in localizations and rename_translation(key) != key:
            localizations[source] = {"stringUnit": {"state": "translated", "value": rename_translation(key)}}
            values[0] += 1
        new_key = rename_key(key)
        if new_key != key:
            if new_key in strings:
                errors.append(f"{path.name}: renamed key collides with an existing key: {new_key!r}")
                new_key = key
            else:
                renamed_keys += 1
        new_strings[new_key] = entry
        if values[0] != before or new_key != key:
            keys += 1
    doc["strings"] = new_strings
    if not dry_run:
        path.write_text(json.dumps(doc, indent=2, sort_keys=True, ensure_ascii=False, separators=(",", " : ")) + "\n")
    return keys, values[0], skipped, renamed_keys, errors


# --------------------------------------------------------------------- main

def key_form(text: str) -> str:
    return FORMAT_SPEC.sub("%@", text)


def run(root: Path, mode: str) -> int:
    dry_run = mode in ("dry-run", "check", "diff")
    errors: list[str] = []
    seen_replace: set = set()
    seen_keep: set = set()
    seen_bare: set = set()
    counts = {"prose": 0, "replace": 0, "keep": 0, "bare": 0, "ident": 0}
    files_changed = 0
    old_keys: set[str] = set()
    new_keys: set[str] = set()

    for path in swift_files(root):
        file = root / path
        src = file.read_text(encoding="utf-8")
        if not BRAND.search(src):
            continue
        edits: list[tuple[int, int, str]] = []
        for lit in swift_literals(src):
            found, kind = literal_edits(path, src, lit)
            if kind == "plain":
                continue
            counts[kind] += 1
            raw = src[lit.start:lit.end]
            text, _ = normalize(src, lit)
            if kind == "replace":
                seen_replace.add((path, raw))
            elif kind == "keep":
                seen_keep.add(raw if raw in KEEP_LITERALS else text)
            elif kind == "bare":
                if (path, raw) in BARE_KEEP:
                    seen_bare.add((path, raw))
                elif not is_test(path):
                    line = src.count("\n", 0, lit.start) + 1
                    errors.append(f"{path}:{line}: unclassified literal {raw!r}: add it to BARE_KEEP or REPLACE")
            if kind == "prose" and found:
                old_keys.add(key_form(text))
                new_keys.add(key_form(apply(text, mentions(text))))
            edits.extend(found)
        if not edits:
            continue
        files_changed += 1
        new_src = apply(src, edits)
        if mode == "diff":
            for (a, b) in zip(src.splitlines(), new_src.splitlines()):
                if a != b:
                    print(f"{path}\n  - {a.strip()}\n  + {b.strip()}")
        if not dry_run:
            file.write_text(new_src, encoding="utf-8")

    for rel in RESOURCES:
        file = root / rel
        if not file.exists():
            errors.append(f"resource {rel} is missing")
            continue
        text = file.read_text(encoding="utf-8")
        new = apply(text, mentions(text))
        if new != text:
            files_changed += 1
            if mode == "diff":
                for (a, b) in zip(text.splitlines(), new.splitlines()):
                    if a != b:
                        print(f"{rel}\n  - {a.strip()}\n  + {b.strip()}")
            if not dry_run:
                file.write_text(new, encoding="utf-8")

    for name in CATALOGS:
        path = root / name
        if not path.exists():
            continue
        if mode == "check":
            doc = json.loads(path.read_text())
            for key in doc.get("strings", {}):
                if key_form(key) in old_keys and key_form(rename_key(key)) not in new_keys:
                    errors.append(f"{name}: key {key!r} and its Swift literal would be renamed differently")
        keys, values, skipped, renamed, errs = process_catalog(path, dry_run=True if mode == "check" else dry_run)
        errors.extend(errs)
        print(f"{name}: {values} values in {keys} keys renamed ({renamed} keys renamed), {skipped} keys kept")

    print(f"Swift literals: {counts['prose']} prose, {counts['replace']} replaced, {counts['keep']} kept, "
          f"{counts['bare']} bare, {counts['ident']} identifiers; {files_changed} files changed")

    if mode == "check":
        for key in REPLACE:
            if key not in seen_replace:
                errors.append(f"REPLACE entry no longer matches: {key}")
        for key in BARE_KEEP:
            if key not in seen_bare:
                errors.append(f"BARE_KEEP entry no longer matches: {key}")
        for text in KEEP_LITERALS:
            if text not in seen_keep:
                errors.append(f"KEEP_LITERALS entry no longer matches: {text!r}")
        for error in errors:
            print(f"error: {error}", file=sys.stderr)
        if errors:
            print("flightdeck-catalogs: --check failed. Classify the literal in scripts/flightdeck-catalogs.py.",
                  file=sys.stderr)
            return 1
        print("flightdeck-catalogs: check passed")
        return 0
    for error in errors:
        print(f"error: {error}", file=sys.stderr)
    return 1 if errors else 0


def main(argv: list[str]) -> int:
    mode = "write"
    for arg in argv:
        if arg in ("--dry-run", "--check", "--diff"):
            mode = arg[2:]
        else:
            print(__doc__, file=sys.stderr)
            return 2
    return run(ROOT, mode)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
