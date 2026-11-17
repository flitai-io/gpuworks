#!/usr/bin/env python3
"""Stage the repository's Chinese and English docs into the OINK content tree.

Markdown comes from ../docs and ../docs-en, with ../docs as the translation
baseline. Hugo gets generated copies so sources need no Hugo-specific syntax.
"""

from __future__ import annotations

import json
import re
import shutil
import unicodedata
from pathlib import Path

import translation_markers


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "docs"
DEST = Path(__file__).resolve().parent / "content"
STATIC_ASSETS = Path(__file__).resolve().parent / "static" / "assets"

SECTION_META = {
    "tutorials": ("用户教程", "从零启动 GPUWorks，并完成第一个模型部署。"),
    "how-to": ("操作指南", "针对安装、部署、同步、监控和生产环境的任务型文档。"),
    "reference": ("参考手册", "API、CLI、配置、支持矩阵和安全边界的精确说明。"),
    "explanation": ("架构说明", "解释 GPUWorks 的连接、调度、显存估算和 dGPU 设计。"),
    "releases": ("发布说明", "各版本的新增功能、行为变化、缺陷修复与升级须知。"),
    "dev": ("开发者文档", "本地搭建、测试、代码地图、发布和贡献约定。"),
    "adr": ("架构决策记录", "已接受的长期架构取舍和协议边界。"),
}
SECTION_WEIGHTS = {slug: weight for weight, slug in enumerate(SECTION_META, start=1)}
ENGLISH_SECTION_META = {
    "tutorials": ("Tutorials", "Start GPUWorks and deploy your first model."),
    "how-to": ("How-to Guides", "Complete installation, deployment, synchronization, monitoring, and production tasks."),
    "reference": ("Reference", "Look up APIs, CLI commands, configuration, supported hardware, and security boundaries."),
    "explanation": ("Architecture", "Understand connections, scheduling, GPU memory estimation, and dGPU."),
    "releases": ("Release Notes", "See what each version adds, changes, and fixes, and what to check before upgrading."),
    "dev": ("Developer Documentation", "Set up, test, navigate, release, and contribute to GPUWorks."),
    "adr": ("Architecture Decision Records", "Review accepted architectural decisions and protocol boundaries."),
}


def how_to_groups(source_dir: Path, *, allow_missing: bool = False) -> list[dict]:
    """Read navigation labels and order from the documentation index."""
    lines = (source_dir / "README.md").read_text(encoding="utf-8").splitlines()
    starts = [
        index for index, line in enumerate(lines)
        if re.fullmatch(r"###\s+(?:.+[（(]how-to[）)]|How-To Guides)\s*", line, re.IGNORECASE)
    ]
    if len(starts) != 1:
        raise ValueError("docs/README.md must contain one how-to section")

    groups = []
    seen_paths = set()
    seen_titles = set()
    for line in lines[starts[0] + 1:]:
        if re.match(r"#{1,3}\s", line):
            break
        group = re.fullmatch(r"\s*\*\*(.+?)\*\*\s*", line)
        if group:
            title = group.group(1).strip()
            if not title or title in seen_titles:
                raise ValueError(f"empty or duplicate how-to group: {title!r}")
            groups.append({"title": title, "pages": []})
            seen_titles.add(title)
            continue

        entry = re.match(r"\s*[-*+]\s+\[([^\]]+)\]\(([^)]+)\)", line)
        if not entry:
            continue
        title, path = entry.group(1), entry.group(2).strip()
        if not groups:
            raise ValueError(f"how-to page appears before a group: {path}")
        if (
            not path.startswith("how-to/")
            or not path.endswith(".md")
            or any(part in {"", ".", ".."} for part in path.split("/"))
            or any(char in path for char in "\\?#")
            or Path(path).name in {"README.md", "_index.md"}
        ):
            raise ValueError(f"invalid how-to page path: {path}")
        target = source_dir / path
        if not target.resolve().is_relative_to((source_dir / "how-to").resolve()):
            raise ValueError(f"how-to page escapes its source directory: {path}")
        if not target.is_file():
            if allow_missing:
                continue
            raise ValueError(f"how-to page does not exist: {path}")
        if path in seen_paths:
            raise ValueError(f"duplicate how-to page: {path}")
        seen_paths.add(path)
        groups[-1]["pages"].append({"title": title, "path": path})

    if allow_missing:
        groups = [group for group in groups if group["pages"]]
    elif not groups or any(not group["pages"] for group in groups):
        raise ValueError("how-to navigation must contain nonempty groups")
    pages = {
        path.relative_to(source_dir).as_posix()
        for path in (source_dir / "how-to").rglob("*.md")
        if path.is_file() and path.name not in {"README.md", "_index.md"}
    }
    missing = pages - seen_paths
    if missing:
        raise ValueError(f"how-to pages missing from docs/README.md: {', '.join(sorted(missing))}")
    return groups


