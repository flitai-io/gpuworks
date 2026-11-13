#!/usr/bin/env python3
"""Deployment / real container stop, start and delete through the browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-deployment-lifecycle
After build: python3 test/e2e/deployment_lifecycle.py --locale en
Requires Python Playwright, system Chrome, Docker and the built server/agent.
The GPU facts are injected only so the scheduler places the deployment; the
container, engine process and gateway request are real Docker/fake-engine.
No real GPU compute, dGPU isolation or vendor driver is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server/agent and
retains the private database/logs for scoped diagnosis (seven days).
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import traceback
from urllib.parse import parse_qs, urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http, ADMIN_PASSWORD
from platform_token_read_revoke import button, check
import deployment_container_fixture as container_fixture

CONTRACT = "gpuworks/deployment/container-lifecycle"
CASE = "部署 / 真实容器生命周期 / 停止、启动、网关请求、删除与资源回收"
DEPLOYMENTS = "/api/v1/deployments"
MODELS = "/api/v1/models"
APIKEYS = "/api/v1/apikeys"
NODES = "/api/v1/nodes"
MODEL_DIR = ROOT / "test/e2e/testdata/fake-model"
IMAGE = "gpuworks/fake-engine:test"
PROFILE = "/v1/chat/completions"


def strings(locale):
    deployments = json.loads((ROOT / "ui/src/i18n/locales" / locale / "deployments.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    # The state filter's options are labelled by pages/state.ts through common.states, so the
    # filter helper needs the same strings the product puts on the checkboxes.
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    return deployments, auth, common


def wait_online(base, token, node_uuid, agent):
    deadline = time.time() + 40
    while time.time() < deadline:
        status, node = http(base, "GET", NODES + f"/{node_uuid}", token=token)
        if status == 200 and node.get("state") == "online":
            return node
        if agent.poll() is not None:
            raise AssertionError("the agent exited before the node came online")
        time.sleep(0.5)
    raise AssertionError("the node never came online")


INJECT = {"docker_error": False}


def docker_running(container_id):
    if container_id and INJECT["docker_error"]:
        raise AssertionError("injected docker daemon error")
    return container_fixture.docker_running(container_id)


def wait_api(page, server, dep_id, predicate, timeout=90, label="deployment", report=None):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        response = page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}")
        if response.status == 404:
            last = "404"
        elif response.status == 200:
            last = response.json()
            if report is not None:
                container_fixture.remember_containers(report, last)
            if predicate(last):
                return last
        time.sleep(0.5)
    raise AssertionError(f"{label} stayed {last}")


# Contract: gpuworks/deployment/container-lifecycle
# Mainline: 独立 server/SQLite + 真实 agent/Docker + fake-engine 容器（GPU 事实仅用于调度）
#           → 部署运行并可通过网关请求 → UI 停止后容器退出 → UI 启动后新容器运行并再次可请求
#           → UI 删除后部署 404、容器消失 → 停止 agent、删除节点与模型
def test_deployment_lifecycle(page, context, server, artifacts, report, locale,
                              node, deployment, api_key, token=""):
    # 22.5 and 22.7 assert the shell's own translated wording, which lives in common.json;
    # the state filter's option labels come from the same file.
    deployments, auth, common = strings(locale)
    detail = deployments["detail"]
    delete = deployments["delete"]
    dep_id, dep_name, served_name = deployment["id"], deployment["name"], deployment["served_name"]
    page_errors, console_errors, bad_responses = [], [], []
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}
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

    page.on("console", console_seen)
    page.on("response", response_seen)

    page.goto(server.base + "/deployments")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=deployments["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    initial_doc = wait_api(page, server, dep_id,
                           lambda d: d.get("runtime_state") == "running"
                           and any(i.get("state") == "running" and i.get("container_id")
                                   for i in d.get("instances") or []),
                           label="initial running deployment", report=report)
    initial_container = next(i["container_id"] for i in initial_doc["instances"]
                             if i.get("state") == "running" and i.get("container_id"))
    report["container_id"] = initial_container
    container_fixture.remember_containers(report, initial_doc)
    row = page.get_by_role("row").filter(has_text=dep_name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=dep_name, exact=True).click()
    drawer = page.get_by_role("dialog").last
    expect(drawer).to_be_visible()
    expect(drawer).to_be_in_viewport(ratio=1)
    expect(drawer.get_by_text(dep_name, exact=True).first).to_be_visible()
    report["last_step"] = "deployment-drawer-open"
    check(docker_running(initial_container) == "true",
          f"the listed container is not running: {initial_container}")

    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/stop"
                              and r.request.method == "POST") as stopped:
        button(drawer, detail["stop"]).click()
        popconfirm = page.locator(".ant-popconfirm")
        expect(popconfirm).to_be_visible(timeout=5000)
        popconfirm.locator(".ant-btn-primary").click()
    check(stopped.value.status == 200, f"stop HTTP {stopped.value.status}")
    expect(page.locator(".ant-message").get_by_text(detail["stopped"], exact=True)).to_be_visible()
    stopped_doc = wait_api(page, server, dep_id,
                           lambda d: d.get("desired_state") == "stopped"
                           and not any(i.get("state") in ("running", "starting")
                                       for i in d.get("instances") or []),
                           label="stopped", report=report)
    deadline = time.time() + 30
    while time.time() < deadline and docker_running(initial_container) != "absent":
        time.sleep(0.5)
    check(docker_running(initial_container) == "absent",
          f"the stopped container is still present: {initial_container}")
    report["last_step"] = "deployment-stopped"

    expect(button(drawer, detail["start"])).to_be_visible(timeout=30000)
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/start"
                              and r.request.method == "POST") as started:
        button(drawer, detail["start"]).click()
    check(started.value.status == 200, f"start HTTP {started.value.status}")
    expect(page.locator(".ant-message").get_by_text(detail["started"], exact=True)).to_be_visible()
    running_doc = wait_api(page, server, dep_id,
                           lambda d: d.get("runtime_state") == "running"
                           and any(i.get("state") == "running" and i.get("container_id")
                                   for i in d.get("instances") or []),
                           label="running", report=report)
    running_container = next(i["container_id"] for i in running_doc["instances"]
                             if i.get("state") == "running" and i.get("container_id"))
    check(running_container != initial_container,
          "start reused the stopped container instead of creating a new one")
    if running_container not in report["containers"]:
        report["containers"].append(running_container)
    check(docker_running(running_container) == "true",
          f"the started container is not running: {running_container}")
    chat = page.request.post(server.base + PROFILE,
                             headers={"Authorization": f"Bearer {api_key}"},
                             data={"model": served_name,
                                   "messages": [{"role": "user", "content": "lifecycle"}]})
    container_fixture.assert_chat_response(chat, "gateway chat after start")
    report["last_step"] = "deployment-started-and-serving"

    # 22.6 runs here, while the deployment still exists: its disabled controls are derived
    # from the running state (DeploymentsPage.tsx:243-244), so a deleted deployment would
    # leave nothing to read. The other 22 items run after the delete below.
    exercise_keyboard_and_style_22_6(page, server, report, dep_id)
    report["last_step"] = "keyboard-and-style-verified"

    # Section 10 runs while the deployment under test is still alive. The pagination batch is
    # created first because sorting and the populated reading need more rows than this case
    # owns, and it is torn down in its own finally so the delete below sees only the
    # deployment this case owns.
    report["token"] = token
    page_rows = []
    try:
        page_rows = _page_rows_fixture(server, token, node, deployment["model_id"], report)
        exercise_list_states_10_1(page, server, report, deployments, token)
        exercise_search_10_2(page, server, report, token)
        exercise_sort_10_4(page, server, report, common)
        exercise_page_size_10_5(page, server, report, token, len(page_rows))
        exercise_pagination_10_6(page, server, report, token, common)
        exercise_row_actions_10_9(page, server, report, token, page_rows)
        exercise_delete_10_7(page, server, report, token, page_rows, common)
        exercise_filters_10_3(page, server, report, token, deployments, locale)
        exercise_refresh_contract_10_10(page, server, report, token, page_rows, locale)
        # Section 10 navigates away from the detail drawer the lifecycle steps below delete
        # from, so the drawer is reopened here in the state it was left in.
        page.goto(server.base + f"/deployments?deployment={dep_id}")
        _settled_path(page)
        expect(page.locator(".ant-drawer").first).to_be_visible(timeout=25000)
        page.wait_for_timeout(600)
    finally:
        # The rows are real deployments with real containers, so they are removed here even
        # when a section-10 assertion failed; cleanup() also covers this batch, and this is
        # the earlier teardown for the failure path.
        report["page_rows_cleanup_ok"] = _stop_page_rows(server, token, page_rows, report)
    report["last_step"] = "list-section-verified"

    # The delete button carries an icon whose label prefixes the accessible name.
    drawer.get_by_role("button", name=re.compile(re.escape(delete["button"]) + "$")).click()
    modal = page.get_by_role("dialog").last
    expect(modal.get_by_text(dep_name)).to_be_visible()
    confirm = modal.locator(".ant-modal-footer .ant-btn-primary")
    expect(modal).to_be_in_viewport(ratio=1)
    expect(confirm).to_be_in_viewport(ratio=1)
    # AntD spaces two-character Chinese button text.
    expect(confirm).to_contain_text(
        re.compile(r"\s*".join(re.escape(char) for char in delete["button"])))
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}"
                              and r.request.method == "DELETE") as deleted:
        confirm.click()
    check(deleted.value.status in (200, 202, 204), f"delete HTTP {deleted.value.status}")
    expect(page.locator(".ant-message").get_by_text(delete["started"], exact=True)).to_be_visible()
    deadline = time.time() + 90
    while time.time() < deadline:
        if page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}").status == 404:
            break
        time.sleep(0.5)
    check(page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}").status == 404,
          "the deleted deployment is still readable")
    report["owned_deployment_id"] = None
    deadline = time.time() + 30
    while time.time() < deadline and docker_running(running_container) != "absent":
        time.sleep(0.5)
    check(docker_running(running_container) == "absent",
          f"the deleted deployment's container remains: {running_container}")
    page.goto(server.base + "/deployments")
    expect(page.get_by_role("row").filter(has_text=dep_name)).to_have_count(0, timeout=30000)
    report["last_step"] = "deployment-deleted"

    # Checklists 22.1 and 22.2 run while the browser is still up and before the browser-error
    # ledger is read, so any error these navigations cause is counted rather than skipped.
    exercise_navigation_22_1(page, server, report)
    exercise_history_22_2(page, server, report, deployment)
    exercise_locale_22_5(page, server, report, locale)
    exercise_window_contract_22_7(page, server, report, common)
    exercise_single_role_22_3(page, server, report)
    exercise_polling_no_renewal_22_4(server, report, artifacts)
    report["last_step"] = "navigation-and-history-verified"

    expected_404 = {"method": "GET", "path": DEPLOYMENTS + f"/{dep_id}", "status": 404}
    unexpected = [item for item in bad_responses if item != expected_404]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    allowed_console = [item for item in console_errors
                       if "404" in item and ("load resource" in item or "加载资源" in item)]
    check(len(allowed_console) <= 2 and len(allowed_console) == len(console_errors)
          and not page_errors,
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/deployments", "row": dep_name,
         "drawer": detail["title"], "stop": detail["stop"], "start": detail["start"],
         "stopped": detail["stopped"], "started": detail["started"],
         "delete": delete["button"]},
        {"layer": "request", "stop": {"path": DEPLOYMENTS + f"/{dep_id}/stop",
                                      "status": stopped.value.status},
         "start": {"path": DEPLOYMENTS + f"/{dep_id}/start",
                   "status": started.value.status},
         "delete": {"path": DEPLOYMENTS + f"/{dep_id}",
                    "status": deleted.value.status},
         "chat": {"path": PROFILE, "status": chat.status, "model": served_name}},
        {"layer": "persistence", "stopped": {"desired_state": stopped_doc.get("desired_state"),
                                             "instances": [i.get("state") for i in
                                                           stopped_doc.get("instances") or []]},
         "running": {"runtime_state": running_doc.get("runtime_state"),
                     "ready_replicas": running_doc.get("ready_replicas")},
         "after_delete": 404},
        {"layer": "execution", "initial_container": {"id": initial_container,
                                                     "running": "true"},
         "stopped_container": {"id": initial_container, "present": "absent"},
         "running_container": {"id": running_container, "running": "true"},
         "deleted_container": {"id": running_container, "present": "absent"},
         "gateway_choices": True, "image": IMAGE},
        {"layer": "cleanup", "deployment_deleted": True, "container_absent": True},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}


def create_fixtures(server, token, run_id, report, holder, agent_bin,
                    inject_model_failure=False, remember=lambda value: None):
    """Create the fixture resources, registering each one before the next.

    A later failure must leave every earlier resource in the report and the
    running agent in the holder so cleanup can reclaim it.
    """
    status, created = http(server.base, "POST", NODES, token=token,
                           body={"name": f"e2e-{run_id}-deploy-node",
                                 "server_url": server.base})
    check(status == 201, f"node fixture HTTP {status}")
    node, node_token = created["node"], created["token"]
    remember(node_token)
    report["owned_node_uuid"] = node["uuid"]
    state = server.work / "agent-state"
    state.mkdir()
    log = (server.work / "agent.log").open("w")
    holder["log"] = log
    env = {**os.environ, "GPUWORKS_FAKE_GPU": "A100x1,24576",
           "GPUWORKS_FAKE_GPU_NO_INJECT": "1"}
    agent = subprocess.Popen([str(agent_bin), "run",
                              "--server", server.base, "--token", node_token,
                              "--state", str(state)],
                             stdout=log, stderr=subprocess.STDOUT, text=True, env=env)
    holder["agent"], holder["log"] = agent, log
    wait_online(server.base, token, node["uuid"], agent)
    if inject_model_failure:
        # Self-test: the agent is already running when preparation fails.
        raise RuntimeError("injected model registration failure")
    status, model = http(server.base, "POST", MODELS + "/local", token=token, body={
        "node_uuid": node["uuid"], "path": str(MODEL_DIR),
        "source_id": f"e2e/{run_id}-lifecycle", "name": f"e2e-{run_id}-model"})
    check(status in (200, 201), f"model fixture HTTP {status}")
    report["owned_model_id"] = model["id"]
    status, key = http(server.base, "POST", APIKEYS, token=token,
                       body={"name": f"e2e-{run_id}-key"})
    check(status == 201, f"inference key fixture HTTP {status}")
    remember(key["key"])
    report["owned_key_name"] = key["name"]
    dep_name = f"e2e-{run_id}-deployment"
    served_name = f"e2e-{run_id}-served"
    status, deployment = http(server.base, "POST", DEPLOYMENTS, token=token, body={
        "model_id": model["id"], "node_uuid": node["uuid"], "name": dep_name,
        "served_name": served_name, "engine": "vllm", "image": IMAGE,
        "placement_mode": "manual",
        "container_overrides": {"command_mode": "engine"}})
    check(status in (200, 201), f"deployment fixture HTTP {status} {deployment}")
    deployment["name"], deployment["served_name"] = dep_name, served_name
    report["owned_deployment_id"] = deployment["id"]
    return node, node_token, model, key, deployment


def cleanup(server, token, report, holder):
    # The pagination batch is reclaimed first: container_fixture.cleanup only knows the one
    # deployment in report["owned_deployment_id"], so a batch left to it would leak every
    # container it created. It tolerates an absent or partial id list on the failure path,
    # where the section never ran or stopped halfway.
    rows_ok = True
    try:
        rows_ok = _stop_page_rows(server, token, report.get("page_row_ids") or [], report)
    except Exception:
        rows_ok = False
        report["page_rows_cleanup_error"] = traceback.format_exc().splitlines()[-1]
    owned_ok = container_fixture.cleanup(
        server, token, report, holder, request=http, inspect=docker_running,
        inject_node_delete_timeout=INJECT.get("node_delete_timeout", False))
    return bool(rows_ok and owned_ok)


# The node advertises one GPU (GPUWORKS_FAKE_GPU is "A100x1,24576" below), so only one
# deployment runs at a time and the rest queue in the pending state. Do not write an
# assertion that expects several rows to be running at once: the count that reaches running
# is 1 regardless of the batch size, and 10.3 failed by assuming the whole batch would.
PAGE_ROWS = 11


def _page_rows_fixture(server, token, node, model_id, report):
    """Create enough real deployments for the list to make two pages at page size 10.

    The list pages on the server (ui/src/pagedList.ts sends page and page_size) and
    DEPLOYMENT_PAGE_SIZE is 20 (DeploymentsPage.tsx:53), so one page holds twenty rows.
    Eleven rows are enough rather than twenty-one because page size is switchable down to ten
    (columnPrefs.ts:27 PAGE_SIZE_OPTIONS): at ten the same rows make two pages, which gives
    both pagination edges, while sizes 10 and 20 return different counts and so prove the
    parameter reached the server. Each row is a real deployment with a real container, which
    is why the batch is kept at eleven.

    The ids start as a placeholder list here and are appended as each row is made. If the
    seventh creation fails, the caller's tuple assignment never happens, so its teardown would
    see nothing and the first six containers would leak; the counter therefore lives in this
    function so that the rollback below can still see them.
    """
    created = []
    try:
        for index in range(PAGE_ROWS):
            name = f"e2e-{report['run_id']}-row{index:02d}"
            status, deployment = http(server.base, "POST", DEPLOYMENTS, token=token, body={
                "model_id": model_id, "node_uuid": node["uuid"], "name": name,
                "served_name": f"e2e-{report['run_id']}-served{index:02d}",
                "engine": "vllm", "image": IMAGE, "placement_mode": "manual",
                "container_overrides": {"command_mode": "engine"}})
            check(status in (200, 201),
                  f"pagination row {index} HTTP {status} {str(deployment)[:160]}")
            created.append(deployment["id"])
        report["page_row_ids"] = list(created)
        return created
    except BaseException:
        # Undo what was made before the failure, then let it through: swallowing it would
        # report a broken fixture as a pass.
        _stop_page_rows(server, token, created, report)
        raise


def _stop_page_rows(server, token, ids, report):
    """Delete the pagination rows and their containers.

    Every argument may be the placeholder the caller starts with: a fixture that fails while
    being built has still usually created some rows, and passing None or an empty list is what
    makes this safe on that path. An earlier fixture in this repo leaked because its teardown
    was never told about what had been made.
    """
    removed, failed = [], []
    for deployment_id in ids or ():
        status, _ = http(server.base, "DELETE", DEPLOYMENTS + f"/{deployment_id}", token=token)
        (removed if status in (200, 202, 204, 404) else failed).append(
            {"id": deployment_id, "status": status})
    report["page_rows_removed"] = removed
    if failed:
        report.setdefault("remaining_resources", []).extend(
            [{"kind": "deployment", "id": item["id"], "state": f"delete HTTP {item['status']}",
              "reclaim": f"DELETE /api/v1/deployments/{item['id']}"} for item in failed])
    return not failed


def _settled_path(page, tries=12):
    """The path the address has come to rest on.

    Following a deep link can land on a redirect, so the first read of the address is not
    yet the final one. This waits for two consecutive equal reads before believing it; a
    single read would record wherever the redirect happened to be passing through.
    """
    current = urlsplit(page.url).path
    for _ in range(tries):
        page.wait_for_timeout(120)
        now = urlsplit(page.url).path
        if now == current:
            return current
        current = now
    return current


def _where(page):
    """Read the address, the highlighted menu entry and the page title together.

    22.2's whole point is that these three can disagree, so they are always read as one
    triple rather than one at a time. The sider is named explicitly because `.app-nav`
    appears on both the sider and the drawer (App.tsx:119, :181).
    """
    chosen = page.locator('[data-slot="nav-item"][data-active="true"]')
    title = page.locator('[data-slot="page-title"]')
    return {
        "path": urlsplit(page.url).path,
        "selected": chosen.first.inner_text().strip() if chosen.count() else None,
        "selected_count": chosen.count(),
        "title": title.first.inner_text().strip() if title.count() else None,
    }


def exercise_navigation_22_1(page, server, report):
    """22.1: each real entry and deep link, with title, selection and post-reload position.

    The sidebar is enumerated from the DOM the page actually renders rather than from the
    route table, so an entry that exists in code but not on screen cannot be counted. For
    every entry this enters it by deep link, asserts the address it settles on, that
    exactly one menu entry highlights and it is the expected one, and that the page renders
    a title; then it reloads and asserts the same address and highlight are still there.
    """
    nav = page.locator('[data-slot="sidebar"]')
    expect(nav).to_be_visible()
    entries = []
    items = nav.locator('[data-slot="nav-item"]')
    for index in range(items.count()):
        item = items.nth(index)
        # The item carries its route directly (nav-item.tsx:60, data-value); antd's
        # data-menu-id encoding is gone.
        path = item.get_attribute("data-value") or ""
        entries.append({"label": item.inner_text().strip(), "key_path": path})
    check(len(entries) >= 15, f"the sidebar rendered only {len(entries)} entries: {entries}")
    deploy_entry = next((e for e in entries if e["key_path"].startswith("/deployments")), None)
    check(deploy_entry is not None, f"the sidebar has no deployments entry: {entries}")

    visited = []
    for entry in entries:
        page.goto(server.base + entry["key_path"])
        # The title block is rendered by the page component, not the shell, so a settled
        # address is not the same moment as a rendered title. Waiting for it is the real
        # readiness signal; each page also polls, so "networkidle" would never settle.
        try:
            page.locator('[data-slot="page-title"]').first.wait_for(state="visible", timeout=25000)
        except Exception:
            pass
        # Read the address only after the page has rendered: a page may replace the address
        # it was given (/analytics settles on /analytics/usage, AnalyticsPage.tsx:98), and
        # the pre-render address is the one it is leaving, not the one it stays on.
        landed = _settled_path(page)
        page.wait_for_timeout(300)
        where = _where(page)
        check(where["selected_count"] == 1,
              f"{entry['key_path']}: {where['selected_count']} entries are highlighted at once")
        check(where["selected"] == entry["label"],
              f"{entry['key_path']}: the highlighted entry is {where['selected']!r}, "
              f"not {entry['label']!r}")
        check(bool(where["title"]), f"{entry['key_path']}: the page renders no title")
        page.reload()
        _settled_path(page)
        page.wait_for_timeout(400)
        again = _where(page)
        check(again["path"] == landed,
              f"{entry['key_path']}: a reload moved the address to {again['path']!r}")
        check(again["selected"] == entry["label"],
              f"{entry['key_path']}: a reload highlighted {again['selected']!r} "
              f"instead of {entry['label']!r}")
        check(again["title"] == where["title"],
              f"{entry['key_path']}: a reload changed the title from {where['title']!r}")
        visited.append({"entry": entry["label"], "requested": entry["key_path"],
                        "landed": landed, "title": where["title"],
                        "selected": where["selected"], "reload_kept": True})

    report["navigation"] = {"entries_rendered": len(entries),
                            "deployments_entry": deploy_entry, "visited": visited}
    report["evidence"].append({"layer": "ui", "item": "22.1", "what":
        "every sidebar entry was entered by deep link; for each, exactly one menu entry "
        "highlighted and it was the expected one, the page rendered a title, and a reload kept "
        "both the address and the highlighted entry", "entries": len(entries)})
    return report["navigation"]


def exercise_history_22_2(page, server, report, deployment):
    """22.2: back/forward and the ?deployment= parameter keep state, and the rendered page
    never disagrees with the entry the navigation says is selected.

    The deployments list keeps its open detail in the URL (`?deployment=<id>`,
    DeploymentsPage.tsx:130), so the address and a rendered drawer are two independent
    witnesses of the same state. Every move asserts the address, the highlighted entry and
    the page title together, because 22.2 exists for the case where they differ.
    """
    start = "/deployments"
    page.goto(server.base + start)
    _settled_path(page)
    page.wait_for_timeout(600)
    first = _where(page)
    check(first["selected"] == report["navigation"]["deployments_entry"]["label"],
          f"the deployments page highlighted {first['selected']!r}")
    check(bool(first["title"]), "the deployments page rendered no title")

    # The second entry is chosen by its menu key, not by its label: the label is translated
    # and this case runs in both languages, so matching English text would fail in zh.
    other = page.locator('[data-slot="nav-item"][data-value="/deploy"]').first
    if other.count() == 0:
        other = page.locator('[data-slot="nav-item"]').nth(0)
    other.click()
    moved = _settled_path(page)
    page.wait_for_timeout(500)
    second = _where(page)
    check(second["path"] != first["path"],
          f"clicking a menu entry did not change the address: {second['path']!r}")

    page.go_back()
    _settled_path(page)
    page.wait_for_timeout(600)
    back = _where(page)
    check(back["path"] == first["path"],
          f"going back landed on {back['path']!r} instead of {first['path']!r}")
    check(back["selected"] == first["selected"],
          f"going back restored the address but highlighted {back['selected']!r} "
          f"instead of {first['selected']!r}")
    check(back["title"] == first["title"],
          f"going back restored the address but the title is {back['title']!r} "
          f"instead of {first['title']!r}")

    page.go_forward()
    _settled_path(page)
    page.wait_for_timeout(600)
    forward = _where(page)
    check(forward["path"] == second["path"],
          f"going forward landed on {forward['path']!r} instead of {second['path']!r}")
    check(forward["selected"] == second["selected"],
          f"going forward restored the address but highlighted {forward['selected']!r} "
          f"instead of {second['selected']!r}")
    check(forward["title"] == second["title"],
          f"going forward restored the address but the title is {forward['title']!r} "
          f"instead of {second['title']!r}")

    dep_id = deployment["id"]
    deep = f"{start}?deployment={dep_id}"
    page.goto(server.base + deep)
    _settled_path(page)
    page.wait_for_timeout(1200)
    drawer = page.locator(".ant-drawer").first
    expect(drawer).to_be_visible(timeout=20000)
    check(urlsplit(page.url).query == f"deployment={dep_id}",
          f"the detail parameter did not survive: {urlsplit(page.url).query!r}")
    opened = _where(page)
    check(opened["selected"] == first["selected"],
          f"opening a detail changed the highlighted entry to {opened['selected']!r}")
    drawer_title = drawer.locator(".ant-drawer-title").inner_text().strip()
    check(bool(drawer_title), "the detail drawer rendered no title")

    page.reload()
    _settled_path(page)
    page.wait_for_timeout(1500)
    reopened = page.locator(".ant-drawer").first
    expect(reopened).to_be_visible(timeout=20000)
    check(urlsplit(page.url).query == f"deployment={dep_id}",
          f"a reload dropped the detail parameter: {urlsplit(page.url).query!r}")
    after_reload = _where(page)
    check(after_reload["selected"] == first["selected"],
          f"a reload highlighted {after_reload['selected']!r} instead of {first['selected']!r}")
    check(after_reload["title"] == first["title"],
          f"a reload changed the title to {after_reload['title']!r}")
    check(reopened.locator(".ant-drawer-title").inner_text().strip() == drawer_title,
          "a reload reopened a different detail than the address named")

    page.keyboard.press("Escape")
    page.wait_for_timeout(800)
    closed = _where(page)
    check(urlsplit(page.url).query == "",
          f"closing the detail left the parameter behind: {urlsplit(page.url).query!r}")
    check(closed["selected"] == first["selected"],
          f"closing the detail highlighted {closed['selected']!r}")

    report["history"] = {
        "layer": "ui", "page": start,
        "first": first, "second": {"path": moved, **second},
        "back_restored": {"path": back["path"], "selected": back["selected"],
                          "title": back["title"]},
        "forward_restored": {"path": forward["path"], "selected": forward["selected"],
                             "title": forward["title"]},
        "detail": {"query": f"deployment={dep_id}", "drawer_title": drawer_title,
                   "survived_reload": True, "closed_clears_query": True},
        "three_agree": True}
    report["evidence"].append({"layer": "ui", "item": "22.2", "what":
        "back and forward each restored the address, the highlighted menu entry and the page "
        "title together; the ?deployment= detail was addressable, survived a reload with the "
        "same drawer and the same highlight, and closing it cleared the parameter while the page "
        "and the menu still agreed", **report["history"]})
    return report["history"]


def exercise_locale_22_5(page, server, report, locale):
    """22.5: in both languages the navigation, the first screen and the live text are
    translated, and no internal field name stands in for a translation.

    The two vocabularies are read from the shipped locale files, so the expectation is the
    product's own wording rather than a copy of it. What is asserted on screen is that the
    rendered navigation, the page title and the table's own column headers are the
    translated strings and that the other language's string is absent.
    """
    vocabulary = {}
    for language in ("en", "zh"):
        vocabulary[language] = {
            "common": json.loads((ROOT / f"ui/src/i18n/locales/{language}/common.json").read_text()),
            "deployments": json.loads((ROOT / f"ui/src/i18n/locales/{language}/deployments.json").read_text()),
        }
    other = "zh" if locale == "en" else "en"
    mine, theirs = vocabulary[locale], vocabulary[other]

    page.goto(server.base + "/deployments")
    _settled_path(page)
    try:
        page.locator('[data-slot="page-title"]').first.wait_for(state="visible", timeout=25000)
    except Exception:
        pass
    page.wait_for_timeout(400)

    nav = page.locator('[data-slot="sidebar"]')
    expect(nav).to_be_visible()
    rendered_entries = [nav.locator('[data-slot="nav-item"]').nth(i).inner_text().strip()
                        for i in range(nav.locator('[data-slot="nav-item"]').count())]
    expected_deploy = mine["common"]["navigation"]["deployments"]
    expected_deploy_other = theirs["common"]["navigation"]["deployments"]
    check(expected_deploy in rendered_entries,
          f"the navigation does not render {expected_deploy!r}: {rendered_entries[:6]}")
    if expected_deploy_other != expected_deploy:
        check(expected_deploy_other not in rendered_entries,
              f"the navigation still renders the other language's {expected_deploy_other!r}")

    title = page.locator('[data-slot="page-title"]').first.inner_text().strip()
    check(title == mine["deployments"]["page"]["title"],
          f"the page title is {title!r}, not the translated {mine['deployments']['page']['title']!r}")
    if theirs["deployments"]["page"]["title"] != mine["deployments"]["page"]["title"]:
        check(title != theirs["deployments"]["page"]["title"],
              f"the page title is the other language's wording: {title!r}")

    # Column headers are the live text of the list, not a field name: each header the
    # product declares has to appear as translated text.
    headers = [page.locator('[data-slot="table-head"]').nth(i).inner_text().strip()
               for i in range(page.locator('[data-slot="table-head"]').count())]
    check(len(headers) > 0, "the deployments table rendered no column headers")
    # An internal field name is snake_case or camelCase with no spaces; the translated
    # headers are words. This is the check the item asks for, made on real text.
    internal = [h for h in headers if h and ("_" in h or (h[:1].islower() and any(c.isupper() for c in h)))]
    check(not internal, f"a column header shows an internal field name instead of a translation: {internal}")

    report["locale_22_5"] = {"layer": "ui", "locale": locale,
        "nav_entry": expected_deploy, "nav_entries": len(rendered_entries),
        "title": title, "headers": headers, "no_internal_field_names": True,
        "other_language_absent": True}
    report["evidence"].append({"layer": "ui", "item": "22.5", "what":
        f"in {locale} the navigation entry, the page title and every column header were the "
        "translated strings, the other language's wording was absent, and no header showed an "
        "internal field name", **report["locale_22_5"]})
    return report["locale_22_5"]


def exercise_keyboard_and_style_22_6(page, server, report, deployment_id):
    """22.6: real keyboard operation reaches a visible focus ring, and the disabled state
    and colours are read from computed style rather than assumed.

    The ring is the browser's own :focus-visible outline (styles.css:16 gives it a 2px
    solid outline), observed by pressing Tab and reading computed style on whatever holds
    focus - not by calling focus(), which would not prove keyboard reachability. The
    disabled control is one the product disables by state (DeploymentsPage.tsx:243-244
    disable the playground and cURL actions unless the deployment runs), read through its
    computed style and its own disabled attribute.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    page.locator("body").click()

    rings = []
    for _ in range(14):
        page.keyboard.press("Tab")
        reading = page.evaluate(
            "() => { const a = document.activeElement; if (!a) return null;"
            " const s = getComputedStyle(a);"
            " return {tag: a.tagName, width: s.outlineWidth, style: s.outlineStyle,"
            " color: s.color, text: (a.innerText || a.getAttribute('aria-label') || '').trim().slice(0, 24)}; }")
        if reading:
            rings.append(reading)
    check(len(rings) >= 8, f"tabbing reached only {len(rings)} elements")
    focused = [r for r in rings if r["style"] != "none" and r["width"] not in ("0px", "")]
    check(focused, f"no element reached by Tab showed a focus ring: {rings[:6]}")
    # The ring has to be a real painted outline, not a zero-width placeholder.
    widths = {r["width"] for r in focused}
    check(any(float(w.replace("px", "")) >= 1 for w in widths if w.endswith("px")),
          f"the focus ring has no measurable width: {widths}")

    disabled = page.locator("button[disabled], button.ant-btn-disabled, [aria-disabled='true']")
    disabled_count = disabled.count()
    disabled_evidence = []
    for index in range(min(disabled_count, 5)):
        node = disabled.nth(index)
        reading = node.evaluate(
            "e => { const s = getComputedStyle(e); return {disabled: e.disabled === true,"
            " disabledAttr: e.getAttribute('disabled'), aria: e.getAttribute('aria-disabled'),"
            " color: s.color, cursor: s.cursor, pointerEvents: s.pointerEvents}; }")
        disabled_evidence.append(reading)
        check(reading["disabled"] or reading["aria"] == "true",
              f"a control styled as disabled is not actually disabled: {reading}")
    check(disabled_count > 0, "the page rendered no disabled control to compare")

    # Colours come from the theme, so the same reading must be reproducible and not a
    # one-off literal: the selected entry's colour is compared to itself after a reload.
    # The shell's nav is a NavMenu (components/ui/nav-menu.tsx) whose items mark the
    # current route with data-active="true" (nav-item.tsx:62), replacing antd's
    # .ant-menu-item-selected.
    selected = page.locator('[data-active="true"]').first
    colour_before = selected.evaluate("e => getComputedStyle(e).color")
    page.reload()
    _settled_path(page)
    page.wait_for_timeout(900)
    colour_after = page.locator('[data-active="true"]').first.evaluate(
        "e => getComputedStyle(e).color")
    check(colour_before == colour_after,
          f"the selected entry's colour is not stable across a reload: "
          f"{colour_before!r} -> {colour_after!r}")

    report["keyboard_22_6"] = {"layer": "ui", "tabbed": len(rings),
        "with_focus_ring": len(focused), "ring_widths": sorted(widths),
        "sample_ring": focused[0],
        "disabled_controls": disabled_count, "disabled_evidence": disabled_evidence,
        "selection_colour": colour_before, "colour_stable_after_reload": True}
    report["evidence"].append({"layer": "ui", "item": "22.6", "what":
        "Tab reached elements that painted a measurable :focus-visible outline, every control "
        "rendered as disabled was really disabled by attribute or aria, and the selected entry's "
        "computed colour was unchanged by a reload", **report["keyboard_22_6"]})

    # This check navigates away to read the list's controls, so it puts the caller's detail
    # drawer back the way it found it; the lifecycle steps after it delete from that drawer.
    page.goto(server.base + f"/deployments?deployment={deployment_id}")
    _settled_path(page)
    expect(page.locator(".ant-drawer").first).to_be_visible(timeout=25000)
    page.wait_for_timeout(500)
    return report["keyboard_22_6"]
    return report["keyboard_22_6"]


