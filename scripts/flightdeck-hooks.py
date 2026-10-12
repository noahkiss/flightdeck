#!/usr/bin/env python3
"""Check flightdeck's hook lines in upstream files against their manifest.

flightdeck code lives under rootshell/Flightdeck/. Upstream files reach it
only through one-line hooks of this form:

    FlightdeckHooks.<point>(...)  // FLIGHTDECK-HOOK: <id>

scripts/flightdeck-hooks.json lists every hook (id, file, the enclosing
function or type as `anchor`, and the exact `call` text). It also lists the
flightdeck sources that project.pbxproj adds to the rootshellTests target,
which has no test host and compiles only the app sources it lists.

--check exits 1 and names the problem when:
  - a manifest hook is missing from its file, or appears twice;
  - a hook line no longer carries its `call` text;
  - a hook sits outside its anchor, or the anchor is missing or ambiguous;
  - a FLIGHTDECK-HOOK marker exists that the manifest does not list;
  - a Flightdeck symbol named in a call has no definition under
    rootshell/Flightdeck/;
  - a listed test source is missing from the rootshellTests target, or the
    target lists a rootshell/Flightdeck/ source the manifest does not;
  - an `upstreamText` entry (upstream source text that flightdeck code
    matches on) is missing from its file;
  - the FLIGHTDECK.md hooks table differs from --table.

--table prints the FLIGHTDECK.md hooks table. --write rewrites it in place.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MANIFEST = "scripts/flightdeck-hooks.json"
FLIGHTDECK_DIR = "rootshell/Flightdeck"
PBXPROJ = "rootshell.xcodeproj/project.pbxproj"
LEDGER = "FLIGHTDECK.md"
TABLE_BEGIN = "<!-- flightdeck-hooks:begin -->"
TABLE_END = "<!-- flightdeck-hooks:end -->"
# The rootshellTests target's Sources build phase.
TEST_SOURCES_PHASE = "47AC7E570000000000000030"

MARKER_RE = re.compile(r"//\s*FLIGHTDECK-HOOK:\s*([A-Za-z0-9_.-]+)")
SKIP_DIRS = {".git", "SourcePackages", "DerivedData", "build", ".build", "node_modules"}
SCAN_SUFFIXES = {".swift", ".m", ".h", ".mm"}


def load_manifest(root: Path) -> dict:
    with open(root / MANIFEST, encoding="utf-8") as f:
        return json.load(f)


def strip_code(line: str) -> str:
    """Drop string literals and // comments so braces can be counted."""
    line = re.sub(r'"(?:\\.|[^"\\])*"', '""', line)
    return line.split("//", 1)[0]


def block_range(lines: list[str], start: int) -> tuple[int, int] | None:
    """Return the (first, last) line index of the brace block opened at or after `start`."""
    depth = 0
    opened = False
    for i in range(start, len(lines)):
        for ch in strip_code(lines[i]):
            if ch == "{":
                depth += 1
                opened = True
            elif ch == "}":
                depth -= 1
                if opened and depth == 0:
                    return start, i
    return None


def scan_markers(root: Path) -> dict[str, list[tuple[str, int]]]:
    """Map each marker id to the (file, line number) pairs that carry it."""
    found: dict[str, list[tuple[str, int]]] = {}
    for path in sorted(root.rglob("*")):
        if path.suffix not in SCAN_SUFFIXES or not path.is_file():
            continue
        rel = path.relative_to(root)
        if any(part in SKIP_DIRS for part in rel.parts):
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        for n, line in enumerate(text.splitlines(), 1):
            for m in MARKER_RE.finditer(line):
                found.setdefault(m.group(1), []).append((rel.as_posix(), n))
    return found


def flightdeck_sources(root: Path) -> str:
    parts = []
    base = root / FLIGHTDECK_DIR
    if base.is_dir():
        for path in sorted(base.rglob("*.swift")):
            parts.append(path.read_text(encoding="utf-8"))
    return "\n".join(parts)


