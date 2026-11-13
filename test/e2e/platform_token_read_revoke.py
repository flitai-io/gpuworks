#!/usr/bin/env python3
"""Single-machine browser closure: read token creation, authorization, revocation.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-platform-token
Requires the existing Python Playwright installation and system Chrome.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
import json
import os
import re
from pathlib import Path
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright

from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http


CONTRACT = "gpuworks/platform-token/read-create-revoke"
CASE = "平台 API 令牌 / UI 创建只读令牌并吊销 / 刷新持久化、只读权限生效且吊销立即失效"
TOKEN_PATH = "/api/v1/platform-tokens"
ITEM_FIELDS = {"id", "name", "prefix", "scope", "created_at", "expires_at", "last_used_at"}


def strings(locale):
    directory = ROOT / "ui/src/i18n/locales" / locale
    return (json.loads((directory / "apikeys.json").read_text()),
            json.loads((directory / "auth.json").read_text()))


def check(condition, description):
    # Never include actual credentials in assertion diagnostics.
    if not condition:
        raise AssertionError(description)


def button(scope, label):
    # AntD inserts a display space into two-character Chinese button text.
    accessible = r"\s*".join(label) if re.fullmatch(r"[\u3400-\u9fff]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile("^" + accessible + "$"))


def collection(response):
    check(response.status == 200, f"collection HTTP {response.status}")
    body = response.json()
    check(set(body) == {"items", "total"}, "unexpected collection envelope")
    check(isinstance(body["items"], list) and body["total"] == len(body["items"]), "incorrect collection total")
    return body["items"]


def modal_geometry(dialog, submit):
    expect(dialog).to_be_visible()
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(submit).to_be_in_viewport(ratio=1)
    # Modal scale animation changes both rectangles. Read them in one frame only
    # after that animation ends; separate protocol calls compare different frames.
    dialog.page.wait_for_function("""({dialog, submit}) => {
        if (dialog.getAnimations().some(a => a.pending || a.playState === 'running')) return false;
        const box = dialog.getBoundingClientRect();
        const button = submit.getBoundingClientRect();
        return box.width > 0 && box.height > 0 && button.width > 0 && button.height > 0 &&
            box.left >= 0 && box.top >= 0 && box.right <= innerWidth && box.bottom <= innerHeight &&
            button.left >= box.left && button.top >= box.top &&
            button.right <= box.right && button.bottom <= box.bottom;
    }""", arg={"dialog": dialog.element_handle(), "submit": submit.element_handle()}, timeout=5000)


def exercise_create_dialog(page, create, platform, k, mutations, report):
    """Create dialog identity, layout, focus, and dismissals. No token POST."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    title = platform["createTitle"]
    card = page.locator(".ant-card").filter(has_text=platform["title"]).first

    def posts():
        return sum(1 for item in mutations if item["method"] == "POST" and item["path"] == TOKEN_PATH)

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
        posts, create)
    cancel_button(k["actions"]["cancel"])(dialog)
    expect(dialog).to_be_hidden()
    expect(card).to_be_visible()
    check(posts() == 0, "create dialog dismissal sent POST")
    report["evidence"].append({"layer": "ui", "create_dialog": title,
        "dismissals": ["cancel", "escape", "close", "mask"], "post_requests": 0})


def theme_color_source(token_name, mode="light"):
    """Resolve a named color to its hex value from the generated theme tokens.

    tokens.ts stopped holding literal hex: each entry of `colors` now names a
    semantic token in the generated Zeron set (tokens.ts:22-36, e.g.
    `primary: semantic["brand"]`). Read the mapping, then the value it points at,
    for the mode the page is actually showing.
    """
    source = (ROOT / "ui/src/theme/tokens.ts").read_text()
    mapped = re.search(rf'^  {re.escape(token_name)}: semantic\["([^"]+)"\]', source, re.M)
    if mapped is not None:
        generated = (ROOT / "ui/src/theme/zeron.generated.ts").read_text()
        value = re.search(rf'"{re.escape(mapped[1])}": "(#[0-9a-fA-F]{{6}})"',
                          generated.split(f'"{mode}": {{', 1)[-1], re.M)
        check(value is not None, f"theme color {token_name} absent from the {mode} generated tokens")
        return value[1]
    # A literal hex in tokens.ts still resolves directly.
    literal = re.search(rf'^  {re.escape(token_name)}: "(#[0-9a-fA-F]{{6}})"', source, re.M)
    check(literal is not None, f"theme color {token_name} absent")
    return literal[1]


