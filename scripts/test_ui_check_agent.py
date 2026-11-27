"""Exercise the host-agent decision in ui_check_ci.sh before the server build."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class EmbeddedAgentTest(unittest.TestCase):
    def test_existing_multiplatform_files_survive_the_ui_check(self):
        for layout in ("pack", "legacy-zstd", "legacy-plain", "missing-host"):
            with self.subTest(layout=layout), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                scripts = root / "scripts"
                scripts.mkdir()
                check = scripts / "ui_check_ci.sh"
                shutil.copy(Path(__file__).with_name(check.name), check)
                dist = root / "internal/server/webui/dist"
                dist.mkdir(parents=True)
                (dist / "index.html").touch()
                dl = root / "internal/server/agentbin/dl"
                dl.mkdir(parents=True)
                resources = dl / "pack" if layout == "pack" else dl
                resources.mkdir(exist_ok=True)
                suffix = "" if layout == "legacy-plain" else ".zst"
                other = resources / ("gpuworks-agent-darwin-arm64" + suffix)
                other.write_bytes(b"existing platform")
                if layout != "missing-host":
                    (resources / ("gpuworks-agent-linux-amd64" + suffix)).touch()

                # Record build requests and stop at the subsequent Go server
                # build. No browser, compilation or external service is needed.
                for name, body in {
                    "make": 'echo "$*" >> make.log',
                    "go": 'case "$*" in "env GOOS") echo linux;; "env GOARCH") echo amd64;; build*) exit 73;; *) exit 1;; esac',
                    "curl": 'exit 1',
                }.items():
                    cmd = root / name
                    cmd.write_text("#!/bin/sh\n" + body + "\n")
                    cmd.chmod(0o755)
                env = dict(os.environ, PATH=str(root) + ":" + os.environ["PATH"],
                           CI="true", UI_CHECK_BASE_REF="")
                run = subprocess.run(["bash", str(check)], env=env, capture_output=True, text=True)
                self.assertEqual(run.returncode, 73, run.stderr)
                calls = (root / "make.log").read_text().splitlines()
                self.assertEqual("cross-agents PLATFORMS=linux/amd64" in calls, layout == "missing-host")
                self.assertEqual(other.read_bytes(), b"existing platform")


if __name__ == "__main__":
    unittest.main()
