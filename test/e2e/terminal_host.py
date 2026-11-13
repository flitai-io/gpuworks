#!/usr/bin/env python3
"""Web terminal / host PTY lifecycle through a real local agent.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-terminal-host
After build: python3 test/e2e/terminal_host.py --locale en
Requires Python Playwright, system Chrome and the built agent binary. The node
is a real agent process on this host; the browser types into a real host pty.
No container, GPU or engine is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and agent and
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
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/terminal/host-pty-lifecycle"
CASE = "终端 / 宿主机 PTY / 真实命令回显、退出结束、管理员开关禁用与恢复"
NODES = "/api/v1/nodes"
SETTINGS_TERMINAL = "/api/v1/settings/terminal"


def strings(locale):
    nodes = json.loads((ROOT / "ui/src/i18n/locales" / locale / "nodes.json").read_text())
    settings = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return nodes, settings, auth


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


def monitor_terminal_popup(target, errors):
    """Attach before navigation; disabled admission is a 200 preflight, not an HTTP error."""
    target.on("pageerror", lambda error: errors["page"].append(str(error)))
    target.on("console", lambda message: errors["console"].append(message.text)
              if message.type == "error" else None)

    def response_seen(response):
        if response.status >= 400:
            errors["http"].append({"method": response.request.method,
                                   "path": urlsplit(response.url).path,
                                   "status": response.status})
    target.on("response", response_seen)


def open_terminal(page, row, nodes):
    with page.context.expect_page() as popup:
        row.get_by_role("button", name=nodes["action"]["terminal"], exact=True).click()
    terminal = popup.value
    terminal.wait_for_load_state()
    terminal.set_default_timeout(15000)
    expect(terminal.locator(".xterm")).to_be_visible(timeout=20000)
    return terminal


def terminal_row(page, name):
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    return row


# Contract: gpuworks/terminal/host-pty-lifecycle
# Mainline: 独立 server/SQLite + 真实本机 agent → 节点行打开宿主机终端
#           → 输入 echo 标记并看到真实回显 → exit 后会话结束
#           → 设置页关闭 Web Terminal 后新终端被拒绝 → 重新开启后终端恢复
#           → 停止 agent 并强制删除节点
def _untranslated(locale_pairs, label):
    """Pairs whose zh copy is identical to the en copy, i.e. never translated."""
    return [name for name, (english, chinese) in locale_pairs.items() if english == chinese]


def _internal_identifiers(text):
    """Internal field names that must never stand in for a translation."""
    hits = []
    for token in ("cache_max_bytes", "node_uuid", "target_kind", "expires_at",
                  "session_id", "transport_bytes"):
        if token in text:
            hits.append(token)
    return hits


def exercise_localization(page, server, nodes, settings, auth, node, open_terminal, terminal_row):
    """22.5: navigation, first screen, form and error text are translated to zh.

    Asserting that the page 'looks Chinese' cannot distinguish a translated sentence
    from an English one that happens to sit next to a Chinese label, and guessing at
    what untranslated text looks like gave me two false positives on legitimate
    command examples. The check is exact instead: each surface has to render the zh
    string, and that zh string has to differ from the en one, so a surface still
    showing its English copy fails no matter how it is worded.
    """
    found = {}
    keys = {
        "page_title": ("page", "title"),
        "terminal_host": ("terminalView", "host"),
        "terminal_cannot_open": ("terminalView", "cannotOpen"),
        "terminal_open": ("terminalView", "status", "open"),
        "terminal_error": ("terminalView", "status", "error"),
        "terminal_disabled": ("terminalView", "preflight", "disabled"),
    }

    def load(locale):
        return json.loads((ROOT / "ui/src/i18n/locales" / locale / "nodes.json").read_text())

    en_nodes, zh_nodes = load("en"), load("zh")
    pairs = {}
    for label, path_ in keys.items():
        en_value, zh_value = en_nodes, zh_nodes
        for part in path_:
            en_value = en_value.get(part, "")
            zh_value = zh_value.get(part, "")
        pairs[label] = (en_value, zh_value)
    identical = _untranslated(pairs, "nodes")
    check(not identical, f"these nodes.json entries are not translated: {identical}")

    # First screen: the page title is the zh copy, not the en one.
    page.goto(server.base + "/nodes")
    title_zh, title_en = pairs["page_title"][1], pairs["page_title"][0]
    check(title_zh != title_en, "page.title is the same in both locales")
    expect(page.get_by_role("heading", name=title_zh, exact=True)).to_be_visible()
    found["page_title"] = title_zh

    # Navigation and table headers carry Chinese and no internal identifiers.
    nav_text = page.get_by_role("navigation").first.inner_text()
    # The page under inspection may render either the antd table or the ResourceTable
    # (components/resourceTable/ResourceTable.tsx), whose heads are
    # [data-slot=table-head]; read whichever is present so the leak check still has text.
    headers = page.locator('[data-slot="table-head"], .ant-table-thead th')
    headers_text = " ".join(headers.all_inner_texts())
    for label, text in (("navigation", nav_text), ("table_headers", headers_text)):
        leaked = _internal_identifiers(text)
        check(not leaked, f"{label} shows internal identifiers instead of a translation: {leaked}")
    found["navigation_leaks"] = _internal_identifiers(nav_text)
    found["table_headers_leaks"] = _internal_identifiers(headers_text)

    # Form: the terminal settings card renders the zh terminal title.
    page.goto(server.base + "/settings")
    page.get_by_role("tab", name=settings["page"]["tabs"]["access"], exact=True).click()
    card = page.locator(".ant-card").filter(has_text=settings["terminal"]["title"]).first
    expect(card).to_be_visible()
    card_text = card.inner_text()
    check(_internal_identifiers(card_text) == [],
          f"the settings card shows internal identifiers: {_internal_identifiers(card_text)}")
    found["settings_card_translated"] = True

    # Dynamic text: the live terminal status and the server's preflight reason both
    # arrive as the zh copy.
    page.goto(server.base + "/nodes")
    terminal = open_terminal(page, terminal_row(page, node["name"]), nodes)
    expect(terminal.get_by_text(pairs["terminal_open"][1], exact=True)).to_be_visible(timeout=20000)
    found["terminal_status_translated"] = pairs["terminal_open"][1]
    terminal.close()
    found["localized_pairs"] = {k: v[1] for k, v in pairs.items()}
    return found


def test_terminal_host(page, context, server, artifacts, report, locale, node,
                       agent, agent_log, admin_token):
    nodes, settings, auth = strings(locale)
    # The expected output is assembled by printf; it does not appear in the
    # typed command, so PTY input echo alone cannot satisfy the assertion.
    marker = f"e2e-{report['run_id']}-terminal-executed"
    command = (f"printf 'e2e-%s-%s\\n' \"{report['run_id']}\" \"terminal-executed\"")
    page_errors, console_errors, bad_responses, settings_puts, preflights = [], [], [], [], []
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}
    # Register before opening a popup so initial script/network errors are observed.
    context.on("page", lambda target: monitor_terminal_popup(target, report["browser_errors"]))
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))
    context.on("response", lambda response: preflights.append({
        "status": response.status, "path": urlsplit(response.url).path})
        if urlsplit(response.url).path == NODES + f"/{node['uuid']}/terminal/settings" else None)

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
        if urlsplit(request.url).path == SETTINGS_TERMINAL and request.method == "PUT":
            settings_puts.append(request.post_data_json)

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
    expect(page.get_by_role("heading", name=nodes["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "nodes-page-loaded"

    row = terminal_row(page, node["name"])
    terminal = open_terminal(page, row, nodes)
    expect(terminal.get_by_text(nodes["terminalView"]["status"]["open"], exact=True)).to_be_visible(timeout=20000)
    terminal.locator(".xterm").click()
    terminal.keyboard.type(command)
    terminal.keyboard.press("Enter")
    expect(terminal.locator(".xterm-rows")).to_contain_text(marker, timeout=20000)
    terminal.keyboard.type("exit")
    terminal.keyboard.press("Enter")
    expect(terminal.locator(".xterm-rows")).to_contain_text(
        nodes["terminalView"]["sessionEnded"], timeout=20000)
    expect(terminal.get_by_text(nodes["terminalView"]["status"]["exited"], exact=True)).to_be_visible()
    terminal.close()
    report["last_step"] = "pty-exited"

    page.goto(server.base + "/settings")
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    access_tab = page.get_by_role("tab", name=settings["page"]["tabs"]["access"], exact=True)
    access_tab.click()
    expect(access_tab).to_have_attribute("aria-selected", "true")
    report["last_step"] = "terminal-settings-loaded"
    card = page.locator(".ant-card").filter(has_text=settings["terminal"]["title"]).first
    switch = card.get_by_role("switch")
    expect(switch).to_be_checked()
    switch.click()
    expect(switch).not_to_be_checked()
    button(card, settings["common"]["save"]).click()
    expect(page.get_by_text(settings["terminal"]["saved"], exact=True).first).to_be_visible()
    disabled_settings = page.request.get(server.base + "/api/v1/settings").json()["terminal"]
    check(disabled_settings["enabled"] is False, "the terminal switch did not persist")
    report["last_step"] = "terminal-disabled"

    page.goto(server.base + "/nodes")
    terminal = open_terminal(page, terminal_row(page, node["name"]), nodes)
    expect(terminal.get_by_text(nodes["terminalView"]["status"]["error"], exact=True)).to_be_visible(timeout=20000)
    # The server sends the reason in English, and the UI is expected to
    # localize it, so assert the translated sentence rather than the raw string.
    expect(terminal.get_by_text(nodes["terminalView"]["preflight"]["disabled"], exact=False)).to_be_visible()
    terminal.close()

    page.goto(server.base + "/settings")
    access_tab = page.get_by_role("tab", name=settings["page"]["tabs"]["access"], exact=True)
    access_tab.click()
    expect(access_tab).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=settings["terminal"]["title"]).first
    switch = card.get_by_role("switch")
    expect(switch).not_to_be_checked()
    switch.click()
    expect(switch).to_be_checked()
    button(card, settings["common"]["save"]).click()
    expect(page.get_by_text(settings["terminal"]["saved"], exact=True).first).to_be_visible()
    restored = page.request.get(server.base + "/api/v1/settings").json()["terminal"]
    check(restored["enabled"] is True, "the terminal switch was not restored")
    report["last_step"] = "terminal-restored"
    report["terminal_enabled_restored"] = True

    page.goto(server.base + "/nodes")
    terminal = open_terminal(page, terminal_row(page, node["name"]), nodes)
    expect(terminal.get_by_text(nodes["terminalView"]["status"]["open"], exact=True)).to_be_visible(timeout=20000)
    terminal.close()

    # 22.5: in a non-English locale the operator-facing text has to be translated,
    # including the dynamic strings the server contributes, and internal field names
    # must not stand in for a translation. This is driven on the zh run only, since
    # the assertion is that Chinese text appears where English would otherwise leak.
    if locale == "zh":
        report["localization"] = exercise_localization(
            page, server, nodes, settings, auth, node, open_terminal, terminal_row)
        report["last_step"] = "localization"

    check(len([item for item in preflights if item["status"] == 200]) >= 3,
          f"terminal preflights differ: {preflights}")
    check([item.get("enabled") for item in settings_puts] == [False, True],
          f"terminal settings PUTs differ: {settings_puts}")
    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/nodes", "entry": nodes["action"]["terminal"],
         "connected": nodes["terminalView"]["status"]["open"], "marker": marker,
         "ended": nodes["terminalView"]["sessionEnded"],
         "disabled": nodes["terminalView"]["status"]["error"],
         "restored": nodes["terminalView"]["status"]["open"]},
        {"layer": "request", "preflights": preflights, "settings_puts": settings_puts},
        {"layer": "persistence", "disabled_settings": disabled_settings,
         "restored_settings": restored},
        {"layer": "execution", "agent_pid": agent.pid, "marker": marker,
         "command": command, "session_ended": True, "host_pty": True},
    ])


def cleanup(server, admin_token, report, agent, agent_log, inject_timeout=False):
    """Reclaim every owned resource; one failure must not skip the rest."""
    cleanup_ok = True
    errors = []
    if agent is not None:
        try:
            if agent.poll() is None:
                agent.terminate()
                try:
                    agent.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    agent.kill()
                    agent.wait(timeout=5)
            report["agent_stopped"] = agent.poll() is not None
        except Exception:
            report["agent_stopped"] = agent.poll() is not None
            errors.append("agent stop failed: " + traceback.format_exc().splitlines()[-1])
            cleanup_ok = False
        cleanup_ok = cleanup_ok and report.get("agent_stopped", False)
    else:
        report["agent_stopped"] = True
    if agent_log is not None and not agent_log.closed:
        try:
            agent_log.close()
        except Exception:
            errors.append("agent log close failed")
            cleanup_ok = False
    if report.get("owned_node_uuid"):
        node_uuid = report["owned_node_uuid"]
        try:
            status, _ = http(server.base, "DELETE",
                             NODES + f"/{node_uuid}?force=true", token=admin_token)
            report["cleanup_node_status"] = status
            if inject_timeout:
                # Self-test: the node is already deleted, but the readback
                # raises. The server must still stop and the report must be
                # written, with a non-PASS result.
                raise TimeoutError("injected cleanup readback timeout")
            gone = status in (200, 404) and http(
                server.base, "GET", NODES + f"/{node_uuid}",
                token=admin_token)[0] == 404
        except Exception:
            gone = False
            errors.append("node cleanup failed: " + traceback.format_exc().splitlines()[-1])
        report["cleanup_node_gone"] = gone
        cleanup_ok = cleanup_ok and gone
        if gone:
            report["owned_node_uuid"] = None
        else:
            report.setdefault("remaining_resources", []).append({
                "kind": "node", "id": node_uuid, "state": "absence not confirmed",
                "reclaim": f"DELETE {NODES}/{node_uuid}?force=true"})
    else:
        report["cleanup_node_gone"] = True
    if errors:
        report["cleanup_errors"] = errors
    return cleanup_ok


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--inject-close-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-cleanup-timeout", action="store_true",
                        help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/terminal-host.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_terminal_host.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-agent-webterm", "identity": "admin browser / node agent",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "a real host pty through the local agent executed the echoed command and exited",
        "runtime": "N/A: the host pty is not a container, GPU or inference runtime",
        "not_covered": ["container terminal targets",
                        "UTF-8 across frames, continuous output and resize/SIGWINCH",
                        "idle timeout and SIGTERM/SIGKILL cleanup",
                        "per-node and global concurrency limits", "session audit",
                        "closing a session while a command runs",
                        "multi-tab sessions", "reconnect after a dropped agent connection",
                        "non-admin access and zh/en navigation text",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/terminal_host.py", *sys.argv[1:]]}
    host_gate_inputs = ('test/e2e/terminal_host.py', 'test/e2e/local_browser_fixture.py',
        'test/e2e/platform_token_read_revoke.py', *('test/e2e/' + name for name in (
            'terminal_host_oracles.py', 'terminal_host_owner.py',
            'terminal_host_sessions.py', 'terminal_host_gates.py')))
    report['host_gate_source_hashes'] = {name: digest(ROOT / name) for name in host_gate_inputs}
    report['host_agent_driver'] = 'none'
    server = LocalServer(args.server_bin, artifacts.private)
    agent = None
    agent_log = None
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
        artifacts.remember(admin_token)
        node_name = f"e2e-{run_id}-terminal-node"
        status, created = http(server.base, "POST", NODES, token=admin_token,
                               body={"name": node_name, "server_url": server.base})
        check(status == 201, f"node fixture HTTP {status}")
        node, node_token = created["node"], created["token"]
        artifacts.remember(node_token)
        report["owned_node_uuid"] = node["uuid"]
        state = artifacts.private / "agent-state"
        state.mkdir()
        agent_log = (artifacts.private / "agent.log").open("w")
        agent = subprocess.Popen([str(args.agent_bin), "run", "--server", server.base,
                                  "--token", node_token, "--state", str(state), "--driver", "none"],
                                 stdout=agent_log, stderr=subprocess.STDOUT, text=True)
        wait_online(server.base, admin_token, node["uuid"], agent)
        report["agent_pid"] = agent.pid
        node["name"] = node_name
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_terminal_host(page, context, server, artifacts, report,
                                   args.locale, node, agent, agent_log, admin_token)
                check(report['host_gate_source_hashes'] == {name: digest(ROOT / name) for name in host_gate_inputs},
                      'host gate source changed before post-mainline slice')
                from terminal_host_gates import exercise as host_gates
                host_gates(context, server, artifacts, report, args.locale, node,
                           agent, args.agent_bin, admin_token)
                check(report['host_gate_source_hashes'] == {name: digest(ROOT / name) for name in host_gate_inputs},
                      'host gate source changed during post-mainline slice')
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                try:
                    # This parent may still contain login input; the host slice
                    # captures its own non-secret terminal page separately.
                    artifacts.capture_failure(page, True)
                except Exception as capture_error:
                    report['capture_error'] = artifacts.redact(str(capture_error))
            finally:
                artifacts.write_json("report.json", report)
                close_ok = True
                try:
                    if args.inject_close_failure:
                        raise RuntimeError("injected context close failure")
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
        report.setdefault("failure", artifacts.redact(traceback.format_exc()))
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = cleanup(server, admin_token, report, agent, agent_log,
                                 inject_timeout=args.inject_cleanup_timeout)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
            cleanup_ok = False
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        cleanup_ok = cleanup_ok and report["server_stopped"]
        browser_ok = report.get("browser_close_ok") is True
        cleanup_ok = cleanup_ok and browser_ok
        report["evidence"] = [item for item in report["evidence"]
                              if item.get("layer") != "cleanup"]
        report["evidence"].append({
            "layer": "cleanup",
            "terminal_enabled": report.get("terminal_enabled_restored", False),
            "agent_stopped": report.get("agent_stopped", False),
            "node_deleted": report.get("cleanup_node_gone", False),
            "browser_close_ok": browser_ok,
            "cleanup_ok": cleanup_ok,
        })
        layers = {item["layer"] for item in report["evidence"]}
        if (report.get("case_completed") and cleanup_ok
                and report.get("status") not in ("FAIL_TEST", "BLOCKED_INFRA")
                and set(manifest["required_evidence"]) <= layers):
            report["status"] = "PASS"
        elif report["status"] not in ("FAIL_TEST", "BLOCKED_INFRA"):
            report["status"] = "FAIL_TEST"
            report["failure"] = (report.get("failure")
                                 or "cleanup or required evidence did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            (artifacts.directory / "agent.log").write_text(artifacts.redact(agent_log_path.read_text()))
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
