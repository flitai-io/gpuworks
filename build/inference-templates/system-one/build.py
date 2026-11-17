#!/usr/bin/env python3
"""Rebuild the System One catalog runtimes from their tested source.

The System One templates start gpuworks_serve.py with an adapter per model
family: Intern-Decision (0.8B, 2B, 4B) with systemone_adapter.py, Laya (English,
multilingual, typed-decisions) with laya_adapter.py. Every file comes from
test/runtime-wrapper-assets, where it is unit-tested. This packs each family's
files reproducibly (fixed mtime, USTAR, sorted names) into its models' assets
directories and writes the bundle's SHA-256 back into each model.yaml.
"""
import gzip
import hashlib
import io
from pathlib import Path
import re
import tarfile

BUNDLES = {
    "gpuworks-systemone-runtime.tar.gz": {
        "sources": ("gpuworks_serve.py", "systemone_adapter.py"),
        "models": ("internlm/intern-decision-0.8b", "internlm/intern-decision-2b", "internlm/intern-decision-4b"),
    },
    "gpuworks-laya-runtime.tar.gz": {
        "sources": ("gpuworks_serve.py", "laya_adapter.py"),
        "models": ("convaiinnovations/laya", "convaiinnovations/laya-multilingual",
                   "convaiinnovations/laya-typed-decisions"),
    },
}


def bundle(root: Path, sources) -> bytes:
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for name in sorted(sources):
                data = (root / "test/runtime-wrapper-assets" / name).read_bytes()
                entry = tarfile.TarInfo(name)
                entry.mode, entry.size = 0o444, len(data)
                archive.addfile(entry, io.BytesIO(data))
    return output.getvalue()


def main():
    root = Path(__file__).resolve().parents[3]
    for name, family in BUNDLES.items():
        write(root, name, family["sources"], family["models"])


def write(root: Path, bundle_name: str, sources, models):
    data = bundle(root, sources)
    digest = hashlib.sha256(data).hexdigest()
    pattern = re.compile(r"(file: assets/" + re.escape(bundle_name) + r"\n\s+sha256: )[0-9a-f]{64}")
    for model_id in models:
        model = root / "internal/server/inferencecatalog/seed/catalog/models" / model_id
        manifest = model / "model.yaml"
        updated, count = pattern.subn(lambda match: match[1] + digest, manifest.read_text())
        if count < 1:
            raise ValueError(f"{manifest}: expected at least one {bundle_name} reference")
        (model / "assets").mkdir(exist_ok=True)
        (model / "assets" / bundle_name).write_bytes(data)
        manifest.write_text(updated)
        print(digest, (model / "assets" / bundle_name).relative_to(root))


if __name__ == "__main__":
    main()
