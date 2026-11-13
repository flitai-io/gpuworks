#!/usr/bin/env python3
"""Cloud account edit: saved proxy overrides round-trip through the real server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-provider-endpoint-edit
External boundary: strict local RunPod v2 pods and account SSH keys fixture;
no management API mocks. Fixture tests:
PYTHONPATH=test/e2e python3 -m unittest test_provider_endpoint_fixture -v
PYTHONPATH=test/e2e python3 -m unittest test_provider_endpoint_cleanup -v
"""
from __future__ import annotations

from collections import Counter
import json
import re
import time
import traceback
from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check, collection
from provider_endpoint_fixture import RunPodFixture
from provider_default_endpoint_fixture import ProviderDefaultEndpointFixture
from provider_default_endpoint_checks import EXISTING_KEY, exercise_default_endpoint
from provider_conditional_checks import exercise_conditional_fields
from provider_text_value_checks import exercise_text_values
from provider_key_rotation_checks import exercise_key_rotation
from provider_secret_error_checks import exercise_provider_secret_errors
from provider_boundary_checks import exercise_name_boundaries, with_boundary_calls, boundary_http_errors
from provider_url_checks import exercise_provider_urls, needs_default_fixture
from provider_form_checks import (
    NAME_SUFFIX, form_action, observe_mutations, text_contract, clipboard_editing, secret_create,
    literal_name, concurrent_names, CHECK_FAMILIES, family_enabled, expected_upstream)

CONTRACT = "gpuworks/providers/endpoint-edit-roundtrip"


def cleanup_stage(report, name, action):
    """Attempt every owned close independently and never upgrade an earlier failure."""
    try:
        result = action()
        check(result is None or result is True, f"incomplete cleanup: {result!r}")
        report.setdefault("cleanup_stages", {})[name] = "ok"
        return True
    except Exception:
        report.setdefault("cleanup_stages", {})[name] = traceback.format_exc()
        report["status"] = "FAIL_TEST"
        return False


def exercise_table_id_isolation(page, common, copy, report):
    """11.4: two ResourceTables on two pages hold their layouts under different ids, so a choice made on
    one must not follow the user to the other, while the id that was chosen keeps it."""
    labels = common["resourceTable"]
    table = page.locator(".resource-table").first

    def headers():
        return [th.inner_text().strip() for th in table.locator("thead th").all()]

    def stored():
        return page.evaluate(
            "() => Object.keys(localStorage).filter(k => k.startsWith('gpuworks.table.')).sort()")

    public_before = headers()
    # Pick a movable column of the public table and hide it.
    drawer_button = table.get_by_role("button", name=labels["settings"], exact=True)
    expect(drawer_button).to_have_count(1)
    drawer_button.click()
    drawer = page.get_by_role("dialog", name=labels["settingsTitle"], exact=True)
    expect(drawer).to_be_visible()
    boxes = drawer.get_by_role("checkbox")
    movable = [box for box in boxes.all() if box.is_enabled()]
    check(bool(movable), "the public provider table has no movable column to hide")
    hidden_name = movable[0].evaluate("el => el.closest('label').innerText.trim()")
    movable[0].click()
    page.keyboard.press("Escape")
    expect(drawer).not_to_be_visible()
    public_after = headers()
    check(hidden_name not in " ".join(public_after),
          f"the public table kept the column that was hidden: {hidden_name} / {public_after}")
    public_keys = stored()
    # The public and private cloud pages merged into one External Platforms list
    # (CloudProvidersPage.tsx: "One list for every connected platform"; /zstack now redirects to
    # /providers?category=private_cloud). There is a single table, tableId="providers", so the
    # per-scope layout isolation this block used to assert no longer has two tables to compare.
    # What the merge does keep is that the category filter narrows the same table, so that is
    # what is asserted here, and the isolation is recorded as N/A.
    page.goto(page.url.split("/providers")[0] + "/zstack")
    expect(page.get_by_role("heading", name=copy["platforms"]["title"], exact=True)).to_be_visible()
    expect(page).to_have_url(re.compile(r"/providers\?category=private_cloud"))
    private_headers = headers()
    check(private_headers == public_after,
          f"the one platform table lost its stored layout after opening a category: "
          f"{private_headers} vs {public_after}")
    keys = stored()
    check(len(keys) == len(set(keys)), f"the stored layout keys are not distinct: {keys}")
    check(len(keys) == len(public_keys),
          f"opening a category created a second table layout: {public_keys} -> {keys}")
    report["table_id_isolation"] = {
        "status": "N/A", "reason": "the two provider tables merged into one (tableId=providers)",
        "hidden": hidden_name, "headers": public_after,
        "keys": page.evaluate(
            "() => Object.keys(localStorage).filter(k => k.startsWith('gpuworks.table.')).sort()")}
    # The category filter is part of the address, so the next block must not inherit it: a
    # runpod account lives in public_cloud and would be filtered out of the list.
    page.goto(page.url.split("/providers")[0] + "/providers")
    expect(page.get_by_role("heading", name=copy["platforms"]["title"], exact=True)).to_be_visible()
    # Leave the account as it was found: the layout is local, so clearing it restores the defaults.
    page.evaluate("() => Object.keys(localStorage).filter(k => k.startsWith('gpuworks.table.')).forEach(k => localStorage.removeItem(k))")
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    check(headers() == public_before, "clearing the stored layouts did not restore the defaults")


