"""Label key and value length boundaries for the node labels dialog.

The key grammar accepts 1 to 128 single-byte characters. The value limit is
256 bytes, so 85 CJK characters save and 86 do not.
"""
import re
from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout
from playwright.sync_api import expect

from platform_token_read_revoke import check

NODES = "/api/v1/nodes"


def exercise_label_field_inventory(modal, ui, requests):
    """Register the new label row before any key or value is typed.

    The key and the value are both textboxes. Only the key is required. Both
    open empty, neither has a unit, and nothing in the row is hidden. Reading
    them sends no request.
    """
    baseline = len(requests)
    key_name = ui["labels"]["key"]
    value_name = ui["labels"]["value"]
    key = modal.get_by_role("textbox", name=key_name, exact=True)
    expect(key).to_have_count(1)
    value = modal.get_by_role("textbox", name=value_name, exact=True)
    expect(value).to_have_count(1)
    expect(modal.get_by_role("textbox")).to_have_count(2)
    expect(key).to_have_attribute("placeholder", key_name)
    expect(value).to_have_attribute("placeholder", value_name)
    expect(key).to_have_attribute("aria-required", "true")
    check(value.get_attribute("aria-required") is None, "the value textbox is required")
    expect(key).to_have_value("")
    expect(value).to_have_value("")
    expect(key).to_have_attribute("maxlength", "128")
    expect(value).to_have_attribute("maxlength", "256")
    expect(key).to_be_visible()
    expect(value).to_be_visible()
    add_name = ui["labels"]["add"]
    accessible = r"\s*".join(add_name) if re.fullmatch(r"[㐀-鿿]{2}", add_name) else re.escape(add_name)
    add = modal.get_by_role("button", name=re.compile(accessible))
    expect(add).to_have_count(1)
    expect(add).to_have_class(re.compile(r"\bant-btn-dashed\b"))
    check(modal.locator(".ant-input-suffix, .ant-input-number, .ant-slider").count() == 0,
          "a label field shows a unit")
    check(modal.locator(".ant-spin").count() == 0, "the labels dialog is still loading")
    expect(modal.locator(".ant-modal-footer .ant-btn-primary")).to_be_enabled()
    check(modal.locator("input[type=password]").count() == 0, "the new label row has a secret")
    check(len(requests) == baseline, "reading the label fields sent a request")
    return {
        "fields": [
            {"label": key_name, "type": "textbox", "required": True, "default": "",
             "unit": None, "max_length": 128, "conditional": False},
            {"label": value_name, "type": "textbox", "required": False, "default": "",
             "unit": None, "max_length": 256, "conditional": False},
        ],
        "loading": False,
        "save_enabled": True,
        "secret": False,
        "add_button_dashed": True,
        "requests": 0,
    }


def exercise_label_dismissals(page, dialog, name, node_uuid, base, ui, requests, report):
    """Cancel, Escape, close and the mask drop a typed draft and send no PUT."""
    from dialog_checks import (
        assert_nested_dismissals, cancel_button, close_button, escape, mask)

    report["last_step"] = "label-dismissals"
    parent = page.get_by_role("heading", name=ui["page"]["title"], exact=True)

    def open_dialog():
        page.get_by_role("row").filter(has_text=name).get_by_role(
            "button", name=ui["action"]["more"], exact=True).click()
        page.get_by_role("menuitem").filter(has_text=ui["labels"]["edit"]).click()
        opened = page.get_by_role("dialog")
        expect(opened).to_be_visible()
        return opened

    def with_draft(dismiss):
        def run(target):
            accessible = (r"\s*".join(ui["labels"]["add"])
                          if re.fullmatch(r"[\u3400-\u9fff]{2}", ui["labels"]["add"])
                          else re.escape(ui["labels"]["add"]))
            target.get_by_role("button", name=re.compile(accessible)).first.click()
            target.get_by_label(ui["labels"]["key"], exact=True).fill("team")
            target.get_by_label(ui["labels"]["value"], exact=True).fill("draft")
            dismiss(target)
        return run

    baseline = len(requests)
    dialog = assert_nested_dismissals(
        dialog, parent, open_dialog,
        [("cancel", with_draft(cancel_button(ui["action"]["cancel"]))),
         ("escape", with_draft(escape)),
         ("close", with_draft(close_button)),
         ("mask", with_draft(mask))],
        lambda: len(requests))
    check(len(requests) == baseline, "dismissing the label dialog sent a PUT")
    read = page.request.get(base + NODES + "/" + node_uuid)
    check(read.status == 200 and read.json()["labels"] == {},
          f"a dismissed label draft was stored: {read.status} {read.text()}")
    report["label_dismissals"] = {
        "dismissals": ["cancel", "escape", "close", "mask"],
        "writes": 0, "stored": {}}
    return dialog


