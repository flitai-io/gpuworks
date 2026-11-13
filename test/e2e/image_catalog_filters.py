#!/usr/bin/env python3
"""Image series client-side filtering against a real server and SQLite.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-image-filters
After building: python3 test/e2e/image_catalog_filters.py --case filters --locale en
Use --locale zh for Chinese; optional --server-bin and --artifact-dir follow the
shared runner. Set NO_PROXY/no_proxy=127.0.0.1,localhost when using a proxy.

Fixtures import owned custom/community series through the real API. Filtering
uses the complete GET /images response locally: no invented filter query, no
management mock, no agent/GPU/container. Architecture belongs to versions and
has no series filter. This page has no keyword search. Artifacts, trace and
failure SQLite stay in ignored secrets/e2e/<runId>; stop the owned server and
remove private runs within 7 days. Failed resources remain only in that DB.
"""
from __future__ import annotations

import json
from pathlib import Path
import re
from urllib.parse import urlsplit
from playwright.sync_api import expect
from image_catalog import button, cleanup, observe, read_detail, strings
from image_catalog_runner import run
from local_browser_fixture import ROOT
from platform_token_read_revoke import check, collection


def filter_menu(page, title):
    header = page.get_by_role("columnheader", name=re.compile("^" + re.escape(title) + r"(?:\s|$)"))
    header.get_by_role("button", name="filter", exact=True).click()
    menu = page.get_by_role("menu").filter(has=page.get_by_role("checkbox"))
    expect(menu).to_be_visible()
    return menu


def select_filter(page, title, values, options, locale):
    menu = filter_menu(page, title)
    expect(menu.get_by_role("menuitem")).to_have_text(options)
    for label in options:
        checkbox = menu.get_by_role("menuitem", name=label, exact=True).get_by_role("checkbox")
        checkbox.set_checked(label in values)
        expect(checkbox).to_be_checked(checked=label in values)
    button(page, "确定" if locale == "zh" else "OK").click()
    expect(menu).not_to_be_visible()


def reset_filter(page, title, options, locale):
    menu = filter_menu(page, title)
    button(page, "重置" if locale == "zh" else "Reset").click()
    for label in options:
        expect(menu.get_by_role("menuitem", name=label, exact=True).get_by_role("checkbox")).not_to_be_checked()
    button(page, "确定" if locale == "zh" else "OK").click()
    expect(menu).not_to_be_visible()


