import io
import json
import os
from pathlib import Path
import shutil
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import apply_video_cancel as installer
import build


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'sglang'
        self.asset = Path(self.temp.name) / 'asset'
        for name, data in build.originals().items():
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        self.asset.mkdir()
        with tarfile.open(fileobj=io.BytesIO(build.bundle())) as archive:
            archive.extractall(self.asset, filter='data')
        self.manifest = json.loads((self.asset / 'manifest.json').read_bytes())
        self.existing = [name for name, item in self.manifest.items() if item['before']]
        self.added = [name for name, item in self.manifest.items() if item['before'] is None]

    def install(self):
        return installer.install(self.root, self.asset)

    def snapshot(self):
        return {p.relative_to(self.root).as_posix(): p.read_bytes() for p in self.root.rglob('*.py')}

    def test_install_changes_vulnerable_sources_and_adds_cancel_protocol(self):
        original = self.snapshot()
        self.assertNotIn('multimodal_gen/runtime/entrypoints/openai/video_cancel.py', original)
        self.assertEqual(self.install(), 15)
        self.assertEqual(self.snapshot(), build.patched_sources())

    def test_restart_is_noop_even_on_readonly_sources(self):
        self.install()
        for target in self.root.rglob('*.py'):
            target.chmod(0o444)
            self.addCleanup(target.chmod, 0o644)
        before = {p: p.stat().st_ino for p in self.root.rglob('*.py')}
        with patch.object(installer.os, 'replace', side_effect=AssertionError('restart rewrote a patched file')):
            self.assertEqual(self.install(), 0)
        self.assertEqual(before, {p: p.stat().st_ino for p in self.root.rglob('*.py')})

    def test_unknown_source_rejects_entire_install_before_any_write(self):
        (self.root / self.existing[-1]).write_text('# unsupported source\n')
        before = self.snapshot()
        with self.assertRaisesRegex(ValueError, 'unsupported SGLang source checksum'):
            self.install()
        self.assertEqual(self.snapshot(), before)

    def test_missing_original_rejected_before_any_write(self):
        (self.root / self.existing[-1]).unlink()
        before = self.snapshot()
        with self.assertRaisesRegex(ValueError, 'original source file is missing'):
            self.install()
        self.assertEqual(self.snapshot(), before)

    def test_existing_unrecognized_new_module_is_not_overwritten(self):
        (self.root / self.added[-1]).write_text('# user file\n')
        before = self.snapshot()
        with self.assertRaisesRegex(ValueError, 'unsupported SGLang source checksum'):
            self.install()
        self.assertEqual(self.snapshot(), before)

    def test_corrupt_payload_rejected_before_any_write(self):
        (self.asset / 'files' / self.existing[-1]).write_text('# corrupted asset\n')
        before = self.snapshot()
        with self.assertRaisesRegex(ValueError, 'payload checksum mismatch'):
            self.install()
        self.assertEqual(self.snapshot(), before)

    def test_readonly_source_rejected(self):
        target = self.root / self.existing[-1]
        target.chmod(0o444)
        self.addCleanup(target.chmod, 0o644)
        before = self.snapshot()
        with self.assertRaisesRegex(ValueError, 'source is read-only'):
            self.install()
        self.assertEqual(self.snapshot(), before)

    def test_symlink_source_rejected(self):
        target = self.root / self.existing[-1]
        outside = Path(self.temp.name) / 'outside.py'
        target.rename(outside)
        target.symlink_to(outside)
        before = outside.read_bytes()
        with self.assertRaisesRegex(ValueError, 'symlinks are not supported'):
            self.install()
        self.assertEqual(outside.read_bytes(), before)

    def test_symlink_parent_rejected(self):
        target = self.root / 'multimodal_gen'
        outside = Path(self.temp.name) / 'outside'
        target.rename(outside)
        target.symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, 'symlinks are not supported'):
            self.install()

    def test_udocker_shared_image_and_second_container_are_isolated(self):
        image = Path(self.temp.name) / 'image'
        other = Path(self.temp.name) / 'other'
        shutil.copytree(self.root, image, copy_function=os.link)
        shutil.copytree(self.root, other, copy_function=os.link)
        original = self.snapshot()
        self.install()
        for name, data in original.items():
            self.assertEqual((image / name).read_bytes(), data)
            self.assertEqual((other / name).read_bytes(), data)
            self.assertNotEqual((image / name).stat().st_ino, (self.root / name).stat().st_ino)

    def test_interrupted_install_resumes_known_mixed_state(self):
        real_replace = os.replace
        attempts = 0

        def interrupt(source, destination):
            nonlocal attempts
            attempts += 1
            if attempts == 3:
                raise OSError('interrupted startup')
            real_replace(source, destination)

        with patch.object(installer.os, 'replace', side_effect=interrupt):
            with self.assertRaisesRegex(OSError, 'interrupted startup'):
                self.install()
        self.assertFalse(list(self.root.rglob('.gpuworks-video-cancel-*')))
        self.assertEqual(self.install(), 13)
        self.assertEqual(self.snapshot(), build.patched_sources())

    def test_traversal_and_absolute_manifest_paths_rejected(self):
        for name in ('../../escape.py', '/tmp/escape.py', 'multimodal_gen/runtime/../escape.py'):
            with self.subTest(name=name):
                with self.assertRaisesRegex(ValueError, 'unsupported patch path'):
                    installer.checked_path(self.root, name)


if __name__ == '__main__':
    unittest.main(verbosity=2)
