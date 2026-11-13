import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from html.parser import HTMLParser

import export_pdf


ROOT = Path(__file__).resolve().parent


class IdentityTest(unittest.TestCase):
    def test_release_and_preview_identities_are_accepted(self):
        for version in ("v26.11.3", "preview-abcdef0"):
            export_pdf.validate_identity(version, "abcdef0123456789")

    def test_unsafe_names_and_unknown_sources_are_rejected(self):
        for version in ("", "../v26.11.3", "v1/a", "/tmp/report", "v1\nforged", "a" * 81):
            with self.subTest(version=version), self.assertRaises(ValueError):
                export_pdf.validate_identity(version, "abcdef0")
        for commit in ("", "unknown", "ABCDEF0", "abc", "abc1234\n"):
            with self.subTest(commit=commit), self.assertRaises(ValueError):
                export_pdf.validate_identity("v26.11.3", commit)

    def test_metadata_reads_the_canonical_url_and_decodes_entities(self):
        source = '<meta name="unrelated" content="ignored"><meta content="https://docs.invalid/docs/?a=1&amp;b=2" name="handbook-base-url">'
        self.assertEqual(export_pdf.Metadata(source).base_url, "https://docs.invalid/docs/?a=1&b=2")

    def test_incomplete_pdf_is_rejected_before_external_validation(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "broken.pdf"
            for data in (b"", b"%PDF-1.7\ntruncated", b"not-pdf\n%%EOF"):
                path.write_bytes(data)
                with self.subTest(data=data), self.assertRaisesRegex(ValueError, "incomplete PDF"):
                    export_pdf.validate_pdf(path, "zh", "v26.11.3", "abcdef0", "end")


class HandbookHTML(HTMLParser):
    def __init__(self, text):
        super().__init__()
        self.articles = []
        self.placeholders = set()
        self.links = []
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "article":
            self.articles.append(attrs)
        if tag == "a":
            self.links.append(attrs.get("href", ""))
        self.placeholders.update(key for key in attrs if key.startswith("data-handbook-"))


class HandbookTemplateTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        hugo = os.environ.get("HUGO_BIN") or shutil.which("hugo")
        if not hugo:
            raise RuntimeError("Hugo is required: set HUGO_BIN or install hugo on PATH")
        cls.temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temp.cleanup)
        cls.root = Path(cls.temp.name)

        def write(name, content):
            path = cls.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8")

        write("hugo.toml", '''baseURL = "https://docs.invalid/docs/"
defaultContentLanguage = "zh"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
[languages.zh]
weight = 1
[languages.en]
weight = 2
[outputs]
home = ["HTML", "handbook"]
[outputFormats.handbook]
mediaType = "text/html"
baseName = "index"
path = "handbook"
isHTML = true
permalinkable = false
''')
        write("layouts/index.handbook.html", (ROOT / "layouts/index.handbook.html").read_text())
        write("layouts/_partials/print/page-content.html", '{{ return .page.Content }}')
        write("layouts/_default/single.html", "{{ .Content }}")
        write("layouts/_default/list.html", "{{ .Content }}")
        write("assets/css/handbook.css", (ROOT / "assets/css/handbook.css").read_text())
        write("assets/third_party/mermaid/mermaid.min.js", "// Template fixture: no diagrams are authored.\n")
        for language, suffix in (("zh", ""), ("en", ".en")):
            write(f"content/_index{suffix}.md", f"---\ntitle: Home {language}\n---\n")
            for section in ("tutorials", "how-to", "reference", "explanation", "dev", "adr", "releases"):
                write(f"content/{section}/_index{suffix}.md", f"---\ntitle: {section} {language}\n---\n")
                # Reverse filename/weight order to prove that the navigation
                # weight, rather than filesystem order, controls the manual.
                for filename, weight in (("a", 2), ("z", 1)):
                    write(f"content/{section}/{filename}{suffix}.md", f"---\ntitle: {section}-{filename}-{language}\nweight: {weight}\nsource_path: {section}/{filename}.md\n---\nUnique {section}-{filename}-{language} body.\n")
        subprocess.run([hugo, "--source", str(cls.root), "--quiet"], check=True, capture_output=True, text=True)

    def test_only_user_sections_are_included_in_navigation_order_for_each_language(self):
        for language, prefix in (("zh", ""), ("en", "en/")):
            with self.subTest(language=language):
                text = (self.root / "public" / prefix / "handbook/index.html").read_text()
                document = HandbookHTML(text)
                expected = [f"{section}/{name}.md" for section in ("tutorials", "how-to", "reference", "explanation") for name in ("z", "a")]
                self.assertEqual([article["data-source-path"] for article in document.articles], expected)
                self.assertEqual(len({article["id"] for article in document.articles}), len(expected))
                for article in document.articles:
                    self.assertTrue(article["data-page-path"].startswith(f"/docs/{prefix}"))
                    self.assertIn("#" + article["id"], document.links)
                for omitted in ("dev", "adr", "releases"):
                    self.assertNotIn(f"Unique {omitted}-", text)
                self.assertIn(export_pdf.TITLES[language], text)
                self.assertEqual(document.placeholders, {"data-handbook-version", "data-handbook-commit"})


if __name__ == "__main__":
    unittest.main()
