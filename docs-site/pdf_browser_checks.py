#!/usr/bin/env python3
"""Explicit PDF checks with a real browser, embedded fonts, and Poppler."""
import functools
import hashlib
import json
from http.server import ThreadingHTTPServer
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest

from playwright.sync_api import sync_playwright

import export_pdf


ROOT = Path(__file__).resolve().parent
VERSION = "v26.11.3"
COMMIT = "abcdef0123456789"
LONG_ANCHOR = "中文参数与默认行为" * 30


def handbook(language, extra=""):
    prefix = "/docs/en/" if language == "en" else "/docs/"
    return f'''<!doctype html><html lang="{language}"><head><meta charset="utf-8">
<meta name="handbook-base-url" content="https://docs.invalid/docs/">
<title>{export_pdf.TITLES[language]}</title>
<style>@page {{ size: A4; }} body {{ font: 12pt "Noto Sans CJK SC", sans-serif; }}
article {{ break-before: page; }} pre {{ white-space: pre-wrap; }}</style></head><body>
<header><h1>{export_pdf.TITLES[language]}</h1><p data-handbook-version></p><p data-handbook-commit></p></header>
<nav><a href="#first">First chapter</a><a href="#last">Last chapter</a></nav>
<article id="first" data-page-path="{prefix}tutorials/start/">
<h1>开始使用 Start</h1><h2 id="settings">配置 Settings</h2>
<p id="fn:1">first footnote</p><a data-test="footnote" href="#fn:1">footnote</a>
<a data-test="local" href="#settings">same page</a>
<a data-test="cross" href="{prefix}reference/end/#settings">cross page</a>
<a data-test="canonical-cross" href="https://docs.invalid{prefix}reference/end/#settings">canonical cross</a>
<a data-test="long-anchor" href="{prefix}reference/end/#{LONG_ANCHOR}">long Chinese anchor</a>
<a data-test="excluded" href="{prefix}dev/testing/#settings">developer reference</a>
<a data-test="external" href="https://external.invalid/a#b">external reference</a>
<img alt="local graphic" src="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='10'%3E%3Crect width='10' height='10' fill='blue'/%3E%3C/svg%3E">
{extra}</article>
<article id="last" data-page-path="{prefix}reference/end/">
<h1>末章完成 Final chapter</h1><h2 id="settings">More settings</h2><p id="fn:1">second footnote</p>
<h2 id="{LONG_ANCHOR}" data-test="long-anchor-target">详细参数 Detailed parameters</h2>
<p>中文字符完整保留。This paragraph must be searchable in the generated PDF.</p></article>
</body></html>'''


class PDFBrowserTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.chrome = os.environ.get("CHROME_PATH") or shutil.which("google-chrome") or shutil.which("chromium")
        if not cls.chrome:
            raise RuntimeError("Chrome/Chromium is required for explicit PDF browser checks")
        cls.play = sync_playwright().start()
        cls.addClassCleanup(cls.play.stop)
        cls.browser = cls.play.chromium.launch(executable_path=cls.chrome, headless=True)
        cls.addClassCleanup(cls.browser.close)
        cls.prepare_script = (ROOT / "prepare_pdf.js").read_text()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.public = self.root / "public"
        for language, path in (("zh", "handbook/index.html"), ("en", "en/handbook/index.html")):
            file = self.public / path
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text(handbook(language), encoding="utf-8")
        (self.public / "docs-manifest.json").write_text(json.dumps({"commit": COMMIT}), encoding="utf-8")
        handler = functools.partial(export_pdf.SiteHandler, directory=str(self.public), prefix="/docs/")
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_server)
        self.context = self.browser.new_context(locale="zh-CN", viewport={"width": 688, "height": 1000})
        self.addCleanup(self.context.close)
        self.page = self.context.new_page()

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def load(self, source=None):
        if source is not None:
            (self.public / "handbook/index.html").write_text(source, encoding="utf-8")
        self.page.goto(f"http://127.0.0.1:{self.server.server_port}/docs/handbook/", wait_until="load")

    def prepare(self):
        return self.page.evaluate(type(self).prepare_script, {"language": "zh", "version": VERSION, "commit": COMMIT})

    def run_export(self, output):
        return subprocess.run([sys.executable, str(ROOT / "export_pdf.py"), "--public", str(self.public),
                               "--output", str(output), "--version", VERSION, "--commit", COMMIT,
                               "--chrome", self.chrome], capture_output=True, text=True)

    def test_duplicate_ids_and_cross_document_links_are_resolved_inside_the_manual(self):
        self.load()
        result = self.prepare()
        self.assertEqual((result["articles"], result["images"]), (2, 1))
        ids = self.page.locator("[id]").evaluate_all("elements => elements.map(element => element.id)")
        self.assertEqual(len(ids), len(set(ids)))
        for name, selector in (("local", "#first h2"), ("cross", "#last h2"), ("canonical-cross", "#last h2"), ("footnote", "#first p")):
            destination = self.page.locator(selector).first.get_attribute("id")
            self.assertEqual(self.page.locator(f'[data-test="{name}"]').get_attribute("href"), "#" + destination)
        self.assertEqual(self.page.locator('[data-test="excluded"]').get_attribute("href"), "https://docs.invalid/docs/dev/testing/#settings")
        self.assertEqual(self.page.locator('[data-test="external"]').get_attribute("href"), "https://external.invalid/a#b")
        self.assertEqual(self.page.locator("[data-handbook-version]").inner_text(), VERSION)

    def test_long_chinese_heading_anchors_remain_linked_with_pdf_safe_destination_names(self):
        self.assertGreater(len(LONG_ANCHOR.encode("utf-8")), 127)
        self.load()
        self.prepare()
        target = self.page.locator('[data-test="long-anchor-target"]')
        destination = target.get_attribute("id")
        self.assertEqual(self.page.locator('[data-test="long-anchor"]').get_attribute("href"), "#" + destination)
        self.assertEqual(target.inner_text(), "详细参数 Detailed parameters")
        ids = self.page.locator("[id]").evaluate_all("elements => elements.map(element => element.id)")
        self.assertTrue(all(len(value.encode("utf-8")) <= 127 for value in ids), ids)

    def test_missing_internal_anchor_fails_instead_of_exporting_a_broken_link(self):
        self.load(handbook("zh").replace("reference/end/#settings", "reference/end/#absent"))
        with self.assertRaisesRegex(Exception, "Missing handbook anchor"):
            self.prepare()

    def test_missing_image_fails_instead_of_exporting_an_incomplete_manual(self):
        self.load(handbook("zh", '<img src="/docs/assets/not-found.png" alt="required screenshot">'))
        with self.assertRaisesRegex(Exception, "(decoded|decode|EncodingError)"):
            self.prepare()

    def test_missing_diagram_renderer_fails_instead_of_leaving_source_code(self):
        self.load(handbook("zh", '<pre class="td-mermaid-source">graph LR; A --&gt; B</pre>'))
        with self.assertRaisesRegex(Exception, "Mermaid renderer is unavailable"):
            self.prepare()

    def test_invalid_mermaid_source_fails_with_the_real_local_renderer(self):
        self.load(handbook("zh", '<pre class="td-mermaid-source">not a valid mermaid diagram</pre>'))
        candidates = sorted((ROOT / "public/third_party/mermaid").glob("mermaid.min*.js"))
        renderer = candidates[0] if candidates else ROOT / "themes/oink/assets/third_party/mermaid/mermaid.min.js"
        self.assertTrue(renderer.is_file(), "The built site's local Mermaid asset is required")
        self.page.add_script_tag(path=str(renderer))
        with self.assertRaisesRegex(Exception, "(diagram type|Parse error|parse error)"):
            self.prepare()

    def test_real_bilingual_pdfs_are_searchable_embed_fonts_and_match_their_checksums(self):
        output = self.root / "dist/docs"
        result = self.run_export(output)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        names = {f"gpuworks-{VERSION}-{language}.pdf" for language in ("zh", "en")}
        self.assertEqual({file.name for file in output.iterdir()}, names | {"SHA256SUMS"})
        sums = dict(line.split("  ", 1)[::-1] for line in (output / "SHA256SUMS").read_text().splitlines())
        self.assertEqual(set(sums), names)
        for language in ("zh", "en"):
            file = output / f"gpuworks-{VERSION}-{language}.pdf"
            self.assertEqual(hashlib.sha256(file.read_bytes()).hexdigest(), sums[file.name])
            self.assertGreaterEqual(export_pdf.validate_pdf(file, language, VERSION, COMMIT, "末章完成 Final chapter"), 3)
            destinations = subprocess.run(["pdfinfo", "-dests", str(file)], capture_output=True, text=True)
            self.assertEqual(destinations.returncode, 0, destinations.stderr)
            self.assertEqual(destinations.stderr, "", "PDF destination names must not produce parser warnings")

    def test_failure_in_second_language_does_not_publish_the_first_pdf(self):
        (self.public / "en/handbook/index.html").write_text(handbook("en", '<img src="/docs/assets/not-found.png" alt="required screenshot">'), encoding="utf-8")
        output = self.root / "dist/docs"
        result = self.run_export(output)
        self.assertNotEqual(result.returncode, 0)
        self.assertRegex(result.stderr, "(decoded|decode|EncodingError)")
        self.assertIn('"language": "zh"', result.stdout)
        self.assertFalse(output.exists())
        self.assertEqual(list(output.parent.glob("docs-pdf-*")), [])

    def test_mismatched_source_commit_is_rejected_before_output_is_created(self):
        (self.public / "docs-manifest.json").write_text(json.dumps({"commit": "123456789abcdef0"}), encoding="utf-8")
        output = self.root / "dist/docs"
        result = self.run_export(output)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match the requested source commit", result.stderr)
        self.assertFalse(output.exists())

    def test_unrelated_output_files_are_preserved_and_rejected(self):
        output = self.root / "dist/docs"
        output.mkdir(parents=True)
        unrelated = output / "previous-version.pdf"
        unrelated.write_bytes(b"preserve this older document")
        result = self.run_export(output)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("output directory contains unrelated files", result.stderr)
        self.assertEqual(unrelated.read_bytes(), b"preserve this older document")


if __name__ == "__main__":
    unittest.main()
