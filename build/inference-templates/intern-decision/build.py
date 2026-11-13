#!/usr/bin/env python3
"""Rebuild the Intern-Decision catalog runtime from its tested source.

The three Intern-Decision templates (0.8B, 2B, 4B) start gpuworks_serve.py with
the System One adapter; both files come from test/runtime-wrapper-assets, where
they are unit-tested. This packs them reproducibly (fixed mtime, USTAR, sorted
names) into each model's assets directory and writes the bundle's SHA-256 back
into that model's model.yaml.
"""
import gzip
import hashlib
import io
from pathlib import Path
import re
import tarfile

MODELS = ("internlm/intern-decision-0.8b", "internlm/intern-decision-2b", "internlm/intern-decision-4b")
BUNDLE = "gpuworks-systemone-runtime.tar.gz"
SOURCES = ("gpuworks_serve.py", "systemone_adapter.py")


def bundle(root: Path) -> bytes:
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for name in SOURCES:
                data = (root / "test/runtime-wrapper-assets" / name).read_bytes()
                entry = tarfile.TarInfo(name)
                entry.mode, entry.size = 0o444, len(data)
                archive.addfile(entry, io.BytesIO(data))
    return output.getvalue()


def main():
    root = Path(__file__).resolve().parents[3]
    data = bundle(root)
    digest = hashlib.sha256(data).hexdigest()
    pattern = re.compile(r"(file: assets/" + re.escape(BUNDLE) + r"\n\s+sha256: )[0-9a-f]{64}")
    for model_id in MODELS:
        model = root / "internal/server/inferencecatalog/seed/catalog/models" / model_id
        manifest = model / "model.yaml"
        updated, count = pattern.subn(lambda match: match[1] + digest, manifest.read_text())
        if count < 1:
            raise ValueError(f"{manifest}: expected at least one {BUNDLE} reference")
        (model / "assets").mkdir(exist_ok=True)
        (model / "assets" / BUNDLE).write_bytes(data)
        manifest.write_text(updated)
        print(digest, (model / "assets" / BUNDLE).relative_to(root))


if __name__ == "__main__":
    main()