def tutorial_weights(source_dir: Path) -> dict[str, int]:
    weights = {}
    in_tutorials = False
    for line in (source_dir / "README.md").read_text(encoding="utf-8").splitlines():
        if re.match(r"#{1,3}\s", line):
            in_tutorials = bool(re.fullmatch(r"###\s+(?:.+[（(]tutorials[）)]|Tutorials)\s*", line, re.IGNORECASE))
        if not in_tutorials:
            continue
        entry = re.match(r"\s*[-*+]\s+\[[^\]]+\]\((tutorials/[^\s)?#]+\.md)\)", line)
        if entry and entry.group(1) not in weights:
            weights[entry.group(1)] = len(weights) + 1
    return weights


def release_weights(source_dir: Path) -> dict[str, int]:
    """Order release pages as releases/README.md lists them, newest first."""
    index = source_dir / "releases" / "README.md"
    if not index.is_file():
        return {}
    weights = {}
    for link in re.findall(r"\]\(((?!\.\./)[^\s)?#/]+\.md)\)", index.read_text(encoding="utf-8")):
        path = f"releases/{link}"
        if path not in weights:
            weights[path] = len(weights) + 1
    return weights


def quote(value: str) -> str:
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def first_heading(text: str, fallback: str) -> str:
    match = re.search(r"(?m)^#\s+(.+?)\s*$", text)
    if not match:
        return fallback.replace("-", " ").replace("_", " ").title()
    return re.sub(r"[`*_]", "", match.group(1)).strip()


def add_front_matter(text: str, title: str, **metadata) -> str:
    fields = "".join(f"{key}: {json.dumps(value, ensure_ascii=False)}\n" for key, value in metadata.items())
    if text.startswith("---\n"):
        return "---\n" + fields + text[4:]
    return "---\n" + f"title: {quote(title)}\n" + "type: docs\n" + fields + "---\n\n" + text


def how_to_index(groups: list[dict], language: str = "zh", **metadata) -> str:
    sections = []
    for group in groups:
        links = [
            f'- [{page["title"]}]({page["path"].removeprefix("how-to/").removesuffix(".md")}/)'
            for page in group["pages"]
        ]
        sections.append(f'## {group["title"]}\n\n' + "\n".join(links))
    title, description = (ENGLISH_SECTION_META if language == "en" else SECTION_META)["how-to"]
    return add_front_matter(
        "\n\n".join(sections) + "\n", title,
        description=description, no_list=True,
        weight=SECTION_WEIGHTS["how-to"], guide_groups=groups, **metadata,
    )


def remove_leading_heading(text: str) -> str:
    """Let OINK render the front matter title once instead of twice."""
    return re.sub(r"\A\s*#\s+[^\n]+\n+", "", text, count=1)


def headings(text: str) -> list[tuple[int, str, str]]:
    result = []
    fence = ""
    for index, line in enumerate(text.splitlines()):
        marker = re.match(r"^\s*(`{3,}|~{3,})", line)
        if marker:
            if not fence:
                fence = marker[1]
            elif marker[1][0] == fence[0] and len(marker[1]) >= len(fence):
                fence = ""
            continue
        match = re.match(r"^(#{1,6})\s+(.+?)\s*$", line)
        if match and not fence:
            result.append((index, match[1], match[2]))
    return result


def preserve_heading_ids(original: str, translated: str) -> str:
    source_headings = headings(original)
    target_headings = headings(translated)
    if [h[1] for h in source_headings] != [h[1] for h in target_headings]:
        raise ValueError("translation must preserve the source heading structure")
    lines = translated.splitlines(keepends=True)
    seen = {}
    for source, target in zip(source_headings, target_headings):
        title = source[2].lower().strip()
        explicit = re.search(r"\s+\{#([^}]+)\}$", title)
        anchor = explicit[1] if explicit else "".join(
            "-" if char == " " else char for char in title
            if char in " -_" or unicodedata.category(char)[0] in "LNM"
        )
        duplicate = seen.get(anchor, 0)
        seen[anchor] = duplicate + 1
        if duplicate:
            anchor += f"-{duplicate}"
        if target[1] != "#":
            lines[target[0]] = re.sub(r"\s+\{#[^}]+\}$", "", lines[target[0]].rstrip("\n")) + f" {{#{anchor}}}\n"
    return "".join(lines)