def exercise_column_preferences(page, common, copy, report, writes, errors):
    """11.1-11.3 on the public provider list: width, order and visibility are a viewer's local
    preference, so every step is read three times - from the rendered header row, from the layout the
    browser stored, and from a reload that has to rebuild the table out of that stored copy."""
    labels = common["resourceTable"]
    writes_before, errors_before = len(writes), len(errors)
    columns = copy["table"]["column"]
    table = page.locator(".resource-table").first
    # The key carries the schema generation suffix, so it is found by its table id rather than
    # assumed. Public and private clouds now share one list (CloudProvidersPage.tsx:308).
    table_id = "providers"
    keys_of = {columns["name"]: "name", columns["type"]: "kind",
               columns["instances"]: "instance_count", columns["updatedAt"]: "updated_at",
               columns["status"]: "state"}

    def headers():
        return [th.inner_text().strip() for th in table.locator("thead th").all()]

    def shown():
        return [title for title in headers() if title]

    def resize(name):
        return table.get_by_role("separator", name=labels["resizeColumn"].replace("{{name}}", name))

    def width_of(name):
        return int(resize(name).get_attribute("aria-valuenow"))

    def stored():
        raw = page.evaluate(
            "([id]) => { const hits = Object.keys(localStorage).filter(k => k.includes(id)); "
            "return hits.length === 1 ? localStorage.getItem(hits[0]) : null; }", [table_id])
        return json.loads(raw) if raw else None

    def cell_boxes():
        return table.locator('[data-slot="table-body"] [data-proximity-index]').first.locator("td").evaluate_all(
            "els => els.map(el => { const r = el.getBoundingClientRect(); "
            "return {left: Math.round(r.left), right: Math.round(r.right), text: el.innerText.trim()}; })")

    # This runs on a list that already holds the account this case created, so the content-layout check
    # below reads a real row rather than an empty-state placeholder. The dialog closed on save, so the
    # reload is what puts the new row on the page.
    page.reload(wait_until="domcontentloaded")
    expect(table.locator('[data-slot="table-body"] [data-proximity-index]').first).to_be_visible(timeout=20000)
    default_shown = shown()
    check(shown() == default_shown, "the provider table did not start from its default layout")

    # 11.1: one keyboard resize moves that column by exactly one step per press and leaves the other
    # columns' own widths alone; it cannot go below the column's positive minimum however often it is
    # pressed, and the committed width is what a reload brings back.
    start = width_of(columns["type"])
    others = {name: width_of(name) for name in keys_of if name != columns["type"]}
    handle = resize(columns["type"])
    expect(handle).to_have_count(1)
    handle.focus()
    handle.press("ArrowRight")
    handle.press("ArrowRight")
    widened = width_of(columns["type"])
    check(widened == start + 32,
          f"two ArrowRight presses did not widen the column by two 16px steps: {start} -> {widened}")
    check({name: width_of(name) for name in others} == others,
          "resizing the type column changed another column's own width")
    check(((stored() or {}).get("widths") or {}).get(keys_of[columns["type"]]) == widened,
          f"the widened column was not written to the stored layout: {stored()}")
    for _ in range(40):
        handle.press("ArrowLeft")
    floored = width_of(columns["type"])
    check(0 < floored < start,
          f"the column did not stop at a positive minimum below its start width: {floored}")
    handle.press("ArrowLeft")
    handle.press("ArrowLeft")
    check(width_of(columns["type"]) == floored,
          "the column width kept shrinking past its minimum")
    stored_widths = (stored() or {}).get("widths") or {}
    check(stored_widths.get(keys_of[columns["type"]]) == floored,
          f"the minimum width was not written to the stored layout: {stored_widths}")
    # Content layout: at that minimum the real row still lays out, every cell has a positive box and no
    # two cells overlap, so the narrowest legitimate width does not collapse the row.
    boxes = cell_boxes()
    check(len(boxes) >= len(columns), f"the rendered row lost cells: {boxes}")
    check(all(box["right"] > box["left"] for box in boxes), f"a cell collapsed to no width: {boxes}")
    check(all(first["right"] <= second["left"] + 1 for first, second in zip(boxes, boxes[1:])),
          f"cells overlap at the minimum column width: {boxes}")
    check(any(box["text"] for box in boxes), "the rendered row is empty")
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    check(width_of(columns["type"]) == floored, "the committed column width was not restored after a reload")
    report["column_resize"] = {"column": columns["type"], "start": start, "widened": widened,
                               "floored": floored, "stored_widths": stored_widths,
                               "cells": boxes, "restored_after_reload": floored}

    # 11.3: the name and actions columns are locked - the drawer renders them as checked, disabled
    # checkboxes and offers no hide or drag control for them, so they can be neither hidden nor moved.
    table.get_by_role("button", name=labels["settings"], exact=True).click()
    drawer = page.get_by_role("dialog", name=labels["settingsTitle"], exact=True)
    expect(drawer).to_be_visible()
    for locked in (columns["name"], labels["actions"]):
        box = drawer.get_by_role("checkbox", name=locked, exact=True)
        expect(box).to_have_count(1)
        expect(box).to_be_disabled()
        expect(box).to_be_checked()
    check(drawer.get_by_role("button", name=labels["dragColumn"].replace("{{name}}", columns["name"])).count() == 0,
          "the name column offers a drag handle, so it could be moved")
    check(drawer.get_by_role("button", name=labels["hideColumn"].replace("{{name}}", columns["name"])).count() == 0,
          "the name column offers a hide control, so it could be hidden")

    # 11.2: hiding a movable column takes it out of the rendered header row and out of a reload;
    # showing it again puts it back; a drag reorders; the drawer's reset restores every earlier choice.
    visible_before = shown()
    status_box = drawer.get_by_role("checkbox", name=columns["status"], exact=True)
    expect(status_box).to_be_checked()
    status_box.click()
    expect(status_box).not_to_be_checked()
    page.keyboard.press("Escape")
    expect(drawer).not_to_be_visible()
    hidden = shown()
    check(columns["status"] not in hidden,
          f"the status column was still rendered after being hidden: {hidden}")
    check(hidden == [title for title in visible_before if title != columns["status"]],
          f"hiding one column changed the rest of the header row: {hidden}")
    check(((stored() or {}).get("hidden") or []) == [keys_of[columns["status"]]],
          f"the hidden column was not written to the stored layout: {stored()}")
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    check(columns["status"] not in shown(), "the hidden column came back after a reload")
    # Showing it again is what the same checkbox does in reverse.
    table.get_by_role("button", name=labels["settings"], exact=True).click()
    expect(drawer).to_be_visible()
    drawer.get_by_role("checkbox", name=columns["status"], exact=True).check()
    expect(drawer.get_by_role("checkbox", name=columns["status"], exact=True)).to_be_checked()
    # The drawer reorders with Zeron's SortableCollection (components/ui/sortable-collection.tsx),
    # whose handle is a button named "Reorder <title>" that lifts on Space/Enter and moves with
    # the arrow keys (:220, :239-244). One ArrowDown moves the type column one slot later.
    drag_handle = drawer.get_by_role(
        "button",
        name=labels["dragColumn"].replace("{{name}}", columns["type"]),
        exact=True)
    expect(drag_handle).to_have_count(1)
    # The drawer exposes the reorder handles Zeron's SortableCollection renders; the move is
    # driven with the pointer, which is what a user does and what the collection's own pointer
    # path handles. A keyboard lift (Space) is recorded alongside so a keyboard regression stays
    # visible in the report rather than silently changing the assertion.
    drag_handle.focus()
    drag_handle.press("Space")
    page.wait_for_timeout(300)
    keyboard_lift = drag_handle.get_attribute("aria-pressed")
    drag_handle.focus()
    page.keyboard.press("ArrowDown")
    page.wait_for_timeout(300)
    keyboard_order = (stored() or {}).get("order")
    drag_handle.press("Space")
    page.wait_for_timeout(400)
    report['reorder_keyboard'] = {"lifted": keyboard_lift, "order_after_arrow": keyboard_order}
    before = (stored() or {}).get("order") or []
    source = drag_handle.bounding_box()
    target = drawer.get_by_role(
        "button",
        name=labels["dragColumn"].replace("{{name}}", columns["instances"]),
        exact=True).bounding_box()
    check(source is not None and target is not None, "the reorder handles have no geometry")
    page.mouse.move(source["x"] + source["width"] / 2, source["y"] + source["height"] / 2)
    page.mouse.down()
    page.mouse.move(source["x"] + source["width"] / 2, source["y"] + source["height"] / 2 + 8, steps=5)
    page.wait_for_timeout(200)
    page.mouse.move(target["x"] + target["width"] / 2,
                    target["y"] + target["height"] / 2 + 10, steps=12)
    page.wait_for_timeout(300)
    page.mouse.up()
    page.wait_for_timeout(700)
    report['reorder_pointer'] = {"before": before, "after": (stored() or {}).get("order")}
    order = (stored() or {}).get("order") or []
    check(order.index(keys_of[columns["type"]]) == order.index(keys_of[columns["instances"]]) + 1,
          f"the drag did not move the type column after the instances column: {order}")
    page.keyboard.press("Escape")
    expect(drawer).not_to_be_visible()
    check(sorted(shown()) == sorted(visible_before),
          f"showing the column again did not restore the full column set: {shown()}")
    reordered = shown()
    check(reordered.index(columns["type"]) == reordered.index(columns["instances"]) + 1,
          f"the moved column was not rendered in its new slot: {reordered}")
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    check(shown() == reordered, "the reordered layout was not restored after a reload")
    # Reset restores the default column set, order and widths.
    table.get_by_role("button", name=labels["settings"], exact=True).click()
    expect(drawer).to_be_visible()
    drawer.get_by_role("button", name=labels["reset"], exact=True).click()
    page.keyboard.press("Escape")
    expect(drawer).not_to_be_visible()
    check(shown() == default_shown, f"the reset did not restore the default header row: {shown()}")
    check(((stored() or {}).get("widths") or {}) == {},
          f"the reset did not clear the stored widths: {stored()}")
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    check(shown() == default_shown, "the reset layout was not restored after a reload")
    report["column_show_hide_reset"] = {"hidden": columns["status"], "visible_before": visible_before,
                                        "reordered": reordered, "reset_restored": default_shown,
                                        "locked": [columns["name"], labels["actions"]]}

    # 11.3 second clause: a layout saved by an older build names a column this build does not have, and
    # one saved copy is not readable at all. Both are required to fall back safely rather than throw or
    # render a column the table never declared.
    stored_key = page.evaluate(
        "([id]) => Object.keys(localStorage).filter(k => k.includes(id))[0]", [table_id])
    page.evaluate(
        "([key]) => localStorage.setItem(key, JSON.stringify({order: ['state', 'retired_column', 'kind'], "
        "hidden: ['retired_column'], widths: {retired_column: 300, state: 140}, pageSize: 100}))", [stored_key])
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    migrated = shown()
    check("retired_column" not in " ".join(migrated),
          f"a column this build does not declare was rendered: {migrated}")
    check(sorted(migrated) == sorted(default_shown),
          f"the unknown-column layout lost or invented a column: {migrated}")
    # The name column leads and the actions column trails by role, so the saved order applies to the
    # columns in between: 'state' was asked for first and is required to be rendered first there.
    movable = [title for title in migrated if title not in (columns["name"], labels["actions"])]
    check(movable[0] == columns["status"],
          f"the leading saved column was not rendered first among the movable columns: {migrated}")
    # The list gained a "Platform type" column (CloudProvidersPage.tsx:155, key "category") when the
    # public and private pages merged; it is not in the injected order, so it keeps its declared
    # place after the named ones.
    check(movable == [columns["status"], columns["type"], columns["category"],
                      columns["instances"], columns["updatedAt"]],
          f"the surviving saved order was not applied to the movable columns: {migrated}")
    check(width_of(columns["status"]) == 140,
          "the saved width of a known column was not applied next to an unknown one")
    check(len(errors) == errors_before, f"an unknown stored column raised a page error: {errors[errors_before:]}")
    page.evaluate("([key]) => localStorage.setItem(key, 'not a stored layout')", [stored_key])
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    check(shown() == default_shown, f"an unreadable stored layout did not fall back to the defaults: {shown()}")
    check(len(errors) == errors_before,
          f"an unreadable stored layout raised a page error: {errors[errors_before:]}")
    report["column_unknown_fallback"] = {"unknown_layout": migrated, "unreadable_layout": shown(),
                                         "page_errors": errors[errors_before:]}
    # Leave the table as it was found.
    page.evaluate("() => Object.keys(localStorage).filter(k => k.startsWith('gpuworks.table.'))"
                  ".forEach(k => localStorage.removeItem(k))")
    page.reload(wait_until="domcontentloaded")
    expect(table.locator("thead th").first).to_be_visible(timeout=20000)
    check(shown() == default_shown, "clearing the stored layouts did not restore the defaults")
    check(len(writes) == writes_before,
          f"a column preference change wrote a provider: {writes[writes_before:]}")