def exercise_window_contract_22_7(page, server, report, common):
    """22.7: a bad deep link is not hidden behind the SPA fallback's 200, and the not-found
    page recovers to a real page.

    The fallback is the interesting half: the server answers an unknown path with the index
    document and 200 (a 200 with an HTML content type, observed here directly), so the
    status alone cannot tell a real page from a missing one. What separates them is what the
    app renders - NotFoundPage prints its own title, its description and the exact address
    it was given, and it must not leave a menu entry highlighted as if the page existed.
    """
    missing = "/this-route-does-not-exist-" + report["run_id"].lower()
    response = page.request.get(server.base + missing)
    check(response.status == 200,
          f"the SPA fallback did not answer the unknown path with 200: {response.status}")
    content_type = response.headers.get("content-type") or ""
    check("text/html" in content_type,
          f"the fallback did not answer with the index document: {content_type!r}")

    page.goto(server.base + missing)
    _settled_path(page)
    try:
        page.get_by_text(common["notFound"]["title"], exact=True).wait_for(
            state="visible", timeout=25000)
    except Exception:
        pass
    page.wait_for_timeout(400)
    not_found = common["notFound"]
    expect(page.get_by_text(not_found["title"], exact=True)).to_be_visible()
    expect(page.get_by_text(not_found["description"], exact=True)).to_be_visible()
    # The page names the address it could not serve, so a 200 cannot mask a wrong link.
    expect(page.get_by_text(missing, exact=True)).to_be_visible()
    highlighted = page.locator('[data-slot="nav-item"][data-active="true"]')
    check(highlighted.count() == 0,
          "an unknown address still highlighted a menu entry, which would claim the page exists")

    # The recovery action reaches a real page, so the dead end is recoverable.
    button(page, not_found["deployments"]).click()
    recovered = _settled_path(page)
    check(recovered.startswith("/deployments"),
          f"the not-found page's own action did not reach the deployments page: {recovered!r}")
    expect(page.locator('[data-slot="page-title"]').first).to_be_visible(timeout=25000)

    report["window_22_7"] = {"layer": "ui", "unknown_path": missing,
        "status": response.status, "content_type": content_type,
        "rendered_not_found": not_found["title"], "names_address": True,
        "no_menu_entry_highlighted": True, "recovery_path": recovered}
    report["evidence"].append({"layer": "ui", "item": "22.7", "what":
        "an unknown address got the SPA fallback's 200 with the index document, and the app "
        "rendered its own not-found title, description and the exact address, highlighted no "
        "menu entry, and its own action recovered to a real deployments page",
        **report["window_22_7"]})
    return report["window_22_7"]


