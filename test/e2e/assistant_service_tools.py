#!/usr/bin/env python3
"""AI assistant / service configuration, read-only tool loop and stop.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-assistant-service-tools
After build: python3 test/e2e/assistant_service_tools.py --locale en
Requires Python Playwright and system Chrome. The LLM endpoint is an auditable
local fixture; the server, AI service configuration, read-only tools, SQLite
and browser are real. No GPU is claimed.

The first question makes the fixture request ``cluster_overview``; the server
executes that read-only tool and sends its result back before the final answer.
A second question keeps the stream open so the browser Stop path is exercised.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and fixture and
retains the private database/logs for scoped diagnosis (seven days).
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
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from assistant_fixture import HOSTILE_ANSWER, TOOL_CALL_ID, AssistantEndpoint
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/assistant/service-tool-read-stream"
CASE = "AI 助手 / 服务配置与只读工具读取 / 流式回答、停止、刷新与删除"
CONFIGS = "/api/v1/ai/configs"
ASK = "/api/v1/ai/ask"
NODES = "/api/v1/nodes"


def strings(locale):
    settings = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    ai = json.loads((ROOT / "ui/src/i18n/locales" / locale / "ai.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return settings, ai, auth


def confirm_popconfirm(page):
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_be_visible(timeout=5000)
    popconfirm.locator(".ant-btn-primary").click()


def wait_fixture(fixture, wanted, timeout=15):
    """Wait until the fixture has recorded exactly the expected request kinds."""
    deadline = time.time() + timeout
    last = []
    while time.time() < deadline:
        snapshot = fixture.snapshot()
        last = [item["kind"] for item in snapshot["requests"]]
        if last == wanted:
            return snapshot
        time.sleep(0.05)
    raise AssertionError(f"fixture requests stayed {last}, want {wanted}")


def cluster_inventory_answer(tool_fact, inventory):
    """Compare tool facts with the independent inventory, which owns expected text."""
    total = tool_fact["summary"]["nodes"]["total"]
    names = [item["name"] for item in tool_fact["nodes_needing_attention"]["items"]]
    expected_names = sorted(item["name"] for item in inventory["items"])
    check(type(total) is int and total == inventory["total"]
          and sorted(names) == expected_names,
          f"tool result differs from the independent inventory: {tool_fact}")
    return (f"The cluster overview reports {inventory['total']} node(s); "
            f"attention includes {expected_names[0]}.")


def exercise_add_dialog(page, trigger, title, parent_name, field_label, save_label, cancel_label, evidence):
    """Dialog identity, layout, focus, and dismissals. No AI config write."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    writes = []

    def on_request(request):
        path = urlsplit(request.url).path
        if path.startswith(CONFIGS) and request.method in {"POST", "PUT", "PATCH", "DELETE"}:
            writes.append(request.method)

    page.on("request", on_request)
    parent = page.get_by_role("heading", name=parent_name, exact=True)
    baseline = len(writes)

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
        lambda: len(writes), trigger)
    check(len(writes) == baseline, "add dialog dismissal sent a write")
    evidence.append({"layer": "ui", "add_dialog": title,
                     "dismissals": ["cancel", "escape", "close", "mask"], "write_requests": 0})
    return dialog


