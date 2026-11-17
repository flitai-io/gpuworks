"""Offline startup contracts; no GPU, network, or model-size fixtures."""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('deepseek_entrypoint', HERE / 'entrypoint.py')
entry = importlib.util.module_from_spec(spec)
spec.loader.exec_module(entry)


class ExecReached(Exception):
    pass


class Startup(unittest.TestCase):
    def fixture(self, root):
        manifest = json.loads((HERE / 'model-manifest.json').read_text())
        env = {}
        for item in manifest['files']:
            folder = root / item['role']
            folder.mkdir()
            (folder / item['filename']).write_bytes(b'GGUFtest')
            item['size'] = 8
            item['sha256'] = hashlib.sha256(b'GGUFtest').hexdigest()
            if item['role'] != 'shard1':
                env[entry.MOUNTS[item['role']]] = str(folder)
        return manifest, env

    def test_default_bundled_metadata_needs_no_external_metadata_mount(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, env = self.fixture(root)
            item = next(item for item in manifest['files'] if item['role'] == 'engram_meta')
            adapter = root / 'adapter'
            bundled = adapter / item['bundled_path']
            bundled.parent.mkdir(parents=True)
            (root / 'engram_meta' / item['filename']).rename(bundled)
            env.pop(entry.MOUNTS['engram_meta'])
            with mock.patch.object(entry, 'HERE', adapter):
                files = entry.locate_files(root / 'shard1', manifest, env)
                self.assertEqual(files['engram_meta'], bundled)
                bundled.write_bytes(b'GGUFbad!')
                with self.assertRaisesRegex(ValueError, 'SHA-256 mismatch'):
                    entry.locate_files(root / 'shard1', manifest, env)
            self.assertFalse((root / 'state').exists())

    def test_explicit_metadata_override_is_also_pinned(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, env = self.fixture(root)
            item = next(item for item in manifest['files'] if item['role'] == 'engram_meta')
            (root / 'engram_meta' / item['filename']).write_bytes(b'GGUFbad!')
            with self.assertRaisesRegex(ValueError, 'SHA-256 mismatch'):
                entry.locate_files(root / 'shard1', manifest, env)

    def test_missing_auxiliary_fails_before_writable_state(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, env = self.fixture(root)
            env.pop(entry.MOUNTS['engram'])
            with self.assertRaisesRegex(ValueError, 'STRATA_DSV41_ENGRAM_DIR'):
                entry.locate_files(root / 'shard1', manifest, env)
            self.assertFalse((root / 'state').exists())

    def test_duplicate_and_truncated_inputs_are_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, env = self.fixture(root)
            first = manifest['files'][0]
            nested = root / 'shard1/nested'
            nested.mkdir()
            duplicate = nested / first['filename']
            duplicate.write_bytes(b'GGUFtest')
            with self.assertRaisesRegex(ValueError, 'found 2'):
                entry.locate_files(root / 'shard1', manifest, env)
            duplicate.unlink()
            (root / 'shard1' / first['filename']).write_bytes(b'GGUF')
            with self.assertRaisesRegex(ValueError, 'wrong size'):
                entry.locate_files(root / 'shard1', manifest, env)

    def test_shared_repository_mounts_ignore_other_files_and_find_nested_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, _ = self.fixture(root)
            ista, unsloth = root / 'ista', root / 'unsloth'
            ista.mkdir()
            (unsloth / 'UD-Q8_K_XL').mkdir(parents=True)
            env = {}
            for item in manifest['files']:
                role = item['role']
                folder = unsloth if role in ('dspark', 'engram_meta') else ista
                target = folder / ('UD-Q8_K_XL' if role == 'engram_meta' else '') / item['filename']
                (root / role / item['filename']).rename(target)
                if role != 'shard1':
                    env[entry.MOUNTS[role]] = str(folder)
            (ista / 'unrelated.gguf').write_bytes(b'ignore this')
            files = entry.locate_files(ista, manifest, env)
            self.assertEqual(len(files), 7)
            self.assertEqual(files['engram_meta'].parent.name, 'UD-Q8_K_XL')

    def test_text_start_prepares_only_small_state_and_preserves_assigned_gpus(self):
        with tempfile.TemporaryDirectory(prefix='deepseek spaces ') as directory:
            root = Path(directory)
            manifest, env = self.fixture(root)
            adapter = root / 'adapter'
            adapter.mkdir()
            (adapter / 'model-manifest.json').write_text(json.dumps(manifest))
            upstream = root / 'upstream'
            (upstream / 'tools/dsv41').mkdir(parents=True)
            # Real subprocesses emulate the pack's output and the engine's file check.
            (upstream / 'tools/dsv41/pack_gguf.py').write_text(
                'import pathlib,sys\n'
                'p=pathlib.Path(sys.argv[sys.argv.index("--out")+1]); p.mkdir()\n'
                '(p/"tokenizer").mkdir(); (p/"tokenizer/vocab.json").write_text("{}")\n'
                '(p/"tensors.txt").write_text("checked")\n')
            (upstream / 'data/dsv41').mkdir(parents=True)
            (upstream / 'data/dsv41/profile-2.00.bin').write_bytes(b'profile')
            engine = upstream / 'engine'
            engine.write_text('#!/bin/sh\ntest -s "$3/tensors.txt"\n')
            engine.chmod(0o755)
            state = root / 'state'
            env.update(CUDA_VISIBLE_DEVICES='3,1', TEST_BACKEND_KEY='test-secret')
            seen = {}

            def execute(program, command):
                config = Path(command[command.index('--config') + 1])
                seen.update(config=json.loads(config.read_text()), mode=config.stat().st_mode & 0o777,
                            command=command, gpu=os.environ['CUDA_VISIBLE_DEVICES'])
                raise ExecReached()

            previous = Path.cwd()
            try:
                with mock.patch.multiple(entry, HERE=adapter, STRATA=upstream, ENGINE=engine), \
                        mock.patch.dict(os.environ, env), mock.patch.object(entry.os, 'execv', execute):
                    with self.assertRaises(ExecReached):
                        entry.main(['--model', str(root / 'shard1'), '--data-dir', str(state),
                                    '--host', '0.0.0.0', '--api-key-env', 'TEST_BACKEND_KEY'])
            finally:
                os.chdir(previous)
            config = seen['config']
            self.assertEqual(config['arch'], 'deepseek41')
            self.assertEqual(config['api_key'], 'test-secret')
            self.assertEqual(config['engine_ready_s'], 3600)
            self.assertEqual(seen['mode'], 0o600)
            self.assertEqual(seen['gpu'], '3,1')
            self.assertFalse('gpu' in config or 'gpus' in config)
            self.assertIn('--offline', seen['command'])
            self.assertEqual(config['args'][config['args'].index('--vision') + 1], 'off')
            self.assertEqual(config['args'][config['args'].index('--dspark') + 1], 'auto')
            self.assertTrue(all((root / item['role'] / item['filename']).read_bytes() == b'GGUFtest'
                                for item in manifest['files']))
            self.assertEqual(list(state.iterdir()), [])

    def test_unknown_and_out_of_range_arguments_do_not_reach_engine(self):
        for extra in (['--vision', 'on'], ['--context', '4096'], ['--parallel', '9'], ['--ssd-cache', 'nan'],
                      ['--mmap-experts', 'invalid'], ['--mmap-experts']):
            with self.subTest(extra=extra), mock.patch('sys.stderr', new_callable=io.StringIO), self.assertRaises(SystemExit):
                entry.arguments(['--model', '/model', *extra])

    def test_native_expert_placement_is_optional_and_preserves_explicit_modes(self):
        for mode in (None, 'auto', 'on', 'off'):
            with self.subTest(mode=mode):
                extra = [] if mode is None else ['--mmap-experts', mode]
                args = entry.arguments(['--model', '/model', *extra])
                command = entry.run_config(args, Path('/pack'), Path('/engine.log'), {})['args']
                if mode is None:
                    self.assertNotIn('--mmap-experts', command)
                else:
                    self.assertEqual(command.count('--mmap-experts'), 1)
                    self.assertEqual(command[command.index('--mmap-experts') + 1], mode)

    def test_deployment_can_override_platform_bind_to_loopback(self):
        args = entry.arguments(['--model', '/model', '--host', '0.0.0.0', '--port', '8080',
                                '--host', '127.0.0.1'])
        command = entry.server_command(args, Path('/state/run.json'))
        self.assertEqual(command[command.index('--host') + 1], '127.0.0.1')

    def test_failed_pack_never_starts_server_and_cleans_only_its_state(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, env = self.fixture(root)
            adapter = root / 'adapter'
            adapter.mkdir()
            (adapter / 'model-manifest.json').write_text(json.dumps(manifest))
            upstream = root / 'upstream'
            (upstream / 'tools/dsv41').mkdir(parents=True)
            (upstream / 'tools/dsv41/pack_gguf.py').write_text('import sys; sys.exit(9)\n')
            state = root / 'state'
            state.mkdir()
            existing = state / 'unrelated.txt'
            existing.write_text('keep')
            with mock.patch.multiple(entry, HERE=adapter, STRATA=upstream), \
                    mock.patch.dict(os.environ, env), mock.patch.object(entry.os, 'execv') as execute:
                with self.assertRaises(subprocess.CalledProcessError):
                    entry.main(['--model', str(root / 'shard1'), '--data-dir', str(state)])
                execute.assert_not_called()
            self.assertEqual(list(state.iterdir()), [existing])
            self.assertEqual(existing.read_text(), 'keep')

    def test_sigterm_during_pack_reaps_child_and_removes_partial_state(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, env = self.fixture(root)
            adapter = root / 'adapter'
            adapter.mkdir()
            (adapter / 'model-manifest.json').write_text(json.dumps(manifest))
            upstream = root / 'upstream'
            (upstream / 'tools/dsv41').mkdir(parents=True)
            child_pid = root / 'pack.pid'
            (upstream / 'tools/dsv41/pack_gguf.py').write_text(
                'import os,pathlib,time\n'
                f'pathlib.Path({str(child_pid)!r}).write_text(str(os.getpid()))\n'
                'time.sleep(5)\n')
            state = root / 'state'
            driver = root / 'driver.py'
            driver.write_text(
                'import importlib.util,signal\n'
                f's=importlib.util.spec_from_file_location("entry", {str(HERE / "entrypoint.py")!r})\n'
                'e=importlib.util.module_from_spec(s);s.loader.exec_module(e)\n'
                f'e.HERE=e.Path({str(adapter)!r});e.STRATA=e.Path({str(upstream)!r})\n'
                'signal.signal(signal.SIGTERM,e.stop_preparation)\n'
                f'e.main(["--model",{str(root / "shard1")!r},"--data-dir",{str(state)!r}])\n')
            process = subprocess.Popen([sys.executable, str(driver)], env={**os.environ, **env})
            try:
                deadline = time.monotonic() + 2
                while not child_pid.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertTrue(child_pid.exists())
                process.send_signal(signal.SIGTERM)
                self.assertEqual(process.wait(timeout=3), 143)
                with self.assertRaises(ProcessLookupError):
                    os.kill(int(child_pid.read_text()), 0)
                self.assertEqual(list(state.iterdir()), [])
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=3)


if __name__ == '__main__':
    unittest.main()