def theme_color(locator, property_name, token_name):
    mode = locator.page.evaluate(
        "() => document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light'")
    hex_value = theme_color_source(token_name, mode)
    expected = [int(hex_value[i:i + 2], 16) for i in (1, 3, 5)] + [255]
    # Normalize CSS Color serialization without inserting or changing UI DOM.
    locator.page.wait_for_function("""({el, property, expected}) => {
        const ctx = new OffscreenCanvas(1, 1).getContext('2d');
        ctx.fillStyle = getComputedStyle(el)[property];
        ctx.fillRect(0, 0, 1, 1);
        return Array.from(ctx.getImageData(0, 0, 1, 1).data).every((value, i) => value === expected[i]);
    }""", arg={"el": locator.element_handle(), "property": property_name, "expected": expected}, timeout=5000)


# Contract: gpuworks/platform-token/read-create-revoke
# Mainline: 独立真实 server/SQLite → UI 校验、取消并创建默认只读令牌 → 真实复制且刷新/GET脱敏回读
#           → 无Cookie调用读取200、写入403且无副作用 → UI取消吊销仍可用、确认吊销 → 刷新消失且旧令牌401
def exercise_complete_token(page, context, server, artifacts, report, k, platform, mutations):
    """Fill both optional fields, then read back every stored field."""
    from platform_token_options import select_expiry

    name = f"e2e-{report['run_id']}-write30"
    before = len(mutations)
    create = button(page, platform["create"])
    create.click()
    dialog = page.get_by_role("dialog", name=platform["createTitle"], exact=True)
    expect(dialog).to_be_visible()
    dialog.get_by_label(k["table"]["name"], exact=True).fill(name)
    write = dialog.get_by_role("radio", name=platform["scopes"]["write"], exact=True)
    write.check()
    expect(write).to_be_checked()
    labels = [platform["noExpiry"] if value == 0 else platform["days"].replace("{{count}}", str(value))
              for value in (0, 30, 90, 365)]
    chosen = labels[1]
    select_expiry(page, dialog, platform["expiresAt"], chosen, labels)
    expect(dialog.get_by_role("radio", name=platform["scopes"]["read"], exact=True)).not_to_be_checked()
    clock = page.request.get(server.base + TOKEN_PATH)
    collection(clock)
    t0 = parsedate_to_datetime(clock.headers["date"])
    artifacts.stop_trace(context)
    report["sensitive_phase"] = True
    report["last_step"] = "complete-token-submit"
    with page.expect_response(lambda r: urlsplit(r.url).path == TOKEN_PATH and r.request.method == "POST") as created:
        button(dialog, k["actions"]["create"]).click()
    response = created.value
    check(response.status == 201, f"complete create HTTP {response.status}")
    check(response.request.post_data_json == {"name": name, "scope": "write", "expires_in_days": 30},
          "complete create body differs")
    check(not urlsplit(response.url).query, "complete create sent a query")
    payload = response.json()
    check(set(payload) == {"token", "item"}, "complete create envelope differs")
    secret, item = payload["token"], payload["item"]
    artifacts.remember(secret)
    check(isinstance(secret, str) and secret.startswith("gwp_"), "complete create returned no token")
    check(set(item) == ITEM_FIELDS, "complete create item fields differ")
    check(item["name"] == name and item["scope"] == "write", "complete create did not store the chosen name and scope")
    check(item["prefix"] == secret[:12] and isinstance(item["id"], int), "complete create prefix differs")
    expiry = datetime.fromisoformat(item["expires_at"].replace("Z", "+00:00"))
    t1 = parsedate_to_datetime(response.headers["date"]) + timedelta(seconds=1)
    check(t0 + timedelta(days=30) <= expiry < t1 + timedelta(days=30), "stored expiry is not 30 UTC days")
    rendered_expiry = expiry.astimezone(timezone.utc).strftime("%Y-%m-%d %H:%M")
    secret_dialog = page.get_by_role("dialog", name=k["secret"]["title"], exact=True)
    expect(secret_dialog).to_be_visible()
    button(secret_dialog, k["secret"]["saved"]).click()
    expect(secret_dialog).not_to_be_visible()
    page.wait_for_function("secret => !document.documentElement.outerHTML.includes(secret)", arg=secret, timeout=5000)
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    report["last_step"] = "complete-token-persisted"
    page.reload()
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(platform["scopes"]["write"])
    expect(row).to_contain_text(item["prefix"])
    expect(row).to_contain_text(rendered_expiry)
    persisted = collection(page.request.get(server.base + TOKEN_PATH))
    check(persisted == [item], "independent GET differs from the complete token")
    check(secret not in page.content() and secret not in json.dumps(persisted), "complete token secret leaked")
    tag = row.locator(".ant-tag-orange")
    expect(tag).to_have_count(1, timeout=12000)
    expect(tag).to_have_text(platform["scopes"]["write"])
    report["last_step"] = "complete-token-tag"
    report["complete_token"] = {"method": "POST", "path": TOKEN_PATH,
        "body": {"name": name, "scope": "write", "expires_in_days": 30}, "status": 201,
        "stored": {"name": item["name"], "scope": item["scope"], "expires_at": item["expires_at"],
                   "prefix": item["prefix"]},
        "tag": "ant-tag-orange", "ui_expiry_utc": rendered_expiry}
    confirmation = platform["revokeConfirm"].replace("{{name}}", name)
    button(row, platform["revoke"]).click()
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    expect(popup).to_be_visible()
    delete_path = f"{TOKEN_PATH}/{item['id']}"
    with page.expect_response(lambda r: urlsplit(r.url).path == delete_path and r.request.method == "DELETE") as deleted:
        button(popup, platform["revoke"]).click()
    check(deleted.value.status == 204, f"complete token revoke HTTP {deleted.value.status}")
    expect(row).to_have_count(0)
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "complete token remained after revoke")
    check(mutations[before:] == [{"method": "POST", "path": TOKEN_PATH}, {"method": "DELETE", "path": delete_path}],
          "complete token mutations differ")
    report["evidence"].append({"layer": "request", "method": "POST", "path": TOKEN_PATH,
        "body": {"name": name, "scope": "write", "expires_in_days": 30}, "status": 201,
        "expires_at": item["expires_at"], "scope": "write", "tag": "ant-tag-orange"})
    report["evidence"].append({"layer": "cleanup", "method": "DELETE", "path": delete_path, "status": 204,
        "refresh_and_get": "empty"})