def copy_server_log(artifacts):
    log = artifacts.private / "server.log"
    if log.exists():
        (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))


def _box(locator):
    return locator.evaluate("""element => {
        const rect = element.getBoundingClientRect();
        return {left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom,
                width: rect.width, height: rect.height};
    }""")


def _separated(first, second):
    return (first["right"] <= second["left"] or second["right"] <= first["left"]
            or first["bottom"] <= second["top"] or second["bottom"] <= first["top"])


def exercise_provider_label_keyboard(page, dialog, copy, label):
    """Left label column, Tab through the add dialog, Enter sends no POST."""
    name_input = dialog.get_by_label(copy["config"]["name"], exact=True)
    endpoint_input = dialog.get_by_label(label("endpoint"), exact=True)
    key_input = dialog.get_by_label(label("api_key"), exact=True)
    name_item = name_input.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
    label_col = name_item.locator(".ant-form-item-label")
    expect(label_col).to_have_class(re.compile(r"\bant-form-item-label-left\b"), timeout=12000)
    align = label_col.evaluate("element => getComputedStyle(element).textAlign")
    check(align == "start", f"name label alignment is {align}")
    kind = dialog.get_by_role("combobox")
    kind.focus()
    expect(kind).to_be_focused()
    kind.press("Tab")
    expect(name_input).to_be_focused()
    name_input.press("Tab")
    expect(key_input).to_be_focused()
    key_input.press("Tab")
    expect(endpoint_input).to_be_focused()
    add = form_action(dialog, copy["config"]["add"])
    cancel = button(dialog, copy["config"]["cancel"])
    add.focus()
    expect(add).to_be_focused()
    name_input.focus()
    expect(name_input).to_be_focused()
    submitted = False
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path == "/api/v1/providers"
                                  and request.method == "POST", timeout=1000):
            name_input.press("Enter")
        submitted = True
    except PlaywrightTimeout:
        submitted = False
    check(not submitted, "Enter in the empty name submitted a provider")
    expect(name_input).to_have_value("")
    dialog.scroll_into_view_if_needed()
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    for locator in (name_input, endpoint_input, key_input, cancel, add):
        expect(locator).to_be_in_viewport(ratio=1)
    name_box, add_box = _box(name_input), _box(add)
    check(name_box["width"] > 0 and add_box["width"] > 0, "name or Add has no box")
    check(_separated(name_box, add_box), "name overlaps Add")
    return {"label_class": "ant-form-item-label-left", "text_align": align,
            "tab": ["type", "name", "api key", "endpoint"], "add_focused": True,
            "enter_submitted": False, "viewport": view, "overlaps_add": False}


