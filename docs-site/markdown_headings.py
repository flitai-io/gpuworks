"""ATX headings of a Markdown page, ignoring fenced code blocks."""

import re


def headings(text: str) -> list[tuple[int, str, str]]:
    result = []
    fence = ""
    for index, line in enumerate(text.splitlines()):
        marker = re.match(r"^\s*(`{3,}|~{3,})", line)
        if marker:
            if not fence:
                fence = marker[1]
            elif marker[1][0] == fence[0] and len(marker[1]) >= len(fence):
                fence = ""
            continue
        match = re.match(r"^(#{1,6})\s+(.+?)\s*$", line)
        if match and not fence:
            result.append((index, match[1], match[2]))
    return result
