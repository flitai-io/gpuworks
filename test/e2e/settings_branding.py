#!/usr/bin/env python3
"""Settings / branding through the real browser, server and sign-in page.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-settings-branding
After build: python3 test/e2e/settings_branding.py --locale en
Requires Python Playwright and system Chrome. No agent, GPU or container is
needed: branding is a control-plane setting rendered by the console and the
public sign-in page.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and retains the
private database/logs for scoped diagnosis (seven days).
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
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check
from settings_branding_fixture import watch_public_page
from settings_branding_inventory import exercise_branding_conflict, exercise_branding_field_inventory

CONTRACT = "gpuworks/settings/branding"
CASE = "设置 / 品牌外观 / 文字与图标保存、刷新回读、登录页与浏览器标题生效"
BRANDING = "/api/v1/branding"


def strings(locale):
    settings = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    return settings, auth, common


def branding_doc(response, label):
    check(response.status == 200, f"{label} branding HTTP {response.status}")
    return response.json()


def _box(locator):
    return locator.evaluate("""element => {
        const rect = element.getBoundingClientRect();
        return {left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom,
                width: rect.width, height: rect.height};
    }""")


def _separated(first, second):
    return (first["right"] <= second["left"] or second["right"] <= first["left"]
            or first["bottom"] <= second["top"] or second["bottom"] <= first["top"])


def exercise_branding_name_keyboard(page, card, branding, product_input, title_input,
                                    description_input, save_button, patches):
    """Product-name count, Tab through the three fields, Enter sends no PATCH."""
    item = card.locator(".ant-form-item").filter(has_text=branding["productName"]).first
    expect(item.locator(".ant-input-show-count-suffix")).to_have_text("0 / 32", timeout=12000)
    card.scroll_into_view_if_needed()
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    for locator in (product_input, title_input, description_input, save_button):
        expect(locator).to_be_in_viewport(ratio=1)
    product_box, save_box = _box(product_input), _box(save_button)
    check(product_box["width"] > 0 and save_box["width"] > 0, "product name or Save has no box")
    check(_separated(product_box, save_box), "product name overlaps Save")
    upload = card.get_by_role(
        "button", name=re.compile(re.escape(branding["uploadLogo"])))
    upload.focus()
    expect(upload).to_be_focused()
    upload.press("Tab")
    expect(product_input).to_be_focused()
    product_input.press("Tab")
    expect(title_input).to_be_focused()
    title_input.press("Tab")
    expect(description_input).to_be_focused()
    save_button.focus()
    expect(save_button).to_be_focused()
    product_input.focus()
    expect(product_input).to_be_focused()
    before = len(patches)
    submitted = False
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path == BRANDING
                                  and request.method == "PATCH", timeout=1000):
            product_input.press("Enter")
        submitted = True
    except PlaywrightTimeout:
        submitted = False
    check(not submitted, "Enter in the product name submitted branding")
    expect(product_input).to_have_value("")
    check(len(patches) == before, "Enter recorded a branding PATCH")
    return {"count": "0 / 32",
            "tab": ["upload", "product name", "header title", "sign-in text"],
            "save_focused": True, "enter_submitted": False, "viewport": view,
            "overlaps_save": False}


def _svg(fill):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32">'
            f'<rect width="32" height="32" fill="{fill}"/></svg>').encode()


def _logo_url(raw):
    return "data:image/svg+xml;base64," + base64.b64encode(raw).decode()


def exercise_branding_single_field(page, card, branding, field_input, click_save, server_base):
    """Change one stored branding field at a time, then return the card to empty."""
    name_a, name_b, description = "AlphaName", "BetaName", "Sign-in line"
    empty = {"product_name": "", "console_title": "", "login_description": "", "logo": ""}
    expect(card.locator(".ant-switch")).to_have_count(0, timeout=12000)
    expect(card.locator(".ant-upload-select")).to_have_count(1, timeout=12000)
    name = field_input(branding["productName"])
    title = field_input(branding["consoleTitle"])
    sign_in = field_input(branding["loginDescription"], "textarea")

    def save_exact(body, label):
        with page.expect_response(lambda response: urlsplit(response.url).path == BRANDING
                                  and response.request.method == "PATCH") as saved:
            click_save()
        check(saved.value.status == 200, f"{label} HTTP {saved.value.status}")
        sent = saved.value.request.post_data_json
        check(sent == body, f"{label} body differs")
        stored = branding_doc(page.request.get(server_base + BRANDING), label)
        check(stored == body, f"{label} independent GET differs")
        return stored

    name.fill(name_a)
    save_exact({**empty, "product_name": name_a}, "product name A")
    expect(title).to_have_value("")
    expect(sign_in).to_have_value("")
    name.fill(name_b)
    save_exact({**empty, "product_name": name_b}, "product name B")
    expect(title).to_have_value("")
    expect(sign_in).to_have_value("")
    sign_in.fill(description)
    save_exact({**empty, "product_name": name_b, "login_description": description},
               "sign-in text set")
    expect(name).to_have_value(name_b)
    expect(title).to_have_value("")
    sign_in.fill("")
    save_exact({**empty, "product_name": name_b}, "sign-in text cleared")
    expect(title).to_have_value("")

    def accept_logo(raw, data_url):
        upload = card.locator(".ant-upload-select input[type='file']")
        expect(upload).to_have_count(1, timeout=12000)
        upload.set_input_files({"name": "logo.svg", "mimeType": "image/svg+xml", "buffer": raw})
        crop = page.get_by_role("dialog").last
        expect(crop.get_by_text(branding["crop"]["title"], exact=True)).to_be_visible()
        confirm = button(crop, branding["crop"]["confirm"])
        expect(confirm).to_be_enabled(timeout=15000)
        confirm.click()
        expect(crop).to_be_hidden()
        expect(card.locator("img.app-brand-logo")).to_have_attribute("src", data_url, timeout=12000)

    logo_a, logo_b = _svg("#112233"), _svg("#445566")
    url_a, url_b = _logo_url(logo_a), _logo_url(logo_b)
    accept_logo(logo_a, url_a)
    save_exact({**empty, "product_name": name_b, "logo": url_a}, "logo A")
    expect(name).to_have_value(name_b)
    expect(title).to_have_value("")
    expect(sign_in).to_have_value("")
    accept_logo(logo_b, url_b)
    save_exact({**empty, "product_name": name_b, "logo": url_b}, "logo B")
    expect(name).to_have_value(name_b)
    expect(title).to_have_value("")
    expect(sign_in).to_have_value("")
    button(card, branding["restoreDefault"]).click()
    # BrandMark always renders an <img> and falls back to the built-in wordmark,
    # so "restored" is the fallback src rather than a missing element.
    expect(card.locator("img.app-brand-logo")).to_have_attribute(
        "src", "/assets/gpuworks-wordmark.svg")
    save_exact({**empty, "product_name": name_b}, "logo cleared")
    expect(title).to_have_value("")
    expect(sign_in).to_have_value("")
    name.fill("")
    save_exact(empty, "single-field restore")
    return {"switches": 0, "upload_selects": 1, "name_a": name_a, "name_b": name_b,
            "description_cleared": True, "logo_replaced": True, "logo_cleared": True}


# Contract: gpuworks/settings/branding
# Mainline: 独立 server/SQLite → 登录页默认品牌 → 设置页拒绝非图片文件
#           → 裁切弹窗确认后接受 SVG → 保存产品名/标题/登录文案/图标
#           → 产品名保存中文、英文、数字和标点；控制字符被拒绝且原值不变
#           → 控制台标题、标签页与公开登录页生效 → 刷新/独立 GET 回读
#           → 登录文案中的换行被保存 → 恢复默认并再次核对
def test_branding(page, context, browser, server, artifacts, report, locale):
    settings, auth, common = strings(locale)
    branding = settings["branding"]
    # The product has no default header title: consoleTitle defaults to "" and
    # Brand renders the logo alone with no title span when it is empty. Reading
    # a defaultConsoleTitle string out of the locale would be inventing a value
    # the product never shows.
    default_console_title = ""
    product_name = f"e2e-{report['run_id'][-8:]}-brand"
    console_title = f"e2e-{report['run_id']}-console"
    login_description = f"Signed in to {report['run_id']}\nSecond line"
    svg = (b'<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32">'
           b'<rect width="32" height="32" fill="#0076f7"/></svg>')
    logo = "data:image/svg+xml;base64," + base64.b64encode(svg).decode()
    page_errors, console_errors, bad_responses, patches = [], [], [], []
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
        if urlsplit(request.url).path == BRANDING and request.method == "PATCH":
            patches.append(request.post_data_json)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/login")
    # With no configured product name the sign-in page shows the built-in logo
    # and no visible name: ProductName is only rendered once an administrator
    # has set one. The default name reaches the reader through the logo's alt
    # text and the document title, so assert it there rather than as body text.
    expect(page.locator(".auth-brand-panel img.app-brand-logo")).to_have_attribute(
        "alt", "GPUWorks")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    held_load = []

    def hold_get(route):
        if route.request.method != "GET" or urlsplit(route.request.url).path != BRANDING:
            route.continue_()
            return
        held_load.append(route)

    page.route("**" + BRANDING, hold_get)
    page.goto(server.base + "/settings")
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    general = page.get_by_role("tab", name=settings["page"]["tabs"]["general"], exact=True)
    expect(general).to_have_attribute("aria-selected", "true")
    report["last_step"] = "settings-general"
    card = page.locator("#branding")
    save_button = card.locator("button.ant-btn-primary")
    expect(save_button).to_be_disabled()
    check(card.locator("input[type='password']").count() == 0, "branding card echoes a secret")
    for _ in range(50):
        if held_load:
            break
        page.wait_for_timeout(100)
    expect(save_button).to_be_disabled()
    check(len(held_load) >= 1, f"branding load did not start ({len(held_load)})")
    for route in held_load:
        route.continue_()
    page.unroute("**" + BRANDING, hold_get)
    expect(save_button).to_be_enabled()

    initial = branding_doc(page.request.get(server.base + BRANDING), "initial")
    check(initial == {"product_name": "", "console_title": "",
                      "login_description": "", "logo": ""},
          f"initial branding differs: {initial}")

    card = page.locator("#branding")
    expect(card.get_by_text(branding["title"], exact=True).first).to_be_visible()

    def field_input(label, selector="input"):
        # The AntD label is not associated with the control, so scope by the
        # form item that carries the visible label.
        item = card.locator(".ant-form-item").filter(has_text=label).first
        return item.locator(selector).first

    product_input = field_input(branding["productName"])
    title_input = field_input(branding["consoleTitle"])
    description_input = field_input(branding["loginDescription"], "textarea")
    expect(product_input).to_have_value("")
    expect(title_input).to_have_value("")
    expect(description_input).to_have_value("")
    expect(card.locator(".ant-upload-select")).to_have_count(1, timeout=12000)
    report["branding_logo_upload"] = True

    def expect_description_autosize():
        # autoSize writes an inline height. min-height and max-height do not count.
        expect(description_input).to_have_attribute(
            "style", re.compile(r"(?:^|[^-])height:\s*\d+(?:\.\d+)?px"), timeout=12000)

    expect_description_autosize()
    report["branding_name_keyboard"] = exercise_branding_name_keyboard(
        page, card, branding, product_input, title_input, description_input,
        save_button, patches)
    report["branding_inventory"] = exercise_branding_field_inventory(
        card, branding, patches, {
            "productName": "GPUWorks",
            "consoleTitle": branding["consoleTitlePlaceholder"],
            "loginDescription": common["branding"]["defaultLoginDescription"],
        }, initial)

    def click_save():
        # Role clicks flake while a control character is in the field or while
        # the button re-renders into its loading state.
        target = card.locator("button.ant-btn-primary")
        expect(target).to_be_enabled()
        box = target.bounding_box()
        check(box is not None, "branding save button has no box")
        page.mouse.click(box["x"] + box["width"] / 2, box["y"] + box["height"] / 2)

    report["last_step"] = "branding-conflict"
    report["branding_conflict"] = exercise_branding_conflict(
        page, card, branding, server.base, BRANDING, patches, click_save, default_console_title)

    # 2.4: one saved value covers Chinese, English, a digit and punctuation.
    # A following control character is forbidden on the single-line name.
    # The multiline login description keeps a newline; that save is below.
    mixed_name = "测A1."
    forbidden_name = mixed_name + "\u0001"
    product_input.fill(mixed_name)
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method == "PATCH") as accepted_name:
        click_save()
    check(accepted_name.value.status == 200,
          f"character-class save HTTP {accepted_name.value.status}")
    check(accepted_name.value.request.post_data_json["product_name"] == mixed_name,
          "the saved product name is not the mixed text")
    expect(page.locator("img.app-brand-logo").first).to_have_attribute("alt", mixed_name)
    check(page.title() == mixed_name, f"document title after saving the name is {page.title()!r}")
    stored_name = branding_doc(page.request.get(server.base + BRANDING), "character class")
    check(stored_name["product_name"] == mixed_name,
          f"independent GET dropped the mixed name: {stored_name['product_name']!r}")
    product_input.fill(forbidden_name)
    expect(product_input).to_have_value(forbidden_name)
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method == "PATCH") as rejected_name:
        click_save()
    check(rejected_name.value.status == 400,
          f"a control character in the product name was stored: HTTP {rejected_name.value.status}")
    rejected_body = rejected_name.value.json()
    check(rejected_body.get("code") == "FIELD_INVALID",
          f"control-character rejection is {rejected_body}")
    check("control character" in str(rejected_body.get("params", {})),
          f"control-character reason missing: {rejected_body}")
    invalid_value = common["apiError"]["FIELD_INVALID"].replace(
        "{{detail}}", "product_name: contains a control character")
    save_failed = settings["error"]["save"].replace("{{detail}}", invalid_value)
    expect(page.get_by_text(save_failed, exact=True).first).to_be_visible(timeout=12000)
    # Count, Tab, Enter and viewport already passed. The visible rejection is the
    # remaining 1.15 clause, so the flag waits until this sentence is on screen.
    report["branding_name_show_count"] = True
    expect(page.get_by_text("control character", exact=False).first).to_be_visible()
    expect(product_input).to_have_value(forbidden_name)
    expect(page.locator("img.app-brand-logo").first).to_have_attribute("alt", mixed_name)
    check(page.title() == mixed_name, f"document title after saving the name is {page.title()!r}")
    kept_name = branding_doc(page.request.get(server.base + BRANDING), "after forbidden name")
    check(kept_name["product_name"] == mixed_name
          and kept_name["console_title"] == ""
          and kept_name["login_description"] == ""
          and kept_name["logo"] == "",
          f"the forbidden name changed stored branding: {kept_name}")
    expect(card.get_by_text(branding["title"], exact=True).first).to_be_visible(timeout=12000)
    expect(page.get_by_role("dialog")).to_have_count(0)
    expect_description_autosize()
    report["last_step"] = "product-name-character-classes"
    report["character_class"] = {"accepted": mixed_name, "forbidden_status": 400,
                                 "stored_after_reject": kept_name["product_name"]}

    # 2.3: the product name limit is 32 Unicode characters. Empty is the
    # shortest stored value (the closing clear). Nothing is shorter than empty.
    longest_name = "测" * 32
    product_input.fill(longest_name + "测")
    expect(product_input).to_have_value(longest_name)
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method == "PATCH") as longest_saved:
        click_save()
    check(longest_saved.value.status == 200, f"32-character save HTTP {longest_saved.value.status}")
    check(longest_saved.value.request.post_data_json["product_name"] == longest_name,
          "the saved product name is not 32 characters")
    expect(page.locator("img.app-brand-logo").first).to_have_attribute("alt", longest_name)
    check(page.title() == longest_name, f"document title after saving the name is {page.title()!r}")
    stored_longest = branding_doc(page.request.get(server.base + BRANDING), "32 characters")
    check(stored_longest["product_name"] == longest_name,
          "independent GET did not keep the 32-character name")
    report["branding_textarea_autosize"] = True
    too_long = page.request.patch(server.base + BRANDING, data={"product_name": longest_name + "测"})
    check(too_long.status == 400, f"33 characters were stored: HTTP {too_long.status}")
    too_long_body = too_long.json()
    check(too_long_body.get("code") == "FIELD_INVALID", f"length rejection is {too_long_body}")
    check("at most 32 characters" in str(too_long_body.get("params", {})),
          f"length reason missing: {too_long_body}")
    kept_longest = branding_doc(page.request.get(server.base + BRANDING), "after 33 characters")
    check(kept_longest["product_name"] == longest_name,
          "the 33-character name changed the stored product name")
    report["last_step"] = "product-name-length"
    report["name_length"] = {"longest": 32, "rejected": 33, "unit": "unicode characters"}

    patches_before_logo = len(patches)
    upload = card.locator('input[type="file"]')
    upload.set_input_files({"name": "bad.txt", "mimeType": "text/plain",
                            "buffer": b"not an image"})
    expect(page.get_by_text(branding["logoError"]["notImage"], exact=True).first).to_be_visible()
    check(len(patches) == patches_before_logo, "a rejected logo submitted branding")
    upload.set_input_files({"name": "logo.svg", "mimeType": "image/svg+xml", "buffer": svg})
    crop = page.get_by_role("dialog").last
    expect(crop.get_by_text(branding["crop"]["title"], exact=True)).to_be_visible()
    expect(crop.locator(".logo-crop-image")).to_be_visible()
    expect(crop.get_by_test_id("logo-crop-preview")).to_be_visible()
    confirm_crop = button(crop, branding["crop"]["confirm"])
    expect(confirm_crop).to_be_enabled(timeout=15000)
    confirm_crop.click()
    expect(crop).to_be_hidden()
    expect(card.locator("img.app-brand-logo")).to_have_attribute("src", logo)
    expect(button(card, branding["restoreDefault"])).to_be_visible()
    report["branding_inventory"]["restore_after_logo"] = True
    report["last_step"] = "logo-cropped-and-validated"

    # 2.6: paste, keyboard clear, then keyboard editing. The newline in the
    # typed description is what the later save, reload and GET keep.
    context.grant_permissions(["clipboard-read", "clipboard-write"])
    pasted = "粘A1."
    patches_before_edit = len(patches)
    page.evaluate("(text) => navigator.clipboard.writeText(text)", pasted)
    description_input.click()
    description_input.press("Control+V")
    expect(description_input).to_have_value(pasted)
    description_input.press("Control+A")
    description_input.press("Backspace")
    expect(description_input).to_have_value("")
    first_line, second_line = login_description.split("\n", 1)
    description_input.press_sequentially(first_line)
    description_input.press("Enter")
    description_input.press_sequentially(second_line)
    expect(description_input).to_have_value(login_description)
    check(len(patches) == patches_before_edit, "editing the login description submitted branding")
    report["last_step"] = "login-description-edited"
    report["text_edit"] = {"pasted": pasted, "cleared": True, "keyboard": login_description}

    product_input.fill(product_name)
    title_input.fill(console_title)
    expected_body = {"product_name": product_name, "console_title": console_title,
                     "login_description": login_description, "logo": logo}
    check({field["request_field"] for field in report["branding_inventory"]["fields"]}
          == set(expected_body),
          "the save body fields are not the inventoried branding fields")
    held = []

    def hold_patch(route):
        if route.request.method != "PATCH" or urlsplit(route.request.url).path != BRANDING:
            route.continue_()
            return
        held.append(route)

    page.route("**" + BRANDING, hold_patch)
    click_save()
    inflight = card.locator("button.ant-btn-primary")
    expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
    inflight.click()
    product_input.press("Enter")
    check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
    check(held[0].request.post_data_json == expected_body, "held branding body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method == "PATCH") as saved:
        held[0].continue_()
    page.unroute("**" + BRANDING, hold_patch)
    check(saved.value.status == 200, f"branding save HTTP {saved.value.status}")
    check(saved.value.request.post_data_json == expected_body,
          f"branding PATCH differs: {saved.value.request.post_data_json}")
    expect(page.get_by_text(branding["saved"], exact=True).first).to_be_visible()
    expect(page.locator(".app-brand-name").first).to_have_text(console_title)
    check(page.title() == product_name, f"document title is {page.title()!r}")
    saved_logo = page.locator("img.app-brand-logo").first
    expect(saved_logo).to_have_attribute("src", logo)
    expect(saved_logo).to_be_visible()
    expect(saved_logo).to_have_js_property("complete", True)
    expect(saved_logo).to_have_js_property("naturalWidth", 32)
    expect(saved_logo).to_have_js_property("naturalHeight", 32)
    report["last_step"] = "branding-saved"

    public_context = browser.new_context(
        viewport=report["viewport"],
        locale="zh-CN" if locale == "zh" else "en-US", timezone_id="UTC")
    public_errors = []
    try:
        public_page = public_context.new_page()
        public_errors = watch_public_page(public_page)
        public_page.goto(server.base + "/login")
        expect(public_page.get_by_text(product_name, exact=True).first).to_be_visible()
        expect(public_page.get_by_text(login_description, exact=True)).to_be_visible()
        public_logo = public_page.locator("img.app-brand-logo").first
        expect(public_logo).to_have_attribute("src", logo)
        expect(public_logo).to_be_visible()
        expect(public_logo).to_have_js_property("complete", True)
        expect(public_logo).to_have_js_property("naturalWidth", 32)
        expect(public_logo).to_have_js_property("naturalHeight", 32)
        check(public_page.title() == product_name,
              f"sign-in document title is {public_page.title()!r}")
    finally:
        public_context.close()
    report["last_step"] = "public-login-branded"

    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    card = page.locator("#branding")
    expect(field_input(branding["productName"])).to_have_value(product_name)
    expect(field_input(branding["consoleTitle"])).to_have_value(console_title)
    expect(field_input(branding["loginDescription"], "textarea")
           ).to_have_value(login_description)
    expect(card.locator("img.app-brand-logo")).to_have_attribute("src", logo)
    persisted = branding_doc(page.request.get(server.base + BRANDING), "persisted")
    check(persisted == expected_body, f"persisted branding differs: {persisted}")
    check("\n" in persisted["login_description"]
          and persisted["login_description"] == login_description,
          "the saved login description did not keep its newline")
    report["last_step"] = "branding-reloaded"

    button(card, branding["restoreDefault"]).click()
    field_input(branding["productName"]).fill("")
    field_input(branding["consoleTitle"]).fill("")
    field_input(branding["loginDescription"], "textarea").fill("")
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method == "PATCH") as cleared:
        click_save()
    check(cleared.value.status == 200, f"branding clear HTTP {cleared.value.status}")
    check(cleared.value.request.post_data_json == {
        "product_name": "", "console_title": "", "login_description": "", "logo": ""},
        f"branding clear body differs: {cleared.value.request.post_data_json}")
    cleared_doc = branding_doc(page.request.get(server.base + BRANDING), "cleared")
    check(cleared_doc == {"product_name": "", "console_title": "",
                          "login_description": "", "logo": ""},
          f"branding was not restored: {cleared_doc}")
    expect(page.locator(".app-brand-name")).to_have_count(0)
    check(page.title() == "GPUWorks", f"document title after restore is {page.title()!r}")
    expect(page.locator("img.app-brand-logo").first).to_have_attribute(
        "src", "/assets/gpuworks-wordmark.svg")
    # The built-in brand is a wordmark image, not a lettered mark: the restored
    # state is the fallback wordmark carrying the default product name in alt.
    expect(page.locator("img.app-brand-logo").first).to_have_attribute("alt", "GPUWorks")
    report["last_step"] = "branding-restored"
    report["branding_single_field"] = exercise_branding_single_field(
        page, card, branding, field_input, click_save, server.base)
    report["last_step"] = "branding-single-field"

    check(not public_errors, "public sign-in page errors: " + str(public_errors))
    invalid_responses = [item for item in bad_responses
                         if item == {"method": "PATCH", "path": BRANDING, "status": 400}]
    unexpected_responses = [item for item in bad_responses if item not in invalid_responses]
    check(len(invalid_responses) == 1,
          "the forbidden product name did not return 400: " + str(bad_responses))
    check(not unexpected_responses, "unexpected browser HTTP errors: " + str(unexpected_responses))
    control_logs = [item for item in console_errors if "400" in item]
    other_logs = [item for item in console_errors if item not in control_logs]
    check(len(control_logs) == 1,
          "the forbidden product name did not log its 400: " + str(console_errors))
    check(not page_errors and not other_logs,
          f"browser errors: {page_errors}, {other_logs}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "settings page has horizontal overflow")
    report["last_step"] = "branding-html-escaping"
    # 2.7: the product name is free text, so it can carry markup. It is shown on the sign-in
    # page, in the header and in the document title, and every one of those has to keep it as
    # text. The value is saved through the real PATCH and read back from the independent GET.
    markup_name = "e2e-<img src=x onerror=1>-brand"
    name_input = field_input(branding["productName"])
    name_input.fill(markup_name)
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method in {"PUT", "PATCH"}) as markup_saved:
        click_save()
    check(markup_saved.value.status == 200, f"markup name save HTTP {markup_saved.value.status}")
    stored_markup = branding_doc(page.request.get(server.base + BRANDING), "markup name")
    check(stored_markup["product_name"] == markup_name,
          f"the stored name is not the markup string: {stored_markup['product_name']!r}")
    # The edit control holds the value verbatim, and the page built no element from it.
    expect(field_input(branding["productName"])).to_have_value(markup_name)
    check(page.locator("img[src='x']").count() == 0,
          "the name created an img element on the settings page")
    # The sign-in page is the public surface for the same value.
    markup_context = browser.new_context(
        viewport=report["viewport"],
        locale="zh-CN" if locale == "zh" else "en-US", timezone_id="UTC")
    try:
        markup_page = markup_context.new_page()
        markup_page.goto(server.base + "/login")
        shown = markup_page.get_by_text(markup_name, exact=True).first
        expect(shown).to_be_visible()
        check(shown.evaluate("element => element.children.length") == 0,
              "the sign-in page renders the name through child nodes, not as text")
        check(markup_page.locator("img[src='x']").count() == 0,
              "the name created an img element on the sign-in page")
        check(markup_page.title() == markup_name,
              f"sign-in document title is {markup_page.title()!r}")
    finally:
        markup_context.close()
    report["html_escaping"] = {"markup": markup_name, "stored": True, "sign_in_text": True,
                               "settings_text": True, "injected_nodes": 0}
    # Restore the name so the recorded evidence matches the original brand.
    field_input(branding["productName"]).fill(product_name)
    click_save()
    expect(field_input(branding["productName"])).to_have_value(product_name)
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/settings", "card": branding["title"],
         "logo_rejected": branding["logoError"]["notImage"],
         "logo_type": "image/svg+xml", "header_title": console_title,
         "document_title": product_name, "sign_in_description": "Second line",
         "accepted_name": report.get("character_class", {}).get("accepted"),
         "forbidden_name_status": report.get("character_class", {}).get("forbidden_status"),
         "login_description_keeps_newline": "\n" in login_description,
         "text_edit": report.get("text_edit"),
         "logo_upload_select": report.get("branding_logo_upload"),
         "single_field": report.get("branding_single_field"),
         "restored": branding["restoreDefault"]},
        {"layer": "request", "patch_bodies": [
            {"product_name": item.get("product_name"),
             "console_title": item.get("console_title"),
             "login_description": item.get("login_description"),
             "logo_type": item.get("logo", "").split(";")[0]} for item in patches]},
        {"layer": "persistence", "initial": initial,
         "saved": {"product_name": persisted["product_name"],
                   "console_title": persisted["console_title"],
                   "login_description": persisted["login_description"],
                   "logo_matches": persisted["logo"] == logo},
         "restored": cleared_doc},
        {"layer": "cleanup", "restored": cleared_doc == {
            "product_name": "", "console_title": "",
            "login_description": "", "logo": ""}},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "public_page": public_errors}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/settings-branding.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_branding.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin / public sign-in page",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "branding_helper_sha256": digest(ROOT / "test/e2e/settings_branding_fixture.py"),
        "execution": "N/A: branding is rendered from the control-plane setting; no workload",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["maximum-length boundaries and whitespace-only text",
                        "PNG/JPEG/WebP compression and the tooLarge path",
                        "non-admin authorization",
                        "branding defaults in the other locale",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/settings_branding.py", *sys.argv[1:]]}
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
                test_branding(page, context, browser, server, artifacts, report, args.locale)
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
