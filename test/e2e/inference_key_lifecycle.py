#!/usr/bin/env python3
"""Real single-server inference credential lifecycle; no GPU inference claim.

A fresh server starts with no API key rows and /v1 requires a key by default;
a keyless request is admitted only when an administrator explicitly turns the
open-access setting on. Revoking every key therefore leaves /v1 refusing all
callers until another key is created.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run after make build: python3 test/e2e/inference_key_lifecycle.py
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
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check, collection, modal_geometry, strings, theme_color

CONTRACT = "gpuworks/inference-key/create-copy-delete"


def active(rows):
    """Keys still able to authenticate: revoking keeps the row on purpose.

    DELETE now revokes in place so the name stays on the usage it produced;
    the row is therefore expected to remain with revoked_at set.
    """
    return [row for row in rows if not row.get("revoked_at")]


def revoked(rows):
    return [row for row in rows if row.get("revoked_at")]

CASE = "推理令牌 / UI 创建复制并删除 / 刷新回读、真实网关鉴权与即时撤销生效"
KEY_PATH = "/api/v1/apikeys"
SECURITY_PATH = "/api/v1/security/settings"
DENIED = {"error": {"message": "invalid or missing API key", "type": "gpuworks_proxy"}}
MODELS = {"object": "list", "data": []}


def exercise_create_dialog(page, create, k, mutations, report):
    """Create dialog identity, layout, focus, and dismissals. No key POST."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    title = k["create"]["title"]
    card = page.locator(".ant-card").filter(has_text=k["gateway"]["title"]).first

    def writes():
        return sum(1 for item in mutations if item["path"].startswith(KEY_PATH))

    def open_create():
        create.click()
        dialog = page.get_by_role("dialog", name=title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = open_create()
    assert_dialog_shell(page, dialog, dialog.get_by_label(k["table"]["name"], exact=True),
                        button(dialog, k["actions"]["create"]))
    dialog = assert_nested_dismissals(
        dialog, card, open_create,
        [("cancel", cancel_button(k["actions"]["cancel"])), ("escape", escape),
         ("close", close_button), ("mask", mask)],
        writes, create)
    cancel_button(k["actions"]["cancel"])(dialog)
    expect(dialog).to_be_hidden()
    expect(card).to_be_visible()
    check(writes() == 0, "create dialog dismissal sent a key mutation")
    report["evidence"].append({"layer": "ui", "create_dialog": title,
        "dismissals": ["cancel", "escape", "close", "mask"], "key_mutations": 0})


def exercise_key_field_inventory(dialog, k, mutations):
    """Register the create dialog's fields before a name is typed.

    Name is a required textbox. It opens empty, with no unit and no control
    range. The other field is the key's scope: two radios defaulting to "all
    model services", with the deployment picker appearing only after "some" is
    chosen. Reading the form sends no key write. The later submit in this run is
    the request and the persisted record: POST body of name and scope, then a
    list row of name, prefix and created_at.
    """
    before = len(mutations)
    name_label = k["table"]["name"]
    name = dialog.get_by_label(name_label, exact=True)
    expect(dialog.get_by_role("textbox")).to_have_count(1)
    expect(name).to_have_count(1)
    expect(name).to_have_value("")
    expect(name).to_have_attribute("placeholder", k["create"]["namePlaceholder"])
    expect(name).to_have_attribute("aria-required", "true")
    check(name.get_attribute("maxlength") is None, "name field exposes a control range")
    required = dialog.locator("label.ant-form-item-required")
    expect(required).to_have_count(1)
    expect(required).to_have_text(name_label)
    expect(dialog.locator(".ant-input-suffix, .ant-input-number")).to_have_count(0)
    # A key now carries a scope (apikeys/KeyScope.tsx:34-41): all model services or
    # a chosen subset. The default is the "all" radio, and the deployment picker
    # only exists once "some" is chosen, so the create form has exactly two radios
    # and no combobox until then.
    scope = dialog.get_by_role("radio")
    expect(scope).to_have_count(2)
    expect(dialog.get_by_role("radio", name=k["scope"]["all"], exact=True)).to_be_checked()
    expect(dialog.get_by_role("combobox")).to_have_count(0)
    expect(dialog.get_by_role("switch")).to_have_count(0)
    expect(dialog.locator(".ant-form-item")).to_have_count(2)
    expect(dialog.locator(".ant-form-item-hidden")).to_have_count(0)
    check(len(mutations) == before, "reading the create field sent a key write")
    return {
        "fields": [{"label": name_label, "type": "textbox", "required": True, "value": "", "unit": None, "range": None}],
        "hidden": 0,
        "writes": 0,
    }


# Contract: gpuworks/inference-key/create-copy-delete
# Mainline: 独立 server/SQLite（无任何 Key 行，/v1 默认要求 Key）和同轮 guard Key → UI 校验、创建并真实复制推理令牌
#           → 刷新/GET脱敏回读 → 原样Bearer调用/v1/models且拒绝管理API → UI取消删除仍可用、确认删除后401
#           → guard仍可用并清理 → 零 Key 后 /v1 对所有人仍 401（放开无 Key 访问是显式的安全设置，默认关闭）
def test_inference_key_lifecycle(page, context, server, artifacts, report, locale):
    k, auth = strings(locale)
    name = f"e2e-{report['run_id']}-inference"
    guard_name = f"e2e-{report['run_id']}-guard"
    errors, console_errors, bad_responses, mutations = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            if not authenticated and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me" and "401" in message.text:
                return
            console_errors.append(message.text)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"path": path, "status": response.status})

    def request_seen(request):
        if urlsplit(request.url).path.startswith(KEY_PATH) and request.method in {"POST", "DELETE"}:
            mutations.append({"method": request.method, "path": urlsplit(request.url).path})

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)
    page.goto(server.base + "/apikeys")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=k["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    # The bootstrap mints no API key: a fresh server has no key rows at all, and
    # /v1 refuses a keyless request until an administrator creates one. The
    # refusal comes from the open-access setting, which reads closed by default,
    # not from the key count, so it is read independently here.
    fresh_keys = collection(page.request.get(server.base + KEY_PATH))
    check(fresh_keys == [], f"fresh server must start with zero key rows: {fresh_keys}")
    security = page.request.get(server.base + SECURITY_PATH)
    check(security.status == 200, f"security settings read failed: {security.status}")
    posture = security.json()
    check(posture.get("inference_open_access") is False, f"open access must default closed: {posture}")
    check(http(server.base, "GET", "/v1/models") == (401, DENIED),
          "a fresh server with no keys still admitted a keyless gateway request")
    # Keep a guard key until the revoked key has been proved unauthorized: with
    # no live key the gateway refuses every request, so leaving zero keys would
    # not distinguish "this key was revoked" from "no key exists".
    guard_response = page.request.post(server.base + KEY_PATH, data={"name": guard_name})
    check(guard_response.status == 201, "guard fixture create failed")
    guard_payload = guard_response.json()
    check(set(guard_payload) == {"name", "key"} and guard_payload["name"] == guard_name, "guard create envelope drift")
    guard = guard_payload["key"]
    artifacts.remember(guard)
    guard_prefix = guard[:12]
    check(http(server.base, "GET", "/v1/models") == (401, DENIED),
          "creating a key changed the keyless posture: /v1 must still refuse a request without a key")
    report["fixture"] = {"guard_name": guard_name, "guard_prefix": guard_prefix,
                         "fresh_server_key_rows": len(fresh_keys), "inference_open_access": posture["inference_open_access"],
                         "keyless_gateway_status": 401,
                         "reason": "A guard key keeps one live credential in the store while the target is revoked; the "
                                   "gateway posture comes from the open-access setting, which stays off, not from the key count"}
    artifacts.start_trace(context)
    create = button(page, k["create"]["button"])
    expect(create).to_be_visible()
    report["step"] = "create dialog over the inference token card"
    exercise_create_dialog(page, create, k, mutations, report)

    create.click()
    dialog = page.get_by_role("dialog", name=k["create"]["title"], exact=True)
    submit = button(dialog, k["actions"]["create"])
    field = dialog.get_by_label(k["table"]["name"], exact=True)
    modal_geometry(dialog, submit)
    expect(field).to_have_value("")
    report["key_inventory"] = exercise_key_field_inventory(dialog, k, mutations)
    for invalid in ["", "   "]:
        field.fill(invalid)
        submit.click()
        expect(dialog.get_by_text(k["create"]["nameRequired"], exact=True)).to_be_visible()
        expect(field).to_have_attribute("aria-invalid", "true")
        check(mutations == [], "invalid form sent mutation")
    field.fill(name)
    for _ in range(8):
        page.keyboard.press("Tab")
        check(dialog.evaluate("el => el.contains(document.activeElement)"), "Tab escaped creation modal")
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(create).to_be_focused()
    check(mutations == [], "cancelled create sent mutation")
    create.click()
    field.fill(name)
    field.press("Tab")
    page.keyboard.press("Shift+Tab")
    expect(field).to_be_focused()
    theme_color(field, "borderColor", "primary")
    modal_geometry(dialog, submit)
    page.screenshot(path=artifacts.directory / "create-form.png")
    report["evidence"].append({"layer": "ui", "checks": ["empty/whitespace rejected without POST", "Tab trapped inside modal", "Esc without POST and focus return", "named dialog and footer in viewport", "focus border inherits theme.primary"]})
    artifacts.stop_trace(context)
    held = []

    def hold_create(route):
        if route.request.method != "POST":
            route.continue_()
            return
        held.append(route)

    page.route("**" + KEY_PATH, hold_create)
    writes_before = len(mutations)
    submit.click()
    inflight = dialog.locator(".ant-modal-footer button.ant-btn-primary")
    expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
    inflight.click()
    field.press("Enter")
    check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
    reason = "upstream fixture is unreachable"
    with page.expect_response(lambda r: urlsplit(r.url).path == KEY_PATH and r.request.method == "POST") as failed:
        held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
    page.unroute("**" + KEY_PATH, hold_create)
    check(failed.value.status == 502, "forced create failure did not surface")
    expect(dialog).to_be_visible()
    expect(field).to_have_value(name)
    expect(page.get_by_text(k["create"]["failed"].replace("{{detail}}", reason), exact=True)).to_be_visible()
    check(bad_responses == [{"path": KEY_PATH, "status": 502}], "forced create failure was not the only HTTP error")
    bad_responses.clear()
    check(len(console_errors) == 1 and "502" in console_errors[0],
          "forced create failure console: " + str(console_errors))
    console_errors.clear()
    del mutations[writes_before:]
    report["evidence"].append({"layer": "ui", "inflight_posts": len(held), "confirm_loading": True,
        "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})
    report["sensitive_phase"] = True
    with page.expect_response(lambda r: urlsplit(r.url).path == KEY_PATH and r.request.method == "POST") as created:
        field.press("Enter")
    response = created.value
    # The default scope is submitted with the name: every model service, with an
    # empty explicit list (apikeys/KeyScope.tsx:26-32, ALL_DEPLOYMENTS).
    check(response.status == 201
          and response.request.post_data_json == {"name": name, "all_deployments": True, "deployment_ids": []}
          and not urlsplit(response.url).query,
          "incorrect inference create method/path/query/body/status")
    payload = response.json()
    check(set(payload) == {"name", "key"} and payload["name"] == name, "incorrect inference create envelope")
    secret = payload["key"]
    artifacts.remember(secret)
    check(isinstance(secret, str) and secret.startswith("gw-") and len(secret) == 51, "incorrect inference credential shape")
    secret_dialog = page.get_by_role("dialog", name=k["secret"]["title"], exact=True)
    expect(secret_dialog).to_be_visible()
    check(secret_dialog.get_by_text(secret, exact=True).count() == 1, "one-time display differs from response")
    modal_geometry(secret_dialog, button(secret_dialog, k["secret"]["saved"]))
    button(secret_dialog, k["secret"]["copy"]).click()
    copied = page.evaluate("navigator.clipboard.readText()")
    check(copied == secret, "real clipboard differs from displayed credential")
    button(secret_dialog, k["secret"]["saved"]).click()
    expect(secret_dialog).not_to_be_visible()
    page.wait_for_function("value => !document.documentElement.outerHTML.includes(value)", arg=secret)
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    report["evidence"].append({"layer": "request", "method": "POST", "path": KEY_PATH,
        "body": {"name": name}, "status": 201, "response_fields": ["name", "key"], "clipboard": "matches display and response; used unchanged; secret omitted"})
    page.reload()
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(secret[:12])
    everything = collection(page.request.get(server.base + KEY_PATH))
    items = active(everything)
    check({item["name"] for item in items} == {name, guard_name},
          f"independent GET active set drift: {[row['name'] for row in everything]}")
    item = next(item for item in items if item["name"] == name)
    # A key now reports the scope it was created with: all_deployments plus the
    # resolved deployments list (apikeys/KeyScope.tsx). The secret must still not
    # appear - the prefix is the only part of the credential in the record.
    check(set(item) == {"name", "prefix", "created_at", "all_deployments", "deployments"}
          and item["prefix"] == secret[:12]
          and item["all_deployments"] is True and item["deployments"] == [],
          "list fields/prefix/scope drift or secret leak")
    check(datetime.fromisoformat(item["created_at"].replace("Z", "+00:00")).timestamp() > 0, "invalid persisted created time")
    check(secret not in page.content() and secret not in json.dumps(everything), "persisted list or DOM reveals full secret")
    report["evidence"].append({"layer": "persistence", "method": "GET", "path": KEY_PATH, "status": 200,
        "target": item, "total": 2, "refresh": "exactly one target row; prefix only"})
    check(http(server.base, "GET", "/v1/models", copied) == (200, MODELS), "copied key cannot consume real OpenAI model collection")
    check(http(server.base, "GET", "/v1/models", copied + "x") == (401, DENIED), "incorrect secret sharing a valid prefix was accepted")
    code, denied_management = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 401 and denied_management["code"] == "AUTHENTICATION_REQUIRED", "inference key unexpectedly authorizes management")
    used = next(item for item in collection(page.request.get(server.base + KEY_PATH)) if item["name"] == name)
    check(used.get("last_used") and {key: value for key, value in used.items() if key != "last_used"} == item,
          "successful gateway use did not persist last_used or changed credential metadata")
    # 20.9: the public Bearer path and the signed-in Playground session path are
    # separate credential planes. Passing one must not stand in for the other,
    # so each is checked for the credential it must refuse.
    playground_catalog = "/api/v1/playground/models"
    bearer_on_session, bearer_session_body = http(server.base, "GET", playground_catalog, copied)
    check(bearer_on_session == 401,
          f"an inference key opened the signed-in playground session path: {bearer_on_session} {bearer_session_body}")
    session_public = page.request.get(server.base + "/v1/models")
    check(session_public.status == 401,
          f"the signed-in browser session authenticated the public Bearer path: {session_public.status}")
    check(http(server.base, "GET", "/v1/models", copied) == (200, MODELS),
          "the inference key stopped authenticating its own public path")
    report["evidence"].append({"layer": "runtime", "auth": "bearer only; no cookies", "path": "/v1/models", "status": 200,
        "body": MODELS, "wrong_secret": 401, "management": {"status": code, "body": denied_management}, "last_used": used["last_used"],
        "credential_planes": {"inference_key_on_session_path": bearer_on_session,
                              "browser_session_on_public_path": session_public.status,
                              "inference_key_on_public_path": 200,
                              "session_path": playground_catalog, "public_path": "/v1/models"},
        "limit": "Empty deployed model inventory proves gateway authentication only; no generation/GPU claim"})
    delete = button(row, k["actions"]["revoke"])
    delete.click()
    confirmation = k["revoke"]["confirm"].replace("{{name}}", name)
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    expect(popup).to_be_visible()
    expect(popup).to_be_in_viewport(ratio=1)
    button(popup, k["actions"]["cancel"]).click()
    expect(popup).not_to_be_visible()
    check(len(mutations) == 1 and http(server.base, "GET", "/v1/models", copied) == (200, MODELS), "cancel delete mutated or revoked key")
    delete.click()
    delete_path = KEY_PATH + "/" + item["prefix"]
    with page.expect_response(lambda r: urlsplit(r.url).path == delete_path and r.request.method == "DELETE") as deleted:
        button(popup, k["actions"]["revoke"]).click()
    check(deleted.value.status == 200 and deleted.value.json() == {"status": "revoked"}, "unexpected delete response")
    check(deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query, "delete body/query drift")
    expect(row).to_have_count(0)
    remaining = collection(page.request.get(server.base + KEY_PATH))
    check([(row["name"], row["prefix"]) for row in active(remaining)] == [(guard_name, guard_prefix)],
          f"delete changed the unrelated guard or left the target able to authenticate: {remaining}")
    check(item["prefix"] in [row["prefix"] for row in revoked(remaining)],
          f"the revoked target did not keep its row for usage attribution: {remaining}")
    check(http(server.base, "GET", "/v1/models", copied) == (401, DENIED), "deleted key retained runtime authorization")
    check(http(server.base, "GET", "/v1/models") == (401, DENIED), "target delete disabled gateway authentication")
    check(http(server.base, "GET", "/v1/models", guard) == (200, MODELS), "target delete revoked unrelated guard")
    page.reload()
    expect(row).to_have_count(0)
    expect(page.get_by_role("row").filter(has_text=guard_name)).to_have_count(1)
    check(mutations == [{"method": "POST", "path": KEY_PATH}, {"method": "DELETE", "path": delete_path}], "extra browser mutations")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page horizontal overflow")
    page.screenshot(path=artifacts.directory / "revoked.png")
    guard_cleanup = page.request.delete(server.base + KEY_PATH + "/" + guard_prefix)
    check(guard_cleanup.status == 200 and guard_cleanup.json() == {"status": "revoked"}, "guard cleanup failed")
    final_keys = collection(page.request.get(server.base + KEY_PATH))
    check(active(final_keys) == [], f"owned keys leaked: {final_keys}")
    check(len(revoked(final_keys)) == len(final_keys),
          f"cleanup left a key that never got revoked: {final_keys}")
    check(http(server.base, "GET", "/v1/models") == (401, DENIED),
          "deleting every key opened the gateway: keyless /v1 must stay refused because open access is off by default")
    final_posture = page.request.get(server.base + SECURITY_PATH)
    check(final_posture.status == 200 and final_posture.json().get("inference_open_access") is False,
          f"this case never changed the security posture, yet it now reads: {final_posture.status} {final_posture.text()[:200]}")
    check(not errors and not console_errors and not bad_responses, "unexpected browser errors: " + str([errors, console_errors, bad_responses]))
    report["evidence"].append({"layer": "cleanup", "method": "DELETE", "path": delete_path, "status": 200,
        "cancel": "no DELETE, original key still 200", "revoked_key": 401, "keyless": 401, "guard_key": 200,
        "refresh_and_get": "target absent, guard unchanged",
        "fixture_cleanup": "guard DELETE 200 then the collection has no active row; the stored open-access policy still reads false; keyless /v1/models 401 because open access is off, not because a key happens to exist",
        "browser_mutations": mutations})
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": bad_responses}


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
    manifest_path = ROOT / "test/e2e/coverage/inference-key-lifecycle.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_inference_key_lifecycle.__name__, "manifest does not bind case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN", "evidence": [],
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin creates; inference bearer consumes without cookies",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900}, "selection": [CONTRACT], "retries": 0,
        "coverage_manifest_sha256": digest(manifest_path), "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/platform_token_read_revoke.py"),
        "dialog_checks_sha256": digest(ROOT / "test/e2e/dialog_checks.py"),
        "execution": "N/A: gateway key authorization requires no agent; model inventory is empty",
        "not_covered": manifest["missing"], "command": ["python3", "test/e2e/inference_key_lifecycle.py", *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US",
                timezone_id="UTC", permissions=["clipboard-read", "clipboard-write"])
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_inference_key_lifecycle(page, context, server, artifacts, report, args.locale)
                check(set(manifest["required_evidence"]) <= {item["layer"] for item in report["evidence"]}, "missing evidence")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = traceback.format_exc()
                artifacts.write_json("report.json", report)
                if report.get("sensitive_phase") or any(value in page.content() for value in artifacts.secrets if len(value) > 8):
                    report["failure_screenshot"] = "omitted: a credential may be visible"
                else:
                    page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        report["private_state"] = "private/ retains isolated DB/log; no process retained; owner removes scoped run after diagnosis within 7 days"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; do not upload"
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": CONTRACT, "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