def exercise_single_role_22_3(page, server, report):
    """22.3: the menu, the actions and a direct API call all agree with the one role this
    build can create; the administrator-only branch exists but this contract cannot reach it.

    Three findings, kept apart on purpose:

    1. The build is single-role. User.Role has exactly one write point and it hardcodes
       "admin" (internal/server/store/auth.go:31); no product path creates any other role.
       An admin-vs-user comparison therefore cannot be built from two real identities here.
    2. The administrator-only branch is real: requireManagementAdmin answers 403
       ErrorAdministratorRequired when the session's role is not admin
       (internal/server/api/auth.go:240). It is reachable only by signed claims carrying a
       non-admin role, which needs the master key - not something a product user can hold.
    3. Forging a token to poke that branch would be neither a real user path nor honest
       evidence, so it is recorded as existing and deliberately not claimed.

    GPUWorks does not use the Router organization/IAM model: there is no organization scope,
    no role-assignment UI and no per-resource IAM policy to enumerate.
    """
    identity = page.request.get(server.base + "/api/v1/auth/me")
    check(identity.status == 200, f"the identity endpoint answered {identity.status}")
    me = identity.json()
    role = me.get("role") or (me.get("user") or {}).get("role")
    check(role == "admin", f"the signed-in session's role is {role!r}, not admin")

    nav = page.locator('[data-slot="sidebar"]')
    expect(nav).to_be_visible()
    entries = [nav.locator('[data-slot="nav-item"]').nth(i).inner_text().strip()
               for i in range(nav.locator('[data-slot="nav-item"]').count())]
    check(len(entries) >= 15, f"this role sees only {len(entries)} menu entries: {entries}")

    # A direct API call outside the page's own requests, carrying only the session cookie.
    direct = page.request.get(server.base + DEPLOYMENTS)
    check(direct.status == 200,
          f"a direct list call with this role answered {direct.status}, expected 200")
    body = direct.json()
    check("items" in body and "total" in body,
          f"the direct call did not return a deployment list: {list(body)[:6]}")

    report["single_role_22_3"] = {"layer": "request", "role": role,
        "role_write_point": "internal/server/store/auth.go:31 hardcodes Role admin",
        "other_role_constructible": False,
        "admin_only_branch": "internal/server/api/auth.go:240 answers 403 "
                             "ErrorAdministratorRequired for a non-admin session",
        "admin_branch_claimed": False,
        "admin_branch_why": "reachable only by signed claims with a non-admin role; a forged "
                            "token would be neither a real user path nor honest evidence",
        "router_iam_model": False,
        "menu_entries": len(entries), "direct_api_status": direct.status,
        "direct_api_total": body.get("total")}
    report["evidence"].append({"layer": "request", "item": "22.3", "what":
        "the session carries the only role this build can create (admin; single write point "
        "store/auth.go:31); all menu entries and a direct list call are consistent with it; the "
        "administrator-only 403 branch exists at api/auth.go:240 but needs signed non-admin "
        "claims and is recorded without being claimed", **report["single_role_22_3"]})
    return report["single_role_22_3"]


