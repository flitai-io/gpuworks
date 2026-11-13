"""Exercise the CI scheduler checks and Make's failure boundary offline."""

import pathlib
import re
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
CHECK = ROOT / "scripts/check_ci_yaml.sh"


def change_job_needs(text, name, needs):
    pattern = r"(?ms)^" + re.escape(name) + r":\n.*?(?=^[A-Za-z0-9_.-]+:|\Z)"
    match = re.search(pattern, text)
    if match is None:
        raise AssertionError("missing fixture job " + name)
    block = re.sub(r"(?m)^  needs:.*\n(?:^    .*\n)*", "", match.group())
    block = block.replace(name + ":\n", name + ":\n  needs: " + needs + "\n", 1)
    return text[:match.start()] + block + text[match.end():]


class CIGateTests(unittest.TestCase):
    def check(self, text):
        with tempfile.TemporaryDirectory(prefix="gpuworks-ci-dag-") as directory:
            pathlib.Path(directory, ".gitlab-ci.yml").write_text(text)
            return subprocess.run([str(CHECK)], cwd=directory, text=True,
                                  capture_output=True, check=False)

    def test_current_pipeline_is_gated(self):
        result = self.check((ROOT / ".gitlab-ci.yml").read_text())
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_ui_shards_cannot_bypass_gate_with_empty_needs(self):
        text = change_job_needs((ROOT / ".gitlab-ci.yml").read_text(), "core-ui-test", "[]")
        result = self.check(text)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("core-ui-test can start without", result.stderr)

    def test_inherited_go_and_docker_jobs_cannot_bypass_gate(self):
        text = change_job_needs((ROOT / ".gitlab-ci.yml").read_text(), "core-ui", "[]")
        result = self.check(text)
        self.assertNotEqual(result.returncode, 0)
        for job in ("core-ui", "core-test", "core-l2", "core-l3"):
            self.assertIn(job + " can start without", result.stderr)

    def test_gate_cannot_depend_on_ui(self):
        text = change_job_needs((ROOT / ".gitlab-ci.yml").read_text(), "change-record", "[core-ui]")
        result = self.check(text)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("earlier stage", result.stderr)

    def test_gate_failure_cannot_be_ignored(self):
        text = (ROOT / ".gitlab-ci.yml").read_text().replace(
            "change-record:\n", "change-record:\n  allow_failure: true\n", 1)
        result = self.check(text)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("mandatory on_success job", result.stderr)

    def test_legacy_pipeline_without_gate_is_rejected(self):
        result = self.check("stages:\n  - core-prepare\n  - core\n"
                            "core-ui:\n  stage: core-prepare\n"
                            "core-test:\n  stage: core\n  needs: [core-ui]\n"
                            "core-ui-test:\n  stage: core\n  needs: []\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing change-record job", result.stderr)

    def test_make_parallel_failure_stops_ui_and_tests(self):
        text = (ROOT / "Makefile").read_text()
        targets = ("test", "test-unit", "test-unit-checked")
        rules = []
        prerequisites = set()
        for target in targets:
            match = re.search(r"(?m)^" + target + r":([^\n]*)\n((?:\t[^\n]*\n)*)", text)
            self.assertIsNotNone(match, target)
            rules.append(match.group())
            prerequisites.update(match.group(1).split())
        for goal in ("test", "test-unit"):
            with self.subTest(goal=goal), tempfile.TemporaryDirectory(prefix="gpuworks-make-gate-") as directory:
                fixture = pathlib.Path(directory, "Makefile")
                bodies = [".PHONY: " + " ".join(prerequisites | set(targets)) + "\n"] + rules
                for target in sorted(prerequisites - set(targets)):
                    recipe = "sleep 0.1; exit 9" if target == "check-change-record" else "touch work-started"
                    bodies.append(target + ":\n\t@" + recipe + "\n")
                fixture.write_text("\n".join(bodies))
                result = subprocess.run(["make", "--no-print-directory", "-j8", goal],
                                        cwd=directory, text=True, capture_output=True, check=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("check-change-record", result.stderr)
                self.assertFalse(pathlib.Path(directory, "work-started").exists(), result.stdout)


if __name__ == "__main__":
    unittest.main()
