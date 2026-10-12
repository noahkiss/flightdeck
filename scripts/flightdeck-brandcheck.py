#!/usr/bin/env python3
"""Fail when a built app still shows the rootshell name.

CI runs this on each built .app, after scripts/flightdeck-catalogs.py has
rewritten the checkout and xcodebuild has built it. It reads:

- every Mach-O file in the bundle, as `strings` would: runs of printable
  UTF-8 text (so curly quotes do not split a sentence);
- every compiled string table (*.lproj/*.strings, *.stringsdict), values in
  every language;
- every bundle's Info.plist, through the InfoPlist.strings that replace its
  values in each language;
- every other text file, and every file name.

The name may remain only where scripts/flightdeck-catalogs.py keeps it on
purpose (identifiers, PROTECTED names, KEEP_PHRASES, KEEP_LITERALS, the herdr
fork rule) or where KEEP below lists it with a reason. Anything else is
printed with its file and the script exits 1.

Usage: scripts/flightdeck-brandcheck.py [--report] <App.app> [<App.app> ...]
  --report  also print every kept mention, grouped by reason
"""

from __future__ import annotations

import importlib.util
import plistlib
import re
import sys
from collections import Counter
from pathlib import Path

_spec = importlib.util.spec_from_file_location(
    "flightdeck_catalogs", Path(__file__).resolve().parent / "flightdeck-catalogs.py")
fc = importlib.util.module_from_spec(_spec)
sys.modules["flightdeck_catalogs"] = fc
_spec.loader.exec_module(fc)

# What may remain in compiled code, beyond flightdeck-catalogs.py's rules.
# Each pattern matches a whole extracted string.
KEEP = [
    (r"(?i)rootshell", "bare identifier: module name, URL scheme, LC_TERMINAL, protocol names (BARE_KEEP reviews each source site)"),
    (r"rootshell Capture CA( \()?", "Capture CA name (KEEP_LITERALS); interpolation splits the literal"),
    (r"rootshell Virtual Display ?", "dependency kitknox/rootshell-vnc: Screen Sharing virtual display name; not rewritable from this repo"),
    (r"\[ rootshell tmux control mode \]", "prebuilt GhosttyKit (kitknox/ghosttykit-rootshell); not rewritable from this repo"),
    (r".*generic_renderer.*for rootshell cursor.*", "prebuilt GhosttyKit log message"),
    (r"`rootshell` renders a `#` symbol as the cursor.*", "prebuilt GhosttyKit config docs: the cursor style value is named rootshell"),
    (r"<(title|h2)>rootshell Capture CA</(title|h2)>|<p>Install this certificate so rootshell can decrypt HTTPS traffic.*",
     "prebuilt VPNTunnel (kitknox/trzsz-ssh-rootshell): Capture CA install page; HTTPS capture is off (row 5)"),
    (r"__OBJC_\S*", "Objective-C metadata symbol (debug builds); never displayed"),
    (r"Rootshell Encrypted Backup", "raw Info.plist copy embedded in debug binaries; InfoPlist.strings replaces it (checked on Info.plist)"),
]
KEEP = [(re.compile(p), why) for p, why in KEEP]

# Files that are never shown to anyone.
SKIP_FILE = [
    (re.compile(r"(^|/)_CodeSignature/|(^|/)CodeResources$"), "code signature manifest"),
    (re.compile(r"\.provisionprofile$"), "provisioning profile"),
    (re.compile(r"\.xcconfig$"), "build settings bundled by accident; never displayed"),
    (re.compile(r"\.car$"), "compiled asset catalog (images)"),
]

# File names users may see that keep the name.
KEEP_FILE_NAME = {
    "rootshell": "main executable (CFBundleExecutable = PRODUCT_NAME); renaming breaks @testable import and paths",
    "rootshell AMOLED": "bundled theme; its name is the saved theme setting, so renaming would orphan users' selection",
}

