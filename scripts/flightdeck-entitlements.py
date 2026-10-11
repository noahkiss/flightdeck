#!/usr/bin/env python3
"""Derive flightdeck's *-flightdeck.entitlements from upstream's files.

Each derived file is the upstream file minus the capabilities flightdeck's
team cannot sign: push (aps-environment), upstream's associated domains, and,
in the iOS App Store file, Apple's request-gated multicast grant.

Run with no arguments to rewrite the derived files. Run with --check to exit 1
when a derived file no longer matches its upstream source (CI runs this, so an
upstream entitlement change shows up as a red build, not a silent drift).
"""

import plistlib
import sys
from pathlib import Path

ENTITLEMENTS = Path(__file__).resolve().parent.parent / "rootshell" / "Entitlements"

DROP = ["aps-environment", "com.apple.developer.associated-domains"]
SOURCES = {
    "AppStore": DROP + ["com.apple.developer.networking.multicast"],
    "AppStore-MacCatalyst": DROP,
    "Standalone": DROP,
    "Standalone-MacCatalyst": DROP,
}


def derive(name: str, drop: list[str]) -> bytes:
    with open(ENTITLEMENTS / f"{name}.entitlements", "rb") as f:
        data = plistlib.load(f)
    for key in drop:
        data.pop(key, None)
    return plistlib.dumps(data, sort_keys=False)


def main() -> int:
    check = "--check" in sys.argv[1:]
    stale = []
    for name, drop in SOURCES.items():
        target = ENTITLEMENTS / f"{name}-flightdeck.entitlements"
        wanted = derive(name, drop)
        if check:
            if not target.exists() or target.read_bytes() != wanted:
                stale.append(target.name)
        else:
            target.write_bytes(wanted)
    if stale:
        print("stale, rerun scripts/flightdeck-entitlements.py:", ", ".join(stale))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