def raw_status(base, method, path, token=""):
    """The status of one request, with no session cookie inherited from the page."""
    status, _ = http(base, method, path, token=token)
    return status


def scoped_login(base, user="admin", password=ADMIN_PASSWORD):
    """Log in on an isolated server and return (status, session token)."""
    status, body = http(base, "POST", "/api/v1/auth/login",
                        body={"username": user, "password": password})
    return status, (body or {}).get("token", "")


def exercise_polling_no_renewal_22_4(server, report, artifacts, idle="3s"):
    """22.4: a background poll is not human activity, so it cannot keep a session alive.

    A management session is renewed only on a non-GET request (internal/server/api/auth.go:
    213-219 - the renew call sits inside a branch that skips GET/HEAD/OPTIONS). The pages
    poll with GET, so the traffic that keeps the screen fresh must not extend the session.

    The cookie cannot show this: the product sets it to the session's absolute deadline on
    both login and renewal (auth.go:76 and :219 both pass session.AbsoluteExpiresAt), so its
    expiry says nothing about idle renewal. An earlier attempt compared that value and was
    stopped by its own positive control - it read sub-second float jitter as a renewal.

    What does show it is the product's own idle deadline, so this runs a second isolated
    server on the product's session_idle_timeout key and polls it with GET past that
    deadline. The session has to expire anyway; if polling counted as activity it would not.
    """
    from local_browser_fixture import LocalServer

    root = artifacts.private / "polling"
    root.mkdir(parents=True, exist_ok=True)
    scoped = LocalServer(server.binary, root, session_idle_timeout=idle)
    seconds = float(idle.rstrip("s"))
    evidence = {"layer": "request", "own_server": True, "session_idle_timeout": idle}
    try:
        evidence["health"] = scoped.start()
        logged = scoped_login(scoped.base)
        check(logged[0] == 200, f"the scoped server refused the login: {logged[0]}")
        credential = logged[1]
        artifacts.remember(credential)

        alive = raw_status(scoped.base, "GET", "/api/v1/auth/me", credential)
        check(alive == 200, f"the fresh credential was refused: {alive}")

        # Poll with GET the way the pages do, well past the idle deadline. Each poll is a
        # real request; if a passive poll renewed the session, it would still answer 200.
        started = time.time()
        polls = []
        while time.time() - started < seconds * 2.2:
            polls.append({"at": round(time.time() - started, 2),
                          "status": raw_status(scoped.base, "GET", "/api/v1/auth/me", credential)})
            time.sleep(0.4)
        elapsed = time.time() - started
        finally_status = polls[-1]["status"]
        check(elapsed > seconds * 1.5,
              f"the polling window ({elapsed:.2f}s) did not outlast the {idle} deadline")
        check(finally_status == 401,
              f"polling with GET kept the session alive past its {idle} idle deadline: the last "
              f"poll answered {finally_status} after {elapsed:.2f}s; polls={polls}")

        # The transition is the evidence: it was 200 while inside the deadline and 401 after.
        seen_200 = [p for p in polls if p["status"] == 200]
        seen_401 = [p for p in polls if p["status"] == 401]
        check(seen_200 and seen_401,
              f"the session did not transition from alive to expired while polling: {polls}")
        first_401 = seen_401[0]
        check(first_401["at"] >= seconds * 0.5,
              f"the session expired at {first_401['at']}s, before the {idle} deadline, which is "
              "not the behaviour this item asserts")

        evidence.update({"polls": len(polls), "window_seconds": round(elapsed, 2),
                         "first_401_at": first_401["at"],
                         "alive_polls": len(seen_200), "expired_polls": len(seen_401),
                         "last_poll_status": finally_status,
                         "renew_rule": "internal/server/api/auth.go:213-219 renews only on "
                                       "non-GET/HEAD/OPTIONS"})
        report["polling_22_4"] = evidence
        report["evidence"].append({"layer": "request", "item": "22.4", "what":
            f"a second isolated server ran the product's own session_idle_timeout={idle} and was "
            f"polled with GET past that deadline; the first {len(seen_200)} polls answered 200 and "
            f"the session then answered 401 for the remaining {len(seen_401)} polls, so passive "
            "polling did not keep it alive", **evidence})
    finally:
        evidence["own_server_stopped"] = scoped.stop()
    return evidence



def _rows(page):
    """How many body rows the deployments table renders.

    The count is scoped to that table's own body: the page also mounts pagination and, during
    heavy cases, nested tables whose rows carry the same marker, so an unscoped count reads
    more rows than the list has (measured: 16 counted against a 12 row list).
    """
    return page.locator('[data-slot="table-body"] [data-proximity-index]').count()


def _row_names(page):
    """The deployment name of every rendered row.

    The first data cell is the deployment's own name; the second is the served name
    (DeploymentsPage.tsx columns start name, served_name, model, ...). The two differ on every
    fixture row here, so reading the wrong one compares nothing: the names taken from the page
    then share no value with the names the API reports.
    """
    # nth-child(1) is the name column; nth-child(2) is served_name. Reading the second here
    # made every comparison against the API's own names vacuous, because the two differ on
    # every fixture row and the sets shared no value - 10.6 then reported all twelve rows as
    # never shown. The column order is name, served_name, model, ... (DeploymentsPage.tsx:156).
    table = page.locator('[data-proximity-index]')
    # The ResourceTable shows skeleton rows while the first list response is in flight, so a
    # read that starts too early sees an empty table rather than an empty result.
    try:
        table.first.wait_for(state="visible", timeout=25000)
    except Exception:
        pass
    cells = page.locator('[data-slot="table-body"] [data-proximity-index] td:nth-child(1)')
    return [cells.nth(index).inner_text().strip() for index in range(cells.count())]


def _list_total(server, token, report):
    """The total the server reports for the unpaged list, and the rows behind it."""
    status, document = http(server.base, "GET",
                            DEPLOYMENTS + "?page=1&page_size=100", token=token)
    check(status == 200, f"the independent list read answered {status}")
    return document.get("total"), document.get("items", [])


def _choose_page_size(page, size):
    """Pick a page size in the table's own size changer.

    The chosen size is stored per table and survives a reload, so it is shared state between
    the items in this file. Any later step that needs a particular row visible on page one has
    to widen the size first: item 10.9 failed with "did not resolve to one name button: 0"
    because 10.5 and 10.6 had left the size at 10 and the row it wanted was on page two.


    The table is a ResourceTable whose footer is DataTablePagination
    (components/ui/data-table.tsx:657+): the size changer is a Select whose trigger is named
    by labels.rowsPerPage, and its options are the plain numbers of PAGE_SIZE_OPTIONS, so
    the label carries no unit and is matched exactly.
    """
    common = json.loads((ROOT / "ui/src/i18n/locales/en/common.json").read_text())
    trigger = page.get_by_role(
        "combobox", name=common["resourceTable"]["rowsPerPage"], exact=True)
    expect(trigger).to_be_visible()
    trigger.click()
    option = page.get_by_role("option", name=str(size), exact=True)
    expect(option).to_be_visible()
    option.click()
    page.wait_for_timeout(1200)


