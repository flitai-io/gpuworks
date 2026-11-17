import io
import hashlib
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest.mock import patch
from urllib.parse import urljoin

import stage_docs
import check_translations
import translation_markers


class TranslationTest(unittest.TestCase):
    def test_source_changes_and_modified_commands_require_translation_review(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            for name in ("docs", "docs-en", "docs-site"):
                (root / name).mkdir()
            source = root / "docs/guide.md"
            translated = root / "docs-en/guide.md"
            source.write_text("# 指南\n\n```bash\ncurl --fail /healthz\n```\n")
            translated.write_text(translation_markers.render(
                "# Guide\n\n```bash\ncurl --fail /healthz\n```\n",
                hashlib.sha256(source.read_bytes()).hexdigest(), {}))
            self.assertEqual(check_translations.inspect(root)["errors"], [])
            source.write_text(source.read_text() + "\n新增的前提条件。\n")
            self.assertIn("source changed", check_translations.inspect(root)["errors"][0])
            translated.write_text(translated.read_text().replace("curl --fail", "curl"))
            self.assertTrue(any("code blocks differ" in error for error in check_translations.inspect(root)["errors"]))

    def test_page_without_marker_is_reported_and_write_records_the_source(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            for name in ("docs/how-to", "docs-en/how-to"):
                (root / name).mkdir(parents=True)
            (root / "docs/how-to/install.md").write_text("# 安装\n\n```bash\n# 安装 server\nmake build\n```\n")
            (root / "docs-en/how-to/install.md").write_text("# Install\n\n```bash\n# Install the server\nmake build\n```\n")
            (root / "docs/extra.md").write_text("# 额外\n")
            result = check_translations.inspect(root)
            self.assertEqual(result["missing"], ["extra.md"])
            self.assertEqual(result["errors"], [
                "how-to/install.md: translation source changed or review hash missing",
                "how-to/install.md: code blocks differ",
            ])
            with redirect_stderr(io.StringIO()):
                check_translations.write(root, [check_translations.page_name("docs/how-to/install.md", root)])
            self.assertEqual(check_translations.inspect(root)["errors"], ["how-to/install.md: code blocks differ"])
            with redirect_stderr(io.StringIO()):
                check_translations.write(root, ["how-to/install.md"], approve_code_blocks=True)
            self.assertEqual(check_translations.inspect(root)["errors"], [])
            page = (root / "docs-en/how-to/install.md").read_text()
            self.assertTrue(page.startswith(
                "# Install\n\n```bash\n# Install the server\nmake build\n```\n\n<!-- translation-source-sha256: "))
            self.assertEqual(page.count("translation-localized-code-block"), 1)
            # Rewriting keeps one marker block and leaves the body unchanged.
            with redirect_stderr(io.StringIO()):
                check_translations.write(root, [check_translations.page_name("docs-en/how-to/install.md", root)])
            self.assertEqual((root / "docs-en/how-to/install.md").read_text(), page)
            with self.assertRaises(SystemExit):
                check_translations.write(root, ["extra.md"])

    def test_markers_are_validated_and_only_the_trailing_block_counts(self):
        digest = "a" * 64
        body = "# Guide\n\n<!-- translation-source-sha256: " + "b" * 64 + " -->\n\nProse.\n"
        text = body + "\n<!-- translation-source-sha256: " + digest + " -->\n\n"
        self.assertEqual(translation_markers.strip(text), body)
        self.assertEqual(translation_markers.parse(text).source_sha256, digest)
        self.assertEqual(translation_markers.parse(body).problems, [])
        broken = translation_markers.parse(
            "# Guide\n<!-- translation-source-sha256: xyz -->\n"
            "<!-- translation-localized-code-block: " + digest + " -->\n"
            "<!-- translation-reviewer: someone -->\n")
        self.assertEqual(broken.problems, [
            "malformed translation-source-sha256 marker",
            "malformed translation-localized-code-block marker",
            "unknown translation-reviewer marker",
        ])

    def test_staged_english_pages_drop_the_review_markers(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            source, english, output = root / "docs", root / "docs-en", root / "content"
            for directory, title in ((source, "安装"), (english, "Install")):
                (directory / "how-to").mkdir(parents=True)
                (directory / "README.md").write_text(
                    "# Docs\n### Guides (how-to)\n**Setup**\n- [Install](how-to/install.md)\n")
                (directory / "how-to/install.md").write_text(f"# {title}\n\nGPUWorks.\n")
            page = english / "how-to/install.md"
            page.write_text(translation_markers.render(
                page.read_text(), "c" * 64, {("d" * 64, "e" * 64): "Reviewed."}))
            with patch.multiple(stage_docs, SOURCE=source, DEST=output, STATIC_ASSETS=root / "assets"), \
                    redirect_stdout(io.StringIO()):
                stage_docs.main()
            staged = (output / "how-to/install.en.md").read_text()
            self.assertIn("GPUWorks.", staged)
            self.assertNotIn("translation-", staged)

    def test_download_stays_in_shared_assets_from_english_page(self):
        body = stage_docs.rewrite_asset_links(
            "[Download](../assets/examples/prompts.jsonl)", Path("tutorials/start.md"), "en"
        )
        href = body.split("](")[1][:-1]
        self.assertEqual(urljoin("https://example.com/docs/en/tutorials/start/", href),
                         "https://example.com/docs/assets/examples/prompts.jsonl")

    def test_display_block_localization_only_accepts_the_reviewed_content(self):
        source = '```mermaid\nflowchart LR\nA["用户"] --> B["server"]\n```'
        translated = source.replace("用户", "User")
        self.assertFalse(check_translations.code_blocks_match(source, translated))
        approved = {(hashlib.sha256(source.encode()).hexdigest(),
                     hashlib.sha256(translated.encode()).hexdigest()): ""}
        self.assertTrue(check_translations.code_blocks_match(source, translated, approved))
        self.assertFalse(check_translations.code_blocks_match(
            source, translated.replace("-->", "---"), approved))
        self.assertFalse(check_translations.code_blocks_match(
            source, translated + "\n```bash\nrm example\n```", approved))

    def test_translated_headings_retain_existing_cross_reference_targets(self):
        source = "# 原文\n\n## 1. 配置 `model`\n\n```bash\n# 注释\n```\n"
        translated = "# Original\n\n## 1. Configure `model`\n\n```bash\n# 注释\n```\n"
        result = stage_docs.preserve_heading_ids(source, translated)
        self.assertIn("## 1. Configure `model` {#1-配置-model}", result)
        self.assertIn("```bash\n# 注释\n```", result)

    def test_omitted_sections_cannot_silently_break_anchor_alignment(self):
        with self.assertRaisesRegex(ValueError, "heading structure"):
            stage_docs.preserve_heading_ids("# 文档\n## 前置条件\n## 安装\n", "# Guide\n## Install\n")

    def test_staging_pairs_languages_without_modifying_chinese_or_inventing_translations(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            source, english, output = root / "docs", root / "docs-en", root / "content"
            for directory in (source, english):
                (directory / "how-to").mkdir(parents=True)
                (directory / "README.md").write_text(
                    "# Docs\n### Guides (how-to)\n**Setup**\n- [Install](how-to/install.md)\n- [Offline](how-to/offline.md)\n"
                )
            (source / "how-to/install.md").write_text("# 安装\n## 准备\n正文。\n")
            (english / "README.md").write_text(
                "# Documentation\n### Tutorials\n- [Start](tutorials/start.md)\n"
                "### How-To Guides\n**Setup**\n- [Install](how-to/install.md)\n- [Offline](how-to/offline.md)\n"
            )
            self.assertEqual(stage_docs.tutorial_weights(english), {"tutorials/start.md": 1})
            (source / "how-to/offline.md").write_text("# 离线\n中文。\n")
            (english / "how-to/install.md").write_text("# Install\n## Prepare\nInstructions.\n")
            for directory, title in ((source, "开始"), (english, "Start")):
                (directory / "tutorials").mkdir()
                (directory / "tutorials/start.md").write_text(f"# {title}\n\nGPUWorks.\n")
            before = {p: p.read_bytes() for p in source.rglob("*.md")}
            with patch.multiple(stage_docs, SOURCE=source, DEST=output, STATIC_ASSETS=root / "assets"), redirect_stdout(io.StringIO()):
                stage_docs.main()
            self.assertTrue((output / "how-to/install.md").exists())
            translated = (output / "how-to/install.en.md").read_text()
            self.assertIn('source_path: "how-to/install.md"', translated)
            self.assertIn("## Prepare {#准备}", translated)
            self.assertFalse((output / "how-to/offline.en.md").exists())
            english_index = (output / "how-to/_index.en.md").read_text()
            self.assertIn("[Install](install/)", english_index)
            self.assertIn('"to": "docs-en/README.md"', english_index)
            self.assertNotIn("[Offline]", english_index)
            for language in ("", ".en"):
                section = (output / f"tutorials/_index{language}.md").read_text()
                self.assertEqual(section.split("---\n", 2)[2], "")
            self.assertEqual(before, {p: p.read_bytes() for p in source.rglob("*.md")})
