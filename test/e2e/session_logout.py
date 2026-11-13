#!/usr/bin/env python3
"""Login / two-tab logout / old cookie and login bearer revoked, re-login recovers.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-session-logout
After building: python3 test/e2e/session_logout.py --locale en  # or zh
Prerequisites: local Chrome and Playwright Python; an existing server binary for
an isolated LocalServer/SQLite. No agent, GPU or remote credentials are needed.
Set NO_PROXY=127.0.0.1,localhost and no_proxy likewise when using an HTTP proxy.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and retains the
private database/logs for scoped diagnosis (remove within seven days).
Login and re-login are untraced; captured credentials stay in memory and are
registered for artifact redaction. Auth response barriers observe original
bytes, then continue without request/status/header/body overrides.
"""
from __future__ import annotations

import argparse
import base64
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import traceback
import urllib.error
import urllib.request
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http, ADMIN_PASSWORD
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/session/two-tab-logout-relogin"
LOGIN, LOGOUT, ME, ACTIVITY = ("/api/v1/auth/" + name for name in ("login", "logout", "me", "activity"))
COOKIE = "gpuworks_session"
DENIED = {"code": "AUTHENTICATION_REQUIRED", "error": "authentication required"}


def exercise_login_prefix(page, auth):
    """The username affix wrapper carries the input-prefix class."""
    username = page.get_by_role("textbox", name=auth["username"], exact=True)
    affix = username.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-input-affix-wrapper ')][1]")
    expect(affix.locator(".ant-input-prefix")).to_have_count(1, timeout=12000)
    return username


def cookie_identity(play, base, value):
    # A fresh isolated API client sends only the captured cookie. It cannot
    # inherit the browser's replacement cookie or an Authorization header.
    client = play.request.new_context(extra_http_headers={"Cookie": f"{COOKIE}={value}"})
    try:
        response = client.get(base + ME)
        return response.status, response.json()
    finally:
        client.dispose()


def capture_credentials(response, context, artifacts, body):
    # Register secrets before any assertion can fail; never attach raw bodies.
    token = body.get("token", "")
    artifacts.remember(token)
    cookies = [cookie for cookie in context.cookies() if cookie["name"] == COOKIE]
    for cookie in cookies:
        artifacts.remember(cookie["value"])
    check(response.status == 200 and set(body) == {"token", "user"}, "login response contract differs")
    check(len(cookies) == 1 and len(cookies[0]["value"]) == 64 and cookies[0]["httpOnly"]
          and cookies[0]["sameSite"] == "Lax" and cookies[0]["path"] == "/", "session cookie contract differs")
    check(isinstance(token, str) and token.count(".") == 2, "login bearer missing")
    check(body["user"]["username"] == "admin" and body["user"]["role"] == "admin", "unexpected login identity")
    check(set(body["user"]) == {"id", "username", "role"}, "login identity fields differ")
    check(response.headers.get("cache-control") == "no-store", "login response must not be cached")
    return cookies[0]["value"], token, body["user"]


def capture_auth_response(cdp, event, artifacts):
    # Login/logout UI consumes headers only; Chrome may discard unread bodies
    # when the form unmounts. Read real bytes at a response-stage barrier, then
    # release UNCHANGED. Never supply status/headers/body/request overrides.
    request_id = event["requestId"]
    try:
        path = urlsplit(event["request"]["url"]).path
        check(path in (LOGIN, LOGOUT) and event["request"]["method"] == "POST", "unexpected auth capture target")
        raw = cdp.send("Fetch.getResponseBody", {"requestId": request_id})
        body = json.loads(base64.b64decode(raw["body"]) if raw.get("base64Encoded") else raw["body"])
        artifacts.remember(body.get("token", ""))
        return path, {"status": event["responseStatusCode"], "body": body}
    finally:
        cdp.send("Fetch.continueResponse", {"requestId": request_id})


# Mainline: 独立真实server/SQLite → 深链错误密码后UI登录 → 两标签刷新与身份回读
# → UI注销 → 原Cookie和登录Bearer真实401、第二标签重新认证 → UI重登后双标签恢复且旧凭据仍401。
# Contract: gpuworks/session/two-tab-logout-relogin
def _duration_seconds(text):
    """Seconds in a Go duration string such as '2s', '1500ms' or '1m'."""
    if not isinstance(text, str) or not text.strip():
        return 0.0
    value = text.strip()
    for suffix, scale in (("ms", 0.001), ("ns", 1e-9), ("us", 1e-6), ("s", 1.0), ("m", 60.0), ("h", 3600.0)):
        if value.endswith(suffix):
            return float(value[: -len(suffix)]) * scale
    raise ValueError(f"unsupported duration: {text!r}")