# Info.plist keys users see, by bundle type. Usage descriptions in plug-ins
# and extensions are never shown: the system asks on behalf of the app.
VISIBLE_ALWAYS = re.compile(r"^(CFBundleName|CFBundleDisplayName)$")
VISIBLE_APP = re.compile(r"^(NFCReaderUsageDescription|NS\w+UsageDescription)$")

MACHO_MAGIC = {b"\xcf\xfa\xed\xfe", b"\xce\xfa\xed\xfe", b"\xca\xfe\xba\xbe", b"\xfe\xed\xfa\xcf", b"\xbe\xba\xfe\xca"}
RUN = re.compile(rb"(?:[\x20-\x7e\t]|[\xc2-\xdf][\x80-\xbf]|[\xe0-\xef][\x80-\xbf]{2}|[\xf0-\xf4][\x80-\xbf]{3}){4,}")
NEEDLE = re.compile(rb"[Rr][Oo][Oo][Tt][Ss][Hh][Ee][Ll][Ll]")


class Report:
    def __init__(self) -> None:
        self.errors: list[str] = []
        self.kept: Counter = Counter()

    def keep(self, why: str) -> None:
        self.kept[why] += 1

    def fail(self, where: str, text: str) -> None:
        self.errors.append(f"{where}: {text.strip()[:240]!r}")


def leftover(text: str, pattern: re.Pattern) -> bool:
    """True when the text names rootshell where no rule keeps it."""
    return bool(fc.mentions(text, pattern))


def check_string(text: str, where: str, report: Report, pattern: re.Pattern = fc.WORD) -> None:
    if not re.search("rootshell", text, re.IGNORECASE):
        return
    if not leftover(text, pattern):
        report.keep("identifier or kept by flightdeck-catalogs.py rules")
        return
    stripped = text.strip()
    if stripped in fc.KEEP_LITERALS:
        report.keep(fc.KEEP_LITERALS[stripped])
        return
    for regex, why in KEEP:
        if regex.fullmatch(stripped):
            report.keep(why)
            return
    report.fail(where, text)


def load_plist(path: Path):
    raw = path.read_bytes()
    if raw[:2] in (b"\xff\xfe", b"\xfe\xff"):
        raw = raw.decode("utf-16").replace('encoding="UTF-8"', "").encode("utf-8")
    try:
        return plistlib.loads(raw)
    except Exception:
        return None


def strings_values(node):
    if isinstance(node, dict):
        for value in node.values():
            yield from strings_values(value)
    elif isinstance(node, list):
        for value in node:
            yield from strings_values(value)
    elif isinstance(node, str):
        yield node


def check_macho(path: Path, rel: str, report: Report) -> None:
    data = path.read_bytes()
    seen: set[bytes] = set()
    for m in RUN.finditer(data):
        chunk = m.group(0)
        if not NEEDLE.search(chunk) or chunk in seen:
            continue
        seen.add(chunk)
        check_string(chunk.decode("utf-8", "replace"), rel, report)


def check_lproj(path: Path, rel: str, report: Report) -> None:
    doc = load_plist(path)
    if doc is None:
        text = path.read_text(encoding="utf-8", errors="replace")
        for line in text.splitlines():
            check_string(line, rel, report, fc.BRAND)
        return
    for value in strings_values(doc):
        check_string(value, rel, report, fc.BRAND)


def bundle_kind(bundle: Path) -> str:
    return bundle.suffix.lstrip(".")


