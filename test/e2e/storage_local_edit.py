"""Local storage edit changes: one field at a time, then the label binding.

The caller owns the browser and the already-created local storage. Each save is
the real PATCH. The independent GET is the oracle. No secret is recorded.
"""
from __future__ import annotations

from urllib.parse import urlsplit

from playwright.sync_api import expect
from platform_token_read_revoke import button, check

STORAGES = "/api/v1/storages"


def _title(storage, name):
    return storage["form"]["editTitle"].replace("{{name}}", name)


def _open(page, storage, name):
    # The list row and the open detail drawer each render Edit, so take the one in
    # the drawer header for the row under test.
    header = page.locator(".ant-drawer-header").filter(has_text=name).first
    expect(header).to_be_visible()
    button(header, storage["actions"]["edit"]).click()
    dialog = page.get_by_role("dialog", name=_title(storage, name), exact=True)
    expect(dialog).to_be_visible()
    return dialog


def _labels(doc):
    return [item for item in doc.get("targets") or [] if item.get("label_key")]


def _saved(page, server, storage_id, name, path_value, all_nodes, labels):
    response = page.request.get(server.base + STORAGES + f"/{storage_id}")
    check(response.status == 200, f"storage GET HTTP {response.status}")
    doc = response.json()
    check(doc["name"] == name and doc["path"] == path_value and doc["type"] == "local"
          and doc["all_nodes"] is all_nodes and _labels(doc) == labels,
          "stored storage differs")
    return doc


def _patch(page, dialog, storage, storage_id, expected):
    api_path = STORAGES + f"/{storage_id}"
    with page.expect_response(lambda r: urlsplit(r.url).path == api_path
                              and r.request.method == "PATCH") as saved:
        button(dialog, storage["form"]["save"]).click()
    result = saved.value
    body = result.request.post_data_json
    check(result.status == 200, f"storage edit HTTP {result.status}")
    check(body == expected, "storage edit body differs")
    expect(dialog).to_be_hidden()
    return body


def exercise_local_edit_changes(page, server, storage, storage_id, name, path_value):
    """Name A to B, label A to B to empty to none. The directory stays. No switch."""
    scope = storage["form"]["scope"]
    renamed = name + "-b"
    page.goto(server.base + f"/storage/{storage_id}")
    expect(page.get_by_text(name, exact=True).first).to_be_visible()
    dialog = _open(page, storage, name)
    expect(dialog.get_by_role("switch")).to_have_count(0)
    expect(dialog.get_by_label(storage["form"]["name"], exact=True)).to_have_value(name)
    expect(dialog.get_by_label(storage["form"]["path"], exact=True)).to_have_value(path_value)
    expect(dialog.get_by_role("radio", name=scope["all"], exact=True)).to_be_checked()
    dialog.get_by_label(storage["form"]["name"], exact=True).fill(renamed)
    name_body = _patch(page, dialog, storage, storage_id, {"name": renamed})
    renamed_doc = _saved(page, server, storage_id, renamed, path_value, True, [])

    page.goto(server.base + f"/storage/{storage_id}")
    expect(page.get_by_text(renamed, exact=True).first).to_be_visible()
    dialog = _open(page, storage, renamed)
    expect(dialog.get_by_label(storage["form"]["path"], exact=True)).to_have_value(path_value)
    dialog.get_by_text(scope["labels"], exact=True).click()
    expect(dialog.get_by_role("radio", name=scope["labels"], exact=True)).to_be_checked()
    dialog.get_by_label(scope["labelKey"], exact=True).fill("team")
    dialog.get_by_label(scope["labelValue"], exact=True).fill("alpha")
    alpha = [{"label_key": "team", "label_value": "alpha"}]
    alpha_body = _patch(page, dialog, storage, storage_id, {
        "all_nodes": False, "targets": alpha,
        "expected_updated_at": renamed_doc["updated_at"]})
    alpha_doc = _saved(page, server, storage_id, renamed, path_value, False, alpha)
    expect(page.get_by_text("team=alpha", exact=True).first).to_be_visible()

    page.goto(server.base + f"/storage/{storage_id}")
    dialog = _open(page, storage, renamed)
    expect(dialog.get_by_label(storage["form"]["name"], exact=True)).to_have_value(renamed)
    expect(dialog.get_by_label(storage["form"]["path"], exact=True)).to_have_value(path_value)
    dialog.get_by_label(scope["labelKey"], exact=True).fill("zone")
    dialog.get_by_label(scope["labelValue"], exact=True).fill("east")
    east = [{"label_key": "zone", "label_value": "east"}]
    east_body = _patch(page, dialog, storage, storage_id, {
        "targets": east, "expected_updated_at": alpha_doc["updated_at"]})
    east_doc = _saved(page, server, storage_id, renamed, path_value, False, east)

    page.goto(server.base + f"/storage/{storage_id}")
    dialog = _open(page, storage, renamed)
    dialog.get_by_label(scope["labelValue"], exact=True).fill("")
    empty = [{"label_key": "zone", "label_value": ""}]
    empty_body = _patch(page, dialog, storage, storage_id, {
        "targets": empty, "expected_updated_at": east_doc["updated_at"]})
    empty_doc = _saved(page, server, storage_id, renamed, path_value, False,
                       [{"label_key": "zone"}])
    expect(page.get_by_text("zone=", exact=True).first).to_be_visible()

    page.goto(server.base + f"/storage/{storage_id}")
    dialog = _open(page, storage, renamed)
    dialog.get_by_text(scope["all"], exact=True).click()
    expect(dialog.get_by_role("radio", name=scope["all"], exact=True)).to_be_checked()
    none_body = _patch(page, dialog, storage, storage_id, {
        "all_nodes": True, "targets": [],
        "expected_updated_at": empty_doc["updated_at"]})
    _saved(page, server, storage_id, renamed, path_value, True, [])
    expect(page.get_by_text("zone=", exact=True)).to_have_count(0)

    page.goto(server.base + f"/storage/{storage_id}")
    dialog = _open(page, storage, renamed)
    expect(dialog.get_by_label(storage["form"]["path"], exact=True)).to_have_value(path_value)
    expect(dialog.get_by_role("radio", name=scope["all"], exact=True)).to_be_checked()
    dialog.get_by_label(storage["form"]["name"], exact=True).fill(name)
    restored_body = _patch(page, dialog, storage, storage_id, {"name": name})
    _saved(page, server, storage_id, name, path_value, True, [])
    return {
        "switch": False,
        "name_to": renamed,
        "name_restored": name,
        "path_unchanged": True,
        "label_alpha": alpha,
        "label_east": east,
        "label_empty": empty,
        "unbound": True,
        "patches": [name_body, alpha_body, east_body, empty_body, none_body, restored_body],
    }
