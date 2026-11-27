"""Build-input contracts. Git and the shell wrapper are real; the Go driver
reports the date it was given and the manifest its compiler would read.
"""

from datetime import datetime
import os
from pathlib import Path
import select
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("seed_catalog.sh").resolve()
CATALOG = Path("internal/server/inferencecatalog/seed/catalog")
FIRST = "2026-11-20T01:02:03+00:00"
SECOND = "2026-11-21T04:05:06+00:00"
GO_DRIVER = """#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
if sys.argv[1] == "run":
    print(sys.argv[sys.argv.index("--published-at") + 1])
elif sys.argv[1] == "build":
    if os.environ.get("PAUSE_COMPILER"):
        print("ready", flush=True)
        assert input() == "continue"
    overlay = Path(sys.argv[2].removeprefix("-overlay="))
    mapping = json.loads(overlay.read_text())["Replace"]
    manifest = Path(mapping["internal/server/inferencecatalog/seed/release-manifest.yaml"])
    print(manifest.read_text().strip())
else:
    raise SystemExit(2)
"""


class SeedCatalogBuildTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        go = self.root / "go-driver"
        go.write_text(GO_DRIVER)
        go.chmod(0o755)
        self.env = dict(os.environ, GO=str(go))
        for name in ("SEED_CATALOG_MANIFEST", "SEED_CATALOG_PUBLISHED_AT", "PAUSE_COMPILER"):
            self.env.pop(name, None)

    def git(self, cwd, *args, date=FIRST):
        subprocess.run(["git", "-c", "user.name=Seed Test", "-c", "user.email=seed@example.invalid",
                        "-c", "commit.gpgsign=false", *args], cwd=cwd, check=True, capture_output=True,
                       env=dict(self.env, GIT_AUTHOR_DATE=date, GIT_COMMITTER_DATE=date))

    def invoke(self, cwd, action, **env):
        return subprocess.run([SCRIPT, action], cwd=cwd, text=True, capture_output=True,
                              timeout=20, env=dict(self.env, **env))

    def assert_manifest_date(self, cwd, expected, **env):
        result = self.invoke(cwd, "manifest", **env)
        self.assertEqual(result.returncode, 0, result.stderr)
        timestamp_format = "%Y-%m-%dT%H:%M:%S%z"
        self.assertEqual(datetime.strptime(result.stdout.strip(), timestamp_format),
                         datetime.strptime(expected, timestamp_format))

    def test_shallow_boundary_needs_history_or_explicit_date(self):
        repo = self.root / "repo"
        (repo / CATALOG).mkdir(parents=True)
        (repo / CATALOG / "manifest.yaml").write_text("schema_version: 3\n")
        self.git(repo, "init", "-q")
        self.git(repo, "add", ".")
        self.git(repo, "commit", "-qm", "catalog")
        self.git(repo, "commit", "--allow-empty", "-qm", "unrelated", date=SECOND)
        self.assert_manifest_date(repo, FIRST)
        shallow = self.root / "shallow"
        self.git(self.root, "clone", "--depth=1", repo.as_uri(), str(shallow))
        refused = self.invoke(shallow, "manifest")
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("SEED_CATALOG_PUBLISHED_AT", refused.stderr)
        self.assert_manifest_date(shallow, FIRST, SEED_CATALOG_PUBLISHED_AT=FIRST)
        self.git(shallow, "fetch", "--unshallow")
        self.assert_manifest_date(shallow, FIRST)

    def test_shallow_clone_with_a_visible_catalog_change_uses_its_date(self):
        repo = self.root / "repo"
        (repo / CATALOG).mkdir(parents=True)
        self.git(repo, "init", "-q")
        self.git(repo, "commit", "--allow-empty", "-qm", "initial")
        self.git(repo, "commit", "--allow-empty", "-qm", "boundary")
        (repo / CATALOG / "manifest.yaml").write_text("schema_version: 3\n")
        self.git(repo, "add", ".")
        self.git(repo, "commit", "-qm", "add catalog", date=SECOND)
        shallow = self.root / "shallow"
        self.git(self.root, "clone", "--depth=2", repo.as_uri(), str(shallow))
        self.assert_manifest_date(shallow, SECOND)

    def test_replaying_a_catalog_change_keeps_its_publication_time(self):
        repo = self.root / "repo"
        (repo / CATALOG).mkdir(parents=True)
        self.git(repo, "init", "-q")
        self.git(repo, "commit", "--allow-empty", "-qm", "initial")
        self.git(repo, "branch", "-M", "release")
        self.git(repo, "checkout", "-qb", "templates")
        (repo / CATALOG / "manifest.yaml").write_text("schema_version: 3\n")
        self.git(repo, "add", ".")
        self.git(repo, "commit", "-qm", "catalog", date=FIRST)
        self.assert_manifest_date(repo, FIRST)

        self.git(repo, "checkout", "-q", "release")
        self.git(repo, "commit", "--allow-empty", "-qm", "unrelated release change", date=SECOND)
        self.git(repo, "checkout", "-qb", "backport")
        self.git(repo, "cherry-pick", "templates", date=SECOND)
        with self.subTest("cherry-pick"):
            self.assert_manifest_date(repo, FIRST)
        self.git(repo, "checkout", "-q", "templates")
        self.git(repo, "rebase", "release", date=SECOND)
        with self.subTest("rebase"):
            self.assert_manifest_date(repo, FIRST)

    def test_build_rejects_an_invalid_supplied_manifest(self):
        empty = self.root / "provided.yaml"
        empty.touch()
        for path, reason in [(empty, "empty seed manifest"), (self.root / "missing.yaml", "readable file")]:
            with self.subTest(path=path):
                result = self.invoke(self.root, "build", SEED_CATALOG_MANIFEST=str(path))
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("SEED_CATALOG_MANIFEST", result.stderr)
                self.assertIn(str(path), result.stderr)
                self.assertIn(reason, result.stderr)

    def test_concurrent_builds_keep_their_own_manifest_until_compile_finishes(self):
        with subprocess.Popen([SCRIPT, "build"], cwd=self.root, text=True, stdin=subprocess.PIPE,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              env=dict(self.env, SEED_CATALOG_PUBLISHED_AT=FIRST, PAUSE_COMPILER="1")) as first:
            try:
                self.assertTrue(select.select([first.stdout], [], [], 20)[0], "compiler did not start")
                self.assertEqual(first.stdout.readline().strip(), "ready")
                second = self.invoke(self.root, "build", SEED_CATALOG_PUBLISHED_AT=SECOND)
                self.assertEqual(second.returncode, 0, second.stderr)
                self.assertEqual(second.stdout.strip(), SECOND)
                output, error = first.communicate("continue\n", timeout=20)
                self.assertEqual(first.returncode, 0, error)
                self.assertEqual(output.strip(), FIRST)
            finally:
                if first.poll() is None:
                    first.stdin.close()
                    first.wait(timeout=20)
        self.assertEqual(list((self.root / "bin").iterdir()), [])


if __name__ == "__main__":
    unittest.main()
