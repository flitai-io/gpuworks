#!/usr/bin/env python3
"""HTTP model preview/registration and upstream loss recovery against real server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-http-model-registration (builds, then both English cases).
CLI after make build:
  python3 test/e2e/http_model_registration.py --locale en --case register
Choices: --locale en|zh; --case register|recovery|all.
Set NO_PROXY and no_proxy to 127.0.0.1,localhost.
Requires Python Playwright/system Chrome. Each case owns a server/SQLite and
an audited loopback HTTP directory; no agent download or GPU execution claim.
Artifacts: secrets/e2e/<runId>/report.json, trace and source-request audit.
Failed state stays private for diagnosis; only this case's listeners are stopped.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from http_model_fixture import ModelDirectory
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check, modal_geometry, theme_color

MODELS = "/api/v1/models"
PREVIEW = MODELS + "/preview-url"
REGISTER = MODELS + "/register-url"


def model_db(server):
    conn = sqlite3.connect(server.work / "server-data" / "gpuworks.db", timeout=5)
    conn.execute("PRAGMA busy_timeout=5000")
    return conn


def model_db_count(server, sql, args):
    conn = model_db(server)
    try:
        return conn.execute(sql, args).fetchone()[0]
    finally:
        conn.close()


def place_model_reference(server, model_id):
    # DeleteModel counts deployment rows for this model before it removes anything.
    conn = model_db(server)
    try:
        name = "e2e-ref-" + str(model_id)
        conn.execute(
            "INSERT INTO deployments (name, model_id, served_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
            (name, model_id, name, "2026-10-24 00:00:00", "2026-10-24 00:00:00"))
        conn.commit()
        left = conn.execute("SELECT COUNT(*) FROM deployments WHERE model_id = ?", (model_id,)).fetchone()[0]
        check(left == 1, "deployment reference missing after insert")
        return name
    finally:
        conn.close()


def clear_model_reference(server, model_id):
    conn = model_db(server)
    try:
        cur = conn.execute("DELETE FROM deployments WHERE model_id = ?", (model_id,))
        conn.commit()
        check(cur.rowcount == 1, "deployment reference was not the one inserted")
    finally:
        conn.close()


def place_deleting_file(server, model_id):
    # A file still awaiting deletion acknowledgement refuses model removal.
    conn = model_db(server)
    try:
        conn.execute(
            "INSERT INTO model_files (model_id, state, created_at, updated_at) VALUES (?, ?, ?, ?)",
            (model_id, "deleting", "2026-10-24 00:00:00", "2026-10-24 00:00:00"))
        conn.commit()
        left = conn.execute(
            "SELECT COUNT(*) FROM model_files WHERE model_id = ? AND state = ?",
            (model_id, "deleting")).fetchone()[0]
        check(left == 1, "deleting file row missing after insert")
    finally:
        conn.close()


def clear_deleting_file(server, model_id):
    conn = model_db(server)
    try:
        cur = conn.execute("DELETE FROM model_files WHERE model_id = ? AND state = ?", (model_id, "deleting"))
        conn.commit()
        check(cur.rowcount == 1, "deleting file row was not the one inserted")
    finally:
        conn.close()


def model_list(page, server):
    response = page.request.get(server.base + MODELS)
    check(response.status == 200, "independent model GET failed")
    data = response.json()
    check(set(data) == {"items", "total"} and data["total"] == len(data["items"]), "model page envelope drift")
    return data["items"]


def exercise_register_dialog(page, trigger, title, parent_name, field_label, save_label, cancel_label, mutations, evidence):
    """Dialog identity, layout, focus, and dismissals. No model write."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    parent = page.get_by_role("heading", name=parent_name, exact=True)
    baseline = len(mutations)

    def open_dialog():
        trigger.click()
        dialog = page.get_by_role("dialog", name=title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = open_dialog()
    assert_dialog_shell(page, dialog, dialog.get_by_label(field_label, exact=True), button(dialog, save_label))
    dialog = assert_nested_dismissals(
        dialog, parent, open_dialog,
        [("cancel", cancel_button(cancel_label)), ("escape", escape),
         ("close", close_button), ("mask", mask)],
        lambda: len(mutations), trigger)
    check(len(mutations) == baseline, "register dialog dismissal sent a write")
    evidence.append({"layer": "ui", "register_dialog": title,
                     "dismissals": ["cancel", "escape", "close", "mask"], "write_requests": 0})
    return dialog


# Contract: gpuworks/models/http-register; gpuworks/models/http-register-recovery
# Mainline (register): 独立HTTP目录 → UI预览、覆盖名称/来源ID并注册 → GET+刷新核对元数据、真实来源和零副本 → UI删除、GET消失。
# Mainline (recovery): 独立HTTP目录成功预览 → 上游真实503 → UI注册502且草稿保留/零持久化 → 恢复上游并原样重试 → GET+刷新正确 → UI删除。
def test_model_registration(page, context, server, fixture, artifacts, report, locale, scenario):
    directory = ROOT / "ui/src/i18n/locales" / locale
    k = json.loads((directory / "models.json").read_text())
    auth = json.loads((directory / "auth.json").read_text())
    common = json.loads((directory / "common.json").read_text())
    mutations, failures, errors, console_errors = [], [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(server.base + "/models")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=k["page"]["title"], exact=True)).to_be_visible()
    check(model_list(page, server) == [], "fresh isolated database contains models")
    report["preflight"] = "PASS"
    report["active_evidence_layer"] = "ui"
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path})
            if urlsplit(r.url).path.startswith(MODELS) and r.method in {"POST", "DELETE", "PATCH"} else None)
    page.on("console", lambda message: console_errors.append({"path": urlsplit(message.location.get("url", "")).path, "text": message.text}) if message.type == "error" else None)
    page.on("response", lambda r: failures.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    artifacts.start_trace(context)
    # Choose the product's list view through its real segmented control.
    page.get_by_role("img", name=k["view"]["list"], exact=True).click()
    expect(page.get_by_role("radio", name=k["view"]["list"], exact=True)).to_be_checked()
    # Empty state repeats the same action; the header's first occurrence is stable document order.
    trigger = button(page, k["action"]["register"]).first
    expect(trigger).to_be_visible()
    dialog = exercise_register_dialog(
        page, trigger, k["action"]["register"], k["page"]["title"],
        k["column"]["source"], k["register"]["confirm"], k["action"]["cancel"],
        mutations, report["evidence"])
    source = dialog.get_by_label(k["column"]["source"], exact=True)
    source.click()
    page.get_by_text(k["source"]["url"], exact=True).click()
    report["last_step"] = "register-modal-width"
    expect(dialog).to_have_css("width", "720px", timeout=12000)
    report["register_modal_width"] = True
    url = dialog.get_by_label(k["register"]["directoryURL"], exact=True)
    expect(url).to_have_value("")
    from http_register_inventory import exercise_http_register_inventory
    report["register_inventory"] = exercise_http_register_inventory(dialog, k)
    report["evidence"].append({"layer": "ui", "register_inventory": report["register_inventory"]})
    parse = button(dialog, k["register"]["parseDirectory"])
    parse.click()
    expect(dialog.get_by_text(k["register"]["directoryURLRequired"], exact=True)).to_be_visible()
    expect(url).to_have_attribute("aria-invalid", "true")
    check(mutations == [], "empty directory sent a preview or mutation")
    # 1.3: the required rule declares whitespace:true, so a directory of spaces is refused in the
    # browser exactly like an empty one. Without that flag antd accepts it and a preview is posted.
    url.fill("   ")
    parse.click()
    expect(dialog.get_by_text(k["register"]["directoryURLRequired"], exact=True)).to_be_visible()
    expect(url).to_have_attribute("aria-invalid", "true")
    expect(url).to_have_value("   ")
    check(mutations == [], "a whitespace-only directory sent a preview or mutation")
    report["directory_whitespace_refused"] = True
    url.fill(fixture.url + "/")
    url.press("Tab")
    page.keyboard.press("Shift+Tab")
    expect(url).to_be_focused()
    theme_color(url, "borderColor", "primary")
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(trigger).to_be_focused()
    check(mutations == [], "Esc sent a mutation")
    trigger.click()
    source.click()
    page.get_by_text(k["source"]["url"], exact=True).click()
    url.fill(fixture.url + "/")
    report["active_evidence_layer"] = "preview"
    with page.expect_response(lambda r: urlsplit(r.url).path == PREVIEW and r.request.method == "POST") as preview_response:
        parse.click()
    preview = preview_response.value
    expected_preview = {"source_url": fixture.url, "source_id": fixture.root.lstrip("/"), "name": "tiny-model",
        "arch": "Qwen3ForCausalLM", "precision": "bfloat16", "context_length": 40960,
        "weight_bytes": len(fixture.files["model.safetensors"]), "files_count": 2, "input_modalities": ["text"]}
    check(preview.status == 200 and preview.request.post_data_json == {"url": fixture.url + "/"}
          and not urlsplit(preview.url).query and preview.json() == expected_preview, "preview request/metadata mismatch")
    expect(dialog.get_by_text(k["register"]["urlParsed"], exact=True)).to_be_visible()
    report["last_step"] = "register-parsed-columns"
    parsed = dialog.locator(".ant-alert-success .ant-descriptions")
    expect(parsed).to_have_count(1, timeout=12000)
    arch_row = parsed.locator(".ant-descriptions-row").filter(has_text="Qwen3ForCausalLM")
    expect(arch_row).to_have_count(1, timeout=12000)
    expect(arch_row).to_contain_text("bfloat16", timeout=12000)
    expect(arch_row).not_to_contain_text(fixture.url, timeout=12000)
    report["register_parsed_columns"] = True
    expect(dialog.get_by_text("Qwen3ForCausalLM", exact=True)).to_be_visible()
    expect(dialog.get_by_text("bfloat16", exact=True)).to_be_visible()
    expect(dialog.get_by_text("40,960", exact=True)).to_be_visible()
    expect(dialog.get_by_text(f"{len(fixture.files['model.safetensors']) / 1024:.1f} KiB", exact=True)).to_be_visible()
    expect(dialog.get_by_text(fixture.url, exact=True)).to_be_visible()
    expect(url).to_have_value(fixture.url)
    name = dialog.get_by_label(k["column"]["name"], exact=True)
    source_id = dialog.get_by_label(k["register"]["modelID"], exact=True)
    expect(name).to_have_value("tiny-model")
    expect(source_id).to_have_value(expected_preview["source_id"])
    check(model_list(page, server) == [], "preview persisted a model")
    before_submit = fixture.audit()
    check(any(r["method"] == "GET" and r["path"] == fixture.root + "/config.json" and r["status"] == 200 for r in before_submit), "preview did not read actual config")
    check(all(r["sha256"] == hashlib.sha256(fixture.files["config.json"]).hexdigest() for r in before_submit if r["path"] == fixture.root + "/config.json"), "audited config bytes differ from fixture authority")
    check(any(r["method"] == "HEAD" and r["path"] == fixture.root + "/model.safetensors" and r["status"] == 200 for r in before_submit), "preview did not verify exact weight bytes")
    target_name = f"e2e-{report['run_id']}-registered"
    target_source = f"e2e-{report['run_id']}/operator-model"
    name.fill(target_name)
    source_id.fill(target_source)
    # 1.3: source_id is required with whitespace:true and declares no message, so the antd default
    # wording is locale-dependent and is not asserted. What is asserted is the contract: an empty
    # and a whitespace-only value each leave the dialog open with the field marked aria-invalid and
    # put nothing on the wire.
    early_submit = dialog.get_by_role("button").filter(
        has_text=re.compile("^" + re.escape(k["register"]["confirm"]) + "$"))
    for blank in ("", "   "):
        source_id.fill(blank)
        before_blank = len(mutations)
        early_submit.click()
        expect(source_id).to_have_attribute("aria-invalid", "true")
        expect(source_id).to_have_value(blank)
        expect(dialog).to_be_visible()
        check(len(mutations) == before_blank, f"a source id of {blank!r} posted a registration")
    report["source_id_blank_refused"] = {"empty": True, "whitespace": True}
    source_id.fill(target_source)
    expect(source_id).to_have_value(target_source)
    submitted = {"source_url": fixture.url, "source_id": target_source, "name": target_name}
    # AntD may retain a decorative loading image during its leave transition;
    # anchor the button to its exact visible label, independent of the icon name.
    submit = dialog.get_by_role("button").filter(has_text=re.compile("^" + re.escape(k["register"]["confirm"]) + "$"))
    expect(submit).to_be_enabled()
    modal_geometry(dialog, submit)
    for _ in range(8):
        page.keyboard.press("Tab")
        check(dialog.evaluate("el => el.contains(document.activeElement)"), "Tab escaped registration modal")
    page.screenshot(path=artifacts.directory / "preview.png")
    report["evidence"].append({"layer": "ui", "checks": ["empty URL blocked without request", "Esc focus return", "URL normalized after preview", "metadata/defaults visible", "operator overrides retained", "Tab containment and viewport", "theme focus border"]})
    report["evidence"].append({"layer": "preview", "path": PREVIEW, "body": preview.request.post_data_json,
        "status": preview.status, "response": expected_preview, "fixture_requests": before_submit})
    if scenario == "recovery":
        report["active_evidence_layer"] = "recovery"
        fixture.set_available(False)
        held = []

        def hold_first(route):
            if route.request.method == "POST":
                held.append(route)
                return
            route.continue_()

        page.route("**" + REGISTER, hold_first)
        submit.click()
        inflight = dialog.locator(".ant-modal-footer button.ant-btn-primary")
        expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
        inflight.click()
        check(len(held) == 1, "a second click sent another register POST")
        with page.expect_response(lambda r: urlsplit(r.url).path == REGISTER and r.request.method == "POST") as rejected:
            held[0].continue_()
        page.unroute("**" + REGISTER, hold_first)
        rejection = rejected.value
        check(rejection.status == 502 and rejection.request.post_data_json == submitted
              and rejection.json()["code"] == "HTTP_DIRECTORY_UNREACHABLE", "stale preview was trusted or wrong error contract")
        expect(page.get_by_text(re.compile(re.escape(common["apiError"]["HTTP_DIRECTORY_UNREACHABLE"])))).to_be_visible()
        expect(dialog).to_be_visible()
        expect(url).to_have_value(fixture.url)
        expect(name).to_have_value(target_name)
        expect(source_id).to_have_value(target_source)
        check(model_list(page, server) == [], "failed registration partially persisted")
        failed_reads = fixture.audit()[len(before_submit):]
        check(failed_reads and all(r["status"] == 503 for r in failed_reads), "registration did not re-read unavailable directory")
        report["evidence"].append({"layer": "recovery", "status": 502, "body": rejection.json(), "fixture_requests": failed_reads,
            "inflight_posts": 1, "confirm_loading": True,
            "draft": "URL, source ID and name unchanged; zero model records"})
        fixture.set_available(True)
    report["active_evidence_layer"] = "request-and-persistence"
    read_offset = len(fixture.audit())
    expect(submit).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == REGISTER and r.request.method == "POST") as registration:
        submit.click()
    response = registration.value
    check(response.status == 201 and response.request.post_data_json == submitted and not urlsplit(response.url).query, "registration request/status mismatch")
    created = response.json()
    report["owned_model_id"] = created["id"]
    expect(dialog).not_to_be_visible()
    path = MODELS + "/" + str(created["id"])
    read = page.request.get(server.base + path)
    check(read.status == 200, "persisted model detail unavailable")
    persisted = read.json()
    for key, value in {"source": "url", **submitted, "revision": "", "arch": "Qwen3ForCausalLM", "precision": "bfloat16",
                       "context_length": 40960, "weight_gib": len(fixture.files["model.safetensors"]) / (1024 ** 3)}.items():
        check(persisted.get(key) == value and created.get(key) == value, f"persisted {key} differs from authority")
    check(not persisted.get("nodes") and not persisted.get("storages"), "registration invented a local model copy")
    check(len(model_list(page, server)) == 1, "registration duplicated model")
    final_reads = fixture.audit()[read_offset:]
    check(any(r["method"] == "GET" and r["path"] == fixture.root + "/config.json" and r["status"] == 200 for r in final_reads), "registration reused stale config")
    page.reload()
    row = page.get_by_role("row").filter(has_text=target_name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(target_source)
    detail_trigger = button(row, target_name)
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "GET") as detail_response:
        detail_trigger.click()
    check(detail_response.value.status == 200, "browser detail GET failed")
    detail = page.get_by_role("dialog", name=k["detail"]["title"], exact=True)
    expect(detail).to_be_in_viewport()
    report["last_step"] = "register-detail-heading"
    heading = detail.locator("h4.ant-typography")
    expect(heading).to_have_count(1, timeout=12000)
    expect(heading).to_have_text(target_name, timeout=12000)
    report["register_detail_heading"] = True
    expect(detail.get_by_text("Qwen3ForCausalLM", exact=True)).to_be_visible()
    expect(detail.get_by_text("bfloat16", exact=True)).to_be_visible()
    expect(detail.get_by_text("40,960", exact=True)).to_be_visible()
    page.wait_for_function("el => el.contains(document.activeElement)", arg=detail.element_handle())
    page.keyboard.press("Escape")
    expect(detail).not_to_be_visible()
    expect(detail_trigger).to_be_focused()
    report["evidence"].extend([{"layer": "request", "path": REGISTER, "body": submitted, "status": 201},
        {"layer": "persistence", "detail": persisted, "reload": "one row; detail architecture/precision/context match"},
        {"layer": "source", "fixture_requests": final_reads, "limit": "Directory metadata read; no agent/download/GPU execution"}])
    report["active_evidence_layer"] = "cleanup"
    confirmation = k["delete"]["confirm"].replace("{{name}}", target_name)

    def open_model_delete():
        button(row, k["action"]["more"]).click()
        page.get_by_role("menu").get_by_text(k["action"]["deleteModel"], exact=True).click()
        deletion = page.get_by_role("dialog", name=confirmation, exact=True)
        expect(deletion).to_be_visible()
        expect(deletion.get_by_text(k["delete"]["listDescription"], exact=True)).to_be_visible()
        return deletion

    deletion = open_model_delete()
    modal_geometry(deletion, button(deletion, k["action"]["delete"]))
    button(deletion, k["action"]["cancel"]).click()
    expect(deletion).not_to_be_visible()
    check(not any(m["method"] == "DELETE" for m in mutations), "cancel sent DELETE")
    check(page.request.get(server.base + path).status == 200, "cancel removed model")
    place_model_reference(server, created["id"])
    deletion = open_model_delete()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as blocked:
        button(deletion, k["action"]["delete"]).click()
    blocked_response = blocked.value
    check(blocked_response.status == 409, f"reference block status {blocked_response.status} {blocked_response.text()}")
    blocked_body = blocked_response.json()
    check(blocked_body.get("code") == "MODEL_IN_USE", f"reference block code {blocked_body}")
    check(blocked_response.request.post_data is None and not urlsplit(blocked_response.url).query,
          "reference block sent a body or query")
    blocked_reason = common["apiError"]["MODEL_IN_USE"].split("{{")[0].strip()
    expect(page.get_by_text(re.compile(re.escape(blocked_reason))).first).to_be_visible()
    expect(deletion).not_to_be_visible()
    check(page.request.get(server.base + path).status == 200, "reference block deleted the model")
    check(model_db_count(server, "SELECT COUNT(*) FROM deployments WHERE model_id = ?", (created["id"],)) == 1,
          "reference block removed the deployment")
    report["last_step"] = "model-delete-reference-block"
    clear_model_reference(server, created["id"])
    place_deleting_file(server, created["id"])
    deletion = open_model_delete()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as pending:
        button(deletion, k["action"]["delete"]).click()
    pending_response = pending.value
    pending_body = pending_response.json()
    check(pending_response.status == 409 and "cache deletion is awaiting confirmation" in pending_body.get("error", ""),
          f"deleting file did not keep the model {pending_response.status} {pending_body}")
    expect(page.get_by_text(re.compile(r"cache deletion is awaiting confirmation")).first).to_be_visible()
    expect(deletion).not_to_be_visible()
    check(page.request.get(server.base + path).status == 200, "cache-deletion refusal removed the model")
    check(model_db_count(server, "SELECT COUNT(*) FROM model_files WHERE model_id = ? AND state = ?",
                         (created["id"], "deleting")) == 1, "cache-deletion refusal removed the file")
    report["last_step"] = "model-delete-cache-pending"
    clear_deleting_file(server, created["id"])
    deletion = open_model_delete()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as deleted:
        button(deletion, k["action"]["delete"]).click()
    check(deleted.value.status == 204 and deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query, "delete contract mismatch")
    expect(row).to_have_count(0)
    check(page.request.get(server.base + path).status == 404 and model_list(page, server) == [], "deleted resource remains reachable")
    page.reload()
    expect(page.get_by_role("row").filter(has_text=target_name)).to_have_count(0)
    report["last_step"] = "register-minimum-create"
    trigger = button(page, k["action"]["register"]).first
    trigger.click()
    dialog = page.get_by_role("dialog", name=k["action"]["register"], exact=True)
    expect(dialog).to_be_visible()
    dialog.get_by_label(k["column"]["source"], exact=True).click()
    page.get_by_text(k["source"]["url"], exact=True).click()
    expect(dialog).to_have_css("width", "720px", timeout=12000)
    url = dialog.get_by_label(k["register"]["directoryURL"], exact=True)
    url.fill(fixture.url + "/")
    with page.expect_response(lambda r: urlsplit(r.url).path == PREVIEW and r.request.method == "POST") as minimum_preview:
        button(dialog, k["register"]["parseDirectory"]).click()
    check(minimum_preview.value.status == 200, "minimum preview failed")
    name = dialog.get_by_label(k["column"]["name"], exact=True)
    source_id = dialog.get_by_label(k["register"]["modelID"], exact=True)
    expect(name).to_have_value("tiny-model")
    expect(source_id).to_have_value(expected_preview["source_id"])
    name.fill("")
    expect(name).to_have_value("")
    minimum_body = {"source_url": fixture.url, "source_id": expected_preview["source_id"], "name": ""}
    submit = dialog.get_by_role("button").filter(has_text=re.compile("^" + re.escape(k["register"]["confirm"]) + "$"))
    with page.expect_response(lambda r: urlsplit(r.url).path == REGISTER and r.request.method == "POST") as minimum_registration:
        submit.click()
    minimum_response = minimum_registration.value
    check(minimum_response.status == 201 and minimum_response.request.post_data_json == minimum_body, "minimum registration body mismatch")
    created_min = minimum_response.json()
    min_path = MODELS + "/" + str(created_min["id"])
    report["owned_model_id"] = created_min["id"]
    minimum_read = page.request.get(server.base + min_path)
    persisted_min = minimum_read.json()
    check(minimum_read.status == 200 and persisted_min.get("name") == "tiny-model" and created_min.get("name") == "tiny-model", "minimum name default mismatch")
    for key, value in {"source": "url", "source_url": fixture.url, "source_id": expected_preview["source_id"], "revision": "",
                       "arch": "Qwen3ForCausalLM", "precision": "bfloat16", "context_length": 40960,
                       "weight_gib": len(fixture.files["model.safetensors"]) / (1024 ** 3)}.items():
        check(persisted_min.get(key) == value and created_min.get(key) == value, f"minimum persisted {key} differs")
    check(not persisted_min.get("nodes") and not persisted_min.get("storages"), "minimum registration invented a local copy")
    page.reload()
    minimum_row = page.get_by_role("row").filter(has_text="tiny-model")
    expect(minimum_row).to_have_count(1)
    detail_trigger = button(minimum_row, "tiny-model")
    with page.expect_response(lambda r: urlsplit(r.url).path == min_path and r.request.method == "GET") as minimum_detail:
        detail_trigger.click()
    check(minimum_detail.value.status == 200 and minimum_detail.value.json().get("name") == "tiny-model", "minimum detail mismatch")
    detail = page.get_by_role("dialog", name=k["detail"]["title"], exact=True)
    expect(detail.locator("h4.ant-typography")).to_have_text("tiny-model", timeout=12000)
    expect(detail.get_by_text("Qwen3ForCausalLM", exact=True)).to_be_visible()
    page.wait_for_function("el => el.contains(document.activeElement)", arg=detail.element_handle())
    page.keyboard.press("Escape")
    expect(detail).not_to_be_visible()
    confirmation = k["delete"]["confirm"].replace("{{name}}", "tiny-model")
    button(minimum_row, k["action"]["more"]).click()
    page.get_by_role("menu").get_by_text(k["action"]["deleteModel"], exact=True).click()
    deletion = page.get_by_role("dialog", name=confirmation, exact=True)
    with page.expect_response(lambda r: urlsplit(r.url).path == min_path and r.request.method == "DELETE") as minimum_deleted:
        button(deletion, k["action"]["delete"]).click()
    check(minimum_deleted.value.status == 204, "minimum delete failed")
    page.reload()
    expect(page.get_by_role("row").filter(has_text="tiny-model")).to_have_count(0)
    check(page.request.get(server.base + min_path).status == 404, "minimum model remains")
    report["owned_model_id"] = None
    report["register_minimum_create"] = True
    check(mutations == [{"method": "POST", "path": PREVIEW}] + [{"method": "POST", "path": REGISTER}] * (2 if scenario == "recovery" else 1) + [{"method": "DELETE", "path": path}] * 3 + [{"method": "POST", "path": PREVIEW}, {"method": "POST", "path": REGISTER}, {"method": "DELETE", "path": min_path}], "unexpected/duplicate mutation")
    refused = [{"path": path, "status": 409}, {"path": path, "status": 409}]
    expected_failures = ([{"path": REGISTER, "status": 502}] if scenario == "recovery" else []) + refused
    check(failures == expected_failures and not errors, "unexpected browser request failure or exception")
    if scenario == "recovery":
        check(len(console_errors) == 3 and console_errors[0]["path"] == REGISTER and "502" in console_errors[0]["text"],
              f"unexpected browser console error {console_errors}")
        delete_console = console_errors[1:]
    else:
        delete_console = console_errors
        check(len(delete_console) == 2, f"unexpected browser console error {console_errors}")
    check(all(item["path"] == path and "409" in item["text"] for item in delete_console),
          f"delete console errors {delete_console}")
    check(page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), "page has horizontal overflow")
    report["browser_errors"] = {"pageerrors": errors, "responses": failures, "console": console_errors}
    report["owned_model_id"] = None
    report["evidence"].append({"layer": "cleanup", "ui_delete": 204, "get_after_delete": 404, "remaining_models": 0,
        "scope": "Only the run-owned model; isolated server and HTTP fixture stopped by runner"})


