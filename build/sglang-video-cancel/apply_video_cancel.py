"""Install the reviewed SGLang 0.5.21 video cancellation backport before serve."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import stat
import sys
import tempfile

HERE = Path(__file__).resolve().parent
MAX_SOURCE_BYTES = 2 << 20


def read_source(path):
    with path.open('rb') as source:
        data = source.read(MAX_SOURCE_BYTES + 1)
    if len(data) > MAX_SOURCE_BYTES:
        raise ValueError(f'{path}: source exceeds supported size')
    return data


def digest(data):
    return hashlib.sha256(data).hexdigest()


def checked_path(root, relative):
    name = PurePosixPath(relative)
    if (name.is_absolute() or '..' in name.parts or '\\' in relative
            or not relative.startswith('multimodal_gen/runtime/')
            or name.suffix != '.py'):
        raise ValueError(f'unsupported patch path: {relative}')
    target = root.joinpath(*name.parts)
    for path in (target, *target.parents):
        if path == root:
            break
        if path.is_symlink():
            raise ValueError(f'{path}: symlinks are not supported')
    return target


def install(root, bundle=HERE):
    root = root.resolve(strict=True)
    manifest = json.loads(read_source(bundle / 'manifest.json'))
    pending = []
    for relative, checksums in manifest.items():
        target = checked_path(root, relative)
        payload = checked_path(bundle / 'files', relative)
        updated = read_source(payload)
        if digest(updated) != checksums['after']:
            raise ValueError(f'{relative}: patch payload checksum mismatch')
        compile(updated, relative, 'exec')
        if target.exists():
            info = target.stat()
            if not stat.S_ISREG(info.st_mode):
                raise ValueError(f'{relative}: expected a regular file')
            current = digest(read_source(target))
            if current == checksums['after']:
                continue
            if current != checksums['before']:
                raise ValueError(f'{relative}: unsupported SGLang source checksum; expected runner r2')
            if not info.st_mode & 0o222:
                raise ValueError(f'{relative}: source is read-only')
            mode = stat.S_IMODE(info.st_mode)
        else:
            if checksums['before'] is not None:
                raise ValueError(f'{relative}: expected original source file is missing')
            mode = 0o644
        if not target.parent.is_dir() or not target.parent.stat().st_mode & 0o222:
            raise ValueError(f'{relative}: source directory is missing or read-only')
        pending.append((target, updated, mode))

    # Validate every source and payload before the first write. Each replacement
    # breaks udocker image hardlinks; an interrupted run can safely resume.
    for target, updated, mode in pending:
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(dir=target.parent, prefix='.gpuworks-video-cancel-', delete=False) as output:
                temporary = Path(output.name)
                output.write(updated)
                output.flush()
                os.fchmod(output.fileno(), mode)
                os.fsync(output.fileno())
            os.replace(temporary, target)
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)
    return len(pending)


def main():
    try:
        spec = importlib.util.find_spec('sglang')
        if spec is None or spec.origin is None:
            raise ValueError('SGLang package was not found')
        changed = install(Path(spec.origin).parent)
        print(f'gpuworks: SGLang r2 video cancellation patch ready ({changed} files updated)', flush=True)
    except (OSError, ValueError, KeyError, TypeError, SyntaxError) as error:
        print(f'gpuworks: SGLang video cancellation patch failed: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
