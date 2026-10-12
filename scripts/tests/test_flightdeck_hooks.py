#!/usr/bin/env python3
"""Tests for scripts/flightdeck-hooks.py. Run: python3 -m unittest scripts/tests/test_flightdeck_hooks.py"""

from __future__ import annotations

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "flightdeck-hooks.py"
spec = importlib.util.spec_from_file_location("flightdeck_hooks", SCRIPT)
fh = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fh)

UPSTREAM = """\
struct App {
    init() {
        guard ready else { return }
        FlightdeckHooks.launch()  // FLIGHTDECK-HOOK: app.launch
        let s = "{ not a brace"
    }

    func other() {
    }
}
"""

HOOKS = """\
enum FlightdeckHooks {
    static func launch() {}
}
struct FlightdeckExtras {}
"""

PBXPROJ = """\
		47AC7E570000000000000030 /* Sources */ = {
			isa = PBXSourcesBuildPhase;
			files = (
				AAAA /* rootshell/Core/Upstream.swift in Sources */,
				BBBB /* rootshell/Flightdeck/Zellij/Parser.swift in Sources */,
			);
		};
"""


class HookCheckTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.manifest = {
            "hooks": [{
                "id": "app.launch",
                "file": "rootshell/App/App.swift",
                "anchor": "init() {",
                "call": "FlightdeckHooks.launch()",
            }],
            "testSources": ["rootshell/Flightdeck/Zellij/Parser.swift"],
        }
        self.write("rootshell/App/App.swift", UPSTREAM)
        self.write("rootshell/Flightdeck/Core/FlightdeckHooks.swift", HOOKS)
        self.write("rootshell/Flightdeck/Zellij/Parser.swift", "struct Parser {}\n")
        self.write("rootshell.xcodeproj/project.pbxproj", PBXPROJ)
        self.write("FLIGHTDECK.md", f"# ledger\n\n{fh.TABLE_BEGIN}\n{fh.TABLE_END}\n")
        self.save_manifest()
        fh.write_ledger(self.root, self.manifest)

    def tearDown(self):
        self.tmp.cleanup()

    def write(self, rel: str, text: str):
        path = self.root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def save_manifest(self):
        self.write(fh.MANIFEST, json.dumps(self.manifest))

    def errors(self) -> list[str]:
        return fh.check(self.root)

    def assertError(self, fragment: str):
        errors = self.errors()
        self.assertTrue(any(fragment in e for e in errors), f"{fragment!r} not in {errors}")

    def test_clean_tree_passes(self):
        self.assertEqual(self.errors(), [])

    def test_missing_hook(self):
        self.write("rootshell/App/App.swift", UPSTREAM.replace(
            "        FlightdeckHooks.launch()  // FLIGHTDECK-HOOK: app.launch\n", ""))
        self.assertError("hook app.launch missing from init() {")

    def test_duplicate_hook(self):
        line = "        FlightdeckHooks.launch()  // FLIGHTDECK-HOOK: app.launch\n"
        self.write("rootshell/App/App.swift", UPSTREAM.replace(line, line + line))
        self.assertError("appears 2 times")

    def test_hook_outside_anchor(self):
        line = "        FlightdeckHooks.launch()  // FLIGHTDECK-HOOK: app.launch\n"
        moved = UPSTREAM.replace(line, "").replace("    func other() {\n", "    func other() {\n" + line)
        self.write("rootshell/App/App.swift", moved)
        self.assertError("is outside init() {")

    def test_string_braces_do_not_end_anchor(self):
        # The "{ not a brace" literal must not shift the block end.
        self.assertEqual(self.errors(), [])

    def test_missing_anchor(self):
        self.write("rootshell/App/App.swift", UPSTREAM.replace("init() {", "init(x: Int) {"))
        self.assertError("anchor 'init() {' is missing")

    def test_changed_call_text(self):
        self.write("rootshell/App/App.swift", UPSTREAM.replace(
            "FlightdeckHooks.launch()  //", "FlightdeckHooks.launch(early: true)  //"))
        self.assertError("no longer reads")

    def test_unlisted_marker(self):
        self.write("rootshell/Core/Other.swift", "x()  // FLIGHTDECK-HOOK: stray.hook\n")
        self.assertError("stray.hook at rootshell/Core/Other.swift:1 is not in")

    def test_undefined_hook_point(self):
        self.manifest["hooks"][0]["call"] = "FlightdeckHooks.launch()"
        self.write("rootshell/Flightdeck/Core/FlightdeckHooks.swift", "enum FlightdeckHooks {}\n")
        self.assertError("FlightdeckHooks.launch has no definition")

    def test_undefined_flightdeck_type(self):
        self.write("rootshell/App/App.swift", UPSTREAM.replace(
            "        FlightdeckHooks.launch()  // FLIGHTDECK-HOOK: app.launch\n",
            "        FlightdeckHooks.launch()  // FLIGHTDECK-HOOK: app.launch\n"
            "        var extras: FlightdeckMissing? = nil  // FLIGHTDECK-HOOK: app.extras\n"))
        self.manifest["hooks"].append({
            "id": "app.extras", "file": "rootshell/App/App.swift",
            "anchor": "init() {", "call": "var extras: FlightdeckMissing? = nil",
        })
        self.save_manifest()
        fh.write_ledger(self.root, self.manifest)
        self.assertError("FlightdeckMissing has no definition")

    def test_test_source_missing_from_target(self):
        self.write("rootshell.xcodeproj/project.pbxproj", PBXPROJ.replace(
            "				BBBB /* rootshell/Flightdeck/Zellij/Parser.swift in Sources */,\n", ""))
        self.assertError("is not in the rootshellTests target")

    def test_unlisted_test_source(self):
        self.manifest["testSources"] = []
        self.save_manifest()
        fh.write_ledger(self.root, self.manifest)
        self.assertError("which scripts/flightdeck-hooks.json does not list")

    def test_stale_ledger(self):
        self.write("FLIGHTDECK.md", f"# ledger\n\n{fh.TABLE_BEGIN}\n| old |\n{fh.TABLE_END}\n")
        self.assertError("hooks table is stale")

    def test_table_lists_hooks_and_test_sources(self):
        table = fh.render_table(self.manifest)
        self.assertIn("| `app.launch` | `rootshell/App/App.swift` | `init() {` | `FlightdeckHooks.launch()` |", table)
        self.assertIn("- `rootshell/Flightdeck/Zellij/Parser.swift`", table)

    def test_repository_passes(self):
        self.assertEqual(fh.check(fh.ROOT), [])


if __name__ == "__main__":
    unittest.main()
