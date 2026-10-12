#!/usr/bin/env python3
"""Derive flightdeck's InfoPlist.xcstrings files from upstream's Info.plist text.

Upstream writes its permission prompts and extension names in English, with
the rootshell name in them, and ships no InfoPlist localization. This script
collects every localizable Info.plist value that names rootshell, from three
places:

- target-level INFOPLIST_KEY_* settings in project.pbxproj,
- INFOPLIST_KEY_* lines in Configuration/Standalone.xcconfig,
- the Info.plist files themselves.

It rewrites the name to FlightDeck and writes one InfoPlist.xcstrings beside
each Info.plist, with the same value in every language the project knows. The
system reads a localized value in place of the Info.plist value, and every
language has one, so no language falls back to the rootshell text. Each folder
is a file-system-synchronized group, so the catalogs join their targets with no
project.pbxproj edit.

Run with no arguments to rewrite the catalogs. Run with --check to exit 1 when
a catalog no longer matches upstream's text or language list (CI runs this).
"""

import json
import plistlib
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PBXPROJ = ROOT / "rootshell.xcodeproj" / "project.pbxproj"
STANDALONE_XCCONFIG = ROOT / "Configuration" / "Standalone.xcconfig"

NAME = "FlightDeck"
BRAND = re.compile(r"\broot ?shell\b", re.IGNORECASE)

# Info.plist keys the system localizes and shows to the user.
LOCALIZABLE = re.compile(r"^(CFBundleName|CFBundleDisplayName|NFCReaderUsageDescription|NS\w+UsageDescription)$")

# Info.plist files that get a catalog. Every folder here is a
# PBXFileSystemSynchronizedRootGroup of the targets that use the plist.
PLISTS = [
    "rootshell/Info.plist",
    "rootshell-helper/Sources/Info.plist",
    "rootshellvpn/Info.plist",
    "tunnel/Info.plist",
    "VPNTunnelExtension/Info.plist",
    "PushNotificationService/Info.plist",
]

# Values upstream does not write as a literal, or that flightdeck words
# differently instead of renaming.
EXTRA = {
    # The Mac menu bar, Help, Services and the Dock use CFBundleName, which the
    # app targets take from PRODUCT_NAME (rootshell).
    "rootshell/Info.plist": {"CFBundleName": NAME},
}
# Upstream's microphone prompt describes the AI voice agent, which flightdeck
# turns off.
MICROPHONE = {"NSMicrophoneUsageDescription": f"{NAME} uses the microphone for voice input when you turn it on."}
OVERRIDES = {
    "rootshell/Info.plist": MICROPHONE,
    "rootshellvpn/Info.plist": MICROPHONE,
}


def rebrand(text: str) -> str:
    text = re.sub(r"\bthe rootshell shell\b", NAME, text, flags=re.IGNORECASE)
    return BRAND.sub(NAME, text)


def pbx_value(raw: str) -> str:
    raw = raw.strip()
    if raw.startswith('"') and raw.endswith('"'):
        raw = raw[1:-1].replace('\\"', '"').replace("\\\\", "\\")
    return raw


def languages(pbx: str) -> list[str]:
    m = re.search(r"knownRegions = \((.*?)\);", pbx, re.DOTALL)
    regions = [pbx_value(r) for r in m.group(1).split(",") if r.strip()]
    return sorted(r for r in regions if r != "Base")


def add(found: dict, plist: str, key: str, value: str, where: str) -> None:
    keys = found.setdefault(plist, {})
    if key in keys and keys[key][0] != value:
        sys.exit(f"{plist} {key}: two upstream values ({keys[key][1]} and {where}); "
                 "pick one in scripts/flightdeck-infoplist.py")
    keys.setdefault(key, (value, where))


def collect(pbx: str) -> dict:
    found: dict = {}
    for block in re.finditer(r"isa = XCBuildConfiguration;.*?buildSettings = \{(.*?)\n\t\t\t\};", pbx, re.DOTALL):
        settings = dict(
            (m.group(1), pbx_value(m.group(2)))
            for m in re.finditer(r"^\t+([A-Za-z0-9_]+) = (.*);$", block.group(1), re.MULTILINE)
        )
        for setting, value in settings.items():
            key = setting.removeprefix("INFOPLIST_KEY_")
            if key == setting or not LOCALIZABLE.match(key) or not BRAND.search(value):
                continue
            plist = settings.get("INFOPLIST_FILE")
            if plist not in PLISTS:
                sys.exit(f"project.pbxproj: {setting} names rootshell in a target whose "
                         f"INFOPLIST_FILE ({plist}) has no catalog")
            add(found, plist, key, value, "project.pbxproj")
    for m in re.finditer(r"^INFOPLIST_KEY_(\w+)(?:\[[^\]]*\])? = (.*)$", STANDALONE_XCCONFIG.read_text(), re.MULTILINE):
        if LOCALIZABLE.match(m.group(1)) and BRAND.search(m.group(2)):
            add(found, "rootshell/Info.plist", m.group(1), m.group(2).strip(), "Standalone.xcconfig")
    for plist in PLISTS:
        with open(ROOT / plist, "rb") as f:
            data = plistlib.load(f)
        for key, value in data.items():
            if LOCALIZABLE.match(key) and isinstance(value, str) and BRAND.search(value):
                add(found, plist, key, value, plist)
    return found


def catalog(strings: dict, langs: list[str]) -> str:
    entries = {}
    for key, value in sorted(strings.items()):
        unit = {"stringUnit": {"state": "translated", "value": value}}
        entries[key] = {
            "comment": "Generated by scripts/flightdeck-infoplist.py. Do not edit.",
            "extractionState": "manual",
            "localizations": {lang: unit for lang in langs},
        }
    doc = {"sourceLanguage": "en", "strings": entries, "version": "1.0"}
    return json.dumps(doc, indent=2, sort_keys=True, ensure_ascii=False, separators=(",", " : ")) + "\n"


def main() -> int:
    check = "--check" in sys.argv[1:]
    pbx = PBXPROJ.read_text()
    langs = languages(pbx)
    found = collect(pbx)
    stale = []
    for plist in PLISTS:
        strings = {key: rebrand(value) for key, (value, _) in found.get(plist, {}).items()}
        strings.update(EXTRA.get(plist, {}))
        strings.update(OVERRIDES.get(plist, {}))
        target = ROOT / Path(plist).parent / "InfoPlist.xcstrings"
        if not strings:
            if target.exists():
                stale.append(str(target.relative_to(ROOT)))
                if not check:
                    target.unlink()
            continue
        wanted = catalog(strings, langs)
        if check:
            if not target.exists() or target.read_text() != wanted:
                stale.append(str(target.relative_to(ROOT)))
        else:
            target.write_text(wanted)
    if stale:
        print("stale, rerun scripts/flightdeck-infoplist.py:", ", ".join(stale))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
