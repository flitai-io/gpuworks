"""Assign open checklist gaps to the coverage-completion phases.

Run from the repo root:
  PYTHONPATH=test/e2e:scripts python3 -m coverage_completion_queue
Registration is not a pass. Covered declarations stay out of the queue.
"""
from __future__ import annotations

from collections import Counter
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))

from e2e_coverage_components import components  # noqa: E402
from e2e_coverage_inventory import inventory  # noqa: E402

# Sections closed in full by phases 1–10. Form items 1.4/1.5/1.6 wait
# until the control section they depend on is closed.
SECTION_PHASE = {"12": "1", "5": "3", "10": "4", "22": "5", "6": "6", "8": "7", "13": "8", "2": "9", "3": "10"}
FORM_SHELL = {"1.1", "1.2", "1.3", "1.7", "1.8", "1.9", "1.10", "1.11", "1.12", "1.13", "1.14", "1.15"}
# Phase 11 lifts modules that are still under 80% after those ten sections.
MODULE_SECTION = {
    ("playground", "20"), ("logs", "18"), ("inference-templates", "15"),
    ("evaluations", "17"), ("apikeys", "21"), ("deployments", "17"),
    ("terminal", "19"), ("settings", "7"), ("assistant", "23"),
}


def item_id(label):
    return str(label).split()[0].split("[")[0]


def phase_for(module, item, provider_section):
    if item in FORM_SHELL:
        return "2"
    if item == "1.5":
        return "3"
    if item == "1.6":
        return "6"
    if item == "1.4":
        return "10"
    section = item.split(".")[0]
    if section in SECTION_PHASE:
        return SECTION_PHASE[section]
    if (module, section) in MODULE_SECTION or (module == "providers" and section == provider_section):
        return "11"
    return "out"


def provider_section(rows):
    """Smallest residual section on 外部平台 after phases 1–10."""
    gaps = Counter()
    for row in rows:
        if row.get("module") != "providers" or row.get("dimension") != "checklist" or not row.get("denominator"):
            continue
        section = row["name"].split(".", 1)[0].strip()
        if section in SECTION_PHASE or section == "1":
            continue
        gaps[section] += len(row.get("missing") or []) + len(row.get("unknown") or [])
    positive = {section: count for section, count in gaps.items() if count}
    return min(positive, key=lambda section: (positive[section], int(section))) if positive else None


def queue(root):
    data = inventory(Path(root))
    rows = components(Path(root), data)
    extra = provider_section(rows)
    assigned = []
    for row in rows:
        if row.get("dimension") != "checklist" or not row.get("denominator"):
            continue
        for bucket in ("missing", "unknown"):
            for label in row.get(bucket) or []:
                item = item_id(label)
                assigned.append({"phase": phase_for(row["module"], item, extra), "module": row["module"],
                                 "manifest": row["manifest"], "item": item, "bucket": bucket})
    return extra, assigned


def summary(root=ROOT):
    extra, assigned = queue(root)
    counts = Counter(item["phase"] for item in assigned)
    return extra, counts, len(assigned)


def main():
    extra, counts, total = summary()
    print(f"provider residual section: {extra}")
    for phase in [str(number) for number in range(1, 12)] + ["out"]:
        print(f"phase {phase}: {counts[phase]}")
    print(f"assigned gaps: {total}")


if __name__ == "__main__":
    main()
