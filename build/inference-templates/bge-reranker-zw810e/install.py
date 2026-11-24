#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Apply the verified BGE pooling patch in the container's writable layer."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

BUNDLE = Path(__file__).resolve().parent
SITE_PACKAGES = Path("/usr/local/lib/python3.12/site-packages")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def install(bundle=BUNDLE, root=SITE_PACKAGES):
    files = json.loads((bundle / "manifest.json").read_text())["files"]
    patch = (bundle / "grouped-enqueue.patch").read_bytes()
    fragments = {}
    for fragment in re.split(rb"(?m)(?=^--- a/vllm/)", patch):
        if fragment:
            name = fragment.splitlines()[0].removeprefix(b"--- a/").decode()
            if name in fragments:
                raise ValueError(f"duplicate patch target: {name}")
            fragments[name] = fragment
    if fragments.keys() != files.keys():
        raise ValueError("patch targets differ from the manifest")

    # Verify and stage every patch before replacing any live module.
    pending = []
    with tempfile.TemporaryDirectory() as directory:
        staged = Path(directory) / "module.py"
        for name, hashes in files.items():
            relative = Path(name)
            if relative.parts[0] != "vllm" or ".." in relative.parts:
                raise ValueError(f"invalid vLLM module path: {name}")
            target = root / relative
            source = target.read_bytes()
            current = digest(source)
            if current == hashes["patched_sha256"]:
                continue
            if current != hashes["base_sha256"]:
                raise ValueError(f"unexpected HGGC runner module: {name}")
            staged.write_bytes(source)
            subprocess.run(
                ["patch", "--batch", "--fuzz=0", str(staged)],
                input=fragments[name], check=True, stdout=subprocess.DEVNULL,
            )
            patched = staged.read_bytes()
            if digest(patched) != hashes["patched_sha256"]:
                raise ValueError(f"patched module checksum mismatch: {name}")
            compile(patched, str(target), "exec")
            pending.append((target, patched))

    for target, patched in pending:
        with tempfile.NamedTemporaryFile(dir=target.parent, delete=False) as staging:
            staged = Path(staging.name)
            try:
                staging.write(patched)
                staging.close()
                staged.chmod(target.stat().st_mode & 0o777)
                os.replace(staged, target)
            finally:
                staged.unlink(missing_ok=True)
    print(f"Verified {len(files)} BGE pooling modules", flush=True)


if __name__ == "__main__":
    install()
