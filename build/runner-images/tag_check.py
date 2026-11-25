#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Refuse to publish a runner tag whose name is outside the release grammar.

The grammar is not written here. tag-grammar.txt is exported by
`make image-manifest` from cmd/gpuworks-image-manifest, the one place that
defines it, and this module only reads that file. push.sh and publish.py both
call in, so a name the catalog generator would reject cannot reach a registry.

A destination tag passes when it
  * matches the grammar (<id>-r<number>), or
  * is the bare <id> of a grammar tag: build.sh --alias publishes it as a
    moving, human-friendly alias (README.md, key convention 1). The catalog
    never lists a bare alias as a build; or
  * is an existing tag declared with a legacy_reason under `legacy_tags:` in
    matrix.yaml (copying a tag that already exists creates no new name), or
  * starts with zz-pilot-, the scratch prefix of the publishing rehearsals in
    README.md. Scratch tags are for trying a registry out and are deleted
    afterwards.

Stdlib only, Python 3.6 compatible, no PyYAML (same constraints as publish.py).

  tag_check.py TAG...     exit 1 and name each refused tag
"""
from __future__ import print_function

import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SCRATCH_PREFIX = "zz-pilot-"


def load_grammar(path=None):
    path = path or os.path.join(HERE, "tag-grammar.txt")
    with open(path, encoding="utf-8") as f:
        lines = [ln.strip() for ln in f if ln.strip() and not ln.startswith("#")]
    if len(lines) != 1:
        raise ValueError("%s: expected one expression, found %d" % (path, len(lines)))
    return re.compile(lines[0])


def declared_legacy(path=None):
    """Tags under `legacy_tags:` that carry a legacy_reason."""
    path = path or os.path.join(HERE, "matrix.yaml")
    tags, tag, in_legacy_tags = set(), None, False
    with open(path, encoding="utf-8") as f:
        for ln in f:
            if re.match(r"^\S", ln) and not ln.startswith("#"):
                in_legacy_tags = ln.startswith("legacy_tags:")
                continue
            if not in_legacy_tags:
                continue
            if re.match(r"^  - ", ln):
                tag = None
            m = re.match(r"^\s+(?:- )?tag:\s*[\"']?([^\s\"']+)", ln)
            if m:
                tag = m.group(1)
            m = re.match(r"^\s+(?:- )?legacy_reason:\s*\S", ln)
            if m and tag:
                tags.add(tag)
    return tags


def refusal(tag, grammar=None, legacy=None, matrix=None):
    """Why tag may not be published, or "" when it may."""
    grammar = grammar or load_grammar()
    # The bare alias is the grammar without its revision; asking the one
    # expression about <tag>-r0 avoids a second expression for it.
    if grammar.match(tag) or grammar.match(tag + "-r0") or tag.startswith(SCRATCH_PREFIX):
        return ""
    if legacy is None:
        matrix = matrix or os.path.join(HERE, "matrix.yaml")
        legacy = declared_legacy(matrix) if os.path.exists(matrix) else set()
    if tag in legacy:
        return ""
    return ("%s is outside the runner tag grammar (<backend><version>[-<model>]-<engine><version>-r<number>) "
            "and is not a declared legacy tag in matrix.yaml; rename it" % tag)


def main(argv=None):
    tags = (sys.argv[1:] if argv is None else argv)
    grammar = load_grammar()
    refused = [r for r in (refusal(t, grammar) for t in tags) if r]
    for r in refused:
        print("REFUSED " + r, file=sys.stderr)
    return 1 if refused else 0


if __name__ == "__main__":
    sys.exit(main())
