#!/usr/bin/env python3
"""Reproduce the small, offline MinerU logits processor catalog asset."""
import gzip
import hashlib
import io
from pathlib import Path
import tarfile

HERE = Path(__file__).resolve().parent
TARGET = HERE.parents[2] / "internal/server/inferencecatalog/seed/catalog/models/opendatalab/mineru2.5-2509-1.2b/assets/mineru-logits.tar.gz"


def archive():
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode="wb", filename="", mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w") as tar:
            for name in ("mineru_logits_processor.py", "LICENSE.txt", "UPSTREAM.md"):
                data = (HERE / name).read_bytes()
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                entry.mode = 0o644
                tar.addfile(entry, io.BytesIO(data))
    return output.getvalue()


if __name__ == "__main__":
    data = archive()
    TARGET.parent.mkdir(parents=True, exist_ok=True)
    TARGET.write_bytes(data)
    print(hashlib.sha256(data).hexdigest(), len(data), TARGET)
