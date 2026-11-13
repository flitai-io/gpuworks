#!/usr/bin/env python3
"""Execute every image-registration enum through Chrome and the real server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Build/run: make test-e2e-image-enums
Individual: python3 test/e2e/image_catalog_enums.py --case engines --locale en
Cases: engines, frameworks, architectures. Locales: en, zh.
Each invocation owns a fresh server/SQLite; no agent, GPU or external registry.
Tagged references are metadata, not downloaded images. Artifacts, private state
and cleanup use image_catalog_runner; optional --server-bin/--artifact-dir.
"""
import json
import re
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import expect

from image_catalog import button, cleanup, observe, open_custom, read_detail, strings
from image_catalog_runner import run
from platform_token_read_revoke import check, collection


FRAMEWORKS = [("cuda", "CUDA"), ("cann", "CANN"), ("rocm", "ROCm"),
              ("hggc", "HGGC"), ("dtk", "DTK"), ("metal", "Metal")]
ARCHITECTURES = [(value, value) for value in ("multi", "amd64", "arm64")]


def choose(control, label):
    control.focus()
    control.press("ArrowDown")
    expect(control).to_have_attribute("aria-expanded", "true")
    # AntD virtual options have a separate ARIA mirror. Click the visible label;
    # the form is new/empty or we select a different value from its default.
    control.page.get_by_text(label, exact=True).filter(visible=True).click()
    expect(control).to_have_attribute("aria-expanded", "false")


def option_labels(control, count):
    """Visit the live virtual ARIA options with the keyboard, including wrap."""
    control.focus()
    control.press("ArrowDown")
    expect(control).to_have_attribute("aria-expanded", "true")
    labels = []
    for _ in range(count + 1):
        active_id = control.get_attribute("aria-activedescendant")
        check(active_id, "open select has no active option")
        active = control.page.locator('[id="' + active_id + '"]')
        expect(active).to_have_attribute("role", "option")
        labels.append(active.get_attribute("aria-label") or active.inner_text())
        control.press("ArrowDown")
    control.press("Escape")
    expect(control).to_have_attribute("aria-expanded", "false")
    check(labels[-1] == labels[0] and len(set(labels[:-1])) == count,
          f"visible enum option count/order changed: {labels}")
    return labels[:-1]


