"""Name and path reject empty and whitespace. No storage create."""
from __future__ import annotations

import re
from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect
from platform_token_read_revoke import button, check

STORAGES = "/api/v1/storages"


def _item(control):
    return control.locator(
        "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")


def exercise_storage_required(page, dialog, storage, directory):
    """Each required field rejects an empty value and a whitespace-only value."""
    name = dialog.get_by_label(storage["form"]["name"], exact=True)
    path = dialog.get_by_label(storage["form"]["path"], exact=True)
    save = button(dialog, storage["form"]["save"])
    name_error = storage["form"]["error"]["name"]
    path_error = storage["form"]["error"]["pathRequired"]
    name_item = _item(name)
    path_item = _item(path)

    def save_posts():
        try:
            with page.expect_request(
                    lambda request: request.method == "POST" and urlsplit(request.url).path == STORAGES,
                    timeout=1000):
                save.click()
            return True
        except PlaywrightTimeout:
            return False

    def located(item, message, control, other):
        expect(item).to_have_class(re.compile(r"ant-form-item-has-error"))
        expect(item.get_by_text(message, exact=True)).to_be_visible()
        expect(control).to_be_visible()
        expect(other).not_to_have_class(re.compile(r"ant-form-item-has-error"))

    path.fill(directory)
    name.fill("")
    check(not save_posts(), "empty name sent a storage create")
    expect(name).to_have_value("")
    located(name_item, name_error, name, path_item)

    name.fill("   ")
    check(not save_posts(), "whitespace name sent a storage create")
    expect(name).to_have_value("   ")
    located(name_item, name_error, name, path_item)

    name.fill("req-probe")
    path.fill("")
    check(not save_posts(), "empty path sent a storage create")
    expect(path).to_have_value("")
    located(path_item, path_error, path, name_item)

    path.fill("   ")
    check(not save_posts(), "whitespace path sent a storage create")
    expect(path).to_have_value("   ")
    located(path_item, path_error, path, name_item)

    return {"empty_name": True, "whitespace_name": True, "empty_path": True,
            "whitespace_path": True, "creates": 0}


def exercise_storage_name_contract(page, dialog, t, events):
    """The name field's real contract: trim, length bounds, and allowed characters.

    storageForm.ts validates /^[a-z0-9][a-z0-9-]{0,62}$/ against the trimmed value
    and submits the trimmed value, so both the bounds and the trim are product
    behaviour rather than test preferences.
    """
    name_input = dialog.get_by_label(t['form']['name'], exact=True)
    path_input = dialog.get_by_label(t['form']['path'], exact=True)
    save = button(dialog, t['form']['save'])
    error = t['form']['error']['name']
    name_item = name_input.locator(
        'xpath=ancestor::div[contains(concat(" ", normalize-space(@class), " "), " ant-form-item ")][1]')
    path_item = path_input.locator(
        'xpath=ancestor::div[contains(concat(" ", normalize-space(@class), " "), " ant-form-item ")][1]')
    path_input.fill('/tmp/e2e-name-contract')
    rows = []

    def attempt(value):
        # The form validates as it is typed; pressing Save here would also trip the
        # disabled/probe paths, so the item's own error class is the signal.
        name_input.fill(value)
        name_input.blur()
        rejected = name_item.get_attribute('class') or ''
        return 'error' in rejected

    # 2.3 - the bounds are 1 and 63 characters: 63 accepted, 64 refused, and the
    # single character case accepted.
    for value, ok in [(('a' * 63), True), (('a' * 64), False), ('a', True), (('9' + 'b' * 62), True)]:
        rejected = attempt(value)
        check(rejected == (not ok),
              f'name of {len(value)} chars: rejected={rejected}, expected accepted={ok}')
        rows.append({'value_length': len(value), 'accepted': ok})

    # 2.2 - surrounding spaces are trimmed, not rejected: the trimmed value is what
    # the field accepts, and an all-space value is empty after the trim.
    for raw, ok in [('  spaced  ', True), ('   ', False)]:
        rejected = attempt(raw)
        check(rejected == (not ok),
              f'name {raw!r}: rejected={rejected}, expected accepted={ok}')
        rows.append({'value': raw, 'accepted': ok, 'trimmed': raw.strip()})

    # 2.4 - the allowed alphabet is lowercase letters, digits and inner hyphens.
    for raw, ok in [('Ünicode', False), ('中文名', False), ('UPPER', False),
                    ('under_score', False), ('has space', False), ('ok-name-2', True)]:
        rejected = attempt(raw)
        check(rejected == (not ok),
              f'name {raw!r}: rejected={rejected}, expected accepted={ok}')
        rows.append({'value': raw, 'accepted': ok})

    check(events == [], 'rejected names sent a mutation')
    expect(path_item).not_to_have_class(re.compile(r'ant-form-item-has-error'))
    name_input.fill('')
    return rows


def exercise_storage_name_editing(page, dialog, t, events):
    """Typing, pasting and clearing all leave the controlled input consistent.

    The name input is a controlled component driven by values.name, so every one
    of these paths has to end with the field showing exactly what was entered -
    a controlled input that swallows a paste or keeps stale text is a real defect.
    """
    name = dialog.get_by_label(t['form']['name'], exact=True)
    rows = []

    # Keyboard editing: type, backspace, then complete the value.
    name.fill('')
    name.type('paste-target', delay=10)
    expect(name).to_have_value('paste-target')
    for _ in range(6):
        page.keyboard.press('Backspace')
    expect(name).to_have_value('paste-')
    name.type('edited', delay=10)
    expect(name).to_have_value('paste-edited')
    rows.append({'path': 'keyboard', 'value': 'paste-edited'})

    # A real clipboard paste, which is a different path from fill(): the controlled
    # value has to take the whole clip rather than the first character.
    name.fill('')
    name.focus()
    page.evaluate("value => navigator.clipboard.writeText(value)", 'pasted-value')
    page.keyboard.press('Control+V')
    page.wait_for_timeout(150)
    pasted = name.input_value()
    check(pasted == 'pasted-value', f'paste produced {pasted!r}, expected the pasted text')
    rows.append({'path': 'paste', 'value': pasted})

    # Clearing with select-all + delete leaves an empty controlled value.
    page.keyboard.press('Control+A')
    page.keyboard.press('Delete')
    expect(name).to_have_value('')
    rows.append({'path': 'clear', 'value': ''})

    check(events == [], 'editing the name field sent a mutation')
    return rows
