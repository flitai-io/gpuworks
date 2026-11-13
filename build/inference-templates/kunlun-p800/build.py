#!/usr/bin/env python3
"""Pack the validated Kunlun catalog with the server's canonical package writer."""
import argparse
import subprocess
from pathlib import Path

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[3]
    subprocess.run(["go", "run", "./cmd/gpuworks-pack-catalog", "--catalog",
                    str(Path(__file__).resolve().parent / "catalog"), "--output",
                    str(args.output.resolve())], cwd=repo, check=True)