IMAGE_RE = re.compile(r"(!\[[^\]]*\]\()((?:\.\./)*(?:\./)?assets/[^)\s]+)(\))")
ASSET_LINK_RE = re.compile(r"(?<!!)(\[[^\]]*\]\()((?:\.\./)*(?:\./)?assets/[^)\s]+)(\))")


def rewrite_images(text: str) -> str:
    """Point ![](../assets/x.png) at the published static copy.

    Hugo serves pages from directory-style URLs, so a relative image path that
    works on GitHub would resolve one level too deep on the site. The OINK
    image render hook prepends the site's base path to root-relative paths, so
    the rewrite must not include it.
    """
    return IMAGE_RE.sub(
        lambda m: m.group(1) + "/assets/" + m.group(2).split("assets/", 1)[1] + m.group(3),
        text,
    )


def rewrite_asset_links(text: str, source: Path, language: str = "zh") -> str:
    """Resolve downloads from directory-style page URLs under any base path."""
    levels = len(source.parts) - (source.name in {"README.md", "_index.md"})
    levels += language != "zh"
    prefix = "../" * levels
    return ASSET_LINK_RE.sub(
        lambda m: m.group(1) + prefix + "assets/" + m.group(2).split("assets/", 1)[1] + m.group(3),
        text,
    )


def home_page(language: str = "zh") -> str:
    if language == "en":
        return add_front_matter(
            "GPUWorks is a lightweight management platform for GPU clusters. "
            "Browse tutorials, task guides, reference material, architecture, and developer documentation.\n\n"
            "## Start Here\n\n"
            "- [Start GPUWorks and deploy your first model](tutorials/getting-started/)\n"
            "- [Connect nodes and check GPUs](how-to/connect-nodes/)\n"
            "- [Deploy models and call the OpenAI-compatible API](how-to/deploy-models/)\n"
            "- [Troubleshoot problems](how-to/troubleshooting/)\n"
            "- [Browse how-to guides by category](how-to/)\n"
            "- [Look up APIs, CLI commands, and configuration](reference/)\n"
            "- [Read the release notes](releases/)\n",
            "GPUWorks Documentation", description="From your first GPU to a working inference service.",
        )
    return (
        "---\n"
        'title: "GPUWorks 文档"\n'
        'description: "从第一台 GPU，到可用的推理服务。"\n'
        "type: docs\n"
        "---\n\n"
        "GPUWorks 是一个面向 GPU 集群的轻量管理平台。这里汇总当前版本的教程、操作指南、"
        "参考手册、架构说明和开发者文档。\n\n"
        "## 推荐入口\n\n"
        "- [从零启动并部署第一个模型](tutorials/getting-started/)\n"
        "- [接入节点并检查 GPU](how-to/connect-nodes/)\n"
        "- [部署模型并调用 OpenAI 兼容 API](how-to/deploy-models/)\n"
        "- [遇到问题：排障指南](how-to/troubleshooting/)\n"
        "- [按分类浏览操作指南](how-to/)\n"
        "- [查看 API、CLI 和配置参考](reference/)\n"
        "- [查看各版本发布说明](releases/)\n"
    )


def main() -> None:
    if not SOURCE.is_dir():
        raise SystemExit(f"missing documentation source: {SOURCE}")
    groups = how_to_groups(SOURCE)
    navigation = {
        page["path"]: {"linkTitle": page["title"], "weight": weight}
        for weight, page in enumerate(
            (page for group in groups for page in group["pages"]), start=1
        )
    }
    navigation.update({path: {"weight": weight} for path, weight in tutorial_weights(SOURCE).items()})
    navigation.update({path: {"weight": weight} for path, weight in release_weights(SOURCE).items()})
    shutil.rmtree(DEST, ignore_errors=True)
    DEST.mkdir(parents=True)

    shutil.rmtree(STATIC_ASSETS, ignore_errors=True)
    if (SOURCE / "assets").is_dir():
        shutil.copytree(SOURCE / "assets", STATIC_ASSETS)

    for source in sorted(SOURCE.rglob("*.md")):
        relative = source.relative_to(SOURCE)
        target_relative = relative
        if relative.name == "README.md":
            target_relative = relative.with_name("_index.md")
        target = DEST / target_relative
        target.parent.mkdir(parents=True, exist_ok=True)
        body = source.read_text(encoding="utf-8")
        if relative == Path("README.md"):
            target.write_text(home_page(), encoding="utf-8")
            continue
        body = rewrite_images(body)
        body = rewrite_asset_links(body, relative)
        title = first_heading(body, target.stem)
        metadata = navigation.get(relative.as_posix(), {})
        if relative.name == "README.md" and relative.parent.as_posix() in SECTION_WEIGHTS:
            metadata = {"weight": SECTION_WEIGHTS[relative.parent.as_posix()]}
        target.write_text(
            add_front_matter(remove_leading_heading(body), title, **metadata),
            encoding="utf-8",
        )

    (DEST / "how-to" / "_index.md").write_text(how_to_index(groups), encoding="utf-8")

    for slug, (title, description) in SECTION_META.items():
        section = DEST / slug / "_index.md"
        if section.exists():
            continue
        section.parent.mkdir(parents=True, exist_ok=True)
        section.write_text(
            "---\n"
            f"title: {quote(title)}\n"
            f"description: {quote(description)}\n"
            f"weight: {SECTION_WEIGHTS[slug]}\n"
            "type: docs\n---\n",
            encoding="utf-8",
        )

    count = sum(1 for _ in DEST.rglob("*.md"))
    print(f"staged {count} Markdown pages from {SOURCE}")
    english_source = SOURCE.with_name("docs-en")
    if (english_source / "README.md").is_file():
        stage_english(english_source)


