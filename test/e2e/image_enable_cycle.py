#!/usr/bin/env python3
"""Image series/version switches against the real server and SQLite.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-image-enable-cycle (builds, then English).
After building: python3 test/e2e/image_enable_cycle.py --case enabled-cycle --locale en
Use --locale zh for Chinese; --server-bin and --artifact-dir use the shared runner.
Set NO_PROXY/no_proxy=127.0.0.1,localhost when using a proxy.

Owns a fresh server/SQLite and two metadata-only versions. No agent, registry,
Docker, scheduling or runtime availability claim. No management API mocks.
Artifacts and failed DB stay in ignored secrets/e2e/<runId>; remove only that
directory within seven days. Cleanup deletes only the owned series and versions.
"""
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import expect

from dialog_checks import assert_drawer_shell
from image_catalog import button, cleanup, observe, open_custom, read_detail, strings
from image_catalog_runner import run
from platform_token_read_revoke import check, collection


# Contract: gpuworks/image-catalog/series-version-enable-cycle
# Mainline: 自有系列与A/B版本 → UI关闭A、刷新后用键盘重启A，再关闭B → 关闭父系列时两个版本均禁用 → 刷新并恢复父系列后A开B关不变 → 精确PATCH、独立系列/版本GET及刷新一致；清理只删自有目录，不声明调度或运行时。
def test_image_enable_cycle(page, server, artifacts, report, locale):
    text = strings(locale)
    name = "e2e-" + report["run_id"]
    refs = [f"registry.example/{name}/runner:{tag}" for tag in ["a", "b"]]
    seeded = collection(page.request.post(server.base + "/api/v1/images/import", data={
        "series": [{"name": name, "engine": "vllm", "framework": "cuda", "source": "custom",
            "versions": [{"image_ref": ref, "framework_version": "12.8", "engine_version": "1.0",
                          "arch": "multi", "source": "custom"} for ref in refs]}]}))
    check(len(seeded) == 1, "fixture must create one series")
    series_id = seeded[0]["id"]
    series_path = f"/api/v1/images/{series_id}"
    original = read_detail(page, server.base, series_id)
    versions = {row["image_ref"]: row for row in original["versions"]}
    check(set(versions) == set(refs), "fixture versions differ")
    ids = [versions[ref]["id"] for ref in refs]
    version_paths = [f"/api/v1/images/versions/{identity}" for identity in ids]
    errors, failures, mutations = observe(page)
    requests = []
    report["active_evidence_layer"] = "ui"

    def version_switch(dialog, index):
        return dialog.get_by_role("switch", name=text["versionTable"]["enabledLabel"].replace("{{ref}}", refs[index]), exact=True)

    def series_switch():
        return page.get_by_role("switch", name=text["seriesTable"]["enabledLabel"].replace("{{name}}", name), exact=True)

    shell_checked = {"done": False}

    def open_detail():
        open_custom(page, text)
        with page.expect_response(lambda r: urlsplit(r.url).path == series_path and r.request.method == "GET") as read:
            button(page, name).click()
        check(read.value.status == 200, "browser detail read failed")
        # The drawer's accessible name is the series name alone; the engine and framework moved
        # to the subtitle line under the title (ImageDetailDrawer.tsx:131-132), which is why the
        # header's own text still carries "<series>\n<engine> · <framework>".
        dialog_name = name
        dialog = page.get_by_role("dialog", name=dialog_name, exact=True)
        expect(dialog).to_be_in_viewport(ratio=1)
        if not shell_checked["done"]:
            evidence = assert_drawer_shell(
                page, dialog, f"{name}\nvllm · cuda",
                version_switch(dialog, 0),
                page.get_by_role("dialog", name=text["import"]["title"]["custom"], exact=True))
            evidence["accessible_name"] = dialog_name
            report["evidence"].append(evidence)
            shell_checked["done"] = True
        return dialog

    def close_detail(dialog):
        dialog.press("Escape")
        expect(dialog).not_to_be_visible()
        expect(button(page, name)).to_be_focused()

    def assert_state(parent_enabled, child_enabled, dimension):
        saved = read_detail(page, server.base, series_id)
        check(saved["enabled"] is parent_enabled and saved["version_count"] == 2, "series state/count differs")
        rows = {row["id"]: row for row in saved["versions"]}
        check(set(rows) == set(ids), "version identity changed")
        for index, identity in enumerate(ids):
            row = rows[identity]
            check(row["enabled"] is child_enabled[index], "series change overwrote version's own setting")
            check(row["effective_enabled"] is (parent_enabled and child_enabled[index]), "effective enabled differs")
            unchanged = {k: v for k, v in row.items() if k not in {"enabled", "effective_enabled"}}
            before = {k: v for k, v in versions[refs[index]].items() if k not in {"enabled", "effective_enabled"}}
            check(unchanged == before, "switch changed unrelated version metadata")
            check(row["present_nodes"] == 0 and row["total_nodes"] == 0, "catalog switch invented node availability")
        listed = [row for row in collection(page.request.get(server.base + "/api/v1/images")) if row["id"] == series_id]
        check(len(listed) == 1 and listed[0]["enabled"] is parent_enabled, "series collection differs")
        version_list = {row["id"]: row for row in collection(page.request.get(server.base + "/api/v1/images/versions")) if row["series_id"] == series_id}
        check(version_list == rows, "version collection differs from series detail")
        report["evidence"].append({"layer": "persistence", "dimension": dimension, "series": saved})

    def reload_state(parent_enabled, child_enabled, dimension):
        page.reload()
        open_custom(page, text)
        expect(series_switch()).to_be_checked(checked=parent_enabled)
        dialog = open_detail()
        for index, child in enumerate(child_enabled):
            control = version_switch(dialog, index)
            expect(control).to_be_checked(checked=parent_enabled and child)
            expect(control).to_be_disabled() if not parent_enabled else expect(control).to_be_enabled()
            row = dialog.get_by_role("row").filter(has=page.get_by_role("link", name=refs[index], exact=True))
            expect(row.get_by_text(text["versionTable"]["disabledBySeries"], exact=True)).to_have_count(0 if parent_enabled else 1)
        assert_state(parent_enabled, child_enabled, dimension)
        report["evidence"].append({"layer": "ui", "dimension": dimension,
            "reload": True, "series_enabled": parent_enabled, "version_enabled": child_enabled,
            "switches_disabled": not parent_enabled, "named_controls": True})
        return dialog

    def toggle(control, path, enabled, keyboard=False):
        expect(control).to_be_enabled()
        expect(control).to_be_in_viewport(ratio=1)
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "PATCH") as changed:
            if keyboard:
                control.focus()
                expect(control).to_be_focused()
                control.press("Space")
            else:
                control.click()
        response = changed.value
        check(response.status == 200 and urlsplit(response.url).query == "", "PATCH status/query differs")
        check(response.request.post_data_json == {"enabled": enabled}, "PATCH boolean body differs")
        check(response.json()["enabled"] is enabled, "PATCH response state differs")
        expect(control).to_be_checked(checked=enabled)
        requests.append({"method": "PATCH", "path": path, "body": {"enabled": enabled}, "status": 200, "keyboard": keyboard})

    dialog = reload_state(True, [True, True], "initial")
    toggle(version_switch(dialog, 0), version_paths[0], False)
    assert_state(True, [False, True], "version-a-disabled")
    dialog = reload_state(True, [False, True], "version-a-disabled-reload-editable")
    toggle(version_switch(dialog, 0), version_paths[0], True, keyboard=True)
    dialog = reload_state(True, [True, True], "version-a-reenabled")
    toggle(version_switch(dialog, 1), version_paths[1], False)
    assert_state(True, [True, False], "version-b-disabled")
    close_detail(dialog)
    toggle(series_switch(), series_path, False)
    dialog = reload_state(False, [True, False], "series-disabled-keeps-children")
    page.screenshot(path=artifacts.directory / "series-disabled.png")
    close_detail(dialog)
    toggle(series_switch(), series_path, True, keyboard=True)
    dialog = reload_state(True, [True, False], "series-restored-preserves-mixed-children")
    page.screenshot(path=artifacts.directory / "series-restored.png")
    close_detail(dialog)
    check(mutations == [{"method": "PATCH", "path": path} for path in [version_paths[0], version_paths[0], version_paths[1], series_path, series_path]],
          "unexpected/duplicate mutation or parent cycle wrote a child")
    check(not errors and not failures, "unexpected browser or HTTP error")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page horizontal overflow")
    report["evidence"].append({"layer": "request", "patches": requests, "no_duplicate_or_child_patch_during_parent_cycle": True})
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    report["active_evidence_layer"] = "cleanup"
    cleanup(page, server.base, series_id, report)


if __name__ == "__main__":
    raise SystemExit(run({"enabled-cycle": test_image_enable_cycle}, entry_path=Path(__file__), product_sources=[
        "test/e2e/image_catalog.py", "ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImageSeriesTable.tsx",
        "ui/src/pages/images/ImageVersionColumns.tsx", "ui/src/pages/images/ImageDetailDrawer.tsx",
        "internal/server/api/images.go", "internal/server/api/images_catalog_import.go"]))