def exercise_label_edit_backfill(modal, ui):
    """Reopening a saved label shows the stored key and value in full."""
    key = modal.get_by_label(ui["labels"]["key"], exact=True)
    value = modal.get_by_label(ui["labels"]["value"], exact=True)
    expect(key).to_have_count(1)
    expect(key).to_have_value("team")
    expect(value).to_have_count(1)
    expect(value).to_have_value("alpha")
    check(modal.locator("input[type=password]").count() == 0, "the labels dialog echoed a secret")
    check(value.input_value() == "alpha", "the saved label value was not shown in full")
    modal.locator(".ant-modal-footer .ant-btn-default").click()
    expect(modal).not_to_be_visible()
    return {"key": "team", "value": "alpha", "secret": False}


def exercise_label_lengths(page, base, node_uuid, ui, open_labels, requests):
    """Shortest, one-shorter, longest and one-longer for the key and the value."""
    labels_path = NODES + f"/{node_uuid}/labels"
    node_path = NODES + f"/{node_uuid}"
    puts = []

    def read_labels():
        response = page.request.get(base + node_path)
        check(response.status == 200, f"label length GET HTTP {response.status}")
        return response.json()["labels"]

    def save(dialog, body, status):
        with page.expect_response(lambda response: urlsplit(response.url).path == labels_path
                                  and response.request.method == "PUT") as saved:
            dialog.locator(".ant-modal-footer .ant-btn-primary").click()
        check(saved.value.status == status, f"label length HTTP {saved.value.status} for {status}")
        check(saved.value.request.post_data_json == body,
              f"label length body {saved.value.request.post_data_json}")
        puts.append(body)
        return saved.value

    # The empty value is the shortest stored value. Nothing is shorter.
    dialog = open_labels()
    value_input = dialog.get_by_label(ui["labels"]["value"], exact=True)
    value_input.fill("")
    expect(value_input).to_have_value("")
    save(dialog, {"labels": {"team": ""}}, 200)
    expect(dialog).not_to_be_visible()
    check(read_labels() == {"team": ""}, "the empty label value was not stored")

    # 256 ASCII characters are 256 bytes. The control stops at 256.
    dialog = open_labels()
    value_input = dialog.get_by_label(ui["labels"]["value"], exact=True)
    longest_value = "v" * 256
    value_input.fill(longest_value + "v")
    expect(value_input).to_have_value(longest_value)
    save(dialog, {"labels": {"team": longest_value}}, 200)
    expect(dialog).not_to_be_visible()
    check(read_labels() == {"team": longest_value}, "the 256-byte label value was not stored")
    over_value = page.request.put(base + labels_path, data={"labels": {"team": longest_value + "v"}})
    check(over_value.status == 400, f"257 value bytes were stored: HTTP {over_value.status}")
    over_value_body = over_value.json()
    check(over_value_body.get("code") == "NODE_LABELS_INVALID", f"value length code {over_value_body}")
    check("invalid value" in over_value_body.get("params", {}).get("detail", ""),
          f"value length reason {over_value_body}")
    check(read_labels() == {"team": longest_value}, "257 value bytes changed the stored label")

    # 85 CJK characters are 255 bytes and save. 86 are 258 bytes. The control
    # still shows all 86, so the limit counts bytes rather than characters.
    accepted = "测" * 85
    check(len(accepted.encode()) == 255, "85 CJK characters are not 255 bytes")
    dialog = open_labels()
    value_input = dialog.get_by_label(ui["labels"]["value"], exact=True)
    value_input.fill(accepted)
    expect(value_input).to_have_value(accepted)
    save(dialog, {"labels": {"team": accepted}}, 200)
    expect(dialog).not_to_be_visible()
    check(read_labels() == {"team": accepted}, "the 255-byte label value was not stored")

    rejected = "测" * 86
    check(len(rejected.encode()) == 258, "86 CJK characters are not 258 bytes")
    dialog = open_labels()
    value_input = dialog.get_by_label(ui["labels"]["value"], exact=True)
    value_input.fill(rejected)
    expect(value_input).to_have_value(rejected)
    failed = save(dialog, {"labels": {"team": rejected}}, 400)
    check(failed.json().get("code") == "NODE_LABELS_INVALID", "CJK overflow code differs")
    expect(dialog).to_be_visible()
    expect(page.get_by_text(re.compile(
        re.escape(ui["labels"]["failed"].split("{{")[0].strip()))).first).to_be_visible()
    check(read_labels() == {"team": accepted}, "86 CJK characters changed the stored label")

    # One shorter than the shortest key sends nothing. One character saves.
    before = len(requests)
    key_input = dialog.get_by_label(ui["labels"]["key"], exact=True)
    key_input.fill("")
    value_input.fill("a")
    dialog.locator(".ant-modal-footer .ant-btn-primary").click()
    expect(dialog.get_by_text(ui["labels"]["keyRequired"], exact=True)).to_be_visible()
    check(len(requests) == before, "an empty label key was submitted")
    check(read_labels() == {"team": accepted}, "an empty label key changed stored labels")
    key_input.fill("k")
    value_input.fill("a")
    save(dialog, {"labels": {"k": "a"}}, 200)
    expect(dialog).not_to_be_visible()
    check(read_labels() == {"k": "a"}, "the one-character label key was not stored")

    # 128 ASCII characters are 128 bytes. The control stops at 128.
    dialog = open_labels()
    key_input = dialog.get_by_label(ui["labels"]["key"], exact=True)
    value_input = dialog.get_by_label(ui["labels"]["value"], exact=True)
    longest_key = "k" + ("a" * 127)
    key_input.fill(longest_key + "a")
    expect(key_input).to_have_value(longest_key)
    value_input.fill("a")
    save(dialog, {"labels": {longest_key: "a"}}, 200)
    expect(dialog).not_to_be_visible()
    check(read_labels() == {longest_key: "a"}, "the 128-byte label key was not stored")
    over_key = page.request.put(base + labels_path, data={"labels": {"a" * 129: "a"}})
    check(over_key.status == 400, f"129 key bytes were stored: HTTP {over_key.status}")
    over_key_body = over_key.json()
    check(over_key_body.get("code") == "NODE_LABELS_INVALID", f"key length code {over_key_body}")
    check("invalid node label key" in over_key_body.get("params", {}).get("detail", ""),
          f"key length reason {over_key_body}")
    check(read_labels() == {longest_key: "a"}, "129 key bytes changed the stored label")

    dialog = open_labels()
    dialog.get_by_label(ui["labels"]["key"], exact=True).fill("team")
    dialog.get_by_label(ui["labels"]["value"], exact=True).fill("alpha")
    save(dialog, {"labels": {"team": "alpha"}}, 200)
    expect(dialog).not_to_be_visible()
    check(read_labels() == {"team": "alpha"}, "restoring team=alpha failed")
    return {
        "observed": {
            "value_shortest": 0,
            "value_no_shorter": True,
            "value_longest_bytes": 256,
            "value_over_ascii_bytes": 257,
            "value_cjk_saved_chars": 85,
            "value_cjk_saved_bytes": 255,
            "value_cjk_rejected_chars": 86,
            "value_cjk_rejected_bytes": 258,
            "value_unit": "bytes",
            "key_shortest": 1,
            "key_shorter_rejected": True,
            "key_longest_bytes": 128,
            "key_over_bytes": 129,
            "key_unit": "bytes",
            "key_multibyte": False,
        },
        "extra_puts": puts,
    }