def missing_definitions(call: str, sources: str) -> list[str]:
    missing = []
    for member in re.findall(r"\bFlightdeckHooks\.([A-Za-z_]\w*)", call):
        pattern = rf"\b(?:func|var|let|struct|class|enum|typealias)\s+{member}\b"
        if not re.search(pattern, sources):
            missing.append(f"FlightdeckHooks.{member}")
    for type_name in set(re.findall(r"\b(Flightdeck[A-Za-z_]\w*)\b", call)):
        pattern = rf"\b(?:struct|class|enum|protocol|typealias|actor)\s+{type_name}\b"
        if not re.search(pattern, sources):
            missing.append(type_name)
    return sorted(set(missing))


def test_sources_in_pbxproj(text: str) -> list[str] | None:
    """Return the source paths in the rootshellTests Sources phase, or None if absent."""
    m = re.search(
        rf"\t\t{TEST_SOURCES_PHASE} /\* Sources \*/ = \{{.*?files = \((.*?)\);",
        text,
        re.S,
    )
    if not m:
        return None
    return re.findall(r"/\* (\S+) in Sources \*/", m.group(1))


def check_hooks(root: Path, manifest: dict) -> list[str]:
    errors: list[str] = []
    hooks = manifest.get("hooks", [])
    sources = flightdeck_sources(root)
    markers = scan_markers(root)
    listed = set()

    for hook in hooks:
        hid, rel, anchor, call = hook["id"], hook["file"], hook["anchor"], hook["call"]
        if hid in listed:
            errors.append(f"hook {hid} is listed twice in {MANIFEST}")
        listed.add(hid)
        path = root / rel
        if not path.is_file():
            errors.append(f"hook {hid}: file {rel} is missing")
            continue
        lines = path.read_text(encoding="utf-8").splitlines()
        sites = [i for i, line in enumerate(lines)
                 if any(m.group(1) == hid for m in MARKER_RE.finditer(line))]
        if not sites:
            errors.append(f"hook {hid} missing from {anchor} in {rel}")
            continue
        if len(sites) > 1:
            numbers = ", ".join(str(i + 1) for i in sites)
            errors.append(f"hook {hid} appears {len(sites)} times in {rel} (lines {numbers})")
            continue
        site = sites[0]
        if call not in lines[site]:
            errors.append(f"hook {hid} at {rel}:{site + 1} no longer reads: {call}")
        anchors = [i for i, line in enumerate(lines) if anchor in line]
        if not anchors:
            errors.append(f"hook {hid}: anchor {anchor!r} is missing from {rel}")
        elif len(anchors) > 1:
            errors.append(f"hook {hid}: anchor {anchor!r} appears {len(anchors)} times in {rel}")
        else:
            span = block_range(lines, anchors[0])
            if span is None or not span[0] < site <= span[1]:
                errors.append(f"hook {hid} at {rel}:{site + 1} is outside {anchor}")
        for symbol in missing_definitions(call, sources):
            errors.append(f"hook {hid}: {symbol} has no definition under {FLIGHTDECK_DIR}/")

    for hid, sites in sorted(markers.items()):
        if hid not in listed:
            where = ", ".join(f"{f}:{n}" for f, n in sites)
            errors.append(f"marker FLIGHTDECK-HOOK: {hid} at {where} is not in {MANIFEST}")
    return errors


def check_test_sources(root: Path, manifest: dict) -> list[str]:
    errors: list[str] = []
    wanted = manifest.get("testSources", [])
    pbx = root / PBXPROJ
    if not pbx.is_file():
        return [f"{PBXPROJ} is missing"] if wanted else []
    present = test_sources_in_pbxproj(pbx.read_text(encoding="utf-8"))
    if present is None:
        return [f"rootshellTests Sources phase {TEST_SOURCES_PHASE} is missing from {PBXPROJ}"]
    for rel in wanted:
        if not (root / rel).is_file():
            errors.append(f"test source {rel} is missing")
        if rel not in present:
            errors.append(f"test source {rel} is not in the rootshellTests target")
    for rel in present:
        if rel.startswith(FLIGHTDECK_DIR + "/") and rel not in wanted:
            errors.append(f"rootshellTests compiles {rel}, which {MANIFEST} does not list")
    return errors


