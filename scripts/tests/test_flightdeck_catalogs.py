#!/usr/bin/env python3
"""Tests for scripts/flightdeck-catalogs.py. Run: python3 -m unittest scripts/tests/test_flightdeck_catalogs.py"""

from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "flightdeck-catalogs.py"
spec = importlib.util.spec_from_file_location("flightdeck_catalogs", SCRIPT)
fc = importlib.util.module_from_spec(spec)
sys.modules["flightdeck_catalogs"] = fc
spec.loader.exec_module(fc)


def rewrite(src: str, path: str = "rootshell/Example.swift") -> str:
    edits = []
    for lit in fc.swift_literals(src):
        found, _ = fc.literal_edits(path, src, lit)
        edits.extend(found)
    return fc.apply(src, edits)


class SwiftLiteralTests(unittest.TestCase):
    def test_prose_is_renamed(self):
        self.assertEqual(rewrite('Text("Start Using rootshell")'), 'Text("Start Using FlightDeck")')

    def test_identifiers_stay(self):
        src = 'let a = "com.kk2.rootshell"; let b = "rootshell"; let c = "rootshell://vpn"; let d = ".rootshellrc"'
        self.assertEqual(rewrite(src), src)

    def test_compound_names_in_prose_stay(self):
        src = 'Text("Run rootshell-notify setup, then edit ~/.rootshellrc or open push.rootshell.com.")'
        self.assertEqual(rewrite(src), src)

    def test_interpolation_code_is_not_text(self):
        src = 'Text("Hi \\(rootshellName) from rootshell")'
        self.assertEqual(rewrite(src), 'Text("Hi \\(rootshellName) from FlightDeck")')

    def test_nested_literal_in_interpolation(self):
        src = '"--path  \\(String(localized: "Prepend Rootshell\'s PATH wrapper", comment: "c"))"'
        self.assertIn("Prepend FlightDeck's PATH wrapper", rewrite(src))

    def test_multiline_and_raw_strings(self):
        src = 'let s = """\n    rootshell files - TUI file browser\n    """\nlet r = #"rootshell says "hi""#'
        out = rewrite(src)
        self.assertIn("FlightDeck files - TUI file browser", out)
        self.assertIn('#"FlightDeck says "hi""#', out)

    def test_comments_are_skipped(self):
        src = '// "a rootshell comment"\n/* "rootshell /* nested */ here" */ let x = 1'
        self.assertEqual(rewrite(src), src)

    def test_keep_literal_and_phrase(self):
        src = 'let a = "rootshell VPN"; Text("Based on rootshell by Kit Knox")'
        self.assertEqual(rewrite(src), src)

    def test_mcp_command_is_lowercase(self):
        self.assertEqual(rewrite('"claude mcp add rootshell -- nc localhost"'), '"claude mcp add flightdeck -- nc localhost"')

    def test_replace_entry(self):
        path, raw = next(iter(fc.REPLACE))
        src = f'let x = "{raw}"'
        self.assertEqual(rewrite(src, path), f'let x = "{fc.REPLACE[(path, raw)]}"')

    def test_escaped_newline_counts_as_whitespace(self):
        self.assertEqual(rewrite('"Version: rootshell\\n"'), '"Version: FlightDeck\\n"')


class HerdrRuleTests(unittest.TestCase):
    def test_english(self):
        key = "No control stream; rootshell will use fallback mode. Install the rootshell herdr fork for full control mode."
        self.assertEqual(fc.rename_key(key),
                         "No control stream; FlightDeck will use fallback mode. Install the rootshell herdr fork for full control mode.")
        self.assertEqual(fc.rename_key("This uses rootshell’s herdr fork, which tracks upstream herdr."),
                         "This uses rootshell’s herdr fork, which tracks upstream herdr.")

    def test_translations(self):
        de = "Kein Steuerungsstream; rootshell verwendet den Fallback-Modus. Installiere den herdr-Fork von rootshell."
        self.assertEqual(fc.rename_translation(de),
                         "Kein Steuerungsstream; FlightDeck verwendet den Fallback-Modus. Installiere den herdr-Fork von rootshell.")
        zh = "此主机上的 herdr 没有控制流，因此 rootshell 正在使用回退模式。请安装 rootshell 的 herdr 分支版本。"
        self.assertEqual(fc.rename_translation(zh),
                         "此主机上的 herdr 没有控制流，因此 FlightDeck 正在使用回退模式。请安装 rootshell 的 herdr 分支版本。")
        ko = "이 호스트의 herdr에는 제어 스트림이 없어 rootshell이 폴백 모드를 사용합니다. rootshell herdr 포크를 설치하십시오."
        self.assertEqual(fc.rename_translation(ko),
                         "이 호스트의 herdr에는 제어 스트림이 없어 FlightDeck이 폴백 모드를 사용합니다. rootshell herdr 포크를 설치하십시오.")


class TranslationTests(unittest.TestCase):
    def test_inflections_and_compounds(self):
        self.assertEqual(fc.rename_translation("Avaa rootshellissä"), "Avaa FlightDeckissä")
        self.assertEqual(fc.rename_translation("Rootshell-Helper erforderlich"), "FlightDeck-Helper erforderlich")
        self.assertEqual(fc.rename_translation("rootshell-helper läuft nicht"), "rootshell-helper läuft nicht")
        self.assertEqual(fc.rename_translation("~/.rootshellrc bearbeiten"), "~/.rootshellrc bearbeiten")

    def test_key_rule_matches_literal_rule(self):
        self.assertEqual(fc.rename_key("Rootshell wasn't granted access to %@."), "FlightDeck wasn't granted access to %@.")
        self.assertEqual(fc.rename_key("Rootshell"), "Rootshell")
        self.assertEqual(fc.rename_key("rootshell VPN"), "rootshell VPN")


if __name__ == "__main__":
    unittest.main()