def stage_english(english_source: Path) -> None:
    groups = how_to_groups(english_source, allow_missing=True)
    navigation = {
        page["path"]: {"linkTitle": page["title"], "weight": weight}
        for weight, page in enumerate(
            (page for group in groups for page in group["pages"]), start=1
        )
    }
    navigation.update({path: {"weight": weight} for path, weight in tutorial_weights(english_source).items()})
    navigation.update({path: {"weight": weight} for path, weight in release_weights(english_source).items()})
    for source in sorted(english_source.rglob("*.md")):
        relative = source.relative_to(english_source)
        if not (SOURCE / relative).is_file():
            raise ValueError(f"English page has no Chinese source: {relative}")
        stem = "_index" if relative.name == "README.md" else relative.stem
        target = DEST / relative.with_name(stem + ".en.md")
        target.parent.mkdir(parents=True, exist_ok=True)
        if relative == Path("README.md"):
            body = home_page("en")
        else:
            body = translation_markers.strip(source.read_text(encoding="utf-8"))
            body = preserve_heading_ids((SOURCE / relative).read_text(encoding="utf-8"), body)
            title = first_heading(body, stem)
            body = rewrite_asset_links(rewrite_images(body), relative, "en")
            metadata = navigation.get(relative.as_posix(), {})
            if relative.name == "README.md" and relative.parent.as_posix() in SECTION_WEIGHTS:
                metadata = {"weight": SECTION_WEIGHTS[relative.parent.as_posix()]}
            body = add_front_matter(remove_leading_heading(body), title, **metadata)
        body = body.replace("type: docs\n", f"type: docs\nsource_path: {quote(relative.as_posix())}\n", 1)
        mapping = {"from": "^content/.*$", "to": "docs-en/" + relative.as_posix()}
        body = body.replace("type: docs\n", "type: docs\npath_base_for_github_subdir: " + json.dumps(mapping) + "\n", 1)
        if relative == Path("README.md"):
            total = sum(1 for _ in SOURCE.rglob("*.md"))
            translated = sum(1 for _ in english_source.rglob("*.md"))
            if translated < total:
                body += f"\n## Translation Coverage\n\n{translated} of {total} source documents have English translations. Links marked (Chinese) open the original document. Screenshots retain the original Chinese interface.\n"
        target.write_text(body, encoding="utf-8")
    index_mapping = {"from": "^content/.*$", "to": "docs-en/README.md"}
    (DEST / "how-to" / "_index.en.md").write_text(
        how_to_index(groups, "en", path_base_for_github_subdir=index_mapping), encoding="utf-8"
    )
    for slug, (title, description) in ENGLISH_SECTION_META.items():
        section = DEST / slug / "_index.en.md"
        if not section.exists():
            pending = ""
            if not any((english_source / slug).rglob("*.md")):
                pending = "English translations for this section are pending. Use the language selector to read the Chinese documentation.\n"
            section.write_text(add_front_matter(
                pending, title, description=description, weight=SECTION_WEIGHTS[slug],
                path_base_for_github_subdir=index_mapping,
            ).rstrip() + "\n", encoding="utf-8")
    if not groups:
        with (DEST / "how-to" / "_index.en.md").open("a", encoding="utf-8") as output:
            output.write("\nEnglish translations for this section are pending. Use the language selector to read the Chinese documentation.\n")


if __name__ == "__main__":
    main()
