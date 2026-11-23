"""Complete a settings confirmation through the visible localized button."""
import json
from pathlib import Path

from playwright.sync_api import expect


def confirm_setting(page, trigger):
    trigger.click()
    language = page.evaluate("navigator.language").split("-")[0]
    messages = Path(__file__).resolve().parents[2] / "ui/src/i18n/locales" / language / "common.json"
    label = json.loads(messages.read_text())["actions"]["confirm"]
    popup = page.locator('[data-slot="popover-content"]:visible')
    confirm = popup.get_by_role("button", name=label, exact=True)
    expect(confirm).to_be_enabled()
    confirm.click()