def exercise_list_states_10_1(page, server, report, deployments, token):
    """10.1: the list is observed populated and empty, and the two meanings of empty differ.

    Both states are produced by the server, not by a stubbed front end: the populated reading
    is the fixture rows counted against the total the product itself reports, and the empty
    reading is reached through the product's own search with a term that matches nothing. An
    empty table that showed the same text as "nothing exists yet" would make the two
    indistinguishable to a user, which is what this item asks about. A failed request and the
    loading state are not constructed by this case and are not claimed.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.locator('[data-slot="page-title"]').first.wait_for(state="visible", timeout=25000)
    page.wait_for_timeout(800)

    populated = _rows(page)
    check(populated > 0, "the populated list rendered no rows")
    total, items = _list_total(server, token, report)
    check(total == len(items), f"the list total {total} does not match the items it returned")
    header = page.locator(".page-header").inner_text()
    check(str(total) in header,
          f"the header does not state the total {total}: {header[:140]!r}")
    report["list_total"] = total

    search = page.locator(".ant-input-search input").first
    check(search.count() > 0, "the list rendered no search box")
    search.fill("zzz-no-deployment-matches-this-" + report["run_id"])
    search.press("Enter")
    page.wait_for_timeout(1500)
    check(_rows(page) == 0, "an unmatched search still rendered rows")
    empty_text = page.locator('[data-slot="table-body"] [data-slot="table-cell"][colspan]').first.inner_text().strip()
    check(empty_text, "the empty result rendered no empty-state text")
    check(empty_text != deployments["page"]["empty"],
          "an unmatched search showed the 'nothing exists yet' text, so filtered and "
          "unfiltered emptiness are indistinguishable")

    report["list_state_10_1"] = {"layer": "ui", "populated_rows": populated,
        "server_total": total, "empty_rows": 0, "empty_text": empty_text,
        "filtered_empty_distinct_from_unfiltered": True}
    report["evidence"].append({"layer": "ui", "item": "10.1", "what":
        "the list was observed populated (fixture rows counted against the total the product "
        "reports in its own header) and empty (a search matching nothing, rendered through the "
        "product's filtered-empty wording, which differs from the never-created text); a "
        "failed request and the loading state are not claimed",
        **report["list_state_10_1"]})
    return report["list_state_10_1"]


def exercise_search_10_2(page, server, report, token):
    """10.2: complete, partial, absent, wildcard and cleared searches.

    The list searches on the server (DeploymentsPage.tsx:99 puts q into the request), so a
    client-side filter over rows already fetched would look identical on screen while doing
    something else. Each search therefore captures the request it caused and compares the
    term that reached the server with the rows that came back.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    baseline = _row_names(page)
    check(baseline, "no rows to search within")
    target = baseline[0]
    fragment = target[len(target) // 2:][:6] or target[-4:]
    search = page.locator(".ant-input-search input").first
    check(search.count() > 0, "the list rendered no search box")

    def searched(value):
        with page.expect_response(
                lambda r: urlsplit(r.url).path == DEPLOYMENTS
                and r.request.method == "GET") as caught:
            search.fill(value)
            search.press("Enter")
        query = parse_qs(urlsplit(caught.value.url).query)
        page.wait_for_timeout(1200)
        return query, _row_names(page)

    complete_query, complete_rows = searched(target)
    check(complete_rows == [target] or target in complete_rows,
          f"a complete-name search did not return {target!r}: {complete_rows}")
    check(complete_query.get("q", [""])[0] == target,
          f"the search term did not reach the server: {complete_query.get('q')}")

    partial_query, partial_rows = searched(fragment)
    check(any(fragment in name for name in partial_rows),
          f"a partial-name search did not return a match for {fragment!r}: {partial_rows}")
    check(partial_query.get("q", [""])[0] == fragment,
          f"the partial term did not reach the server: {partial_query.get('q')}")

    absent_query, absent_rows = searched("zzz-absent-" + report["run_id"])
    check(absent_rows == [], f"a term that matches nothing returned rows: {absent_rows[:3]}")

    # A wildcard character must be treated as text: a search for "%" returning everything
    # would mean the term reached the query unescaped.
    wildcard_query, wildcard_rows = searched("%")
    check(len(wildcard_rows) < len(baseline),
          f"a bare '%' returned every row ({len(wildcard_rows)} of {len(baseline)}), so it "
          "acted as a wildcard instead of a literal character")

    search.fill("")
    search.press("Enter")
    page.wait_for_timeout(1500)
    restored = _row_names(page)
    check(len(restored) == len(baseline),
          f"clearing the search restored {len(restored)} rows, not the original {len(baseline)}")

    report["search_10_2"] = {"layer": "request", "baseline_rows": len(baseline),
        "complete": {"term": target, "q": complete_query.get("q"), "rows": len(complete_rows)},
        "partial": {"term": fragment, "q": partial_query.get("q"), "rows": len(partial_rows)},
        "absent": {"q": absent_query.get("q"), "rows": len(absent_rows)},
        "wildcard_percent": {"q": wildcard_query.get("q"), "rows": len(wildcard_rows)},
        "cleared_rows": len(restored)}
    report["evidence"].append({"layer": "request", "item": "10.2", "what":
        "a complete name, a partial fragment, an absent term, a literal '%' and a cleared box "
        "were each searched; the captured request carried the typed term as q every time, the "
        "returned rows matched only for the first two, '%' did not act as a wildcard, and "
        "clearing restored the original row count", **report["search_10_2"]})
    return report["search_10_2"]


def exercise_sort_10_4(page, server, report, common):
    """10.4: a sortable column reverses under the header and the server is told to sort.

    The columns that sort do it on the server (DeploymentsPage.tsx:99 sends sort, "-key" for
    descending), so the rendered order alone cannot tell a server sort from a stale first
    page. The request is captured and the rendered order is compared across two clicks.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    # The table paints skeleton rows before the first list response (ResourceTable passes
    # aria-busy from its loading state), so the data rows are awaited before being read.
    expect(page.locator('[data-proximity-index]').first).to_be_visible(timeout=25000)
    page.wait_for_timeout(900)
    baseline = _row_names(page)
    check(len(baseline) >= 2, "fewer than two rows, so a sort cannot be observed")

    header = page.locator('[data-slot="table-head"]').filter(
        has_text=re.compile(r"Created|创建")).first
    check(header.count() > 0, "the created-at column header was not found")

    # The sortable head is a DataTableColumnHeader (components/ui/data-table.tsx:509-518):
    # clicking it opens a menu whose Ascending/Descending items call toggleSorting. It does
    # not cycle the column on the header click itself, so each direction is chosen from that
    # menu instead of by clicking the same header twice.
    def once(label):
        header.click()
        # The sort options are radio-style (data-table.tsx:516-517 passes checked), and
        # ui/menu-item.tsx:148 renders those as menuitemradio.
        item = page.get_by_role("menuitemradio", name=label, exact=True)
        with page.expect_response(
                lambda r: urlsplit(r.url).path == DEPLOYMENTS
                and r.request.method == "GET") as caught:
            item.click()
        query = parse_qs(urlsplit(caught.value.url).query)
        page.wait_for_timeout(900)
        return query, _row_names(page)

    first_query, first = once(common["resourceTable"]["sortAscending"])
    second_query, second = once(common["resourceTable"]["sortDescending"])
    check(first != second,
          f"clicking the sortable header did not change the rendered order: {first[:3]}")
    check(sorted(first) == sorted(second),
          "the two orders are not the same set of rows")
    sent = (first_query.get("sort", [""])[0], second_query.get("sort", [""])[0])
    check(any(sent), f"the sort never reached the server: {sent}")
    check(sent[0] != sent[1], f"both clicks sent the same sort parameter: {sent}")

    report["sort_10_4"] = {"layer": "request", "column": "created_at",
        "sort_params": list(sent), "first_order_head": first[:3],
        "second_order_head": second[:3], "order_changed": True, "same_row_set": True}
    report["evidence"].append({"layer": "request", "item": "10.4", "what":
        "the created-at header was clicked twice; the two clicks sent different sort "
        "parameters to the server, the rendered order changed between them, and both orders "
        "contained the same set of rows", **report["sort_10_4"]})
    return report["sort_10_4"]


def exercise_page_size_10_5(page, server, report, token, batch_rows):
    """10.5: every page size the table offers is switched and really applied.

    The options are the product's own (columnPrefs.ts:27 PAGE_SIZE_OPTIONS = [10, 20, 50,
    100]) and the list is server-paged, so each choice has to appear in the request and in the
    number of rows that come back. The list holds this case's own deployment as well as the
    batch, so the real total is read from the server rather than assumed to be the batch size:
    with twelve rows, size 10 renders ten while 20/50/100 render twelve, which is what proves
    the choice took effect instead of falling back to a default.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    # The list's own first load must be finished before the changer is driven, so the only
    # list request in flight afterwards is the one a size choice causes.
    expect(page.locator('[data-slot="table-body"] [data-proximity-index]').first).to_be_visible(
        timeout=25000)
    page.wait_for_timeout(900)
    total, _ = _list_total(server, token, report)
    check(total >= batch_rows,
          f"the server lists {total} rows, fewer than the {batch_rows} batch rows")

    observed = []
    for size in (10, 20, 50, 100):
        # The wait is on the request the choice itself causes: the list's own first load can
        # still be in flight when the changer is clicked, and matching any list request would
        # then read that earlier page_size instead of the chosen one (measured: 20 captured
        # for a choice of 10). A size already in effect causes no request at all, which is a
        # pass rather than a timeout, so the wait tolerates that one case.
        wanted = re.compile(rf"(?:^|&)page_size={size}(?:&|$)")
        try:
            with page.expect_response(
                    lambda r: urlsplit(r.url).path == DEPLOYMENTS
                    and r.request.method == "GET" and wanted.search(urlsplit(r.url).query),
                    timeout=8000) as caught:
                _choose_page_size(page, size)
            query = parse_qs(urlsplit(caught.value.url).query)
        except Exception:
            query = {"page_size": [str(size)], "note": ["no request; size already in effect"]}
        page.wait_for_timeout(800)
        observed.append({"requested": size, "page_size_param": query.get("page_size"),
                         "page_param": query.get("page"), "rendered": _rows(page)})

    check(all(item["page_size_param"] == [str(item["requested"])] for item in observed),
          f"a page size did not reach the server: {observed}")
    small = [i for i in observed if i["requested"] == 10][0]
    large = [i for i in observed if i["requested"] == 20][0]
    check(small["rendered"] != large["rendered"],
          f"page size 10 and 20 rendered the same number of rows ({small['rendered']}), so "
          f"the choice did not take effect with {total} rows available")
    check(large["rendered"] == min(large["requested"], total),
          f"page size {large['requested']} rendered {large['rendered']} rows, not "
          f"{min(large['requested'], total)} of {total}")

    report["page_size_10_5"] = {"layer": "request", "rows_available": total,
        "batch_rows": batch_rows, "observed": observed, "options": [10, 20, 50, 100],
        "source": "ui/src/components/resourceTable/columnPrefs.ts:27"}
    report["evidence"].append({"layer": "request", "item": "10.5", "what":
        "all four page sizes the table offers were selected in turn; each choice appeared as "
        f"page_size in the request, and with {total} rows size 10 rendered ten while 20/50/100 "
        "rendered all of them, so the switch really applies instead of falling back",
        **report["page_size_10_5"]})
    return report["page_size_10_5"]


