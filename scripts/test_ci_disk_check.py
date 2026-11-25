"""Exercise the CI disk check against the real filesystem of this machine."""

import os
import pathlib
import re
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
CHECK = ROOT / "scripts/ci_disk_check.sh"


def run(paths, fail_gb, warn_gb="0"):
    env = dict(os.environ)
    env.update({
        "GPUWORKS_CI_DISK_PATHS": paths,
        "GPUWORKS_CI_DISK_FAIL_GB": fail_gb,
        "GPUWORKS_CI_DISK_WARN_GB": warn_gb,
        "CI_RUNNER_DESCRIPTION": "runner-under-test",
    })
    return subprocess.run(["sh", str(CHECK)], env=env, capture_output=True, text=True)


class DiskCheckTests(unittest.TestCase):
    def test_enough_space_passes_and_reports_each_mount_once(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = run(tmp + " " + tmp + "/.", "0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(re.findall(r"GiB free", result.stdout)), 1, result.stdout)

    def test_too_little_space_fails_and_names_the_runner(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = run(tmp, "1000000000")
        self.assertEqual(result.returncode, 1)
        self.assertIn("runner runner-under-test", result.stderr)
        self.assertIn("below the 1000000000 GiB this job needs", result.stderr)

    def test_low_space_under_the_warning_line_only_warns(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = run(tmp, "0", "1000000000")
        self.assertEqual(result.returncode, 0)
        self.assertIn("warning", result.stderr)

    def test_missing_paths_are_skipped(self):
        result = run("/nonexistent/ci-disk-check", "1000000000")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout + result.stderr, "")

    def test_unreachable_path_does_not_fail_the_job(self):
        with tempfile.TemporaryDirectory() as tmp:
            locked = pathlib.Path(tmp, "locked", "inner")
            locked.mkdir(parents=True)
            os.chmod(locked.parent, 0)
            try:
                result = run(str(locked), "0")
            finally:
                os.chmod(locked.parent, 0o700)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
