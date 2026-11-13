#!/usr/bin/env python3
"""Image summary layout on the real server, embedded UI and isolated SQLite.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Build and run both languages: make test-e2e-image-summary-layout
After building the server: python3 test/e2e/image_summary_layout.py --case summary-layout --locale en
Repeat with --locale zh. --server-bin and --artifact-dir use image_catalog_runner.
No management API mocks, agent, registry or container; setup imports three
metadata-only custom versions. The runner retains evidence under secrets/e2e.
"""
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import expect

from image_catalog import cleanup, observe, open_custom, read_detail, strings
from image_catalog_runner import run
from platform_token_read_revoke import check, collection


KEYS = ("series", "images", "distributed", "catalogSize", "localSize")
HELP = {
    "zh": (
        "汇总当前来源中各镜像版本声明的归档体积。未知体积不计入小计；全部未知时显示“—”。归档体积不等于节点实际磁盘占用。",
        "汇总各节点最近上报的镜像副本体积。共享层按每个镜像重复计入，因此该数值不代表去重后的实际磁盘占用。未知体积不计入小计。",
    ),
    "en": (
        "Sum of declared archive sizes for image versions in the current source. Unknown sizes are excluded; “—” means all sizes are unknown. Archive size is not actual node disk usage.",
        "Sum of the most recently reported image-copy sizes across nodes. Shared layers are counted for each image, so this is not deduplicated disk usage. Unknown sizes are excluded.",
    ),
}


def cards(page):
    return [page.get_by_test_id("image-summary-" + key) for key in KEYS]


def assert_layout(page, report, width):
    report["last_step"] = f"summary-layout-{width}"
    page.set_viewport_size({"width": width, "height": 900})
    measured = []
    for key, card in zip(KEYS, cards(page)):
        expect(card).to_be_visible()
        expect(card.get_by_text(strings(report["locale"])["page"]["summary"][key], exact=True)).to_be_visible()
        measured.append(card.evaluate("""el => {
          const r = el.getBoundingClientRect();
          return {width:r.width, height:r.height, top:r.top, bottom:r.bottom,
            overflow:el.scrollWidth > el.clientWidth};
        }"""))
    report.setdefault("layout", []).append({"viewport": width, "cards": measured})
    check(max(r["width"] for r in measured) - min(r["width"] for r in measured) < 1,
          f"summary widths differ: {measured}")
    check(max(r["height"] for r in measured) - min(r["height"] for r in measured) < 1,
          f"summary heights differ: {measured}")
    check(not any(r["overflow"] for r in measured), f"card content overflow: {measured}")
    if width == 1440:
        check(max(r["top"] for r in measured) - min(r["top"] for r in measured) < 1,
              f"desktop cards are not in one row: {measured}")
    for card in cards(page):
        footer = card.get_by_test_id("image-summary-info")
        expect(footer).to_have_count(1)
        budget = footer.evaluate("""el => ({height:el.getBoundingClientRect().height,
          lineHeight:parseFloat(getComputedStyle(el).lineHeight), overflow:el.scrollHeight > el.clientHeight})""")
        check(budget["height"] <= 2 * budget["lineHeight"] + 1 and not budget["overflow"],
              f"auxiliary text exceeds two lines: {budget}")
        for text in card.locator("[data-summary-text]").all():
            check(text.evaluate("el => el.scrollWidth <= el.clientWidth && el.scrollHeight <= el.clientHeight"),
                  f"summary text clipped at {width}: {text.inner_text()}")


def assert_help(page, report, locale):
    for index, key in enumerate(KEYS[-2:]):
        report["last_step"] = "help-" + key
        trigger = page.get_by_test_id("image-summary-" + key).get_by_role("button")
        expect(trigger).to_have_accessible_name(strings(locale)["page"]["summary"][key])
        trigger.hover()
        popup = page.get_by_role("tooltip", name=HELP[locale][index], exact=True)
        expect(popup).to_have_text(HELP[locale][index])
        rect = popup.bounding_box()
        check(rect["x"] >= 0 and rect["y"] >= 0 and rect["x"] + rect["width"] <= page.viewport_size["width"] + 1
              and rect["y"] + rect["height"] <= page.viewport_size["height"] + 1, f"help outside viewport: {rect}")
        # While the help is open it must not sit over another card's help
        # trigger: covering a control the user needs next is the failure this
        # item is about, and staying inside the viewport alone does not catch it.
        covered = []
        for other in KEYS:
            if other == key:
                continue
            candidate = page.get_by_test_id("image-summary-" + other).get_by_role("button")
            if candidate.count() == 0 or not candidate.is_visible():
                continue  # a narrow viewport may stack or drop a card entirely
            box = candidate.bounding_box()
            if box and not (rect["x"] + rect["width"] <= box["x"] or box["x"] + box["width"] <= rect["x"]
                            or rect["y"] + rect["height"] <= box["y"] or box["y"] + box["height"] <= rect["y"]):
                covered.append(other)
        check(not covered, f"the {key} help covers another help trigger: {covered}")
        report.setdefault("help_geometry", []).append({"key": key, "viewport": page.viewport_size["width"],
                                                       "popup": rect, "covers_triggers": covered})
        trigger.click()
        page.get_by_role("heading", name=strings(locale)["page"]["title"], exact=True).hover()
        expect(popup).to_be_visible()
        trigger.press("Escape")
        expect(popup).not_to_be_visible()
        expect(trigger).to_be_focused()
        trigger.press("Tab")
        trigger.focus()
        expect(popup).to_have_text(HELP[locale][index])
        trigger.press("Escape")
        expect(popup).not_to_be_visible()
        trigger.click()
        expect(popup).to_be_visible()
        page.get_by_role("heading", name=strings(locale)["page"]["title"], exact=True).click()
        expect(popup).not_to_be_visible()


