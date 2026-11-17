"""Build the template-local patch asset from the pinned r2 fixture and diff."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile

HERE = Path(__file__).resolve().parent
OUTPUT = HERE.parents[1] / 'internal/server/inferencecatalog/seed/catalog/models/wan-ai/wan2.1-t2v-1.3b-diffusers/assets/sglang-video-cancel.tar.gz'


def originals():
    with tarfile.open(HERE / 'tests/r2-sources.tar.gz') as archive:
        return {item.name: archive.extractfile(item).read() for item in archive if item.isfile()}


def patched_sources():
    sources = originals()
    changes = (HERE / 'video-cancel.patch').read_text(encoding='utf-8').splitlines(keepends=True)
    sections = [i for i, line in enumerate(changes) if line.startswith('diff --git ')] + [len(changes)]
    result = {}
    for start, end in zip(sections, sections[1:]):
        section = changes[start:end]
        header = re.fullmatch(r'diff --git a/python/sglang/(.+) b/python/sglang/\1\n', section[0])
        if header is None:
            raise ValueError('unsupported patch file header')
        name = header[1]
        source = sources.get(name, b'').decode().splitlines(keepends=True)
        out, cursor = [], 0
        index = next(i for i, line in enumerate(section) if line.startswith('@@ '))
        while index < len(section):
            match = re.fullmatch(r'@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@[^\n]*\n', section[index])
            if match is None:
                raise ValueError(f'{name}: invalid hunk header')
            old_start, old_count, new_start, new_count = match.groups()
            position = max(0, int(old_start) - 1)
            if not cursor <= position <= len(source):
                raise ValueError(f'{name}: overlapping hunk')
            out.extend(source[cursor:position])
            if int(new_start) != len(out) + 1:
                raise ValueError(f'{name}: inconsistent output position')
            cursor, removed, added = position, 0, 0
            index += 1
            while index < len(section) and not section[index].startswith('@@ '):
                line = section[index]
                if line[:1] in (' ', '-'):
                    if cursor >= len(source) or source[cursor] != line[1:]:
                        raise ValueError(f'{name}: patch context mismatch')
                    cursor += 1
                    removed += 1
                if line[:1] in (' ', '+'):
                    out.append(line[1:])
                    added += 1
                if line[:1] not in (' ', '+', '-'):
                    raise ValueError(f'{name}: unsupported patch line')
                index += 1
            if (removed, added) != (int(old_count or 1), int(new_count or 1)):
                raise ValueError(f'{name}: hunk counts mismatch')
        out.extend(source[cursor:])
        result[name] = ''.join(out).encode()
        compile(result[name], name, 'exec')
    return result


def bundle():
    before, after = originals(), patched_sources()
    sha = lambda data: hashlib.sha256(data).hexdigest()
    manifest = {name: {'before': sha(before[name]) if name in before else None, 'after': sha(data)} for name, data in sorted(after.items())}
    files = {name: (HERE / name).read_bytes() for name in ('apply_video_cancel.py', 'LICENSE.txt')}
    files['manifest.json'] = (json.dumps(manifest, indent=2) + '\n').encode()
    files.update({'files/' + name: data for name, data in after.items()})
    result = io.BytesIO()
    with gzip.GzipFile(fileobj=result, mode='wb', filename='', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as archive:
            for name, data in sorted(files.items()):
                item = tarfile.TarInfo(name)
                item.size, item.mode, item.mtime = len(data), 0o644, 0
                archive.addfile(item, io.BytesIO(data))
    return result.getvalue()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    data = bundle()
    if args.check:
        if not OUTPUT.is_file() or OUTPUT.read_bytes() != data:
            parser.exit(1, 'SGLang video cancellation asset is stale; run build/sglang-video-cancel/build.py\n')
    else:
        OUTPUT.parent.mkdir(parents=True, exist_ok=True)
        OUTPUT.write_bytes(data)
    print(hashlib.sha256(data).hexdigest())


if __name__ == '__main__':
    main()