def exercise_label_text_edit(page, context, base, node_uuid, ui, open_labels, requests):
    """Paste, keyboard clear and keyboard edit. A newline is not a stored value."""
    labels_path = NODES + f"/{node_uuid}/labels"
    node_path = NODES + f"/{node_uuid}"
    context.grant_permissions(["clipboard-read", "clipboard-write"])
    dialog = open_labels()
    value_input = dialog.get_by_label(ui["labels"]["value"], exact=True)
    before = len(requests)

    def paste(text):
        page.evaluate("(value) => navigator.clipboard.writeText(value)", text)
        value_input.click()
        value_input.press("Control+A")
        value_input.press("Control+V")

    pasted = "粘A1."
    paste(pasted)
    expect(value_input).to_have_value(pasted)
    value_input.press("Control+A")
    value_input.press("Backspace")
    expect(value_input).to_have_value("")
    value_input.press_sequentially("alpha")
    expect(value_input).to_have_value("alpha")
    paste("上\n下")
    shown = value_input.input_value()
    check("\n" not in shown and "\r" not in shown, f"a newline stayed in the label value {shown!r}")
    value_input.press("Control+A")
    value_input.press("Backspace")
    value_input.press_sequentially("alpha")
    expect(value_input).to_have_value("alpha")
    check(len(requests) == before, "editing the label value submitted it")

    rejected = page.request.put(base + labels_path, data={"labels": {"team": "al\npha"}})
    check(rejected.status == 400, f"a newline value was stored: HTTP {rejected.status}")
    rejected_body = rejected.json()
    check(rejected_body.get("code") == "NODE_LABELS_INVALID", f"newline code {rejected_body}")
    check("invalid value" in rejected_body.get("params", {}).get("detail", ""),
          f"newline reason {rejected_body}")
    stored = page.request.get(base + node_path)
    check(stored.status == 200 and stored.json()["labels"] == {"team": "alpha"},
          "the newline value changed stored labels")

    with page.expect_response(lambda response: urlsplit(response.url).path == labels_path
                              and response.request.method == "PUT") as saved:
        dialog.locator(".ant-modal-footer .ant-btn-primary").click()
    check(saved.value.status == 200, f"keyboard label save HTTP {saved.value.status}")
    check(saved.value.request.post_data_json == {"labels": {"team": "alpha"}},
          f"keyboard label body {saved.value.request.post_data_json}")
    expect(dialog).not_to_be_visible()
    reread = page.request.get(base + node_path)
    check(reread.status == 200 and reread.json()["labels"] == {"team": "alpha"},
          "the saved keyboard value was not stored")
    reopened = open_labels()
    expect(reopened.get_by_label(ui["labels"]["value"], exact=True)).to_have_value("alpha")
    # Two-character Chinese button names can be exposed with a space between
    # the characters. The footer default button is the cancel control.
    reopened.locator(".ant-modal-footer .ant-btn-default").click()
    expect(reopened).not_to_be_visible()
    # 2.7: a label value is free text. A value carrying markup is stored verbatim and shown as
    # text wherever the product lists labels, never as an element.
    markup = "<img src=x onerror=1>"
    saved_markup = page.request.put(base + labels_path, data={"labels": {"team": markup}})
    check(saved_markup.status == 200, f"a markup label value was refused: HTTP {saved_markup.status}")
    markup_reread = page.request.get(base + node_path)
    check(markup_reread.status == 200 and markup_reread.json()["labels"] == {"team": markup},
          f"the markup value was not stored verbatim: {markup_reread.json().get('labels')}")
    page.reload()
    expect(page.get_by_role("heading", name=ui["page"]["title"], exact=True).first).to_be_visible()
    # The list row renders every label as key=value text, so that is where arbitrary label text
    # is shown. It has to arrive as text, not as an element the value built.
    pair = f"team={markup}"
    row_text = page.get_by_text(pair, exact=True)
    expect(row_text.first).to_be_visible()
    check(page.locator("img[src='x']").count() == 0,
          "the label value created an img element on the nodes list")
    # Restore the plain value so anything reading labels afterwards sees what it seeded.
    page.request.put(base + labels_path, data={"labels": {"team": "alpha"}})
    return {
        "observed": {"pasted": pasted, "cleared": True, "keyboard": "alpha",
                     "newline_in_control": False, "newline_status": 400, "restored": "alpha"},
        "extra_puts": [{"labels": {"team": "alpha"}}],
    }


