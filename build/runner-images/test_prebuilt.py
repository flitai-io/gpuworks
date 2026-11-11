"""Stdlib-only script orchestration tests; no registry or daemon is used."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class PrebuiltMatrixTest(unittest.TestCase):
    def test_batch_scripts_skip_external_artifacts_but_keep_built_images(self):
        here = Path(__file__).parent
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("push.sh", "verify.sh"):
                shutil.copyfile(here / name, root / name)
            # The fixture is JSON (a YAML subset). This narrow yaml adapter
            # keeps orchestration tests offline on CI images without PyYAML;
            # production YAML syntax is checked by the manifest generator.
            (root / "yaml.py").write_text("from json import load as safe_load\n")
            (root / "matrix.yaml").write_text(json.dumps({"items": [
                {"id": "external", "builder": "prebuilt", "revision": "r1"},
                {"id": "mac", "builder": "host-env", "revision": "r1"},
                {"id": "built", "revision": "r2"},
            ]}))
            for name in ("docker", "skopeo"):
                command = root / name
                command.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CALLS"\nexit "${COMMAND_EXIT:-1}"\n')
                command.chmod(0o755)
            calls = root / "calls"
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"], PYTHONPATH=str(root),
                       CALLS=str(calls), COMMAND_EXIT="1", SRC="source.example/runner", REGISTRY="source.example/runner")
            result = subprocess.run(["bash", str(root / "push.sh"), "--dest", "dest.example/runner",
                                     "--dry-run"], env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("source.example/runner:built-r2 -> dest.example/runner:built-r2", result.stdout)
            self.assertNotIn("external-r1", result.stdout)
            self.assertIn("source.example/runner:mac-r1 -> dest.example/runner:mac-r1", result.stdout)
            self.assertFalse(calls.exists(), "dry run must not copy images")
            result = subprocess.run(["bash", str(root / "push.sh"), "--dest", "dest.example/runner",
                                     "--tags-from", "matrix"], env=dict(env, COMMAND_EXIT="0"),
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(sorted(calls.read_text().splitlines()), [
                "copy --all --preserve-digests docker://source.example/runner:built-r2 docker://dest.example/runner:built-r2",
                "copy --all --preserve-digests docker://source.example/runner:mac-r1 docker://dest.example/runner:mac-r1",
            ])
            calls.unlink()
            result = subprocess.run(["bash", str(root / "verify.sh"), "--all"],
                                    env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 1, "the unavailable built image must still fail")
            self.assertIn("SKIP external: prebuilt", result.stderr)
            self.assertIn("source.example/runner:built-r2", calls.read_text())
            self.assertNotIn("external-r1", calls.read_text())
            self.assertNotIn("mac-r1", calls.read_text())


if __name__ == "__main__":
    unittest.main()
