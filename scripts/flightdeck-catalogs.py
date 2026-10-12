#!/usr/bin/env python3
"""Rewrite the rootshell name to FlightDeck in the string catalogs, for a build.

CI runs this on its checkout just before xcodebuild (ci.yml and release.yml).
It never runs on a tracked tree: the catalogs in git stay upstream's, so the
nightly rebase never conflicts on them, and strings or languages that upstream
adds are renamed on the next build.

Only values change, never keys, so the Swift code still finds every string.
A key that names rootshell and has no English value gets one, because English
otherwise shows the key itself. Upstream's push relay host becomes "FlightDeck
Push", since FlightDeck never uses it. Other command, file and host names that
contain rootshell (.rootshellrc, rootshell-notify, ...) are kept,
and so are strings about upstream's own projects (its herdr fork, the "Based
on rootshell" credit).

Usage: scripts/flightdeck-catalogs.py [--dry-run]
"""

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CATALOGS = [
    "Localizable.xcstrings",
    "PushNotificationService/Localizable.xcstrings",
    "SessionActivityWidget/Localizable.xcstrings",
    "rootshell/AppShortcuts.xcstrings",
]
NAME = "FlightDeck"

# Names that contain rootshell but are not the app's name. They are masked
# before the rewrite and restored after it.
PROTECTED = [
    ".rootshellrc",
    "rootshell-askpass",
    "rootshell-channel",
    "rootshell-helper",
    "rootshell-notify",
    "rootshell-open-clipboard",
    "rootshell-open",
    "rootshell.com",
    "rootshellbackup",
    "rootshellvpn",
]
# Upstream's relay host, named in the push registration status. FlightDeck
# registers with its own relay, so the host is replaced, not kept.
HOSTS = {"push.rootshell.com": "FlightDeck Push"}
HOSTS_RE = re.compile("|".join(re.escape(h) for h in HOSTS), re.IGNORECASE)

# Entries left as they are, in every language: they name upstream's projects.
SKIP_KEY = re.compile(r"herdr fork|Based on rootshell", re.IGNORECASE)

# No boundary after the name: translations inflect it (rootshells, rootshellissä,
# rootshellben) and put CJK text right next to it. The masks above keep the
# compound names.
BRAND = re.compile(r"(?<![A-Za-z0-9_])rootshell", re.IGNORECASE)
PROTECTED_RE = re.compile("|".join(re.escape(t) for t in PROTECTED), re.IGNORECASE)


def rebrand(text: str) -> str:
    kept: list[str] = []

    def mask(m: re.Match) -> str:
        kept.append(m.group(0))
        return f"\x00{len(kept) - 1}\x00"

    text = HOSTS_RE.sub(lambda m: HOSTS[m.group(0).lower()], text)
    text = BRAND.sub(NAME, PROTECTED_RE.sub(mask, text))
    return re.sub(r"\x00(\d+)\x00", lambda m: kept[int(m.group(1))], text)


def rewrite_units(node, counter: list) -> None:
    if isinstance(node, dict):
        unit = node.get("stringUnit")
        if isinstance(unit, dict) and isinstance(unit.get("value"), str):
            new = rebrand(unit["value"])
            if new != unit["value"]:
                unit["value"] = new
                counter[0] += 1
        for value in node.values():
            rewrite_units(value, counter)
    elif isinstance(node, list):
        for value in node:
            rewrite_units(value, counter)


def process(path: Path, dry_run: bool) -> tuple[int, int, int]:
    raw = path.read_text()
    doc = json.loads(raw)
    source = doc.get("sourceLanguage", "en")
    values = [0]
    keys = skipped = 0
    for key, entry in doc.get("strings", {}).items():
        if not BRAND.search(key) and not BRAND.search(json.dumps(entry, ensure_ascii=False)):
            continue
        if SKIP_KEY.search(key):
            skipped += 1
            continue
        before = values[0]
        localizations = entry.setdefault("localizations", {})
        rewrite_units(localizations, values)
        if source not in localizations and rebrand(key) != key:
            localizations[source] = {"stringUnit": {"state": "translated", "value": rebrand(key)}}
            values[0] += 1
        if values[0] != before:
            keys += 1
    if not dry_run:
        path.write_text(json.dumps(doc, indent=2, sort_keys=True, ensure_ascii=False, separators=(",", " : ")) + "\n")
    return keys, values[0], skipped


def main() -> int:
    dry_run = "--dry-run" in sys.argv[1:]
    for name in CATALOGS:
        path = ROOT / name
        if not path.exists():
            continue
        keys, values, skipped = process(path, dry_run)
        print(f"{name}: {values} values in {keys} keys renamed, {skipped} keys kept")
    return 0


if __name__ == "__main__":
    sys.exit(main())