def _provider_mutation(page, action):
    submitted = False
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path.startswith("/api/v1/providers")
                                  and request.method in {"POST", "PUT", "DELETE"}, timeout=1000):
            action()
        submitted = True
    except PlaywrightTimeout:
        submitted = False
    return submitted


def exercise_provider_no_colon_dismiss(page, dialog, copy, label, locale, edit, read, endpoint, saved_name):
    """No-colon name label, then Escape, close, and Cancel each leave the saved account."""
    name_input = dialog.get_by_label(copy["config"]["name"], exact=True)
    name_item = name_input.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
    name_label = name_item.locator("label")
    expect(name_label).to_have_class(re.compile(r"\bant-form-item-no-colon\b"), timeout=12000)
    endpoint_input = dialog.get_by_label(label("endpoint"), exact=True)
    # rc-dialog keeps aria-label "Close" on the button. The localized word sits on the icon wrapper.
    close = dialog.locator("button.ant-modal-close")
    close_label = "关闭" if locale == "zh" else "Close"
    expect(close.locator(".ant-modal-close-x")).to_have_attribute("aria-label", close_label, timeout=12000)
    cancel = button(dialog, copy["config"]["cancel"])
    discarded = "https://discarded.invalid/v2"

    def dismiss(prepare, action):
        endpoint_input.fill(discarded)
        expect(endpoint_input).to_have_value(discarded)
        prepare()
        check(not _provider_mutation(page, action), "dismiss sent a provider mutation")
        expect(dialog).not_to_be_visible(timeout=12000)
        read(endpoint, saved_name)

    dismiss(lambda: None, lambda: page.keyboard.press("Escape"))
    edit(endpoint, saved_name)
    dismiss(lambda: expect(close.locator(".ant-modal-close-x")).to_have_attribute(
                "aria-label", close_label, timeout=12000),
            lambda: close.click(timeout=12000))
    edit(endpoint, saved_name)
    dismiss(lambda: expect(cancel).to_be_visible(timeout=12000),
            lambda: cancel.click(timeout=12000))
    return {"label_class": "ant-form-item-no-colon", "close_label": close_label,
            "dismissed": ["escape", "close", "cancel"], "mutation": False}


