#!/opt/strata/.venv/bin/python
"""Prepare local ISTA-DAS 2.00 files and replace this process with Strata's API."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
STRATA = Path('/opt/strata')
ENGINE = STRATA / 'engine-dsv41/strata-dsv41'
MOUNTS = {
    'shard2': 'STRATA_DSV41_SHARD2_DIR',
    'shard3': 'STRATA_DSV41_SHARD3_DIR',
    'shard4': 'STRATA_DSV41_SHARD4_DIR',
    'engram': 'STRATA_DSV41_ENGRAM_DIR',
    'dspark': 'STRATA_DSV41_DSPARK_DIR',
    'engram_meta': 'STRATA_DSV41_ENGRAM_META_DIR',
}


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--model', required=True, type=Path, help='primary shard directory')
    parser.add_argument('--host', default='127.0.0.1')
    parser.add_argument('--port', type=int, default=8080)
    parser.add_argument('--context', type=int, default=8192)
    parser.add_argument('--parallel', type=int, default=1)
    parser.add_argument('--mmap-experts', choices=('auto', 'on', 'off'),
                        help='native CPU expert placement; omitted by default')
    parser.add_argument('--cpu-threads', type=int)
    parser.add_argument('--ssd-cache', type=float, help='RAM expert-store cap in GB (engine option)')
    parser.add_argument('--max-queue', type=int)
    parser.add_argument('--queue-timeout', type=float)
    parser.add_argument('--api-key-env', help='optional environment variable holding the backend API key')
    parser.add_argument('--data-dir', type=Path, default=Path('/strata-data'))
    args = parser.parse_args(argv)
    if not 1 <= args.port <= 65535:
        parser.error('--port must be 1..65535')
    if not 8192 <= args.context <= 1048576:
        parser.error('--context must be 8192..1048576')
    if not 1 <= args.parallel <= 8:
        parser.error('--parallel must be 1..8')
    if args.cpu_threads is not None and args.cpu_threads < 1:
        parser.error('--cpu-threads must be positive')
    if args.ssd_cache is not None and not 0 <= args.ssd_cache < float('inf'):
        parser.error('--ssd-cache must be finite and nonnegative')
    if args.max_queue is not None and args.max_queue < 0:
        parser.error('--max-queue must be nonnegative')
    if args.queue_timeout is not None and not 0.1 <= args.queue_timeout <= 600:
        parser.error('--queue-timeout must be 0.1..600')
    return args


def locate_files(primary: Path, manifest: dict, env) -> dict[str, Path]:
    """Validate inputs before writable state; hash the small bundled metadata only."""
    files = {}
    for item in manifest['files']:
        role = item['role']
        value = str(primary) if role == 'shard1' else env.get(MOUNTS[role], '')
        if role == 'engram_meta' and not value:
            path = HERE / item['bundled_path']
            if not path.is_file():
                raise ValueError(f'{role}: bundled metadata is missing: {path}')
        else:
            if not value:
                raise ValueError(f'missing component mount: {MOUNTS[role]}')
            root = Path(value)
            if not root.is_dir():
                raise ValueError(f'{role}: component directory does not exist: {root}')
            candidates = sorted(p for p in root.rglob(item['filename']) if p.is_file())
            if len(candidates) != 1:
                raise ValueError(f'{role}: expected exactly one {item["filename"]} under {root}, found {len(candidates)}')
            path = candidates[0].resolve()
        if path.stat().st_size != item['size']:
            raise ValueError(f'{role}: wrong size for {path.name}; expected {item["size"]} bytes')
        with path.open('rb') as stream:
            if stream.read(4) != b'GGUF':
                raise ValueError(f'{role}: invalid GGUF header: {path.name}')
        if role == 'engram_meta':
            with path.open('rb') as stream:
                digest = hashlib.file_digest(stream, 'sha256').hexdigest()
            if digest != item['sha256']:
                raise ValueError(f'{role}: SHA-256 mismatch for {path.name}')
        files[role] = path
    return files


def pack_command(files: dict[str, Path], model: Path, pack: Path, python=sys.executable):
    shards = [files[f'shard{i}'].name for i in range(1, 5)]
    return [python, str(STRATA / 'tools/dsv41/pack_gguf.py'),
            '--model', str(model), '--shards', *shards,
            '--engram', str(files['engram']), '--engram-meta', str(files['engram_meta']),
            '--dspark-gguf', str(files['dspark']), '--dspark-noise-token', '128799',
            '--dspark-markov-rank', '256', '--out', str(pack)]


def run_config(args, pack: Path, log: Path, env) -> dict:
    engine_args = ['--pack', str(pack), '--max-context', str(args.context), '--eos', '1',
                   '--vision', 'off', '--dspark', 'auto', '--dspark-conf', '0.65']
    for flag, value in [('--mmap-experts', args.mmap_experts),
                        ('--cpu-threads', args.cpu_threads), ('--ssd-cache', args.ssd_cache)]:
        if value is not None:
            engine_args += [flag, str(value)]
    config = {'exe': str(ENGINE), 'args': engine_args, 'cwd': str(STRATA),
              'tokenizer': str(pack / 'tokenizer'), 'arch': 'deepseek41',
              'model_name': 'deepseek-v4.1-flash-ista-das-2.00bpw',
              'log': str(log), 'lib_dirs': [str(ENGINE.parent)],
              'engine_ready_s': 3600, 'parallel': args.parallel}
    # CUDA_VISIBLE_DEVICES is supplied by the agent. Leaving gpu/gpus absent preserves
    # the runtime's visible-device numbering in Docker and udocker alike.
    if args.api_key_env:
        key = env.get(args.api_key_env, '')
        if not key.strip():
            raise ValueError(f'API key environment variable is empty: {args.api_key_env}')
        config['api_key'] = key
    return config


def server_command(args, config: Path, python=sys.executable):
    command = [python, '-m', 'serve.server', '--engine', 'strata', '--offline',
               '--config', str(config), '--host', args.host, '--port', str(args.port)]
    for flag, value in [('--max-queue', args.max_queue), ('--queue-timeout', args.queue_timeout)]:
        if value is not None:
            command += [flag, str(value)]
    return command


def main(argv=None):
    args = arguments(argv)
    manifest = json.loads((HERE / 'model-manifest.json').read_text())
    files = locate_files(args.model, manifest, os.environ)
    # Reject credentials before running preparation, without printing their value.
    run_config(args, Path('/unused'), Path('/unused'), os.environ)
    args.data_dir.mkdir(parents=True, exist_ok=True)
    # A fresh pack prevents stale absolute paths or tensor indexes surviving a remount.
    # Only small state lives here; source weights remain in their read-only mounts.
    work = Path(tempfile.mkdtemp(prefix='deepseek-', dir=args.data_dir))
    model, pack = work / 'model', work / 'pack'
    try:
        model.mkdir()
        for i in range(1, 5):
            source = files[f'shard{i}']
            (model / source.name).symlink_to(source)
        subprocess.run(pack_command(files, model, pack), check=True, timeout=600)
        shutil.copyfile(STRATA / 'data/dsv41/profile-2.00.bin', pack / 'expert-profile.bin')
        subprocess.run([str(ENGINE), '--pack-info', '--pack', str(pack)], check=True, timeout=60)
        config = work / 'run.json'
        descriptor = os.open(config, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'w') as output:
            json.dump(run_config(args, pack, work / 'engine.log', os.environ), output)
        os.chdir(STRATA)
        command = server_command(args, config)
        os.execv(command[0], command)
    except BaseException:
        shutil.rmtree(work)
        raise


def stop_preparation(signum, _frame):
    # A container's PID 1 must install its own SIGTERM handler. subprocess.run
    # kills and waits for the current pack/check child when this unwinds it.
    raise SystemExit(128 + signum)


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, stop_preparation)
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(f'strata-deepseek: {error}', file=sys.stderr)
        sys.exit(2)
