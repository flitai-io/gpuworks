"""Stdlib-only tests of the publish-time tag check; no registry is used."""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import tag_check  # noqa: E402

MATRIX = """\
items:
  - id: cuda13.0-vllm0.30.0
    revision: r4
    tag: not-a-published-tag
    legacy_reason: items are not the inventory
legacy_tags:
  - id: cuda13.0-vllm0.34.0
    tag: cuda13.0-vllm0.34.0-amd64-slot3
    legacy_reason: Existing tag.
    platforms: [linux/amd64]
  - id: cuda13.0-vllm0.34.0
    tag: cuda13.0-vllm0.34.0-arm64-slot3
    platforms: [linux/arm64]
"""


class TagCheckTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.matrix = self.root / "matrix.yaml"
        self.matrix.write_text(MATRIX)

    def refusal(self, tag):
        return tag_check.refusal(tag, matrix=str(self.matrix))

    def test_grammar_alias_declared_and_scratch_tags_pass(self):
        for tag in ("cuda13.0-vllm0.34.0-r1", "metal15.0-llama.cpp-b12631-r2", "cann9.1-910b-vllm-omni0.30.0-r3",
                    "cuda13.0-vllm0.34.0", "cuda13.0-vllm0.34.0-amd64-slot3", "zz-pilot-anything"):
            self.assertEqual(self.refusal(tag), "", tag)

    def test_everything_else_is_refused_by_name(self):
        for tag in ("cuda13.0-vllm0.34.0-r1-amd64", "cuda13.0-vllm0.34.0-amd64-r1", "latest",
                    "cuda13.0-vllm0.34.0-r", "cuda13.0-vllm0.34.0-r1x", "xcuda13.0-vllm0.34.0-r1",
                    # listed under published without a declaration
                    "cuda13.0-vllm0.34.0-arm64-slot3",
                    # declared, but under items
                    "not-a-published-tag"):
            problem = self.refusal(tag)
            self.assertIn(tag + " is outside the runner tag grammar", problem)

    def test_declarations_come_only_from_the_published_section(self):
        self.assertEqual(tag_check.declared_legacy(str(self.matrix)), {"cuda13.0-vllm0.34.0-amd64-slot3"})

    def test_missing_matrix_refuses_legacy_tags_instead_of_passing_them(self):
        self.matrix.unlink()
        self.assertEqual(self.refusal("cuda13.0-vllm0.34.0-r1"), "")
        self.assertNotEqual(self.refusal("cuda13.0-vllm0.34.0-amd64-slot3"), "")

    def test_grammar_file_must_hold_exactly_one_expression(self):
        bad = self.root / "tag-grammar.txt"
        bad.write_text("# only a comment\n")
        with self.assertRaises(ValueError):
            tag_check.load_grammar(str(bad))

    def test_cli_names_every_refused_tag(self):
        result = subprocess.run([sys.executable, str(HERE / "tag_check.py"), "cuda13.0-vllm0.34.0-r1", "latest", "foo"],
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 1)
        self.assertIn("REFUSED latest", result.stderr)
        self.assertIn("REFUSED foo", result.stderr)
        self.assertNotIn("cuda13.0-vllm0.34.0-r1", result.stderr)

    def test_the_checked_in_matrix_declares_what_the_check_reads(self):
        # Every declaration in the real matrix is visible to the line parser:
        # the two counts differ only if the YAML is written in a shape the
        # parser does not read.
        text = (HERE / "matrix.yaml").read_text(encoding="utf-8")
        self.assertEqual(len(tag_check.declared_legacy()), text.count("\n    legacy_reason: "))


class PublishScriptsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        for name in ("push.sh", "publish.py", "tag_check.py", "tag-grammar.txt"):
            shutil.copyfile(HERE / name, self.root / name)
        (self.root / "matrix.yaml").write_text(MATRIX)
        self.calls = self.root / "calls"
        skopeo = self.root / "skopeo"
        skopeo.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CALLS"\n')
        skopeo.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.root) + os.pathsep + os.environ["PATH"],
                        CALLS=str(self.calls), SRC="source.example/runner")

    def push(self, *args):
        return subprocess.run(["bash", str(self.root / "push.sh"), "--dest", "dest.example/runner"] + list(args),
                              env=self.env, capture_output=True, text=True, timeout=10)

    def test_push_copies_a_grammar_tag(self):
        result = self.push("--only", "cuda13.0-vllm0.34.0-r1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("docker://dest.example/runner:cuda13.0-vllm0.34.0-r1", self.calls.read_text())

    def test_push_refuses_a_tag_outside_the_grammar_before_copying(self):
        result = self.push("--only", "cuda13.0-vllm0.34.0-r1-amd64")
        self.assertEqual(result.returncode, 2)
        self.assertIn("REFUSED cuda13.0-vllm0.34.0-r1-amd64", result.stderr)
        self.assertFalse(self.calls.exists(), "nothing may be copied")

    def test_push_fails_closed_without_the_grammar_file(self):
        (self.root / "tag-grammar.txt").unlink()
        result = self.push("--only", "cuda13.0-vllm0.34.0-r1")
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists(), "nothing may be copied")

    def publish(self, *args):
        # The check runs before any registry or credential access, so a
        # refused tag never reaches the network; an accepted one is stopped
        # at the first registry read by the unroutable source.
        return subprocess.run([sys.executable, str(self.root / "publish.py")] + list(args) +
                              ["--dry-run", "--src", "127.0.0.1:1/runner", "--env-file", str(self.root / "none")],
                              env=self.env, capture_output=True, text=True, timeout=60)

    def test_publish_refuses_a_destination_tag_outside_the_grammar(self):
        result = self.publish("cuda13.0-vllm0.34.0-r1", "swr", "--dest-tag", "cuda13.0-vllm0.34.0-r1-amd64")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("cuda13.0-vllm0.34.0-r1-amd64 is outside the runner tag grammar", result.stdout + result.stderr)

    def test_publish_fails_closed_without_the_checker(self):
        (self.root / "tag_check.py").unlink()
        result = self.publish("cuda13.0-vllm0.34.0-r1", "swr")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("cannot check the destination tag", result.stdout + result.stderr)

    def test_publish_lets_a_grammar_tag_past_the_check(self):
        result = self.publish("cuda13.0-vllm0.34.0-r1", "swr", "--retries", "1")
        self.assertNotIn("outside the runner tag grammar", result.stdout + result.stderr)
        self.assertNotIn("cannot check the destination tag", result.stdout + result.stderr)
        self.assertNotEqual(result.returncode, 2, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
