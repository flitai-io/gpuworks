#!/usr/bin/env python3
"""Nodes / offline-reconnect lifecycle and host doctor through a real agent.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-node-reconnect-doctor
After build: python3 test/e2e/node_reconnect_doctor.py --locale en
Requires Python Playwright, system Chrome and the built agent binary. The node
is a real agent process on this host. No container, GPU or engine is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and agent and
retains the private database/logs for scoped diagnosis (seven days).
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
import platform
import socket
from pathlib import Path
import re
import subprocess
import sys
import time
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from dialog_checks import (
    assert_drawer_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/node/offline-reconnect-doctor"
CASE = "节点 / 真实 agent 离线重连与主机诊断 / 状态回读、同一节点身份、诊断报告"
NODES = "/api/v1/nodes"


def strings(locale):
    nodes = json.loads((ROOT / "ui/src/i18n/locales" / locale / "nodes.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return nodes, auth


def start_agent(binary, base, token, state, log_path):
    log = log_path.open("w")
    try:
        process = subprocess.Popen([str(binary), "run", "--server", base, "--token", token,
                                    "--state", str(state)],
                                   stdout=log, stderr=subprocess.STDOUT, text=True)
    except Exception:
        log.close()
        raise
    return process, log


def stop_agent(process, log):
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
    if log is not None and not log.closed:
        log.close()


def wait_state(base, token, node_uuid, wanted, process=None, timeout=35):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        status, node = http(base, "GET", NODES + f"/{node_uuid}", token=token)
        if status == 200:
            last = node.get("state", "")
            if last == wanted:
                return node
        if process is not None and process.poll() is not None and wanted == "online":
            raise AssertionError("the agent exited before the node came online")
        time.sleep(0.5)
    raise AssertionError(f"node state stayed {last!r}, want {wanted!r}")


def node_row(page, name):
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    return row


def goarch() -> str:
    machine = platform.machine().lower()
    return {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64",
            "arm64": "arm64"}.get(machine, machine)


# Contract: gpuworks/node/offline-reconnect-doctor
# Mainline: 独立 server/SQLite + 真实 agent → 节点在线 → 停止 agent 后 UI/GET 显示离线
#           → 同一 token/state 重启 agent 后同一节点恢复在线 → 详情页运行主机诊断
#           → 报告包含主机与运行时事实 → 停止 agent 并强制删除节点
def test_node_reconnect_doctor(page, context, server, artifacts, report, locale,
                               node, holder, node_token, state, admin_token,
                               agent_bin):
    nodes, auth = strings(locale)
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

    page.goto(server.base + "/nodes")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=nodes["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "nodes-page-loaded"

    row = node_row(page, node["name"])
    expect(row.get_by_text(nodes["state"]["online"], exact=True)).to_be_visible()

    stop_agent(holder["agent"], holder["log"])
    holder["agent"] = holder["log"] = None
    report["first_agent_stopped"] = True
    wait_state(server.base, admin_token, node["uuid"], "offline")
    row = node_row(page, node["name"])
    expect(row.get_by_text(nodes["state"]["offline"], exact=True)).to_be_visible(timeout=30000)
    report["last_step"] = "node-offline"

    holder["agent"], holder["log"] = start_agent(
        agent_bin, server.base, node_token, state,
        artifacts.private / "agent-2.log")
    report["second_agent_pid"] = holder["agent"].pid
    wait_state(server.base, admin_token, node["uuid"], "online", holder["agent"])
    row = node_row(page, node["name"])
    expect(row.get_by_text(nodes["state"]["online"], exact=True)).to_be_visible(timeout=30000)
    still = page.request.get(server.base + NODES + f"/{node['uuid']}")
    check(still.status == 200 and still.json()["uuid"] == node["uuid"],
          "reconnect changed the node identity")
    page.reload()
    row = node_row(page, node["name"])
    expect(row.get_by_text(nodes["state"]["online"], exact=True)).to_be_visible(timeout=30000)
    report["last_step"] = "node-reconnected"

    row.get_by_role("button", name=node["name"], exact=True).click()
    drawer = page.get_by_role("dialog")
    expect(drawer).to_be_visible()
    viewport = page.viewport_size

    def wait_inside(locator, label):
        # The drawer slides in; wait for the animation to settle instead of
        # measuring the first painted frame.
        deadline = time.time() + 10
        box = None
        while time.time() < deadline:
            box = locator.bounding_box()
            if (box and box["x"] >= 0 and box["y"] >= 0
                    and box["x"] + box["width"] <= viewport["width"] + 2
                    and box["y"] + box["height"] <= viewport["height"] + 2):
                return box
            time.sleep(0.1)
        raise AssertionError(f"{label} is outside the viewport: {box}")

    wait_inside(drawer, "node detail drawer")
    wait_inside(button(drawer, nodes["action"]["doctor"]), "the Diagnose action")
    # DetailDrawer keeps the hostname on a second title line, and only the title line is the
    # dialog's accessible name (ui-kit/SideDrawer.tsx:29-30 puts the subtitle outside DialogTitle).
    accessible = node['name']
    expect(page.get_by_role("dialog", name=accessible, exact=True)).to_be_visible()
    evidence = assert_drawer_shell(
        page, drawer, f"{node['name']}\n{socket.gethostname()}",
        button(drawer, nodes["action"]["doctor"]),
        page.get_by_role("dialog", name=nodes["rename"]["title"], exact=True))
    evidence["accessible_name"] = accessible
    report["evidence"].append(evidence)
    # Rename is a modal over this drawer. Cancel, Escape, close and mask leave the drawer up.
    rename_writes = []

    def on_rename(request):
        if (request.method == "PATCH"
                and urlsplit(request.url).path == NODES + f"/{node['uuid']}/name"):
            rename_writes.append(request.method)

    page.on("request", on_rename)
    parent = page.get_by_role("dialog", name=accessible, exact=True)
    edit = button(parent, nodes["action"]["edit"])

    def open_rename():
        edit.click()
        dialog = page.get_by_role("dialog", name=nodes["rename"]["title"], exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = assert_nested_dismissals(
        open_rename(), parent, open_rename,
        [("cancel", cancel_button(nodes["action"]["cancel"])),
         ("escape", escape), ("close", close_button), ("mask", mask)],
        lambda: len(rename_writes), edit)
    cancel_button(nodes["action"]["cancel"])(dialog)
    expect(dialog).to_be_hidden()
    expect(parent).to_be_visible()
    check(rename_writes == [], "rename dialog dismissal changed the node name")
    named = page.request.get(server.base + NODES + f"/{node['uuid']}")
    check(named.status == 200 and named.json().get("name") == node["name"],
          "rename dialog dismissal changed the stored name")
    report["evidence"].append({
        "layer": "ui", "rename_dialog": nodes["rename"]["title"], "parent": accessible,
        "dismissals": ["cancel", "escape", "close", "mask"],
        "focus_returned": True, "rename_patches": 0, "stored_name": node["name"]})
    with page.expect_response(lambda r: urlsplit(r.url).path == NODES + f"/{node['uuid']}/doctor"
                              and r.request.method == "POST", timeout=60000) as doctor:
        button(drawer, nodes["action"]["doctor"]).click()
    check(doctor.value.status == 200, f"doctor HTTP {doctor.value.status}")
    result = doctor.value.json()
    check(result.get("report"), "doctor returned no raw report")
    host = result.get("host") or {}
    expected_hostname = socket.gethostname()
    expected_os = "linux" if sys.platform.startswith("linux") else platform.system().lower()
    expected_arch = goarch()
    expected_kernel = platform.release()
    check(host.get("hostname") == expected_hostname and host.get("os") == expected_os
          and host.get("arch") == expected_arch and host.get("kernel") == expected_kernel,
          f"doctor host facts differ: {host}, expected {expected_hostname}/"
          f"{expected_os}/{expected_arch}/{expected_kernel}")
    host_line = (f"{expected_hostname} · {expected_os}/{expected_arch} · "
                 f"kernel {expected_kernel}")
    expect(drawer.get_by_text(host_line, exact=True)).to_be_visible(timeout=30000)
    runtimes = result.get("runtimes") or []
    check(runtimes and all(item.get("name") for item in runtimes),
          f"doctor runtimes differ: {runtimes}")
    runtime_section = drawer.get_by_role("region", name=nodes["doctor"]["runtimes"], exact=True)
    for item in runtimes:
        label = "runveil" if item["name"] == "udocker" else item["name"]
        # has is evaluated from each candidate row. A locator rooted at the
        # enclosing region would search for a second region inside that row.
        runtime_row = runtime_section.get_by_role("row").filter(
            has=page.get_by_text(label, exact=True))
        expect(runtime_row).to_have_count(1)
        if item.get("status") == "ok":
            expected = item.get("version") or "—"
        else:
            expected = nodes["doctor"]["runtimeNotFound" if item.get("status") == "not_found"
                                         else "runtimeQueryFailed"]
        expect(runtime_row.get_by_text(expected, exact=True)).to_be_visible()
        expect(runtime_row.get_by_text(nodes["doctor"]["runtimeSelected"], exact=True)
               ).to_have_count(1 if item.get("selected") else 0)
    selected = [item for item in runtimes if item.get("selected")]
    check(len(selected) == 1, f"the doctor did not select exactly one runtime: {runtimes}")
    expect(drawer.get_by_text(nodes["doctor"]["runtimeSelected"], exact=True)
           ).to_have_count(len(selected))
    # The report is appended after the node's own detail; its section labels
    # can repeat the detail's terms, so take the last occurrence.
    for label in (nodes["doctor"]["overview"], nodes["doctor"]["host"],
                  nodes["doctor"]["cpu"], nodes["doctor"]["memory"],
                  nodes["doctor"]["disk"]):
        expect(drawer.get_by_text(label, exact=True).last).to_be_visible(timeout=30000)
    check(drawer.evaluate("el => el.scrollWidth <= el.clientWidth + 1"),
          "the doctor report overflows the drawer horizontally")
    report["last_step"] = "doctor-verified"

    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    stop_agent(holder["agent"], holder["log"])
    holder["agent"] = holder["log"] = None
    removed = page.request.delete(server.base + NODES + f"/{node['uuid']}?force=true")
    check(removed.status == 200, f"node cleanup HTTP {removed.status}")
    check(page.request.get(server.base + NODES + f"/{node['uuid']}").status == 404,
          "the cleaned node is still readable")
    report["owned_node_uuid"] = None
    report["last_step"] = "cleanup-complete"

    report["evidence"].extend([
        {"layer": "ui", "page": "/nodes", "online": nodes["state"]["online"],
         "offline": nodes["state"]["offline"], "reconnected": nodes["state"]["online"],
         "doctor": nodes["action"]["doctor"],
         "sections": [nodes["doctor"]["overview"], nodes["doctor"]["host"],
                      nodes["doctor"]["cpu"], nodes["doctor"]["memory"],
                      nodes["doctor"]["disk"]]},
        {"layer": "request", "doctor": {"path": NODES + f"/{node['uuid']}/doctor",
         "status": 200}, "node_identity": node["uuid"]},
        {"layer": "persistence", "state_transition": ["online", "offline", "online"],
         "same_uuid": still.json()["uuid"]},
        {"layer": "execution", "agent_pids": [report["first_agent_pid"],
         report["second_agent_pid"]], "host": host,
         "runtimes": [{"name": r.get("name"), "status": r.get("status")}
                      for r in result["runtimes"]]},
        {"layer": "cleanup", "agent_stopped": True, "node_deleted": True},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}


def cleanup(server, admin_token, report, holder):
    """Verify every resource is gone and retain failed ownership for recovery."""
    errors = []
    agent = holder.get("agent")
    try:
        stop_agent(agent, holder.get("log"))
    except Exception:
        errors.append("agent stop/log close failed: " + traceback.format_exc().splitlines()[-1])
    stopped = agent is None or agent.poll() is not None
    report["agent_stopped"] = stopped
    cleanup_ok = stopped and not errors
    if stopped:
        holder["agent"] = holder["log"] = None
    else:
        report.setdefault("remaining_resources", []).append({
            "kind": "agent", "id": agent.pid, "state": "running",
            "reclaim": f"kill {agent.pid}"})
    if report.get("owned_node_uuid"):
        node_uuid = report["owned_node_uuid"]
        try:
            status, _ = http(server.base, "DELETE", NODES + f"/{node_uuid}?force=true",
                             token=admin_token)
            report["cleanup_node_status"] = status
            observed = http(server.base, "GET", NODES + f"/{node_uuid}", token=admin_token)[0]
            gone = observed == 404
            state = f"GET {observed}"
        except Exception:
            gone, state = False, "unknown (query failed)"
            errors.append("node delete/readback failed: " + traceback.format_exc().splitlines()[-1])
        report["cleanup_node_gone"] = gone
        cleanup_ok = cleanup_ok and gone
        if gone:
            report["owned_node_uuid"] = None
        else:
            report.setdefault("remaining_resources", []).append({
                "kind": "node", "id": node_uuid, "state": state,
                "reclaim": f"DELETE {NODES}/{node_uuid}?force=true"})
    if errors:
        report["cleanup_errors"] = errors
    return cleanup_ok


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/node-reconnect-doctor.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_node_reconnect_doctor.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-agent-webdoctor", "identity": "admin browser / node agent",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "the real agent executed the host doctor; no container, GPU or engine",
        "runtime": "N/A: host diagnostics only; no container, GPU or engine",
        "not_covered": ["token regeneration and stale-session rejection", "server_dial nodes",
                        "doctor failures and older agents", "agent upgrade and downgrade",
                        "heartbeat timing and fleet-scale reconnects", "non-admin access",
                        "column sorting", "list pagination",
                        "zh/en navigation and error text beyond the asserted labels",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/node_reconnect_doctor.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    admin_token = ""
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "gpuworks-test-password-2026"})
        check(status == 200, f"fixture login HTTP {status}")
        admin_token = login["token"]
        # Register immediately; the token never enters the report.
        artifacts.remember(admin_token)
        node_name = f"e2e-{run_id}-reconnect-node"
        status, created = http(server.base, "POST", NODES, token=admin_token,
                               body={"name": node_name, "server_url": server.base})
        check(status == 201, f"node fixture HTTP {status}")
        node, node_token = created["node"], created["token"]
        node["name"] = node_name
        artifacts.remember(node_token)
        report["owned_node_uuid"] = node["uuid"]
        state = artifacts.private / "agent-state"
        state.mkdir()
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        holder["agent"], holder["log"] = start_agent(
            agent_bin, server.base, node_token, state,
            artifacts.private / "agent.log")
        report["first_agent_pid"] = holder["agent"].pid
        wait_state(server.base, admin_token, node["uuid"], "online", holder["agent"])
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_node_reconnect_doctor(page, context, server, artifacts, report,
                                           args.locale, node, holder, node_token, state,
                                           admin_token, agent_bin)
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
                close_ok = True
                for name, resource in (("context", context), ("browser", browser)):
                    try:
                        resource.close()
                    except Exception:
                        close_ok = False
                        report[name + "_close_error"] = artifacts.redact(traceback.format_exc())
                report["browser_close_ok"] = close_ok
                if not close_ok:
                    report["status"] = "FAIL_TEST"
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = cleanup(server, admin_token, report, holder)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        cleanup_ok = cleanup_ok and report["server_stopped"] and report.get("browser_close_ok", True)
        report["environment_cleanup_ok"] = cleanup_ok
        if not cleanup_ok:
            report["status"] = "FAIL_TEST"
            report.setdefault("failure", "cleanup did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        for name in ("agent.log", "agent-2.log"):
            path = artifacts.private / name
            if path.exists():
                (artifacts.directory / name).write_text(artifacts.redact(path.read_text()))
        report["private_state"] = (
            "private/ retains isolated DB/logs and agent state; "
            + ("see remaining_resources or cleanup_errors for scoped recovery; "
               if not cleanup_ok else "all owned processes and node confirmed gone; ")
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
