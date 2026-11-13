#!/bin/sh
set -eu

# A YAML folded scalar ("- >") joins its lines with single spaces, so a '#'
# written inside the block is not a YAML comment: it survives into the
# assembled one-line shell command and comments out everything after it. That
# silently truncated the Ascend L4 docker invocation past its options, leaving
# "docker run" with no image (job 152755). The failure only appears when the
# job actually runs, which for a manual L4 lane can be weeks later, so keep the
# check mechanical here.
#
# Both CI definitions are checked for that: .gitlab-ci.yml (the L4 lanes) and
# the GitHub workflows. The stage-order check further down is GitLab's alone.
python3 - "$@" <<'PY'
import pathlib
import sys

def folded_comment_violations(path):
    lines = path.read_text().splitlines()
    found = []
    index = 0
    while index < len(lines):
        line = lines[index]
        stripped = line.strip()
        # Folded scalars only: "|" keeps newlines, so '#' there is a real comment.
        if stripped == "- >" or stripped.endswith(": >") or stripped == ">":
            indent = len(line) - len(line.lstrip())
            index += 1
            while index < len(lines):
                body = lines[index]
                if body.strip() and (len(body) - len(body.lstrip())) <= indent:
                    break
                if body.lstrip().startswith("#"):
                    found.append((path, index + 1, body.strip()))
                index += 1
            continue
        index += 1
    return found

path = pathlib.Path(".gitlab-ci.yml")
workflows = sorted(pathlib.Path(".github/workflows").glob("*.y*ml"))
violations = []
for ci_file in ([path] if path.exists() else []) + workflows:
    violations += folded_comment_violations(ci_file)

if violations:
    print("'#' comment inside a folded (>) scalar; it becomes a shell", file=sys.stderr)
    print("comment in the joined command and swallows the rest of the line.", file=sys.stderr)
    print("Move the note above the '- >' instead.", file=sys.stderr)
    for ci_file, number, text in violations:
        print("  %s:%d: %s" % (ci_file, number, text), file=sys.stderr)
    raise SystemExit(1)

if not path.exists():
    print("ci yaml check passed (%d workflows, no .gitlab-ci.yml)" % len(workflows))
    raise SystemExit(0)
lines = path.read_text().splitlines()

# GitLab 14.0 (the instance's version) only lets needs name a job of an
# earlier stage; a job in the same stage fails the whole pipeline at creation
# with "need X is not defined in prior stages" (MR !344). Read stages, stage,
# extends and needs with the file's own layout: two-space job keys, needs as a
# "- job: X" or "- X" list, or an inline list.
import re

stages = []
jobs = {}
current = None
section = None
for line in lines:
    if not line.strip() or line.lstrip().startswith("#"):
        continue
    top = re.match(r"^([A-Za-z0-9_.-]+):\s*(.*)$", line)
    if top:
        current = None if top.group(1) in ("stages", "default", "workflow", "variables", "include") else top.group(1)
        section = "stages" if top.group(1) == "stages" else None
        if current:
            jobs[current] = {}
        continue
    if section == "stages":
        item = re.match(r"^\s+-\s+(\S+)\s*$", line)
        if item:
            stages.append(item.group(1))
        continue
    if current is None:
        continue
    key = re.match(r"^  ([a-z_]+):\s*(.*?)\s*$", line)
    if key:
        name, value = key.group(1), key.group(2)
        section = name
        if name in ("stage", "extends", "when", "allow_failure") and value:
            jobs[current][name] = value
        elif name == "rules":
            jobs[current][name] = value
        elif name == "needs":
            inline = value.strip()
            jobs[current]["needs"] = [x.strip() for x in inline.strip("[]").split(",") if x.strip()] if inline else []
        continue
    if section == "needs":
        item = re.match(r"^    -\s+(?:job:\s*)?([A-Za-z0-9_.-]+)\s*$", line)
        if item:
            jobs[current]["needs"].append(item.group(1))

def inherited(job, field):
    seen = set()
    while job in jobs and job not in seen:
        seen.add(job)
        if field in jobs[job]:
            return jobs[job][field]
        job = jobs[job].get("extends")
    return None

order = {stage: i for i, stage in enumerate(stages)}
stage_violations = []
for job in jobs:
    if job.startswith("."):
        continue
    stage = inherited(job, "stage") or "test"
    for need in inherited(job, "needs") or []:
        if need not in jobs:
            continue
        need_stage = inherited(need, "stage") or "test"
        if order.get(need_stage, len(order)) >= order.get(stage, len(order)):
            stage_violations.append((job, stage, need, need_stage))

if stage_violations:
    print("%s: needs must name a job of an earlier stage (GitLab 14.0):" % path, file=sys.stderr)
    for job, stage, need, need_stage in stage_violations:
        print("  %s (stage %s) needs %s (stage %s)" % (job, stage, need, need_stage), file=sys.stderr)
    raise SystemExit(1)

# A needs graph bypasses stage barriers. Keep every real job behind the
# change-record gate, including jobs that override their template's needs.
# In contrast, a job without needs waits for successful earlier stages.
gate = "change-record"
gate_errors = []
if gate not in jobs:
    gate_errors.append("missing change-record job")
else:
    gate_stage = inherited(gate, "stage") or "test"
    if order.get(gate_stage) != 0:
        gate_errors.append("change-record must be in the first stage")
    if inherited(gate, "needs") != []:
        gate_errors.append("change-record must have needs: [] (no UI prerequisite)")
    if "rules" in jobs[gate]:
        gate_errors.append("change-record must exist in every pipeline")
    if inherited(gate, "allow_failure") == "true" or inherited(gate, "when") not in (None, "on_success"):
        gate_errors.append("change-record must be a mandatory on_success job")

    def waits_for_gate(job, visiting=frozenset()):
        if job == gate:
            return True
        if job not in jobs or job in visiting or inherited(job, "when") == "always":
            return False
        needs = inherited(job, "needs")
        if needs is None:
            stage = inherited(job, "stage") or "test"
            return order.get(stage, -1) > order.get(gate_stage, -1)
        return any(waits_for_gate(need, visiting | {job}) for need in needs)

    for job in jobs:
        if not job.startswith(".") and job != gate and not waits_for_gate(job):
            gate_errors.append("%s can start without a successful change-record gate" % job)

if gate_errors:
    print("%s: change-record must finish before builds or tests:" % path, file=sys.stderr)
    for error in gate_errors:
        print("  %s" % error, file=sys.stderr)
    raise SystemExit(1)

print("ci yaml check passed (%d jobs, %d needs)" % (len([j for j in jobs if not j.startswith(".")]), sum(len(inherited(j, "needs") or []) for j in jobs if not j.startswith("."))))
PY
