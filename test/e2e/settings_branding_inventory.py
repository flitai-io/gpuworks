"""Field inventory and last-write for the branding card.

Product name, header title, and sign-in text are optional textboxes with empty
stored defaults. The logo is an optional file input. Reading them sends no PATCH.
"""
import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import check

_TEXT = (
    ("productName", "input", 32, "product_name"),
    ("consoleTitle", "input", 64, "console_title"),
    ("loginDescription", "textarea", 200, "login_description"),
)
_ACCEPT = "image/png,image/jpeg,image/webp,image/svg+xml"


def exercise_branding_field_inventory(card, branding, patches, placeholders, initial):
    """Register every branding field before any value is typed."""
    baseline = len(patches)
    expect(card.locator(".ant-form-item")).to_have_count(4)
    check(card.locator("input[type='password'], .ant-switch").count() == 0,
          "the branding card has a secret or a switch")
    fields = []
    for key, selector, limit, request_field in _TEXT:
        label = branding[key]
        item = card.locator(".ant-form-item").filter(has_text=label).first
        expect(item).to_be_visible()
        control = item.locator(selector).first
        expect(control).to_be_visible()
        expect(control).to_have_value("")
        expect(control).to_have_attribute("placeholder", placeholders[key])
        expect(control).to_have_attribute("maxlength", str(limit))
        check(item.locator(".ant-form-item-required").count() == 0, f"{label} is required")
        check(control.get_attribute("aria-required") is None, f"{label} is required")
        expect(item.locator(".ant-input-show-count-suffix, .ant-input-data-count")
               ).to_have_text(f"0 / {limit}")
        check(item.locator(".ant-input-number, .ant-slider").count() == 0, f"{label} has a unit")
        fields.append({
            "label": label, "type": "textbox", "required": False, "default": "",
            "unit": None, "max_length": limit, "conditional": False,
            "placeholder": placeholders[key], "request_field": request_field,
        })
    logo_item = card.locator(".ant-form-item").filter(has_text=branding["logo"]).first
    expect(logo_item).to_be_visible()
    upload = logo_item.locator("input[type='file']")
    expect(upload).to_have_count(1)
    expect(upload).to_have_attribute("accept", _ACCEPT)
    # The upload icon's accessible name is "upload", so the button name is not
    # the label alone.
    expect(logo_item.get_by_role(
        "button", name=re.compile(re.escape(branding["uploadLogo"])))).to_be_visible()
    check(logo_item.locator(".ant-form-item-required").count() == 0, "the logo is required")
    check(logo_item.get_by_role("button", name=branding["restoreDefault"], exact=True).count() == 0,
          "restore shows while the logo is empty")
    check(logo_item.locator(".ant-input-number, .ant-slider").count() == 0, "the logo has a unit")
    fields.append({
        "label": branding["logo"], "type": "file", "required": False, "default": "",
        "unit": None, "accept": _ACCEPT, "conditional": False,
        "hidden_until_set": branding["restoreDefault"], "request_field": "logo",
    })
    check(len(patches) == baseline, "reading the branding fields sent a PATCH")
    check(set(initial) == {field["request_field"] for field in fields},
          f"the stored branding fields differ from the inventory: {sorted(initial)}")
    check(all(initial[field["request_field"]] == "" for field in fields),
          f"the stored branding defaults are not empty: {initial}")
    return {"fields": fields, "requests": 0, "restore_while_empty": False}


_EMPTY = {"product_name": "", "console_title": "", "login_description": "", "logo": ""}
_OTHER = {
    "product_name": "Other Session",
    "console_title": "Other Title",
    "login_description": "Other line",
    "logo": "",
}


def exercise_branding_conflict(page, card, branding, base, path, patches, click_save, default_title):
    """Another session replaces branding. The open card keeps its draft and save writes that draft back."""
    baseline = len(patches)

    def text_field(label, selector="input"):
        item = card.locator(".ant-form-item").filter(has_text=label).first
        return item.locator(selector).first

    other = page.request.patch(base + path, data=_OTHER)
    check(other.status == 200, f"the other branding session was rejected: HTTP {other.status}")
    check(other.json() == _OTHER, f"the other branding session was not stored: {other.text()}")
    stored = page.request.get(base + path)
    check(stored.status == 200 and stored.json() == _OTHER,
          f"the other branding session did not win: {stored.status} {stored.text()}")
    expect(text_field(branding["productName"])).to_have_value("")
    expect(text_field(branding["consoleTitle"])).to_have_value("")
    expect(text_field(branding["loginDescription"], "textarea")).to_have_value("")
    # The other session stored no logo, so BrandMark must fall back to the
    # built-in wordmark instead of showing the open card's draft.
    check(card.locator("img.app-brand-logo").get_attribute("src") == "/assets/gpuworks-wordmark.svg",
          "the open card took the other session logo")
    # An empty consoleTitle renders the logo alone, so there is no title span.
    expect(page.locator(".app-brand-name")).to_have_count(0)
    check(card.locator(".ant-alert-error").count() == 0, "the branding card shows a conflict")
    check(len(patches) == baseline, "the other branding session was counted as a page save")
    with page.expect_response(lambda response: urlsplit(response.url).path == path
                              and response.request.method == "PATCH") as saved:
        click_save()
    check(saved.value.status == 200, f"saving the open branding draft failed: HTTP {saved.value.status}")
    check(saved.value.request.post_data_json == _EMPTY,
          f"the open draft was not the saved branding: {saved.value.request.post_data_json}")
    read = page.request.get(base + path)
    check(read.status == 200 and read.json() == _EMPTY,
          f"the open draft did not replace the other session: {read.status} {read.text()}")
    return {
        "other_session": 200,
        "other_read": _OTHER,
        "draft": _EMPTY,
        "saved_status": 200,
        "saved_read": _EMPTY,
        "conflict_status": False,
    }
