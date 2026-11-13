#!/usr/bin/env python3
"""Settings / MCP: UI switches persist and gate real JSON-RPC tool reads.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-mcp-settings
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import re
import os
from pathlib import Path
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check, collection, theme_color

CONTRACT = "gpuworks/settings/mcp-toggle-runtime"
STATUS = "/api/v1/mcp/status"
SETTINGS = "/api/v1/mcp/settings"
MCP = "/api/v1/mcp"
TOKENS = "/api/v1/platform-tokens"


def cleanup_stage(report, name, action):
    try:
        result = action()
        check(result is None or result is True, f"incomplete cleanup: {result!r}")
        report.setdefault("cleanup_stages", {})[name] = "ok"
        return True
    except Exception:
        report.setdefault("cleanup_stages", {})[name] = traceback.format_exc()
        report["status"] = "FAIL_TEST"
        return False


def cleanup_fixture(page, server, report):
    """API cleanup after evidence capture; ownership survives failed observations."""
    owned = report.setdefault("owned_resources", {})
    def disable():
        current = page.request.get(server.base + STATUS)
        check(current.status == 200, f"GET {STATUS}: {current.status}")
        if current.json()["enabled"]:
            response = page.request.put(server.base + SETTINGS, data={"enabled": False})
            check(response.status == 200, f"PUT {SETTINGS}: {response.status}")
        current = page.request.get(server.base + STATUS)
        check(current.status == 200 and current.json()["enabled"] is False, "MCP cleanup did not restore off")
    cleanup_stage(report, "restore-setting", disable)
    for key, path, statuses in (("platform_token_id", TOKENS, (204, 404)),
                                ("network_id", "/api/v1/networks", (200, 404))):
        if key not in owned:
            continue
        def remove(key=key, path=path, statuses=statuses):
            resource_id = owned[key]
            response = page.request.delete(server.base + path + f"/{resource_id}")
            check(response.status in statuses, f"DELETE {path}/{resource_id}: {response.status}")
            response = page.request.get(server.base + path)
            check(response.status == 200, f"GET {path}: {response.status}")
            if key == "platform_token_id":
                items = collection(response)
            else:
                body = response.json()
                check(set(body) == {"items", "roles", "types", "type_roles"} and isinstance(body["items"], list),
                      f"GET {path}: unexpected network collection envelope")
                items = body["items"]
            check(all(item["id"] != resource_id for item in items), f"GET {path}: cleanup absence not observed for {resource_id}")
            del owned[key]
        cleanup_stage(report, key, remove)


def close_browser(report, artifacts, context, browser):
    cleanup_stage(report, "trace", lambda: artifacts.stop_trace(context))
    cleanup_stage(report, "context", context.close)
    cleanup_stage(report, "browser", browser.close)


def stop_server(report, server):
    report["server_stopped"] = cleanup_stage(report, "server", server.stop)
    report["remaining_resources"] = [
        {"kind": key, "id": value, "scope": "isolated SQLite database in private/",
         "reclaim": "remove only this run directory after diagnosis"}
        for key, value in report.get("owned_resources", {}).items()]
    if not report["server_stopped"]:
        report["remaining_resources"].append({"kind": "process", "pid": server.process.pid if server.process else None,
            "reclaim": f"kill {server.process.pid}" if server.process else "server never started"})


def copy_server_log(artifacts):
    log = artifacts.private / "server.log"
    if log.exists():
        (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))


def capture_failure(page, artifacts, report):
    try:
        if not any(secret in page.content() for secret in artifacts.secrets if len(secret) > 8):
            page.screenshot(path=artifacts.directory / "failure.png")
    except Exception:
        report["screenshot_error"] = traceback.format_exc()


# Mainline: 独立server与只读令牌 → UI开启MCP → 刷新/GET回读 → 真实初始化、工具读取库存
#           → UI关闭后同令牌403 → 再开启恢复 → UI恢复默认关闭、清理令牌
# Contract: gpuworks/settings/mcp-toggle-runtime
def test_mcp_toggle_runtime(page, context, server, artifacts, report, locale):
    texts = ROOT / "ui/src/i18n/locales" / locale
    auth = json.loads((texts / "auth.json").read_text())
    settings = json.loads((texts / "settings.json").read_text())
    errors, console_errors, bad_http, mutations = [], [], [], []
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": bad_http}
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(server.base + "/settings")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    report["last_step"] = "login-and-settings-heading"
    # The MCP card lives on the Security & access tab; entering through the real tab
    # also puts its key in the URL fragment, so every reload below reopens that tab.
    access = page.get_by_role("tab", name=settings["page"]["tabs"]["access"], exact=True)
    access.click()
    expect(access).to_have_attribute("aria-selected", "true")
    expect(page).to_have_url(re.compile(r"/settings#access$"))
    report["evidence"].append({"layer": "ui", "tab": "access", "selected": True, "url": page.url})
    # 22.2: the tab is a URL fragment and the page replaces history rather than pushing it, so the
    # invariant this can assert is that the URL and the selected tab never disagree - including when a
    # fragment arrives from outside the tab strip. A deep link to #access is followed by checking that
    # the tab it names is the one selected and that no other tab claims to be, which is exactly the
    # mismatch the item describes; a back/forward expectation would be asserting a history behaviour
    # this page deliberately does not have.
    page.goto(server.base + "/settings#access")
    expect(page.get_by_role("tab", selected=True)).to_have_count(1, timeout=10000)
    check(page.get_by_role("tab", selected=True).inner_text() == settings["page"]["tabs"]["access"],
          "a deep link to #access did not select the access tab")
    expect(access).to_have_attribute("aria-selected", "true")
    for other in page.get_by_role("tab").all():
        if other.inner_text() != settings["page"]["tabs"]["access"]:
            check(other.get_attribute("aria-selected") != "true",
                  f"a second tab claimed selection next to #access: {other.inner_text()}")
    report["evidence"].append({"layer": "ui", "deep_link": "#access",
                               "url": page.url, "selected_tabs": 1})
    # The other tabs are walked too: each is required to select itself, to put its own key in the
    # fragment, and to keep exactly one tab selected, so a tab that rendered without claiming the URL
    # - or a URL that moved without the selection following - is caught rather than only the one tab
    # this case is about.
    visited = []
    for other in page.get_by_role("tab").all():
        name = other.inner_text()
        if name == settings["page"]["tabs"]["access"]:
            continue
        # The strip re-renders as each tab becomes active, so the click is made against a freshly
        # located control and the loop waits for that specific tab to report itself selected before
        # reading anything. Without that, the reading can land on the transition and attribute the
        # previous tab's selection to this one.
        target = page.locator(".settings-tabs > .ant-tabs-nav").get_by_role("tab", name=name, exact=True)
        expect(target).to_have_count(1, timeout=10000)
        target.click()
        for _ in range(100):
            if target.get_attribute("aria-selected") == "true":
                break
            page.wait_for_timeout(100)
        page.wait_for_timeout(200)
        # The page's own tab strip is .settings-tabs; several cards render their own nested Tabs, so
        # the selection has to be read from the top-level strip or a card's inner tab would be
        # counted as a second selected page tab.
        selected_names = [node.inner_text() for node in
                          page.locator(".settings-tabs > .ant-tabs-nav").get_by_role("tab", selected=True).all()]
        check(len(selected_names) == 1,
              f"after clicking {name!r} more than one tab claimed selection: {selected_names}")
        selected_name = selected_names[0]
        check(selected_name == name,
              f"clicking {name!r} selected {selected_name!r} instead")
        check(re.search(r"/settings#[^/]+$", page.url) is not None,
              f"clicking {name!r} did not address the tab in the fragment: {page.url}")
        second = [t for t in page.locator(".settings-tabs > .ant-tabs-nav").get_by_role("tab").all()
                  if t.get_attribute("aria-selected") == "true"]
        check(len(second) == 1, f"{name!r} left {len(second)} top-level tabs selected")
        visited.append(name)
    check(len(visited) >= 2, f"the settings page exposed too few tabs to walk: {visited}")
    report["evidence"].append({"layer": "ui", "tabs_walked": len(visited)})
    page.get_by_role("tab", name=settings["page"]["tabs"]["access"], exact=True).click()
    expect(access).to_have_attribute("aria-selected", "true")
    report["last_step"] = "access-tab-selected"
    report["preflight"] = "PASS"
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)
    page.on("response", lambda r: bad_http.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path, "body": r.post_data_json})
            if urlsplit(r.url).path == SETTINGS and r.method != "GET" else None)
    baseline = page.request.get(server.base + STATUS)
    check(baseline.status == 200, "status preflight failed")
    initial = baseline.json()
    check(set(initial) == {"enabled", "path", "protocol_version", "tools"} and initial["enabled"] is False
          and initial["path"] == MCP and initial["tools"], "fresh MCP defaults/envelope drifted")
    token_response = page.request.post(server.base + TOKENS, data={"name": f"e2e-{report['run_id']}-mcp", "scope": "read"})
    check(token_response.status == 201, "read-token fixture failed")
    payload = token_response.json()
    if isinstance(payload.get("token"), str):
        artifacts.remember(payload["token"])
    if isinstance(payload.get("item"), dict) and "id" in payload["item"]:
        report.setdefault("owned_resources", {})["platform_token_id"] = payload["item"]["id"]
    check(isinstance(payload.get("token"), str) and bool(payload["token"])
          and "platform_token_id" in report.get("owned_resources", {}), "read-token fixture returned an incomplete credential")
    token, token_id = payload["token"], payload["item"]["id"]
    report["fixture_token_id"] = token_id
    artifacts.start_trace(context)
    switch = page.get_by_role("switch", name=settings["mcp"]["switch"], exact=True)
    # 6.1: the control is disabled until the service reports its state, so the operator cannot toggle
    # a service whose current state is not yet known. The status request is held and the switch is
    # required to be disabled while it is pending, then required to become usable once the answer
    # arrives - a control that started enabled would let a click race the state it is meant to flip.
    held = []
    page.route("**/api/v1/mcp/status", lambda route: held.append(route))
    page.reload()
    for _ in range(120):
        if held:
            break
        page.wait_for_timeout(100)
    check(len(held) >= 1, "the MCP status was not requested when the card opened")
    loading_switch = page.get_by_role("switch", name=settings["mcp"]["switch"], exact=True)
    expect(loading_switch).to_be_visible(timeout=12000)
    check(loading_switch.is_disabled(),
          "the MCP switch was operable before the service reported its state")
    for route in held:
        route.continue_()
    held.clear()
    page.unroute("**/api/v1/mcp/status")
    expect(switch).to_be_enabled(timeout=15000)
    report["evidence"].append({"layer": "ui", "disabled_while_status_pending": True,
                               "control": settings["mcp"]["switch"]})
    expect(switch).to_be_enabled()
    expect(switch).not_to_be_checked()
    # This card is below the fold; explicit user scrolling is appropriate for the entry control.
    switch.scroll_into_view_if_needed()
    expect(switch).to_be_in_viewport(ratio=1)
    report["evidence"].append({"layer": "ui", "default_enabled": False, "control": settings["mcp"]["switch"]})

    ping = {"jsonrpc": "2.0", "id": "off", "method": "ping"}
    code, denied = http(server.base, "POST", MCP, token, ping)
    check(code == 403 and denied == {"code": "MCP_DISABLED", "error": "the MCP service is turned off"}, "default-off does not deny MCP")
    report["evidence"].append({"layer": "runtime", "state": "default-off", "status": code, "body": denied})

    def toggle(enabled):
        with page.expect_response(lambda r: urlsplit(r.url).path == SETTINGS and r.request.method == "PUT") as saved:
            switch.click()
        response = saved.value
        report["last_request"] = {"method": "PUT", "path": SETTINGS, "status": response.status,
                                  "expected_body": {"enabled": enabled}, "actual_body": response.request.post_data_json}
        check(response.status == 200 and response.request.post_data_json == {"enabled": enabled}
              and not urlsplit(response.url).query, f"incorrect toggle request/response: {report['last_request']!r}")
        expected = {**initial, "enabled": enabled}
        check(response.json() == expected, "toggle response changed another MCP field")
        expect(switch).to_be_checked(checked=enabled)
        report["evidence"].append({"layer": "request", "method": "PUT", "path": SETTINGS, "body": {"enabled": enabled}, "status": 200})
        read = page.request.get(server.base + STATUS)
        check(read.status == 200 and read.json() == expected, "toggle not persisted in independent GET")
        page.reload()
        expect(page).to_have_url(server.base + "/settings#access")
        expect(access).to_have_attribute("aria-selected", "true")
        expect(switch).to_be_enabled()
        expect(switch).to_be_checked(checked=enabled)
        switch.scroll_into_view_if_needed()
        expect(switch).to_be_in_viewport(ratio=1)
        report["evidence"].append({"layer": "persistence", "path": STATUS, "status": 200, "body": expected, "reload_checked": enabled})
        report["last_step"] = f"toggle-{enabled}-persisted-and-reloaded"

    def rpc(method, request_id, params=None):
        body = {"jsonrpc": "2.0", "id": request_id, "method": method}
        if params is not None:
            body["params"] = params
        code, reply = http(server.base, "POST", MCP, token, body)
        check(code == 200 and reply.get("jsonrpc") == "2.0" and reply.get("id") == request_id
              and "result" in reply and "error" not in reply, f"MCP {method} failed")
        report["evidence"].append({"layer": "runtime", "auth": "read bearer only; no cookies", "method": "POST", "path": MCP,
                                   "request": body, "status": code, "response": reply})
        return reply["result"]

    toggle(True)
    page.get_by_text(settings["mcp"]["title"], exact=True).hover()
    theme_color(switch, "backgroundColor", "primary")
    report["evidence"].append({"layer": "ui", "computed_style": "enabled Switch background derives from theme.colors.primary"})
    initialized = rpc("initialize", "init", {"protocolVersion": initial["protocol_version"], "capabilities": {},
                                              "clientInfo": {"name": report["run_id"], "version": "1"}})
    check(initialized["protocolVersion"] == initial["protocol_version"] and initialized["serverInfo"]["name"] == "gpuworks"
          and "tools" in initialized["capabilities"], "MCP initialize capability mismatch")
    code, notification = http(server.base, "POST", MCP, token, {"jsonrpc": "2.0", "method": "notifications/initialized"})
    check(code == 202 and notification is None, "initialized notification not accepted")
    listed = rpc("tools/list", "tools")["tools"]
    check(sorted(t["name"] for t in listed) == sorted(initial["tools"]), "tools differ from authoritative status")
    check(all(t["annotations"]["readOnlyHint"] is True and isinstance(t["inputSchema"], dict) and t["description"] for t in listed), "invalid tool capabilities")
    nodes = collection(page.request.get(server.base + "/api/v1/nodes"))
    check(nodes == [], "fresh node fixture is not empty")
    read = rpc("tools/call", "nodes", {"name": "call_api", "arguments": {"path": "/api/v1/nodes", "jq": ".items"}})
    check(read.get("isError") is False and read["content"][0]["type"] == "text"
          and read["content"][0]["text"] == json.dumps(nodes) + "\n(empty. If you expected data, check the parameter values against search_api's description: an unknown filter value answers empty, not an error.)", "MCP tool did not read canonical node inventory with the documented empty-result hint")
    # The service's read-only tools must not expose credentials even to an admin-owned read token.
    denied_tool = rpc("tools/call", "secrets", {"name": "call_api", "arguments": {"path": TOKENS}})
    check(denied_tool.get("isError") is True and "not readable" in denied_tool["content"][0]["text"], "MCP exposed credential management")
    code, denied = http(server.base, "PUT", SETTINGS, token, {"enabled": False})
    check(code == 403 and denied.get("code") == "PLATFORM_TOKEN_READ_ONLY", "read token changed MCP settings")
    check(page.request.get(server.base + STATUS).json()["enabled"] is True, "denied write changed state")
    report["evidence"].append({"layer": "runtime", "read_token_mutation": {"status": code, "body": denied}, "side_effects": "none"})
    toggle(False)
    code, denied = http(server.base, "POST", MCP, token, ping)
    check(code == 403 and denied.get("code") == "MCP_DISABLED", "disabled service remains callable")
    report["evidence"].append({"layer": "runtime", "state": "disabled-after-use", "status": code, "body": denied})
    toggle(True)
    recovered = rpc("tools/call", "recovered", {"name": "call_api", "arguments": {"path": STATUS, "jq": ".enabled"}})
    check(recovered.get("isError") is False and recovered["content"][0]["text"] == "true", "same token cannot resume real tool calls")
    toggle(False)
    deleted = page.request.delete(server.base + TOKENS + f"/{token_id}")
    check(deleted.status == 204 and collection(page.request.get(server.base + TOKENS)) == [], "fixture token cleanup failed")
    del report["owned_resources"]["platform_token_id"]
    check(mutations == [{"method": "PUT", "path": SETTINGS, "body": {"enabled": state}} for state in [True, False, True, False]], "unexpected browser settings mutations")
    check(not errors and not console_errors and not bad_http, f"browser errors: {errors}, {console_errors}, {bad_http}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "settings page overflows horizontally")
    page.screenshot(path=artifacts.directory / "mcp-restored.png")
    report["evidence"].append({"layer": "cleanup", "setting_restored": initial, "fixture_tokens": [], "browser_mutations": mutations})
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": bad_http}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / "test/e2e/coverage/mcp-settings-lifecycle.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_mcp_toggle_runtime.__name__, "manifest binding drift")
    report = {"contract": CONTRACT, "case": manifest["name"], "status": "NOT_RUN", "run_id": run_id,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "target_branch": "wei/master", "target_sha": subprocess.check_output(["git", "rev-parse", "wei/master"], cwd=ROOT, text=True).strip(),
        "lane": "local-real-server-sqlite", "identity": "admin browser / read bearer MCP client", "locale": args.locale,
        "viewport": {"width": 1440, "height": 900}, "timezone": "UTC", "selection": [CONTRACT], "retries": 0, "evidence": [],
        "test_sha256": digest(Path(__file__)), "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/platform_token_read_revoke.py"), "coverage_manifest_sha256": digest(manifest_path),
        "command": ["python3", "test/e2e/mcp_settings_lifecycle.py", *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"], report["owned_server_pid"] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_mcp_toggle_runtime(page, context, server, artifacts, report, args.locale)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "missing required evidence")
                report["business_complete"] = True
            except Exception:
                report["status"], report["failure"] = "FAIL_TEST", traceback.format_exc()
                capture_failure(page, artifacts, report)
            finally:
                cleanup_fixture(page, server, report)
                close_browser(report, artifacts, context, browser)
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        stop_server(report, server)
        cleanup_stage(report, "server-log", lambda: copy_server_log(artifacts))
        report["private_state"] = "isolated DB/logs retained for diagnosis; remove only this run directory within 7 days"
        cleanup_stage(report, "report", lambda: artifacts.write_json("report.json", report))
        try:
            artifacts.scan()
            report["artifact_scan"] = "PASS"
            if report.get("business_complete") and report["server_stopped"] and report["status"] == "NOT_RUN":
                report["status"] = "PASS"
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts"
        if not cleanup_stage(report, "report-final", lambda: artifacts.write_json("report.json", report)):
            cleanup_stage(report, "report-failure", lambda: artifacts.write_json("report.json", report))
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
