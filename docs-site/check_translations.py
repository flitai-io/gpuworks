#!/usr/bin/env python3
"""Check translation coverage and the source revision actually reviewed."""

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path

import translation_markers
from stage_docs import headings


ROOT = Path(__file__).resolve().parents[1]


def code_blocks(text):
    blocks = []
    current = []
    fence = ""
    for line in text.splitlines():
        marker = re.match(r"^\s*(`{3,}|~{3,})(.*)$", line)
        if not fence and marker:
            fence = marker[1]
            current = [line]
        elif fence:
            current.append(line)
            if marker and marker[1][0] == fence[0] and len(marker[1]) >= len(fence) and not marker[2].strip():
                blocks.append("\n".join(current))
                fence = ""
    if fence:
        raise ValueError("unclosed code fence")
    return blocks


def code_block_pairs(original, translated):
    """Yield the sha256 pairs of code blocks the translation changed."""
    for source, target in zip(code_blocks(original), code_blocks(translated)):
        if source != target:
            yield tuple(hashlib.sha256(block.encode()).hexdigest() for block in (source, target))


def code_blocks_match(original, translated, approved=()):
    """Code blocks must be identical unless the pair is approved on the page."""
    if len(code_blocks(original)) != len(code_blocks(translated)):
        return False
    return all(pair in approved for pair in code_block_pairs(original, translated))


def inspect(root):
    source = root / "docs"
    english = root / "docs-en"
    originals = {p.relative_to(source).as_posix(): p for p in source.rglob("*.md")}
    translations = {p.relative_to(english).as_posix(): p for p in english.rglob("*.md")}
    errors = []
    for name, target in sorted(translations.items()):
        if name not in originals:
            errors.append(f"{name}: missing Chinese source")
            continue
        original = originals[name]
        text = target.read_text()
        markers = translation_markers.parse(text)
        errors.extend(f"{name}: {problem}" for problem in markers.problems)
        digest = hashlib.sha256(original.read_bytes()).hexdigest()
        if markers.source_sha256 != digest:
            errors.append(f"{name}: translation source changed or review hash missing")
        old, new = original.read_text(), translation_markers.strip(text)
        if [h[1] for h in headings(old)] != [h[1] for h in headings(new)]:
            errors.append(f"{name}: heading structure changed")
        if not code_blocks_match(old, new, markers.code_blocks):
            errors.append(f"{name}: code blocks differ")
    return {"source_pages": len(originals), "translated_pages": len(translations),
            "missing": sorted(originals.keys() - translations.keys()), "errors": errors}


def page_name(argument, root):
    """Accept docs/<path>, docs-en/<path> or <path> (relative to docs/)."""
    path = Path(argument)
    if path.is_absolute():
        path = path.resolve().relative_to(root)
    if path.parts and path.parts[0] in ("docs", "docs-en"):
        path = Path(*path.parts[1:])
    return path.as_posix()


def write(root, names, approve_code_blocks=False):
    """Record the current Chinese source hash in each matching docs-en page."""
    for name in names:
        original, target = root / "docs" / name, root / "docs-en" / name
        if not original.is_file():
            raise SystemExit(f"{name}: no Chinese source at docs/{name}")
        if not target.is_file():
            raise SystemExit(f"{name}: no translation at docs-en/{name}")
        text = target.read_text()
        markers = translation_markers.parse(text)
        body = translation_markers.strip(text)
        approved = markers.code_blocks
        if approve_code_blocks:
            approved = {pair: markers.code_blocks.get(pair, "")
                        for pair in code_block_pairs(original.read_text(), body)}
        digest = hashlib.sha256(original.read_bytes()).hexdigest()
        target.write_text(translation_markers.render(body, digest, approved))
        print(f"recorded docs-en/{name}: source sha256 {digest}", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    parser.add_argument("--write", nargs="+", metavar="PAGE",
                        help="record the current docs/ source hash in the matching docs-en/ page "
                             "(docs/<path>, docs-en/<path> or <path>), after reviewing the translation")
    parser.add_argument("--approve-code-blocks", action="store_true",
                        help="with --write, also approve every code block the translation currently "
                             "changes and drop approvals that no longer apply")
    args = parser.parse_args()
    if args.approve_code_blocks and not args.write:
        parser.error("--approve-code-blocks requires --write")
    if args.write:
        write(ROOT, [page_name(name, ROOT) for name in args.write], args.approve_code_blocks)
    result = inspect(ROOT)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return bool(result["errors"] or (args.require_complete and result["missing"]))


if __name__ == "__main__":
    raise SystemExit(main())
