#!/usr/bin/env python3
"""Settings / deployment start timeout through the real browser and server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-settings-deployment-policy
After build: python3 test/e2e/settings_deployment_policy.py --locale en
Requires Python Playwright and system Chrome. No agent, GPU or container is
needed: this contract ends at the cluster-wide deployment policy.
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
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from settings_confirm import confirm_setting
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/settings/deployment-start-timeout"
CASE = "设置 / 部署启动超时默认值 / 边界保存、刷新回读与服务端拒绝"
SETTINGS_PATH = "/api/v1/settings/deployment"
SETTINGS = "/api/v1/settings"
MIN_SECONDS = 60
MAX_SECONDS = 24 * 60 * 60
POLICY_FIELDS = ("pending_timeout_sec", "event_retention_days", "event_retention_bytes")


def strings(locale):
    ui = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return ui, auth


# Contract: gpuworks/settings/deployment-start-timeout
# Mainline: 独立 server/SQLite → 设置页读默认值 → 清空时保存禁用且无请求
#           → 服务端直接拒绝 30 秒 → UI 保存下界 60 与上界 86400 → 每次刷新/独立 GET 回读
#           → 恢复原值且其余部署策略字段不变
def test_deployment_start_timeout(page, context, server, artifacts, report, locale):
    ui, auth = strings(locale)
    label = ui["deployment"]["startTimeout"]
    save_label = ui["common"]["save"]
    saved_text = ui["deployment"]["saved"]
    page_errors, console_errors, bad_responses, puts = [], [], [], []
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
        if urlsplit(request.url).path == SETTINGS_PATH and request.method == "PUT":
            puts.append(request.post_data_json)

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
    expect(page.get_by_role("heading", name=ui["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    nodes_tab = page.get_by_role("tab", name=ui["page"]["tabs"]["nodes"], exact=True)
    nodes_tab.click()
    expect(nodes_tab).to_have_attribute("aria-selected", "true")
    report["last_step"] = "nodes-tab-selected"

    field = page.get_by_label(label, exact=True)
    expect(field).to_be_visible()
    # The card loads the stored policy asynchronously; an empty control before
    # that response is not a saved value.
    expect(field).to_have_value(re.compile(r"^\d+$"))
    card = page.locator(".ant-card").filter(has=field)
    save = button(card, save_label)
    initial = int(field.input_value())
    check(MIN_SECONDS <= initial <= MAX_SECONDS, f"initial timeout {initial} is out of range")

    initial_response = page.request.get(server.base + SETTINGS)
    check(initial_response.status == 200, f"settings GET HTTP {initial_response.status}")
    initial_policy = initial_response.json()["deployment"]
    check(initial_policy["start_timeout_sec"] == initial, "initial GET differs from the form")
    report["last_step"] = "initial-policy-read"

    # 1.1: one spinbutton, the seeded default, seconds, and the server range.
    # Same field-inventory family as the label dialog. No second product mutant.
    expect(page.get_by_role("spinbutton", name=label, exact=True)).to_have_count(1)
    check(initial == 1800, f"default timeout {initial} is not the seeded 1800 seconds")
    check(field.get_attribute("aria-required") != "true", "start timeout is marked required")
    expect(field).to_have_attribute("aria-valuemin", str(MIN_SECONDS))
    expect(field).to_have_attribute("aria-valuemax", str(MAX_SECONDS))
    expect(field).to_have_attribute("step", "60")
    expect(card.locator(".ant-input-number-suffix")).to_have_text(ui["deployment"]["seconds"])
    check(card.get_by_role("switch").count() == 0, "the timeout card hides the field behind a switch")
    check(card.get_by_role("spinbutton").count() == 1, "the timeout card has another field")
    check(puts == [], "reading the timeout submitted a deployment policy")
    report["timeout_inventory"] = {
        "role": "spinbutton", "name": label, "required": False, "default": initial,
        "unit": ui["deployment"]["seconds"], "min": MIN_SECONDS, "max": MAX_SECONDS,
        "step": 60, "conditional": False, "fields": 1, "requests": len(puts),
        "request_field": "start_timeout_sec",
        "persisted": initial_policy["start_timeout_sec"],
    }
    report["last_step"] = "timeout-inventory"
    from settings_deployment_keyboard import exercise_timeout_keyboard
    report["timeout_keyboard"] = exercise_timeout_keyboard(page, card, field, save, puts, initial)

    field.fill("")
    expect(save).to_be_disabled()
    check(puts == [], "clearing the field submitted a deployment policy")
    cleared = page.request.get(server.base + SETTINGS)
    check(cleared.status == 200, f"cleared GET HTTP {cleared.status}")
    cleared_policy = cleared.json()["deployment"]
    check(cleared_policy["start_timeout_sec"] == initial, "clearing the timeout stored an empty value")
    for policy_name in POLICY_FIELDS:
        check(cleared_policy[policy_name] == initial_policy[policy_name],
              f"clearing the timeout changed {policy_name}")
    report["edit_cycle_clear"] = {"stored": initial, "save_disabled": True, "puts": 0,
                                  "switch": False, "binding": False,
                                  "other_fields": list(POLICY_FIELDS)}
    report["last_step"] = "empty-disables-save"

    # 3.2: blur clamps to the real min and max. Precision 0 rounds .5 and up.
    # Empty already left Save disabled. A non-numeric entry restores the last number.
    def commit_number(text):
        field.fill(text)
        field.blur()

    clamped = (("0", str(MIN_SECONDS)), ("30", str(MIN_SECONDS)), ("-1", str(MIN_SECONDS)),
               (str(MAX_SECONDS + 1), str(MAX_SECONDS)), ("60.4", "60"), ("60.6", "61"),
               ("abc", "61"))
    for typed, shown in clamped:
        commit_number(typed)
        expect(field).to_have_value(shown)
        expect(save).to_be_enabled()
    check(puts == [], "a clamped or rejected number submitted a deployment policy")
    after_bounds = page.request.get(server.base + SETTINGS)
    check(after_bounds.status == 200, f"bounds GET HTTP {after_bounds.status}")
    check(after_bounds.json()["deployment"]["start_timeout_sec"] == initial,
          "clamped numbers changed the stored timeout")
    report["last_step"] = "number-bounds"
    report["number_bounds"] = [{"typed": typed, "shown": shown} for typed, shown in clamped]

    # 3.3: keyboard and the step buttons change the shown value by 60.
    # This control has no slider and no second linked input. A step at either
    # bound stays on that bound. Stepping does not submit.
    stepped_from = int(field.input_value())
    check(stepped_from == 61, f"step start {stepped_from} is not the rounded bound")
    field.press("ArrowUp")
    expect(field).to_have_value(str(stepped_from + 60))
    field.press("ArrowDown")
    expect(field).to_have_value(str(stepped_from))
    increase = card.get_by_role("button", name="Increase Value", exact=True)
    decrease = card.get_by_role("button", name="Decrease Value", exact=True)
    expect(increase).to_have_count(1)
    # The handlers stay at width 0 until the control is hovered.
    def click_step(target):
        card.locator(".ant-input-number-affix-wrapper").hover()
        target.click(force=True)

    click_step(increase)
    expect(field).to_have_value(str(stepped_from + 60))
    click_step(decrease)
    expect(field).to_have_value(str(stepped_from))
    check(card.locator(".ant-slider").count() == 0, "the start timeout rendered a slider")
    check(card.get_by_role("spinbutton").count() == 1, "the start timeout has a second linked input")
    field.fill(str(MIN_SECONDS))
    expect(field).to_have_value(str(MIN_SECONDS))
    click_step(decrease)
    field.press("ArrowDown")
    expect(field).to_have_value(str(MIN_SECONDS))
    field.fill(str(MAX_SECONDS))
    expect(field).to_have_value(str(MAX_SECONDS))
    click_step(increase)
    field.press("ArrowUp")
    expect(field).to_have_value(str(MAX_SECONDS))
    check(puts == [], "stepping the timeout submitted a deployment policy")
    after_steps = page.request.get(server.base + SETTINGS)
    check(after_steps.status == 200, f"steps GET HTTP {after_steps.status}")
    check(after_steps.json()["deployment"]["start_timeout_sec"] == initial,
          "stepping changed the stored timeout")
    report["last_step"] = "number-steps"
    report["number_steps"] = {"from": stepped_from, "keyboard": stepped_from + 60,
                              "button": stepped_from + 60, "at_min": MIN_SECONDS,
                              "at_max": MAX_SECONDS, "slider": False, "linked_inputs": 1}

    invalid = page.request.put(server.base + SETTINGS_PATH,
                               data={"start_timeout_sec": MIN_SECONDS - 30})
    check(invalid.status == 400, f"server accepted an out-of-range timeout: {invalid.status}")
    after_invalid = page.request.get(server.base + SETTINGS)
    check(after_invalid.json()["deployment"]["start_timeout_sec"] == initial,
          "an out-of-range request changed the stored policy")
    report["last_step"] = "server-rejects-below-floor"

    # 1.13: another session's PUT is the last write. Same family as node labels.
    # No second product mutant. The open draft stays, and Save writes it back.
    other_timeout = 2400
    other = page.request.put(server.base + SETTINGS_PATH, data={"start_timeout_sec": other_timeout})
    check(other.status == 200, f"the other session was rejected: HTTP {other.status}")
    other_read = page.request.get(server.base + SETTINGS)
    check(other_read.status == 200
          and other_read.json()["deployment"]["start_timeout_sec"] == other_timeout,
          f"the other session was not stored: {other_read.status}")
    expect(field).to_have_value(str(MAX_SECONDS))
    with page.expect_response(lambda r: urlsplit(r.url).path == SETTINGS_PATH
                              and r.request.method == "PUT") as kept_draft:
        confirm_setting(page, save)
    check(kept_draft.value.status == 200, f"draft save HTTP {kept_draft.value.status}")
    check(kept_draft.value.request.post_data_json == {"start_timeout_sec": MAX_SECONDS},
          f"draft save body {kept_draft.value.request.post_data_json}")
    kept_get = page.request.get(server.base + SETTINGS)
    check(kept_get.json()["deployment"]["start_timeout_sec"] == MAX_SECONDS,
          "saving the open draft did not replace the other session")
    report["timeout_conflict"] = {"other_session": 200, "other_read": other_timeout,
                                  "draft": MAX_SECONDS, "saved_status": 200,
                                  "saved_read": MAX_SECONDS, "conflict_status": False}
    report["last_step"] = "timeout-conflict"
    expect(page.get_by_text(saved_text, exact=True).first).to_be_visible()
    expect(page.locator(".ant-message-notice")).to_have_count(0)

    saved_values = []
    for value in (MIN_SECONDS, MAX_SECONDS):
        field.fill(str(value))
        held = []

        def hold_put(route):
            if route.request.method != "PUT" or urlsplit(route.request.url).path != SETTINGS_PATH:
                route.continue_()
                return
            held.append(route)

        if value == MIN_SECONDS:
            page.route("**" + SETTINGS_PATH, hold_put)
            confirm_setting(page, save)
            inflight = save
            expect(inflight).to_be_disabled()
            inflight.evaluate("(button) => button.click()")
            field.press("Enter")
            check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
            check(held[0].request.post_data_json == {"start_timeout_sec": value}, "held policy body differs")
            with page.expect_response(lambda r: urlsplit(r.url).path == SETTINGS_PATH
                                      and r.request.method == "PUT") as saved:
                held[0].continue_()
            page.unroute("**" + SETTINGS_PATH, hold_put)
        else:
            with page.expect_response(lambda r: urlsplit(r.url).path == SETTINGS_PATH
                                      and r.request.method == "PUT") as saved:
                confirm_setting(page, save)
        check(saved.value.status == 200, f"save HTTP {saved.value.status}")
        check(saved.value.request.post_data_json == {"start_timeout_sec": value},
              f"save body differs: {saved.value.request.post_data_json}")
        expect(page.get_by_text(saved_text, exact=True).first).to_be_visible()
        page.reload()
        expect(page.get_by_role("tab", name=ui["page"]["tabs"]["nodes"], exact=True)
               ).to_have_attribute("aria-selected", "true")
        field = page.get_by_label(label, exact=True)
        expect(field).to_have_value(str(value))
        independent = page.request.get(server.base + SETTINGS)
        check(independent.status == 200, f"independent GET HTTP {independent.status}")
        saved_policy = independent.json()["deployment"]
        check(saved_policy["start_timeout_sec"] == value,
              "refresh and independent GET differ")
        report["last_step"] = "policy-other-fields"
        for policy_name in POLICY_FIELDS:
            check(saved_policy[policy_name] == initial_policy[policy_name],
                  f"saving the timeout changed {policy_name}")
        report["other_fields_unchanged"] = True
        saved_values.append(value)
        report["last_step"] = f"saved-{value}-and-reloaded"

    field.fill(str(initial))
    with page.expect_response(lambda r: urlsplit(r.url).path == SETTINGS_PATH
                              and r.request.method == "PUT") as restored:
        confirm_setting(page, save)
    check(restored.value.status == 200, f"restore HTTP {restored.value.status}")
    page.reload()
    expect(page.get_by_role("tab", name=ui["page"]["tabs"]["nodes"], exact=True)
           ).to_have_attribute("aria-selected", "true")
    field = page.get_by_label(label, exact=True)
    expect(field).to_have_value(str(initial))
    final_response = page.request.get(server.base + SETTINGS)
    final_policy = final_response.json()["deployment"]
    check(final_policy["start_timeout_sec"] == initial, "the original timeout was not restored")
    for name in POLICY_FIELDS:
        check(final_policy[name] == initial_policy[name],
              f"editing the start timeout changed {name}")
    report["edit_cycle"] = {"from": initial, "values": [MIN_SECONDS, MAX_SECONDS, initial],
                            "switch": False, "binding": False, "empty_stored": False,
                            "other_fields_unchanged": list(POLICY_FIELDS)}
    report["last_step"] = "original-policy-restored"

    check(puts == [{"start_timeout_sec": MAX_SECONDS},
                   {"start_timeout_sec": MIN_SECONDS},
                   {"start_timeout_sec": MAX_SECONDS},
                   {"start_timeout_sec": initial}], f"unexpected PUT bodies: {puts}")
    expected_invalid = {"method": "PUT", "path": SETTINGS_PATH, "status": 400}
    unexpected = [item for item in bad_responses
                  if not (item["method"] == expected_invalid["method"]
                          and item["path"] == expected_invalid["path"]
                          and item["status"] == expected_invalid["status"])]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "settings page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/settings", "tab": ui["page"]["tabs"]["nodes"],
         "card": ui["deployment"]["title"],
         "field": label, "initial": initial, "cleared_save_disabled": True,
         "bounds": report.get("number_bounds"), "steps": report.get("number_steps"),
         "saved": saved_values, "restored": initial},
        {"layer": "request", "puts": puts,
         "invalid": {"method": "PUT", "path": SETTINGS_PATH, "body": {"start_timeout_sec": MIN_SECONDS - 30},
                     "status": 400}},
        {"layer": "persistence", "refresh": "60 then 86400 then initial",
         "independent_get": {"initial": initial, "after_min": MIN_SECONDS,
                             "after_max": MAX_SECONDS, "restored": initial},
         "other_policy_fields_unchanged": list(POLICY_FIELDS)},
        {"layer": "cleanup", "restored": initial, "independent_get": final_policy},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "puts": puts}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/settings-deployment-policy.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_deployment_start_timeout.__name__,
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
        "keyboard_sha256": digest(ROOT / "test/e2e/settings_deployment_keyboard.py"),
        "execution": "N/A: a cluster-wide setting; no agent or workload is involved",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["administrator-only authorization with a non-admin session",
                        "pending timeout and event retention editing",
                        "a deployment that inherits the cluster default at launch",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/settings_deployment_policy.py", *sys.argv[1:]]}
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
                test_deployment_start_timeout(page, context, server, artifacts, report, args.locale)
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
                context.close()
                browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        report["server_stopped"] = server.stop()
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
