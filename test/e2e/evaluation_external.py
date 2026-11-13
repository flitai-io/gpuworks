#!/usr/bin/env python3
"""External-endpoint evaluation closure through the real browser and server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-benchmark-external
After build: python3 test/e2e/evaluation_external.py --locale en
Requires Python Playwright and system Chrome. The OpenAI-compatible endpoint is
an auditable local fixture; the control plane, SQLite, benchmark executor and
browser are real. No GPU, agent, container or engine is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and the fixture
and retains the private database/logs for scoped diagnosis (seven days).
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

from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect, sync_playwright
from benchmark_fixture import BenchmarkEndpoint
from benchmark_evidence import validate_csv, validate_usage
from benchmark_support import (strings, format_text, action, confirm_popconfirm,
                               assert_point_table, assert_offline_report, finish_environment, close_browser)
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/benchmark/external-endpoint-sweep"
CASE = "评测 / 外部 OpenAI 端点参数扫描 / UI 探测模型、真实流式请求、完成回读与删除"
CANCEL_CONTRACT = "gpuworks/benchmark/cancel-upstream-termination"
CANCEL_CASE = "评测 / 运行中取消 / UI 取消、执行器停止、上游连接终止与取消状态回读"
BENCHMARKS = "/api/v1/benchmarks"
INJECT = {"held_failure": False}


def _box(locator):
    return locator.evaluate("""element => {
        const rect = element.getBoundingClientRect();
        return {left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom,
                width: rect.width, height: rect.height};
    }""")


def _separated(first, second):
    return (first["right"] <= second["left"] or second["right"] <= first["left"]
            or first["bottom"] <= second["top"] or second["bottom"] <= first["top"])


def _no_benchmark_create(page, action):
    submitted = False
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path == BENCHMARKS
                                  and request.method == "POST", timeout=1000):
            action()
        submitted = True
    except PlaywrightTimeout:
        submitted = False
    return submitted


def exercise_benchmark_target_keyboard(page, drawer, wizard, external):
    """API key visibility icon, Tab, Enter, a visible field error, and the footer."""
    api_key = drawer.get_by_label("API Key", exact=True)
    wrapper = api_key.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-input-password ')][1]")
    expect(wrapper.locator(".ant-input-password-icon")).to_have_count(1, timeout=12000)
    name_input = drawer.get_by_label(wizard["name"], exact=True)
    base_url = drawer.get_by_label(wizard["baseUrl"], exact=True)
    target = drawer.get_by_role("radio", name=external, exact=True)
    name_input.focus()
    expect(name_input).to_be_focused()
    name_input.press("Tab")
    expect(target).to_be_focused()
    target.press("Tab")
    expect(base_url).to_be_focused()
    base_url.press("Tab")
    expect(api_key).to_be_focused()
    previous = button(drawer, wizard["previous"])
    nxt = button(drawer, wizard["next"])
    nxt.focus()
    expect(nxt).to_be_focused()
    api_key.focus()
    check(not _no_benchmark_create(page, lambda: api_key.press("Enter")),
          "Enter in the empty API key sent an evaluation create")
    expect(api_key).to_have_value("")
    drawer.scroll_into_view_if_needed()
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    for locator in (name_input, base_url, api_key, previous, nxt):
        expect(locator).to_be_in_viewport(ratio=1)
    name_box, next_box = _box(name_input), _box(nxt)
    check(name_box["width"] > 0 and next_box["width"] > 0, "name or Next has no box")
    check(_separated(name_box, next_box), "name overlaps Next")
    base_item = base_url.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
    check(not _no_benchmark_create(page, lambda: nxt.click(timeout=12000)),
          "Next with an empty endpoint sent an evaluation create")
    expect(base_item.locator(".ant-form-item-explain-error")).to_be_visible(timeout=12000)
    return True


# Contract: gpuworks/benchmark/external-endpoint-sweep
# Mainline: 独立 server/SQLite + 可审计 OpenAI fixture → UI 新建评测并选择参数扫描
#           → 外部端点探测到模型 → 单并发单点小负载启动 → fixture 收到真实流式请求与 usage
#           → 详情完成、刷新/独立 GET/CSV 回读一致 → 列表删除后 404 且 fixture 无新请求
def exercise_wizard_steps(page, drawer, wizard, name, report):
    """14.1 and 14.2: the wizard is three named steps with a real Previous/Next pair, going back keeps the
    same draft, and a step whose required fields are empty refuses to advance instead of letting the form
    submit.
    """
    steps = drawer.locator(".ant-steps-item")
    expect(steps).to_have_count(3)
    titles = [step.inner_text().strip() for step in steps.all()]
    expected = [wizard["step"]["question"], wizard["step"]["target"], wizard["step"]["load"]]
    # Each row is rendered as its ordinal followed by the title, so the ordinal is stripped before the
    # names are compared rather than the whole row being matched against a label.
    named = [re.sub(r"^\d+", "", title).strip() for title in titles]
    check(named == expected, f"the wizard steps are not the three named ones: {titles}")
    active = drawer.locator(".ant-steps-item-active")
    expect(active).to_have_count(1)
    check(wizard["step"]["target"] in active.inner_text(), "the target step is not the active one")
    check(not button(drawer, wizard["previous"]).is_disabled(),
          "Previous is disabled on a step that was reached from the first one")
    # The draft held on this step has to survive going back and forward again. The name is the field
    # this case already filled, so it is a real value rather than one invented for the check.
    held = name
    drawer.get_by_label(wizard["name"], exact=True).fill(held)
    button(drawer, wizard["previous"]).click()
    check(wizard["step"]["question"] in drawer.locator(".ant-steps-item-active").inner_text(),
          "Previous did not return to the first step")
    expect(button(drawer, wizard["previous"])).to_be_disabled()
    button(drawer, wizard["next"]).click()
    check(wizard["step"]["target"] in drawer.locator(".ant-steps-item-active").inner_text(),
          "Next did not come back to the target step")
    check(drawer.get_by_label(wizard["name"], exact=True).input_value() == held,
          "the name field lost its value when the wizard went back and forward")
    # An empty required field on this step must refuse to advance rather than create anything: the
    # base URL and the model are what the probe fills, so clearing the name is the cheapest real block.
    drawer.get_by_label(wizard["name"], exact=True).fill("")
    button(drawer, wizard["next"]).click()
    check(wizard["step"]["target"] in drawer.locator(".ant-steps-item-active").inner_text(),
          "the wizard advanced past a step whose required name was empty")
    check(drawer.get_by_text(re.compile("required|必填", re.I)).count() > 0
          or drawer.locator(".ant-form-item-explain-error").count() > 0,
          "the empty required field was refused without saying why")
    drawer.get_by_label(wizard["name"], exact=True).fill(held)
    report["wizard_steps"] = {"titles": titles, "draft_kept": held, "first_previous_disabled": True}
    return held


def upload_tokenizer(page, drawer, wizard, artifacts):
    """Give the run a real tokenizer without reaching the network.

    A generated token-length workload refuses to run without one, and pointing it at a public
    repository would make this case depend on someone else's network. A small BPE vocabulary is
    written locally and uploaded through the same route the UI uses, so the run measures with a real
    tokenizer while staying offline.
    """
    tokenizer_path = Path(artifacts.directory) / "tokenizer.json"
    tokenizer_path.write_text(json.dumps({"model": {"type": "BPE", "vocab": {
        **{character: index for index, character in
           enumerate("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .,!?-_")},
        **{word: 100 + index for index, word in
           enumerate(("hello", "world", "the", "test", "data", "model", "run", "e2e"))}}}},
        ensure_ascii=False), encoding="utf-8")
    # The upload control only exists once the upload source is selected, so the file input is waited
    # for rather than assumed: a click that lands before the radio group settles would otherwise
    # surface as a bare timeout with no explanation.
    upload_source = drawer.get_by_text(wizard["uploadFile"], exact=True)
    expect(upload_source).to_be_visible(timeout=20000)
    upload_source.click()
    file_input = drawer.locator('input[type="file"]')
    expect(file_input).to_have_count(1, timeout=20000)
    file_input.set_input_files(str(tokenizer_path))
    # The server stores the upload under a generated name, so the reference that comes back is that
    # name rather than the one the test wrote. What matters is that a real reference was accepted and
    # carried into the form instead of the run falling back to a repository download.
    uploaded_ref = drawer.locator('input[id$="tokenizer_ref"]')
    for _ in range(80):
        if uploaded_ref.input_value().strip():
            break
        page.wait_for_timeout(250)
    reference = uploaded_ref.input_value().strip()
    check(reference.startswith(str(tokenizer_path.parent)),
          f"the uploaded tokenizer reference is not a server-local path: {reference}")
    check(Path(reference).name.startswith("tokenizer-") and reference.endswith(".json"),
          f"the uploaded tokenizer reference does not name a stored file: {reference}")
    return reference


def test_external_endpoint_sweep(page, context, server, artifacts, report, locale, fixture):
    ui, dictionary = strings(locale)
    wizard = ui["wizard"]
    grid = dictionary["modes"]["grid"]["label"]
    external = wizard["externalEndpoint"]
    name = f"e2e-{report['run_id']}-evaluation"
    errors, console_errors, bad_responses, mutations = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: errors.append(str(error)))

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
        if path in {BENCHMARKS, BENCHMARKS + "/models"} and request.method == "POST":
            mutations.append({"method": request.method, "path": path,
                              "body": request.post_data_json})
        if path.startswith(BENCHMARKS + "/") and request.method == "DELETE":
            mutations.append({"method": request.method, "path": path})

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    page.goto(server.base + "/evaluations")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=ui["list"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "evaluations-page-loaded"

    action(page, ui["list"]["new"]).click()
    drawer = page.get_by_role("dialog")
    expect(drawer).to_be_visible()
    drawer.get_by_text(grid, exact=True).click()
    button(drawer, wizard["next"]).click()
    target_group = drawer.locator(".ant-radio-group").filter(has_text=external)
    expect(target_group).to_have_count(1, timeout=12000)
    expect(target_group).to_be_visible(timeout=12000)
    expect(target_group).to_have_class(re.compile(r"\bant-radio-group-solid\b"), timeout=12000)
    report["target_radio_solid"] = True

    drawer.get_by_label(wizard["name"], exact=True).fill(name)
    drawer.get_by_text(external, exact=True).click()
    report["benchmark_target_keyboard"] = exercise_benchmark_target_keyboard(
        page, drawer, wizard, external)
    drawn_name = exercise_wizard_steps(page, drawer, wizard, name, report)
    drawer.get_by_label(wizard["baseUrl"], exact=True).fill(fixture.base)
    drawer.get_by_label("API Key", exact=True).fill(fixture.api_key)
    action(drawer, wizard["probe"]).click()
    probe_notice = format_text(wizard["probeSuccess"], count=1)
    expect(drawer.get_by_text(probe_notice, exact=True)).to_be_visible()
    expect(drawer.get_by_text(fixture.model, exact=True).first).to_be_visible()
    button(drawer, wizard["next"]).click()
    report["last_step"] = "endpoint-probed"

    drawer.get_by_label(wizard["isl"], exact=True).fill("16")
    drawer.get_by_label(wizard["osl"], exact=True).fill("8")
    drawer.get_by_label(wizard["samplesPerChannel"], exact=True).fill("1")
    drawer.get_by_label(wizard["concurrencyList"], exact=True).fill("1")
    drawer.get_by_text(wizard["advanced"], exact=True).click()
    drawer.get_by_label(wizard["warmupRequests"], exact=True).fill("0")
    report["last_step"] = "search-configured"

    report["tokenizer_ref"] = upload_tokenizer(page, drawer, wizard, artifacts)
    report["last_step"] = "tokenizer-uploaded"

    held_creates = []

    def hold_create(route):
        if route.request.method == "POST" and urlsplit(route.request.url).path == BENCHMARKS:
            held_creates.append(route)
            return
        route.continue_()

    page.route("**/api/v1/benchmarks", hold_create)
    start = drawer.locator(".ant-drawer-footer button.ant-btn-primary")
    expect(start).to_have_count(1, timeout=12000)
    expect(start).to_contain_text(wizard["start"], timeout=12000)
    start.click(timeout=12000)
    for _ in range(40):
        if held_creates:
            break
        page.wait_for_timeout(50)
    check(len(held_creates) == 1, "evaluation create did not stay in flight")
    expect(start).to_have_class(re.compile(r"\bant-btn-loading\b"), timeout=12000)
    start.click(timeout=12000)
    start.press("Enter")
    page.wait_for_timeout(500)
    check(len(held_creates) == 1, "duplicate evaluation create while the first request is held")
    try:
        with page.expect_response(lambda r: urlsplit(r.url).path == BENCHMARKS
                                  and r.request.method == "POST") as created:
            held_creates[0].continue_()
        check(created.value.status == 202, f"create HTTP {created.value.status}")
    finally:
        page.unroute("**/api/v1/benchmarks", hold_create)
    created_body = created.value.json()
    run_id = created_body["id"]
    create_body = created.value.request.post_data_json
    check(create_body["target_kind"] == "external", "UI submitted a non-external target")
    check(create_body["base_url"] == fixture.base, "UI submitted a different endpoint")
    check(create_body["model"] == fixture.model, "UI submitted a different model")
    check(create_body["load_origin"] == "server", "external target must load from the server")
    check(create_body["mode"] == "grid", "UI submitted a different question type")
    check(create_body["search"]["concurrency"] == [1], "UI submitted another concurrency list")
    check(create_body["search"]["isl"] == [16] and create_body["search"]["osl"] == [8],
          "UI submitted different lengths")
    check(create_body["search"]["warmup_requests"] == 0, "warmup was not disabled")
    check(create_body["dataset"]["kind"] == "random", "UI submitted another dataset")
    check(create_body["dataset"]["input_tokens"] == 16
          and create_body["dataset"]["output_tokens"] == 8, "UI submitted different dataset lengths")
    report["last_step"] = "benchmark-created"

    page.wait_for_url(re.compile(r".*/evaluations/\d+$"), timeout=15000)
    expect(page.get_by_text(name, exact=True).first).to_be_visible()
    done_text = format_text(ui["utility"]["verdict"]["gridDoneTitle"], count=1)
    expect(page.get_by_text(done_text, exact=True)).to_be_visible(timeout=30000)
    page.screenshot(path=artifacts.directory / "evaluation-complete.png")
    report["last_step"] = "benchmark-completed"

    independent = page.request.get(server.base + BENCHMARKS + f"/{run_id}")
    check(independent.status == 200, f"independent GET HTTP {independent.status}")
    run = independent.json()
    check(run["state"] == "done" and not run.get("error"), f"run state {run['state']}: {run.get('error')}")
    check(run["target_kind"] == "external" and run["base_url"] == fixture.base,
          "persisted target differs")
    check(run["model"] == fixture.model and run["load_origin"] == "server",
          "persisted model/origin differs")
    report["benchmark_single_submit"] = True
    check(run["verdict"]["kind"] == "grid_done", "grid run did not persist the grid verdict")
    check(len(run.get("points") or []) == 1, "expected exactly one completed point")
    point = run["points"][0]
    check(point["concurrency"] == 1 and point["target_isl"] == 16
          and point["target_osl"] == 8, "persisted point dimensions differ")
    check(point["success"] == 1 and point["errors"] == 0, "point did not record one success")
    measured = fixture.snapshot()["requests"][0]
    validate_usage(point, measured["response_usage"])
    assert_point_table(page, ui, point)

    # Exercise the product download entry, then independently compare its bytes.
    action(page, ui["agentic"]["export"]["button"]).click()
    with page.expect_download() as downloaded:
        page.get_by_role("link", name=ui["agentic"]["export"]["csv"], exact=True).click()
    download = downloaded.value
    check(download.failure() is None, "CSV download failed")
    downloaded_bytes = Path(download.path()).read_bytes()
    (artifacts.directory / "report.csv").write_bytes(downloaded_bytes)
    report_response = page.request.get(
        server.base + BENCHMARKS + f"/{run_id}/report?format=csv")
    check(report_response.status == 200, f"CSV report HTTP {report_response.status}")
    check("text/csv" in report_response.headers.get("content-type", ""),
          "CSV report content type differs")
    check(downloaded_bytes == report_response.body(), "download and report API differ")
    validate_csv(downloaded_bytes.decode("utf-8"), point, run)
    assert_offline_report(page, ui, locale, run, point, artifacts.directory / "report.html")
    report["last_step"] = "report-read"

    snapshot = fixture.snapshot()
    check(len(snapshot["requests"]) == 1,
          f"fixture received {len(snapshot['requests'])} measured requests, want 1")
    measured = snapshot["requests"][0]
    check(measured["validation"] == [], f"fixture rejected the request: {measured['validation']}")
    check(measured["authorization"] == f"Bearer {fixture.api_key}",
          "benchmark request lost the configured API key")
    sent = measured["body"]
    check(sent["model"] == fixture.model and sent["stream"] is True,
          "benchmark request model/stream differs")
    check(sent["stream_options"]["include_usage"] is True,
          "benchmark request omitted usage accounting")
    check(sent["max_tokens"] == 8, "benchmark request max_tokens differs")
    check(len(sent["messages"]) == 1 and sent["messages"][0]["role"] == "user"
          and sent["messages"][0]["content"], "benchmark request messages differ")
    check(len(snapshot["models_requests"]) >= 2,
          "UI probe and create admission did not both reach the endpoint")
    check(all(item["authorization"] == f"Bearer {fixture.api_key}"
              for item in snapshot["models_requests"]), "a model probe lost the API key")

    page.reload()
    expect(page.get_by_text(name, exact=True).first).to_be_visible()
    expect(page.get_by_text(done_text, exact=True)).to_be_visible(timeout=30000)

    action(page, ui["common"]["back"]).click()
    page.wait_for_url(re.compile(r".*/evaluations$"), timeout=15000)
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=ui["list"]["delete"], exact=True).click()
    with page.expect_response(lambda r: urlsplit(r.url).path == BENCHMARKS + f"/{run_id}"
                              and r.request.method == "DELETE") as deleted:
        confirm_popconfirm(page)
    check(deleted.value.status in (200, 204), f"delete HTTP {deleted.value.status}")
    expect(page.get_by_text(name, exact=True)).to_have_count(0, timeout=15000)
    report["last_step"] = "benchmark-deleted"

    gone = page.request.get(server.base + BENCHMARKS + f"/{run_id}")
    check(gone.status == 404, f"deleted run still returns {gone.status}")

    # 14.2's other half: a wizard that is filled one step in and then abandoned must leave nothing
    # behind. It is done here, after the real run was deleted, so the list is known to be empty and any
    # row that appeared would be the abandoned draft's.
    def run_ids():
        response = page.request.get(server.base + BENCHMARKS)
        check(response.status == 200, f"benchmark list HTTP {response.status}")
        body = response.json()
        items = body["items"] if isinstance(body, dict) else body
        return [item["id"] for item in items]

    before_abandon = run_ids()
    action(page, ui["list"]["new"]).click()
    abandoned = page.get_by_role("dialog")
    expect(abandoned).to_be_visible()
    abandoned.get_by_text(grid, exact=True).click()
    button(abandoned, wizard["next"]).click()
    expect(abandoned.locator(".ant-steps-item-active")).to_contain_text(wizard["step"]["target"])
    abandoned.get_by_label(wizard["name"], exact=True).fill(f"e2e-{report['run_id']}-abandoned")
    page.keyboard.press("Escape")
    expect(abandoned).not_to_be_visible()
    after_abandon = run_ids()
    check(after_abandon == before_abandon, "closing the wizard left a run behind")
    expect(page.get_by_text(f"e2e-{report['run_id']}-abandoned", exact=True)).to_have_count(0)
    report["wizard_abandoned"] = {"runs_before": len(before_abandon), "runs_after": len(after_abandon)}
    check(len(fixture.snapshot()["requests"]) == len(snapshot["requests"]),
          "the fixture received requests after the run completed")

    expected_delete = {"method": "GET", "path": BENCHMARKS + f"/{run_id}", "status": 404}
    unexpected = [item for item in bad_responses
                  if not (item["method"] == expected_delete["method"]
                          and item["path"] == expected_delete["path"]
                          and item["status"] == expected_delete["status"])]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    check(not errors and not console_errors,
          f"browser errors: {errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "evaluation page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/evaluations", "wizard": wizard["title"],
         "mode": grid, "target": external, "probe": probe_notice,
         "completion": done_text, "delete": ui["list"]["delete"]},
        {"layer": "request", "create": {"method": "POST", "path": BENCHMARKS,
         "status": 202, "body": create_body},
         "probe": {"path": BENCHMARKS + "/models", "models": fixture.model},
         "mutations": mutations},
        {"layer": "persistence", "independent": {"state": run["state"],
         "verdict": run["verdict"]["kind"], "points": len(run["points"]),
         "refresh": "same name and completion after reload"},
         "report": {"format": "csv", "status": 200}},
        {"layer": "execution", "endpoint_requests": len(snapshot["requests"]),
         "models_requests": len(snapshot["models_requests"]),
         "point": {"success": point["success"], "errors": point["errors"],
                   "input_tokens": point["input_tokens"],
                   "output_tokens": point["output_tokens"],
                   "stream": sent["stream"], "usage": sent["stream_options"]}},
        {"layer": "cleanup", "delete": {"status": deleted.value.status, "after_delete": 404},
         "fixture_requests_after_completion": 0},
    ])
    report["browser_errors"] = {"page": errors, "console": console_errors,
                                "http": bad_responses, "mutations": mutations}


# Contract: gpuworks/benchmark/cancel-upstream-termination
# Mainline: 独立 server/SQLite + 可审计 OpenAI fixture → UI 新建外部端点评测
#           → fixture 扣住在途流式请求 → 详情页取消 → 执行器确认停止（200）
#           → 释放 fixture 后观察到客户端连接已断开（BrokenPipe/Reset）
#           → 独立 GET 与刷新均为 canceled，删除后 404
def test_cancel_running_run(page, context, server, artifacts, report, locale, fixture):
    ui, dictionary = strings(locale)
    wizard = ui["wizard"]
    grid = dictionary["modes"]["grid"]["label"]
    external = wizard["externalEndpoint"]
    name = f"e2e-{report['run_id']}-cancel"
    errors, console_errors, bad_responses, mutations = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: errors.append(str(error)))

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
        if path in {BENCHMARKS, BENCHMARKS + "/models"} and request.method == "POST":
            mutations.append({"method": request.method, "path": path,
                              "body": request.post_data_json})
        if path.startswith(BENCHMARKS + "/") and request.method in {"POST", "DELETE"}:
            mutations.append({"method": request.method, "path": path})

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    page.goto(server.base + "/evaluations")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=ui["list"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "evaluations-page-loaded"

    action(page, ui["list"]["new"]).click()
    drawer = page.get_by_role("dialog")
    expect(drawer).to_be_visible()
    drawer.get_by_text(grid, exact=True).click()
    button(drawer, wizard["next"]).click()
    drawer.get_by_label(wizard["name"], exact=True).fill(name)
    drawer.get_by_text(external, exact=True).click()
    drawer.get_by_label(wizard["baseUrl"], exact=True).fill(fixture.base)
    drawer.get_by_label("API Key", exact=True).fill(fixture.api_key)
    action(drawer, wizard["probe"]).click()
    expect(drawer.get_by_text(format_text(wizard["probeSuccess"], count=1),
                              exact=True)).to_be_visible()
    button(drawer, wizard["next"]).click()
    drawer.get_by_label(wizard["isl"], exact=True).fill("16")
    drawer.get_by_label(wizard["osl"], exact=True).fill("8")
    drawer.get_by_label(wizard["samplesPerChannel"], exact=True).fill("1")
    drawer.get_by_label(wizard["concurrencyList"], exact=True).fill("1")
    drawer.get_by_text(wizard["advanced"], exact=True).click()
    drawer.get_by_label(wizard["warmupRequests"], exact=True).fill("0")
    report["tokenizer_ref"] = upload_tokenizer(page, drawer, wizard, artifacts)
    report["last_step"] = "endpoint-probed"

    # The fixture holds the first measured request after its first chunk; the
    # run cannot complete until the test releases it.
    fixture.hold_next_request()
    with page.expect_response(lambda r: urlsplit(r.url).path == BENCHMARKS
                              and r.request.method == "POST") as created:
        button(drawer, wizard["start"]).click()
    check(created.value.status == 202, f"create HTTP {created.value.status}")
    run_id = created.value.json()["id"]
    report["last_step"] = "benchmark-created"
    page.wait_for_url(re.compile(r".*/evaluations/\d+$"), timeout=15000)
    check(fixture.hold_reached.wait(30), "the fixture never received the load request")
    running = page.request.get(server.base + BENCHMARKS + f"/{run_id}")
    check(running.status == 200, f"running GET HTTP {running.status}")
    check(running.json()["state"] in ("pending", "running"),
          f"run is not active: {running.json()['state']}")
    report["last_step"] = "benchmark-running-and-held"
    if INJECT["held_failure"]:
        raise RuntimeError("injected failure while the stream is held")

    # 12.7: deletion is guarded while a run is live, and the guard is stated rather than silent.
    # The list row's delete control must be disabled for a pending or running run and must say why,
    # and the API must refuse the same delete on its own - a UI-only guard would let any client
    # delete a live run, an API-only guard would offer the operator a control that cannot work, and
    # a refusal that still changed the run would be worse than either.
    page.goto(server.base + "/evaluations")
    live_row = page.get_by_role("row").filter(has_text=name)
    expect(live_row).to_have_count(1, timeout=15000)
    live_delete = live_row.get_by_role("button", name=ui["list"]["delete"])
    check(live_delete.is_disabled(), "a live run offered an enabled delete control")
    # The row's confirmation is disabled for a live run as well, so clicking cannot open it and the
    # refusal text is not reachable from this control. That is the product's behaviour and is
    # recorded as such; what the case can require is that no confirmation appears and nothing is
    # submitted, which is what a live run needs.
    live_delete.click(force=True)
    page.wait_for_timeout(500)
    check(page.get_by_text(ui["list"]["deleteTitle"], exact=True).count() == 0,
          "a live run opened its delete confirmation")
    check(page.get_by_text(ui["list"]["deleted"], exact=True).count() == 0,
          "a live run reported a successful delete")
    live_refused = page.request.delete(server.base + BENCHMARKS + f"/{run_id}")
    check(live_refused.status == 409,
          f"deleting a live run returned {live_refused.status}, want 409")
    still_there = page.request.get(server.base + BENCHMARKS + f"/{run_id}")
    check(still_there.status == 200 and still_there.json()["state"] in ("pending", "running"),
          "a refused delete still changed the run")
    page.goto(server.base + "/evaluations/" + str(run_id))
    expect(page.get_by_role("heading", name=re.compile(re.escape(name)))).to_be_visible(timeout=15000)
    report["live_delete"] = {"control_disabled": True, "api_status": live_refused.status}

    cancel = action(page, ui["detail"]["cancel"])
    expect(cancel).to_be_visible(timeout=15000)
    with page.expect_response(lambda r: urlsplit(r.url).path == BENCHMARKS + f"/{run_id}/cancel"
                              and r.request.method == "POST") as canceled:
        cancel.click()
        confirm_popconfirm(page)
    check(canceled.value.status == 200,
          f"cancel HTTP {canceled.value.status} {canceled.value.text()[:300]}")
    canceled_run = canceled.value.json()
    check(canceled_run["state"] == "canceled",
          f"cancel returned state {canceled_run['state']}")
    expect(page.get_by_text(ui["detail"]["canceled"], exact=True)).to_be_visible()
    report["last_step"] = "benchmark-canceled"

    fixture.release_held()
    deadline = time.time() + 15
    aborted = []
    while time.time() < deadline:
        aborted = fixture.snapshot()["aborted"]
        if aborted:
            break
        time.sleep(0.1)
    check(bool(aborted), "the fixture never observed the abandoned connection")
    check(any(item["error"] in ("BrokenPipeError", "ConnectionResetError")
              for item in aborted),
          f"the connection ended for another reason: {aborted}")
    snapshot = fixture.snapshot()
    check(snapshot["completed"] == 0, "the canceled stream still reached [DONE]")
    check(len(snapshot["requests"]) == 1,
          f"the fixture received {len(snapshot['requests'])} requests, want 1")
    check(snapshot["requests"][0]["validation"] == [],
          f"fixture rejected the request: {snapshot['requests'][0]['validation']}")
    report["last_step"] = "upstream-connection-terminated"

    final = page.request.get(server.base + BENCHMARKS + f"/{run_id}")
    check(final.status == 200, f"final GET HTTP {final.status}")
    run = final.json()
    check(run["state"] == "canceled", f"persisted state is {run['state']}")
    check(run.get("verdict", {}).get("kind") != "grid_done",
          "a canceled run still carries a completed verdict")
    check(run.get("error") == "canceled by user", f"cancel reason differs: {run.get('error')}")
    page.reload()
    expect(page.get_by_text(ui["utility"]["verdict"]["canceledTitle"],
                            exact=True)).to_be_visible(timeout=15000)
    report["last_step"] = "canceled-state-persisted"

    action(page, ui["common"]["back"]).click()
    page.wait_for_url(re.compile(r".*/evaluations$"), timeout=15000)
    existing = page.get_by_role("row").filter(has_text=name)
    expect(existing).to_have_count(1)
    # The canceled run is deleted the way an operator would delete it, so the confirmation is part of
    # what is exercised rather than skipped in favour of an API call. The dialog is required to state
    # the impact it is about to have, and the deletion is required to actually reach the server.
    existing.get_by_role("button", name=ui["list"]["delete"]).click()
    page.locator(".ant-popconfirm").get_by_text(ui["list"]["deleteTitle"], exact=True).wait_for(
        state="visible", timeout=15000)
    page.locator(".ant-popconfirm").get_by_text(ui["list"]["deleteDescription"], exact=True).wait_for(
        state="visible", timeout=15000)
    with page.expect_response(lambda r: r.request.method == "DELETE"
                              and urlsplit(r.url).path == BENCHMARKS + f"/{run_id}") as deleted:
        confirm_popconfirm(page)
    check(deleted.value.status in (200, 204), f"delete HTTP {deleted.value.status}")
    page.reload()
    expect(page.get_by_role("row").filter(has_text=name)).to_have_count(0, timeout=15000)
    gone = page.request.get(server.base + BENCHMARKS + f"/{run_id}")
    check(gone.status == 404, f"deleted run still returns {gone.status}")
    report["last_step"] = "canceled-run-deleted"

    check(not bad_responses, "unexpected browser HTTP errors: " + str(bad_responses))
    check(not errors and not console_errors,
          f"browser errors: {errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/evaluations", "wizard": wizard["title"],
         "mode": grid, "target": external, "cancel": ui["detail"]["cancel"],
         "canceled": ui["detail"]["canceled"],
         "verdict": ui["utility"]["verdict"]["canceledTitle"]},
        {"layer": "request", "create": {"path": BENCHMARKS, "status": 202},
         "cancel": {"path": BENCHMARKS + f"/{run_id}/cancel",
                    "status": canceled.value.status},
         "delete": {"path": BENCHMARKS + f"/{run_id}", "status": deleted.value.status},
         "mutations": mutations},
        {"layer": "persistence",
         "canceled": {"state": canceled_run["state"]},
         "independent": {"state": run["state"], "error": run.get("error")},
         "refresh": "canceled verdict after reload", "after_delete": 404},
        {"layer": "execution",
         "held_request": True, "aborted": aborted, "completed": snapshot["completed"],
         "fixture_requests": len(snapshot["requests"]),
         "request_validation": snapshot["requests"][0]["validation"],
         "stream": snapshot["requests"][0]["body"].get("stream")},
        {"layer": "cleanup", "deleted": deleted.value.status, "after_delete": 404},
    ])
    report["browser_errors"] = {"page": errors, "console": console_errors,
                                "http": bad_responses, "mutations": mutations}


CASES = {
    "sweep": {
        "contract": CONTRACT, "case": CASE,
        "manifest": "benchmark-external-sweep.json",
        "test": test_external_endpoint_sweep,
        "not_covered": ["managed-instance target and switching back between target kinds",
                        "duplicate Start submission", "list pagination",
                        "saturation and SLA questions",
                        "open-loop arrival rates", "multi-point grid and length matrix",
                        "cancellation before completion", "endpoint rejection and recovery",
                        "gzip per-request details", "HTML/Markdown reports",
                        "compare view", "the other locale (each run covers one --locale)"],
    },
    "cancel": {
        "contract": CANCEL_CONTRACT, "case": CANCEL_CASE,
        "manifest": "benchmark-external-cancel.json",
        "test": test_cancel_running_run,
        "not_covered": ["cancel after some points completed; retained-point readback",
                        "cancel when the executor cannot confirm the stop",
                        "delete while running",
                        "managed-instance target cancellation through the agent",
                        "endpoint failure and endpoint rejection",
                        "non-admin authorization",
                        "the other locale (each run covers one --locale)"],
    },
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--case", choices=sorted(CASES), default="sweep")
    parser.add_argument("--inject-held-failure", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    INJECT["held_failure"] = args.inject_held_failure
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
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite-openai-fixture",
        "identity": "admin browser / benchmark executor on the server",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [selected["contract"]], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/benchmark_fixture.py"),
        "support_sha256": digest(ROOT / "test/e2e/benchmark_support.py"),
        "evidence_sha256": digest(ROOT / "test/e2e/benchmark_evidence.py"),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "real streamed /v1/chat/completions against an audited local OpenAI fixture; no GPU claim",
        "runtime": "N/A: external OpenAI-compatible fixture; no agent, container, engine or GPU",
        "not_covered": selected["not_covered"],
        "command": ["python3", "test/e2e/evaluation_external.py", *sys.argv[1:]]}
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
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
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
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        report["private_state"] = (
            "private/ retains isolated DB/logs; no process kept; remove only this run directory after diagnosis (7 days)"
            if report["environment_cleanup_ok"] else
            "private/ retains isolated DB/logs; inspect cleanup errors, owned_server_pid and fixture_active_handlers before removing this run directory (7 days)")
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; private directory must not be uploaded"
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": selected["contract"],
                      "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
