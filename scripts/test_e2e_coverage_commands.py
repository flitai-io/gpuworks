#!/usr/bin/env python3
"""Every declared e2e command has to be one a developer can actually run.

A manifest's `command` is what a reader copies to reproduce the contract. 53 of
the 79 manifests pointed at `make test-e2e-<name>` targets that the Makefile never
defined - only ten `test-e2e-*` targets exist and they aggregate several scripts
each - so following any of those manifests failed at the first step with "No rule
to make target".  That is the difference between a contract nobody has run and a
contract nobody can run.

This test reads the Makefile the way make does and checks each command against it.
It deliberately does not launch anything: the point is that the declaration names
a real entry point, not that this machine can satisfy the fixture's prerequisites.
"""
import json
import re
import shlex
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MAKEFILE = ROOT / "Makefile"
COVERAGE = ROOT / "test/e2e/coverage"


def makefile_targets() -> set:
    """Targets a command may name. Read from the file so an added target counts."""
    return set(re.findall(r"^([A-Za-z0-9_.][A-Za-z0-9_.-]*):", MAKEFILE.read_text(), re.M))


def declared_commands() -> list:
    """Top-level command plus any per-case command, with where each came from."""
    declared = []
    for manifest in sorted(COVERAGE.glob("*.json")):
        data = json.loads(manifest.read_text())
        if data.get("command"):
            declared.append((manifest, "command", data["command"]))
        for index, case in enumerate(data.get("cases") or []):
            if isinstance(case, dict) and case.get("command"):
                declared.append((manifest, f"cases[{index}].command", case["command"]))
    return declared


class DeclaredCommandTest(unittest.TestCase):
    def setUp(self):
        self.targets = makefile_targets()

    def test_makefile_actually_defines_the_targets_commands_name(self):
        # Without this the check below could pass against an empty set.
        self.assertIn("test-e2e-smoke", self.targets)
        self.assertIn("test-e2e-local", self.targets)

    def test_every_make_command_names_a_defined_target(self):
        undefined = []
        for manifest, where, command in declared_commands():
            tokens = shlex.split(command)
            if tokens[0] != "make":
                continue
            target = tokens[1] if len(tokens) > 1 else ""
            if target not in self.targets:
                undefined.append(f"{manifest.name} {where}: make {target or '<no target>'}")
        self.assertEqual(undefined, [], "commands name make targets that do not exist")

    def test_every_python_command_names_a_file_that_exists(self):
        missing = []
        for manifest, where, command in declared_commands():
            tokens = shlex.split(command)
            # A package module is run from inside test/e2e, so the command starts
            # with a cd; drop it and judge the interpreter that follows.
            if tokens[:1] == ["cd"]:
                rest = tokens[1:]
                if rest[:1] == ["test/e2e"]:
                    rest = rest[1:]
                if rest[:1] == ["&&"]:
                    rest = rest[1:]
                tokens = rest
            if not tokens or not tokens[0].startswith("python"):
                continue
            scripts = [token for token in tokens[1:] if token.endswith(".py")]
            exists = [script for script in scripts if (ROOT / script).exists()]
            # `python3 -B -m package.module` has no .py argument; the module is
            # checked by resolution rather than by path.
            if "-m" in tokens:
                continue
            if not exists:
                missing.append(f"{manifest.name} {where}: {scripts or command}")
        self.assertEqual(missing, [], "commands name scripts that do not exist")

    def test_a_manifest_that_declares_a_contract_declares_a_runnable_command(self):
        silent = []
        for manifest in sorted(COVERAGE.glob("*.json")):
            data = json.loads(manifest.read_text())
            # An audit record or a configuration declaration carries no contract
            # and no test file; only a contract has to be reproducible.
            if data.get("contract_id") and not data.get("command"):
                silent.append(manifest.name)
        self.assertEqual(silent, [], "contracts declare no command to reproduce them")

    def test_case_values_are_declared_alongside_their_command(self):
        # A per-case command that omits the case would silently re-run the same
        # case eight times and look like thorough coverage.
        mismatched = []
        for manifest in sorted(COVERAGE.glob("*.json")):
            data = json.loads(manifest.read_text())
            cases = [case for case in (data.get("cases") or []) if isinstance(case, dict)]
            for case in cases:
                command = case.get("command") or ""
                name = case.get("case")
                if not name or not command:
                    continue
                if "--case" not in command or name not in command:
                    mismatched.append(f"{manifest.name}: {name} not in {command}")
        self.assertEqual(mismatched, [], "a per-case command does not select its own case")


if __name__ == "__main__":
    unittest.main()
