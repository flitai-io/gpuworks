#!/usr/bin/env python3
"""Print the built, release-specific user handbook using existing browser tools."""
from __future__ import annotations

import argparse
import functools
import hashlib
from html.parser import HTMLParser
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import threading
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parent
TITLES = {"zh": "GPUWorks 用户手册", "en": "GPUWorks User Guide"}


class Metadata(HTMLParser):
    def __init__(self, source):
        super().__init__()
        self.base_url = ""
        self.feed(source)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "meta" and attrs.get("name") == "handbook-base-url":
            self.base_url = attrs.get("content", "")


def validate_identity(version, commit):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,79}", version):
        raise ValueError("version must be a filename-safe release identifier")
    if not re.fullmatch(r"[0-9a-f]{7,40}", commit):
        raise ValueError("commit must be the source Git commit (7 to 40 hex digits)")


def validate_pdf(path, language, version, commit, last_title):
    data = path.read_bytes()
    if not data.startswith(b"%PDF-") or not data.rstrip().endswith(b"%%EOF"):
        raise ValueError(f"{path}: incomplete PDF")
    text = subprocess.run(["pdftotext", str(path), "-"], check=True, capture_output=True, text=True).stdout
    normalized = " ".join(text.split())
    for expected in (TITLES[language], version, commit, last_title):
        if " ".join(expected.split()) not in normalized:
            raise ValueError(f"{path}: missing searchable text: {expected}")
    info = subprocess.run(["pdfinfo", str(path)], check=True, capture_output=True, text=True).stdout
    pages = re.search(r"(?m)^Pages:\s+(\d+)$", info)
    if not pages or int(pages[1]) < 3:
        raise ValueError(f"{path}: handbook has too few pages")
    fonts = subprocess.run(["pdffonts", str(path)], check=True, capture_output=True, text=True).stdout
    if language == "zh" and not re.search(r"NotoSansCJK.*\s+yes\s+", fonts):
        raise ValueError(f"{path}: Chinese font is not embedded")
    destinations = subprocess.run(["pdfinfo", "-dests", str(path)], check=True, capture_output=True, text=True)
    if destinations.stderr:
        raise ValueError(f"{path}: invalid PDF destinations: {destinations.stderr.strip()}")
    return int(pages[1])


class SiteHandler(SimpleHTTPRequestHandler):
    def __init__(self, *args, prefix, **kwargs):
        self.prefix = prefix
        super().__init__(*args, **kwargs)

    def do_GET(self):
        if not urlsplit(self.path).path.startswith(self.prefix):
            self.send_error(404)
            return
        self.path = "/" + self.path[len(self.prefix):]
        super().do_GET()

    def log_message(self, *_args):
        pass


def export(public, output, version, commit, chrome):
    from playwright.sync_api import sync_playwright

    validate_identity(version, commit)
    manifest = json.loads((public / "docs-manifest.json").read_text(encoding="utf-8"))
    source_commit = manifest.get("commit", "")
    if not re.fullmatch(r"[0-9a-f]{7,40}", source_commit) or not (source_commit.startswith(commit) or commit.startswith(source_commit)):
        raise ValueError("built documentation does not match the requested source commit")
    for tool in ("pdftotext", "pdfinfo", "pdffonts", "fc-match"):
        if not shutil.which(tool):
            raise RuntimeError(f"{tool} is required to validate PDF output")
    font_languages = subprocess.run(["fc-match", "-f", "%{lang}", "Noto Sans CJK SC"], check=True, capture_output=True, text=True).stdout
    if "zh-cn" not in font_languages:
        raise RuntimeError("Install Noto Sans CJK SC before generating the Chinese handbook")
    inputs = {language: public / ("en/handbook/index.html" if language == "en" else "handbook/index.html") for language in TITLES}
    metadata = {language: Metadata(path.read_text(encoding="utf-8")) for language, path in inputs.items()}
    bases = {item.base_url for item in metadata.values()}
    if len(bases) != 1 or not urlsplit(next(iter(bases))).netloc:
        raise ValueError("both handbooks must have the same canonical site URL")
    prefix = urlsplit(next(iter(bases))).path.rstrip("/") + "/"
    names = {f"gpuworks-{version}-{language}.pdf" for language in TITLES} | {"SHA256SUMS"}
    if output.exists() and any(path.name not in names or not path.is_file() or path.is_symlink() for path in output.iterdir()):
        raise ValueError("output directory contains unrelated files; choose an empty directory")
    output.parent.mkdir(parents=True, exist_ok=True)
    handler = functools.partial(SiteHandler, directory=str(public), prefix=prefix)
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="docs-pdf-", dir=output.parent) as temporary, sync_playwright() as play:
            stage = Path(temporary)
            browser = play.chromium.launch(executable_path=chrome, headless=True)
            try:
                for language in TITLES:
                    context = browser.new_context(locale="zh-CN" if language == "zh" else "en-US", color_scheme="light", viewport={"width": 688, "height": 1000})
                    errors = []
                    local = f"http://127.0.0.1:{server.server_port}"

                    def route(request):
                        if request.request.url.startswith((local + "/", "data:")):
                            request.continue_()
                        else:
                            errors.append(f"external resource: {request.request.url}")
                            request.abort()

                    context.route("**/*", route)
                    page = context.new_page()
                    page.on("pageerror", lambda error: errors.append(str(error)))
                    page.on("response", lambda response: errors.append(f"HTTP {response.status}: {response.url}") if response.status >= 400 else None)
                    page.on("requestfailed", lambda request: errors.append(f"{request.failure}: {request.url}"))
                    try:
                        page.emulate_media(media="print")
                        response = page.goto(local + prefix + ("en/" if language == "en" else "") + "handbook/", wait_until="load", timeout=120_000)
                        if response.status != 200:
                            raise RuntimeError(f"handbook returned HTTP {response.status}")
                        try:
                            details = page.evaluate((ROOT / "prepare_pdf.js").read_text(), {"version": version, "commit": commit, "language": language})
                        except Exception as error:
                            raise RuntimeError(f"{language} handbook: {error}; resource failures: {errors}") from error
                        if errors:
                            raise RuntimeError("; ".join(errors))
                        name = f"gpuworks-{version}-{language}.pdf"
                        page.pdf(path=str(stage / name), format="A4", print_background=True, prefer_css_page_size=True,
                                 display_header_footer=True, header_template="<span></span>",
                                 footer_template='<div style="width:100%;text-align:center;font-size:8px;color:#526078"><span class="pageNumber"></span> / <span class="totalPages"></span></div>',
                                 outline=True, tagged=True)
                        details["pages"] = validate_pdf(stage / name, language, version, commit, details.pop("lastTitle"))
                        print(json.dumps(details, ensure_ascii=False))
                    finally:
                        context.close()
            finally:
                browser.close()
            sums = "".join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in sorted(stage.glob("*.pdf")))
            (stage / "SHA256SUMS").write_text(sums, encoding="utf-8")
            output.mkdir(exist_ok=True)
            for name in sorted(names):
                (stage / name).replace(output / name)
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--public", type=Path, default=ROOT / "public")
    parser.add_argument("--output", type=Path, default=ROOT.parent / "dist/docs")
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--chrome", default=os.environ.get("CHROME_PATH") or shutil.which("google-chrome") or shutil.which("chromium"))
    args = parser.parse_args()
    if not args.chrome:
        parser.error("install Chrome/Chromium or set CHROME_PATH")
    export(args.public.resolve(), args.output.resolve(), args.version, args.commit, args.chrome)


if __name__ == "__main__":
    main()
