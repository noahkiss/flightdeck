#!/usr/bin/env python3
"""Add one release to flightdeck's Sparkle appcast.

release.yml runs this on the `appcast` branch after it has signed the Mac zip.
It reads the current appcast.xml (or starts one), drops any item for the same
tag, puts the new item first and keeps the newest --keep items by build number.
Sparkle orders updates by <sparkle:version>, the CFBundleVersion, which is the
release run number, so a newer run always wins.
"""

import argparse
import email.utils
import os
import sys
import xml.etree.ElementTree as ET

SPARKLE = "http://www.andymatuschak.org/xml-namespaces/sparkle"
ET.register_namespace("sparkle", SPARKLE)


def s(tag):
    return f"{{{SPARKLE}}}{tag}"


def new_feed(title, link):
    rss = ET.Element("rss", {"version": "2.0"})
    channel = ET.SubElement(rss, "channel")
    ET.SubElement(channel, "title").text = title
    ET.SubElement(channel, "link").text = link
    ET.SubElement(channel, "description").text = f"{title} updates"
    ET.SubElement(channel, "language").text = "en"
    return rss


def item_tag(item):
    return item.findtext("guid") or ""


def item_build(item):
    try:
        return int(item.findtext(s("version")) or "0")
    except ValueError:
        return 0


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--appcast", required=True, help="appcast.xml to update (created if missing)")
    p.add_argument("--tag", required=True)
    p.add_argument("--build", required=True, type=int, help="CFBundleVersion of the app")
    p.add_argument("--short-version", required=True, help="text Sparkle shows as the version")
    p.add_argument("--url", required=True, help="download url of the zip")
    p.add_argument("--length", required=True, type=int, help="zip size in bytes")
    p.add_argument("--signature", required=True, help="sparkle:edSignature of the zip")
    p.add_argument("--min-os", required=True, help="LSMinimumSystemVersion of the app")
    p.add_argument("--notes-url", required=True, help="release page")
    p.add_argument("--title", default="FlightDeck")
    p.add_argument("--link", default="https://github.com/noahkiss/flightdeck")
    p.add_argument("--keep", type=int, default=10)
    p.add_argument("--date", help="RFC 2822 pubDate (default: now)")
    a = p.parse_args(argv)

    if os.path.exists(a.appcast):
        rss = ET.parse(a.appcast).getroot()
    else:
        rss = new_feed(a.title, a.link)
    channel = rss.find("channel")
    if channel is None:
        sys.exit(f"{a.appcast}: no <channel>")

    items = [i for i in channel.findall("item") if item_tag(i) != a.tag]
    for i in channel.findall("item"):
        channel.remove(i)

    item = ET.Element("item")
    ET.SubElement(item, "title").text = f"{a.title} {a.short_version}"
    ET.SubElement(item, "link").text = a.notes_url
    ET.SubElement(item, "guid", {"isPermaLink": "false"}).text = a.tag
    ET.SubElement(item, s("version")).text = str(a.build)
    ET.SubElement(item, s("shortVersionString")).text = a.short_version
    ET.SubElement(item, s("minimumSystemVersion")).text = a.min_os
    ET.SubElement(item, s("fullReleaseNotesLink")).text = a.notes_url
    ET.SubElement(item, "description").text = (
        f'<p>{a.title} {a.short_version}. <a href="{a.notes_url}">Release notes</a></p>'
    )
    ET.SubElement(item, "pubDate").text = a.date or email.utils.formatdate(usegmt=True)
    ET.SubElement(item, "enclosure", {
        "url": a.url,
        "length": str(a.length),
        "type": "application/octet-stream",
        s("edSignature"): a.signature,
    })

    items = sorted([item] + items, key=item_build, reverse=True)[: a.keep]
    for i in items:
        channel.append(i)

    ET.indent(rss, space="  ")
    tmp = a.appcast + ".tmp"
    ET.ElementTree(rss).write(tmp, encoding="utf-8", xml_declaration=True)
    with open(tmp, "a", encoding="utf-8") as f:
        f.write("\n")
    os.replace(tmp, a.appcast)
    print(f"{a.appcast}: {len(items)} items, newest {item_tag(items[0])} ({item_build(items[0])})")


if __name__ == "__main__":
    main()