def check_upstream_text(root: Path, manifest: dict) -> list[str]:
    errors: list[str] = []
    for item in manifest.get("upstreamText", []):
        path = root / item["file"]
        if not path.is_file():
            errors.append(f"upstream text: file {item['file']} is missing")
        elif item["text"] not in path.read_text(encoding="utf-8"):
            errors.append(f"upstream text {item['text']!r} is missing from {item['file']}"
                          f" (used by {item['usedBy']})")
    return errors


def render_table(manifest: dict) -> str:
    rows = [
        "| Hook | Upstream file | Inside | Line |",
        "|---|---|---|---|",
    ]
    for hook in manifest.get("hooks", []):
        # A pipe inside a code span still splits a table cell unless escaped.
        cells = [hook[k].replace("|", "\\|") for k in ("id", "file", "anchor", "call")]
        rows.append("| " + " | ".join(f"`{c}`" for c in cells) + " |")
    tests = manifest.get("testSources", [])
    if tests:
        rows.append("")
        rows.append(f"`{PBXPROJ}` adds these files to the `rootshellTests` target"
                    " (four lines each: build file, file reference, group child, Sources entry):")
        rows.append("")
        rows.extend(f"- `{rel}`" for rel in tests)
    texts = manifest.get("upstreamText", [])
    if texts:
        rows.append("")
        rows.append("flightdeck code matches this upstream text, and `--check` fails if it changes:")
        rows.append("")
        rows.extend(f"- `{item['file']}`: `{item['text']}` (`{item['usedBy']}`)" for item in texts)
    return "\n".join(rows)


def ledger_table(text: str) -> str | None:
    if TABLE_BEGIN not in text or TABLE_END not in text:
        return None
    return text.split(TABLE_BEGIN, 1)[1].split(TABLE_END, 1)[0].strip("\n")


def check_ledger(root: Path, manifest: dict) -> list[str]:
    path = root / LEDGER
    if not path.is_file():
        return [f"{LEDGER} is missing"]
    current = ledger_table(path.read_text(encoding="utf-8"))
    if current is None:
        return [f"{LEDGER} has no {TABLE_BEGIN} ... {TABLE_END} block"]
    if current != render_table(manifest):
        return [f"{LEDGER} hooks table is stale; run scripts/flightdeck-hooks.py --write"]
    return []


def write_ledger(root: Path, manifest: dict) -> None:
    path = root / LEDGER
    text = path.read_text(encoding="utf-8")
    head, rest = text.split(TABLE_BEGIN, 1)
    _, tail = rest.split(TABLE_END, 1)
    path.write_text(f"{head}{TABLE_BEGIN}\n{render_table(manifest)}\n{TABLE_END}{tail}", encoding="utf-8")


def check(root: Path) -> list[str]:
    manifest = load_manifest(root)
    return (check_hooks(root, manifest)
            + check_test_sources(root, manifest)
            + check_upstream_text(root, manifest)
            + check_ledger(root, manifest))


def main(argv: list[str]) -> int:
    root = ROOT
    if "--table" in argv:
        print(render_table(load_manifest(root)))
        return 0
    if "--write" in argv:
        write_ledger(root, load_manifest(root))
        return 0
    if "--check" in argv:
        errors = check(root)
        for error in errors:
            print(f"flightdeck-hooks: {error}")
        if not errors:
            count = len(load_manifest(root).get("hooks", []))
            print(f"flightdeck-hooks: {count} hooks OK")
        return 1 if errors else 0
    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