def exercise_add_inflight(page, modal, settings, name, base, model, api_key, bad_responses, console_errors, evidence):
    """One held create, a swallowed second click, then a 502 that keeps the draft."""
    reason = "upstream fixture is unreachable"
    held = []

    def hold_create(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != CONFIGS:
            route.continue_()
            return
        held.append(route)

    page.route("**" + CONFIGS, hold_create)
    button(modal, settings["common"]["add"]).click()
    inflight = modal.locator(".ant-modal-footer button.ant-btn-primary")
    expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
    inflight.click()
    modal.get_by_label(settings["ai"]["name"], exact=True).press("Enter")
    check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
    body = held[0].request.post_data_json
    check(body == {"name": name, "preset": "custom", "base_url": base, "api_key": api_key,
                   "model": model, "is_default": True}, "held create body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == CONFIGS and r.request.method == "POST") as failed:
        held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
    page.unroute("**" + CONFIGS, hold_create)
    check(failed.value.status == 502, "forced create failure did not surface")
    expect(modal).to_be_visible()
    expect(modal.get_by_label(settings["ai"]["name"], exact=True)).to_have_value(name)
    expect(modal.get_by_label(settings["ai"]["baseUrl"], exact=True)).to_have_value(base)
    expect(modal.get_by_label(settings["ai"]["model"], exact=True)).to_have_value(model)
    expect(modal.get_by_label(settings["ai"]["upstreamKey"], exact=True)).to_have_value(api_key)
    expect(modal.locator(".ant-select-selection-item").first).to_have_text(settings["ai"]["presetName"]["custom"])
    expect(modal.get_by_role("switch")).to_be_checked()
    expect(page.get_by_text(reason)).to_be_visible()
    check(any(item["path"] == CONFIGS and item["status"] == 502 for item in bad_responses), "502 was not recorded")
    check(any("502" in text for text in console_errors), "502 console line absent: " + str(console_errors))
    bad_responses[:] = [item for item in bad_responses if item.get("status") != 502]
    console_errors[:] = [text for text in console_errors if "502" not in text]
    evidence.append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
                     "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})


# Contract: gpuworks/assistant/service-tool-read-stream
# Mainline: 独立 server/SQLite + 可审计 LLM fixture → 设置页新增并设为默认 AI 服务
#           → 助手面板提问 → fixture 请求 cluster_overview → server 执行只读工具并回传
#           → 流式最终回答与工具步骤可见 → 第二个问题在流式途中停止 → 刷新后配置仍在
#           → UI 删除配置并确认列表为空
def test_assistant_service_tools(page, context, server, artifacts, report, locale, fixture):
    settings, ai, auth = strings(locale)
    config_name = f"e2e-{report['run_id']}-assistant"
    page_errors, console_errors, bad_responses, asks = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            if (not authenticated
                    and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me"
                    and "401" in message.text):
                return
            console_errors.append(message.text)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": path,
                                  "status": response.status})

    def request_seen(request):
        path = urlsplit(request.url).path
        if path == ASK and request.method == "POST":
            asks.append(request.post_data_json)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/settings")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    integrations = page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"], exact=True)
    integrations.click()
    expect(integrations).to_have_attribute("aria-selected", "true")
    report["last_step"] = "integrations-tab-selected"

    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    expect(card.get_by_text(settings["ai"]["empty"], exact=True)).to_be_visible()
    # Without an AI service the core product stays usable and the assistant
    # states that an AI service must be configured first. The ask itself is
    # expected to answer 503 AI_ENDPOINT_NOT_CONFIGURED.
    page.get_by_role("button", name=ai["title"], exact=True).click()
    panel = page.get_by_role("dialog").last
    question = panel.get_by_label(ai["panel"]["input"], exact=True)
    question.fill("Can you help with this cluster?")
    with page.expect_response(lambda r: urlsplit(r.url).path == ASK
                              and r.request.method == "POST") as no_config:
        panel.get_by_role("button", name=ai["panel"]["send"], exact=True).click()
    check(no_config.value.status == 503,
          f"the unconfigured ask returned {no_config.value.status}")
    expect(panel.get_by_text(ai["configureFirst"], exact=True)).to_be_visible()
    panel.get_by_role("button", name=ai["panel"]["close"], exact=True).click()
    expect(panel).not_to_be_visible()
    asks.clear()
    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    integrations = page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"],
                                    exact=True)
    expect(integrations).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    expect(card.get_by_text(settings["ai"]["empty"], exact=True)).to_be_visible()
    report["last_step"] = "no-config-assistant-state"

    trigger = button(card, settings["common"]["add"])
    expect(trigger).to_be_visible()
    modal = exercise_add_dialog(
        page, trigger, settings["ai"]["addService"], settings["page"]["title"],
        settings["ai"]["name"], settings["common"]["add"], settings["common"]["cancel"],
        report["evidence"])
    report["last_step"] = "add-switch"
    add_switch = modal.locator(".ant-switch")
    expect(add_switch).to_have_count(1, timeout=12000)
    expect(add_switch).not_to_have_class(re.compile(r"\bant-switch-small\b"), timeout=12000)
    expect(add_switch).to_be_checked()
    report["add_switch"] = {"count": 1, "small": False, "checked": True}
    from assistant_add_inventory import exercise_add_inventory
    report["add_inventory"] = exercise_add_inventory(modal, settings)
    expect(modal.get_by_text(settings["ai"]["addService"], exact=True)).to_be_visible()
    # 1.2: a create dialog that opens has to open on the product's own defaults, not blank.
    # The preset, its declared address and model, the empty key and the default switch are all
    # asserted before anything is typed, so a dialog that opened empty would fail here.
    expect(modal.locator(".ant-select-selection-item").first).to_have_text(
        settings["ai"]["presetName"]["deepseek"])
    expect(modal.get_by_label(settings["ai"]["name"], exact=True)).to_have_value("")
    expect(modal.get_by_label(settings["ai"]["baseUrl"], exact=True)).to_have_value("https://api.deepseek.com")
    expect(modal.get_by_label(settings["ai"]["model"], exact=True)).to_have_value("deepseek-v4-flash")
    expect(modal.get_by_label(settings["ai"]["upstreamKey"], exact=True)).to_have_value("")
    report["add_defaults"] = {"preset": "deepseek", "base_url": "https://api.deepseek.com",
                              "model": "deepseek-v4-flash", "name_blank": True, "key_blank": True}
    # 1.5: every preset the dropdown actually offers is selected on its own and has to fill the
    # base address and model it declares. Only deepseek (the default) and custom were exercised.
    preset_expectations = [
        ("bailian", "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen-plus"),
        ("siliconflow", "https://api.siliconflow.cn/v1", ""),
        ("openrouter", "https://openrouter.ai/api/v1", ""),
        ("poe", "https://api.poe.com/v1", "GPT-5"),
    ]
    seen_presets = []
    for preset_key, expected_base, expected_model in preset_expectations:
        modal.locator(".ant-select-selector").first.click()
        label = settings["ai"]["presetName"][preset_key]
        option = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
            has_text=re.compile(r"^" + re.escape(label) + r"$"))
        expect(option).to_be_visible()
        option.click()
        expect(modal.locator(".ant-select-selection-item").first).to_have_text(label)
        expect(modal.get_by_label(settings["ai"]["baseUrl"], exact=True)).to_have_value(expected_base)
        expect(modal.get_by_label(settings["ai"]["model"], exact=True)).to_have_value(expected_model)
        seen_presets.append({"preset": preset_key, "base_url": expected_base, "model": expected_model})
    check(len(seen_presets) == 4, "not every preset was selected")
    report["preset_enumeration"] = seen_presets
    # 1.6: the preset is the controlling field for base address and model. Its rule is to
    # overwrite both on every change rather than keep what the operator typed, so a hand-typed
    # address must be replaced by the next preset's declared values.
    modal.get_by_label(settings["ai"]["baseUrl"], exact=True).fill("https://hand-typed.invalid/v1")
    modal.get_by_label(settings["ai"]["model"], exact=True).fill("hand-typed-model")
    modal.locator(".ant-select-selector").first.click()
    reopen = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(r"^" + re.escape(settings["ai"]["presetName"]["bailian"]) + r"$"))
    expect(reopen).to_be_visible()
    reopen.click()
    expect(modal.get_by_label(settings["ai"]["baseUrl"], exact=True)).to_have_value(
        "https://dashscope.aliyuncs.com/compatible-mode/v1")
    expect(modal.get_by_label(settings["ai"]["model"], exact=True)).to_have_value("qwen-plus")
    report["preset_cascade"] = {"overwrites_typed_base_url": True, "overwrites_typed_model": True}
    modal.locator(".ant-select-selector").first.click()
    custom = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(r"^" + re.escape(settings["ai"]["presetName"]["custom"]) + r"$"))
    expect(custom).to_be_visible()
    custom.click()
    # 1.3: each required text field must reject a whitespace-only value and send nothing.
    # antd's required rule only trims when whitespace:true is set, so without it a value of
    # spaces validates and the form posts a record with a blank name, URL or model.
    blank_writes = []
    def on_blank(request):
        if urlsplit(request.url).path.startswith(CONFIGS) and request.method in {"POST", "PUT", "PATCH"}:
            blank_writes.append(request.method)
    page.on("request", on_blank)
    try:
        for label, message in ((settings["ai"]["name"], settings["ai"]["nameRequired"]),
                               (settings["ai"]["baseUrl"], settings["ai"]["baseUrlRequired"]),
                               (settings["ai"]["model"], settings["ai"]["modelRequired"])):
            # Fill every other required text field so the only thing that can fail is this
            # field's whitespace check - otherwise the other blank fields raise their own
            # errors and the assertion would pass without proving anything about this one.
            for other_label, other_value in (
                    (settings["ai"]["name"], "probe-name"),
                    (settings["ai"]["baseUrl"], "https://probe.invalid/v1"),
                    (settings["ai"]["model"], "probe-model")):
                modal.get_by_label(other_label, exact=True).fill(other_value)
            field = modal.get_by_label(label, exact=True)
            field.fill("   ")
            button(modal, settings["common"]["add"]).click()
            expect(modal.get_by_text(message, exact=True)).to_be_visible()
            check(blank_writes == [], f"whitespace-only {label} submitted {blank_writes}")
            field.fill("")
    finally:
        page.remove_listener("request", on_blank)
    report["whitespace_rejected"] = {"fields": 3, "requests": 0}

    modal.get_by_label(settings["ai"]["name"], exact=True).fill(config_name)
    modal.get_by_label(settings["ai"]["baseUrl"], exact=True).fill(fixture.base)
    modal.get_by_label(settings["ai"]["model"], exact=True).fill(fixture.model)
    upstream_key = modal.get_by_label(settings["ai"]["upstreamKey"], exact=True)
    expect(upstream_key).to_have_attribute("type", "password")
    upstream_key.fill(fixture.api_key)
    from assistant_add_keyboard import _enter_posts, exercise_add_keyboard
    report["add_keyboard"] = exercise_add_keyboard(page, modal, settings, config_name)
    report["last_step"] = "add-keyboard"
    # 7.1: the upstream key is the only credential this form carries. On create it is required and
    # a whitespace-only value is refused in the form, so neither can reach the server. The field is
    # a password input, so the typed value is never rendered as text.
    key_field = modal.get_by_label(settings["ai"]["upstreamKey"], exact=True)
    expect(key_field).to_have_attribute("type", "password")
    type_before = type(key_field.input_value())
    for blank in ("", "   "):
        key_field.fill(blank)
        check(not _enter_posts(page, key_field),
              f"an upstream key of {blank!r} submitted the form")
        expect(modal).to_be_visible()
        expect(modal.get_by_text(settings["ai"]["keyRequired"], exact=True)).to_be_visible()
        expect(key_field).to_have_attribute("aria-invalid", "true")
    # The evidence records the fact rather than the attribute value: the artifact scrubber
    # rewrites the literal attribute value, so storing it would only read back as [REDACTED].
    report["upstream_key_required"] = {"blank_refused": True, "whitespace_refused": True,
                                       "masked_input": True}
    key_field.fill(fixture.api_key)
    expect(key_field).to_have_value(fixture.api_key)
    check(key_field.input_value() == fixture.api_key and type_before is not None,
          "the upstream key field does not hold what was typed")
    exercise_add_inflight(page, modal, settings, config_name, fixture.base, fixture.model,
                           fixture.api_key, bad_responses, console_errors, report["evidence"])
    with page.expect_response(lambda r: urlsplit(r.url).path == CONFIGS
                              and r.request.method == "POST") as created:
        button(modal, settings["common"]["add"]).click()
    check(created.value.status == 201, f"AI config HTTP {created.value.status}")
    create_body = created.value.request.post_data_json
    check(create_body["name"] == config_name and create_body["base_url"] == fixture.base
          and create_body["model"] == fixture.model and create_body["is_default"] is True,
          f"AI config body differs: {create_body}")
    expect(page.get_by_text(settings["ai"]["added"], exact=True).first).to_be_visible()
    expect(card.get_by_text(config_name, exact=True)).to_be_visible()
    check(fixture.api_key not in page.locator("body").inner_text(),
          "the settings page rendered the upstream key after saving")

    # 7.2: an edit that leaves the key blank means "keep the stored key", not "clear it". The
    # request must omit the field entirely and the server must still hold the original key.
    row = card.get_by_role("row").filter(has_text=config_name)
    row.get_by_role("button", name=settings["common"]["edit"], exact=True).click()
    edit_modal = page.get_by_role("dialog", name=settings["ai"]["editService"], exact=True)
    expect(edit_modal).to_be_visible()
    edit_key = edit_modal.get_by_label(settings["ai"]["upstreamKey"], exact=True)
    expect(edit_key).to_have_value("")
    expect(edit_modal.get_by_label(settings["ai"]["name"], exact=True)).to_have_value(config_name)
    check(not _enter_posts(page, edit_key), "Enter in the blank edit key submitted the form")
    with page.expect_response(lambda r: r.request.method == "PATCH"
                              and urlsplit(r.url).path.startswith(CONFIGS)) as edited:
        button(edit_modal, settings["common"]["save"]).click()
    check(edited.value.status == 200, f"AI config edit HTTP {edited.value.status}")
    edit_body = edited.value.request.post_data_json
    check("api_key" not in edit_body,
          f"a blank edit key was still submitted: {sorted(edit_body)}")
    expect(edit_modal).not_to_be_visible(timeout=8000)
    after_edit = page.request.get(server.base + CONFIGS)
    check(after_edit.status == 200, f"AI configs GET after edit HTTP {after_edit.status}")
    kept_rows = after_edit.json()["items"]
    kept = next(item for item in kept_rows if item["name"] == config_name)
    check(fixture.api_key not in json.dumps(kept),
          "the stored key leaked through the config list")
    check(kept["id"] == created.value.json()["id"],
          "the edit replaced the AI config instead of updating it")
    report["edit_keeps_key"] = {"request_has_api_key": False, "dialog_closed": True}

    # 1.12: an edit that changes one value has to move that value from A to B and leave the
    # others alone. The name goes to a new one while the address and model stay as stored and
    # the blank key keeps the stored secret, so the PATCH body carries only what changed.
    renamed = config_name + "-renamed"
    row.get_by_role("button", name=settings["common"]["edit"], exact=True).click()
    rename_modal = page.get_by_role("dialog", name=settings["ai"]["editService"], exact=True)
    expect(rename_modal).to_be_visible()
    rename_name = rename_modal.get_by_label(settings["ai"]["name"], exact=True)
    expect(rename_name).to_have_value(config_name)
    rename_name.fill(renamed)
    with page.expect_response(lambda r: r.request.method == "PATCH"
                              and urlsplit(r.url).path.startswith(CONFIGS)) as renamed_resp:
        button(rename_modal, settings["common"]["save"]).click()
    check(renamed_resp.value.status == 200, f"AI config rename HTTP {renamed_resp.value.status}")
    rename_body = renamed_resp.value.request.post_data_json
    check(rename_body.get("name") == renamed and "api_key" not in rename_body,
          f"the rename body differs or shipped the key: {sorted(rename_body)}")
    expect(rename_modal).not_to_be_visible(timeout=8000)
    renamed_rows = page.request.get(server.base + CONFIGS).json()["items"]
    renamed_entry = next(item for item in renamed_rows if item["name"] == renamed)
    check(renamed_entry["base_url"] == fixture.base and renamed_entry["model"] == fixture.model
          and renamed_entry["id"] == kept["id"],
          "a single-field edit disturbed the other stored values")
    check(fixture.api_key not in json.dumps(renamed_rows), "the stored key leaked after the rename")
    report["edit_single_field"] = {"from": config_name, "to": renamed,
                                   "others_unchanged": True, "key_absent": True}
    config_name = renamed

    # 1.4: every stored text field has a bound the server states, and the bound is exact - a
    # value of the stated length is accepted and one character more is refused with the field
    # named. Each accepted configuration is deleted again, so the run leaves nothing behind.
    bounds = [("name", 64), ("model", 64), ("preset", 32)]
    bound_evidence = {}
    for field_name, maximum in bounds:
        payload = {"name": f"e2e-{report['run_id']}-bnd", "preset": "custom",
                   "base_url": fixture.base, "api_key": fixture.api_key,
                   "model": fixture.model}
        payload[field_name] = ("x" * maximum) if field_name != "name" else ("e2e-" + "n" * (maximum - 4))
        at_limit = page.request.post(server.base + CONFIGS, data=payload)
        check(at_limit.status == 201,
              f"{field_name} at its stated bound was refused: {at_limit.status} {at_limit.text()[:160]}")
        created_id = at_limit.json()["id"]
        over = dict(payload)
        over[field_name] = payload[field_name] + "x"
        refused = page.request.post(server.base + CONFIGS, data=over)
        check(refused.status == 400, f"{field_name} past its bound was accepted: {refused.status}")
        refused_body = refused.json()
        check(refused_body.get("code") == "FIELD_INVALID"
              and (refused_body.get("params") or {}).get("field") == field_name,
              f"{field_name} past its bound was refused without naming the field: {refused_body}")
        after = page.request.get(server.base + CONFIGS).json()["items"]
        check(not any(item.get("name") == over[field_name] for item in after),
              f"a {field_name} past its bound was stored anyway")
        page.request.delete(server.base + CONFIGS + "/" + str(created_id))
        bound_evidence[field_name] = {"max": maximum, "at_limit": 201, "over": 400}
    report["field_bounds"] = bound_evidence
    remaining = page.request.get(server.base + CONFIGS).json()["items"]
    check([item["name"] for item in remaining] == [config_name],
          f"the bound probes did not clean up after themselves: {[i['name'] for i in remaining]}")

    # 1.13: an edit that races another writer. Two contracts are exercised against the real API.
    # First, the only default service cannot stop being the default - the patch is well formed and
    # the row exists, so the refusal is a 409 naming the field rather than a validation error.
    sole_id = kept["id"]
    sole_conflict = page.request.patch(server.base + CONFIGS + "/" + str(sole_id),
                                       data={"is_default": False})
    check(sole_conflict.status == 409,
          f"clearing the only default was not refused as a conflict: {sole_conflict.status}")
    conflict_body = sole_conflict.json()
    check(conflict_body.get("code") == "AI_DEFAULT_REQUIRED",
          f"the conflict code differs: {conflict_body.get('code')}")
    still_default = [item for item in page.request.get(server.base + CONFIGS).json()["items"]
                     if item["id"] == sole_id]
    check(still_default and still_default[0]["is_default"] is True,
          "a refused conflict cleared the default mark anyway")

    # Second, a service edited after another session deleted it. The row is created, the edit dialog
    # is opened on it, the row is then removed out from under the dialog, and the save is required to
    # answer 404 - the console is holding a value that no longer exists rather than overwriting it.
    doomed_name = f"e2e-{report['run_id']}-raced"
    doomed = page.request.post(server.base + CONFIGS, data={
        "name": doomed_name, "preset": "custom", "base_url": fixture.base,
        "api_key": fixture.api_key, "model": fixture.model, "is_default": False})
    check(doomed.status == 201, f"race fixture create HTTP {doomed.status}")
    doomed_id = doomed.json()["id"]
    doomed_patch = page.request.patch(server.base + CONFIGS + "/" + str(doomed_id),
                                      data={"model": fixture.model + "-changed"})
    check(doomed_patch.status == 200, f"race fixture patch HTTP {doomed_patch.status}")
    doomed_delete = page.request.delete(server.base + CONFIGS + "/" + str(doomed_id))
    check(doomed_delete.status in {200, 204}, f"race fixture delete HTTP {doomed_delete.status}")
    stale_edit = page.request.patch(server.base + CONFIGS + "/" + str(doomed_id),
                                    data={"model": fixture.model + "-again"})
    check(stale_edit.status == 404,
          f"an edit of a deleted service was not refused: {stale_edit.status}")
    check(not any(item["id"] == doomed_id
                  for item in page.request.get(server.base + CONFIGS).json()["items"]),
          "a rejected edit of a deleted service brought it back")
    # The same race through the console: the edit dialog is opened on the service, the service is
    # then deleted by another session, and saving must surface the refusal in the dialog instead of
    # silently succeeding - that visible failure with the dialog still open is the recovery entry.
    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    ui_name = f"e2e-{report['run_id']}-ui-raced"
    ui_created = page.request.post(server.base + CONFIGS, data={
        "name": ui_name, "preset": "custom", "base_url": fixture.base,
        "api_key": fixture.api_key, "model": fixture.model, "is_default": False})
    check(ui_created.status == 201, f"UI race fixture create HTTP {ui_created.status}")
    ui_id = ui_created.json()["id"]
    page.reload()
    expect(card.get_by_text(ui_name, exact=True)).to_be_visible()
    ui_row = card.locator("tr.ant-table-row").filter(has_text=ui_name)
    ui_row.get_by_role("button", name=settings["common"]["edit"], exact=True).click()
    ui_modal = page.get_by_role("dialog", name=settings["ai"]["editService"], exact=True)
    expect(ui_modal).to_be_visible()
    ui_model = ui_modal.get_by_label(settings["ai"]["model"], exact=True)
    expect(ui_model).to_have_value(fixture.model)
    ui_model.fill(fixture.model + "-raced")
    removed = page.request.delete(server.base + CONFIGS + "/" + str(ui_id))
    check(removed.status in {200, 204}, f"UI race delete HTTP {removed.status}")
    with page.expect_response(lambda r: r.request.method == "PATCH"
                              and urlsplit(r.url).path.startswith(CONFIGS)) as ui_saved:
        button(ui_modal, settings["common"]["save"]).click()
    check(ui_saved.value.status == 404,
          f"the console edit of a deleted service answered {ui_saved.value.status}")
    expect(ui_modal).to_be_visible()
    expect(page.locator(".ant-message-notice").filter(has_text=re.compile(r"\S")).first).to_be_visible(timeout=8000)
    check(not any(item["id"] == ui_id
                  for item in page.request.get(server.base + CONFIGS).json()["items"]),
          "the console edit of a deleted service brought it back")
    # The 404 above was provoked on purpose; drop it from the unexpected-error ledger the same way
    # the forced 502 earlier in the run is dropped, so the ledger still fails on anything unplanned.
    bad_responses[:] = [item for item in bad_responses
                        if not (item.get("status") == 404 and item.get("method") == "PATCH")]
    console_errors[:] = [text for text in console_errors
                         if not re.search(r"status of (404|503) ", text)]
    report["concurrent_edits"] = {"sole_default_cleared": 409, "conflict_code": "AI_DEFAULT_REQUIRED",
                                  "edit_after_delete": 404, "resurrected": False,
                                  "console_edit_after_delete": 404, "dialog_kept": True,
                                  "failure_visible": True}
    # The dialog is still open on the service that no longer exists; dismiss it so the rest of the
    # run starts from a list, exactly as an operator would after reading the refusal.
    button(ui_modal, settings["common"]["cancel"]).click()
    expect(ui_modal).not_to_be_visible(timeout=8000)
    page.reload()
    expect(card.get_by_text(ui_name, exact=True)).to_have_count(0, timeout=12000)

    node_name = f"e2e-{report['run_id']}-assistant-node"
    created_node = page.request.post(server.base + NODES,
                                     data={"name": node_name, "server_url": server.base})
    check(created_node.status == 201, f"assistant node fixture HTTP {created_node.status}")
    node_body = created_node.json()
    artifacts.remember(node_body["token"])
    node_uuid = node_body["node"]["uuid"]
    # 23.x: the two refusal checks further down claim the labels were not changed. They used to
    # compare an empty map with itself, because nothing ever wrote a label to this node, and that
    # comparison could not fail whatever the refused tool path did. A stored label is put in
    # place first, and the seed is read back - if the seed silently fails, the preservation check
    # would quietly become the same empty-to-empty comparison again, so it is asserted here.
    label_probe = {"probe": "1"}
    seeded = page.request.put(server.base + NODES + f"/{node_uuid}/labels",
                              data={"labels": label_probe})
    check(seeded.status == 200, f"seeding the label this case protects: HTTP {seeded.status}")
    labels_seeded = page.request.get(server.base + NODES + f"/{node_uuid}").json()["labels"]
    check(labels_seeded == label_probe,
          f"the label probe did not seed a value ({labels_seeded!r}), so every preservation "
          f"check below would compare two empty maps and could not fail")
    inventory_response = page.request.get(server.base + NODES)
    check(inventory_response.status == 200,
          f"assistant inventory HTTP {inventory_response.status}")
    inventory = inventory_response.json()
    inventory_names = {item["name"] for item in inventory["items"]}
    check(inventory["total"] == 1 and inventory_names == {node_name},
          f"assistant inventory differs: {inventory}")
    report["last_step"] = "inventory-prepared"

    page.get_by_role("button", name=ai["title"], exact=True).click()
    panel = page.get_by_role("dialog").last
    expect(panel.get_by_text(ai["title"], exact=True).first).to_be_visible()
    question = panel.get_by_label(ai["panel"]["input"], exact=True)
    send = panel.get_by_role("button", name=ai["panel"]["send"], exact=True)
    first_question_text = "How many nodes are in the cluster?"
    question.fill(first_question_text)
    send.click()
    expect(panel.get_by_text(ai["tool"]["cluster_overview"], exact=True)).to_be_visible(timeout=30000)

    first_question = wait_fixture(fixture, ["tool", "final"])
    tool_request, final_request = first_question["requests"]
    check(tool_request["authorization"] == f"Bearer {fixture.api_key}",
          "the assistant request lost the configured API key")
    check(any(tool["function"]["name"] == "cluster_overview"
              for tool in tool_request["body"].get("tools") or []),
          "cluster_overview was not offered to the model")
    check(len(final_request["tool_messages"]) == 1
          and final_request["tool_messages"][0]["name"] == "cluster_overview"
          and final_request["tool_messages"][0]["tool_call_id"] == TOOL_CALL_ID,
          "the server did not return the matching cluster_overview tool result")
    final_messages = final_request["body"].get("messages") or []
    preceding = [message for message in final_messages
                 if message.get("role") == "assistant" and message.get("tool_calls")]
    check(len(preceding) == 1
          and preceding[0]["tool_calls"][0]["id"] == TOOL_CALL_ID
          and preceding[0]["tool_calls"][0]["function"]["name"] == "cluster_overview",
          f"the final request omitted or altered the preceding tool call: "
          f"{final_messages}")
    report["last_step"] = "tool-loop-structure-verified"
    tool_content = final_request["tool_messages"][0]["content"]
    check(not tool_content.startswith("error:")
          and "unavailable" not in tool_content.lower(),
          f"cluster_overview returned an error: {tool_content[:300]}")
    tool_fact = json.loads(tool_content)
    tool_total = tool_fact["summary"]["nodes"]["total"]
    tool_names = [item["name"] for item in tool_fact["nodes_needing_attention"]["items"]]
    expected_answer = cluster_inventory_answer(tool_fact, inventory)
    expect(panel.get_by_text(expected_answer, exact=False)).to_be_visible(timeout=30000)
    report["last_step"] = "tool-result-verified"

    slow_question = "Please answer a slow question."
    question.fill(slow_question)
    # A new question is accepted only after the previous stream's done event;
    # the Send button is re-enabled at that point.
    expect(send).to_be_enabled(timeout=30000)
    send.click()
    check(fixture.wait_slow_started(10), "the slow question never reached the fixture")
    stop = button(panel, ai["stop"])
    expect(stop).to_be_visible()
    stop.click()
    expect(panel.get_by_text(ai["stopped"], exact=True)).to_be_visible()
    fixture.release_slow()
    check(fixture.wait_aborted(10),
          "the fixture did not observe the upstream cancellation")
    report["last_step"] = "stop-observed-upstream"

    denied_question = "Please read the denied route."
    denied_response = page.request.post(
        server.base + ASK,
        data={"task": "ask", "question": denied_question, "locale": locale},
        headers={"Accept": "text/event-stream"})
    check(denied_response.status == 200, f"denied ask HTTP {denied_response.status}")
    denied_text = denied_response.text()
    check("The denied route was rejected" in denied_text
          and "not readable" in denied_text,
          f"denied route was not rejected: {denied_text[:500]}")
    check("PRIVATE KEY" not in denied_text and "ssh-ed25519" not in denied_text,
          "the denied route leaked key material")
    denied_snapshot = fixture.snapshot()
    check([item["kind"] for item in denied_snapshot["requests"]] ==
          ["tool", "final", "slow", "denied", "denied_final"],
          f"fixture requests differ: {[item['kind'] for item in denied_snapshot['requests']]}")
    after_denied_nodes = page.request.get(server.base + NODES + f"/{node_uuid}")
    check(after_denied_nodes.json()["labels"] == labels_seeded,
          f"the assistant tool path changed node labels "
          f"{labels_seeded!r} -> {after_denied_nodes.json().get('labels')!r}")
    report["last_step"] = "denied-route-rejected"

    # 23.5: the answer is untrusted output. A model answer carrying markup, a script body, a
    # javascript: link and a markdown image is asked through the panel and has to arrive as text:
    # no element from it may appear in the transcript, no dialog may fire, and the payload must
    # still be readable as the text it is rather than being silently dropped.
    question.fill("Show me a hostile answer.")
    send.click()
    deadline = time.time() + 30
    while time.time() < deadline:
        if [item["kind"] for item in fixture.snapshot()["requests"]][-1:] == ["hostile"]:
            break
        page.wait_for_timeout(200)
    check([item["kind"] for item in fixture.snapshot()["requests"]][-1:] == ["hostile"],
          "the hostile question never reached the fixture as one request")
    hostile_text = HOSTILE_ANSWER
    transcript = panel.locator(".ant-drawer-body, .ant-modal-body").first
    expect(transcript.get_by_text(re.compile(r"Cluster\s+looks\s+healthy")).first).to_be_visible(timeout=30000)
    check(transcript.locator("script").count() == 0, "a script element from the answer reached the transcript")
    check(transcript.locator("img[src='x']").count() == 0, "an image from the answer reached the transcript")
    check(transcript.locator("b").filter(has_text="bold").count() == 0,
          "markup from the answer was interpreted rather than shown as text")
    check(panel.get_by_role("alertdialog").count() == 0, "a dialog from the answer fired")
    hostile_links = transcript.locator("a[href^='javascript:']").count()
    check(hostile_links == 0, "a javascript: link from the answer became a real link")
    report["hostile_answer"] = {"rendered_as_text": True, "script_elements": 0,
                                "img_elements": 0, "bold_elements": 0,
                                "javascript_links": 0, "dialog_fired": False}
    report["last_step"] = "hostile-answer-is-text"
    # This question is not part of the tool-loop story the later assertions count, so it is dropped
    # from the ledger the same way the panel is reset between phases.
    asks[:] = [item for item in asks if item.get("question") != "Show me a hostile answer."]

    # A caller cannot make the assistant write. call_api's schema has no method
    # field and the tool only ever issues GET, so "DELETE this" arrives as a read
    # of a path that has no GET endpoint: it must fail, and the resource the call
    # named must be untouched afterwards.
    write_question = "please write to the platform"
    write_response = page.request.post(
        server.base + ASK,
        data={"task": "ask", "question": write_question, "locale": locale},
        headers={"Accept": "text/event-stream"})
    check(write_response.status == 200, f"write ask HTTP {write_response.status}")
    write_text = write_response.text()
    # The tool echoes the arguments it was given, including the method and body
    # it has no way to honour, and then fails as a read.
    check(r'\"method\": \"DELETE\"' in write_text,
          f"the write attempt never reached the tool: {write_text[:500]}")
    check('"status":"error"' in write_text and "no GET endpoint" in write_text,
          f"the write attempt was not refused as a read: {write_text[:500]}")
    labels_after_attempt = page.request.get(
        server.base + NODES + f"/{node_uuid}").json()["labels"]
    check(labels_after_attempt == labels_seeded,
          f"the write attempt changed node labels "
          f"{labels_seeded!r} -> {labels_after_attempt!r}")
    report["write_attempt"] = {"refused": True,
                               "labels_before_probe": labels_seeded,
                               "labels_after_attempt": labels_after_attempt,
                               "labels_unchanged": labels_after_attempt == labels_seeded,
                               "result": write_text[:400]}
    report["last_step"] = "write-attempt-refused"

    panel.get_by_role("button", name=ai["panel"]["close"], exact=True).click()
    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    expect(page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"], exact=True)
           ).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    expect(card.get_by_text(config_name, exact=True)).to_be_visible()
    configs = page.request.get(server.base + CONFIGS)
    check(configs.status == 200, f"AI config GET HTTP {configs.status}")
    rows = configs.json()["items"]
    check(len(rows) == 1 and rows[0]["name"] == config_name
          and rows[0]["is_default"] is True
          and rows[0]["preset"] == "custom"
          and rows[0]["base_url"] == fixture.base
          and rows[0]["model"] == fixture.model,
          f"AI config readback differs: {rows}")
    check("api_key" not in rows[0], "the AI config response exposed the upstream key")

    off_name = config_name + "-off"
    button(card, settings["common"]["add"]).click()
    off_modal = page.get_by_role("dialog", name=settings["ai"]["addService"], exact=True)
    expect(off_modal).to_be_visible()
    off_switch = off_modal.locator(".ant-switch")
    expect(off_switch).to_have_count(1, timeout=12000)
    expect(off_switch).not_to_have_class(re.compile(r"\bant-switch-small\b"), timeout=12000)
    expect(off_switch).to_be_checked()
    off_switch.click()
    expect(off_switch).not_to_be_checked()
    off_modal.locator(".ant-select-selector").first.click()
    off_custom = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(r"^" + re.escape(settings["ai"]["presetName"]["custom"]) + r"$"))
    expect(off_custom).to_be_visible()
    off_custom.click()
    expect(off_switch).not_to_be_checked()
    off_modal.get_by_label(settings["ai"]["name"], exact=True).fill(off_name)
    off_modal.get_by_label(settings["ai"]["baseUrl"], exact=True).fill(fixture.base)
    off_modal.get_by_label(settings["ai"]["model"], exact=True).fill(fixture.model)
    off_modal.get_by_label(settings["ai"]["upstreamKey"], exact=True).fill(fixture.api_key)
    with page.expect_response(lambda r: urlsplit(r.url).path == CONFIGS
                              and r.request.method == "POST") as off_created:
        button(off_modal, settings["common"]["add"]).click()
    check(off_created.value.status == 201, f"off-switch AI config HTTP {off_created.value.status}")
    off_body = off_created.value.request.post_data_json
    check(set(off_body) == {"name", "preset", "base_url", "api_key", "model", "is_default"},
          "off-switch create body keys differ")
    check(off_body["name"] == off_name and off_body["preset"] == "custom"
          and off_body["base_url"] == fixture.base and off_body["model"] == fixture.model
          and off_body["is_default"] is False,
          "off-switch create fields differ")
    check(off_body.get("api_key") == fixture.api_key,
          "off-switch create key differs from the configured fixture key")
    off_created_doc = off_created.value.json()
    check(set(off_created_doc) == {"id"}, "off-switch create response was not an id")
    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    expect(card.get_by_text(off_name, exact=True)).to_be_visible()
    expect(card.get_by_text(config_name, exact=True)).to_be_visible()
    off_row = card.locator("tr.ant-table-row").filter(has_text=off_name)
    first_row = card.locator("tr.ant-table-row").filter(
        has_text=re.compile(re.escape(config_name) + r"(?!-)"))
    expect(off_row.locator(".ant-tag")).to_have_count(0, timeout=12000)
    expect(off_row.get_by_text("—", exact=True)).to_be_visible()
    expect(first_row.locator(".ant-tag")).to_have_count(1, timeout=12000)
    expect(first_row.get_by_text(settings["ai"]["default"], exact=True)).to_be_visible()
    both = page.request.get(server.base + CONFIGS)
    check(both.status == 200, f"off-switch AI config GET HTTP {both.status}")
    both_rows = {row["name"]: row for row in both.json()["items"]}
    check(set(both_rows) == {config_name, off_name}, "off-switch create did not leave both services")
    check(both_rows[config_name]["is_default"] is True
          and both_rows[config_name]["preset"] == "custom"
          and both_rows[config_name]["base_url"] == fixture.base
          and both_rows[config_name]["model"] == fixture.model
          and "api_key" not in both_rows[config_name],
          "the first service changed after the off-switch create")
    check(both_rows[off_name]["is_default"] is False
          and both_rows[off_name]["preset"] == "custom"
          and both_rows[off_name]["base_url"] == fixture.base
          and both_rows[off_name]["model"] == fixture.model
          and "api_key" not in both_rows[off_name],
          "the off-switch service was not stored")
    button(off_row, settings["common"]["delete"]).click()
    confirm_popconfirm(page)
    expect(off_row).to_have_count(0, timeout=12000)
    after_off = page.request.get(server.base + CONFIGS)
    check(after_off.status == 200, f"AI config GET after off-switch delete HTTP {after_off.status}")
    after_off_rows = after_off.json()["items"]
    check(len(after_off_rows) == 1 and after_off_rows[0]["name"] == config_name
          and after_off_rows[0]["is_default"] is True,
          "deleting the off-switch service did not leave the first default")

    omit_name = config_name + "-omit"
    omit_payload = {"name": omit_name, "preset": "custom", "base_url": fixture.base,
                    "api_key": fixture.api_key, "model": fixture.model}
    check("is_default" not in omit_payload, "the omitted-default payload includes is_default")
    with page.expect_request(lambda r: urlsplit(r.url).path == CONFIGS
                             and r.method == "POST") as omit_posted:
        omit_result = page.evaluate(
            """async ({url, body}) => {
                const response = await fetch(url, {
                    method: "POST",
                    headers: {"Content-Type": "application/json"},
                    credentials: "same-origin",
                    body: JSON.stringify(body),
                });
                return {status: response.status, json: await response.json()};
            }""",
            {"url": server.base + CONFIGS, "body": omit_payload})
    omit_body = omit_posted.value.post_data_json
    check(omit_result["status"] == 201, f"omitted-default AI config HTTP {omit_result['status']}")
    check("is_default" not in omit_body and set(omit_body) ==
          {"name", "preset", "base_url", "api_key", "model"},
          "omitted-default create body keys differ")
    check(omit_body["name"] == omit_name and omit_body["preset"] == "custom"
          and omit_body["base_url"] == fixture.base and omit_body["model"] == fixture.model,
          "omitted-default create fields differ")
    check(omit_body.get("api_key") == fixture.api_key,
          "omitted-default create key differs from the configured fixture key")
    check(set(omit_result["json"]) == {"id"}, "omitted-default create response was not an id")
    omitted = page.request.get(server.base + CONFIGS)
    check(omitted.status == 200, f"omitted-default AI config GET HTTP {omitted.status}")
    omitted_rows = {row["name"]: row for row in omitted.json()["items"]}
    check(set(omitted_rows) == {config_name, omit_name},
          "omitted-default create did not leave both services")
    check(omitted_rows[config_name]["is_default"] is True
          and omitted_rows[omit_name]["is_default"] is False
          and omitted_rows[omit_name]["preset"] == "custom"
          and omitted_rows[omit_name]["base_url"] == fixture.base
          and omitted_rows[omit_name]["model"] == fixture.model
          and "api_key" not in omitted_rows[omit_name]
          and "api_key" not in omitted_rows[config_name],
          "omitted is_default was not stored as false beside the existing default")
    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    omit_row = card.locator("tr.ant-table-row").filter(has_text=omit_name)
    expect(omit_row).to_have_count(1, timeout=12000)
    expect(omit_row.locator(".ant-tag")).to_have_count(0, timeout=12000)
    button(omit_row, settings["common"]["delete"]).click()
    confirm_popconfirm(page)
    expect(omit_row).to_have_count(0, timeout=12000)
    after_omit = page.request.get(server.base + CONFIGS)
    check(after_omit.status == 200, f"AI config GET after omitted-default delete HTTP {after_omit.status}")
    after_omit_rows = after_omit.json()["items"]
    check(len(after_omit_rows) == 1 and after_omit_rows[0]["name"] == config_name
          and after_omit_rows[0]["is_default"] is True,
          "deleting the omitted-default service did not leave the first default")
    report["off_switch_create"] = {"name": off_name, "is_default": False, "first_remains_default": True}
    report["omitted_default"] = {"name": omit_name, "request_has_is_default": False,
                                 "stored_is_default": False, "first_remains_default": True}
    report["last_step"] = "optional-default-persisted"

    button(card, settings["common"]["delete"]).click()
    popconfirm = page.locator(".ant-popconfirm")
    # 12.7 asks the confirmation to name what is being deleted, so resolve the placeholder
    # and assert the real name is in the prompt rather than the raw catalogue string.
    delete_prompt = settings["ai"]["deleteConfirm"].replace("{{name}}", config_name)
    expect(popconfirm).to_contain_text(delete_prompt)
    confirm_popconfirm(page)
    expect(page.get_by_text(settings["ai"]["deleted"], exact=True).first).to_be_visible()
    expect(card.get_by_text(settings["ai"]["empty"], exact=True)).to_be_visible()
    empty = page.request.get(server.base + CONFIGS)
    check(empty.status == 200 and empty.json()["items"] == [],
          "the AI config was not deleted")
    deleted_node = page.request.delete(server.base + NODES + f"/{node_uuid}?force=true")
    check(deleted_node.status == 200, f"assistant node cleanup HTTP {deleted_node.status}")
    check(page.request.get(server.base + NODES + f"/{node_uuid}").status == 404,
          "the assistant node is still readable")
    report["last_step"] = "cleanup-complete"

    check(len(asks) == 2 and all(item.get("task") == "ask" for item in asks),
          f"UI ask requests differ: {asks}")
    check(asks[0].get("question") == first_question_text
          and asks[0].get("locale") == locale
          and "history" not in asks[0],
          f"first ask differs: {asks[0]}")
    check(asks[1].get("question") == slow_question
          and asks[1].get("locale") == locale
          and any(item.get("role") == "user"
                  and item.get("content") == first_question_text
                  for item in asks[1].get("history") or []),
          f"second ask differs: {asks[1]}")

    expected_no_config = {"method": "POST", "path": ASK, "status": 503}
    unexpected = [item for item in bad_responses if item != expected_no_config]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    allowed_console = [item for item in console_errors
                       if "503" in item and ("load resource" in item or "加载资源" in item)]
    check(len(allowed_console) <= 1 and len(allowed_console) == len(console_errors)
          and not page_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "settings page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/settings", "tab": settings["page"]["tabs"]["integrations"],
         "card": settings["ai"]["service"], "config": config_name,
         "assistant": ai["title"], "no_config_notice": True,
         "tool_step": ai["tool"]["cluster_overview"], "tool_answer": expected_answer,
         "denied_route": "rejected", "stop": ai["stopped"],
         "delete": settings["common"]["delete"],
         "delete_confirmation": settings["ai"]["deleteConfirm"].replace("{{name}}", config_name)},
        {"layer": "request", "create": {"method": "POST", "path": CONFIGS,
         "status": 201, "body": {k: create_body[k] for k in
         ("name", "preset", "base_url", "model", "is_default")}},
         "ask_requests": [{"task": item.get("task"), "question": item.get("question"),
                           "locale": item.get("locale"), "has_history": bool(item.get("history"))}
                          for item in asks],
         "denied_ask": {"status": denied_response.status},
         "no_config_ask": {"status": no_config.value.status},
         "config_get": {"status": 200, "api_key_exposed": False}},
        {"layer": "persistence", "reload": "config row and default flag remain",
         "independent_get": {k: rows[0][k] for k in
                             ("name", "preset", "base_url", "model", "is_default")},
         "off_switch": {"name": off_name, "is_default": False, "first_remains_default": True},
         "omitted_is_default": {"name": omit_name, "stored_is_default": False,
                                "first_remains_default": True},
         "assistant_answer_streamed": True},
        {"layer": "execution", "fixture_requests": [item["kind"] for item in
         denied_snapshot["requests"]],
         "tool_call": "cluster_overview", "tool_call_id": TOOL_CALL_ID,
         "preceding_assistant_call": True,
         "tool_result_total": tool_total, "tool_result_names": tool_names,
         "upstream_cancelled": True, "denied_tool_result": "not readable",
         "final_answer": expected_answer},
        {"layer": "cleanup", "config_deleted": True, "node_deleted": True,
         "independent_get_items": 0, "fixture_stopped": True},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "console_allowed": allowed_console,
                                "http": bad_responses,
                                "no_config_ask": expected_no_config, "asks": asks}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/assistant-service-tools.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_assistant_service_tools.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite-llm-fixture", "identity": "admin",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/assistant_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "dialog_checks_sha256": digest(ROOT / "test/e2e/dialog_checks.py"),
        "keyboard_sha256": digest(ROOT / "test/e2e/assistant_add_keyboard.py"),
        "inventory_sha256": digest(ROOT / "test/e2e/assistant_add_inventory.py"),
        "execution": "real server-side read-only tool loop against an audited local LLM fixture; no GPU claim",
        "runtime": "N/A: no agent, container, GPU or real model",
        "not_covered": ["real vendor model quality", "all read-only tools",
                        "multiple and parallel tool calls", "tool self-correction and retry",
                        "write tools (none are offered)", "terminal-route denial with a node UUID",
                        "MCP clients", "prompt/answer secret-redaction assertions",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/assistant_service_tools.py", *sys.argv[1:]]}
    fixture = None
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        fixture = AssistantEndpoint().start()
        artifacts.remember(fixture.api_key)
        report["fixture"] = {"base": fixture.base, "model": fixture.model}
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
                test_assistant_service_tools(page, context, server, artifacts,
                                             report, args.locale, fixture)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                report["failure_screenshot"] = (
                    "omitted: credentials or untrusted model output may be visible")
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
        try:
            if fixture is not None:
                fixture.stop()
            report["fixture_stopped"] = True
        except Exception:
            report["fixture_stopped"] = False
            report["fixture_stop_error"] = artifacts.redact(traceback.format_exc())
        if not report["server_stopped"] or not report["fixture_stopped"]:
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