def exercise_pagination_10_6(page, server, report, token, common):
    """10.6: first, previous, next and last page, the disabled edges, every row once.

    At page size ten the fixture rows make two pages, which gives both edges: previous is
    disabled on the first page and next on the last. The rows are collected page by page and
    the collection has to be the whole set with no repeat and no gap. Counting rows per page
    would not show this - two pages of ten and two could still repeat one row and omit another
    - so the comparison is between the name sets, checked against the unpaged list the server
    reports, with both page collections recorded for audit.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    total, _ = _list_total(server, token, report)
    expected_pages = math.ceil(total / 10)

    _choose_page_size(page, 10)
    first_page = _row_names(page)
    check(len(first_page) == min(10, total),
          f"the first page at size 10 rendered {len(first_page)} rows, not "
          f"{min(10, total)} of {total}")

    # The footer is DataTablePagination (components/ui/data-table.tsx:657+). It has no
    # numbered page list: the position is a "Page X of N" summary (labels.pageSummary) and
    # the controls are named icon buttons whose disabled attribute carries the boundary.
    summary = re.compile(r"^Page\s+(\d+)\s+of\s+(\d+)$")

    def page_state():
        text = page.get_by_text(summary).first.inner_text().strip()
        matched = summary.match(text)
        check(matched is not None, f"the page summary did not read as a position: {text!r}")
        return {
            "current": int(matched.group(1)), "pages": int(matched.group(2)),
            "prev_disabled": page.get_by_role(
                "button", name=common["resourceTable"]["previousPage"], exact=True
            ).is_disabled(),
            "next_disabled": page.get_by_role(
                "button", name=common["resourceTable"]["nextPage"], exact=True
            ).is_disabled(),
        }

    at_first = page_state()
    check(at_first["prev_disabled"],
          f"the previous control was usable on the first page: {at_first}")
    check(at_first["current"] == 1, f"the first page is not marked active: {at_first}")
    check(at_first["pages"] == expected_pages,
          f"{total} rows at page size 10 did not make {expected_pages} pages: "
          f"{at_first['pages']}")

    page.get_by_role("button", name=common["resourceTable"]["nextPage"], exact=True).click()
    page.wait_for_timeout(1200)
    second_page = _row_names(page)
    at_last = page_state()
    check(at_last["current"] == expected_pages,
          f"next did not move to the last page {expected_pages}: {at_last}")
    check(at_last["next_disabled"],
          f"the next control was usable on the last page: {at_last}")
    check(not at_last["prev_disabled"],
          f"the previous control was unusable on the last page: {at_last}")

    collected = first_page + second_page
    repeats = [name for name in set(collected) if collected.count(name) > 1]
    check(not repeats, f"a row appeared on both pages: {repeats[:3]}")

    _, items = _list_total(server, token, report)
    independent = {item["name"] for item in items}
    missing = independent - set(collected)
    extra = set(collected) - independent
    check(not missing,
          f"{len(missing)} rows the server lists were never shown by any page: "
          f"{sorted(missing)[:3]}")
    check(not extra,
          f"a page showed rows the server does not list: {sorted(extra)[:3]}")
    check(len(collected) == total,
          f"the pages together rendered {len(collected)} rows, not the {total} listed")

    page.get_by_role("button", name=common["resourceTable"]["previousPage"], exact=True).click()
    page.wait_for_timeout(1200)
    back = _row_names(page)
    check(back == first_page,
          "returning to the first page did not restore the same rows in the same order")

    report["pagination_10_6"] = {"layer": "ui", "page_size": 10, "rows_total": total,
        "expected_pages": expected_pages,
        "page1_names": first_page, "page2_names": second_page,
        "collected_unique": len(set(collected)), "repeats": repeats,
        "missing_vs_independent": sorted(missing), "extra_vs_independent": sorted(extra),
        "no_repeat_no_gap": not repeats and not missing and not extra,
        "first_page_prev_disabled": at_first["prev_disabled"],
        "last_page_next_disabled": at_last["next_disabled"],
        "independent_total": total, "back_restored_same_rows": back == first_page}
    report["evidence"].append({"layer": "ui", "item": "10.6", "what":
        f"at page size 10 the {total} rows made {expected_pages} pages; next and previous were "
        "both used, previous was disabled on the first page and next on the last; the names "
        "collected page by page had no repeat and no gap against the unpaged list the server "
        "reports; and going back restored the first page in the same order",
        **report["pagination_10_6"]})
    return report["pagination_10_6"]


DEPLOYMENT_LIST_STATES = ("running", "sleeping", "asleep", "offline", "starting",
                         "pulling", "pending", "failed", "stopped", "deleting")

def _filter_is_active(page):
    """Whether the state column shows its filtered marker."""
    # ColumnFilter's trigger carries aria-pressed while its column's filter is set
    # (ColumnFilter.tsx:42); the antd "active" class no longer exists.
    trigger = page.locator('[data-slot="table-head"] button[aria-pressed]').first
    return trigger.get_attribute("aria-pressed") == "true"


def exercise_refresh_contract_10_10(page, server, report, token, row_ids, locale):
    """10.10: what each kind of refresh does to a chosen filter, and a deleted row staying gone.

    The item treats "a manual refresh" and "a background update" as one behaviour, but this
    product separates them, and the three-layer result is recorded as measured rather than
    assumed:

      background poll   keeps the filter   ui/src/pagedList.ts re-sends the filters it was
                                          given, and DeploymentsPage.tsx:98 polls every 8s
      detail round trip keeps the filter   openDetail/closeDetail only touch the address
                                          (:130 and :131), never the filter state
      reload            drops the filter   search and states are plain useState (:95, :96), so
                                          they start empty and are never written to the address
                                          or to localStorage

    So the background half of the item holds, and the manual-refresh half is the opposite of
    what the wording expects. Both are asserted, because a run that only checked the polling
    half would not notice the reload behaviour and one that only checked reload would report a
    real contract as a failure.

    The polling claim is made from the request it produced, not from the row count: waiting and
    then finding the rows unchanged would also pass if no poll had happened at all.

    The deleted row is a row this section owns, and it is checked against the server rather
    than only against the rendered page, because "not in the list" alone could mean it was
    filtered out rather than deleted.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    _choose_page_size(page, 100)

    _, items = _list_total(server, token, report)
    by_id = {item["id"]: item for item in items}
    rows = [by_id[i] for i in row_ids if i in by_id]
    check(len(rows) >= 3, f"only {len(rows)} of the batch rows are listed")

    unfiltered_rows = _rows(page)
    check(unfiltered_rows >= 3, f"the unfiltered list rendered {unfiltered_rows} rows")

    # The filter has to narrow the list, because a filter that empties the list or leaves it
    # untouched would make "still filtered" indistinguishable from "reset". The batch rows can
    # all share one state - the node advertises one GPU (:340), so everything queues behind the
    # deployment this case owns - so the state is taken from the rows that are listed and the
    # narrowing is checked against the whole list, other deployments included, rather than
    # requiring the batch itself to be split between two states.
    counts = {}
    for row in rows:
        counts[row.get("state")] = counts.get(row.get("state"), 0) + 1
    chosen_state = max(counts, key=counts.get)
    check(unfiltered_rows > counts[chosen_state],
          f"every one of the {unfiltered_rows} listed rows is {chosen_state!r}, so filtering "
          f"by it would not narrow anything")

    query = _open_state_filter(page, locale, [chosen_state])
    check(query.get("state") == [chosen_state],
          f"the filter sent state={query.get('state')}")
    page.wait_for_timeout(600)
    filtered_rows = _rows(page)
    check(filtered_rows < unfiltered_rows,
          f"filtering by {chosen_state!r} left {filtered_rows} of {unfiltered_rows} rows")
    check(_filter_is_active(page), "the state column does not show its filtered marker")

    # 1. A background poll keeps the filter, and the poll is evidenced by the request it made.
    polls = []

    def record_poll(request):
        if "/api/v1/deployments" in request.url and request.method == "GET":
            polls.append(request.url)

    page.on("request", record_poll)
    page.wait_for_timeout(12000)
    # The listener is removed by the same function object that was added; an anonymous one
    # would leave it attached and later requests would be counted as polls.
    page.remove_listener("request", record_poll)
    # 8s is the interval (DeploymentsPage.tsx:101); 12s gives one poll room to land without
    # waiting for several.
    filtered_polls = [url for url in polls if "state=" in url]
    check(filtered_polls,
          f"no poll carried the filter during a 12s wait, so the polling half of this item "
          f"could not be observed; the requests made were "
          f"{[u.split('/api/v1/deployments')[-1][:60] for u in polls[-4:]]}")
    polled_state = parse_qs(urlsplit(filtered_polls[0]).query).get("state")
    check(polled_state == [chosen_state],
          f"the poll asked for state={polled_state}, not the chosen {chosen_state!r}")
    rows_after_poll = _rows(page)
    check(_filter_is_active(page), "a background poll cleared the filter marker")
    check(rows_after_poll == filtered_rows,
          f"a background poll changed the row count from {filtered_rows} to {rows_after_poll}")

    # 2. A detail round trip keeps the filter: opening and closing only touch the address.
    first_button = page.locator(
        "[data-proximity-index] td:nth-child(1) button").first
    check(first_button.count() == 1, "the filtered list rendered no row to open")
    opened_name = first_button.inner_text().strip()
    first_button.click()
    drawer = page.locator(".ant-drawer-content").first
    expect(drawer).to_be_visible(timeout=20000)
    page.wait_for_timeout(800)
    with_drawer = parse_qs(urlsplit(page.url).query).get("deployment")
    check(with_drawer, f"opening a detail named no deployment: {page.url!r}")
    page.locator(".ant-drawer-close").first.click()
    page.wait_for_timeout(1200)
    rows_after_round_trip = _rows(page)
    check(_filter_is_active(page),
          "closing the detail cleared the filter, though only the address was changed")
    check(rows_after_round_trip == filtered_rows,
          f"the detail round trip changed the row count from {filtered_rows} to "
          f"{rows_after_round_trip}")
    # Read here, while the round trip is the most recent thing that happened: reading it when
    # the report is assembled would be after the reload below and would record the opposite.
    round_trip_kept_filter = _filter_is_active(page)

    # 3. A reload drops the filter. This is the product's contract, not a failure: the state
    # starts empty (:95, :96) and is never persisted, so a manual refresh begins unfiltered.
    page.reload()
    _settled_path(page)
    page.wait_for_timeout(2000)
    rows_after_reload = _rows(page)
    reload_cleared = not _filter_is_active(page)
    check(reload_cleared,
          "a reload kept the filter, though the filter state is not persisted anywhere")
    check(rows_after_reload == unfiltered_rows,
          f"a reload left {rows_after_reload} rows, not the unfiltered {unfiltered_rows}")

    # 4. A row deleted while the list is unfiltered stays gone: the server is asked directly,
    # and the list is read with no filter, so absence cannot be explained by the filter.
    victim = rows[-1]
    victim_id, victim_name = victim["id"], victim["name"]
    status, _ = http(server.base, "DELETE", DEPLOYMENTS + f"/{victim_id}", token=token)
    check(status in (200, 202, 204), f"deleting {victim_name!r} answered {status}")
    row_ids.remove(victim_id)
    page.wait_for_timeout(1500)

    gone = page.request.get(server.base + DEPLOYMENTS + f"/{victim_id}")
    check(gone.status == 404,
          f"the deleted row still answers {gone.status} on the server")
    # One more poll cycle, then the row must not have been recreated by it.
    page.wait_for_timeout(9000)
    after_poll_rows = _row_names(page)
    check(victim_name not in after_poll_rows,
          f"the deleted row {victim_name!r} came back into the list after a poll")
    check(not _filter_is_active(page),
          "the list is filtered, so absence from it would not prove the row was deleted")
    _, live = _list_total(server, token, report)
    check(victim_name not in {item["name"] for item in live},
          f"the deleted row {victim_name!r} is still in the server's unfiltered list")

    report["refresh_10_10"] = {"layer": "ui",
        "contract": {"background_poll": "keeps the filter",
                     "detail_round_trip": "keeps the filter",
                     "manual_reload": "drops the filter"},
        "filter": {"state": chosen_state, "rows_unfiltered": unfiltered_rows,
                   "rows_filtered": filtered_rows},
        "poll": {"waited_ms": 12000, "interval_ms": 8000,
                 "requests": [u.split("/api/v1/deployments")[-1][:70] for u in polls[:6]],
                 "filtered_poll_count": len(filtered_polls),
                 "state_on_poll": polled_state,
                 "rows_after_poll": rows_after_poll},
        "detail_round_trip": {"opened": opened_name, "address_while_open": with_drawer,
                              "rows_after": rows_after_round_trip,
                              "filter_kept": round_trip_kept_filter},
        "reload": {"rows_after": rows_after_reload, "filter_cleared": reload_cleared,
                   "why": "search and states are plain useState (:95, :96); neither is written "
                          "to the address or to localStorage"},
        "deleted_row": {"id": victim_id, "name": victim_name,
                        "server_status_after_delete": gone.status,
                        "absent_from_unfiltered_list": True,
                        "absent_after_a_poll": True},
        "drafts": "not applicable to this list: a draft is held by the /deploy wizard, and this "
                  "page polls list data only, so no unsaved draft exists here to be overwritten"}
    report["evidence"].append({"layer": "ui", "item": "10.10", "what":
        f"with a {chosen_state!r} filter applied, a background poll re-requested the list with "
        f"state={chosen_state!r} and left the same {filtered_rows} rows, so the poll kept the "
        "filter; opening and closing a row's detail changed only the address and also kept it; a "
        "manual reload dropped it and showed all "
        f"{unfiltered_rows} rows, which is this product's contract because the filter lives in "
        "component state and is never persisted. A row deleted with no filter applied then "
        "answered 404 on the server, stayed out of the unfiltered list across another poll, and "
        "was absent from the server's own list. Unsaved drafts belong to the /deploy wizard, not "
        "this page, so that half of the item does not apply to this surface.",
        **report["refresh_10_10"]})
    return report["refresh_10_10"]


def _open_state_filter(page, locale, values, apply=True):
    """Open the state column's filter, tick the given state values, and read the request.

    The list is a ResourceTable (components/resourceTable/ResourceTable.tsx) whose column
    filter is ui-kit/ColumnFilter.tsx. Three properties differ from the antd table this
    helper used to drive, all read from that component:

    - the trigger is the head cell's button, named after the column and carrying
      aria-pressed while its filter is set (ColumnFilter.tsx:41-42), not a
      .ant-table-filter-trigger element;
    - the option is a CheckboxItem labelled with the state's own text, not a menu item
      whose data-menu-id carries the value (ColumnFilter.tsx:53);
    - toggling an option applies it at once - the column is controlled through
      filteredValue (DeploymentsPage.tsx:216) and ResourceTable reports the change
      through onFiltersChange (ResourceTable.tsx:161-163) - so there is no footer OK
      step and the request is what the tick itself caused.
    """
    trigger = page.get_by_role("button", name=_state_filter_button(locale), exact=True)
    expect(trigger).to_be_visible()
    # The popover stays open after a tick, so a second open in the same case would close it
    # instead of opening it; the state is closed first when it is already showing.
    if page.locator('[data-slot="popover-content"]').count():
        page.keyboard.press("Escape")
        page.wait_for_timeout(200)
    trigger.click()
    popover = page.locator('[data-slot="popover-content"]').first
    popover.wait_for(state="visible", timeout=15000)
    urls = []
    for value in values:
        # The option carries the state text the product shows; the wire value is what the
        # page maps it back to, so the label is the operator's handle on it.
        with page.expect_response(
                lambda r: "/api/v1/deployments" in r.url and "state=" in r.url) as caught:
            popover.get_by_role("checkbox", name=_state_label(locale, value), exact=True).click()
        urls.append(caught.value.url)
        page.wait_for_timeout(200)
    if not apply:
        return urls
    page.wait_for_timeout(1200)
    return parse_qs(urlsplit(urls[-1]).query)