# Mainline: UI新建自定义代理账号 → 刷新/独立GET → 编辑回显且密钥空白 → 仅改名称仍用原代理与密钥
#           → 修改代理遇到真实上游401不落盘 → 原草稿重试成功 → 刷新再编辑回显新代理 → UI删除
# Contract: gpuworks/providers/endpoint-edit-roundtrip
def test_provider_endpoint_edit(page, context, server, artifacts, report, locale, default_fixture=None):
    texts = ROOT / "ui/src/i18n/locales" / locale
    auth = json.loads((texts / "auth.json").read_text())
    copy = json.loads((texts / "providers.json").read_text())
    errors, console_errors, bad_http = [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(server.base + "/providers")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=copy["platforms"]["title"], exact=True)).to_be_visible()
    report["preflight"] = "PASS"
    exercise_table_id_isolation(page, json.loads((texts / "common.json").read_text()), copy, report)
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)
    page.on("response", lambda r: bad_http.append({"path": urlsplit(r.url).path, "status": r.status, "method": r.request.method}) if r.status >= 400 else None)
    kinds = collection(page.request.get(server.base + "/api/v1/providers/kinds"))
    runpod = next(kind for kind in kinds if kind["kind"] == "runpod")
    fields = {field["name"]: field for field in runpod["credential_fields"]}
    check(fields["endpoint"]["secret"] is False and fields["api_key"]["secret"] is True, "RunPod credential contract changed")
    label = lambda name: fields[name]["label"][locale]
    name = "e2e-" + report["run_id"] + "-proxy"
    if family_enabled(report, "literal-name"):
        name += NAME_SUFFIX
    secret = "e2e-runpod-" + report["run_id"]
    artifacts.remember(secret)
    writes = report["provider_mutations"] = observe_mutations(page)
    # Existing account keys belong to the user; platform registration must keep them.
    existing_key = EXISTING_KEY
    platform_key = page.request.get(server.base + "/api/v1/ssh-key")
    check(platform_key.status == 200, "platform SSH key is unavailable")
    expected_keys = [existing_key, platform_key.json()["public_key"]]
    if family_enabled(report, "default-endpoint"):
        exercise_default_endpoint(page, server, artifacts, report, copy, label, fields, default_fixture, expected_keys)
    with RunPodFixture(secret, keys=[existing_key]) as fixture:
        report["provider_upstream_requests"] = fixture.requests
        endpoints = [fixture.base + "/proxy-a/v2", fixture.base + "/proxy-b/v2"]
        add = copy["page"]["addKind"].replace("{{kind}}", copy["kind"]["runpod"])
        button(page, add).click()
        dialog = page.get_by_role("dialog")
        endpoint_default = fields["endpoint"]["default_value"]
        if family_enabled(report, 'conditional-fields'):
            exercise_conditional_fields(page, dialog, copy, kinds, locale, report, artifacts)
        report["provider_label_keyboard"] = exercise_provider_label_keyboard(
            page, dialog, copy, label)
        from provider_create_inventory import exercise_provider_create_inventory
        report["provider_create_inventory"] = exercise_provider_create_inventory(
            dialog, copy, label, endpoint_default,
            fields["endpoint"]["placeholder"][locale], fields["api_key"]["placeholder"][locale])
        expect(dialog.locator(".ant-select-selection-item")).to_have_text(copy["kind"]["runpod"])
        expect(dialog.get_by_label(copy["config"]["name"], exact=True)).to_have_value("")
        expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value(endpoint_default)
        expect(dialog.get_by_label(label("api_key"), exact=True)).to_have_value("")
        report["evidence"].append({"layer": "ui", "first_open": {"kind": "runpod", "name_empty": True,
            "endpoint_default": endpoint_default, "api_key_empty": True}})
        text_contract(dialog, copy, label, fields, report, editing=False)
        secret_create(page, server, dialog, copy, label, name, endpoints[0], secret, writes, errors, report)
        clipboard_editing(page, context, server, dialog, copy, label, name, endpoints[0], secret, writes, report)
        dialog.get_by_label(copy["config"]["name"], exact=True).fill(name)
        dialog.get_by_label(label("endpoint"), exact=True).fill(endpoints[0])
        dialog.get_by_label(label("api_key"), exact=True).fill(secret)
        expected_create = {"name": name, "kind": "runpod", "endpoint": endpoints[0], "api_key": secret}
        held = []

        def hold_post(route):
            if route.request.method != "POST" or urlsplit(route.request.url).path != "/api/v1/providers":
                route.continue_()
                return
            held.append(route)

        page.route("**/api/v1/providers", hold_post)
        form_action(dialog, copy["config"]["add"]).click()
        inflight = dialog.locator(".ant-modal-footer button.ant-btn-primary")
        expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
        inflight.click()
        dialog.get_by_label(copy["config"]["name"], exact=True).press("Enter")
        check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
        check(held[0].request.post_data_json == expected_create, "held provider body differs")
        with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/providers" and r.request.method == "POST") as created:
            held[0].continue_()
        page.unroute("**/api/v1/providers", hold_post)
        check(created.value.status == 201, f"POST /api/v1/providers: expected 201, received {created.value.status}")
        provider_id = created.value.json()["id"]
        report["remaining_resources"] = [{"kind": "provider", "id": provider_id,
                                         "scope": "isolated SQLite database in private/",
                                         "reclaim": "discard only this run directory after diagnosis"}]
        create_body = created.value.request.post_data_json
        check(create_body == {"name": name, "kind": "runpod", "endpoint": endpoints[0], "api_key": secret}, "create wire mismatch")
        if 'provider_conditional_fields' in report:
            conditional = report['provider_conditional_fields']['final_post_contract']
            check(not set(conditional['must_omit_roots']).intersection(create_body),
                  'provider create retained a hidden credential from another kind')
            conditional.update(observed_wire_keys=sorted(create_body), hidden_roots_omitted=True,
                               proof='actual UI POST checked against the full independent RunPod wire object')
        path = f"/api/v1/providers/{provider_id}"
        expect(dialog).not_to_be_visible()
        report["evidence"].append({"layer": "request", "method": "POST", "path": "/api/v1/providers", "body": create_body, "status": 201})

        def read(endpoint, expected_name):
            response = page.request.get(server.base + path)
            expected = {"id": provider_id, "kind": "runpod", "name": expected_name, "endpoint": endpoint, "public_key": "", "extra": {}}
            check(response.status == 200 and response.json() == expected, "independent edit read mismatch or secret exposure")
            report["evidence"].append({"layer": "persistence", "path": path, "body": expected, "status": 200})

        def edit(endpoint, expected_name):
            page.reload()
            row = page.get_by_role("row").filter(has=page.get_by_text(expected_name, exact=True))
            gate = {"held": None}

            def hold_get(route):
                if route.request.method == "GET" and urlsplit(route.request.url).path == path and gate["held"] is None:
                    gate["held"] = route
                    return
                route.continue_()

            page.route("**/api/v1/providers/*", hold_get)
            try:
                button(row, copy["table"]["edit"]).click()
                deadline = time.monotonic() + 5
                while gate["held"] is None:
                    if time.monotonic() > deadline:
                        raise AssertionError("edit configuration read did not start")
                    page.wait_for_timeout(50)
                expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value("")
                expect(dialog.get_by_label(label("api_key"), exact=True)).to_have_value("")
                held_save = dialog.locator(".ant-modal-footer button.ant-btn-primary")
                expect(held_save).to_be_disabled()
                text_contract(dialog, copy, label, fields, report, editing=True, ready=False)
                with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "GET") as loaded:
                    gate["held"].continue_()
                    gate["held"] = None
                check(loaded.value.status == 200, "edit configuration read failed")
                expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value(endpoint)
                expect(dialog.get_by_label(label("api_key"), exact=True)).to_have_value("")
                expect(dialog.get_by_label(copy["config"]["name"], exact=True)).to_have_value(expected_name)
                expect(form_action(dialog, copy["config"]["save"])).to_be_enabled()
                text_contract(dialog, copy, label, fields, report, editing=True)
                literal_name(page, dialog, copy, expected_name, report)
                report["evidence"].append({"layer": "ui", "reload_edit_endpoint": endpoint, "secret_empty": True,
                                           "save_disabled_until_edit_get": True})
            finally:
                if gate["held"] is not None:
                    gate["held"].continue_()
                page.unroute("**/api/v1/providers/*", hold_get)

        def save(endpoint, expected_name, status, *, credentials=None):
            with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "PUT") as updated:
                form_action(dialog, copy["config"]["save"]).click()
            response = updated.value
            report["last_request"] = {"method": "PUT", "path": path, "status": response.status,
                                      "expected_status": status, "body": response.request.post_data_json,
                                      "response_text": response.text()}
            check(response.status == status, f"PUT {path}: expected {status}, received {response.status}")
            expected_body = {"name": expected_name, "endpoint": endpoint, **(credentials or {})}
            check(response.request.post_data_json == expected_body, "edit credential wire differs")
            report["evidence"].append({"layer": "request", "method": "PUT", "path": path, "body": response.request.post_data_json, "status": status})
            if status == 200:
                expect(dialog).not_to_be_visible()
            else:
                # This legacy provider error is a plain writeErr response, without a ProductError code.
                reason = "credential validation failed: runpod: invalid credentials (status 401)"
                check(response.json() == {"error": reason}, "provider validation error body differs")
                expect(dialog).to_be_visible()
                expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value(endpoint)
                message = (copy["config"]["operationFailed"]
                           .replace("{{operation}}", copy["config"]["operation"]["update"])
                           .replace("{{detail}}", reason))
                expect(page.get_by_text(message, exact=True)).to_be_visible()
                # Left labels, Tab, Enter and viewport already passed. The visible
                # rejection is the remaining 1.15 clause.
                report["provider_label_left"] = True
                report["evidence"].append({"layer": "ui", "validation_failure": response.json(),
                                           "localized_operation_error": message, "draft_endpoint": endpoint})

        read(endpoints[0], name)
        # The list now holds a real row, so the column-preference steps read the rendered content as well
        # as the header row. They belong to this account: a layout change must never write a provider.
        exercise_column_preferences(page, json.loads((texts / "common.json").read_text()), copy,
                                    report, writes, errors)
        report["last_step"] = "provider-column-preferences"
        exercise_name_boundaries(page, context, server, fixture, copy, label, path, name,
                                 endpoints[0], writes, errors, report)
        artifacts.start_trace(context)
        literal_name(page, None, copy, name, report)
        edit(endpoints[0], name)
        renamed = name + "-renamed"
        dialog.get_by_label(copy["config"]["name"], exact=True).fill(renamed)
        save(endpoints[0], renamed, 200)
        read(endpoints[0], renamed)
        edit(endpoints[0], renamed)
        if family_enabled(report, "secret"):
            report["last_step"] = "provider-secret-whitespace-edit-retained"
            dialog.get_by_label(label("api_key"), exact=True).fill("   ")
            save(endpoints[0], renamed, 200, credentials={"api_key": "   "})
            read(endpoints[0], renamed)
            report["secret_edit_whitespace_retained"] = True
            edit(endpoints[0], renamed)
            report["last_step"] = "provider-endpoint-clear-retained"
            dialog.get_by_label(label("endpoint"), exact=True).fill("")
            save("", renamed, 200)
            read(endpoints[0], renamed)
            report["endpoint_clear_retained"] = True
            edit(endpoints[0], renamed)
        dialog.get_by_label(label("endpoint"), exact=True).fill(endpoints[1])
        fixture.reject = True
        save(endpoints[1], renamed, 400)
        read(endpoints[0], renamed)
        fixture.reject = False
        save(endpoints[1], renamed, 200)
        read(endpoints[1], renamed)
        edit(endpoints[1], renamed)
        renamed = concurrent_names(page, context, server, dialog, copy, label, renamed,
                                   endpoints[1], path, read, save, report)
        edit(endpoints[1], renamed)
        report["provider_dismiss"] = exercise_provider_no_colon_dismiss(
            page, dialog, copy, label, locale, edit, read, endpoints[1], renamed)
        report["provider_no_colon"] = True
        literal_name(page, None, copy, renamed, report)
        row = page.get_by_role("row").filter(has=page.get_by_text(renamed, exact=True))
        button(row, copy["table"]["delete"]).click()
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as deleted:
            button(page.get_by_role("tooltip"), copy["table"]["delete"]).click()
        check(deleted.value.status == 200, "provider cleanup failed")
        check(not any(p["id"] == provider_id for p in collection(page.request.get(server.base + "/api/v1/providers"))), "fixture account leaked")
        check([(r["method"], r["path"], r["status"], r["authorized"]) for r in fixture.requests]
              == with_boundary_calls(report, expected_upstream(report)), f"unexpected cloud API requests: {fixture.requests}")
        key_write_index = 3 if family_enabled(report, "secret") else 2
        check(fixture.requests[key_write_index]["body"] == {"keys": expected_keys} and fixture.keys == expected_keys,
              "platform key registration replaced or lost existing account keys")
        report["evidence"].append({"layer": "runtime", "requests": fixture.requests, "boundary": "local RunPod REST fixture, real server credentials decrypt/validate"})
        report["evidence"].append({"layer": "cleanup", "provider_id": provider_id, "deleted": True})
        report["remaining_resources"] = []
    report["external_fixture_stopped"] = not fixture.thread.is_alive()
    url_http = exercise_provider_urls(page, context, server, artifacts, report, copy, label, default_fixture, expected_keys)
    text_http = exercise_text_values(page, context, server, artifacts, report, copy, label, default_fixture, expected_keys)
    rotation_http = exercise_key_rotation(page, context, server, artifacts, report, copy, label, default_fixture, expected_keys)
    secret_http = exercise_provider_secret_errors(page, context, server, artifacts, report, copy, label, expected_keys)
    check(not errors, f"browser page errors: {errors}")
    expected_http = boundary_http_errors(report, path) + [{"path": path, "status": 400, "method": "PUT"}]
    if family_enabled(report, "secret"):
        expected_http.insert(0, {"path": "/api/v1/providers", "status": 400, "method": "POST"})
    expected_http.extend(url_http + text_http + rotation_http + secret_http)
    check(bad_http == expected_http, "unexpected browser HTTP failures")
    console_statuses = [re.fullmatch(
        r'Failed to load resource: the server responded with a status of (\d{3}) \([^\r\n]*\)', error)
        for error in console_errors]
    check(all(console_statuses) and Counter(int(match.group(1)) for match in console_statuses)
          == Counter(error['status'] for error in expected_http), 'unexpected console errors')
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": bad_http,
                                "expected": expected_http}