def run_case(args, scenario):
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts((args.artifact_dir / scenario if args.case == "all" else args.artifact_dir)
                          if args.artifact_dir else ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    fixture = ModelDirectory(run_id).start()
    manifest_path = ROOT / "test/e2e/coverage/http-model-registration.json"
    manifest = json.loads(manifest_path.read_text())
    contract = manifest["cases"][scenario]
    report = {"run_id": run_id, "status": "NOT_RUN", "contract": contract["id"], "case": contract["name"],
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite-http-directory", "identity": "admin", "locale": args.locale,
        "timezone": "UTC", "viewport": {"width": 1440, "height": 900}, "retries": 0, "evidence": [],
        "not_covered": manifest["missing"], "command": ["python3", "test/e2e/http_model_registration.py", "--case", scenario, "--locale", args.locale],
        "source_hashes": {name: digest(ROOT / name) for name in ["test/e2e/http_model_registration.py", "test/e2e/http_model_fixture.py",
            "test/e2e/local_browser_fixture.py", "test/e2e/platform_token_read_revoke.py", "test/e2e/dialog_checks.py",
            "test/e2e/coverage/http-model-registration.json", "test/e2e/http_register_inventory.py"]}}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_model_registration(page, context, server, fixture, artifacts, report, args.locale, scenario)
                check(set(contract["required_evidence"]) <= {item["layer"] for item in report["evidence"]}, "missing required evidence")
                check(all(digest(ROOT / name) == value for name, value in report["source_hashes"].items()), "source changed during execution")
                report["status"] = "VERIFYING_ARTIFACTS"
            except Exception as error:
                report["exception_type"] = type(error).__name__
                report["failure_attribution"] = "UNTRIAGED: FAIL_TEST denotes execution failure; inspect evidence layer to distinguish product and test defects"
                report["status"] = "FAIL_TEST"
                report["failure"] = traceback.format_exc()
                if report.get("preflight") == "PASS":
                    page.screenshot(path=artifacts.directory / "failure.png")
                    (artifacts.directory / "failure-aria.txt").write_text(artifacts.redact(page.locator("body").aria_snapshot()))
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        report["fixture_requests"] = fixture.audit()
        report["server_stopped"] = server.stop()
        report["fixture_stopped"] = fixture.stop()
        if not report["server_stopped"] or not report["fixture_stopped"]:
            report["status"] = "FAIL_TEST"
        report["private_state"] = "Isolated DB/raw log retained under private/; no process retained. Owner removes scoped directory after diagnosis within seven days."
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
            if report["status"] == "VERIFYING_ARTIFACTS":
                report["status"] = "PASS"
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; do not upload"
        artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": report["contract"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return report["status"] == "PASS"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--case", choices=["register", "recovery", "all"], default="all")
    args = parser.parse_args()
    os.umask(0o077)
    results = [run_case(args, scenario) for scenario in (["register", "recovery"] if args.case == "all" else [args.case])]
    return 0 if all(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