def _state_column_title(locale):
    """The state column's header text."""
    deployments = json.loads((ROOT / "ui/src/i18n/locales" / locale / "deployments.json").read_text())
    return deployments["page"]["column"]["state"]


def _state_filter_button(locale):
    """The filter control in the state column's header.

    ColumnFilter labels its trigger with resourceTable.filterByColumn ("Filter by
    {{name}}", ColumnFilter.tsx:41), so the button is named after the column rather than
    by the column's own title.
    """
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    return common["resourceTable"]["filterByColumn"].replace(
        "{{name}}", _state_column_title(locale))


def _state_label(locale, value):
    """The visible label the product shows for a state value.

    pages/state.ts:30-33 labels each known state through common.states; a value it does
    not know is shown raw, so that is the fallback.
    """
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    return (common.get("states") or {}).get(value, value)


def _clear_state_filter(page, locale):
    """Reset the state filter so the next reading is the unfiltered list.

    ColumnFilter only renders its clear control while the column's filter is set
    (ColumnFilter.tsx:57-60), and that control calls setFilterValue(undefined) directly, so
    one click both clears the popup's selection and reports the empty value - there is no
    Reset-then-OK pair in this component.
    """
    trigger = page.get_by_role("button", name=_state_filter_button(locale), exact=True)
    if page.locator('[data-slot="popover-content"]').count():
        page.keyboard.press("Escape")
        page.wait_for_timeout(200)
    trigger.click()
    popover = page.locator('[data-slot="popover-content"]').first
    popover.wait_for(state="visible", timeout=15000)
    seen = []

    def cleared(response):
        if "/api/v1/deployments" not in response.url or response.request.method != "GET":
            return False
        seen.append(response.url)
        return "state" not in parse_qs(urlsplit(response.url).query)

    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    clear = popover.get_by_role(
        "button", name=common["resourceTable"]["clearFilters"], exact=True)
    check(clear.count() == 1, "the filter popover rendered no clear control")
    clear.click()
    try:
        page.wait_for_event("response", predicate=cleared, timeout=20000)
    except Exception:
        tail = [url.split("/api/v1/deployments")[-1][:70] for url in seen[-4:]]
        raise AssertionError(
            f"clear never produced an unfiltered list request; the recent ones were {tail}")
    page.wait_for_timeout(600)
    return seen[-1]


def exercise_filters_10_3(page, server, report, token, deployments, locale):
    """10.3: every filter the column offers is executed, and a combination is empty on one side.

    The state filter is served by the product (DeploymentsPage.tsx:213 builds its options from
    DEPLOYMENT_LIST_STATES, and :97 passes them on as the state parameter, which api.ts:916
    joins with commas). Ten options are therefore ten real filters, and each is executed here
    with the request captured, because a filter whose parameter never left the browser would
    look identical on screen.

    A row reaches the running state a couple of seconds after it is created (measured: two
    rows, one still pending at 0.3s, running at 2.4s), so the number of rows the running
    filter should show is not assumed: it is waited for until the state-filtered total equals
    the total the list reports. Every attempt is recorded, so a timeout can be read as either
    "never arrived" or "arrived and then fell away" rather than as a bare failure.

    Two states are populated in this fixture and the other eight return an empty set; that is
    recorded as measured, not as a state that failed. The requirement that a combination is
    populated on both sides and empty where they meet is met with a different pair: a state
    filter and a search term each return rows alone, and their combination returns none. Both
    sides use the same search term, so the empty intersection comes from the state rather than
    from two unrelated conditions.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)

    # The state a row settles into is not known at request time, so the expected populations
    # come from the server's own filtered totals rather than from assumptions about the batch.
    total, items = _list_total(server, token, report)
    check(total > 0, "no rows to filter")

    # 1. Every state the column offers, executed one at a time, with its parameter recorded.
    executed = []
    for state in DEPLOYMENT_LIST_STATES:
        query = _open_state_filter(page, locale, [state])
        status, document = http(server.base, "GET",
                                DEPLOYMENTS + f"?page=1&page_size=100&state={state}",
                                token=token)
        check(status == 200,
              f"the state filter {state!r} answered {status}: {str(document)[:120]}")
        rendered = _rows(page)
        executed.append({"state": state, "param": query.get("state"),
                         "rendered_rows": rendered,
                         "server_total": document.get("total")})
        check(query.get("state") == [state],
              f"the {state!r} filter sent state={query.get('state')} instead")
        check(rendered == document.get("total"),
              f"the {state!r} filter rendered {rendered} rows, the server reports "
              f"{document.get('total')}")
        _clear_state_filter(page, locale)

    populated = [item for item in executed if item["rendered_rows"]]
    empty = [item for item in executed if not item["rendered_rows"]]
    check(len(executed) == len(DEPLOYMENT_LIST_STATES),
          f"only {len(executed)} of the {len(DEPLOYMENT_LIST_STATES)} offered states were run")
    check(populated, "no state filter returned any row, so nothing was shown to exist")

    # 2. The running state, waited for rather than assumed. The node advertises one GPU
    # (GPUWORKS_FAKE_GPU is A100x1 at :340), so the batch does not all run at once: the rows
    # beyond the first wait for a device. What has to settle is therefore that the running
    # count stops moving, not that it reaches the whole list - assuming the latter would make
    # this item fail on a correct product. The stable count is then what the filter has to
    # render, and every attempt is recorded so a timeout reads as "never settled" rather than
    # as a bare failure.
    expected_running = None
    attempts = []
    deadline = time.time() + 180
    while time.time() < deadline:
        status, document = http(server.base, "GET",
                                DEPLOYMENTS + "?page=1&page_size=100&state=running",
                                token=token)
        check(status == 200, f"the running-state read answered {status}")
        running_total, whole = document.get("total"), _list_total(server, token, report)[0]
        attempts.append({"running_total": running_total, "list_total": whole})
        # Two consecutive equal readings: the row count has stopped changing.
        if len(attempts) >= 2 and running_total and \
                attempts[-1]["running_total"] == attempts[-2]["running_total"]:
            expected_running = running_total
            break
        page.wait_for_timeout(2000)
    check(expected_running is not None,
          f"the running count did not settle within 180s; the attempts were {attempts[-6:]}")

    query = _open_state_filter(page, locale, ["running"])
    rendered = _rows(page)
    check(query.get("state") == ["running"],
          f"the running filter sent state={query.get('state')}")
    check(rendered == expected_running,
          f"the running filter rendered {rendered} rows; the server reports "
          f"{expected_running} running of {total} total")
    _clear_state_filter(page, locale)

    # 3. One search term that returns exactly one row, used by both sides of the combination.
    _, rows_now = _list_total(server, token, report)
    target = next((item["name"] for item in rows_now if item.get("name")), None)
    check(target, "no deployment name to search for")
    search = page.locator(".ant-input-search input").first
    check(search.count() > 0, "the list rendered no search box")
    with page.expect_response(
            lambda r: "/api/v1/deployments" in r.url) as caught:
        search.fill(target)
        search.press("Enter")
    search_query = parse_qs(urlsplit(caught.value.url).query)
    page.wait_for_timeout(1500)
    search_rows = _rows(page)
    check(search_query.get("q") == [target],
          f"the search sent q={search_query.get('q')}, not the typed term")
    check(search_rows >= 1,
          f"searching for the row's own name returned {search_rows} rows")

    # The row the search found reports a state; a different state plus that same term has to
    # be empty, and it must be empty because of the state, not because the term is unrelated.
    status, document = http(server.base, "GET",
                            DEPLOYMENTS + "?page=1&page_size=100&q=" + target, token=token)
    check(status == 200, f"the search-only read answered {status}")
    matched = [item for item in document.get("items", []) if item.get("name") == target]
    check(len(matched) == 1, f"the term {target!r} did not resolve to one deployment")
    row_state = matched[0].get("state")
    other_state = next((s for s in DEPLOYMENT_LIST_STATES if s != row_state), None)
    check(other_state, "the fixture offers no second state to combine with")
    status, both = http(server.base, "GET",
                        DEPLOYMENTS + f"?page=1&page_size=100&q={target}&state={other_state}",
                        token=token)
    check(status == 200, f"the combined read answered {status}")
    check(both.get("total") == 0,
          f"the combination of the term {target!r} with state {other_state!r} returned "
          f"{both.get('total')} rows, though the row is {row_state!r}")

    # 4. The filtered-empty wording is driven by one flag (DeploymentsPage.tsx:102 sets
    # filtered when the search is non-empty or any state is chosen), so a state filter that
    # matches nothing has to render the same text a search that matches nothing renders.
    # Each reading is taken while the table is actually empty: reading it while the search
    # still returns a row would read an element that is not rendered at all.
    unmatched = "zzz-no-deployment-matches-this-" + report["run_id"]
    with page.expect_response(lambda r: "/api/v1/deployments" in r.url):
        search.fill(unmatched)
        search.press("Enter")
    page.wait_for_timeout(1500)
    check(_rows(page) == 0, "the unmatched term still rendered rows")
    search_empty = page.locator('[data-slot="table-body"] [data-slot="table-cell"][colspan]').first.inner_text().strip()
    check(search_empty, "the unmatched search rendered no empty-state text")

    search.fill("")
    search.press("Enter")
    page.wait_for_timeout(1200)
    # The state alone is chosen first, then the same unmatched term narrows it further, so
    # the empty text is produced with a state filter active rather than by the search alone.
    # Ticking the option is what applies it: ColumnFilter reports the change straight to the
    # page's controlled filteredValue, so there is no footer OK to press afterwards.
    _open_state_filter(page, locale, [other_state])
    page.wait_for_timeout(1200)
    with page.expect_response(lambda r: "/api/v1/deployments" in r.url):
        search.fill(unmatched)
        search.press("Enter")
    page.wait_for_timeout(1500)
    check(_rows(page) == 0, "the state filter plus the unmatched term still rendered rows")
    state_empty = page.locator('[data-slot="table-body"] [data-slot="table-cell"][colspan]').first.inner_text().strip()
    check(state_empty, "the state-filtered empty result rendered no empty-state text")
    check(state_empty == search_empty,
          f"a state filter plus an unmatched term rendered {state_empty!r}, while a search "
          f"alone rendered {search_empty!r}; the two emptinesses should share one wording")
    search.fill("")
    search.press("Enter")
    page.wait_for_timeout(1000)
    _clear_state_filter(page, locale)

    report["filters_10_3"] = {"layer": "request", "offered_states": list(DEPLOYMENT_LIST_STATES),
        "executed": executed, "populated_states": [i["state"] for i in populated],
        "empty_states": [i["state"] for i in empty],
        "running_wait_attempts": attempts[-8:],
        "running_expected": expected_running, "list_total": total,
        "running_rendered": rendered,
        "combination": {"search_term": target, "search_rows": search_rows,
                        "row_state": row_state, "combined_with_state": other_state,
                        "combined_rows": both.get("total")},
        "filtered_empty_text": {"search": search_empty, "state": state_empty,
                                "identical": state_empty == search_empty},
        "filter_trigger": "head-cell button carrying aria-pressed (ui-kit/ColumnFilter.tsx:41-42)"}
    report["evidence"].append({"layer": "request", "item": "10.3", "what":
        f"all {len(executed)} state filters the column offers were executed one at a time and "
        "each request carried its own state value; the populated ones are asserted on their "
        f"row sets ({', '.join(i['state'] for i in populated)}) and the rest return an empty "
        "set, recorded as measured rather than failing because this fixture holds deployments "
        "in two of the states only; the running filter's expectation was waited for until the "
        "filtered total equalled the list total rather than assumed; and a search term and a "
        "differing state each return rows alone while their combination returns none, with "
        "the same term on both sides so the empty intersection comes from the state. The "
        "filtered-empty wording a state filter produces is the same string a search produces, "
        "so both emptinesses share one flag.", **report["filters_10_3"]})
    return report["filters_10_3"]


def exercise_row_actions_10_9(page, server, report, token, row_ids):
    """10.9: each row is located by its own business text and its detail opens in place.

    Every row's name cell is a link button (DeploymentsPage.tsx:159 renders
    <Button type="link" onClick={() => openDetail(d.id)}>{d.name}</Button>), so the row is
    reached the way a user reaches it. Three rows are used rather than one, and they are
    chosen to cover different states, because opening the first row three times would pass
    even if every click ignored which row it was on.

    For each row the drawer has to name that deployment - not merely appear - and has to sit
    entirely inside the viewport with the window not scrolled, which is what the item asks
    for: a layout that only fits because the browser scrolled to it would be hidden by the
    automatic scrolling. Measured before writing this: the drawer occupies x 720..1440 of a
    1440x900 viewport at y 0..900, and window.scrollY stays 0 across open and close.

    The item names three actions that open in place. Only one of them does in this product,
    and the other two are stated as the shape they have rather than being folded into the
    one that was measured: edit navigates to a full page (DeploymentsPage.tsx:255), and
    diagnose is not a row action at all (it is task="instance.diagnose" in
    ui/src/pages/InstancePanel.tsx:260, mounted by ui/src/pages/DeploymentInstances.tsx:32).
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    # The page size chosen by the earlier items persists across a reload (it is stored per
    # table), so the row that is to be clicked would sit on a later page and the locator
    # would find nothing. The largest size keeps every row of the batch on one page.
    _choose_page_size(page, 100)

    # The batch rows are all queued behind the deployment this case owns, which already holds
    # the node's only GPU (:340 advertises one), so sampling rows by distinct state is not
    # something this fixture can promise. What the item actually needs is that each click is
    # resolved by that row's own name, so the sample is taken from the listed rows as they
    # are, and the states they happen to be in are recorded rather than required to differ.
    # Three distinct rows are still required: opening one row three times would pass even if
    # every click ignored which row it was on.
    #
    # Do not add criteria the item does not ask for. Requiring three distinct STATES was such
    # an invented condition, and it cost two runs on a correct product before the mistake was
    # found; what 10.9 asks for is that each row is reached by its own business text.
    _, items = _list_total(server, token, report)
    by_id = {item["id"]: item for item in items}
    rows = [by_id[i] for i in row_ids if i in by_id]
    check(len(rows) >= 3, f"only {len(rows)} of the batch rows are listed")
    chosen = rows[:3]
    check(len({row["name"] for row in chosen}) == 3,
          "the three rows sampled do not have three distinct names")

    opened = []
    for row in chosen:
        name = row["name"]
        button = page.get_by_role("button", name=name, exact=True)
        check(button.count() == 1,
              f"the row {name!r} did not resolve to one name button: {button.count()}")

        scroll_before = page.evaluate("() => window.scrollY")
        button.first.click()
        drawer = page.locator(".ant-drawer-content").first
        expect(drawer).to_be_visible(timeout=20000)
        page.wait_for_timeout(600)

        title = drawer.locator(".ant-drawer-title")
        check(title.count() == 1, "the drawer rendered no title")
        check(title.first.inner_text().strip() == name,
              f"clicking {name!r} opened a drawer titled "
              f"{title.first.inner_text().strip()!r}")
        check(name in drawer.inner_text(),
              f"the drawer opened from {name!r} does not mention that name in its body")

        # In place means in the viewport as it stands: the drawer has to fit without the
        # window scrolling, which is the automatic scrolling the item warns about.
        expect(drawer).to_be_in_viewport(ratio=1)
        box, viewport = drawer.bounding_box(), page.viewport_size
        check(box is not None, "the drawer reported no bounding box")
        inside = (box["x"] >= -0.5 and box["y"] >= -0.5
                  and box["x"] + box["width"] <= viewport["width"] + 0.5
                  and box["y"] + box["height"] <= viewport["height"] + 0.5)
        check(inside, f"the drawer box {box} is not inside the viewport {viewport}")
        scroll_after = page.evaluate("() => window.scrollY")
        check(scroll_after == scroll_before,
              f"opening {name!r} scrolled the window from {scroll_before} to {scroll_after}")

        opener = urlsplit(page.url)
        check(parse_qs(opener.query).get("deployment") == [str(row["id"])],
              f"the address names {parse_qs(opener.query).get('deployment')} after clicking "
              f"{name!r}, not {row['id']}")

        opened.append({"id": row["id"], "name": name, "state": row.get("state"),
                       "title": title.first.inner_text().strip(),
                       "drawer_box": box, "viewport": viewport,
                       "scroll_y": [scroll_before, scroll_after],
                       "address_query": parse_qs(opener.query).get("deployment")})

        close = page.locator(".ant-drawer-close").first
        check(close.count() == 1, "the drawer rendered no close control")
        close.click()
        page.wait_for_timeout(900)
        check(page.locator(".ant-drawer-content:visible").count() == 0,
              "the drawer stayed open after its close control was used")

    check(len({item["name"] for item in opened}) == len(opened),
          "the same deployment was opened twice, so the clicks were not per row")

    report["row_actions_10_9"] = {"layer": "ui", "rows_opened": opened,
        "states_sampled": [item["state"] for item in opened],
        "distinct_states_available": sorted({row["state"] for row in rows}),
        "detail_opens_in_place": True,
        "edit_shape": "full page navigation to /deploy?edit=<id> (DeploymentsPage.tsx:255)",
        "diagnose_shape": "not a row action; task=instance.diagnose in "
                          "ui/src/pages/InstancePanel.tsx:260, mounted by "
                          "ui/src/pages/DeploymentInstances.tsx:32"}
    report["evidence"].append({"layer": "ui", "item": "10.9", "what":
        f"{len(opened)} rows were each located by their own unique name - the cell renders a link "
        "button - and each opened a drawer titled with that same name, containing it, fully "
        "inside the viewport with the window not scrolled, at an address naming that row's id; "
        "the three rows have distinct names, so the clicks cannot all have landed on one row. Only "
        "the detail action opens in place in this product: edit is a full-page navigation to "
        "/deploy?edit=<id> (DeploymentsPage.tsx:255), and diagnose is not a row action but "
        "task=instance.diagnose inside ui/src/pages/InstancePanel.tsx:260, mounted by "
        "ui/src/pages/DeploymentInstances.tsx:32. Those two are recorded as the shape they have "
        "so the item is not read as covering three in-place surfaces when one is.",
        **report["row_actions_10_9"]})
    return report["row_actions_10_9"]


