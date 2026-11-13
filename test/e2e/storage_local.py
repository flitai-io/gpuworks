#!/usr/bin/env python3
"""Storage / local storage form, conditional fields and lifecycle.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-storage-local
After build: python3 test/e2e/storage_local.py --locale en
Requires Python Playwright and system Chrome. A local storage record is a
control-plane object; this case does not claim an agent trial mount, capacity
or model-file access. No GPU or container is required.
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
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/storage/local-create-delete"
CASE = "存储 / 本地存储表单与生命周期 / 类型条件字段、字段负例、创建、刷新与删除"
STORAGES = "/api/v1/storages"


def strings(locale):
    storage = json.loads((ROOT / "ui/src/i18n/locales" / locale / "storage.json").read_text())
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return storage, common, auth


def format_text(template: str, **values) -> str:
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", str(value))
    return template


def action(scope, label):
    # AntD icons contribute an accessible-name prefix (for example
    # "plus Add storage"), so match the label inside the accessible name.
    accessible = r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile(accessible)).first


def exercise_add_dialog(page, add, storage, posts, report):
    """Add dialog identity, layout, focus, and dismissals. No storage POST."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    title = storage["form"]["addTitle"]
    parent = page.get_by_role("heading", name=storage["page"]["title"], exact=True)

    def writes():
        return len(posts)

    def open_add():
        add.click()
        dialog = page.get_by_role("dialog", name=title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = open_add()
    assert_dialog_shell(page, dialog, dialog.get_by_label(storage["form"]["name"], exact=True),
                        button(dialog, storage["form"]["save"]))
    dialog = assert_nested_dismissals(
        dialog, parent, open_add,
        [("cancel", cancel_button(storage["form"]["cancel"])), ("escape", escape),
         ("close", close_button), ("mask", mask)],
        writes, add)
    cancel_button(storage["form"]["cancel"])(dialog)
    expect(dialog).to_be_hidden()
    expect(parent).to_be_visible()
    check(writes() == 0, "add dialog dismissal sent POST")
    report["evidence"].append({"layer": "ui", "add_dialog": title,
        "dismissals": ["cancel", "escape", "close", "mask"], "post_requests": 0})


def confirm_popconfirm(page):
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_be_visible(timeout=5000)
    popconfirm.locator(".ant-btn-primary").click()


def exercise_name_taken(page, server, storage, name, path, report, bad_http, console_errors):
    """A storage name is unique: the second create is refused and recovers.

    storages.go maps store.ErrStorageNameTaken to 409 STORAGE_NAME_TAKEN, and the
    lookup is an exact name match, so the collision has to reuse the same name.
    """
    # The edit flow leaves its detail drawer open, which would sit over the add
    # dialog's footer, so close it before opening the add dialog.
    page.goto(server.base + "/storage")
    for _ in range(3):
        closer = page.locator(".ant-drawer-close").first
        if closer.count() and closer.is_visible():
            closer.click()
            # The detail drawer unmounts after its close transition finishes, so wait for the
            # drawer to be gone rather than for a fixed delay.
            expect(page.locator(".ant-drawer-open")).to_have_count(0)
        else:
            break
    expect(page.locator(".ant-drawer-open")).to_have_count(0)
    add = action(page, storage["page"]["add"])
    title = storage["form"]["addTitle"]
    existing = page.request.get(server.base + STORAGES).json()
    names = [item.get("name") for item in existing.get("items") or existing or []]
    check(name in names, f'fixture storage {name!r} is missing from the list: {names}')

    add.click()
    dialog = page.get_by_role("dialog", name=title, exact=True)
    expect(dialog).to_be_visible()
    name_input = dialog.get_by_label(storage["form"]["name"], exact=True)
    name_input.fill(name)
    dialog.get_by_label(storage["form"]["path"], exact=True).fill(path + "-taken")
    save = button(dialog, storage["form"]["save"])
    # The dialog re-renders as the name is typed (the save button lives in the
    # footer array), so wait for the modal to settle before pressing.
    page.wait_for_timeout(400)
    expect(save).to_be_visible()
    expect(save).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as refused:
        save.click()
    result = refused.value
    check(result.status == 409, f'duplicate storage name HTTP {result.status}')
    code = result.json().get("code")
    check(code == "STORAGE_NAME_TAKEN", f'duplicate storage name code {code}')
    # The dialog stays open with the typed name, so the operator can fix it.
    page.wait_for_timeout(300)
    report["name_taken_dialog_after_409"] = dialog.is_visible()
    check(dialog.is_visible(), 'the add dialog closed itself on a refused save')
    check(name_input.input_value() == name, 'the typed name was cleared after the refusal')
    # The 409 is the expected outcome of this probe, so account for it rather than
    # letting it fail the run's unexpected-error check.
    from storage_delete_blocks import allow_http_status
    allow_http_status(bad_http, console_errors, 409)

    # A different name that is still free goes through, which shows the refusal was
    # about the collision and not about the record being unsavable.
    free = name + "-free"
    name_input.fill(free)
    dialog.get_by_label(storage["form"]["path"], exact=True).fill(path + "-free")
    page.wait_for_timeout(400)
    retry = button(dialog, storage["form"]["save"])
    expect(retry).to_be_visible()
    expect(retry).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as created:
        retry.click()
    check(created.value.status == 201, f'renamed create HTTP {created.value.status}')
    made = created.value.json()
    report["name_taken"] = {"refused_status": result.status, "code": code,
                            "typed_kept": True, "recovered_name": free,
                            "created_id": made.get("id")}
    report["evidence"].append({"layer": "request", "method": "POST", "path": STORAGES,
                               "status": result.status, "code": code,
                               "duplicate_of": name})
    # Clean up the recovered record so the case leaves nothing behind.
    removed = page.request.delete(server.base + STORAGES + f"/{made['id']}")
    check(removed.status in (200, 204), f'recovered storage delete HTTP {removed.status}')
    report["evidence"].append({"layer": "cleanup", "deleted": made["id"]})
    return report["name_taken"]


def exercise_full_local(page, server, storage, name, directory, report):
    """Second local create that fills the label optional. The label row is start-aligned."""
    full_name = name + "-full"
    full_path = directory + "-full"
    scope = storage["form"]["scope"]
    page.goto(server.base + "/storage")
    expect(page.get_by_role("heading", name=storage["page"]["title"], exact=True)).to_be_visible()
    action(page, storage["page"]["add"]).click()
    dialog = page.get_by_role("dialog", name=storage["form"]["addTitle"], exact=True)
    expect(dialog).to_be_visible()
    dialog.get_by_label(storage["form"]["name"], exact=True).fill(full_name)
    dialog.get_by_label(storage["form"]["path"], exact=True).fill(full_path)
    dialog.get_by_text(scope["labels"], exact=True).click()
    expect(dialog.get_by_role("radio", name=scope["labels"], exact=True)).to_be_checked()
    key = dialog.get_by_label(scope["labelKey"], exact=True)
    expect(key).to_be_visible()
    row = key.locator(
        "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-space ')][1]")
    report["last_step"] = "label-row-align"
    expect(row).to_have_count(1)
    expect(row).to_have_class(re.compile(r"\bant-space-align-start\b"))
    key.fill("team")
    dialog.get_by_label(scope["labelValue"], exact=True).fill("alpha")
    body = {
        "name": full_name,
        "type": "local",
        "all_nodes": False,
        "targets": [{"label_key": "team", "label_value": "alpha"}],
        "path": full_path,
    }
    report["last_step"] = "full-save"
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as created:
        button(dialog, storage["form"]["save"]).click()
    check(created.value.status == 201, f"full storage create HTTP {created.value.status}")
    sent = created.value.request.post_data_json
    check(sent == body, "full storage create body differs")
    storage_id = created.value.json()["id"]
    page.wait_for_url(re.compile(rf".*/storage\?storage={storage_id}(?:&|$)"), timeout=15000)
    expect(page.get_by_text(full_path, exact=True).first).to_be_visible()
    expect(page.get_by_text(storage["overview"]["scopeChosen"], exact=True).first).to_be_visible()
    expect(page.get_by_text("team=alpha", exact=True).first).to_be_visible()
    stored = page.request.get(server.base + STORAGES + f"/{storage_id}")
    check(stored.status == 200, f"full storage GET HTTP {stored.status}")
    doc = stored.json()
    labels = [item for item in doc.get("targets") or [] if item.get("label_key")]
    check(doc["name"] == full_name and doc["type"] == "local" and doc["path"] == full_path
          and doc["all_nodes"] is False and labels == body["targets"],
          "full storage GET differs")
    report["last_step"] = "full-deleted"
    page.goto(server.base + "/storage")
    row = page.locator("tr").filter(has=page.get_by_text(full_name, exact=True))
    expect(row).to_have_count(1)
    row.get_by_role("button", name=storage["actions"]["delete"], exact=True).click()
    expect(page.locator(".ant-popconfirm")).to_contain_text(
        format_text(storage["actions"]["deleteConfirm"], name=full_name))
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES + f"/{storage_id}"
                              and r.request.method == "DELETE") as deleted:
        confirm_popconfirm(page)
    check(deleted.value.status in (200, 204), f"full storage delete HTTP {deleted.value.status}")
    expect(row).to_have_count(0, timeout=15000)
    check(page.request.get(server.base + STORAGES + f"/{storage_id}").status == 404,
          "the full storage remains readable")
    return {
        "name": full_name,
        "path": full_path,
        "label": "team=alpha",
        "status": 201,
        "all_nodes": False,
        "label_row_align": "start",
    }


