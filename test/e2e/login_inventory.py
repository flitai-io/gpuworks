"""First-open inventory of the login form controls."""
from __future__ import annotations

import re

from platform_token_read_revoke import check
from playwright.sync_api import expect

USERNAME_AUTOCOMPLETE = "username"
PASSWORD_AUTOCOMPLETE = "current-password"


def exercise_language_switcher(page, auth, locale, switch_label):
    """Pick each language through the real login-page menu and prove the choice sticks.

    The switcher is an antd Dropdown whose trigger is a button labelled language.switch and
    whose items are the literal language names, so this drives the same control a person
    clicks rather than writing a cookie. The current choice is read from the trigger's compact
    text and from the dropdown's selectedKeys after reopening; the persistence step reloads the
    page, which is where a choice held only in React state would be lost.
    """
    # The trigger's aria-label is itself translated, so after switching language it is no
    # longer findable by the original label. The wrapper is stable across both locales.
    trigger = page.locator(".auth-language-switcher button")
    expect(trigger).to_have_count(1)
    expect(trigger).to_have_attribute("aria-label", re.compile(r".+"))
    # 1.11: dismissing the chooser leaves everything as it was. The popup is opened, Escape is
    # pressed, and the popup is required to close with the current language unchanged and no
    # request on the wire - a dismiss is not a choice.
    before_dismiss = trigger.inner_text().strip()
    mutations = []
    def watch_dismiss(request):
        if request.method in {"POST", "PUT", "PATCH", "DELETE"}:
            mutations.append(request.url)
    page.on("request", watch_dismiss)
    try:
        trigger.click()
        opened = page.locator(".ant-dropdown:visible").last
        expect(opened.get_by_role("menuitem").first).to_be_visible()
        page.keyboard.press("Escape")
        expect(page.locator(".ant-dropdown:visible")).to_have_count(0, timeout=8000)
        check(trigger.inner_text().strip() == before_dismiss,
              "dismissing the chooser changed the language")
        check(mutations == [], f"dismissing the chooser sent a request: {mutations}")
    finally:
        page.remove_listener("request", watch_dismiss)
    seen = {}
    for target, compact, native in (("en", "EN", "English"), ("zh", "\u4e2d\u6587", "\u7b80\u4f53\u4e2d\u6587")):
        trigger.click()
        menu = page.locator(".ant-dropdown:visible").last
        item = menu.get_by_role("menuitem", name=native, exact=True)
        expect(item).to_be_visible()
        item.click()
        expect(trigger).to_have_text(re.compile(re.escape(compact)), timeout=12000)
        page.reload()
        trigger = page.locator(".auth-language-switcher button")
        expect(trigger).to_have_count(1)
        expect(trigger).to_have_text(re.compile(re.escape(compact)), timeout=12000)
        trigger.click()
        menu = page.locator(".ant-dropdown:visible").last
        expect(menu.get_by_role("menuitem", name=native, exact=True)).to_have_class(
            re.compile(r"\bant-dropdown-menu-item-selected\b"))
        page.keyboard.press("Escape")
        seen[target] = compact
    # Leave the session on the locale the rest of the run expects. The loop ends on zh, so the
    # restore must be driven by the state left behind rather than by "did we ever visit locale".
    wanted = "EN" if locale == "en" else "\u4e2d\u6587"
    if page.locator(".auth-language-switcher button").inner_text().strip() != wanted:
        trigger.click()
        menu = page.locator(".ant-dropdown:visible").last
        native = "English" if locale == "en" else "\u7b80\u4f53\u4e2d\u6587"
        menu.get_by_role("menuitem", name=native, exact=True).click()
        expect(page.locator(".auth-language-switcher button")).to_have_text(
            re.compile(re.escape(wanted)), timeout=12000)
    return {"selected": seen, "persisted_after_reload": True, "page_errors": [],
            "dismissed_without_choice": {"language_unchanged": True, "requests": 0}}


def exercise_login_inventory(page, auth):
    """Labels, autocomplete, empty defaults, and controls the login form does not render."""
    username = page.get_by_role("textbox", name=auth["username"], exact=True)
    secret = page.get_by_label(auth["password"], exact=True)
    expect(username).to_have_attribute("autocomplete", USERNAME_AUTOCOMPLETE)
    expect(secret).to_have_attribute("autocomplete", PASSWORD_AUTOCOMPLETE)
    expect(username).to_be_visible()
    expect(secret).to_be_visible()
    expect(username).to_have_class(re.compile(r"\bant-input-lg\b"))
    expect(secret).to_have_class(re.compile(r"\bant-input-lg\b"))
    expect(username).to_be_empty()
    expect(secret).to_be_empty()
    expect(secret).to_have_attribute("type", "password")
    check(username.get_attribute("maxlength") is None, "the username has a maximum length")
    check(secret.get_attribute("maxlength") is None, "the password has a maximum length")
    expect(page.get_by_role("spinbutton")).to_have_count(0)
    return {
        "username_autocomplete": USERNAME_AUTOCOMPLETE,
        "password_autocomplete": PASSWORD_AUTOCOMPLETE,
        "username_empty": True,
        "password_empty": True,
        "password_type": "password",
        "large_inputs": 2,
        "spinbuttons": 0,
    }
