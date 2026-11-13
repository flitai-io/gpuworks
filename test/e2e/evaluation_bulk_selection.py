#!/usr/bin/env python3
"""Evaluation list batch-selection matrix measured through the real browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-benchmark-selection
After building the server: python3 test/e2e/evaluation_bulk_selection.py --locale en
Use --locale zh for Chinese; optional --server-bin and --artifact-dir follow the
shared runner. Set NO_PROXY/no_proxy=127.0.0.1,localhost when using a proxy.

Fixtures seed twenty-one runs through the real POST /api/v1/benchmarks route: the
benchmark executor really runs them against an auditable local OpenAI fixture, and
five of them are pointed at a tokenizer path that does not exist so they end in the
failed state. The list, the compare page, the paging, the column filters and the
delete route are the product's own; every claim below is read back from the DOM or
from an independent GET /api/v1/benchmarks. No GPU, agent, container or engine is
claimed. Artifacts and failure screenshots stay in the ignored secrets/e2e/<runId>
and are removed within seven days; failed runs remain only in that private DB.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import traceback
from urllib.parse import urlsplit, unquote
import uuid

from playwright.sync_api import expect, sync_playwright
from benchmark_fixture import BenchmarkEndpoint
from benchmark_support import strings, format_text, finish_environment, close_browser
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, ADMIN_PASSWORD
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/benchmark/batch-selection-matrix"
CASE = "评测 / 列表批量选择矩阵 / 单行多行取消与全选作用域、五条上限、翻页过滤刷新后的集合"
BENCHMARKS = "/api/v1/benchmarks"
# Ten rows per page against twenty-one runs makes "the current page" and "every
# filtered result" different row sets, which is exactly what 11.5 asks for.
PAGE_SIZE = 10
DONE_RUNS = 16
FAILED_RUNS = 5


def seed_body(fixture, name, tokenizer):
    return {"name": name, "target_kind": "external", "base_url": fixture.base,
            "api_key": fixture.api_key, "model": fixture.model, "load_origin": "server",
            "dataset": {"kind": "random", "input_tokens": 16, "output_tokens": 8,
                        "ignore_eos": True},
            "mode": "grid",
            "search": {"concurrency": [1], "isl": [16], "osl": [8], "warmup_requests": 0,
                       "requests_per_point_multiplier": 1},
            "keep_detail": False, "tokenizer_source": "upload", "tokenizer_ref": tokenizer,
            # The seeded runs deliberately overlap one endpoint; admission is not
            # what this case measures. The executor still really runs each one.
            "allow_conflict": True}


def seed_runs(context, server, fixture, artifacts, report):
    """Create the row set the matrix needs and wait for the real executor.

    The tokenizer is imported through the route the wizard's upload control uses,
    so a generated token-length workload runs offline instead of downloading
    somebody else's repository. The five failed runs get a local path that does
    not exist: the run is still accepted with 202 and then ends in failed, which
    is what a mixed batch needs.
    """
    vocabulary = {character: index for index, character in enumerate(
        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .,!?-")}
    vocabulary.update({word: 100 + index for index, word in enumerate(
        ("hello", "world", "the", "test", "data", "model", "run", "e2e"))})
    raw = json.dumps({"model": {"type": "BPE", "vocab": vocabulary}},
                     ensure_ascii=False).encode()
    imported = context.request.post(server.base + BENCHMARKS + "/tokenizers/import", data=raw,
                                    headers={"Content-Type": "application/json"})
    check(imported.status == 200, f"tokenizer import failed: {imported.status}")
    tokenizer = imported.json()["tokenizer_ref"]
    check(str(tokenizer).startswith(str(artifacts.private)),
          f"the imported tokenizer is not a server-local path: {tokenizer}")
    missing = str(artifacts.directory / "absent-tokenizer" / "tokenizer.json")
    done, failed = [], []
    for index in range(DONE_RUNS):
        name = f"sel-{report['run_id']}-ok-{index:02d}"
        response = context.request.post(server.base + BENCHMARKS,
                                        data=json.dumps(seed_body(fixture, name, tokenizer)))
        check(response.status == 202, f"seeding {name} failed: {response.status}")
        done.append(response.json()["id"])
    for index in range(FAILED_RUNS):
        name = f"sel-{report['run_id']}-bad-{index:02d}"
        response = context.request.post(server.base + BENCHMARKS,
                                        data=json.dumps(seed_body(fixture, name, missing)))
        check(response.status == 202, f"seeding {name} failed: {response.status}")
        failed.append(response.json()["id"])
    deadline = time.time() + 180
    states = {}
    while time.time() < deadline:
        listing = context.request.get(server.base + BENCHMARKS).json()
        states = {}
        for item in listing["items"]:
            states[item["state"]] = states.get(item["state"], 0) + 1
        if states.get("done", 0) >= DONE_RUNS and states.get("failed", 0) >= FAILED_RUNS:
            break
        time.sleep(1)
    check(states.get("done", 0) >= DONE_RUNS and states.get("failed", 0) >= FAILED_RUNS,
          f"seeded runs did not settle: {states}")
    listing = context.request.get(server.base + BENCHMARKS).json()
    by_id = {item["id"]: item for item in listing["items"]}
    check(all(by_id[run]["state"] == "done" for run in done), "a seeded run did not complete")
    check(all(by_id[run]["state"] == "failed" for run in failed),
          "a seeded failure did not end in failed")
    order = [item["id"] for item in listing["items"]]
    check(order == sorted(order, reverse=True),
          "the list is not newest-first, so a page is not a known row set")
    check(len(order) >= DONE_RUNS + FAILED_RUNS, f"seeded row set is short: {len(order)}")
    names = {run: by_id[run]["name"] for run in done + failed}
    report["seeded"] = {"done": done, "failed": failed, "total": len(order),
                        "states": states, "names": names, "tokenizer_ref": tokenizer}
    return done, failed


# Contract: gpuworks/benchmark/batch-selection-matrix
# Mainline: 通过真实 POST /api/v1/benchmarks 造 21 条运行(16 成功 5 失败) → 列表设 10 条/页 → 单行/多行/取消/表头全选/五条上限/翻页/列过滤/刷新逐项断言 → 对比按钮计数与 ?ids= 作为独立读回。
def test_batch_selection(page, context, server, artifacts, report, locale, fixture):
    ui, dictionary = strings(locale)
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    status_label = {key: value["label"] for key, value in dictionary["statuses"].items()}
    done_label = status_label["done"]
    failed_label = status_label["failed"]
    total_template = common["resourceTable"]["total"]
    compare_template = ui["list"]["compare"]
    title = ui["list"]["title"]
    errors, console_errors, bad_responses, allowlisted = [], [], [], []
    expected_http = set()
    authenticated = False
    page.on("pageerror", lambda error: errors.append(str(error)))

    def console_seen(message):
        if message.type != "error":
            return
        if not authenticated and "401" in message.text:
            return
        path = urlsplit((message.location or {}).get("url") or "").path
        if any(entry[1] == path for entry in expected_http):
            allowlisted.append({"text": message.text, "path": path})
            return
        console_errors.append(message.text)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if (response.request.method, path, response.status) in expected_http:
                return
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": path,
                                  "status": response.status})

    page.on("console", console_seen)
    page.on("response", response_seen)

    page.goto(server.base + "/evaluations")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill(ADMIN_PASSWORD)
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=title, exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "evaluations-page-loaded"

    done_runs, failed_runs = seed_runs(context, server, fixture, artifacts, report)
    report["last_step"] = "runs-seeded"

    # A stored page size of ten spans the seeded rows over three pages.
    page.evaluate("size => localStorage.setItem('gpuworks.table.evaluations.v1', "
                  "JSON.stringify({pageSize: size}))", PAGE_SIZE)
    page.reload()
    expect(page.get_by_role("heading", name=title, exact=True)).to_be_visible()
    rows = page.locator('[data-slot="table-body"] [data-row-key]')
    expect(rows.first).to_be_visible(timeout=25000)
    total_rows = report["seeded"]["total"]

    def total_text():
        """The footer's total, which DataTablePagination rewrites as the count changes."""
        return page.get_by_text(re.compile(r"^\d+ (items?|条)$")).first

    expect(total_text()).to_have_text(format_text(total_template, count=total_rows))
    prefix, suffix = compare_template.split("{{count}}")
    compare = page.locator("button", has_text=re.compile(
        r"^" + re.escape(prefix) + r"\d+" + re.escape(suffix) + "$"))
    expect(compare.first).to_be_visible()
    page_size_trigger = page.get_by_role(
        "combobox", name=common["resourceTable"]["rowsPerPage"], exact=True)

    def compare_count():
        """The count the product itself shows for the current selection."""
        text = compare.first.inner_text()
        match = re.search(r"(\d+)/5", text)
        check(match is not None, f"the compare control shows no count: {text}")
        return int(match.group(1))

    def row_keys():
        return [int(key) for key in rows.evaluate_all("els => els.map(e => e.dataset.rowKey)")]

    def box(run_id):
        return page.locator('[data-slot="table-body"] [data-row-key="%s"]' % run_id
                            ).get_by_role("checkbox")

    def checked_rows():
        return [int(key) for key in rows.evaluate_all(
            "els => els.filter(e => {const c=e.querySelector('[data-slot=\"checkbox\"]');"
            "if (!c) return false; const v=c.getAttribute('aria-checked');"
            "return v==='true' || c.hasAttribute('data-checked');}).map(e => e.dataset.rowKey)")]

    def header():
        return page.locator('[data-slot="table-header"]').get_by_role("checkbox").first

    def emitted_ids():
        query = unquote(urlsplit(page.url).query)
        check("ids=" in query, f"the comparison URL carries no ids: {page.url}")
        return [int(value) for value in query.split("ids=")[1].split(",")]

    # The list is a ResourceTable whose column filters are ui-kit/ColumnFilter.tsx: the
    # trigger is a button named after the column ("Filter by <title>"), the options are
    # checkboxes labelled with the values' own text, and a tick applies at once - there is
    # no OK button and the popup stays open until the clear control or an outside click.
    filter_titles = [ui["list"]["column"]["mode"], ui["list"]["column"]["status"]]

    def filter_menu(column):
        title = filter_titles[column]
        trigger = page.get_by_role(
            "button",
            name=common["resourceTable"]["filterByColumn"].replace("{{name}}", title),
            exact=True)
        expect(trigger).to_have_count(1)
        trigger.click()
        menu = page.locator('[data-slot="popover-content"]').first
        expect(menu).to_be_visible()
        return menu

    def set_filter(column, selected):
        """Tick exactly the wanted values in the column's own filter popup."""
        menu = filter_menu(column)
        for label in status_label.values():
            option = menu.get_by_role("checkbox", name=label, exact=True)
            expect(option).to_have_count(1)
            want = label in selected
            checked = option.get_attribute("aria-checked") == "true"
            if want != checked:
                option.click()
            page.wait_for_timeout(150)
        page.keyboard.press("Escape")
        page.wait_for_timeout(600)

    page1 = row_keys()
    check(len(page1) == PAGE_SIZE, f"the first page holds {len(page1)} rows, not {PAGE_SIZE}")
    check(page_size_trigger.inner_text().strip() == str(PAGE_SIZE),
          "the stored page size was not applied")
    check(compare.first.inner_text() == format_text(compare_template, count=0),
          "an empty selection does not read as zero")
    expect(compare.first).to_be_disabled()

    # 11.5 single row: one press selects exactly that row.
    box(page1[0]).check()
    expect(box(page1[0])).to_be_checked()
    check(checked_rows() == [page1[0]], f"one press selected {checked_rows()}")
    check(compare_count() == 1, "the compare count does not follow one selected row")
    expect(compare.first).to_be_disabled()
    report["single_row"] = {"selected": checked_rows(), "compare": compare.first.inner_text(),
                            "compare_disabled": compare.first.is_disabled()}

    # 11.5 multi row: the second press adds instead of replacing.
    box(page1[1]).check()
    check(checked_rows() == [page1[0], page1[1]], f"two presses selected {checked_rows()}")
    check(compare_count() == 2, "the compare count does not follow two selected rows")
    expect(compare.first).to_be_enabled()
    report["multi_row"] = {"selected": checked_rows(), "compare": compare.first.inner_text(),
                           "compare_enabled": compare.first.is_enabled()}

    # 11.5 deselect: unchecking removes exactly that row.
    box(page1[1]).uncheck()
    check(checked_rows() == [page1[0]], f"deselecting left {checked_rows()}")
    check(compare_count() == 1, "the compare count does not follow a deselection")
    expect(compare.first).to_be_disabled()
    report["deselect_row"] = {"selected": checked_rows(), "compare": compare.first.inner_text()}
    box(page1[0]).uncheck()
    check(checked_rows() == [], "the selection did not come back to empty")
    check(compare.first.inner_text() == format_text(compare_template, count=0),
          "clearing the last row does not read as zero")

    # 11.5 select all: the measured scope answer. The header box is pressed while
    # the list holds twenty-one rows and the page shows ten of them.
    header().check()
    page.wait_for_timeout(400)
    page1_selected = checked_rows()
    scope = {"rows_in_list": total_rows, "rows_on_page": len(page1),
             "rows_selected": len(page1_selected), "selected_keys": page1_selected,
             "page_keys": page1, "compare": compare.first.inner_text(),
             "compare_count": compare_count()}
    check(page1_selected == page1,
          f"select all selected {page1_selected} instead of the {len(page1)} rows of this page")
    check(scope["compare_count"] == len(page1),
          f"select all reports {scope['compare_count']} rows while the page holds {len(page1)}")
    check(len(page1_selected) != total_rows,
          f"select all covered all {total_rows} rows, so the page-scope claim is wrong")
    scope["scope"] = "current page only"
    scope["exceeds_five_cap"] = scope["compare_count"] > 5
    if scope["exceeds_five_cap"]:
        # The header is not clamped to the five-run comparison bound: pressing it
        # really selects ten rows, and Compare must refuse instead of truncating.
        expect(compare.first).to_be_disabled()
        scope["compare_disabled_over_five"] = compare.first.is_disabled()
        check(scope["compare_disabled_over_five"], "compare accepts a selection above five")
    header().click()
    page.wait_for_timeout(400)
    check(checked_rows() == [], "a second header press did not clear the page")
    check(compare_count() == 0, "a second header press left a count behind")
    scope["second_press_cleared"] = True
    report["select_all_scope"] = scope

    # 11.5/11.6 cap: the fifth row is selectable, the sixth is not, and pressing
    # the disabled sixth row adds nothing.
    for run_id in page1:
        if len(checked_rows()) >= 5:
            break
        if not box(run_id).is_disabled():
            box(run_id).check()
            page.wait_for_timeout(120)
    check(checked_rows() == page1[:5], f"the first five rows are not the selection: {checked_rows()}")
    check(compare_count() == 5, "the compare count does not reach five")
    expect(compare.first).to_be_enabled()
    sixth = page1[5]
    expect(box(sixth)).to_be_disabled()
    enabled_at_cap = [run_id for run_id in page1 if not box(run_id).is_disabled()]
    check(enabled_at_cap == page1[:5],
          f"rows beyond the cap stay selectable: {enabled_at_cap}")
    box(sixth).click(force=True)
    page.wait_for_timeout(300)
    check(checked_rows() == page1[:5],
          f"pressing a disabled row changed the selection: {checked_rows()}")
    check(compare_count() == 5, "pressing a disabled row changed the compare count")
    cap = {"selected": checked_rows(), "compare": compare.first.inner_text(),
           "sixth_disabled": True, "enabled_rows": enabled_at_cap,
           "forced_press_ignored": True}

    # Deselecting one row restores selectability, and the released slot takes the
    # row that was refused a moment ago.
    box(page1[0]).uncheck()
    page.wait_for_timeout(300)
    check(box(sixth).is_disabled() is False, "the sixth row stays disabled after a deselection")
    box(sixth).check()
    page.wait_for_timeout(300)
    check(checked_rows() == page1[1:6], f"the released slot took {checked_rows()}")
    check(compare_count() == 5, "the selection is not full again")
    cap["restored"] = {"selected": checked_rows(), "sixth_enabled": True,
                       "compare": compare.first.inner_text()}
    report["cap"] = cap

    # 11.7 paging: the selection survives a page change, hidden rows stay named,
    # and a row on another page can be added. Two rows are left selected so a slot
    # is free.
    for run_id in page1[1:4]:
        box(run_id).uncheck()
        page.wait_for_timeout(120)
    kept = checked_rows()
    check(kept == page1[4:6], f"the paging setup left {kept}")
    page.get_by_role("button", name=common["resourceTable"]["nextPage"], exact=True).click()
    page.wait_for_timeout(500)
    page2 = row_keys()
    check(set(page2) & set(page1) == set(), "page two repeats rows of page one")
    check(checked_rows() == [], "rows of page one stay checked on page two")
    check(compare_count() == len(kept), "paging cleared the selection")
    added = page2[0]
    check(box(added).is_disabled() is False, "a page-two row is unselectable with a free slot")
    box(added).check()
    page.wait_for_timeout(300)
    check(compare_count() == len(kept) + 1, "a page-two row did not join the selection")
    page.get_by_role("button", name=common["resourceTable"]["previousPage"], exact=True).click()
    page.wait_for_timeout(500)
    check(row_keys() == page1, "returning to page one did not restore its rows")
    back_selected = checked_rows()
    check(back_selected == kept, f"returning to page one shows {back_selected}, not {kept}")
    check(compare_count() == len(kept) + 1, "the cross-page selection lost a row")
    report["paging"] = {"page1": page1, "page2": page2, "kept": kept,
                        "added_on_page2": added, "back_on_page1": back_selected,
                        "compare": compare.first.inner_text()}
    compare.first.click()
    page.wait_for_url(re.compile(r".*/evaluations/compare\?.*"))
    cross_ids = emitted_ids()
    check(set(cross_ids) == set(kept) | {added},
          f"compare emitted {cross_ids} for the selection {kept} + {added}")
    report["paging"]["emitted_ids"] = cross_ids
    page.go_back()
    page.wait_for_timeout(700)
    check(compare_count() == 0, "coming back from a comparison kept a stale selection")

    # 11.5/11.7 filter: select all on a filtered list is still page-scoped, and a
    # selection that the filter hides is retained and still named by Compare.
    set_filter(1, [done_label])
    filtered_total = DONE_RUNS
    expect(total_text()).to_have_text(format_text(total_template, count=filtered_total))
    filtered_page = row_keys()
    check(len(filtered_page) == PAGE_SIZE,
          f"the filtered page holds {len(filtered_page)} rows, not {PAGE_SIZE}")
    header().check()
    page.wait_for_timeout(400)
    filter_scope = {"filter": done_label, "filtered_total": filtered_total,
                    "filtered_page": filtered_page, "selected": checked_rows(),
                    "compare": compare.first.inner_text()}
    check(checked_rows() == filtered_page,
          f"select all under a filter selected {checked_rows()} instead of {filtered_page}")
    check(filter_scope["compare"] == format_text(compare_template, count=len(filtered_page)),
          f"select all under a filter reports {filter_scope['compare']}")
    check(len(checked_rows()) != filtered_total,
          f"select all covered all {filtered_total} filtered rows, so the scope claim is wrong")
    filter_scope["scope"] = "current page of the filtered result"
    header().click()
    page.wait_for_timeout(400)
    check(checked_rows() == [], "clearing the filtered selection failed")
    report["filter_select_all"] = filter_scope

    for run_id in filtered_page[:2]:
        box(run_id).check()
        page.wait_for_timeout(150)
    check(compare_count() == 2, "the pre-filter selection is not two rows")
    set_filter(1, [failed_label])
    expect(total_text()).to_have_text(format_text(total_template, count=FAILED_RUNS))
    narrowed = row_keys()
    check(len(narrowed) == FAILED_RUNS, f"the narrowed filter shows {len(narrowed)} rows")
    check(not set(narrowed) & set(filtered_page[:2]),
          "the narrowed filter still shows the selected rows, so retention is untested")
    check(checked_rows() == [], "a filtered-out row is still rendered as checked")
    check(compare_count() == 2, "the filter cleared the selection")
    report["filter"] = {"before": done_label, "after": failed_label,
                        "visible": narrowed, "still_selected": compare_count(),
                        "hidden_selected": filtered_page[:2],
                        "compare": compare.first.inner_text()}

    # 11.6 mixed set: Compare over a set the filter hides must name those rows
    # rather than act on what happens to be visible.
    compare.first.click()
    page.wait_for_url(re.compile(r".*/evaluations/compare\?.*"))
    hidden_ids = emitted_ids()
    check(set(hidden_ids) == set(filtered_page[:2]),
          f"compare emitted {hidden_ids} instead of the hidden selection {filtered_page[:2]}")
    for run_id in filtered_page[:2]:
        expect(page.locator(".ant-card-head-title").filter(
            has_text=report["seeded"]["names"][run_id])).to_have_count(1)
    check(page.locator(".ant-alert-error").count() == 0,
          "a readable hidden selection reported a load failure")
    report["compare_hidden_selection"] = {"ids": hidden_ids,
                                          "visible": narrowed, "url": page.url}
    page.go_back()
    page.wait_for_timeout(900)
    expect(total_text()).to_have_text(format_text(total_template, count=total_rows))
    check(compare_count() == 0, "coming back from the filtered comparison kept a selection")

    # 11.6 unactionable row: a selected run deleted behind the browser's back stays
    # in the selection set, so Compare must report the real reason instead of
    # quietly comparing the survivors.
    victim = failed_runs[0]
    survivor = done_runs[-1]
    check({victim, survivor} <= set(row_keys()), "the ghost rows are not on the first page")
    box(survivor).check()
    box(victim).check()
    page.wait_for_timeout(300)
    check(compare_count() == 2, "the ghost scenario did not select two rows")
    expected_http.add(("GET", f"{BENCHMARKS}/{victim}", 404))
    deleted = context.request.delete(server.base + f"{BENCHMARKS}/{victim}")
    check(deleted.status == 204, f"deleting the selected run failed: {deleted.status}")
    gone = context.request.get(server.base + f"{BENCHMARKS}/{victim}")
    check(gone.status == 404, "the deleted run is still readable")
    check(compare_count() == 2, "the deleted run silently left the selection")
    compare.first.click()
    page.wait_for_url(re.compile(r".*/evaluations/compare\?.*"))
    alert = page.locator(".ant-alert-error")
    expect(alert).to_be_visible(timeout=15000)
    alert_text = alert.first.inner_text()
    check(alert_text.splitlines()[0].strip() == ui["compare"]["loadFailed"],
          f"the comparison does not name its failure: {alert_text}")
    check("not found" in alert_text, "the comparison does not report the server's own reason")
    back = page.get_by_role("button", name=ui["common"]["back"])
    expect(back).to_be_visible()
    report["ghost_resource"] = {"deleted_run": victim, "delete_status": deleted.status,
                                "readback_after_delete": gone.status,
                                "alert": ui["compare"]["loadFailed"],
                                "alert_text": alert_text.replace("\n", " "),
                                "action": ui["common"]["back"]}
    back.click()
    page.wait_for_url(re.compile(r".*/evaluations$"))
    page.wait_for_timeout(600)
    check(compare_count() == 0, "the failed comparison left the selection behind")

    # 11.6 no-permission facet: the same rows through a read-scope token. The rows
    # are readable and the batch mutation the list offers is refused with the
    # product's own read-only code.
    minted = context.request.post(server.base + "/api/v1/platform-tokens",
                                  data=json.dumps({"name": f"sel-{report['run_id']}",
                                                   "scope": "read"}))
    check(minted.status == 201, f"minting a read token failed: {minted.status}")
    created = minted.json()
    artifacts.remember(created["token"])
    check(created["item"]["scope"] == "read", "the minted token is not read-scoped")
    read_headers = {"Authorization": f"Bearer {created['token']}"}
    readable = context.request.get(server.base + f"{BENCHMARKS}/{survivor}",
                                   headers=read_headers)
    check(readable.status == 200, "a read token cannot read the selected run")
    refused = context.request.delete(server.base + f"{BENCHMARKS}/{survivor}",
                                     headers=read_headers)
    check(refused.status == 403, f"a read token deleted a run: {refused.status}")
    refused_body = refused.json()
    check(refused_body.get("code") == "PLATFORM_TOKEN_READ_ONLY",
          f"the refusal is not the read-only one: {refused_body}")
    alive = context.request.get(server.base + f"{BENCHMARKS}/{survivor}")
    check(alive.status == 200, "the refused delete removed the run anyway")
    report["no_permission"] = {"scope": created["item"]["scope"],
                               "read_status": readable.status,
                               "batch_status": refused.status,
                               "code": refused_body.get("code"),
                               "run_survived": alive.status}

    # 11.7 refresh: the selection is page state, so a reload must clear it instead
    # of resurrecting rows the new page cannot see.
    box(survivor).check()
    page.wait_for_timeout(300)
    check(compare_count() == 1, "the reload scenario did not select a row")
    page.reload()
    expect(page.get_by_role("heading", name=title, exact=True)).to_be_visible()
    expect(rows.first).to_be_visible(timeout=25000)
    page.wait_for_timeout(700)
    check(compare_count() == 0, "a reload kept the old selection")
    check(checked_rows() == [], "a reload restored checked rows")
    check(page_size_trigger.inner_text().strip() == str(PAGE_SIZE),
          "the page size did not survive the reload")
    report["reload"] = {"compare": compare.first.inner_text(), "page_size": PAGE_SIZE}

    unexpected = [item for item in bad_responses
                  if (item["method"], item["path"], item["status"]) not in expected_http]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    check(not errors and not console_errors, f"browser errors: {errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "evaluation page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"
    created_runs = len(report["seeded"]["done"]) + len(report["seeded"]["failed"])
    report["browser_errors"] = {
        "page": errors, "console": console_errors, "http": bad_responses,
        "allowlisted_console": allowlisted,
        "mutations": [{"method": "POST", "path": BENCHMARKS + "/tokenizers/import"},
                      {"method": "POST", "path": BENCHMARKS, "count": created_runs},
                      {"method": "DELETE", "path": f"{BENCHMARKS}/{victim}"},
                      {"method": "POST", "path": "/api/v1/platform-tokens"}]}

    independent = context.request.get(server.base + BENCHMARKS).json()
    check(independent["total"] == total_rows - 1,
          f"the independent readback reports {independent['total']} rows")
    report["evidence"].extend([
        {"layer": "ui", "page": "/evaluations", "table": "evaluations",
         "select_all_scope": report["select_all_scope"], "single_row": report["single_row"],
         "multi_row": report["multi_row"], "deselect_row": report["deselect_row"],
         "cap": report["cap"], "paging": report["paging"], "filter": report["filter"],
         "filter_select_all": report["filter_select_all"], "reload": report["reload"]},
        {"layer": "request", "seeded_creates": created_runs,
         "compare_ids": report["paging"]["emitted_ids"],
         "hidden_selection_ids": report["compare_hidden_selection"]["ids"],
         "read_token": report["no_permission"],
         "ghost_delete": {"path": f"{BENCHMARKS}/{victim}", "status": deleted.status}},
        {"layer": "persistence", "independent_total": independent["total"],
         "seeded_states": report["seeded"]["states"],
         "ghost_readback": report["ghost_resource"]["readback_after_delete"]},
        {"layer": "execution", "runs_completed": DONE_RUNS, "runs_failed": FAILED_RUNS,
         "endpoint_requests": len(fixture.snapshot()["requests"])},
        {"layer": "cleanup", "server_stopped": True, "fixture_stopped": True,
         "deleted_runs": 1, "rows_left": independent["total"]},
    ])


CASES = {
    "selection": {
        "contract": CONTRACT, "case": CASE,
        "manifest": "benchmark-external-selection.json",
        "test": test_batch_selection,
        "not_covered": ["wide layouts and small viewports",
                        "keyboard multi-select with modifier keys",
                        "the comparison charts' own metric values",
                        "deleting many rows in one batch",
                        "two operators selecting the same rows concurrently",
                        "the other locale (each run covers one --locale)"],
    },
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--case", choices=sorted(CASES), default="selection")
    args = parser.parse_args()
    selected = CASES[args.case]
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage" / selected["manifest"]
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == selected["contract"]
          and manifest["test_function"] == selected["test"].__name__,
          "coverage manifest does not bind this case")
    report = {"contract": selected["contract"], "case": selected["case"],
              "run_id": run_id, "status": "NOT_RUN",
              "source_sha": subprocess.check_output(
                  ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
              "working_tree": "dirty" if subprocess.check_output(
                  ["git", "status", "--porcelain"], cwd=ROOT) else "clean",
              "lane": "local-real-server-sqlite-openai-fixture",
              "identity": "admin browser / benchmark executor on the server",
              "locale": args.locale, "timezone": "UTC",
              "viewport": {"width": 1440, "height": 900},
              "selection": [selected["contract"]], "retries": 0, "evidence": [],
              "coverage_manifest_sha256": digest(manifest_path),
              "test_sha256": digest(Path(__file__)),
              "fixture_sha256": digest(ROOT / "test/e2e/benchmark_fixture.py"),
              "support_sha256": digest(ROOT / "test/e2e/benchmark_support.py"),
              "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
              "execution": "real streamed /v1/chat/completions against an audited local "
                           "OpenAI fixture; no GPU claim",
              "runtime": "N/A: external OpenAI-compatible fixture; no agent, container, "
                         "engine or GPU",
              "not_covered": selected["not_covered"],
              "command": ["python3", "test/e2e/evaluation_bulk_selection.py", *sys.argv[1:]]}
    fixture = None
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        fixture = BenchmarkEndpoint().start()
        artifacts.remember(fixture.api_key)
        report["fixture"] = {"base": fixture.base, "model": fixture.model}
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                                          locale="zh-CN" if args.locale == "zh" else "en-US",
                                          timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(15000)
            try:
                selected["test"](page, context, server, artifacts, report,
                                 args.locale, fixture)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                close_browser(context, browser, report, artifacts)
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        finish_environment(server, fixture, report)
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(
                artifacts.redact(server_log.read_text()))
        report["private_state"] = (
            "private/ retains isolated DB/logs; no process kept; remove only this run "
            "directory after diagnosis (7 days)"
            if report["environment_cleanup_ok"] else
            "private/ retains isolated DB/logs; inspect cleanup errors, owned_server_pid "
            "and fixture_active_handlers before removing this run directory (7 days)")
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = ("credential scan rejected artifacts; private "
                                        "directory must not be uploaded")
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": selected["contract"],
                      "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")},
                     ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