def check_info_plist(plist_path: Path, bundle: Path, app: Path, report: Report) -> None:
    info = load_plist(plist_path)
    if not isinstance(info, dict):
        return
    resources = plist_path.parent / "Resources" if plist_path.parent.name == "Contents" else plist_path.parent
    tables = {}
    for lproj in sorted(resources.glob("*.lproj")):
        strings = lproj / "InfoPlist.strings"
        tables[lproj.stem] = load_plist(strings) if strings.exists() else {}
    kind = bundle_kind(bundle)
    visible: list[tuple[str, str]] = []
    for key, value in info.items():
        if not isinstance(value, str):
            continue
        if VISIBLE_ALWAYS.match(key) or (kind == "app" and VISIBLE_APP.match(key)):
            visible.append((key, value))
    for decl in info.get("UTExportedTypeDeclarations", []) + info.get("UTImportedTypeDeclarations", []):
        if isinstance(decl.get("UTTypeDescription"), str):
            visible.append((decl["UTTypeDescription"], decl["UTTypeDescription"]))
    for doc_type in info.get("CFBundleDocumentTypes", []):
        if isinstance(doc_type.get("CFBundleTypeName"), str):
            visible.append((doc_type["CFBundleTypeName"], doc_type["CFBundleTypeName"]))
    rel = str(plist_path.relative_to(app.parent))
    for key, value in visible:
        if not leftover(value, fc.WORD):
            continue
        if not tables:
            report.fail(f"{rel} {key} (no localization)", value)
            continue
        missing = [lang for lang, table in tables.items()
                   if not isinstance(table, dict) or key not in table or leftover(str(table[key]), fc.BRAND)]
        if missing:
            report.fail(f"{rel} {key} (not replaced in {', '.join(missing[:6])}{'…' if len(missing) > 6 else ''})", value)
        else:
            report.keep("Info.plist value replaced by InfoPlist.strings in every language")


def check_app(app: Path, report: Report) -> None:
    for path in sorted(app.rglob("*")):
        rel = str(path.relative_to(app.parent))
        if any(fc.WORD.search(part) and not fc.kept_mention(part, 0, len(part)) for part in [path.name]):
            name = path.name
            if leftover(name, fc.WORD):
                if name in KEEP_FILE_NAME:
                    report.keep(KEEP_FILE_NAME[name])
                else:
                    report.fail(f"{rel} (file name)", name)
        if path.is_symlink() or not path.is_file():
            continue
        skip = next((why for regex, why in SKIP_FILE if regex.search(rel)), None)
        if skip:
            continue
        if path.name == "Info.plist" and (path.parent.name == "Contents" or path.parent.suffix in (".app", ".appex", ".bundle", ".framework", ".systemextension")):
            bundle = path.parent.parent if path.parent.name == "Contents" else path.parent
            check_info_plist(path, bundle, app, report)
            continue
        with open(path, "rb") as f:
            head = f.read(4)
        if head in MACHO_MAGIC:
            check_macho(path, rel, report)
            continue
        if path.parent.suffix == ".lproj" and path.suffix in (".strings", ".stringsdict"):
            check_lproj(path, rel, report)
            continue
        data = path.read_bytes()
        if not NEEDLE.search(data):
            continue
        if data[:6] == b"bplist" or path.suffix == ".plist":
            doc = load_plist(path)
            if doc is not None:
                for value in strings_values(doc):
                    check_string(value, rel, report)
                continue
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            for m in RUN.finditer(data):
                if NEEDLE.search(m.group(0)):
                    check_string(m.group(0).decode("utf-8", "replace"), rel, report)
            continue
        for line in text.splitlines():
            check_string(line, rel, report)


def main(argv: list[str]) -> int:
    show = "--report" in argv
    apps = [Path(a) for a in argv if a != "--report"]
    if not apps:
        print(__doc__, file=sys.stderr)
        return 2
    failed = False
    for app in apps:
        if not app.is_dir():
            print(f"error: {app} is not a directory", file=sys.stderr)
            return 2
        report = Report()
        check_app(app, report)
        kept = sum(report.kept.values())
        print(f"{app.name}: {len(report.errors)} visible rootshell strings, {kept} kept")
        if show:
            for why, count in report.kept.most_common():
                print(f"  kept {count:5d}  {why}")
        for error in report.errors:
            print(f"  error: {error}")
        failed |= bool(report.errors)
    if failed:
        print("flightdeck-brandcheck: rootshell is still visible. Rename it in scripts/flightdeck-catalogs.py, "
              "or keep it on purpose with a reason there or in KEEP here.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