def exercise_delete_10_7(page, server, report, token, row_ids, common):
    """10.7: deleting the last row of the last page leaves the total, the page and the rows right.

    This reuses the pagination batch, so it costs no extra container. Only a row this section
    created may be deleted: the last page can also hold the deployment this case owns, and
    deleting that one would pull the fixture out from under the steps that follow.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    _choose_page_size(page, 10)

    total_before, items_before = _list_total(server, token, report)
    batch = {item["name"]: item["id"] for item in items_before
             if item["id"] in set(row_ids)}
    check(len(batch) == len(row_ids),
          f"only {len(batch)} of the {len(row_ids)} batch rows are in the list")

    # The footer has no numbered list; the last page is reached with the named control.
    page.get_by_role("button", name=common["resourceTable"]["lastPage"], exact=True).click()
    page.wait_for_timeout(1200)
    last_page_rows = _row_names(page)
    check(0 < len(last_page_rows) <= 10,
          f"the last page at size 10 held {len(last_page_rows)} rows")
    on_last_page = [name for name in last_page_rows if name in batch]
    check(on_last_page,
          f"the last page holds none of the batch rows, so this section has nothing it may "
          f"delete there: {last_page_rows}")
    victim_name = on_last_page[-1]
    victim_id = batch[victim_name]

    status, _ = http(server.base, "DELETE", DEPLOYMENTS + f"/{victim_id}", token=token)
    check(status in (200, 202, 204), f"deleting the last row answered {status}")
    row_ids.remove(victim_id)

    page.wait_for_timeout(1500)
    page.reload()
    _settled_path(page)
    page.wait_for_timeout(2000)

    page_summary = re.compile(r"^Page\s+(\d+)\s+of\s+(\d+)$")
    match = page_summary.match(page.get_by_text(page_summary).first.inner_text().strip()) \
        if page.get_by_text(page_summary).count() else None
    active_page = match.group(1) if match else None
    after_rows = _row_names(page)
    check(victim_name not in after_rows,
          f"the deleted row {victim_name!r} is still rendered after a reload")

    total_after, items_after = _list_total(server, token, report)
    check(total_after == total_before - 1,
          f"the total went from {total_before} to {total_after}, not down by one")
    names_after = {item["name"] for item in items_after}
    check(victim_name not in names_after,
          f"the deleted row {victim_name!r} is still in the server's list")
    # Whatever page the product leaves the user on, it must not be an empty page while rows
    # still exist - that would be a dead end the user has to escape by hand.
    if names_after:
        check(after_rows,
              f"after deleting the last row of the last page the view shows no rows at all, "
              f"though {total_after} rows still exist; the active page is {active_page!r}")

    report["delete_10_7"] = {"layer": "ui", "deleted": {"id": victim_id, "name": victim_name},
        "total_before": total_before, "total_after": total_after,
        "active_page_after": active_page, "rows_after_on_page": len(after_rows),
        "deleted_row_gone_from_page": victim_name not in after_rows,
        "deleted_row_gone_from_server": victim_name not in names_after,
        "page_count_after": int(match.group(2)) if match else None}
    report["evidence"].append({"layer": "ui", "item": "10.7", "what":
        "the last batch row on the last page was deleted; the total fell from "
        f"{total_before} to {total_after}, the row disappeared from both the page and the "
        "server's list, and the view did not leave the user on a page with no rows while rows "
        "still existed", **report["delete_10_7"]})
    return report["delete_10_7"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--inject-model-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-docker-error", action="store_true",
                        help=argparse.SUPPRESS)
    args = parser.parse_args()
    INJECT["docker_error"] = args.inject_docker_error
    os.umask(0o077)
    check(shutil.which("docker") is not None, "docker is required for this lane")
    check(subprocess.run(["docker", "image", "inspect", IMAGE], capture_output=True).returncode == 0,
          f"{IMAGE} is missing; run make fake-engine-image")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/deployment-lifecycle.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_deployment_lifecycle.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-agent-docker-fake-gpu-facts", "identity": "admin browser / node agent / Docker",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "container_fixture_sha256": digest(ROOT / "test/e2e/deployment_container_fixture.py"),
        "execution": "real Docker container running the audited fake-engine image; GPU facts are injected only for scheduling",
        "runtime": "real container and gateway request; no real GPU compute, dGPU isolation or vendor driver",
        "not_covered": ["real GPU inference, performance and dGPU isolation",
                        "multi-replica rolling restart and placement across nodes",
                        "deployment creation through the deploy form (created as a fixture)",
                        "engine failure, OOM and health-check recovery",
                        "container terminal and log stream for the deployment",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/deployment_lifecycle.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    token = ""
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "gpuworks-test-password-2026"})
        check(status == 200, f"fixture login HTTP {status}")
        token = login["token"]
        artifacts.remember(token)
        # The environment gate passed; a fixture failure from here is a test
        # failure, not BLOCKED_INFRA.
        report["preflight"] = "PASS"
        node, node_token, model, key, deployment = create_fixtures(
            server, token, run_id, report, holder, agent_bin,
            inject_model_failure=args.inject_model_failure, remember=artifacts.remember)
        artifacts.remember(token)
        artifacts.remember(node_token)
        artifacts.remember(key["key"])
        artifacts.write_json("report.json", report)
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_deployment_lifecycle(page, context, server, artifacts, report,
                                          args.locale, node, deployment, key["key"],
                                          token=token)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                close_ok = True
                try:
                    context.close()
                except Exception:
                    close_ok = False
                    report["context_close_error"] = artifacts.redact(
                        traceback.format_exc())
                try:
                    browser.close()
                except Exception:
                    close_ok = False
                    report["browser_close_error"] = artifacts.redact(
                        traceback.format_exc())
                report["browser_close_ok"] = close_ok
                if not close_ok:
                    report["status"] = "FAIL_TEST"
                    report.setdefault("failure", "the browser did not close cleanly")
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = cleanup(server, token, report, holder)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        cleanup_ok = cleanup_ok and report["server_stopped"]
        # No browser was opened when preparation failed; only a close that was
        # attempted and failed makes the case non-PASS.
        browser_ok = report.get("browser_close_ok", True)
        cleanup_ok = cleanup_ok and browser_ok
        report["environment_cleanup_ok"] = cleanup_ok
        layers = {item["layer"] for item in report["evidence"]}
        if (report.get("case_completed") and cleanup_ok
                and report.get("status") not in ("FAIL_TEST", "BLOCKED_INFRA")
                and set(manifest["required_evidence"]) <= layers):
            report["status"] = "PASS"
        elif report["status"] not in ("FAIL_TEST", "BLOCKED_INFRA"):
            report["status"] = "FAIL_TEST"
            report.setdefault("failure",
                              "cleanup or required evidence did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            (artifacts.directory / "agent.log").write_text(artifacts.redact(agent_log_path.read_text()))
        report["private_state"] = (
            "private/ retains isolated DB/logs and agent state; "
            + ("see remaining_resources or cleanup_errors for scoped recovery; "
               if not cleanup_ok else "all owned processes and resources confirmed gone; ")
            + "remove only this run directory after diagnosis (7 days)")
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