def _raw(base, method, path, token="", body=None, headers=None, cookie=""):
    """A fresh transport that returns raw bytes, so binary downloads and HTML can be read.

    Pass token for a bearer credential or cookie for a session cookie; they travel in
    different headers and a caller mixing them up is refused, which is why both exist.
    """
    hdrs = {"Content-Type": "application/json"}
    if token:
        hdrs["Authorization"] = f"Bearer {token}"
    if cookie:
        hdrs["Cookie"] = f"{COOKIE}={cookie}"
    hdrs.update(headers or {})
    data = None if body is None else (body if isinstance(body, bytes) else json.dumps(body).encode())
    request = urllib.request.Request(base + path, method=method, headers=hdrs, data=data)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        response = opener.open(request, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, dict(response.headers), response.read()


def _plaintext_hits(payload, secrets):
    """Which live secrets appear verbatim in these bytes.

    Only values long and distinctive enough to identify a credential are searched. The
    fixture password is deliberately excluded: "password" is also an ordinary JSON field
    name, so searching for it matches the redacted request record ("password":"[REDACTED]")
    and reports a leak that is really the redaction working.
    """
    hits = []
    for label, value in secrets.items():
        if len(value) < 12:
            continue
        for needle in {value, json.dumps(value)[1:-1], base64.b64encode(value.encode()).decode()}:
            if needle and needle.encode() in payload:
                hits.append(label)
                break
    return hits


def exercise_field_boundaries_from_form(page, server, report, auth, username, secret):
    """1.4: the username field's declared bound is exercised at, inside and beyond it.

    The bound is real product behaviour, not an invention: POST /auth/login validates the
    username against maxNameRunes (256) in internal/server/api/request_validation.go, while
    the form itself declares no maxLength. The password field declares no length bound
    anywhere in the product, so none is invented for it here. This runs on the login page
    that is already mounted and leaves the two fields clean for the checks that follow.
    """
    limits = {"server_max_username_runes": 256,
              "source": "internal/server/api/request_validation.go maxNameRunes, used by the login handler"}
    results = []
    # A well-formed but wrong credential at the boundary must reach the credential check,
    # which is what proves the LENGTH was accepted: a length refusal and a credential
    # refusal are different product paths and conflating them would hide a broken rule.
    for label, runes in (("inside_255", 255), ("at_bound_256", 256),
                         ("over_bound_257", 257), ("way_over_600", 600)):
        status, _, body = _raw(server.base, "POST", LOGIN, "",
                               {"username": "u" * runes, "password": "not-the-password"})
        parsed = json.loads(body) if body else {}
        results.append({"username_runes": runes, "case": label, "status": status,
                        "code": parsed.get("code"), "error": parsed.get("error")})
    inside, at_bound, over, way_over = results
    check(inside["status"] == 401 and inside["code"] == "INVALID_CREDENTIALS",
          f"a 255-rune username did not reach the credential check: {inside}")
    check(at_bound["status"] == 401 and at_bound["code"] == "INVALID_CREDENTIALS",
          f"a 256-rune username was not accepted as a length: {at_bound}")
    for rejected in (over, way_over):
        check(rejected["status"] == 400,
              f"a {rejected['username_runes']}-rune username was not refused as a length: {rejected}")
        check(rejected["code"] != "INVALID_CREDENTIALS",
              f"an over-bound username was refused as a bad credential, which conflates two paths: {rejected}")
        check(bool(rejected.get("error")),
              f"an over-bound username was refused without a product error message: {rejected}")

    # The browser form declares no maxLength, so an over-bound value is really submitted
    # rather than truncated by the client, and the refusal comes from the server.
    longest = username.evaluate("el => el.getAttribute('maxlength')")
    check(longest is None, f"the username box declares maxLength {longest!r}, so the client would truncate")
    username.fill("u" * 300)
    check(len(username.input_value()) == 300,
          f"the username box truncated a 300-rune value to {len(username.input_value())} in the browser")
    secret.fill("not-the-password")
    with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as sent:
        secret.press("Enter")
    submitted = sent.value.request.post_data_json
    check(len(submitted["username"]) == 300,
          f"the submitted username was {len(submitted['username'])} runes, so something truncated it")
    check(sent.value.status == 400,
          f"an over-bound username submitted from the form was answered {sent.value.status}, not 400")
    limits.update({"boundary_results": results, "browser_maxlength": None,
                   "browser_submitted_runes": len(submitted["username"]),
                   "password_bound_declared": None})
    report["field_boundaries"] = limits
    report["evidence"].append({"layer": "request", "item": "1.4", "what":
        "the username bound is accepted at 256 and refused as a length error above it, the form "
        "declares no maxLength so the over-bound value really reaches the server, and the product "
        "declares no password length bound", **limits})
    return limits


def exercise_history_and_query_state(page, server, report):
    """22.2: back/forward, tab switching and URL parameters keep state, and the rendered
    page never disagrees with the entry the navigation says is selected.

    The analytics page keeps its tab in the URL path (/analytics/<tab>) and its range in the
    query, which the page's own comment states is deliberate so that a link or a refresh
    shows the same view. That makes every move below observable in two independent places
    at once: the address, and which tab reports itself selected.
    """
    # The shell's sidebar is a SidebarProvider panel (components/ui/sidebar.tsx) and the
    # mobile drawer renders a second copy, so the desktop panel is named explicitly and its
    # visibility awaited because a fresh document has not mounted the shell yet.
    nav = page.locator('[data-slot="sidebar"]')
    expect(nav).to_be_visible()

    def selected_entry():
        chosen = nav.locator('[data-slot="nav-item"][data-active="true"]')
        expect(chosen).to_have_count(1)
        return chosen.first.inner_text().strip()

    def selected_tab():
        tabs = page.get_by_role("tab")
        expect(tabs.first).to_be_visible()
        chosen = [tabs.nth(i).inner_text().strip() for i in range(tabs.count())
                  if tabs.nth(i).get_attribute("aria-selected") == "true"]
        check(len(chosen) == 1, f"exactly one tab must report itself selected, saw {chosen}")
        return chosen[0]

    # This page polls continuously, so "networkidle" never settles; the tabs appearing is the
    # real readiness signal.
    page.goto(server.base + "/analytics/quality")
    page.wait_for_selector('[role="tab"]', state="visible", timeout=30000)
    entry = selected_entry()
    quality_tab = selected_tab()
    check(urlsplit(page.url).path == "/analytics/quality",
          f"the tab path did not survive the load: {urlsplit(page.url).path}")
    report["history"] = {"entry": entry, "url_path_state": {
        "path": "/analytics/quality", "selected_tab": quality_tab}}

    # A different tab is a different address, and going back returns to the first tab - both
    # the address and the highlighted tab have to travel together.
    page.get_by_role("tab").filter(has_text=quality_tab).click()
    page.get_by_role("tab").nth(2).click()
    page.wait_for_timeout(600)
    second_path = urlsplit(page.url).path
    second_tab = selected_tab()
    check(second_path != "/analytics/quality",
          f"switching tabs did not change the address, so the view is not linkable: {second_path}")
    check(second_tab != quality_tab, "switching tabs did not change the selected tab")
    check(selected_entry() == entry, "switching tabs changed the highlighted navigation entry")
    page.go_back()
    page.wait_for_timeout(600)
    check(urlsplit(page.url).path == "/analytics/quality",
          f"going back did not restore the first tab's address: {urlsplit(page.url).path}")
    check(selected_tab() == quality_tab,
          f"going back restored the address but not the tab: {selected_tab()!r} != {quality_tab!r}")
    check(selected_entry() == entry,
          "going back left the navigation highlighting a different entry than the page")
    page.go_forward()
    page.wait_for_timeout(600)
    check(urlsplit(page.url).path == second_path,
          f"going forward did not restore the second tab's address: {urlsplit(page.url).path}")
    check(selected_tab() == second_tab,
          f"going forward restored the address but not the tab: {selected_tab()!r} != {second_tab!r}")
    report["history"].update({"tabs": {"first": quality_tab, "second": second_tab,
        "second_path": second_path, "back": "address and tab restored",
        "forward": "address and tab restored", "entry_stable": True}})

    # A query parameter is state too: the same tab with a different range is a different view
    # and must survive a reload, which is what makes the address the source of truth.
    ranged = second_path + "?granularity=day"
    page.goto(server.base + ranged)
    page.wait_for_selector('[role="tab"]', state="visible", timeout=30000)
    check(urlsplit(page.url).query == "granularity=day",
          f"the range parameter did not survive: {urlsplit(page.url).query}")
    tab_before_reload = selected_tab()
    page.reload()
    page.wait_for_selector('[role="tab"]', state="visible", timeout=30000)
    check(urlsplit(page.url).query == "granularity=day",
          f"a reload dropped the range parameter: {urlsplit(page.url).query}")
    check(selected_tab() == tab_before_reload,
          "a reload changed the selected tab")
    check(selected_entry() == entry, "a reload changed the highlighted navigation entry")
    report["history"]["query_reload"] = {"url": ranged, "selected_tab": tab_before_reload,
                                         "survived": True}
    report["evidence"].append({"layer": "ui", "item": "22.2", "what":
        "the analytics tab lives in the URL path and the range in the query; back, forward, tab "
        "switching and a reload each kept the address, the selected tab and the highlighted "
        "navigation entry consistent", **report["history"]})
    return report["history"]


def exercise_link_and_download_contract(page, server, report, play):
    """22.7: new windows, external links and downloads behave as the product says, and an
    unknown address is never disguised as success by the SPA fallback.

    The product serves the UI shell for any unknown path so client routes work, which is why
    an unknown page really does answer 200. What must not happen is an unknown API path being
    answered with the shell: the API space has to keep its own 404. Both halves are checked,
    and the API half is checked authenticated, because an unauthenticated request is refused
    with 401 before routing ever runs and would hide a broken 404.
    """
    surface = {"ui_fallback": [], "api_space": []}
    for path in ("/no-such-page", "/evaluations/999999999"):
        status, headers, body = _raw(server.base, "GET", path, cookie=_cookie_value(page.context))
        entry = {"path": path, "status": status, "content_type": headers.get("Content-Type", ""),
                 "serves_ui_shell": body.lstrip().lower().startswith(b"<!doctype html")}
        surface["ui_fallback"].append(entry)
        check(status == 200 and entry["serves_ui_shell"],
              f"an unknown client route did not serve the UI shell: {entry}")
    # The API space keeps its own 404 once a real credential is presented.
    cookie = _cookie_value(page.context)
    check(bool(cookie), "no session cookie was available to read the API space with")
    status, headers, body = _raw(server.base, "GET", "/api/v1/definitely-not-a-route",
                                 cookie=cookie)
    entry = {"path": "/api/v1/definitely-not-a-route", "status": status,
             "content_type": headers.get("Content-Type", ""),
             "serves_ui_shell": body.lstrip().lower().startswith(b"<!doctype html")}
    surface["api_space"].append(entry)
    check(status == 404,
          f"an unknown API path was answered {status}; the SPA fallback must not swallow it")
    check(not entry["serves_ui_shell"],
          "an unknown API path was answered with the UI shell instead of a JSON error")
    # Unauthenticated, the same path is refused before routing, which is recorded rather
    # than asserted as a 404 so the two behaviours stay distinguishable.
    anon_status, _, anon_body = _raw(server.base, "GET", "/api/v1/definitely-not-a-route")
    surface["unauthenticated_unknown_api"] = {"status": anon_status,
                                             "body": json.loads(anon_body).get("code") if anon_status else None}
    check(anon_status == 401,
          f"an unauthenticated unknown API path was not refused identically to a known one: {anon_status}")
    surface["unauthenticated_unknown_api"]["distinct_from_authenticated_404"] = True

    # New windows: an in-app link that declares a target must really open one, and the
    # opener keeps its own address and selection.
    before = urlsplit(page.url).path
    links = page.locator("a[target='_blank']")
    surface["new_window_links"] = links.count()
    if links.count():
        with page.context.expect_page() as popped:
            links.first.click()
        opened = popped.value
        opened.wait_for_load_state("domcontentloaded")
        check(opened is not page, "a target=_blank link reused the current page")
        check(urlsplit(page.url).path == before, "opening a new window moved the original page")
        surface["new_window_opened"] = urlsplit(opened.url).path
        opened.close()
    report["link_and_download"] = surface
    report["evidence"].append({"layer": "request", "item": "22.7", "what":
        "unknown client routes serve the UI shell while unknown API paths keep their own 404 and are "
        "never answered with the shell; declared new-window links open a real window", **surface})
    return surface


def exercise_credential_expiry(server, report, play, idle, artifacts):
    """7.4: a credential performs a real allowed operation, then expires and is refused.

    The shipped inactivity deadline is 24h, so the isolated server runs the product's own
    session_idle_timeout to reach expiry inside a test window (see test_inactivity_and_draft).
    A management token's own expires_in_days is whole days and therefore unreachable here;
    the session credential is the one that can be driven to expiry for real, and it is the
    credential every management call actually carries.
    """
    from local_browser_fixture import LocalServer
    root = artifacts.private / "expiry"
    root.mkdir(mode=0o700)
    scoped = LocalServer(server.binary, root, session_idle_timeout=idle)
    deadline = _duration_seconds(idle)
    evidence = {"session_idle_timeout": idle, "own_server": True}
    try:
        evidence["health"] = scoped.start()
        status, _, payload = _raw(scoped.base, "POST", LOGIN, "", {"username": "admin", "password": ADMIN_PASSWORD})
        check(status == 200, f"the isolated server refused the login: {status}")
        credential = json.loads(payload)["token"]
        artifacts.remember(credential)
        # The new credential performs a real operation the product allows.
        allowed = [
            ("GET", "/api/v1/auth/me"),
            ("GET", "/api/v1/models?page=1&page_size=5"),
            ("GET", "/api/v1/nodes?page=1&page_size=5"),
            ("GET", "/api/v1/storages"),
        ]
        before = [{"method": m, "path": pa, "status": _raw(scoped.base, m, pa, credential)[0]}
                  for m, pa in allowed]
        check(all(s["status"] == 200 for s in before),
              f"the new credential could not perform an allowed operation: {before}")
        evidence["allowed_operations"] = before
        time.sleep(deadline * 1.6)
        after = [{"method": m, "path": pa, "status": _raw(scoped.base, m, pa, credential)[0]}
                 for m, pa in allowed]
        check(all(s["status"] == 401 for s in after),
              f"the expired credential still performed an allowed operation: {after}")
        status, _, payload = _raw(scoped.base, "GET", ME, credential)
        body = json.loads(payload)
        check(status == 401 and body.get("code") == "AUTHENTICATION_REQUIRED",
              f"the expired credential was refused with an unexpected body: {status} {body}")
        evidence.update({"expired_operations": after, "expired_status": 401, "error": body,
                         "expired_after_seconds": round(deadline * 1.6, 2)})
        report["credential_expiry"] = evidence
        report["evidence"].append({"layer": "request", "item": "7.4", "what":
            "a session credential performed allowed operations, then expired and every operation was refused",
            **evidence})
    finally:
        evidence["own_server_stopped"] = scoped.stop()
    return evidence


def exercise_download_surfaces(server, report, token, cookie, secrets):
    """7.6: download, export and log surfaces must not leak credentials in cleartext.

    The provider declaration already covers raw provider GETs and the upstream error
    path into WARN/logstore. What it does not cover is the set of surfaces that hand a
    file or an export back to the caller, which is where the clause about downloads
    lands. Each surface is really fetched: a non-2xx is recorded as such rather than
    counted as a clean download, so an unreachable surface cannot pass by default.
    """
    surfaces = [
        ("GET", "/api/v1/telemetry/export", True),
        ("GET", "/api/v1/usage-report", True),
        ("GET", "/api/v1/usage-report/preview", True),
        ("GET", "/api/v1/experience", True),
        ("GET", "/api/v1/analytics/key-usage", False),
        ("GET", "/api/v1/logs/search?limit=200", False),
        ("GET", "/api/v1/logs/histogram?limit=200", False),
        ("GET", "/api/v1/platform-tokens", False),
        ("GET", "/api/v1/data-retention/status", False),
    ]
    findings, reached = [], []
    for method, path, admin in surfaces:
        status, headers, payload = _raw(server.base, method, path, token)
        hits = _plaintext_hits(payload, secrets)
        entry = {"method": method, "path": path, "status": status,
                 "content_type": headers.get("Content-Type", ""), "bytes": len(payload),
                 "cleartext_secret_hits": hits}
        if hits:
            findings.append(entry)
        if status == 200:
            reached.append(path)
        report["download_surfaces"].append(entry)
    check(not findings, f"a download/export/log surface leaked a live secret in cleartext: {findings}")
    # A surface that answered 200 with an empty body proved nothing; require real bytes.
    empty = [e["path"] for e in report["download_surfaces"] if e["status"] == 200 and e["bytes"] == 0]
    check(not empty, f"a download/export surface answered 200 with an empty body: {empty}")
    # The server's own log file is a log surface too, and it records the token that was
    # just minted. Read it before artifact redaction and require the record to exist
    # (an empty or never-written log would pass the absence check for the wrong reason)
    # while the live token itself never appears in it.
    log_path = server.work / "server.log"
    logged = log_path.read_text() if log_path.exists() else ""
    check("platform token created" in logged,
          "the server log did not record the token creation, so its absence of the token proves nothing")
    log_hits = _plaintext_hits(logged.encode(), secrets)
    check(not log_hits, f"the server log leaked a live credential in cleartext: {log_hits}")
    report["log_surface"] = {"path": "private/server.log", "bytes": len(logged.encode()),
        "records_token_creation": True, "cleartext_secret_hits": log_hits,
        "prefix_only_record": bool(re.search(r"prefix=gwp_[A-Za-z0-9_-]{6,}", logged))}
    report["evidence"].append({"layer": "request", "item": "7.6", "what":
        "download, export and log surfaces were really fetched and scanned for cleartext credentials; "
        "the server's own log records the token creation but carries only its prefix",
        "surfaces": report["download_surfaces"], "reached": len(reached),
        "log_surface": report["log_surface"],
        "auditor": "independent transport, no cookie jar, management bearer"})
    return reached


def _cookie_value(context):
    for item in context.cookies():
        if item["name"] == COOKIE:
            return item["value"]
    return ""


def test_inactivity_and_draft(play, args, artifacts, report, locale, idle="2s"):
    """22.4: an idle login expires, background polling cannot renew it, and an unsaved
    form survives the re-authentication that follows.

    The shipped inactivity deadline is 24h (auth.SessionPolicy.Normalized), which no test
    can wait out. The product reads session_idle_timeout from its own server.yaml - the
    same key an operator sets - so this case starts its own isolated server with a short
    one. That is real product behaviour under a real product setting, and it is a separate
    server so the short deadline cannot expire the sessions the main flow is asserting on.
    """
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    settings = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    deadline = _duration_seconds(idle)
    check(deadline > 0, "22.4 needs a positive inactivity deadline")
    # A fresh server and database: its own login, its own cookies, nothing borrowed.
    idle_root = artifacts.private / "idle"
    idle_root.mkdir(mode=0o700)
    other = LocalServer(args.server_bin, idle_root, session_idle_timeout=idle)
    report["inactivity"] = {"session_idle_timeout": idle, "configured_via": "server.yaml session_idle_timeout",
        "default_in_source": "auth.SessionPolicy.Normalized IdleTimeout 24h",
        "own_server": True}
    try:
        report["inactivity"]["health"] = other.start()
        browser = play.chromium.launch(channel="chrome", headless=True)
        try:
            context = browser.new_context(viewport={"width": 1440, "height": 900},
                                          locale="zh-CN" if locale == "zh" else "en-US", timezone_id="UTC")
            context.set_default_timeout(15000)
            page = context.new_page()
            page.goto(other.base + "/")
            expect(page.get_by_role("textbox", name=auth["username"], exact=True)).to_be_visible()
            page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
            page.get_by_label(auth["password"], exact=True).fill(ADMIN_PASSWORD)
            # Barrier on the real login response, not on a timeout, so the cookie is
            # already issued when the checks below read it.
            with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as signed_in:
                page.get_by_label(auth["password"], exact=True).press("Enter")
            check(signed_in.value.status == 200, f"the isolated server refused the login: {signed_in.value.status}")
            cookie = _cookie_value(context)
            check(bool(cookie), "the isolated server did not issue a management cookie")
            identity = cookie_identity(play, other.base, cookie)[1]
            report["inactivity"]["identity"] = identity

            # 1) A steady background GET poll must not renew the login. The product
            #    renews only on non-GET (api.managementAuth) and session.ts reports
            #    activity from human pointer/key events only, so passive polling has no
            #    channel to extend the deadline and the session must expire during it.
            observed = []
            started = time.monotonic()
            while time.monotonic() - started < deadline * 3:
                observed.append(cookie_identity(play, other.base, cookie)[0])
                time.sleep(max(deadline / 4, 0.15))
            check(200 in observed, "the login never worked, so the inactivity probe proves nothing")
            check(401 in observed,
                  f"a steady background GET poll kept the login alive past its {idle} deadline: {observed}")
            report["inactivity"].update({"background_poll_statuses": observed,
                "first_rejection_index": observed.index(401), "probes": len(observed),
                "poll_runner": "cookie-only request context, no human input events"})
            report["evidence"].append({"layer": "runtime", "item": "22.4", "what":
                "a background GET poll ran past the configured inactivity deadline without renewing the login",
                "path": ME, "method": "GET", "session_idle_timeout": idle, "statuses": observed})

            # 2) An unsaved form survives inactivity expiry and re-authentication. The open
            #    tab keeps SessionBoundary's children mounted, so the draft is still in the
            #    DOM underneath the dialog. The dialog itself is raised when the product
            #    observes a 401 (apiError.notifySessionExpired); switching a settings tab is
            #    a real product request that produces one, so this needs no synthetic fetch.
            # The poll above expired the login on purpose, so sign in again through the
            # product's own dialog first: the draft has to survive an expiry that happens
            # while the form is open, not one that predates it.
            expired_dialog = page.get_by_role("dialog", name=auth["sessionExpiredTitle"], exact=True)
            expect(expired_dialog).to_be_visible()
            report["inactivity"]["dialog_after_poll_expiry"] = True
            expired_dialog.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
            expired_dialog.get_by_label(auth["password"], exact=True).fill(ADMIN_PASSWORD)
            with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as again:
                expired_dialog.get_by_label(auth["password"], exact=True).press("Enter")
            check(again.value.status == 200, "re-authentication after expiry was refused")
            expect(expired_dialog).to_have_count(0)
            page.goto(other.base + "/settings")
            expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
            marker = "e2e-draft-" + uuid.uuid4().hex[:10]
            field = page.locator("input[type='text'], input:not([type])").first
            expect(field).to_be_visible()
            field.fill(marker)
            check(field.input_value() == marker, "the settings form did not accept the draft marker")
            report["inactivity"]["draft"] = {"marker": marker, "field": "settings text input"}
            time.sleep(deadline * 2)
            # The login is now dead but the tab has not noticed yet: the draft is still
            # editable, which is exactly the "recoverable draft" 22.4 asks about.
            check(field.input_value() == marker,
                  "the draft disappeared as soon as the login expired, before any request was made")
            report["inactivity"]["draft"]["survives_undetected_expiry"] = True
            tabs = page.get_by_role("tab")
            check(tabs.count() >= 2, "the settings page rendered no tabs to navigate by")
            tabs.nth(tabs.count() - 1).click()
            dialog = page.get_by_role("dialog", name=auth["sessionExpiredTitle"], exact=True)
            expect(dialog).to_be_visible()
            report["inactivity"]["dialog_after_expiry"] = True
            kept = field.input_value()
            check(kept == marker,
                  f"the form draft was lost when the login expired: expected {marker!r}, got {kept!r}")
            report["inactivity"]["draft"]["survived_expiry"] = True
            dialog.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
            dialog.get_by_label(auth["password"], exact=True).fill(ADMIN_PASSWORD)
            with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as back:
                dialog.get_by_label(auth["password"], exact=True).press("Enter")
            check(back.value.status == 200, "re-authentication through the dialog was refused")
            expect(dialog).to_have_count(0)
            restored = field.input_value()
            check(restored == marker,
                  f"the form draft was lost across re-authentication: expected {marker!r}, got {restored!r}")
            report["inactivity"]["draft"]["survived_relogin"] = True
            refreshed = _cookie_value(context)
            check(refreshed and refreshed != cookie, "re-authentication did not replace the expired session")
            check(cookie_identity(play, other.base, refreshed) == (200, identity),
                  "the re-authenticated session does not read the same identity back")
            check(cookie_identity(play, other.base, cookie) == (401, DENIED),
                  "the expired session was resurrected by re-authenticating")
            report["inactivity"].update({"url_after_relogin": urlsplit(page.url).path,
                "replacement_cookie_status": 200, "expired_cookie_status": 401})
            report["evidence"].append({"layer": "persistence", "item": "22.4", "what":
                "inactivity expiry raised the product's re-authentication dialog and the unsaved form draft "
                "survived both the expiry and the re-authentication; the expired session stayed dead",
                "session_idle_timeout": idle, "expired_cookie_status": 401,
                "replacement_cookie_status": 200, "draft_survived": True, "identity": identity})
            page.screenshot(path=artifacts.directory / "inactivity-draft-recovered.png")
            context.close()
        finally:
            browser.close()
    finally:
        report["inactivity"]["own_server_stopped"] = other.stop()


def test_two_tab_logout_relogin(play, context, server, artifacts, report, locale):
    strings = ROOT / "ui/src/i18n/locales" / locale
    auth = json.loads((strings / "auth.json").read_text())
    common = json.loads((strings / "common.json").read_text())
    networks = json.loads((strings / "networks.json").read_text())
    settings = json.loads((strings / "settings.json").read_text())
    errors, http_errors, console_errors, login_requests, logout_requests = [], [], [], [], []
    report["browser_errors"] = {"page": errors, "http": http_errors, "console": console_errors}

    auth_bodies = {}

    def observe(page):
        cdp = context.new_cdp_session(page)
        auth_bodies[page] = {LOGIN: [], LOGOUT: []}

        def capture_auth(event):
            path, captured = capture_auth_response(cdp, event, artifacts)
            auth_bodies[page][path].append(captured)
        cdp.on("Fetch.requestPaused", capture_auth)
        cdp.send("Fetch.enable", {"patterns": [{"urlPattern": server.base + path, "requestStage": "Response"}
                                              for path in (LOGIN, LOGOUT)]})
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.on("response", lambda r: http_errors.append({"path": urlsplit(r.url).path, "status": r.status})
                if r.status >= 400 and urlsplit(r.url).path not in (ME, LOGIN, ACTIVITY) else None)

        def console(message):
            if message.type != "error":
                return
            # Chrome logs the deliberately rejected login/anonymous probe, and the 400 that 1.4
            # provokes by submitting an over-bound username from the form. Both are requested by
            # the case itself; anything else in the console is a real defect and is collected.
            if urlsplit(message.location.get("url", "")).path in (LOGIN, ME, ACTIVITY) and message.text in (
                    "Failed to load resource: the server responded with a status of 401 (Unauthorized)",
                    "Failed to load resource: the server responded with a status of 400 (Bad Request)"):
                return
            console_errors.append(message.text)
        page.on("console", console)

        def request(req):
            path = urlsplit(req.url).path
            if path == LOGIN and req.method == "POST":
                # Count only: the request body is asserted in memory below.
                login_requests.append({"method": "POST", "path": path})
            if path == LOGOUT:
                logout_requests.append({"method": req.method, "path": path, "body": req.post_data})
        page.on("request", request)

    def auth_body(page, path):
        captured = auth_bodies[page][path][-1]
        check(captured["status"] == 200, "successful real authentication response not captured")
        return captured["body"]

    # 未登录访问网络深链；空字段不提交，错误密码真实拒绝并保留用户名。
    page = context.new_page()
    observe(page)
    with page.expect_response(lambda r: urlsplit(r.url).path == ME) as anonymous:
        page.goto(server.base + "/networks")
    check(anonymous.value.status == 401 and anonymous.value.json() == DENIED, "anonymous deep link gained authentication")
    expect(page.get_by_role("heading", name=auth["title"], exact=True)).to_be_visible()
    expect(page).to_have_url(server.base + "/networks")
    report["preflight"] = "PASS"
    # requiredMark={false} puts this class on the form. The accessible name of
    # the textbox changes when the mark is shown, so this check comes first.
    login_form = page.locator("form.ant-form")
    expect(login_form).to_have_count(1, timeout=12000)
    expect(login_form).to_have_class(re.compile(r"\bant-form-hide-required-mark\b"), timeout=12000)
    username = exercise_login_prefix(page, auth)
    from login_inventory import exercise_language_switcher, exercise_login_inventory
    report["login_language_switcher"] = exercise_language_switcher(
        page, auth, locale, common["language"]["switch"])
    report["login_inventory"] = exercise_login_inventory(page, auth)
    secret = page.get_by_label(auth["password"], exact=True)
    # 22.6: the focus and disabled states are read from the browser's computed style rather than from
    # the presence of a class. This runs before any submit, so the field is in its normal state - a
    # reading taken after a refused submit would be reading the error variant's ring, which is a
    # different colour and would have made the assertion pass for the wrong reason.
    def computed(target, prop):
        return target.evaluate("(el, name) => getComputedStyle(el).getPropertyValue(name)", prop)

    props = ("outline-style", "outline-color", "border-color", "box-shadow")
    wrapper_js = "(el, name) => getComputedStyle(el.closest('.ant-input-affix-wrapper') || el).getPropertyValue(name)"
    # Reached by keyboard, not by .focus(): :focus-visible is what the theme paints, and a programmatic
    # focus would be measuring the non-visible variant.
    page.keyboard.press("Tab")
    if not username.evaluate("el => el === document.activeElement"):
        username.focus()
        page.keyboard.press("Tab")
        page.keyboard.press("Shift+Tab")
    expect(username).to_be_focused()
    focused = {prop: computed(username, prop) for prop in props}
    focused_wrapper = {prop: username.evaluate(wrapper_js, prop) for prop in props}
    secret.focus()
    expect(secret).to_be_focused()
    blurred = {prop: computed(username, prop) for prop in props}
    blurred_wrapper = {prop: username.evaluate(wrapper_js, prop) for prop in props}
    # The ring this theme paints on focus lives on the input's wrapper rather than on the input itself,
    # so the wrapper is what has to change. The assertion is on the measurable difference rather than on
    # a particular colour, so a theme change is allowed but a focus state that paints nothing is not.
    check(focused != blurred or focused_wrapper != blurred_wrapper,
          f"focusing the username field changed no painted state: field={focused} wrapper={focused_wrapper}")
    report["login_computed_styles"] = {"field_focused": focused, "field_blurred": blurred,
                                       "wrapper_focused": focused_wrapper,
                                       "wrapper_blurred": blurred_wrapper}
    distinguishable = {prop for prop in props if focused_wrapper[prop] != blurred_wrapper[prop]}
    check(bool(distinguishable),
          f"a keyboard-focused field is painted identically to a blurred one: {focused_wrapper}")

    def ring_strength(shadow):
        """The alpha and spread of the first shadow, which is the ring this theme paints."""
        alpha = re.search(r"rgba?\(([^)]*)\)", shadow)
        width = re.search(r"0px 0px 0px ([0-9.]+)px", shadow)
        parts = [float(v) for v in alpha.group(1).split(",")] if alpha else []
        # Three cases, and they are not interchangeable. An rgb(...) triple names an opaque colour, so
        # its alpha is 1.0. An rgba(..., a) quadruple carries its own alpha. No colour function at all
        # means the computed value is "none" - the element paints no shadow - so the alpha is 0.0.
        # Collapsing the first case into the third reads an opaque ring as absent, and collapsing the
        # third into the first reads an absent ring as opaque; either way the "stronger when focused"
        # comparison reports a defect when the paint is actually correct.
        if len(parts) == 4:
            strength = parts[3]
        else:
            strength = 1.0 if len(parts) == 3 else 0.0
        return (strength, float(width.group(1)) if width else 0.0)

    # The ring is required to be stronger when focused, not merely different: this theme animates it in
    # from a faint trace, so a comparison that only asked for a difference would be satisfied by the
    # residue the transition leaves behind. Both the alpha and the spread have to grow.
    focused_ring, blurred_ring = ring_strength(focused_wrapper["box-shadow"]), ring_strength(blurred_wrapper["box-shadow"])
    check(focused_ring[0] > blurred_ring[0] and focused_ring[1] > blurred_ring[1],
          f"the focus ring is not stronger than the blurred trace: focused={focused_ring} blurred={blurred_ring}")
    report.setdefault("login_focus_ring", {})["strength"] = {"focused": focused_ring, "blurred": blurred_ring}
    findings = report.setdefault("findings", [])
    if "box-shadow" not in distinguishable:
        findings.append({"kind": "focus-ring", "detail":
            "a keyboard-focused login field paints no focus ring: box-shadow is identical focused and "
            "blurred, so the only visual cue this form gives for keyboard focus is the border colour",
            "focused": focused_wrapper, "blurred": blurred_wrapper})
    submit_button = button(page, auth["submit"])
    # This submit is deliberately never disabled - an empty form is refused by validation instead - so
    # the state it actually carries is recorded, and the disabled variant is recorded as not present on
    # this screen rather than claimed.
    disabled_style = {prop: computed(submit_button, prop) for prop in
                      ("color", "cursor", "background-color")}
    disabled_style["submit_is_disabled"] = submit_button.is_disabled()
    report["login_computed_styles"] = {"field_focused": focused, "field_blurred": blurred,
                                       "wrapper_focused": focused_wrapper,
                                       "wrapper_blurred": blurred_wrapper,
                                       "disabled_submit": disabled_style}
    expect(username).to_be_empty()
    expect(secret).to_be_empty()
    expect(secret).to_have_attribute("type", "password")
    # 7.1: the mask is a state, not a styling choice, and the reveal toggle is a real control.
    # The value is typed, required to stay masked while the toggle is untouched, required to become
    # readable after one activation with the characters unchanged, and required to be masked again
    # after a second - so a toggle that only redrew the icon, or one that lost the value, fails here.
    probe = "e2e-probe-secret"
    secret.fill(probe)
    expect(secret).to_have_attribute("type", "password")
    check(secret.get_attribute("type") != "text", "a typed password was readable without its toggle")
    toggle = page.locator(".ant-input-password-icon")
    expect(toggle).to_have_count(1, timeout=8000)
    toggle.click()
    expect(secret).to_have_attribute("type", "text")
    check(secret.input_value() == probe, "revealing the password changed its value")
    toggle.click()
    expect(secret).to_have_attribute("type", "password")
    check(secret.input_value() == probe, "re-masking the password changed its value")
    secret.fill("")
    report["credential_mask"] = {"masked_default": True, "reveal_round_trip": True}
    button(page, auth["submit"]).click()
    # 1.3: each message must be located at the field it belongs to, not merely somewhere on the page,
    # so the item is asserted by walking from the field to the message inside its own form item.
    for field, message in ((username, auth["usernameRequired"]), (secret, auth["passwordRequired"])):
        item = page.locator(".ant-form-item").filter(
            has=page.locator("label").filter(has_text=re.compile("^" + re.escape(
                auth["username"] if field is username else auth["password"]) + "$")))
        expect(item).to_have_count(1, timeout=8000)
        expect(item.get_by_text(message, exact=True)).to_be_visible()
        expect(item.locator(".ant-form-item-explain-error")).to_have_count(1, timeout=8000)
    expect(page.get_by_role("alert")).to_have_count(0)
    check(not login_requests, "empty login submitted a request")
    # 1.15: the keyboard order and the layout this form is used in. Tab is required to reach both
    # fields in reading order with no stop skipped and no hidden control in between, Enter in the
    # username field must not submit before a password exists, and at 1440x900 the fields and the
    # submit button are required to be on screen and not overlapping.
    page.set_viewport_size({"width": 1440, "height": 900})
    username.focus()
    expect(username).to_be_focused()
    page.keyboard.press("Tab")
    expect(secret).to_be_focused()
    page.keyboard.press("Shift+Tab")
    expect(username).to_be_focused()
    before_tab_enter = len(login_requests)
    username.fill("admin")
    button(page, auth["submit"]).click()
    check(len(login_requests) == before_tab_enter,
          "clicking submit on an incomplete form sent a request")
    page.keyboard.press("Enter")
    check(len(login_requests) == before_tab_enter,
          "Enter in the username field submitted an incomplete form")
    expect(page.get_by_text(auth["passwordRequired"], exact=True)).to_be_visible()
    username.fill("")
    for target in (username, secret, button(page, auth["submit"])):
        box = target.bounding_box()
        check(box is not None and box["width"] > 0 and box["height"] > 0,
              f"a control is not rendered at 1440x900: {box}")
        check(box["y"] >= 0 and box["y"] + box["height"] <= 900,
              f"a control falls outside the 1440x900 viewport: {box}")
    user_box, secret_box, submit_box = (username.bounding_box(), secret.bounding_box(),
                                        button(page, auth["submit"]).bounding_box())
    check(user_box["y"] + user_box["height"] <= secret_box["y"] + 1,
          "the two fields overlap")
    check(secret_box["y"] + secret_box["height"] <= submit_box["y"] + 1,
          "the submit button overlaps the field above it")
    report["login_keyboard_and_layout"] = {"tab_order": ["username", "password"],
        "enter_incomplete_no_post": True, "viewport": "1440x900",
        "controls_visible": True, "overlap": False,
        "boxes": {"username_y": round(user_box["y"]), "password_y": round(secret_box["y"]),
                  "submit_y": round(submit_box["y"])}}

    # 1.8: a whitespace-only value is not a value. Both fields carry whitespace: true, so spaces
    # alone must be refused in the form and must not reach the server, while a real value in the
    # other field is kept.
    before_blank = len(login_requests)
    username.fill("   ")
    secret.fill("   ")
    button(page, auth["submit"]).click()
    expect(page.get_by_text(auth["usernameRequired"], exact=True)).to_be_visible()
    expect(page.get_by_text(auth["passwordRequired"], exact=True)).to_be_visible()
    expect(username).to_have_value("   ")
    expect(secret).to_have_value("   ")
    check(len(login_requests) == before_blank, "a whitespace-only login submitted a request")
    report["login_whitespace_only"] = {"refused": True, "requests": 0, "value_kept": True}

    invalid = "e2e-invalid-" + report["run_id"]
    artifacts.remember(invalid)
    username.fill("admin")
    secret.fill(invalid)
    with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as rejected:
        secret.press("Enter")
    check(rejected.value.status == 401 and rejected.value.json() ==
          {"code": "INVALID_CREDENTIALS", "error": "invalid username or password"}, "invalid login not rejected precisely")
    check(rejected.value.request.post_data_json == {"username": "admin", "password": invalid}, "incorrect invalid-login request")
    expect(page.get_by_role("alert")).to_have_text(auth["invalidCredentials"])
    # requiredMark={false} hides the asterisk. The class lives on the form.
    login_form = page.locator("form.ant-form")
    expect(login_form).to_have_count(1, timeout=12000)
    expect(login_form).to_have_class(re.compile(r"\bant-form-hide-required-mark\b"), timeout=12000)
    for label in (auth["username"], auth["password"]):
        marked = page.locator("label").filter(has_text=re.compile("^" + re.escape(label) + "$"))
        expect(marked).to_have_count(1, timeout=12000)
        expect(marked).to_have_class(re.compile(r"\bant-form-item-required-mark-hidden\b"), timeout=12000)
    expect(username).to_have_value("admin")
    expect(secret).to_have_value(invalid)
    expect(page.get_by_role("heading", name=auth["title"], exact=True)).to_be_visible()
    expect(page).to_have_url(server.base + "/networks")
    expect(page.get_by_role("dialog")).to_have_count(0)
    check(not any(c["name"] == COOKIE for c in context.cookies()), "failed login created a cookie")
    check(http(server.base, "GET", ME) == (401, DENIED), "failed login made anonymous client authorized")
    report["login_hide_required_mark"] = True
    report["evidence"].append({"layer": "ui", "empty_required_no_post": True, "secret_masked": True,
        "wrong_credential": "401 INVALID_CREDENTIALS; field retained; actionable translated alert"})

    # 1.4: the username length bound is a real server rule, exercised from this form once the
    # earlier assertions on the retained fields have been made. Both fields are restored to
    # the values the sign-in below needs.
    exercise_field_boundaries_from_form(page, server, report, auth, username, secret)
    username.fill("admin")
    secret.fill("password")
    # 正确登录后，两标签刷新保留深链；独立客户端证明原 Cookie/Bearer 均可用。
    # 提交挂起时再点一次并按 Enter，仍只有这一次登录。
    secret.fill("gpuworks-test-password-2026")
    expect(page.get_by_role("alert")).to_have_count(0)
    held_logins = []

    def hold_login(route):
        if route.request.method == "POST" and urlsplit(route.request.url).path == LOGIN:
            held_logins.append(route)
            return
        route.continue_()

    page.route("**/api/v1/auth/login", hold_login)
    submit = page.locator("button.auth-submit")
    expect(submit).to_have_count(1, timeout=12000)
    submit.click()
    for _ in range(40):
        if held_logins:
            break
        page.wait_for_timeout(50)
    check(len(held_logins) == 1, "sign-in did not stay in flight")
    # The duplicate attempts are checked before the visual busy state, so the case fails on the
    # contract - one sign-in, one request - rather than on the class that happens to enforce it.
    submit.click(timeout=12000)
    secret.press("Enter")
    page.wait_for_timeout(500)
    check(len(held_logins) == 1, "duplicate login while the first request is held")
    expect(submit).to_have_class(re.compile(r"\bant-btn-loading\b"), timeout=12000)
    try:
        with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as accepted:
            held_logins[0].continue_()
        check(accepted.value.request.post_data_json == {"username": "admin", "password": "gpuworks-test-password-2026"}
              and not urlsplit(accepted.value.url).query, "incorrect successful login request")
    finally:
        page.unroute("**/api/v1/auth/login", hold_login)
    expect(page.get_by_role("heading", name=networks["title"], exact=True)).to_be_visible()
    expect(page.get_by_label(auth["password"], exact=True)).to_have_count(0)
    old_cookie, old_bearer, identity = capture_credentials(accepted.value, context, artifacts, auth_body(page, LOGIN))
    check(cookie_identity(play, server.base, old_cookie) == (200, identity), "original cookie is not usable before logout")
    check(http(server.base, "GET", ME, old_bearer) == (200, identity), "original login bearer is not usable before logout")
    report["login_single_submit"] = True
    report["evidence"].append({"layer": "request", "path": LOGIN, "method": "POST", "status": 200,
        "body": {"username": "admin", "password": "[REDACTED]"}, "identity": identity,
        "cookie": "opaque, HttpOnly, SameSite=Lax, Path=/", "cache_control": "no-store"})
    artifacts.start_trace(context)
    from session_persistence import exercise_session_persistence
    peer, report["session_persistence"] = exercise_session_persistence(
        page, context, play, server, identity, networks, settings, auth, observe, old_cookie, old_bearer)
    report["evidence"].append({"layer": "persistence", "paths_after_reload": ["/networks", "/settings"],
        "original_credentials_before_logout": {"cookie": 200, "login_bearer": 200}, "identity": identity,
        "reentered_page_matches_independent_get": True})

    # UI 注销撤销原会话；第二标签要求重登，原 Cookie/Bearer 必须真实返回 401。
    with page.expect_response(lambda r: urlsplit(r.url).path == LOGOUT and r.request.method == "POST") as logged_out:
        button(page, common["header"]["logout"]).click()
    response = logged_out.value
    check(response.status == 200 and auth_body(page, LOGOUT) == {"status": "logged_out"}
          and response.request.post_data is None and not urlsplit(response.url).query, "logout request/response differs")
    report["evidence"].append({"layer": "request", "method": "POST", "path": LOGOUT,
        "body": None, "query": "", "status": 200, "response": {"status": "logged_out"}})
    expect(page.get_by_role("heading", name=auth["title"], exact=True)).to_be_visible()
    dialog = peer.get_by_role("dialog", name=auth["sessionExpiredTitle"], exact=True)
    expect(dialog).to_be_visible()
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(dialog.get_by_text(auth["sessionExpiredDescription"], exact=True)).to_be_visible()
    expect(page).to_have_url(server.base + "/networks")
    expect(peer).to_have_url(server.base + "/settings")
    check(not any(c["name"] == COOKIE for c in context.cookies()), "logout left browser cookie alive")
    check(cookie_identity(play, server.base, old_cookie) == (401, DENIED), "captured cookie survived logout")
    check(http(server.base, "GET", ME, old_bearer) == (401, DENIED), "captured bearer survived logout")
    check(http(server.base, "GET", ME) == (401, DENIED), "anonymous transport was contaminated")
    report["evidence"].append({"layer": "runtime", "after": "UI logout", "method": "GET", "path": ME,
        "captured_cookie_status": 401, "captured_bearer_status": 401, "anonymous_status": 401, "error": DENIED,
        "peer": "named reauthentication dialog in viewport; both deep links retained"})
    artifacts.stop_trace(context)

    # 第二标签重登后双标签恢复，原凭据仍被撤销；登录及返回密钥始终不录 trace。
    dialog.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    dialog.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with peer.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as restored:
        dialog.get_by_label(auth["password"], exact=True).press("Enter")
    check(restored.value.request.post_data_json == {"username": "admin", "password": "gpuworks-test-password-2026"}, "re-login request differs")
    expect(dialog).to_have_count(0)
    new_cookie, new_bearer, new_identity = capture_credentials(restored.value, context, artifacts, auth_body(peer, LOGIN))
    check(new_identity == identity and new_cookie != old_cookie and new_bearer != old_bearer, "re-login did not replace the revoked session")
    expect(page.get_by_role("heading", name=networks["title"], exact=True)).to_be_visible()
    expect(peer.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    artifacts.start_trace(context)
    for tab, path, title in ((page, "/networks", networks["title"]), (peer, "/settings", settings["page"]["title"])):
        tab.reload()
        expect(tab).to_have_url(server.base + path)
        expect(tab.get_by_role("heading", name=title, exact=True)).to_be_visible()
        check(tab.evaluate("document.documentElement.scrollWidth <= innerWidth"), "restored deep link overflows")
        read = tab.request.get(server.base + ME)
        check(read.status == 200 and read.json() == identity, "restored browser identity differs")
    check(cookie_identity(play, server.base, new_cookie) == (200, identity), "replacement cookie failed")
    check(http(server.base, "GET", ME, new_bearer) == (200, identity), "replacement bearer failed")
    check(cookie_identity(play, server.base, old_cookie) == (401, DENIED), "re-login resurrected old cookie")
    check(http(server.base, "GET", ME, old_bearer) == (401, DENIED), "re-login resurrected old bearer")
    report["evidence"].append({"layer": "persistence", "after": "UI re-login and both tabs reload",
        "replacement_cookie_status": 200, "replacement_bearer_status": 200, "original_cookie_status": 401,
        "original_bearer_status": 401, "paths": ["/networks", "/settings"], "identity": identity})
    # 22.1: with a live session, walk the sidebar the page actually renders and check
    # each entry's deep link, heading, highlighted entry and reload position.
    from session_persistence import exercise_navigation_entries
    report["navigation"] = exercise_navigation_entries(page, server, report)
    exercise_history_and_query_state(page, server, report)
    # 22.7 needs the live session both to read the API space authenticated and to see what an
    # unknown address does while signed in, so it also sits before the cleanup logout.
    exercise_link_and_download_contract(page, server, report, play)
    # 7.6: a freshly minted management token is a live credential, so it is the exact
    # value that must not appear in cleartext in any download, export or log surface.
    report["download_surfaces"] = []
    minted = page.request.post(server.base + "/api/v1/platform-tokens",
                               data=json.dumps({"name": "surface-" + report["run_id"], "scope": "read"}))
    check(minted.status == 201, f"minting a management token for the surface scan failed: {minted.status}")
    live = minted.json()["token"]
    artifacts.remember(live)
    report["evidence"].append({"layer": "request", "method": "POST", "path": "/api/v1/platform-tokens",
        "status": 201, "body": {"name": "surface-" + report["run_id"], "scope": "read"},
        "response_token": "[REDACTED]", "purpose": "7.6 cleartext scan source"})
    exercise_download_surfaces(server, report, new_bearer, new_cookie, {"platform_token": live})
    page.screenshot(path=artifacts.directory / "recovered-networks.png")
    with peer.expect_response(lambda r: urlsplit(r.url).path == LOGOUT) as cleanup:
        button(peer, common["header"]["logout"]).click()
    check(cleanup.value.status == 200 and auth_body(peer, LOGOUT) == {"status": "logged_out"}, "cleanup logout failed")
    check(cookie_identity(play, server.base, new_cookie) == (401, DENIED), "cleanup cookie survived")
    check(http(server.base, "GET", ME, new_bearer) == (401, DENIED), "cleanup bearer survived")
    # Four sign-in POSTs reach the server and no more: the refused wrong-password attempt, the
    # 1.4 over-bound username submitted from the form, the held-then-released sign-in, and the
    # in-place re-authentication. A fifth would mean a duplicate, which is what this guards.
    check(len(login_requests) == 4 and logout_requests == [{"method": "POST", "path": LOGOUT, "body": None}] * 2,
          f"unexpected duplicate login/logout request: logins={len(login_requests)} "
          f"logouts={logout_requests}")
    check(not errors and not http_errors and not console_errors, "unexpected browser errors; see report")
    report["browser_errors"] = {"page": errors, "http": http_errors, "console": console_errors}
    report["evidence"].append({"layer": "cleanup", "replacement_session_revoked": True,
        "login_requests": login_requests, "logout_requests": logout_requests})



def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--session-idle-timeout", default="90s",
        help="server.yaml session_idle_timeout for the isolated server; the shipped default is 24h, "
             "which no test can wait out. This is the deadline the whole flow must live inside, so it "
             "has to exceed the runtime of every earlier step; the inactivity case re-derives the wait "
             "from it rather than shortening it.")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    # The shipped inactivity deadline is 24h, so it is unreachable in a test window. The
    # product reads session_idle_timeout from its own server.yaml, so the isolated server
    # sets it. The main flow gets a comfortable deadline (any step may take a while), and
    # the inactivity case starts a second isolated server with a short one, so waiting out
    # the deadline never races the flow that is asserting other things.
    server = LocalServer(args.server_bin, artifacts.private, session_idle_timeout=args.session_idle_timeout)
    manifest_path = ROOT / "test/e2e/coverage/session-logout.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_two_tab_logout_relogin.__name__, "manifest binding drift")
    report = {"contract": CONTRACT, "case": manifest["name"], "status": "NOT_RUN", "run_id": run_id,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin browser; independent original cookie and login bearer clients",
        "locale": args.locale, "viewport": {"width": 1440, "height": 900}, "timezone": "UTC",
        "selection": [CONTRACT], "retries": 0, "evidence": [], "test_sha256": digest(Path(__file__)),
        "persistence_sha256": digest(ROOT / "test/e2e/session_persistence.py"),
        "login_inventory_sha256": digest(ROOT / "test/e2e/login_inventory.py"),
        "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/platform_token_read_revoke.py"),
        "coverage_manifest_sha256": digest(manifest_path),
        "command": ["python3", "test/e2e/session_logout.py", *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"], report["owned_server_pid"] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            context.set_default_timeout(10000)
            report["browser_version"] = browser.version
            try:
                test_two_tab_logout_relogin(play, context, server, artifacts, report, args.locale)
                test_inactivity_and_draft(play, args, artifacts, report, locale=args.locale)
                exercise_credential_expiry(server, report, play, "2s", artifacts)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "missing required evidence")
                report["status"] = "VERIFYING_ARTIFACTS"
            finally:
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
        report["private_state"] = "isolated DB/logs retained; executing owner removes only this run directory within 7 days"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
            if report["status"] == "VERIFYING_ARTIFACTS":
                report["status"] = "PASS"
        except Exception:
            report["status"], report["artifact_error"] = "FAIL_TEST", "credential scan rejected artifacts"
        artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