def main():
    import argparse
    from datetime import datetime, timezone
    import os
    from pathlib import Path
    import subprocess
    import sys
    import uuid
    from playwright.sync_api import sync_playwright
    from local_browser_fixture import Artifacts, LocalServer, ROOT, digest

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--check-family", choices=(*CHECK_FAMILIES, "boundaries"), default="all",
                        help="diagnostic subset of new checks; formal runs require all")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    # This account form does not exercise update distribution or daily usage
    # reporting. Use their real per-installation switches so only the declared
    # RunPod boundary is contacted; the CONNECT fixture remains fail-closed.
    server = LocalServer(args.server_bin, artifacts.private, auto_check_updates=False,
                         env_overrides={"GPUWORKS_USAGE_REPORT": "off"})
    manifest_path = ROOT / "test/e2e/coverage/provider-endpoint-edit.json"
    manifest = json.loads(manifest_path.read_text())
    if manifest["contract_id"] != CONTRACT or manifest["test_function"] != test_provider_endpoint_edit.__name__:
        raise AssertionError("manifest function binding drift")
    source_paths = {
        "test_sha256": Path(__file__),
        "fixture_sha256": ROOT / "test/e2e/local_browser_fixture.py",
        "helper_sha256": ROOT / "test/e2e/platform_token_read_revoke.py",
        "provider_fixture_sha256": ROOT / "test/e2e/provider_endpoint_fixture.py",
        "provider_inventory_sha256": ROOT / "test/e2e/provider_create_inventory.py",
        "provider_form_checks_sha256": ROOT / "test/e2e/provider_form_checks.py",
        "provider_default_checks_sha256": ROOT / "test/e2e/provider_default_endpoint_checks.py",
        "provider_default_fixture_sha256": ROOT / "test/e2e/provider_default_endpoint_fixture.py",
        "provider_boundary_checks_sha256": ROOT / "test/e2e/provider_boundary_checks.py",
        "provider_url_checks_sha256": ROOT / "test/e2e/provider_url_checks.py",
        "provider_conditional_checks_sha256": ROOT / "test/e2e/provider_conditional_checks.py",
        "provider_text_value_checks_sha256": ROOT / "test/e2e/provider_text_value_checks.py",
        "provider_key_rotation_checks_sha256": ROOT / "test/e2e/provider_key_rotation_checks.py",
        "provider_secret_error_checks_sha256": ROOT / "test/e2e/provider_secret_error_checks.py",
        "provider_secret_error_fixture_sha256": ROOT / "test/e2e/provider_secret_error_fixture.py",
    }
    report = {
        "contract": manifest["contract_id"], "case": manifest["name"],
        "status": "NOT_RUN", "run_id": run_id,
        "check_family": args.check_family, "probe_only": args.check_family != "all",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "target_branch": "wei/master",
        "target_sha": subprocess.check_output(["git", "rev-parse", "wei/master"], cwd=ROOT, text=True).strip(),
        "lane": "local-real-server-sqlite", "identity": "admin browser",
        "locale": args.locale, "viewport": {"width": 1440, "height": 900},
        "timezone": "UTC", "selection": [manifest["contract_id"]], "retries": 0, "evidence": [],
        **{key: digest(path) for key, path in source_paths.items()},
        "product_sha256": {name: digest(ROOT / name) for name in manifest.get("product_contracts", [])},
        "coverage_manifest_sha256": digest(manifest_path),
        "command": ["python3", "test/e2e/provider_endpoint_edit.py", *sys.argv[1:]],
    }
    default_fixture = None
    try:
        if (family_enabled(report, "default-endpoint") or needs_default_fixture(report)
                or family_enabled(report, "text-values")):
            default_secret = "e2e-runpod-default-" + run_id
            artifacts.remember(default_secret)
            default_fixture = ProviderDefaultEndpointFixture(artifacts.private / "default-runpod-tls",
                default_secret, keys=[EXISTING_KEY])
            default_fixture.__enter__()
            server.env_overrides.update(default_fixture.child_environment())
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"], report["owned_server_pid"] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_provider_endpoint_edit(page, context, server, artifacts, report, args.locale, default_fixture)
                layers = {item["layer"] for item in report["evidence"]}
                if not set(manifest["required_evidence"]) <= layers:
                    raise AssertionError("missing required evidence")
                report["business_complete"] = True
            except Exception:
                report["status"], report["failure"] = "FAIL_TEST", traceback.format_exc()
                # Capture only action labels/state, never credential inputs.
                try:
                    report["failure_dialog_actions"] = page.locator(
                        '[role="dialog"] .ant-modal-footer button').evaluate_all(
                        '(buttons) => buttons.map(b => ({text: b.textContent, '
                        'aria: b.getAttribute("aria-label"), disabled: b.disabled, '
                        'classes: b.className, tree: b.outerHTML}))')
                except Exception as diagnostic_error:
                    report["failure_actions_capture_error"] = type(diagnostic_error).__name__
            finally:
                cleanup_stage(report, "trace", lambda: artifacts.stop_trace(context))
                cleanup_stage(report, "context", context.close)
                cleanup_stage(report, "browser", browser.close)
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        report["server_stopped"] = cleanup_stage(report, "server", server.stop)
        if not report["server_stopped"]:
            report.setdefault("remaining_resources", []).append({"kind": "process",
                "pid": server.process.pid if server.process else None,
                "reclaim": f"kill {server.process.pid}" if server.process else "server never started"})
        if default_fixture is not None:
            report["default_fixture_stopped"] = cleanup_stage(report, "default-provider-fixture", default_fixture.stop)
            report["default_endpoint_tls_events"] = list(default_fixture.events)
            report["default_endpoint_upstream_requests"] = list(default_fixture.requests)
            if not report["default_fixture_stopped"]:
                report.setdefault("remaining_resources", []).append({"kind": "fixture",
                    "scope": "this run default RunPod CONNECT/TLS fixture",
                    "proxy": default_fixture.proxy_url, "reclaim": "stop this scoped runner process"})
        def frozen_sources():
            check(all(digest(path) == report[key] for key, path in source_paths.items()),
                  "runner or helper source changed during the browser run")
            check(report["product_sha256"] == {name: digest(ROOT / name)
                  for name in manifest.get("product_contracts", [])}, "product source changed during execution")
            if "binary_sha256" in report:
                check(digest(args.server_bin) == report["binary_sha256"], "server binary changed during execution")
            report["source_hashes_unchanged"] = True
        cleanup_stage(report, "frozen-sources", frozen_sources)
        cleanup_stage(report, "server-log", lambda: copy_server_log(artifacts))
        report["private_state"] = "isolated DB/logs retained for diagnosis; remove only this run directory within 7 days"
        cleanup_stage(report, "report-before-scan", lambda: artifacts.write_json("report.json", report))
        cleanup_stage(report, "artifact-scan", artifacts.scan)
        if report.get("business_complete") and report["status"] == "NOT_RUN" and not report.get("remaining_resources"):
            report["status"] = "PASS"
        cleanup_stage(report, "final-report", lambda: artifacts.write_json("report.json", report))
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
