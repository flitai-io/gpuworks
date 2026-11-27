#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Build the optional ZW810E BGE reranker catalog package."""
import argparse
import gzip
import io
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
FILES = ("LICENSE.txt", "NOTICE.txt", "grouped-enqueue.patch", "install.py", "manifest.json")


def build_asset(output):
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("wb") as file:
        with gzip.GzipFile(filename="", mode="wb", fileobj=file, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                for name in FILES:
                    data = (HERE / name).read_bytes()
                    entry = tarfile.TarInfo(name)
                    entry.mode, entry.size = 0o644, len(data)
                    archive.addfile(entry, io.BytesIO(data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory() as directory:
        catalog = Path(directory) / "catalog"
        shutil.copytree(HERE / "catalog", catalog)
        build_asset(catalog / "models/baai/bge-reranker-v2-m3/assets/zw810e-pooling.tar.gz")
        subprocess.run(
            ["go", "run", "./cmd/gpuworks-pack-catalog", "--catalog", str(catalog),
             "--output", str(args.output.resolve())],
            cwd=REPO, check=True,
        )


if __name__ == "__main__":
    main()