# Contracts: gpuworks/image-catalog/engine-options, gpuworks/image-catalog/framework-options,
#            gpuworks/image-catalog/architecture-options
# Mainline (engines): 空自定义目录与真实引擎字典 → UI逐项选择字典中所有引擎并创建 → 精确POST、独立GET和刷新详情一致 → 清理自有系列；不声明引擎执行。
# Mainline (frameworks): 空自定义目录 → UI分别选择CUDA/CANN/ROCm/HGGC/DTK/Metal并创建 → 精确POST、独立GET和刷新详情一致 → 清理自有系列；不声明框架兼容性。
# Mainline (architectures): 空自定义目录 → UI分别选择multi/amd64/arm64、双向切换rolling并创建 → 精确POST、独立GET和刷新详情一致 → 清理自有系列；不声明对应架构执行。
def test_image_enum_options(page, server, artifacts, report, locale):
    dimension = {"gpuworks/image-catalog/engine-options": "engines",
                 "gpuworks/image-catalog/framework-options": "frameworks",
                 "gpuworks/image-catalog/architecture-options": "architectures"}[report["contract"]]
    text = strings(locale)
    form = text["import"]
    open_custom(page, text)
    check(collection(page.request.get(server.base + "/api/v1/images?source=custom")) == [],
          "isolated custom catalog must start empty")
    engines = collection(page.request.get(server.base + "/api/v1/images/engines"))
    check(engines and len({e["name"] for e in engines}) == len(engines), "invalid engine dictionary")
    dimensions = {"engines": ("engine", [(e["name"], e["label"]) for e in engines]),
                  "frameworks": ("framework", FRAMEWORKS),
                  "architectures": ("hostArch", ARCHITECTURES)}
    field, options = dimensions[dimension]
    errors, failures, mutations = observe(page)
    saved = []
    for index, (value, label) in enumerate(options):
        # Each actual option gets its own UI creation and independent readback.
        name = f'e2e-{report["run_id"]}-{dimension}-{value}'
        ref = f"registry.example/{name}/runner:v1"
        trigger = button(page, text["page"]["addCustom"])
        trigger.click()
        dialog = page.get_by_role("dialog", name=form["title"]["custom"], exact=True)
        expect(dialog).to_be_in_viewport(ratio=1)
        if index == 0:
            labels = option_labels(dialog.get_by_label(form[field], exact=True), len(options))
            check(set(labels) == {item[1] for item in options}, "UI enum differs from declared contract")
            report["evidence"].append({"layer": "ui", "dimension": dimension, "visible_options": labels})
        dialog.get_by_label(form["seriesName"], exact=True).fill(name)
        dialog.get_by_label(form["imageRef"], exact=True).fill(ref)
        engine = value if dimension == "engines" else engines[0]["name"]
        engine_label = next(e["label"] for e in engines if e["name"] == engine)
        framework = value if dimension == "frameworks" else "cuda"
        framework_label = dict(FRAMEWORKS)[framework]
        arch = value if dimension == "architectures" else "multi"
        choose(dialog.get_by_label(form["engine"], exact=True), engine_label)
        choose(dialog.get_by_label(form["framework"], exact=True), framework_label)
        architecture = dialog.get_by_label(form["hostArch"], exact=True)
        if arch != "multi":
            choose(architecture, arch)
        else:
            # Explicitly select even the default; keyboard selection avoids
            # ambiguous duplicate text in the selected value and popup.
            architecture.focus()
            architecture.press("ArrowDown")
            architecture.press("Enter")
        expect(dialog.get_by_text(arch, exact=True).filter(visible=True)).to_be_visible()
        dialog.get_by_label(form["frameworkVersion"], exact=True).fill("1.2.3")
        dialog.get_by_label(form["engineVersion"], exact=True).fill("4.5.6")
        rolling = dialog.get_by_role("switch", name=form["rollingTag"], exact=True)
        expect(rolling).not_to_be_checked()
        rolling.click()
        expect(rolling).to_be_checked()
        rolling.click()
        expect(rolling).not_to_be_checked()
        rolling_value = index % 2 == 0
        if rolling_value:
            rolling.click()
            expect(rolling).to_be_checked()
        # Selected labels are asserted before submitting; then exact wire values.
        expect(dialog.get_by_text(engine_label, exact=True).filter(visible=True)).to_be_visible()
        expect(dialog.get_by_text(framework_label, exact=True).filter(visible=True)).to_be_visible()
        with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/import"
                                  and r.request.method == "POST") as imported:
            button(dialog, form["addImage"]).click()
        response = imported.value
        version_body = {"engine_version": "4.5.6", "framework_version": "1.2.3", "arch": arch,
                        "image_ref": ref, "source": "custom", "rolling_tag": rolling_value, "registry_sources": []}
        payload = {"series": [{"name": name, "engine": engine, "framework": framework,
                               "source": "custom", "versions": [version_body]}]}
        check(response.status == 200 and urlsplit(response.url).query == "", "import response differs")
        check(response.request.post_data_json == payload, "selected enum wire value differs")
        created = collection(response)
        check(len(created) == 1, "import returned multiple series")
        series_id = created[0]["id"]
        saved.append(series_id)
        expect(dialog).not_to_be_visible()
        before = read_detail(page, server.base, series_id)
        check(all(before[k] == v for k, v in payload["series"][0].items() if k != "versions"),
              "series enum persistence differs")
        check(len(before["versions"]) == 1, "duplicate version")
        version = before["versions"][0]
        check(all(version.get(k, [] if k == "registry_sources" else False if k == "rolling_tag" else None) == v
                  for k, v in version_body.items()), "version enum persistence differs")
        check(version["present_nodes"] == 0 and version["total_nodes"] == 0, "metadata invented availability")
        check([v for v in collection(page.request.get(server.base + "/api/v1/images/versions"))
               if v["id"] == version["id"]] == [version], "versions collection differs")
        page.reload()
        open_custom(page, text)
        # The custom tab renders from its own list request; clicking before it
        # lands finds no row and the detail dialog never opens.
        expect(button(page, name)).to_be_visible()
        button(page, name).click()
        # The drawer names the series and puts engine/framework on a subtitle
        # (ImageDetailDrawer.tsx:131-132); it no longer renders a "name · Details"
        # title, so the accessible name is the series name plus that subtitle.
        detail = page.get_by_role("dialog", name=re.compile('^' + re.escape(name)))
        expect(detail).to_be_in_viewport(ratio=1)
        # The drawer shows a version as a picker entry, not a table row
        # (ImageDetailDrawer.tsx:220-232): the created reference is the only
        # option, and choosing it reveals that version's own distribution table,
        # whose single row carries the three enum values that were submitted.
        # The version list is a table keyed by image tag (ImageDetailDrawer.tsx
        # :220-260): one row per version, carrying the reference that was created
        # from these three enum values. The engine/framework/arch strings appear
        # on that row's detail affordance, and the picker only appears when a
        # series owns more than one version.
        tag = detail.locator('[aria-label=' + json.dumps(ref) + ']')
        expect(tag).to_have_count(1)
        expect(tag.first).to_be_visible()
        # The row that owns it is the version entry; its text is what a reader
        # sees for that version, so it has to name the reference the create sent.
        entry = tag.first.locator('xpath=ancestor::tr[1]')
        expect(entry).to_have_count(1)
        shown = entry.inner_text()
        report['enum_version_row'] = shown
        # The entry splits the reference into name and tag (the tag gets its own
        # cell), so the row is checked by both halves rather than the joined form.
        repository, _, tag_part = ref.rpartition(':')
        check(repository in shown and tag_part in shown,
              f"version entry does not carry the created reference: {shown!r}")
        check(read_detail(page, server.base, series_id) == before, "reload changed enum metadata")
        detail.press("Escape")
        expect(detail).not_to_be_visible()
        report["evidence"].extend([
            {"layer": "ui", "option": f"{dimension}:{value}", "selected_label": label,
             "rolling_transitions": [False, True, False] + ([True] if rolling_value else []), "reload_detail": True},
            {"layer": "request", "option": f"{dimension}:{value}", "method": "POST",
             "path": "/api/v1/images/import", "status": response.status, "body": payload},
            {"layer": "persistence", "option": f"{dimension}:{value}", "series": before}])
    check(len(mutations) == len(options) and all(m == {"method": "POST", "path": "/api/v1/images/import"}
          for m in mutations), "unexpected/duplicate mutations")
    check(not errors and not failures, "unexpected browser/request errors")
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    for series_id in reversed(saved):
        cleanup(page, server.base, series_id, report)


if __name__ == "__main__":
    raise SystemExit(run({"engines": test_image_enum_options, "frameworks": test_image_enum_options,
                          "architectures": test_image_enum_options}, entry_path=Path(__file__),
                         product_sources=["test/e2e/image_catalog.py", "ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImageVersionColumns.tsx",
                                          "internal/server/api/images_catalog_import.go"]))