# Contract: gpuworks/image-catalog/series-filter-matrix
# Mainline: API准备各引擎/框架及启用/禁用的自定义系列和社区对照 → UI逐项筛选、同列OR/跨列AND、空交集与重置 → 对照独立GET精确行集合/总数，刷新保留筛选，重进恢复默认；只读筛选不改持久化，不声明镜像运行时。
def exercise_list_states(page, server, text, common, prefix, all_before, errors, failures, report):
    """10.1: loading, failure, retry and the recovered row set.

    The list reads the whole GET /images response, so the fetch is what decides
    every state. Each state is driven through the table's own refresh path rather
    than a reload, and the recovered set is compared against the independent GET.
    """
    list_path = "/api/v1/images"
    search = text["page"].get("search")
    states = report.setdefault("list_states_probe", {})
    table_rows = page.get_by_role("table").get_by_role("button", name=re.compile("^" + re.escape(prefix)))

    # Failure: a 503 on the list read has to surface as an error state, not as an
    # empty table, and the retry control has to be offered.
    def refuse(route):
        route.fulfill(status=503, content_type="application/json", body='{"message":"fixture unavailable"}')

    page.route(re.compile(re.escape(server.base + list_path) + r"(?:\?|$)"), refuse)
    try:
        with page.expect_response(lambda r: urlsplit(r.url).path == list_path) as failed:
            page.reload()
        check(failed.value.status == 503, f"forced list failure HTTP {failed.value.status}")
        # Scope the retry to the error Alert: a page-wide button lookup could match
        # some other Retry and would keep passing even if the Alert lost its action.
        # The error Alert is the one carrying the failed-to-load message; match on
        # the role only and then confirm its text, so a slow render is waited for
        # rather than filtered out.
        alert = page.get_by_role("alert").first
        expect(alert).to_be_visible()
        expect(alert).to_contain_text(text["page"]["loadFailed"])
        states["error_alert"] = True
        # antd inserts a space between the two glyphs of a two-character label
        # (autoInsertSpace), so a Chinese Retry renders as "重 试" while the label
        # itself is "重试". Comparing the exact text would reject a button that is
        # correct on screen, and it passed in en only because "Retry" is one word.
        # The accessible name is compared with that spacing removed, which still
        # fails if the Alert carries a different action or none at all.
        retry = alert.locator("button")
        expect(retry).to_have_count(1)
        check("".join(retry.inner_text().split()) == "".join(common["actions"]["retry"].split()),
              "the error Alert's only action is not the retry control")
        states["retry_offered"] = True
    finally:
        page.unroute(re.compile(re.escape(server.base + list_path) + r"(?:\?|$)"), refuse)

    # Retry: now that the route is back, the same control has to recover the set.
    with page.expect_response(lambda r: urlsplit(r.url).path == list_path and r.request.method == "GET") as again:
        # antd puts a space between the two glyphs of "重试" (autoInsertSpace), so no
        # name match finds it in zh: neither exact nor substring. The control is
        # located as the error Alert's only button and its text is checked against
        # the label with spacing removed, so a different action still fails.
        retry_button = page.get_by_role("alert").first.locator("button")
        expect(retry_button).to_have_count(1)
        check("".join(retry_button.inner_text().split()) == "".join(common["actions"]["retry"].split()),
              "the retried control is not the error Alert's retry action")
        retry_button.click()
    check(again.value.status == 200, f"retry HTTP {again.value.status}")
    check(collection(again.value) == all_before, "retry lost rows against the independent GET")
    # Recovery has to be judged on the source tab that actually holds the fixture:
    # a reload lands on the system catalog, so switch to custom first.
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    expect(table_rows.first).to_be_visible()
    states["recovered_rows"] = table_rows.count()

    # Loading: hold the read and confirm the table reports its busy state while the
    # response is outstanding, then release it.
    held = []

    def hold(route):
        held.append(route)

    page.route(re.compile(re.escape(server.base + list_path) + r"(?:\?|$)"), hold)
    try:
        page.get_by_role("button", name=common["dataTable"]["refresh"], exact=True).click()
        for _ in range(40):
            if held:
                break
            page.wait_for_timeout(50)
        check(held, "the list refresh was never intercepted")
        busy = page.locator(".ant-spin-spinning").count()
        states["loading_spin"] = busy
        check(busy > 0, "no loading state during a held list read")
        with page.expect_response(lambda r: urlsplit(r.url).path == list_path) as released:
            held[0].continue_()
    finally:
        page.unroute(re.compile(re.escape(server.base + list_path) + r"(?:\?|$)"), hold)
    check(released.value.status == 200, f"released read HTTP {released.value.status}")
    # The list keeps whichever source tab it was left on, and the fixture rows live
    # under custom, so go back there before counting them.
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    page.wait_for_timeout(500)

    # The failure counts as an expected outcome of this probe.
    failures[:] = [item for item in failures if item.get("status") != 503]
    errors[:] = [line for line in errors if "503" not in line]
    if search:
        states["search_present"] = True
    return states