# Contract: gpuworks/image-catalog/summary-layout
# Mainline: 真实API准备三个未知归档体积的自定义版本 → 浏览器刷新并切换来源 → 精确统计与三种视口等宽等高、两行辅助区、完整口径提示 → 刷新回读不变 → 删除自有系列并验证零值空态。
def test_image_summary_layout(page, server, artifacts, report, locale):
    text = strings(locale)
    prefix = "e2e-" + report["run_id"]
    versions = [{"image_ref": f"registry.example/{prefix}:v{i}", "engine_version": str(i),
                 "framework_version": "12.8", "arch": "amd64", "source": "custom"} for i in range(3)]
    report["last_step"] = "seed-three-unknown-versions"
    report["fixture_name"] = prefix
    seeded = collection(page.request.post(server.base + "/api/v1/images/import", data={"series": [
        {"name": prefix, "engine": "vllm", "framework": "cuda", "source": "custom", "versions": versions}]}))
    report["remaining_resources"] = [{"series_id": row["id"], "name": row["name"],
        "cleanup": f"DELETE /api/v1/image-series/{row['id']}"} for row in seeded]
    check(len(seeded) == 1, "expected exactly one owned series")
    identity = seeded[0]["id"]
    original = read_detail(page, server.base, identity)
    check(original["version_count"] == 3 and original["present_nodes"] == 0, "fixture counts differ")
    check({v["image_ref"] for v in original["versions"]} == {v["image_ref"] for v in versions}, "fixture refs differ")
    check(all(not v.get("size_bytes") and v["present_nodes"] == 0 for v in original["versions"]),
          "fixture must have unknown archive sizes and no distributed copies")
    errors, failures, mutations = observe(page)

    def reload_and_verify():
        report["last_step"] = "browser-refresh-custom-summary"
        with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images" and r.request.method == "GET") as series_read, \
             page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/versions" and r.request.method == "GET") as versions_read:
            page.reload()
        listed = [r for r in collection(series_read.value) if r["source"] == "custom"]
        observed = [r for r in collection(versions_read.value) if r["source"] == "custom"]
        check([r["id"] for r in listed] == [identity] and len(observed) == 3, "browser source response differs")
        check({r["image_ref"] for r in observed} == {v["image_ref"] for v in versions}, "browser versions differ")
        open_custom(page, text)
        for card, value in zip(cards(page), ["1", "3", "0", "—", "0 B"]):
            expect(card.get_by_test_id("image-summary-value")).to_have_text(value)
        expected_unknown = "3 个版本体积未知" if locale == "zh" else "Unknown: 3 versions"
        expect(cards(page)[3].get_by_text(expected_unknown, exact=True)).to_be_visible()
        report["evidence"].append({"layer": "request", "method": "GET", "paths": ["/api/v1/images", "/api/v1/images/versions"],
            "status": [series_read.value.status, versions_read.value.status], "custom_series": 1, "custom_versions": 3})

    # Geometry runs first so reverting the layout fails on the reported defect,
    # before a missing new help control can mask the layout regression.
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/versions" and r.request.method == "GET"):
        page.reload()
    open_custom(page, text)
    for width in (1440, 1024, 390):
        assert_layout(page, report, width)
    reload_and_verify()
    for width in (1440, 390):
        page.set_viewport_size({"width": width, "height": 900})
        assert_help(page, report, locale)
    reload_and_verify()
    check(read_detail(page, server.base, identity) == original, "read-only summary changed persisted metadata")
    check(not errors and not failures and not mutations, f"unexpected browser errors/HTTP failures/mutations: {errors}, {failures}, {mutations}")
    report["evidence"].extend([{"layer": "persistence", "series_id": identity, "versions": 3, "unchanged_after_reload": True},
        {"layer": "ui", "counts": [1, 3, 0], "archive": "—", "local": "0 B", "unknown_versions": 3,
         "layout": report["layout"], "help": list(HELP[locale])}])
    cleanup(page, server.base, identity, report)
    report["remaining_resources"] = []
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/versions" and r.request.method == "GET"):
        page.reload()
    open_custom(page, text)
    for card, value in zip(cards(page), ["0", "0", "0", "0 B", "0 B"]):
        expect(card.get_by_test_id("image-summary-value")).to_have_text(value)
    assert_layout(page, report, 1440)
    check(not errors and not failures, f"unexpected errors after empty-source check: {errors}, {failures}")
    report["last_step"] = "empty-summary-and-cleanup-verified"
    page.screenshot(path=artifacts.directory / "summary-empty.png")


if __name__ == "__main__":
    raise SystemExit(run({"summary-layout": test_image_summary_layout}, entry_path=Path(__file__),
        product_sources=("ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImagesPageOverview.tsx",
                         "ui/src/pages/images/ImageSummaryCards.tsx", "ui/src/pages/images/imageSummary.css",
                         "ui/src/styles.css"), metadata_offline=True))
