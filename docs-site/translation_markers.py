"""Translation review markers kept at the end of each docs-en/ page.

A translated page ends with a block of HTML comments that records what was
reviewed, so the facts live next to the translation they describe:

    <!-- translation-source-sha256: <sha256 of the Chinese docs/ page> -->
    <!-- translation-localized-code-block: <source sha256> <translation sha256> [reason] -->

Only the trailing run of marker lines (blank lines allowed in between) counts.
The site staging strips the block, so it never reaches the HTML or the PDF.
"""

import re
from dataclasses import dataclass, field


MARKER_RE = re.compile(r"^<!-- translation-([a-z0-9-]+): (.*?) -->$")
HEX_RE = re.compile(r"^[0-9a-f]{64}$")
SOURCE = "source-sha256"
CODE_BLOCK = "localized-code-block"


@dataclass
class Markers:
    source_sha256: str = ""
    # (source sha256, translation sha256) -> reason, in file order.
    code_blocks: dict = field(default_factory=dict)
    problems: list = field(default_factory=list)


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
    for line in split(text)[1]:
        kind, value = MARKER_RE.match(line).groups()
        if kind == SOURCE:
            if result.source_sha256:
                result.problems.append("duplicate translation-source-sha256 marker")
            elif not HEX_RE.match(value):
                result.problems.append("malformed translation-source-sha256 marker")
            else:
                result.source_sha256 = value
        elif kind == CODE_BLOCK:
            parts = value.split(" ", 2)
            if len(parts) < 2 or not all(HEX_RE.match(part) for part in parts[:2]):
                result.problems.append("malformed translation-localized-code-block marker")
            else:
                result.code_blocks[(parts[0], parts[1])] = parts[2] if len(parts) > 2 else ""
        else:
            result.problems.append(f"unknown translation-{kind} marker")
    return result


def render(body: str, source_sha256: str, code_blocks: dict) -> str:
    lines = [f"<!-- translation-{SOURCE}: {source_sha256} -->"]
    for (source, target), reason in code_blocks.items():
        suffix = f" {reason}" if reason else ""
        lines.append(f"<!-- translation-{CODE_BLOCK}: {source} {target}{suffix} -->")
    return body.rstrip("\n") + "\n\n" + "\n".join(lines) + "\n"