def test_image_series_filters(page, server, artifacts, report, locale):
    text = strings(locale)
    common = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/common.json").read_text())
    prefix = "e2e-" + report["run_id"] + "-"
    engines = sorted(item["name"] for item in collection(page.request.get(server.base + "/api/v1/images/engines")))
    check(len(engines) >= 2, "fixture needs two real engines for OR/empty intersection")
    frameworks = ["cann", "cuda", "dtk", "hggc", "metal", "rocm"]
    fixture = []
    # Each engine has a CUDA row, each framework a row under engine A. Engine B
    # thus has CUDA but no CANN: both individual facets match, their AND is empty.
    combinations = [(engine, "cuda") for engine in engines] + [(engines[0], f) for f in frameworks if f != "cuda"]
    for index, (engine, framework) in enumerate(combinations):
        fixture.append({"name": prefix + str(index), "engine": engine, "framework": framework,
            "source": "custom", "enabled": index % 2 == 0, "versions": [{
                "image_ref": f"registry.example/{prefix}runner:{index}", "framework_version": "1.0",
                "engine_version": "1.0", "arch": "multi", "source": "custom"}]})
    fixture.append({"name": prefix + "community", "engine": engines[0], "framework": "cuda", "source": "community",
        "versions": [{"image_ref": f"registry.example/{prefix}community:v1", "framework_version": "1.0",
            "engine_version": "1.0", "arch": "amd64", "source": "community"}]})
    seeded = collection(page.request.post(server.base + "/api/v1/images/import", data={"series": fixture}))
    check(len(seeded) == len(fixture), "fixture series count differs")
    all_before = collection(page.request.get(server.base + "/api/v1/images"))
    owned = [row for row in all_before if row["name"].startswith(prefix)]
    check(len(owned) == len(fixture), "independent GET lost fixture rows")
    expected_fixture = {row["name"]: row for row in fixture}
    for row in owned:
        source = expected_fixture[row["name"]]
        for key in ["engine", "framework", "source", "enabled"]:
            check(row[key] == source.get(key, True), f"fixture persistence differs for {key}")
    custom = [row for row in owned if row["source"] == "custom"]
    check(len(custom) <= 20, "fixture outgrew the unpaginated filter matrix; extend explicit page collection")
    details_before = {row["id"]: read_detail(page, server.base, row["id"]) for row in owned}
    errors, failures, mutations = observe(page)
    reads = []
    page.on("response", lambda r: reads.append(r) if urlsplit(r.url).path == "/api/v1/images" and r.request.method == "GET" else None)
    # reload() discards the previous document, and Playwright can no longer fetch a
    # response body that belonged to it ("No resource with given identifier"). Reading
    # the body inside the expect_response block keeps it while the page that produced
    # it is still current, so the comparison is made on a body that was actually read
    # rather than on a handle that has gone stale.
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images" and r.request.method == "GET") as initial:
        page.reload()
        initial_items = collection(initial.value)
    check(initial_items == all_before, "browser list differs from independent authority")
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    expect(page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True)).to_have_attribute("aria-selected", "true")
    rows = page.get_by_role("table").get_by_role("button", name=re.compile("^" + re.escape(prefix)))

    def assert_rows(expected, dimension):
        expected_names = sorted(row["name"] for row in expected)
        expect(rows).to_have_count(len(expected_names))
        actual = sorted(rows.all_text_contents())
        check(actual == expected_names, f"{dimension}: included or excluded rows differ")
        # AntD hides pagination when the filtered list is empty; explicit empty
        # state plus zero row identities are the zero-result count evidence.
        if expected:
            total = common["resourceTable"]["total"].replace("{{count}}", str(len(expected)))
            expect(page.get_by_text(total, exact=True)).to_be_visible()
        else:
            expect(page.get_by_text(common["dataTable"]["empty"], exact=True)).to_be_visible()
        report["evidence"].append({"layer": "ui", "dimension": dimension, "ids": sorted(r["id"] for r in expected), "names": actual, "total": len(expected)})

    assert_rows(custom, "default-custom")
    titles = text["seriesTable"]["column"]
    status = [text["common"]["enabled"], text["common"]["disabled"]]
    report["active_evidence_layer"] = "ui"
    # Every actually visible option is matched to the owned authoritative rows,
    # selected separately and reset; no representative-value shortcut.
    for field, options in [("engine", engines), ("framework", frameworks), ("enabled", status)]:
        title = titles["status" if field == "enabled" else field]
        for option in options:
            select_filter(page, title, [option], options, locale)
            expected = [r for r in custom if (r["enabled"] == (option == status[0]) if field == "enabled" else r[field] == option)]
            check(expected, f"fixture option {field}/{option} must independently match a row")
            assert_rows(expected, f"{field}/option:{option}")
            reset_filter(page, title, options, locale)
            assert_rows(custom, f"{field}/reset:{option}")
    select_filter(page, titles["engine"], engines[:2], engines, locale)
    assert_rows([r for r in custom if r["engine"] in engines[:2]], "engine/multi-option-OR")
    select_filter(page, titles["framework"], ["cuda"], frameworks, locale)
    assert_rows([r for r in custom if r["engine"] in engines[:2] and r["framework"] == "cuda"], "engine-framework/AND")
    select_filter(page, titles["status"], [status[0]], status, locale)
    expected = [r for r in custom if r["engine"] in engines[:2] and r["framework"] == "cuda" and r["enabled"]]
    assert_rows(expected, "engine-framework-enabled/AND")
    # A real query refresh must preserve the current local filters and read the
    # same authoritative rows, with no filter parameters invented on the wire.
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images" and r.request.method == "GET") as refreshed:
        page.get_by_role("button", name=common["dataTable"]["refresh"], exact=True).click()
    check(collection(refreshed.value) == all_before, "refresh altered authority")
    assert_rows(expected, "manual-refresh/filters-preserved")
    reset_filter(page, titles["status"], status, locale)
    select_filter(page, titles["engine"], [engines[1]], engines, locale)
    select_filter(page, titles["framework"], ["cann"], frameworks, locale)
    assert_rows([], "engine-framework/individually-nonempty-empty-intersection")
    reset_filter(page, titles["framework"], frameworks, locale)
    assert_rows([r for r in custom if r["engine"] == engines[1]], "empty-intersection/recovery")
    reset_filter(page, titles["engine"], engines, locale)
    assert_rows(custom, "all-filters/reset")
    page.get_by_role("tab", name=text["page"]["tab"]["community"], exact=True).click()
    assert_rows([r for r in owned if r["source"] == "community"], "source/community-excludes-custom")
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    assert_rows(custom, "source/custom-excludes-community")
    # Re-entering the page resets ephemeral filters and the default source tab.
    select_filter(page, titles["framework"], ["cann"], frameworks, locale)
    page.reload()
    expect(page.get_by_role("tab", name=text["page"]["tab"]["official"], exact=True)).to_have_attribute("aria-selected", "true")
    expect(rows).to_have_count(0)
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    assert_rows(custom, "reload/local-filters-reset")
    for response in reads:
        check(response.status == 200 and urlsplit(response.url).query == "" and response.request.post_data is None,
              "client-side filters must read unmodified GET /images")
        check(collection(response) == all_before, "unexpected filtered server response")
    check(not mutations, "read-only list filters caused a mutation")
    report["evidence"].append({"layer": "request", "method": "GET", "path": "/api/v1/images", "query": "", "status": 200,
        "browser_reads": len(reads), "response_ids": sorted(r["id"] for r in all_before), "filtering": "client-side", "mutations": mutations})
    for row in owned:
        check(read_detail(page, server.base, row["id"]) == details_before[row["id"]], "filter changed persisted metadata")
    report["evidence"].append({"layer": "persistence", "unchanged_series_ids": sorted(details_before), "reload": True})
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page horizontal overflow")
    check(not errors and not failures, "browser errors or unexpected HTTP failures")
    exercise_list_states(page, server, text, common, prefix, all_before, errors, failures, report)
    report["list_states"] = report.pop("list_states_probe")
    report["evidence"].append({"layer": "ui", "list_states": report["list_states"]})
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    from metadata_table_checks import exercise as table_pilot
    table_pilot(page, server, artifacts, report, locale, text, engines, frameworks, select_filter, reset_filter)
    report["active_evidence_layer"] = "cleanup"
    for row in reversed(owned):
        cleanup(page, server.base, row["id"], report)


if __name__ == "__main__":
    raise SystemExit(run({"filters": test_image_series_filters}, entry_path=Path(__file__), metadata_offline=True, product_sources=[
        "test/e2e/metadata_table_checks.py", "test/e2e/metadata_table_fixture.py",
        "test/e2e/image_catalog.py", "ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImageSeriesTable.tsx",
        "ui/src/pages/images/ImagesPageOverview.tsx", "ui/src/components/resourceTable/ResourceTable.tsx",
        "ui/src/components/resourceTable/columnPrefs.ts", "ui/src/components/resourceTable/ColumnSettingsDrawer.tsx"]))