def test_read_token_create_revoke(page, context, server, artifacts, report, locale):
    k, auth = strings(locale)
    platform = k["platform"]
    resource_name = f"e2e-{report['run_id']}-read"
    page_errors, console_errors, bad_responses, mutations = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))
    def console_seen(message):
        if message.type != "error":
            return
        if (not authenticated and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me"
                and "401" in message.text):
            return
        console_errors.append(message.text)

    page.on("console", console_seen)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return  # Exactly the anonymous session probe, before login.
            bad_responses.append({"path": path, "status": response.status})

    def request_seen(request):
        if urlsplit(request.url).path.startswith(TOKEN_PATH) and request.method in {"POST", "DELETE"}:
            mutations.append({"method": request.method, "path": urlsplit(request.url).path})

    page.on("response", response_seen)
    page.on("request", request_seen)
    page.goto(server.base + "/apikeys")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "browser login failed")
    # The UI consumes the HttpOnly session, not the unused login JWT body.
    # Do not retain that additional credential or wait for its body download.
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=k["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "fixture token collection must start empty")
    initial_keys = collection(page.request.get(server.base + "/api/v1/apikeys"))
    artifacts.start_trace(context)

    create = button(page, platform["create"])
    expect(create).to_be_visible()
    report["step"] = "create dialog over the platform token card"
    exercise_create_dialog(page, create, platform, k, mutations, report)

    create.click()
    dialog = page.get_by_role("dialog", name=platform["createTitle"], exact=True)
    submit = button(dialog, k["actions"]["create"])
    name = dialog.get_by_label(k["table"]["name"], exact=True)
    modal_geometry(dialog, submit)
    expect(name).to_have_value("")
    from platform_token_options import exercise_token_field_inventory
    report["token_inventory"] = exercise_token_field_inventory(page, dialog, k, platform, mutations)
    expect(dialog.get_by_role("radio", name=platform["scopes"]["read"], exact=True)).to_be_checked()
    expect(dialog.locator(".ant-select-selection-item")).to_have_text(platform["noExpiry"])
    for invalid in ["", "   "]:
        name.fill(invalid)
        submit.click()
        expect(dialog.get_by_text(k["create"]["nameRequired"], exact=True)).to_be_visible()
        expect(name).to_have_attribute("aria-invalid", "true")
        check(not mutations, "invalid name sent a token mutation")
    name.fill(resource_name)
    name.press("Tab")
    for _ in range(12):
        check(dialog.evaluate("el => el.contains(document.activeElement)"), "Tab escaped modal focus trap")
        page.keyboard.press("Tab")
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(create).to_be_focused()
    check(not mutations, "cancelled form sent a mutation")
    report["evidence"].append({"layer": "ui", "checks": ["default read/no expiry", "empty/whitespace rejected without mutation", "Esc cancellation", "focus trap/return", "modal viewport"]})

    create.click()
    name.fill(resource_name)
    modal_geometry(dialog, submit)
    # Keyboard focus triggers the shared input focus style in the browser.
    name.press("Tab")
    page.keyboard.press("Shift+Tab")
    expect(name).to_be_focused()
    theme_color(name, "borderColor", "primary")
    page.screenshot(path=artifacts.directory / "create-form.png")
    held = []

    def hold_create(route):
        if route.request.method != "POST":
            route.continue_()
            return
        held.append(route)

    page.route("**" + TOKEN_PATH, hold_create)
    writes_before = len(mutations)
    submit.click()
    inflight = dialog.locator(".ant-modal-footer button.ant-btn-primary")
    expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
    inflight.click()
    name.press("Enter")
    check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
    reason = "upstream fixture is unreachable"
    with page.expect_response(lambda r: urlsplit(r.url).path == TOKEN_PATH and r.request.method == "POST") as failed:
        held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
    page.unroute("**" + TOKEN_PATH, hold_create)
    check(failed.value.status == 502, "forced create failure did not surface")
    expect(dialog).to_be_visible()
    expect(name).to_have_value(resource_name)
    expect(page.get_by_text(reason, exact=True)).to_be_visible()
    check(bad_responses == [{"path": TOKEN_PATH, "status": 502}], "forced create failure was not the only HTTP error")
    bad_responses.clear()
    check(len(console_errors) == 1 and "502" in console_errors[0],
          "forced create failure console: " + str(console_errors))
    console_errors.clear()
    del mutations[writes_before:]
    report["evidence"].append({"layer": "ui", "inflight_posts": len(held), "confirm_loading": True,
        "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})
    # Stop recording BEFORE the POST or the one-time secret reaches the DOM.
    artifacts.stop_trace(context)
    report["sensitive_phase"] = True
    with page.expect_response(lambda r: urlsplit(r.url).path == TOKEN_PATH and r.request.method == "POST") as created:
        submit.click()
    response = created.value
    check(response.status == 201, f"create HTTP {response.status}")
    check(response.request.post_data_json == {"name": resource_name, "scope": "read"}, "incorrect create JSON/default expiry omission")
    check(not urlsplit(response.url).query, "unexpected create query")
    payload = response.json()
    check(set(payload) == {"token", "item"}, "incorrect created envelope")
    secret, item = payload["token"], payload["item"]
    artifacts.remember(secret)
    check(isinstance(secret, str) and secret.startswith("gwp_"), "missing one-time platform token")
    check(set(item) == ITEM_FIELDS, "created item fields expose secret or drifted")
    check(item["name"] == resource_name and item["scope"] == "read" and item["expires_at"] is None,
          "created item does not preserve defaults")
    check(item["prefix"] == secret[:12] and isinstance(item["id"], int), "incorrect token prefix/id")
    check(datetime.fromisoformat(item["created_at"].replace("Z", "+00:00")).timestamp() > 0, "invalid creation time")
    report["resource_id"] = item["id"]
    secret_dialog = page.get_by_role("dialog", name=k["secret"]["title"], exact=True)
    expect(secret_dialog).to_be_visible()
    check(secret_dialog.get_by_text(secret, exact=True).count() == 1, "one-time display differs from created token")
    modal_geometry(secret_dialog, button(secret_dialog, k["secret"]["saved"]))
    theme_color(secret_dialog.get_by_text(secret, exact=True), "backgroundColor", "canvas")
    button(secret_dialog, k["secret"]["copy"]).click()
    copied = page.evaluate("navigator.clipboard.readText()")
    check(copied == secret, "clipboard differs from the one-time token")
    button(secret_dialog, k["secret"]["saved"]).click()
    expect(secret_dialog).not_to_be_visible()
    # The hidden modal must also release the secret before snapshots resume.
    page.wait_for_function("secret => !document.documentElement.outerHTML.includes(secret)", arg=secret, timeout=5000)
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    report["evidence"].append({"layer": "request", "method": "POST", "path": TOKEN_PATH,
        "body": {"name": resource_name, "scope": "read"}, "status": 201, "item": item,
        "computed_styles": {"keyboard_focus_border": "theme.colors.primary", "secret_background": "theme.colors.canvas"},
        "clipboard": "equal to one-time value; consumed unchanged as bearer; secret omitted"})

    page.reload()
    row = page.get_by_role("row").filter(has_text=resource_name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(platform["scopes"]["read"])
    expect(row).to_contain_text(platform["noExpiry"])
    expect(secret_dialog).not_to_be_visible()
    check(secret not in page.content(), "reload reveals full secret")
    persisted = collection(page.request.get(server.base + TOKEN_PATH))
    check(persisted == [item], "independent GET differs from created record")
    check(secret not in json.dumps(persisted), "list leaks full secret")
    report["evidence"].append({"layer": "persistence", "method": "GET", "path": TOKEN_PATH, "status": 200, "items": persisted, "reload": "one row, no secret"})

    # No browser request context/cookies: this is actual bearer authorization.
    code, identity = http(server.base, "GET", "/api/v1/auth/me", copied)
    check(code == 200 and identity["username"] == "admin" and identity["role"] == "admin", "token attributed to incorrect principal")
    code, nodes = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 200 and nodes == {"items": [], "total": 0}, "read token cannot read isolated node inventory")
    report["evidence"].append({"layer": "runtime", "auth": "bearer only; no cookies", "path": "/api/v1/nodes", "status": code, "body": nodes, "principal": "admin"})
    code, denied = http(server.base, "POST", "/api/v1/apikeys", copied, {"name": resource_name + "-denied"})
    check(code == 403 and denied == {"code": "PLATFORM_TOKEN_READ_ONLY", "error": "a read token can only read", "params": {"scope": "read"}}, "read token write was not denied by scope")
    check(collection(page.request.get(server.base + "/api/v1/apikeys")) == initial_keys, "denied write mutated inference tokens")
    report["evidence"].append({"layer": "runtime", "method": "POST", "path": "/api/v1/apikeys", "status": code, "body": denied, "side_effects": "none (session GET equals baseline)"})
    code, denied = http(server.base, "GET", TOKEN_PATH, copied)
    check(code == 403 and denied["code"] == "PLATFORM_TOKEN_NOT_ALLOWED", "platform token accessed session-only management")
    report["evidence"].append({"layer": "runtime", "method": "GET", "path": TOKEN_PATH, "status": code, "body": denied})
    used = collection(page.request.get(server.base + TOKEN_PATH))[0]
    check(used["id"] == item["id"] and used["last_used_at"] is not None, "successful bearer use was not persisted")

    # 7.7: a platform token has its own lifecycle and is not a management session, so dropping
    # the browser session must not touch it. The token keeps reading while the console's own
    # credential stops being presented - the two are separate channels, which is why the
    # session-revocation half of this item is a CLI action rather than something a browser can
    # trigger, and no attempt is made to claim it here.
    # 7.5: the three credential kinds are told apart by what they are, not by what a caller claims.
    # An inference key is a different family from a platform token: presented as a bearer it is not
    # treated as a platform token, so it cannot reach the token routes, and it is not a management
    # session either, so it cannot read management data. The product carries no expiry field on any
    # of the three, so no expiry control is invented and none is asserted.
    created_key = page.request.post(server.base + "/api/v1/apikeys", data={"name": resource_name + "-gw"})
    check(created_key.status in {200, 201}, f"inference key create HTTP {created_key.status}")
    inference_secret = created_key.json().get("key")
    check(isinstance(inference_secret, str) and inference_secret.startswith("gw-"),
          "the inference key does not carry its own prefix")
    check(not inference_secret.startswith("gwp_"),
          "an inference key was issued in the platform token family")
    inference_headers = {"Authorization": "Bearer " + inference_secret}
    code, refused_read = http(server.base, "GET", "/api/v1/nodes", inference_headers)
    check(code in {401, 403},
          f"an inference key read management inventory: {code}")
    check(refused_read.get("code") != "PLATFORM_TOKEN_NOT_ALLOWED",
          "an inference key was mistaken for a platform token")
    code, refused_token_route = http(server.base, "GET", TOKEN_PATH, inference_headers)
    check(code in {401, 403}, f"an inference key reached the platform token routes: {code}")
    report["evidence"].append({"layer": "runtime", "credential_families": {
        "inference_prefix": "gw-", "platform_prefix": "gwp_", "management": "session cookie",
        "inference_refused_management": True, "inference_not_platform_token": True,
        "expiry_fields": "none on any of the three"}})
    prefix = created_key.json()["key"][:12]
    page.request.delete(server.base + "/api/v1/apikeys/" + prefix)

    page.evaluate("() => { localStorage.clear(); sessionStorage.clear(); }")
    code, still = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 200 and still == {"items": [], "total": 0},
          f"a platform token stopped working when the browser session was dropped: {code}")
    code, untouched = http(server.base, "GET", TOKEN_PATH, copied)
    check(code == 403 and untouched["code"] == "PLATFORM_TOKEN_NOT_ALLOWED",
          "the token gained session-only management access after the console session was dropped")
    report["evidence"].append({"layer": "runtime", "token_survives_session_drop": True,
        "session_only_still_denied": True,
        "note": "password change revokes sessions through the store but is reached by the reset CLI, not by a browser route, so no UI path is invented for it"})

    revoke = button(row, platform["revoke"])
    revoke.click()
    confirmation = platform["revokeConfirm"].replace("{{name}}", resource_name)
    confirm_text = page.get_by_text(confirmation, exact=True)
    expect(confirm_text).to_be_visible()
    expect(confirm_text).to_be_in_viewport(ratio=1)
    button(page, k["actions"]["cancel"]).click()
    expect(confirm_text).not_to_be_visible()
    check(len(mutations) == 1, "cancel revoke sent a mutation")
    code, _ = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 200, "cancel revoke invalidated token")
    revoke.click()
    expect(confirm_text).to_be_visible()
    # Confirmation lives in AntD's accessible tooltip popup, not in the row.
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    confirm = button(popup, platform["revoke"])
    expect(confirm).to_be_in_viewport(ratio=1)
    delete_path = f"{TOKEN_PATH}/{item['id']}"
    with page.expect_response(lambda r: urlsplit(r.url).path == delete_path and r.request.method == "DELETE") as deleted:
        confirm.click()
    check(deleted.value.status == 204, "revoke did not return 204")
    check(deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query, "unexpected revoke body/query")
    expect(row).to_have_count(0)
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "revoked token still persisted")
    code, denied = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 401 and denied["code"] == "AUTHENTICATION_REQUIRED", "revoked token remains authorized")
    # 7.6: a refusal must refuse without quoting the credential back. The body is read as text so a
    # secret echoed in any field - or in a debug string - is caught, not only one in a known key.
    refusal_text = json.dumps(denied)
    check(copied not in refusal_text, "the 401 body echoed the revoked credential")
    check(copied[:16] not in refusal_text, "the 401 body echoed the revoked credential's prefix")
    # A second revoke of the same id must fail cleanly: the row is already gone,
    # so the answer is 404 and nothing else in the collection moves.
    again = page.request.delete(server.base + delete_path)
    again_body = again.json() if again.text() else {}
    check(again.status == 404 and again_body.get("error") == "not found",
          "second revoke did not report not-found: HTTP %d %r" % (again.status, again_body))
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [],
          "a failed revoke left a token behind")
    report["revoke_failure"] = {"status": again.status, "error": again_body.get("error")}
    page.reload()
    expect(page.get_by_text(platform["empty"], exact=True)).to_be_visible()
    expect(row).to_have_count(0)
    check(secret not in page.content(), "secret reappeared after revocation")
    check(mutations == [{"method": "POST", "path": TOKEN_PATH}, {"method": "DELETE", "path": delete_path}], "extra token mutations")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page has horizontal overflow")
    page.screenshot(path=artifacts.directory / "revoked.png")
    exercise_complete_token(page, context, server, artifacts, report, k, platform, mutations)
    check(not page_errors, "unhandled browser errors: " + str(page_errors))
    check(not console_errors, "unexpected browser console errors: " + str(console_errors))
    check(not bad_responses, "unexpected browser HTTP errors: " + str(bad_responses))
    report["evidence"].append({"layer": "cleanup", "method": "DELETE", "path": delete_path,
        "status": 204, "cancel": "no mutation; bearer still 200", "refresh_and_get": "empty",
        "revoked_bearer": {"status": code, "body": denied}, "mutations": mutations})
    report["browser_errors"] = {"page": page_errors, "console": console_errors, "http": bad_responses}


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
    manifest_path = ROOT / "test/e2e/coverage/platform-token-read-revoke.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_read_token_create_revoke.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin creates / read bearer consumes",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "dialog_checks_sha256": digest(ROOT / "test/e2e/dialog_checks.py"),
        "execution": "N/A: platform authorization has no agent/workload",
        "not_covered": ["expiry options other than 30 days", "full field/list matrix", "non-admin owner isolation", "service-failure recovery"],
        "command": ["python3", "test/e2e/platform_token_read_revoke.py", *sys.argv[1:]]}
    try:
        server.require_binary()
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
                test_read_token_create_revoke(page, context, server, artifacts, report, args.locale)
                layers = {e["layer"] for e in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers, "required evidence is missing")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"  # Raw failure retained; triage may identify a product defect.
                report["failure"] = traceback.format_exc()
                artifacts.write_json("report.json", report)
                if report.get("sensitive_phase") or any(value in page.content() for value in artifacts.secrets if len(value) > 8):
                    report["failure_screenshot"] = "omitted at capture boundary: a credential may be visible; use preceding safe trace/form screenshot"
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
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            # 7.6: the log the server wrote is the surface, not the copy that was redacted on the way
            # out. Reading the original is the only way to know the server did not write a credential
            # down itself; scanning the sanitised copy would prove nothing about the product.
            raw_log = server_log.read_text(errors="replace")
            leaked = [value[:8] + "..." for value in artifacts.secrets
                      if len(value) >= 16 and value in raw_log]
            report["server_log_clean"] = not leaked
            if leaked:
                report["status"] = "FAIL_TEST"
                report["server_log_leak"] = {"count": len(leaked), "prefixes": leaked}
            (artifacts.directory / "server.log").write_text(artifacts.redact(raw_log))
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