def exercise_minimal_label(page, base, node, ui):
    """Submit one label with only the required key. The value stays empty."""
    labels_path = f"{NODES}/{node['uuid']}/labels"
    node_path = f"{NODES}/{node['uuid']}"
    row = page.get_by_role("row").filter(has_text=node["name"])
    row.get_by_role("button", name=ui["action"]["more"], exact=True).click()
    page.get_by_role("menuitem").filter(has_text=ui["labels"]["edit"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    add_name = ui["labels"]["add"]
    accessible = r"\s*".join(add_name) if re.fullmatch(r"[㐀-鿿]{2}", add_name) else re.escape(add_name)
    dialog.get_by_role("button", name=re.compile(accessible)).first.click()
    key = dialog.get_by_label(ui["labels"]["key"], exact=True)
    value = dialog.get_by_label(ui["labels"]["value"], exact=True)
    expect(value).to_have_value("")
    key.fill("pool")
    expect(value).to_have_value("")
    with page.expect_response(lambda response: urlsplit(response.url).path == labels_path
                              and response.request.method == "PUT") as saved:
        dialog.locator(".ant-modal-footer .ant-btn-primary").click()
    response = saved.value
    body = response.request.post_data_json
    check(response.status == 200, f"minimal label HTTP {response.status}")
    check(response.request.method == "PUT", f"minimal label method {response.request.method}")
    check(urlsplit(response.url).path == labels_path, f"minimal label path {response.url}")
    check(body == {"labels": {"pool": ""}}, f"minimal label body {body}")
    expect(dialog).not_to_be_visible()
    expect(row.get_by_text("pool=", exact=True)).to_be_visible()
    row.get_by_role("button", name=node["name"], exact=True).click()
    drawer = page.locator(".ant-drawer").filter(has_text=node["name"])
    expect(drawer.get_by_text("pool=", exact=True)).to_be_visible()
    drawer.locator(".ant-drawer-close").click()
    expect(drawer).not_to_be_visible()
    page.reload()
    row = page.get_by_role("row").filter(has_text=node["name"])
    expect(row.get_by_text("pool=", exact=True)).to_be_visible()
    readback = page.request.get(base + node_path)
    check(readback.status == 200 and readback.json()["labels"] == {"pool": ""},
          f"minimal label readback {readback.json().get('labels')!r}")
    return {
        "observed": {"method": "PUT", "path": labels_path, "body": body, "status": 200,
                     "list": "pool=", "detail": "pool=", "refreshed": "pool=",
                     "persisted": {"pool": ""}},
        "extra_requests": [{"method": "PUT", "path": labels_path, "body": body}],
    }


def exercise_label_filter_clear(page, base, filter_select, row_a, row_b, name_a, name_b, clean_queries):
    """The label filter exposes a clear control and that control removes the selection.

    Escape only closes the open menu. The labeled node stays the only row.
    Hovering the filter reveals the clear control. Clicking it removes
    team=alpha, both rows return, and the independent node list has both names.
    """
    page.keyboard.press("Escape")
    expect(row_a).to_have_count(1)
    expect(row_b).to_have_count(0)
    select_root = page.locator("div.ant-select").filter(has=filter_select)
    expect(select_root).to_have_count(1)
    select_root.hover()
    clear = select_root.locator(".ant-select-clear")
    expect(clear).to_have_count(1)
    expect(clear).to_be_visible()
    before = len(clean_queries())
    clear.click()
    expect(row_a).to_have_count(1)
    expect(row_b).to_have_count(1)
    expect(select_root.locator(".ant-select-selection-item")).to_have_count(0)
    check(len(clean_queries()) > before,
          "the browser did not re-query the list after clearing the label filter")
    cleared = page.request.get(base + NODES)
    body = cleared.json()
    got = {item["name"] for item in body["items"]}, body["total"]
    check(cleared.status == 200 and got == ({name_a, name_b}, 2),
          f"cleared label filter returned {got}")
    return {
        "escape_kept_filter": True,
        "clear_control": 1,
        "selection_after_clear": 0,
        "names": sorted(got[0]),
        "total": got[1],
        "query": {"query": "label cleared", "names": sorted(got[0]), "total": got[1]},
    }


def _open_label_dialog(page, node_name, ui):
    row = page.get_by_role("row").filter(has_text=node_name)
    row.get_by_role("button", name=ui["action"]["more"], exact=True).click()
    page.get_by_role("menuitem").filter(has_text=ui["labels"]["edit"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    return dialog


def exercise_label_conflict(page, base, node, ui, console_errors, bad_responses, artifacts):
    """Another session overwrites labels, then a deleted node rejects the open draft."""
    labels_path = f"{NODES}/{node['uuid']}/labels"
    node_path = f"{NODES}/{node['uuid']}"
    dialog = _open_label_dialog(page, node["name"], ui)
    expect(dialog.get_by_label(ui["labels"]["key"], exact=True)).to_have_value("team")
    expect(dialog.get_by_label(ui["labels"]["value"], exact=True)).to_have_value("alpha")
    other = page.request.put(base + labels_path, data={"labels": {"room": "east"}})
    check(other.status == 200, f"the other session was rejected: HTTP {other.status}")
    while_open = page.request.get(base + node_path)
    check(while_open.status == 200 and while_open.json()["labels"] == {"room": "east"},
          f"the other session did not stick: {while_open.json().get('labels')!r}")
    expect(dialog.get_by_label(ui["labels"]["key"], exact=True)).to_have_value("team")
    expect(dialog.get_by_label(ui["labels"]["value"], exact=True)).to_have_value("alpha")
    with page.expect_response(lambda response: urlsplit(response.url).path == labels_path
                              and response.request.method == "PUT") as saved:
        dialog.locator(".ant-modal-footer .ant-btn-primary").click()
    check(saved.value.status == 200, f"saving the open draft HTTP {saved.value.status}")
    check(saved.value.request.post_data_json == {"labels": {"team": "alpha"}},
          f"open draft body {saved.value.request.post_data_json}")
    expect(dialog).not_to_be_visible()
    restored = page.request.get(base + node_path)
    check(restored.status == 200 and restored.json()["labels"] == {"team": "alpha"},
          f"the draft save did not win: {restored.json().get('labels')!r}")

    created = page.request.post(base + NODES, data={"name": f"conflict-{node['uuid'][:8]}", "server_url": base})
    check(created.status == 201, f"conflict node HTTP {created.status}")
    created_body = created.json()
    artifacts.remember(created_body.get("token", ""))
    gone = created_body["node"]
    gone_path = f"{NODES}/{gone['uuid']}/labels"
    page.reload()
    gone_dialog = _open_label_dialog(page, gone["name"], ui)
    add_name = ui["labels"]["add"]
    accessible = r"\s*".join(add_name) if re.fullmatch(r"[㐀-鿿]{2}", add_name) else re.escape(add_name)
    gone_dialog.get_by_role("button", name=re.compile(accessible)).first.click()
    gone_dialog.get_by_label(ui["labels"]["key"], exact=True).fill("gone")
    gone_dialog.get_by_label(ui["labels"]["value"], exact=True).fill("1")
    removed = page.request.delete(base + f"{NODES}/{gone['uuid']}?force=true")
    check(removed.status == 200, f"deleting the open node HTTP {removed.status}")
    with page.expect_response(lambda response: urlsplit(response.url).path == gone_path
                              and response.request.method == "PUT") as missing:
        gone_dialog.locator(".ant-modal-footer .ant-btn-primary").click()
    check(missing.value.status == 404, f"the deleted node accepted labels: HTTP {missing.value.status}")
    check(missing.value.request.post_data_json == {"labels": {"gone": "1"}},
          f"deleted node body {missing.value.request.post_data_json}")
    expect(gone_dialog).to_be_visible()
    expect(page.get_by_text(re.compile(re.escape(ui["labels"]["failed"].split("{{")[0].strip()))).first).to_be_visible()
    gone_dialog.locator(".ant-modal-footer .ant-btn-default").click()
    expect(gone_dialog).not_to_be_visible()
    after = page.request.get(base + f"{NODES}/{gone['uuid']}")
    check(after.status == 404, f"cancel recreated the deleted node: HTTP {after.status}")
    check(any(item.get("path") == gone_path and item.get("status") == 404 for item in bad_responses),
          "the deleted-node 404 was not recorded")
    check(any("404" in text for text in console_errors), "404 console line absent")
    bad_responses[:] = [item for item in bad_responses if item.get("status") != 404]
    console_errors[:] = [text for text in console_errors if "404" not in text]
    page.reload()
    expect(page.get_by_role("row").filter(has_text=node["name"]).get_by_text(
        "team=alpha", exact=True)).to_be_visible()
    return {
        "other_session": 200,
        "other_read": {"room": "east"},
        "draft": {"team": "alpha"},
        "saved_status": 200,
        "saved_read": {"team": "alpha"},
        "deleted_status": 404,
        "after_cancel": 404,
        "puts": [
            {"method": "PUT", "path": labels_path, "body": {"labels": {"team": "alpha"}}},
            {"method": "PUT", "path": gone_path, "body": {"labels": {"gone": "1"}}},
        ],
    }


def _box(locator):
    locator.scroll_into_view_if_needed()
    return locator.evaluate(
        """el => {
          const r = el.getBoundingClientRect();
          return {left: r.left, top: r.top, right: r.right, bottom: r.bottom,
                  width: r.width, height: r.height,
                  pageLeft: r.left + window.scrollX, pageTop: r.top + window.scrollY};
        }""")


def _overlaps(a, b):
    return not (a["pageLeft"] + a["width"] <= b["pageLeft"]
                or b["pageLeft"] + b["width"] <= a["pageLeft"]
                or a["pageTop"] + a["height"] <= b["pageTop"]
                or b["pageTop"] + b["height"] <= a["pageTop"])


def _enter_puts(page, field):
    try:
        with page.expect_request(
                lambda request: urlsplit(request.url).path.endswith("/labels")
                and request.method == "PUT", timeout=1000):
            field.press("Enter")
        return True
    except PlaywrightTimeout:
        return False


def exercise_label_keyboard(page, modal, ui, requests):
    """Tab, Enter, the empty-key error, and the 1440×900 footer.

    The row is left empty. Enter does not save a label.
    """
    baseline = len(requests)
    key = modal.get_by_role("textbox", name=ui["labels"]["key"], exact=True)
    value = modal.get_by_role("textbox", name=ui["labels"]["value"], exact=True)
    footer = modal.locator(".ant-modal-footer")
    save = footer.locator(".ant-btn-primary")
    cancel = footer.locator(".ant-btn-default")
    expect(key).to_have_value("")
    expect(value).to_have_value("")
    key.focus()
    expect(key).to_be_focused()
    key.press("Tab")
    expect(value).to_be_focused()
    save.focus()
    expect(save).to_be_focused()
    key.focus()
    check(not _enter_puts(page, key), "Enter on an empty key saved a label")
    expect(modal).to_be_visible()
    expect(key).to_have_value("")
    check(len(requests) == baseline, "Enter on an empty key sent a label write")
    key.fill("pool")
    value.fill("edge")
    key.focus()
    check(not _enter_puts(page, key), "Enter submitted a label")
    expect(modal).to_be_visible()
    expect(key).to_have_value("pool")
    expect(value).to_have_value("edge")
    check(len(requests) == baseline, "Enter on a filled label sent a write")
    key.fill("")
    value.fill("")
    expect(key).to_have_value("")
    expect(value).to_have_value("")
    save.focus()
    expect(save).to_be_focused()
    save.press("Enter")
    expect(modal.get_by_text(ui["labels"]["keyRequired"], exact=True)).to_be_visible()
    expect(key).to_have_attribute("aria-invalid", "true")
    check(len(requests) == baseline, "Enter on Save with an empty key sent a label write")
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    boxes = {label: _box(locator) for label, locator in (
        ("key", key), ("value", value), ("save", save), ("cancel", cancel))}
    for label, box in boxes.items():
        check(box["width"] > 0 and box["height"] > 0, f"{label} has no box")
        check(-1 <= box["left"] and box["right"] <= view["width"] + 1
              and -1 <= box["top"] and box["bottom"] <= view["height"] + 1,
              f"{label} is outside the viewport {box}")
    check(not _overlaps(boxes["key"], boxes["save"]), "the key overlaps Save")
    check(not _overlaps(boxes["value"], boxes["save"]), "the value overlaps Save")
    check(not _overlaps(boxes["key"], boxes["cancel"]), "the key overlaps Cancel")
    check(not _overlaps(boxes["value"], boxes["cancel"]), "the value overlaps Cancel")
    check(len(requests) == baseline, "reading the label keyboard sent a write")
    return {"tab": ["key", "value", "save"], "enter_submitted": False,
            "empty_key_error": True, "viewport": view}
