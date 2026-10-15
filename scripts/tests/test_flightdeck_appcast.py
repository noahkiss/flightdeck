#!/usr/bin/env python3
"""Tests for the FlightDeck Sparkle appcast generator."""

import subprocess
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github" / "scripts" / "flightdeck-appcast.py"
SPARKLE = "http://www.andymatuschak.org/xml-namespaces/sparkle"


class AppcastTests(unittest.TestCase):
    def test_release_summary_is_plain_text_and_full_notes_keep_the_release_url(self):
        notes_url = "https://github.com/noahkiss/flightdeck/releases/tag/v1.2.3-fd.0-rc.1"
        with tempfile.TemporaryDirectory() as directory:
            appcast = Path(directory) / "appcast.xml"
            subprocess.run(
                [
                    sys.executable,
                    str(SCRIPT),
                    "--appcast", str(appcast),
                    "--tag", "v1.2.3-fd.0-rc.1",
                    "--build", "123",
                    "--short-version", "1.2.3-fd.0-rc.1",
                    "--url", "https://example.com/FlightDeck.zip",
                    "--length", "456",
                    "--signature", "test-signature",
                    "--min-os", "15.0.0",
                    "--notes-url", notes_url,
                    "--date", "Wed, 15 Oct 2026 12:00:00 GMT",
                ],
                check=True,
                capture_output=True,
                text=True,
            )

            item = ET.parse(appcast).getroot().find("channel/item")
            self.assertIsNotNone(item)
            description = item.find("description")
            self.assertIsNotNone(description)
            self.assertEqual(description.text, "FlightDeck 1.2.3-fd.0-rc.1.")
            self.assertEqual(description.get(f"{{{SPARKLE}}}format"), "plain-text")
            self.assertNotIn("<", description.text)
            self.assertEqual(
                item.findtext(f"{{{SPARKLE}}}fullReleaseNotesLink"),
                notes_url,
            )


if __name__ == "__main__":
    unittest.main()
