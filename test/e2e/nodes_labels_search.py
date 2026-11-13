#!/usr/bin/env python3
"""Node definitions / labels, search and label filtering through the browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-nodes-labels-search
After build: python3 test/e2e/nodes_labels_search.py --locale en
Requires Python Playwright and system Chrome. The two offline node definitions
are prepared through the product API so the tested operations (label edit,
search, filter) happen in the browser. No agent, GPU or container is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and retains the
private database/logs for scoped diagnosis (seven days).
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
import traceback
from urllib.parse import parse_qs, quote, urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from dialog_checks import assert_dialog_shell
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from nodes_label_edit import exercise_label_edit_changes
from nodes_label_lengths import (
    exercise_label_dismissals, exercise_label_edit_backfill, exercise_label_field_inventory,
    exercise_label_conflict, exercise_label_filter_clear, exercise_label_lengths,
    exercise_label_text_edit, exercise_label_keyboard, exercise_minimal_label)
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/node/labels-search-filter"
CASE = "节点 / 离线节点定义 / 标签保存、名称搜索与标签筛选 / 刷新和独立 GET 一致"
NODES = "/api/v1/nodes"


def strings(locale):
    ui = json.loads((ROOT / "ui/src/i18n/locales" / locale / "nodes.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return ui, auth


def action(scope, label):
    # AntD icons contribute an accessible-name prefix (for example
    # "plus Add label"), so match the label inside the accessible name.
    accessible = r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile(accessible)).first


# Contract: gpuworks/node/labels-search-filter
# Mainline: 独立 server/SQLite + API 准备两个离线节点 → 节点页同时显示
#           → 名称搜索只保留目标节点 → UI 编辑标签并保存 → 刷新/独立 GET 回读
#           → 标签筛选命中目标且排除另一节点 → 清除筛选 → 强制删除自有节点
def test_node_labels_search(page, context, server, artifacts, report, locale):
    ui, auth = strings(locale)
    name_a = f"e2e-{report['run_id']}-node-a"
    name_b = f"e2e-{report['run_id']}-node-b"
    page_errors, console_errors, bad_responses, requests = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            if (not authenticated
                    and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me"
                    and "401" in message.text):
                return
            console_errors.append(message.text)

    browser_queries = []

    def response_seen(response):
        parsed = urlsplit(response.url)
        path = parsed.path
        if path == NODES and response.request.method == "GET":
            # The page's own list request, not a GET the test replays.
            browser_queries.append({"query": parse_qs(parsed.query),
                                    "status": response.status})
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": path,
                                  "status": response.status})

    def request_seen(request):
        path = urlsplit(request.url).path
        if path.endswith("/labels") and request.method == "PUT":
            requests.append({"method": request.method, "path": path,
                             "body": request.post_data_json})

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/nodes")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=ui["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"

    nodes, query_evidence = [], []
    for name in (name_a, name_b):
        created = page.request.post(server.base + NODES,
                                    data={"name": name, "server_url": server.base})
        check(created.status == 201, f"node fixture HTTP {created.status}")
        body = created.json()
        artifacts.remember(body.get("token", ""))
        check(body["node"]["name"] == name, "node fixture returned another name")
        nodes.append(body["node"])
    node_a, node_b = nodes

    page.reload()
    row_a = page.get_by_role("row").filter(has_text=name_a)
    row_b = page.get_by_role("row").filter(has_text=name_b)
    expect(row_a).to_have_count(1)
    expect(row_b).to_have_count(1)

    def names(response):
        body = response.json()
        return {item["name"] for item in body["items"]}, body["total"]

    def saw_query(**expected):
        return any(item["status"] == 200 and all(
            item["query"].get(key) == [value] for key, value in expected.items())
            for item in browser_queries)

    def clean_queries():
        return [item for item in browser_queries
                if item["status"] == 200 and not item["query"].get("q")
                and not item["query"].get("label")]

    clean_before = len(clean_queries())
    search = page.get_by_label(ui["page"]["search"], exact=True)
    search.fill(report["run_id"])
    search.press("Enter")
    expect(row_a).to_have_count(1)
    expect(row_b).to_have_count(1)
    partial = page.request.get(server.base + NODES + "?q=" + quote(report["run_id"]))
    got = names(partial)
    check(partial.status == 200, f"partial search HTTP {partial.status}")
    check(got == ({name_a, name_b}, 2), f"partial search returned {got}")
    check(saw_query(q=report["run_id"]),
          "the browser did not issue the partial search request")
    query_evidence.append({"query": "q=" + report["run_id"],
                           "names": sorted(got[0]), "total": got[1]})
    report["last_step"] = "partial-search"

    search.fill(name_a)
    search.press("Enter")
    expect(row_a).to_have_count(1)
    expect(row_b).to_have_count(0)
    exact = page.request.get(server.base + NODES + "?q=" + quote(name_a))
    got = names(exact)
    check(exact.status == 200, f"exact search HTTP {exact.status}")
    check(got == ({name_a}, 1), f"exact search returned {got}")
    check(saw_query(q=name_a), "the browser did not issue the exact search request")
    query_evidence.append({"query": "q=" + name_a,
                           "names": sorted(got[0]), "total": got[1]})
    report["last_step"] = "exact-search"

    search.fill(f"{name_a}-missing")
    search.press("Enter")
    expect(row_a).to_have_count(0)
    expect(row_b).to_have_count(0)
    expect(page.get_by_text(ui["empty"]["filtered"], exact=True)).to_be_visible()
    missing = page.request.get(server.base + NODES + "?q=" + quote(f"{name_a}-missing"))
    got = names(missing)
    check(missing.status == 200 and got == (set(), 0), f"missing search returned {got}")
    check(saw_query(q=f"{name_a}-missing"),
          "the browser did not issue the empty-result search request")
    query_evidence.append({"query": "q=" + f"{name_a}-missing",
                           "names": sorted(got[0]), "total": got[1]})
    report["last_step"] = "empty-search"

    search.fill("")
    search.press("Enter")
    expect(row_a).to_have_count(1)
    expect(row_b).to_have_count(1)
    all_nodes = page.request.get(server.base + NODES)
    got = names(all_nodes)
    check(all_nodes.status == 200 and got == ({name_a, name_b}, 2),
          f"cleared search returned {got}")
    check(len(clean_queries()) > clean_before,
          "the browser did not re-query the list after clearing the search")
    query_evidence.append({"query": "", "names": sorted(got[0]), "total": got[1]})
    report["last_step"] = "search-cleared"

    row_a.get_by_role("button", name=ui["action"]["more"], exact=True).click()
    page.get_by_role("menuitem").filter(has_text=ui["labels"]["edit"]).click()
    modal = page.get_by_role("dialog")
    expect(modal).to_be_visible()
    add_label = action(modal, ui["labels"]["add"])
    assert_dialog_shell(page, modal, add_label, modal.locator(".ant-modal-footer .ant-btn-primary"))
    modal = exercise_label_dismissals(page, modal, name_a, node_a["uuid"], server.base, ui, requests, report)
    report["evidence"].append({
        "layer": "ui", "dialog_shell": ui["labels"]["title"], "viewport": "1440x900"})
    viewport = page.viewport_size
    box = modal.bounding_box()
    check(box and box["x"] >= 0 and box["y"] >= 0
          and box["x"] + box["width"] <= viewport["width"] + 1
          and box["y"] + box["height"] <= viewport["height"] + 1,
          f"labels modal is outside the viewport: {box}")
    save_box = button(modal, ui["action"]["save"]).bounding_box()
    check(save_box and save_box["y"] + save_box["height"] <= viewport["height"] + 1,
          f"labels Save is outside the viewport: {save_box}")
    action(modal, ui["labels"]["add"]).click()
    report["label_inventory"] = exercise_label_field_inventory(modal, ui, requests)
    report["last_step"] = "label-field-inventory"
    report["label_keyboard"] = exercise_label_keyboard(page, modal, ui, requests)
    report["last_step"] = "label-keyboard"
    labels_path = NODES + f"/{node_a['uuid']}/labels"
    key_input = modal.get_by_label(ui["labels"]["key"], exact=True)
    value_input = modal.get_by_label(ui["labels"]["value"], exact=True)
    save_button = modal.locator(".ant-modal-footer .ant-btn-primary")
    # 2.2: an empty key never leaves the browser. A whitespace-only key and a
    # key with surrounding spaces are rejected by the server and are not trimmed
    # into a stored key. The value field is still "alpha" and is not saved.
    puts_before_keys = len(requests)
    key_input.fill("")
    value_input.fill("alpha")
    save_button.click()
    expect(modal.get_by_text(ui["labels"]["keyRequired"], exact=True)).to_be_visible()
    check(len(requests) == puts_before_keys, "an empty label key was submitted")
    untouched = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
    check(untouched.json()["labels"] == {}, "an empty label key changed stored labels")

    def reject_key(text):
        key_input.fill(text)
        value_input.fill("alpha")
        with page.expect_response(lambda r: urlsplit(r.url).path == labels_path
                                  and r.request.method == "PUT") as rejected_key:
            save_button.click()
        check(rejected_key.value.status == 400, f"label key {text!r} was accepted")
        check(rejected_key.value.request.post_data_json == {"labels": {text: "alpha"}},
              f"label key request was rewritten: {rejected_key.value.request.post_data_json}")
        expect(modal).to_be_visible()
        stayed = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
        check(stayed.json()["labels"] == {}, f"label key {text!r} changed stored labels")

    # A whitespace-only key never reaches the server: the form marks it required (whitespace:
    # true in NodeLabelsModal), so no PUT is sent and the stored labels stay empty.
    key_input.fill("   ")
    value_input.fill("alpha")
    save_button.click()
    expect(modal.get_by_text(ui["labels"]["keyRequired"], exact=True)).to_be_visible()
    check(len(requests) == puts_before_keys, "a whitespace-only key was submitted")
    reject_key(" team ")
    key_input.fill("team")
    value_input.fill("alpha")
    report["last_step"] = "label-key-whitespace"

    # A reserved gpuworks.ai/ key is a real server-side constraint. The UI
    # must report it, keep the modal and the stored labels unchanged, and
    # allow the corrected save.
    action(modal, ui["labels"]["add"]).click()
    key_fields = modal.get_by_label(ui["labels"]["key"], exact=True)
    value_fields = modal.get_by_label(ui["labels"]["value"], exact=True)
    check(key_fields.count() == 2, "adding a second label row did not render it")
    key_fields.nth(1).fill("gpuworks.ai/reserved")
    value_fields.nth(1).fill("true")
    save_button = modal.locator(".ant-modal-footer .ant-btn-primary")
    with page.expect_response(lambda r: urlsplit(r.url).path == labels_path
                              and r.request.method == "PUT") as rejected:
        save_button.click()
    check(rejected.value.status == 400,
          f"reserved label key was accepted: {rejected.value.status}")
    expect(page.get_by_text(re.compile(
        re.escape(ui["labels"]["failed"].split("{{")[0].strip()))).first).to_be_visible()
    check(modal.is_visible(), "a rejected label save closed the modal")
    after_reject = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
    check(after_reject.json()["labels"] == {},
          "a rejected label save changed the stored labels")
    modal.get_by_role("button", name=ui["labels"]["remove"], exact=True).last.click()
    expect(save_button).to_be_enabled()
    report["last_step"] = "reserved-label-rejected"

    # The corrected row is still only a draft. Hold its PUT: Save shows the
    # loading state, a second click sends nothing, and a 502 keeps the row.
    reason = "upstream fixture is unreachable"
    held = []

    def hold_put(route):
        if route.request.method != "PUT" or urlsplit(route.request.url).path != labels_path:
            route.continue_()
            return
        held.append(route)

    page.route("**" + labels_path, hold_put)
    save_button.click()
    expect(save_button).to_have_class(re.compile(r"\bant-btn-loading\b"))
    save_button.click()
    check(len(held) == 1, "duplicate label save while the first request is held")
    check(held[0].request.post_data_json == {"labels": {"team": "alpha"}}, "held label body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == labels_path
                              and r.request.method == "PUT") as failed_save:
        held[0].fulfill(status=502, content_type="application/json",
                        body=json.dumps({"message": reason}))
    page.unroute("**" + labels_path, hold_put)
    check(failed_save.value.status == 502, "forced label failure did not surface")
    expect(modal).to_be_visible()
    expect(modal.get_by_label(ui["labels"]["key"], exact=True)).to_have_value("team")
    expect(modal.get_by_label(ui["labels"]["value"], exact=True)).to_have_value("alpha")
    expect(page.get_by_text(re.compile(
        re.escape(ui["labels"]["failed"].split("{{")[0].strip()))).first).to_be_visible()
    expect(page.get_by_text(reason).first).to_be_visible()
    after_hold = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
    check(after_hold.json()["labels"] == {}, "the refused label save changed stored labels")
    check(any(item.get("path") == labels_path and item.get("status") == 502 for item in bad_responses),
          "502 was not recorded")
    check(any("502" in text for text in console_errors), "502 console line absent: " + str(console_errors))
    bad_responses[:] = [item for item in bad_responses if item.get("status") != 502]
    console_errors[:] = [text for text in console_errors if "502" not in text]
    expect(save_button).to_be_enabled()
    report["last_step"] = "labels-inflight-rejected"
    report["evidence"].append({"layer": "ui", "inflight_puts": 1, "confirm_loading": True,
                               "failure_status": 502, "draft_retained": True,
                               "failure_reason_visible": True})

    with page.expect_response(lambda r: urlsplit(r.url).path == labels_path
                              and r.request.method == "PUT") as saved:
        save_button.click()
    check(saved.value.status == 200, f"labels save HTTP {saved.value.status}")
    check(saved.value.request.post_data_json == {"labels": {"team": "alpha"}},
          f"labels body differs: {saved.value.request.post_data_json}")
    expect(page.get_by_text(ui["labels"]["saved"], exact=True).first).to_be_visible()
    report["last_step"] = "labels-saved"
    row_a = page.get_by_role("row").filter(has_text=name_a)
    expect(row_a.get_by_text("team=alpha", exact=True)).to_be_visible()

    page.reload()
    row_a = page.get_by_role("row").filter(has_text=name_a)
    expect(row_a.get_by_text("team=alpha", exact=True)).to_be_visible()
    independent = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
    check(independent.status == 200, f"independent node GET HTTP {independent.status}")
    check(independent.json()["labels"] == {"team": "alpha"},
          "independent node read differs from the saved labels")
    saved_fields = list(saved.value.request.post_data_json)
    check(saved_fields == ["labels"], f"label save fields {saved_fields}")
    check(independent.json()["labels"] == saved.value.request.post_data_json["labels"],
          "the independent labels field differs from the save request")
    report["label_inventory"]["request_fields"] = saved_fields
    report["label_inventory"]["persisted_field"] = "labels"
    report["label_inventory"]["persisted"] = independent.json()["labels"]

    def open_labels():
        page.get_by_role("row").filter(has_text=name_a).get_by_role(
            "button", name=ui["action"]["more"], exact=True).click()
        page.get_by_role("menuitem").filter(has_text=ui["labels"]["edit"]).click()
        dialog = page.get_by_role("dialog")
        expect(dialog).to_be_visible()
        return dialog

    reopened = open_labels()
    report["label_backfill"] = exercise_label_edit_backfill(reopened, ui)
    report["last_step"] = "label-edit-backfill"
    report["label_conflict"] = exercise_label_conflict(page, server.base, node_a, ui, console_errors, bad_responses, artifacts)

    def save_label_value(value):
        dialog = open_labels()
        dialog.get_by_label(ui["labels"]["value"], exact=True).fill(value)
        with page.expect_response(lambda r: urlsplit(r.url).path == labels_path
                                  and r.request.method == "PUT") as saved_value:
            dialog.locator(".ant-modal-footer .ant-btn-primary").click()
        body = {"labels": {"team": value}}
        check(saved_value.value.status == 200, f"label value {value!r} HTTP {saved_value.value.status}")
        check(saved_value.value.request.post_data_json == body,
              f"label value request {saved_value.value.request.post_data_json}")
        expect(dialog).not_to_be_visible()
        readback = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
        check(readback.status == 200 and readback.json()["labels"] == {"team": value},
              f"label value readback {readback.json().get('labels')!r}")
        return value

    report["last_step"] = "label-value-whitespace"
    report["label_whitespace"] = {
        "empty_key_submitted": False,
        "rejected_keys": ["   ", " team "],
        "empty_value": save_label_value(""),
        "blank_value": save_label_value("   "),
        "padded_value": save_label_value(" alpha "),
        "restored": save_label_value("alpha"),
    }


    # 2.5: every refusal above happened while the node carried no labels, so "the stored value is
    # preserved" was only ever the empty map compared with itself. That comparison cannot fail
    # whatever the product does with a rejected write. The node now holds team=alpha, so the same
    # service-side rules are driven again against a stored value that could actually be lost:
    # the reserved namespace and an over-long value are refused, and the stored labels must be
    # byte-identical afterwards. The corrected save then has to go through and read back.
    stored_before = page.request.get(server.base + NODES + f"/{node_a['uuid']}").json()["labels"]
    check(stored_before == {"team": "alpha"},
          f"2.5 needs a stored label to protect, found {stored_before!r}")

    def refused_over_a_stored_value(key_text, value_text, why):
        dialog = open_labels()
        rows_key = dialog.get_by_label(ui["labels"]["key"], exact=True)
        rows_value = dialog.get_by_label(ui["labels"]["value"], exact=True)
        rows_key.nth(0).fill(key_text)
        rows_value.nth(0).fill(value_text)
        with page.expect_response(lambda r: urlsplit(r.url).path == labels_path
                                  and r.request.method == "PUT") as refused:
            dialog.locator(".ant-modal-footer .ant-btn-primary").click()
        check(refused.value.status == 400,
              f"{why}: the product answered {refused.value.status}, not a refusal")
        after = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
        check(after.status == 200, f"{why}: the node stopped reading after the refusal")
        check(after.json()["labels"] == stored_before,
              f"{why}: the rejected write changed the stored labels "
              f"{stored_before!r} -> {after.json().get('labels')!r}")
        dialog.locator(".ant-modal-footer .ant-btn-default").click()
        return {"refused_status": refused.value.status,
                "stored_before": stored_before,
                "stored_after": after.json()["labels"],
                "unchanged": after.json()["labels"] == stored_before}

    # Two distinct refusals, both over the stored label. The plain reserved namespace and a
    # subdomain of it are refused separately because Reserved covers both; the value-length rule
    # is not used here because the form's own maxLength stops a 257-byte value from ever being
    # submitted, so a rejection of it would prove only that the input was clipped.
    reserved_over_stored = refused_over_a_stored_value(
        "gpuworks.ai/reserved", "true", "reserved key over a stored label")
    reserved_subdomain_over_stored = refused_over_a_stored_value(
        "rack.gpuworks.ai/rack", "1", "reserved subdomain key over a stored label")

    # The corrected save, from the same dialog position, has to be accepted and read back. It
    # writes the value the rest of this case expects, so no later step inherits a changed label.
    corrected = open_labels()
    corrected.get_by_label(ui["labels"]["key"], exact=True).nth(0).fill("team")
    corrected.get_by_label(ui["labels"]["value"], exact=True).nth(0).fill("alpha")
    with page.expect_response(lambda r: urlsplit(r.url).path == labels_path
                              and r.request.method == "PUT") as recovered:
        corrected.locator(".ant-modal-footer .ant-btn-primary").click()
    check(recovered.value.status == 200,
          f"the corrected save over a stored label HTTP {recovered.value.status}")
    check(recovered.value.request.post_data_json == {"labels": {"team": "alpha"}},
          f"the corrected save body {recovered.value.request.post_data_json}")
    recovered_read = page.request.get(server.base + NODES + f"/{node_a['uuid']}")
    check(recovered_read.status == 200 and recovered_read.json()["labels"] == {"team": "alpha"},
          f"the corrected save did not read back: {recovered_read.json().get('labels')!r}")
    report["evidence"].append({"layer": "request", "constraint_over_a_stored_value": {
        "stored_before": stored_before,
        "reserved_namespace": reserved_over_stored,
        "reserved_subdomain": reserved_subdomain_over_stored,
        "corrected_status": recovered.value.status,
        "corrected_body": recovered.value.request.post_data_json,
        "corrected_readback": recovered_read.json()["labels"],
        "error_cleared": True,
    }})

    report["last_step"] = "label-constraint-over-a-stored-value"
    edited = exercise_label_edit_changes(page, server.base, node_a, ui, open_labels)
    report["label_edit"] = {key: value for key, value in edited.items() if key != "puts"}
    report["label_edit_puts"] = edited["puts"]

    # 2.3: key length is 1..128 bytes. Value length is 0..256 bytes.
    # 86 CJK characters fit in the control and the server still rejects them.
    length = exercise_label_lengths(
        page, server.base, node_a["uuid"], ui, open_labels, requests)
    report["label_length"] = length["observed"]
    report["label_length_puts"] = length["extra_puts"]
    report["last_step"] = "label-length"
    expect(page.get_by_role("row").filter(has_text=name_a).get_by_text(
        "team=alpha", exact=True)).to_be_visible()

    # 2.6: paste, keyboard clear and keyboard edit. A newline is not stored.
    edited = exercise_label_text_edit(
        page, context, server.base, node_a["uuid"], ui, open_labels, requests)
    report["label_text_edit"] = edited["observed"]
    report["label_text_puts"] = edited["extra_puts"]
    report["last_step"] = "label-text-edit"
    expect(page.get_by_role("row").filter(has_text=name_a).get_by_text(
        "team=alpha", exact=True)).to_be_visible()

    minimal = exercise_minimal_label(page, server.base, node_b, ui)
    report.update(label_minimal=minimal["observed"], label_minimal_puts=minimal["extra_requests"], last_step="label-minimal")

    filter_select = page.get_by_role("combobox", name=ui["labelFilter"]["aria"], exact=True)
    filter_select.click()
    # The multi-select accepts a real search term; prove both the filtered
    # candidate set and the no-result state before selecting.
    filter_select.fill("missing-label")
    expect(page.get_by_text(ui["labelFilter"]["none"], exact=True)).to_be_visible()
    filter_select.fill("team=al")
    visible_options = page.locator(".ant-select-dropdown:visible .ant-select-item-option")
    expect(visible_options).to_have_text(["team=alpha"])
    expect(visible_options.filter(has_text="team=beta")).to_have_count(0)
    # AntD keeps a hidden ARIA listbox beside the visible virtual dropdown; the
    # visible option is the one the user can click.
    option = visible_options.filter(has_text=re.compile(r"^team=alpha$"))
    expect(option).to_be_visible()
    option.click()
    expect(row_a).to_have_count(1)
    expect(row_b).to_have_count(0)
    check(saw_query(label="team=alpha"),
          "the browser did not issue the label filter request")
    labeled = page.request.get(server.base + NODES + "?label=" + quote("team=alpha", safe=""))
    got = names(labeled)
    check(labeled.status == 200 and got == ({name_a}, 1),
          f"label filter returned {got}")
    query_evidence.append({"query": "label=team=alpha",
                           "names": sorted(got[0]), "total": got[1]})
    report["last_step"] = "label-filter-selected"
    cleared_filter = exercise_label_filter_clear(
        page, server.base, filter_select, row_a, row_b, name_a, name_b, clean_queries)
    report["label_filter_clear"] = {
        key: cleared_filter[key] for key in (
            "escape_kept_filter", "clear_control", "selection_after_clear", "names", "total")}
    query_evidence.append(cleared_filter["query"])
    report["last_step"] = "label-filter-cleared"

    for node in (node_a, node_b):
        deleted = page.request.delete(server.base + NODES + f"/{node['uuid']}?force=true")
        check(deleted.status == 200, f"node cleanup HTTP {deleted.status}")
    for node in (node_a, node_b):
        check(page.request.get(server.base + NODES + f"/{node['uuid']}").status == 404,
              f"cleaned node {node['uuid']} is still readable")

    expected_reject = {"method": "PUT", "path": labels_path, "status": 400}
    unexpected = [item for item in bad_responses if item != expected_reject]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    # One resource-400 line per refused PUT: the padded key, the reserved key, the 86-character
    # value, and the two refusals 2.5 drives over a stored label. The count is taken from the
    # refusals actually recorded rather than written as a fixed number, so adding a refusal does
    # not silently rot this check - and any console error that is not a 400 line still fails.
    refusals = len([item for item in bad_responses if item == expected_reject])
    check(refusals >= 3, f"only {refusals} refused label writes were recorded")
    allowed_console = [item for item in console_errors
                       if "400" in item and ("load resource" in item or "加载资源" in item)]
    check(len(allowed_console) == refusals
          and len(allowed_console) == len(console_errors) and not page_errors,
          f"browser errors ({len(allowed_console)} 400 lines vs {refusals} refusals): "
          f"{page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "nodes page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    rejected_body = {"labels": {"team": "alpha", "gpuworks.ai/reserved": "true"}}
    saved_body = {"labels": {"team": "alpha"}}
    held_body = {"labels": {"team": "alpha"}}
    check(requests == [
        {"method": "PUT", "path": labels_path, "body": {"labels": {" team ": "alpha"}}},
        {"method": "PUT", "path": labels_path, "body": rejected_body},
        {"method": "PUT", "path": labels_path, "body": held_body},
        {"method": "PUT", "path": labels_path, "body": saved_body}] + report["label_conflict"]["puts"] + [
        {"method": "PUT", "path": labels_path, "body": {"labels": {"team": ""}}},
        {"method": "PUT", "path": labels_path, "body": {"labels": {"team": "   "}}},
        {"method": "PUT", "path": labels_path, "body": {"labels": {"team": " alpha "}}},
        {"method": "PUT", "path": labels_path, "body": {"labels": {"team": "alpha"}}},
        # 2.5: two refusals driven over the stored label, then the corrected save.
        {"method": "PUT", "path": labels_path,
         "body": {"labels": {"gpuworks.ai/reserved": "true"}}},
        {"method": "PUT", "path": labels_path,
         "body": {"labels": {"rack.gpuworks.ai/rack": "1"}}},
        {"method": "PUT", "path": labels_path, "body": {"labels": {"team": "alpha"}}},
    ] + report["label_edit_puts"] + [{"method": "PUT", "path": labels_path, "body": body}
         for body in report["label_length_puts"] + report["label_text_puts"]] + report["label_minimal_puts"],
          f"unexpected label PUTs: {requests}")

    report["evidence"].extend([
        {"layer": "ui", "page": "/nodes", "rows": [name_a, name_b],
         "search": {"partial": 2, "exact": "target only", "missing": "empty state",
                    "cleared": 2},
         "labels_modal": ui["labels"]["title"], "modal_inside_viewport": True,
         "reserved_label_rejected": True,
         "label_inventory": report["label_inventory"],
         "label_backfill": report["label_backfill"],
         "label_length": report["label_length"],
         "label_filter": "target only, clear restores both"},
        {"layer": "request", "fixture_nodes": [{"name": name_a}, {"name": name_b}],
         "list_queries": query_evidence,
         "browser_list_queries": browser_queries, "label_puts": requests,
         "rejected_put": {"path": labels_path, "body": rejected_body, "status": 400}},
        {"layer": "persistence", "refresh": "team=alpha remains on the row",
         "independent_get": independent.json()["labels"]},
        {"layer": "cleanup", "force_deleted": 2, "after_delete": 404},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "console_allowed": allowed_console,
                                "http": bad_responses, "requests": requests}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/nodes-labels-search.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_node_labels_search.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "label_lengths_sha256": digest(ROOT / "test/e2e/nodes_label_lengths.py"),
        "label_edit_sha256": digest(ROOT / "test/e2e/nodes_label_edit.py"),
        "execution": "N/A: the two nodes are offline definitions; no agent or workload is involved",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["the add-node drawer and SSH installation",
                        "renaming a node", "online/reconnect behavior",
                        "scheduling and eviction", "duplicate label keys",
                        "column sorting and pagination", "multiple label filters and their AND semantics",
                        "search by hostname or UUID, special characters and case/whitespace rules",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/nodes_labels_search.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_node_labels_search(page, context, server, artifacts, report, args.locale)
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
                try:
                    context.close()
                finally:
                    browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        report["private_state"] = "private/ retains isolated DB/logs; no process kept; remove only this run directory after diagnosis (7 days)"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; private directory must not be uploaded"
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": CONTRACT, "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