def exercise_local_inflight(page, modal, storage, name, path, body, bad_responses, console_errors, evidence):
    """One held create. Save is disabled, a forced second click sends nothing, and a 502 keeps the draft."""
    reason = "upstream fixture is unreachable"
    held = []

    def hold_create(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != STORAGES:
            route.continue_()
            return
        held.append(route)

    page.route("**" + STORAGES, hold_create)
    button(modal, storage["form"]["save"]).click()
    inflight = modal.locator(".ant-modal-footer button.ant-btn-primary")
    expect(inflight).to_be_disabled()
    inflight.click(force=True)
    modal.get_by_label(storage["form"]["name"], exact=True).focus()
    page.keyboard.press("Enter")
    check(len(held) == 1, "duplicate create while the first request is held")
    check(held[0].request.post_data_json == body, "held create body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES and r.request.method == "POST") as failed:
        held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
    page.unroute("**" + STORAGES, hold_create)
    check(failed.value.status == 502, "forced create failure did not surface")
    expect(modal).to_be_visible()
    expect(modal.get_by_label(storage["form"]["name"], exact=True)).to_have_value(name)
    expect(modal.get_by_label(storage["form"]["path"], exact=True)).to_have_value(path)
    expect(page.get_by_text(reason)).to_be_visible()
    expect(button(modal, storage["form"]["save"])).to_be_enabled()
    check(any(item.get("path") == STORAGES and item.get("status") == 502 for item in bad_responses), "502 was not recorded")
    check(any("502" in text for text in console_errors), "502 console line absent: " + str(console_errors))
    bad_responses[:] = [item for item in bad_responses if item.get("status") != 502]
    console_errors[:] = [text for text in console_errors if "502" not in text]
    evidence.append({"layer": "ui", "inflight_posts": 1, "save_disabled": True,
                     "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})


def exercise_edit_conflict(page, server, storage, common, storage_id, name, path_value, stale,
                           bad_responses, console_errors, evidence, report):
    """The open edit dialog sends its previewed revision. Another writer advances it first."""
    page.goto(server.base + f"/storage?storage={storage_id}")
    # The row and the detail drawer both carry Edit; take the drawer's header.
    header = page.locator(".ant-drawer-header").filter(has_text=name).first
    expect(header).to_be_visible()
    button(header, storage["actions"]["edit"]).click()
    scope = storage["form"]["scope"]
    dialog = page.get_by_role("dialog", name=storage["form"]["editTitle"].replace("{{name}}", name), exact=True)
    expect(dialog).to_be_visible()
    expect(dialog.get_by_label(storage["form"]["name"], exact=True)).to_have_value(name)
    expect(dialog.get_by_label(storage["form"]["path"], exact=True)).to_have_value(path_value)
    expect(dialog.get_by_role("radio", name=scope["all"], exact=True)).to_be_checked()
    expect(dialog.get_by_text(common["storageType"]["local"], exact=True)).to_be_visible()
    expect(dialog.locator("input[type=password]")).to_have_count(0)
    dialog.get_by_text(scope["labels"], exact=True).click()
    expect(dialog.get_by_role("radio", name=scope["labels"], exact=True)).to_be_checked()
    dialog.get_by_label(scope["labelKey"], exact=True).fill("team")
    dialog.get_by_label(scope["labelValue"], exact=True).fill("alpha")
    api_path = STORAGES + f"/{storage_id}"
    moved = page.request.fetch(server.base + api_path, method="PATCH",
                               headers={"content-type": "application/json"},
                               data=json.dumps({"expected_updated_at": stale}))
    preview = moved.json()
    check(moved.status == 200 and preview.get("updated_at") not in (None, stale),
          f"revision move HTTP {moved.status} {preview!r}")
    with page.expect_response(lambda r: urlsplit(r.url).path == api_path and r.request.method == "PATCH") as conflicted:
        button(dialog, storage["form"]["save"]).click()
    result = conflicted.value
    body = result.json()
    sent = result.request.post_data_json
    check(result.status == 409 and body.get("code") == "STORAGE_CONFIG_CHANGED",
          f"stale edit HTTP {result.status} {body!r}")
    check(sent.get("expected_updated_at") == stale and sent.get("all_nodes") is False
          and sent.get("targets") == [{"label_key": "team", "label_value": "alpha"}],
          f"stale edit body differs: {sent}")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_role("radio", name=scope["labels"], exact=True)).to_be_checked()
    expect(dialog.get_by_label(scope["labelKey"], exact=True)).to_have_value("team")
    expect(dialog.get_by_label(scope["labelValue"], exact=True)).to_have_value("alpha")
    report["last_step"] = "conflict-alert-icon"
    alert = dialog.get_by_role("alert").filter(has_text=common["apiError"]["STORAGE_CONFIG_CHANGED"])
    expect(alert).to_be_visible()
    expect(alert.locator(".ant-alert-icon")).to_have_count(1)
    expect(alert).not_to_have_class(re.compile(r"\bant-alert-no-icon\b"))
    report["conflict_alert_icon"] = True
    stayed = page.request.get(server.base + api_path)
    stored = stayed.json()
    check(stayed.status == 200 and stored.get("all_nodes") is True and stored.get("name") == name
          and stored.get("path") == path_value and stored.get("updated_at") == preview["updated_at"]
          and all(not item.get("label_key") for item in stored.get("targets") or []),
          f"stale edit changed the stored storage: {stored}")
    check(any(item.get("path") == api_path and item.get("status") == 409 for item in bad_responses),
          "409 was not recorded")
    check(any("409" in text for text in console_errors), "409 console line absent")
    bad_responses[:] = [item for item in bad_responses if not (item.get("path") == api_path and item.get("status") == 409)]
    console_errors[:] = [item for item in console_errors if "409" not in item]
    evidence.append({"layer": "request", "method": "PATCH", "path": api_path, "status": 409,
                     "code": "STORAGE_CONFIG_CHANGED", "body": sent})
    evidence.append({"layer": "persistence", "conflict": "STORAGE_CONFIG_CHANGED",
                     "updated_at": stored.get("updated_at"), "all_nodes": True, "label_targets": []})
    evidence.append({"layer": "ui", "conflict_code": "STORAGE_CONFIG_CHANGED", "draft_retained": True,
                     "dialog_open": True, "teardown_asserted": False})
    button(dialog, storage["form"]["cancel"]).click()
    expect(dialog).to_be_hidden()
    report["last_step"] = "conflict-reopen"
    page.reload()
    header = page.locator(".ant-drawer-header").filter(has_text=name).first
    expect(header).to_be_visible()
    expect(button(header, storage["actions"]["edit"])).to_be_visible()
    reopened = page.get_by_role("dialog", name=storage["form"]["editTitle"].replace("{{name}}", name), exact=True)
    button(header, storage["actions"]["edit"]).click()
    expect(reopened).to_be_visible()
    expect(reopened.get_by_role("radio", name=scope["all"], exact=True)).to_be_checked()
    expect(reopened.get_by_label(scope["labelKey"], exact=True)).to_have_count(0)
    reopened.get_by_text(scope["labels"], exact=True).click()
    expect(reopened.get_by_role("radio", name=scope["labels"], exact=True)).to_be_checked()
    reopened.get_by_label(scope["labelKey"], exact=True).fill("team")
    reopened.get_by_label(scope["labelValue"], exact=True).fill("alpha")
    recovered_targets = [{"label_key": "team", "label_value": "alpha"}]
    report["last_step"] = "conflict-retry"
    with page.expect_response(lambda r: urlsplit(r.url).path == api_path and r.request.method == "PATCH") as retried:
        button(reopened, storage["form"]["save"]).click()
    retry = retried.value
    retry_sent = retry.request.post_data_json
    check(retry.status == 200, f"reloaded edit HTTP {retry.status}")
    check(retry_sent == {"all_nodes": False, "targets": recovered_targets,
                         "expected_updated_at": preview["updated_at"]},
          f"reloaded edit body differs: {retry_sent}")
    expect(reopened).to_be_hidden()
    expect(page.get_by_text("team=alpha", exact=True).first).to_be_visible()
    recovered = page.request.get(server.base + api_path)
    recovered_doc = recovered.json()
    recovered_labels = [item for item in recovered_doc.get("targets") or [] if item.get("label_key")]
    check(recovered.status == 200 and recovered_doc.get("all_nodes") is False
          and recovered_doc.get("name") == name and recovered_doc.get("path") == path_value
          and recovered_labels == recovered_targets,
          f"reloaded edit GET differs: {recovered_doc}")
    report["last_step"] = "conflict-restored"
    header = page.locator(".ant-drawer-header").filter(has_text=name).first
    button(header, storage["actions"]["edit"]).click()
    restoring = page.get_by_role("dialog", name=storage["form"]["editTitle"].replace("{{name}}", name), exact=True)
    expect(restoring).to_be_visible()
    expect(restoring.get_by_label(scope["labelKey"], exact=True)).to_have_value("team")
    restoring.get_by_text(scope["all"], exact=True).click()
    expect(restoring.get_by_role("radio", name=scope["all"], exact=True)).to_be_checked()
    with page.expect_response(lambda r: urlsplit(r.url).path == api_path and r.request.method == "PATCH") as restored:
        button(restoring, storage["form"]["save"]).click()
    restore = restored.value
    restore_sent = restore.request.post_data_json
    check(restore.status == 200, f"restore all-nodes HTTP {restore.status}")
    check(restore_sent == {"all_nodes": True, "targets": [],
                           "expected_updated_at": recovered_doc["updated_at"]},
          f"restore all-nodes body differs: {restore_sent}")
    expect(restoring).to_be_hidden()
    final = page.request.get(server.base + api_path)
    final_doc = final.json()
    check(final.status == 200 and final_doc.get("all_nodes") is True and final_doc.get("name") == name
          and final_doc.get("path") == path_value
          and all(not item.get("label_key") for item in final_doc.get("targets") or []),
          f"restore changed the stored storage: {final_doc}")
    report["conflict_recovered"] = True


# Contract: gpuworks/storage/local-create-delete
# Mainline: 独立 server/SQLite → 存储页新增本地存储 → NFS 类型显示 Export/Mount point
#           且未试挂时不可保存 → 非法名称与相对路径都不发请求 → 合法本地存储创建
#           → 刷新/独立 GET 回读 → UI 删除后列表消失且 GET 404
def exercise_html_escaping(page, storage, common, posts, bad_responses, server):
    """2.7: a path carrying HTML is shown as text wherever the product displays it.

    A second storage is created through the real form, so the mainline assertions that read the
    first one are untouched. Its stored path is then moved through the real PATCH route, and the
    detail view and the list are each read back. A rendered element instead of text would mean
    the value reached the DOM as HTML, so the checks look for the tag as a node, not only for its
    characters.
    """
    marker = "e2e-xss-<img src=x onerror=1>"
    escaped_name = "e2e-xss-" + uuid.uuid4().hex[:8]
    escaped_path = str(server.storage_root / "e2e-xss")
    api_base = page.url.split("/storage")[0]
    action(page, storage["page"]["add"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    boxes = dialog.get_by_role("textbox")
    boxes.nth(0).fill(escaped_name)
    boxes.nth(1).fill(escaped_path)
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as created:
        button(dialog, storage["form"]["save"]).click()
    check(created.value.status == 201, f"escaping storage create HTTP {created.value.status}")
    escaped_id = created.value.json()["id"]
    injected = f"{escaped_path}-{marker}"
    updated = page.request.patch(f"{api_base}{STORAGES}/{escaped_id}", data={"path": injected})
    check(updated.status in {200, 204}, f"path injection PATCH HTTP {updated.status}")
    reread = page.request.get(f"{api_base}{STORAGES}/{escaped_id}").json()
    check(reread.get("path") == injected, f"persisted path differs: {reread.get('path')!r}")
    page.goto(f"{api_base}/storage?storage={escaped_id}")
    shown = page.get_by_text(injected, exact=True).first
    expect(shown).to_be_visible()
    check(page.locator("img[src='x']").count() == 0 and page.locator("script:has-text('onerror')").count() == 0,
          "the injected path created DOM nodes on the detail view")
    check(shown.evaluate("element => element.children.length") == 0,
          "the detail view renders the path through child nodes, not as text")
    page.goto(f"{api_base}/storage")
    expect(page.get_by_role("heading", name=storage["page"]["title"], exact=True)).to_be_visible()
    list_row = page.get_by_role("row").filter(has_text=escaped_name).first
    expect(list_row).to_be_visible()
    check(list_row.locator("img[src='x']").count() == 0 and list_row.locator("script").count() == 0,
          "the list row created DOM nodes from the path")
    page.request.delete(f"{api_base}{STORAGES}/{escaped_id}")
    return {"marker": marker, "shown_as_text": True, "injected_nodes": 0,
            "persisted": True, "patch_status": updated.status}


def test_storage_local(page, context, server, artifacts, report, locale):
    storage, common, auth = strings(locale)
    name = f"e2e-{report['run_id'].lower()}-{uuid.uuid4().hex[:6]}"
    path = str(server.work / f"e2e-{report['run_id'].lower()}")
    protected_path = f"/etc/e2e-{report['run_id'].lower()}-system-directory"
    page_errors, console_errors, bad_responses, posts = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    refusal_console = []

    def console_seen(message):
        if message.type == "error":
            source = urlsplit(message.location.get("url", "")).path
            if not authenticated and source == "/api/v1/auth/me" and "401" in message.text:
                return
            # Chrome's resource-error line for the asserted system-directory refusal.
            if source == STORAGES and "status of 400" in message.text:
                refusal_console.append(message.text)
                return
            console_errors.append(message.text)

    def response_seen(response):
        pathname = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and pathname == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": pathname,
                                  "status": response.status})

    def request_seen(request):
        if urlsplit(request.url).path == STORAGES and request.method == "POST":
            posts.append(request.post_data_json)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/storage")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=storage["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "storage-page-loaded"

    add = action(page, storage["page"]["add"])
    expect(add).to_be_visible()
    report["last_step"] = "add dialog over the storage page"
    exercise_add_dialog(page, add, storage, posts, report)

    add.click()
    modal = page.get_by_role("dialog")
    expect(modal).to_be_visible()
    expect(modal.get_by_text(storage["form"]["addTitle"], exact=True)).to_be_visible()
    from storage_local_inventory import exercise_local_inventory
    report["local_inventory"] = exercise_local_inventory(modal, storage, common, posts)
    expect(modal.locator("input[type=password]")).to_have_count(0)
    report["create_secret_inputs"] = 0
    save = button(modal, storage["form"]["save"])
    local = modal.get_by_role("radio", name=common["storageType"]["local"], exact=True)
    nfs = modal.get_by_role("radio", name=common["storageType"]["nfs"], exact=True)
    expect(local).to_be_checked()
    expect(modal.get_by_label(storage["form"]["path"], exact=True)).to_be_visible()

    # AntD renders the radio input hidden; the visible label is the click
    # target while the input still carries the checked state.
    modal.get_by_text(common["storageType"]["nfs"], exact=True).click()
    expect(modal.get_by_label(storage["form"]["export"], exact=True)).to_be_visible()
    expect(modal.get_by_label(storage["form"]["mountPoint"], exact=True)).to_be_visible()
    expect(save).to_be_disabled()
    check(posts == [], "an unprobed NFS form saved")
    modal.get_by_text(common["storageType"]["local"], exact=True).click()
    expect(modal.get_by_label(storage["form"]["path"], exact=True)).to_be_visible()
    expect(modal.get_by_label(storage["form"]["export"], exact=True)).to_have_count(0)
    report["last_step"] = "type-switch-verified"

    name_input = modal.get_by_label(storage["form"]["name"], exact=True)
    path_input = modal.get_by_label(storage["form"]["path"], exact=True)
    from storage_local_required import exercise_storage_required
    report["required_fields"] = exercise_storage_required(page, modal, storage, path)
    report["last_step"] = "storage-required"
    from storage_local_required import exercise_storage_name_contract
    report["name_contract"] = exercise_storage_name_contract(page, modal, storage, posts)
    report["evidence"].append({"layer": "ui", "name_contract": report["name_contract"]})
    report["last_step"] = "storage-name-contract"
    from storage_local_required import exercise_storage_name_editing
    report["name_editing"] = exercise_storage_name_editing(page, modal, storage, posts)
    report["evidence"].append({"layer": "ui", "name_editing": report["name_editing"]})
    report["last_step"] = "storage-name-editing"
    name_input.fill("Bad Name")
    path_input.fill(path)
    save.click()
    expect(modal.get_by_text(storage["form"]["error"]["name"], exact=True)).to_be_visible()
    check(posts == [], "an invalid name saved")
    report["last_step"] = "invalid-name-rejected"

    name_input.fill(name)
    path_input.fill("relative")
    save.click()
    expect(modal.get_by_text(storage["form"]["error"]["pathAbsolute"], exact=True)).to_be_visible()
    check(posts == [], "a relative path saved")
    report["last_step"] = "invalid-path-rejected"

    # System directories remain protected even though ordinary custom paths
    # no longer need to fall under the configured storage_probe_roots setting.
    path_input.fill(protected_path)
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as refused:
        save.click()
    refused_body = refused.value.request.post_data_json
    check(refused_body == {"name": name, "type": "local", "all_nodes": True, "targets": [], "path": protected_path},
          f"system-directory create body differs: {refused_body}")
    refusal = refused.value.json()
    check(refused.value.status == 400 and refusal.get("code") == "STORAGE_PATH_DENIED"
          and (refusal.get("params") or {}).get("path") == protected_path,
          f"system-directory path: expected 400 STORAGE_PATH_DENIED naming the path, received {refused.value.status} {refusal}")
    alert = modal.get_by_role("alert").filter(has_text=storage["form"]["saveFailed"])
    expect(alert).to_contain_text(format_text(common["apiError"]["STORAGE_PATH_DENIED"], path=protected_path))
    expect(name_input).to_have_value(name)
    expect(path_input).to_have_value(protected_path)
    listed = page.request.get(server.base + STORAGES).json()["items"]
    check(all(item["name"] != name for item in listed), "a refused storage was created")
    # This refusal is the asserted outcome, not a browser error.
    expected_http = {"method": "POST", "path": STORAGES, "status": 400}
    check(bad_responses.count(expected_http) == 1, f"expected exactly one refused POST: {bad_responses}")
    bad_responses.remove(expected_http)
    check(len(refusal_console) == 1, f"expected one console line for the refused POST: {refusal_console}")
    report["last_step"] = "system-directory-refused"

    # Retrying in the same dialog is BLOCKED_PRODUCT (coverage/storage-local.json): after a
    # fast refusal the Save button can keep AntD's loading icon in its accessible name.
    # Cancel sends nothing; a fresh form then creates the storage.
    button(modal, storage["form"]["cancel"]).click()
    expect(modal).to_have_count(0)
    check(len(posts) == 1, f"cancel after the refusal sent a create: {posts}")
    action(page, storage["page"]["add"]).click()
    expect(modal.get_by_text(storage["form"]["addTitle"], exact=True)).to_be_visible()
    expect(name_input).to_have_value("")
    name_input.fill(name)
    path_input.fill(path)
    exercise_local_inflight(
        page, modal, storage, name, path,
        {"name": name, "type": "local", "all_nodes": True, "targets": [], "path": path},
        bad_responses, console_errors, report["evidence"])
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as created:
        save.click()
    check(created.value.status == 201, f"storage create HTTP {created.value.status}")
    body = created.value.request.post_data_json
    check(body == {"name": name, "type": "local", "all_nodes": True, "targets": [], "path": path},
          f"storage create body differs: {body}")
    storage_id = created.value.json()["id"]
    expect(page.get_by_text(format_text(storage["form"]["created"], name=name),
                            exact=True).first).to_be_visible()
    # The form opens the new storage's detail page.
    page.wait_for_url(re.compile(rf".*/storage\?storage={storage_id}(?:&|$)"), timeout=15000)
    expect(page.get_by_text(name, exact=True).first).to_be_visible()
    expect(page.get_by_text(common["storageType"]["local"], exact=True).first).to_be_visible()
    expect(page.get_by_text(path, exact=True).first).to_be_visible()
    expect(page.get_by_text(storage["scope"]["all"], exact=True).first).to_be_visible()

    config_title = storage["overview"]["config"]
    # The overview is a stack of DetailSection blocks now, not antd cards: the
    # section is a <section class="detail-section"> headed by an h5.
    config_card = page.locator("section.detail-section").filter(
        has=page.get_by_role("heading", name=config_title, exact=True))
    expect(config_card).to_have_count(1)
    report["config_section"] = True
    report["last_step"] = "show-nodes-link"
    show_nodes = page.get_by_role("button", name=storage["overview"]["showNodes"], exact=True)
    expect(show_nodes).to_have_count(1)
    expect(show_nodes).to_have_class(re.compile(r"\bant-btn-link\b"))
    report["show_nodes_link"] = True
    page.goto(server.base + "/storage")
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)

    page.reload()
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    independent = page.request.get(server.base + STORAGES + f"/{storage_id}")
    check(independent.status == 200, f"independent GET HTTP {independent.status}")
    saved = independent.json()
    check(saved["name"] == name and saved["type"] == "local" and saved["path"] == path,
          f"persisted storage differs: {saved}")
    check(saved["all_nodes"] is True, "the storage did not persist the all-nodes scope")
    report["last_step"] = "storage-persisted"
    exercise_edit_conflict(page, server, storage, common, storage_id, name, path, saved["updated_at"],
                           bad_responses, console_errors, report["evidence"], report)
    report["edit_backfill"] = True
    report["edit_secret_inputs"] = 0
    report["last_step"] = "storage-edit-conflict"
    from storage_local_edit import exercise_local_edit_changes
    report["last_step"] = "local-edit-changes"
    report["local_edit"] = exercise_local_edit_changes(
        page, server, storage, storage_id, name, path)
    report["local_edit_sha256"] = digest(ROOT / "test/e2e/storage_local_edit.py")
    report["last_step"] = "name-taken"
    exercise_name_taken(page, server, storage, name, path, report, bad_responses, console_errors)
    report["last_step"] = "full-optional"
    report["full_optional"] = exercise_full_local(page, server, storage, name, path, report)
    page.goto(server.base + "/storage")
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)

    row.get_by_role("button", name=storage["actions"]["delete"], exact=True).click()
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_contain_text(
        format_text(storage["actions"]["deleteConfirm"], name=name))
    expect(popconfirm).to_contain_text(storage["actions"]["deleteHelp"])
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES + f"/{storage_id}"
                              and r.request.method == "DELETE") as deleted:
        confirm_popconfirm(page)
    check(deleted.value.status in (200, 204), f"storage delete HTTP {deleted.value.status}")
    expect(row).to_have_count(0, timeout=15000)
    expect(page.get_by_text(format_text(storage["actions"]["deleted"], name=name),
                            exact=True).first).to_be_visible()
    check(page.request.get(server.base + STORAGES + f"/{storage_id}").status == 404,
          "the deleted storage is still readable")
    report["last_step"] = "storage-deleted"

    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "storage page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    # 2.7 runs last, against its own storage, so moving the path cannot disturb the mainline
    # assertions that already read the first one.
    report["last_step"] = "html-escaping"
    report["html_escaping"] = exercise_html_escaping(
        page, storage, common, posts, bad_responses, server)
    report["evidence"].append({"layer": "ui", "html_escaping": report["html_escaping"]})

    report["evidence"].extend([
        {"layer": "ui", "page": "/storage", "modal": storage["form"]["addTitle"],
         "nfs_fields": [storage["form"]["export"], storage["form"]["mountPoint"]],
         "nfs_save_disabled": True,
         "invalid_name": storage["form"]["error"]["name"],
         "invalid_path": storage["form"]["error"]["pathAbsolute"],
         "system_directory_alert": [storage["form"]["saveFailed"], format_text(common["apiError"]["STORAGE_PATH_DENIED"], path=protected_path)],
         "created": name, "deleted": name,
         "delete_confirmation": {"title": format_text(
             storage["actions"]["deleteConfirm"], name=name),
             "impact": storage["actions"]["deleteHelp"]}},
        {"layer": "request", "post": {"path": STORAGES, "status": 201, "body": body},
         "invalid_submissions": 0,
         "server_rejections": [{"path": STORAGES, "status": 400, "code": refusal["code"],
                                "body": refused_body}],
         "delete": {"path": STORAGES + f"/{storage_id}", "status": deleted.value.status}},
        {"layer": "persistence", "refresh_row": 1,
         "independent_get": {"name": saved["name"], "type": saved["type"],
                             "path": saved["path"], "all_nodes": saved["all_nodes"]}},
        {"layer": "cleanup", "deleted": True, "after_delete": 404},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "posts": posts}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/storage-local.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_storage_local.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "required_sha256": digest(ROOT / "test/e2e/storage_local_required.py"),
        "inventory_sha256": digest(ROOT / "test/e2e/storage_local_inventory.py"),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "dialog_checks_sha256": digest(ROOT / "test/e2e/dialog_checks.py"),
        "execution": "N/A: a local storage record; no agent trial mount or model-file access",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["agent trial mount and capacity",
                        "creating shared_dir, NFS or NFS-cache storage (only local is created)",
                        "unsaved draft left in the add dialog",
                        "one-by-one node scope", "in-use deletion",
                        "duplicate names and exports", "non-admin permissions",
                        "model files and scans", "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/storage_local.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["server_environment"] = server.environment
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(
                viewport=report["viewport"],
                permissions=["clipboard-read", "clipboard-write"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_storage_local(page, context, server, artifacts, report, args.locale)
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
