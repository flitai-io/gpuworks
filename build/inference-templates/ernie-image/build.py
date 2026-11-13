#!/usr/bin/env python3
"""Rebuild the small ERNIE-Image catalog runtime from its tested source."""
import gzip
import hashlib
import io
from pathlib import Path
import re
import tarfile


def main():
    root = Path(__file__).resolve().parents[3]
    model = root / "internal/server/inferencecatalog/seed/catalog/models/paddlepaddle/ernie-image"
    target = model / "assets/gpuworks-diffusers-runtime.tar.gz"
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for name in ("diffusers_adapter.py", "gpuworks_serve.py"):
                data = (root / "test/runtime-wrapper-assets" / name).read_bytes()
                entry = tarfile.TarInfo(name)
                entry.mode, entry.size = 0o444, len(data)
                archive.addfile(entry, io.BytesIO(data))
    data = output.getvalue()
    digest = hashlib.sha256(data).hexdigest()
    manifest = model / "model.yaml"
    updated, count = re.subn(
        r"(file: assets/gpuworks-diffusers-runtime\.tar\.gz\n\s+sha256: )[0-9a-f]{64}",
        lambda match: match[1] + digest, manifest.read_text(),
    )
    if count != 1:
        raise ValueError("expected exactly one runtime asset reference")
    target.write_bytes(data)
    manifest.write_text(updated)
    print(digest, target.relative_to(root))


if __name__ == "__main__":
    main()
