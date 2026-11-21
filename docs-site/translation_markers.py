"""Translation review markers kept at the end of each docs-en/ page.

Every heading of level 1-3 opens a section; text before the first such heading
belongs to the first section. A translated page ends with a block of HTML
comments that records what was reviewed: one line per section, in page order,
with the reviewed sha256 of the matching section of the Chinese docs/ page and
the English heading it belongs to, then any approved localized code blocks:

    <!-- translation-source-sha256: <sha256 of the Chinese section> ## Nodes -->
    <!-- translation-localized-code-block: <source sha256> <translation sha256> [reason] -->

Changes to different sections rewrite different lines of the block, so merge
requests that edit the same page only conflict over it when they edit the same
or neighbouring sections.
Only the trailing run of marker lines (blank lines allowed in between) counts.
A marker line anywhere else outside code blocks is reported: it is usually left
over from resolving a merge conflict on the block.
The site staging strips the block, so it never reaches the HTML or the PDF, and
the GitHub release takes its notes from this module run as a script:

    python3 docs-site/translation_markers.py docs-en/releases/<tag>.md
"""

import hashlib
import re
import sys
from dataclasses import dataclass, field

from markdown_headings import headings, outside_fences


MARKER_RE = re.compile(r"^<!-- translation-([a-z0-9-]+): (.*?) -->$")
HEX_RE = re.compile(r"^[0-9a-f]{64}$")
SOURCE = "source-sha256"
CODE_BLOCK = "localized-code-block"
SECTION_LEVEL = 3


@dataclass
class Markers:
    # One per recorded section in page order; "" when the line is malformed.
    section_sha256: list = field(default_factory=list)
    # (source sha256, translation sha256) -> reason, in file order.
    code_blocks: dict = field(default_factory=dict)
    problems: list = field(default_factory=list)


def section_headings(text: str) -> list[tuple[int, str]]:
    return [(index, f"{level} {title}") for index, level, title in headings(text) if len(level) <= SECTION_LEVEL]


def section_starts(text: str) -> list[int]:
    return [index for index, _ in section_headings(text)]


def section_digests(source: str) -> list[str]:
    """Return the sha256 of each section of a Chinese page."""
    starts = section_starts(source)
    if not starts:
        return []
    lines = source.splitlines(keepends=True)
    bounds = [0, *starts[1:], len(lines)]
    return [hashlib.sha256("".join(lines[begin:end]).encode()).hexdigest()
            for begin, end in zip(bounds, bounds[1:])]


def split(text: str) -> tuple[str, list[str]]:
    """Return the page body without the trailing marker block, and the marker lines."""
    lines = text.splitlines()
    end = len(lines)
    start = end
    while start > 0 and (not lines[start - 1].strip() or MARKER_RE.match(lines[start - 1])):
        start -= 1
    markers = [line for line in lines[start:end] if line.strip()]
    if not markers:
        return text, []
    return "\n".join(lines[:start]).rstrip("\n") + "\n", markers


def strip(text: str) -> str:
    return split(text)[0]


def parse(text: str) -> Markers:
    result = Markers()
    body, trailing = split(text)
    for index, line in outside_fences(body):
        if MARKER_RE.match(line):
            result.problems.append(f"translation marker on line {index + 1} is outside the trailing block")
    for line in trailing:
        kind, value = MARKER_RE.match(line).groups()
        if kind == SOURCE:
            digest = value.split(" ", 1)[0]
            if not HEX_RE.match(digest):
                result.problems.append("malformed translation-source-sha256 marker")
                digest = ""
            result.section_sha256.append(digest)
        elif kind == CODE_BLOCK:
            parts = value.split(" ", 2)
            if len(parts) < 2 or not all(HEX_RE.match(part) for part in parts[:2]):
                result.problems.append("malformed translation-localized-code-block marker")
            else:
                result.code_blocks[(parts[0], parts[1])] = parts[2] if len(parts) > 2 else ""
        else:
            result.problems.append(f"unknown translation-{kind} marker")
    return result


def render(body: str, section_sha256: list[str], code_blocks: dict) -> str:
    sections = section_headings(body)
    if len(sections) != len(section_sha256):
        raise ValueError(f"{len(section_sha256)} section hashes for {len(sections)} sections")
    lines = [f"<!-- translation-{SOURCE}: {digest} {heading} -->"
             for (_, heading), digest in zip(sections, section_sha256)]
    for (source, target), reason in code_blocks.items():
        suffix = f" {reason}" if reason else ""
        lines.append(f"<!-- translation-{CODE_BLOCK}: {source} {target}{suffix} -->")
    return body.rstrip("\n") + "\n\n" + "\n".join(lines) + "\n"


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: translation_markers.py <docs-en page>  (prints the page without its review markers)")
    with open(sys.argv[1], encoding="utf-8") as page:
        sys.stdout.write(strip(page.read()))
