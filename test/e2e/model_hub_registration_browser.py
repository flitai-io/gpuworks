"""Three independent browser metadata registration contracts, no downloads/GPU.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
CLI: model_hub_registration.py --case huggingface|modelscope|quick --locale en|zh.
"""
import hashlib
import json
import re
import time
from urllib.parse import quote, urlsplit
from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect
from dialog_checks import assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import button, check
from model_hub_fixture import CONFIG, INDEX, README, SHA, WEIGHT_BYTES
from model_hub_fixture_setup import MODELS, SPEC
from model_detail_dialog import model_detail_dialog
from model_hub_quick_inventory import exercise_quick_weight_inventory
from model_hub_register_inventory import exercise_hub_register_inventory, expect_hub_source_selected


def strict(actual, expected, label):
    check(json.dumps(actual, sort_keys=True, ensure_ascii=False) == json.dumps(expected, sort_keys=True, ensure_ascii=False), label)


def texts(locale, namespace):
    return json.loads((ROOT / f"ui/src/i18n/locales/{locale}/{namespace}.json").read_text())


def poll(page, read, label, timeout=20):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = read()
        if value:
            return value
        page.wait_for_timeout(100)
    raise AssertionError("timed out: " + label)


def begin(page, context, server, artifacts, report, locale):
    report["sensitive_phase"] = True
    auth = texts(locale, "auth")
    page.goto(server.base + "/models")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    labels = texts(locale, "models")
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    page.get_by_role("img", name=labels["view"]["list"], exact=True).click()
    mutations, failures, errors, consoles = [], [], [], []
    report.update({"requests": mutations, "browser_failures": failures, "page_errors": errors, "console_errors": consoles, "evaluations": []})
    def requested(request):
        parts = urlsplit(request.url)
        if request.method in ("POST", "PUT", "PATCH", "DELETE") and (parts.path == MODELS or parts.path.startswith(MODELS + "/") or parts.path.startswith("/api/v1/deployments")):
            mutations.append({"method": request.method, "path": parts.path, "query": parts.query,
                              "body": request.post_data_json if request.post_data else None})
    page.on("request", requested)
    page.on("request", lambda request: report["evaluations"].append(request.post_data_json)
            if urlsplit(request.url).path == "/api/v1/evaluate" and request.method == "POST" else None)
    page.on("response", lambda r: failures.append({"method": r.request.method, "url": r.url, "status": r.status}) if r.status >= 400 else None)
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("console", lambda message: consoles.append({"url": message.location.get("url", ""), "text": message.text}) if message.type == "error" else None)
    return labels


def listed(server, token, data):
    code, result = http(server.base, "GET", MODELS + "?source_id=" + quote(data["repo"], safe=""), token)
    check(code == 200 and result["total"] == len(result["items"]), "independent model list failed")
    check(all(m["source_id"] == data["repo"] for m in result["items"]), "lookup included foreign repository")
    return result["items"]


def read(server, token, ident):
    code, document = http(server.base, "GET", MODELS + "/" + str(ident), token)
    check(code == 200, "independent model GET failed")
    return document


def facts(document, data, source, revision, known=True, name=None):
    expected = {"source": source, "source_id": data["repo"], "revision": revision,
        "name": name or data["name"], "arch": "Qwen2ForCausalLM" if known else "",
        "weight_gib": WEIGHT_BYTES / 1024 ** 3 if known else 0}
    if known:
        expected.update({"context_length": 4096, "precision": "float16", "readme": README})
    else:
        check(document.get("context_length", 0) == 0 and document.get("precision", "") == ""
              and document.get("readme", "") == "", "outage fabricated known metadata")
    for key, value in expected.items():
        strict(document.get(key), value, "independent metadata mismatch: " + key)
    check(not document.get("nodes") and not document.get("storages"), "metadata registration invented a file copy")


def visible_action(scope, label):
    text = r"\s*".join(label) if re.fullmatch(r"[\u3400-\u9fff]{2}", label) else re.escape(label)
    return scope.get_by_role("button").filter(has_text=re.compile("^" + text + "$"))


def choose_option(page, control, label):
    # AntD's accessibility options are a hidden virtual list. Click the
    # visible option in this control's actual popup, as the user does.
    control.click()
    popup_id = control.get_attribute("aria-controls")
    check(bool(popup_id), "source selector has no controlled popup")
    popup = page.locator("[id=" + json.dumps(popup_id) + "]").locator("..")
    popup.get_by_text(label, exact=True).filter(visible=True).click()


def registration(page, labels, source, repo, name, revision="", report=None, server=None, token=None, data=None):
    trigger = visible_action(page, labels["action"]["register"]).first
    title = labels["action"]["register"]
    posts = []
    page.on("request", lambda request: posts.append(MODELS)
            if request.method == "POST" and urlsplit(request.url).path == MODELS else None)
    before = listed(server, token, data) if server is not None else None

    def reopen():
        trigger.click()
        opened = page.get_by_role("dialog", name=title, exact=True)
        expect(opened).to_be_visible()
        return opened

    opened = reopen()
    if source == "modelscope":
        if report is not None:
            report["last_step"] = "hub-modal-mask"
        mask_el = opened.locator(
            "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-modal-root ')]"
            "/div[contains(concat(' ', normalize-space(@class), ' '), ' ant-modal-mask ')]")
        expect(mask_el).to_have_count(1)
        expect(mask_el).to_be_visible(timeout=12000)
        expect(mask_el).not_to_have_class(re.compile(r"\bant-modal-mask-hidden\b"), timeout=12000)
        if report is not None:
            report["register_modal_mask"] = True
    # The models toolbar stays mounted. This dialog is not nested in another dialog.
    dialog = assert_nested_dismissals(opened, page.locator(".model-content-toolbar").first, reopen, [
        ("cancel", cancel_button(labels["action"]["cancel"])), ("escape", escape),
        ("close", close_button), ("mask", mask)], lambda: len(posts), trigger)
    check(posts == [], "register dialog dismissal created a model")
    if before is not None:
        check(listed(server, token, data) == before, "register dialog dismissal changed the model list")
    if report is not None:
        report.setdefault("evidence", []).append({
            "layer": "ui", "register_dialog": title, "dismissals": ["cancel", "escape", "close", "mask"],
            "focus_returned": True, "register_posts": 0})
        report["last_step"] = "hub-collapse-small"
    source_field = dialog.get_by_label(labels["column"]["source"], exact=True)
    inventory = exercise_hub_register_inventory(dialog, labels)
    if source == "modelscope":
        if report is not None:
            report["last_step"] = "hub-collapse-icon-start"
        expect(dialog.locator(".ant-collapse-ghost")).to_have_class(
            re.compile(r"\bant-collapse-icon-position-start\b"), timeout=12000)
    chosen = "Hugging Face" if source == "huggingface" else "ModelScope"
    choose_option(page, source_field, chosen)
    inventory["selected_source"] = expect_hub_source_selected(dialog, labels, chosen)
    if report is not None:
        report["register_inventory"] = inventory
        report.setdefault("evidence", []).append({"layer": "ui", "register_inventory": inventory})
    dialog.get_by_label(labels["column"]["sourceID"], exact=True).fill(repo)
    dialog.get_by_label(labels["column"]["name"], exact=True).fill(name)
    dialog.get_by_text(labels["register"]["advanced"], exact=True).click()
    revision_input = dialog.get_by_label(labels["detail"]["revision"], exact=True)
    expect(revision_input).to_have_value("")
    if revision:
        revision_input.fill(revision)
    submit = dialog.get_by_role("button").filter(has_text=re.compile("^" + re.escape(labels["register"]["confirm"]) + "$"))
    # Register confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    assert_dialog_shell(page, dialog, source_field, submit)
    if report is not None:
        report.setdefault("evidence", []).append({
            "layer": "ui", "dialog_shell": labels["action"]["register"], "viewport": "1440x900"})
    return dialog, submit


def submit_create(page, submit, report):
    with page.expect_response(lambda r: urlsplit(r.url).path == MODELS and r.request.method == "POST") as response:
        submit.click()
    document = response.value.json()
    if isinstance(document, dict) and type(document.get("id")) is int:
        report.setdefault("owned_model_ids", []).append(document["id"])
    return response.value, document


def hold_first_create(page, submit, report, body):
    # Metadata is read before the create POST. Loading is already on while that
    # read runs, so the second click happens before the held POST exists.
    held = []
    pattern = re.compile(re.escape(MODELS) + r"(?:\?|$)")

    def hold_create(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != MODELS:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_create)
    submit.click()
    report["last_step"] = "hub-register-inflight"
    expect(submit).to_have_class(re.compile(r"\bant-btn-loading\b"))
    submit.click()
    poll(page, lambda: held or None, "held model registration")
    check(len(held) == 1, "duplicate model registration while the first request is held")
    check(held[0].request.post_data_json == body, "held registration body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == MODELS and r.request.method == "POST") as response:
        held[0].continue_()
    page.unroute(pattern, hold_create)
    document = response.value.json()
    if isinstance(document, dict) and type(document.get("id")) is int:
        report.setdefault("owned_model_ids", []).append(document["id"])
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
        "failure_status": response.value.status, "draft_retained": True, "failure_reason_visible": True})
    return response.value, document


def hold_refresh(page, refresh, report, path):
    # setRefreshing runs before the refresh POST, so loading is already on
    # while the held request is still waiting to be sent.
    held = []
    pattern = re.compile(re.escape(path) + r"(?:\?|$)")

    def hold_post(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    refresh.click()
    report["last_step"] = "hub-refresh-inflight"
    expect(refresh).to_have_class(re.compile(r"\bant-btn-loading\b"))
    refresh.click()
    poll(page, lambda: held or None, "held metadata refresh")
    check(len(held) == 1, "duplicate metadata refresh while the first request is held")
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as response:
        held[0].continue_()
    page.unroute(pattern, hold_post)
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
        "failure_status": response.value.status, "row_retained": True, "failure_reason_visible": True})
    return response.value


def hold_quick(page, submit, report, path, body):
    # Lookup finishes before setRegistering, so the loading class appears
    # once register() starts and the quick-register POST is already held.
    held = []
    pattern = re.compile(re.escape(path) + r"(?:\?|$)")

    def hold_post(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    submit.click()
    report["last_step"] = "hub-quick-inflight"
    expect(submit).to_have_class(re.compile(r"\bant-btn-loading\b"))
    submit.click()
    poll(page, lambda: held or None, "held quick registration")
    check(len(held) == 1, "duplicate quick registration while the first request is held")
    check(held[0].request.post_data_json == body, "held quick registration body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as response:
        held[0].fulfill(status=502, content_type="application/json",
                        body=json.dumps({"message": "upstream fixture is unreachable"}))
    page.unroute(pattern, hold_post)
    check(response.value.status == 502, "quick registration failure was not a 502")
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
        "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})
    return response.value


def saved_ui(page, server, labels, data, ident, source, revision, token):
    page.goto(server.base + "/models?detail=" + str(ident))
    dialog = model_detail_dialog(page, labels, data["name"])
    expect(dialog).to_be_in_viewport(ratio=1)
    heading_repo = dialog.get_by_title(labels["column"]["sourceID"], exact=True)
    expect(heading_repo).to_have_text(data["repo"])
    expect(heading_repo).to_be_visible()
    expect(dialog.get_by_text(revision, exact=True)).to_be_visible()
    expect(dialog.get_by_text("Qwen2ForCausalLM", exact=True)).to_be_visible()
    expect(dialog.get_by_text("float16", exact=True)).to_be_visible()
    expect(dialog.get_by_text("4,096", exact=True)).to_be_visible()
    facts(read(server, token, ident), data, source, revision)
    visible_action(dialog, labels["action"]["close"]).click()
    expect(dialog).not_to_be_visible()
    row = page.get_by_role("row").filter(has_text=data["name"])
    expect(row).to_have_count(1)
    return row


def model_delete_wire(ident):
    return {"method": "DELETE", "path": MODELS + "/" + str(ident), "query": "", "body": None}


def delete_ui(page, server, labels, data, ident, token, report, locale):
    from http_model_registration import (
        clear_deleting_file, clear_model_reference, model_db_count, place_deleting_file, place_model_reference)
    common = texts(locale, "common")
    path = MODELS + "/" + str(ident)
    name = labels["delete"]["confirm"].replace("{{name}}", data["name"])

    def open_delete():
        row = page.get_by_role("row").filter(has_text=data["name"])
        button(row, labels["action"]["more"]).click()
        page.get_by_role("menu").get_by_text(labels["action"]["deleteModel"], exact=True).click()
        modal = page.get_by_role("dialog", name=name, exact=True)
        expect(modal).to_be_visible()
        expect(modal.get_by_text(labels["delete"]["listDescription"], exact=True)).to_be_visible()
        return modal

    place_model_reference(server, ident)
    modal = open_delete()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as blocked:
        button(modal, labels["action"]["delete"]).click()
    blocked_response = blocked.value
    check(blocked_response.status == 409, f"reference block status {blocked_response.status} {blocked_response.text()}")
    blocked_body = blocked_response.json()
    check(blocked_body.get("code") == "MODEL_IN_USE", f"reference block code {blocked_body}")
    check(blocked_response.request.post_data is None and not urlsplit(blocked_response.url).query,
          "reference block sent a body or query")
    blocked_reason = common["apiError"]["MODEL_IN_USE"].split("{{")[0].strip()
    expect(page.get_by_text(re.compile(re.escape(blocked_reason))).first).to_be_visible()
    expect(modal).not_to_be_visible()
    code, _ = http(server.base, "GET", path, token)
    check(code == 200, "reference block deleted the model")
    check(model_db_count(server, "SELECT COUNT(*) FROM deployments WHERE model_id = ?", (ident,)) == 1,
          "reference block removed the deployment")
    report["last_step"] = "model-delete-reference-block"
    clear_model_reference(server, ident)
    place_deleting_file(server, ident)
    modal = open_delete()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as pending:
        button(modal, labels["action"]["delete"]).click()
    pending_response = pending.value
    check(pending_response.status == 409, f"deleting file status {pending_response.status}")
    pending_body = pending_response.json()
    check("cache deletion is awaiting confirmation" in pending_body.get("error", ""),
          f"deleting file did not keep the model {pending_body}")
    expect(page.get_by_text(re.compile(r"cache deletion is awaiting confirmation")).first).to_be_visible()
    expect(modal).not_to_be_visible()
    code, _ = http(server.base, "GET", path, token)
    check(code == 200, "cache-deletion refusal removed the model")
    check(model_db_count(server, "SELECT COUNT(*) FROM model_files WHERE model_id = ? AND state = ?",
                         (ident, "deleting")) == 1, "cache-deletion refusal removed the file")
    report["last_step"] = "model-delete-cache-pending"
    clear_deleting_file(server, ident)
    modal = open_delete()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as deleted:
        button(modal, labels["action"]["delete"]).click()
    check(deleted.value.status == 204, "model UI deletion failed")
    code, _ = http(server.base, "GET", path, token)
    check(code == 404 and listed(server, token, data) == [], "deleted model remains in authority")
    expect(page.get_by_role("row").filter(has_text=data["name"])).to_have_count(0)
    report["evidence"].append({
        "layer": "ui", "delete_dialog": name, "impact": labels["delete"]["listDescription"],
        "reference_block_code": "MODEL_IN_USE", "cache_deletion_status": 409, "delete_status": 204})
    report["last_step"] = "browser deletion independently observed"



def applied_model(page, report, ident, repo, label, offset):
    # ModelPicker's selected label proves what the user sees. The actual
    # evaluate wire binds that selection to the exact numeric model ID.
    control = page.locator("#model_id")
    selection = control.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-select ')][1]").locator(".ant-select-selection-item")
    expect(selection).to_have_count(1)
    expect(selection).to_contain_text(repo)
    expect(selection.locator(".ant-tag")).to_have_text("MS")
    actual = poll(page, lambda: next((request for request in reversed(report["evaluations"][offset:])
        if request.get("model_id") == ident), None), label + " evaluate exact selected model_id")
    strict(actual["model_id"], ident, label + " model identity")
    report.setdefault("selected_model_evidence", []).append({"action": label,
        "model_id": ident, "selected_label": selection.inner_text(), "evaluate_request": actual})


def finish(report, server, data, allowed_management):
    origin = data["hub"].base
    allowed_urls = {server.base + path: codes for path, codes in allowed_management.items()}
    for item in report["browser_failures"]:
        if item["url"].startswith(origin + "/") and item["status"] == 503:
            continue
        check(item["status"] in allowed_urls.get(item["url"], []), "unexpected browser HTTP error")
    for error in report["console_errors"]:
        url = error["url"]
        allowed = ([503] if url.startswith(origin + "/") else allowed_urls.get(url, []))
        check(any(str(code) in error["text"] for code in allowed), "unexpected browser console error")
    check(report["page_errors"] == [], "unhandled browser page error")
    report["evidence"].extend([{"layer": "ui", "assertions": "named controls, draft/persisted values, real source branch, exact metadata, deletion"},
        {"layer": "request", "mutations": report["requests"]},
        {"layer": "persistence", "observations": report["observations"]},
        {"layer": "source", "audit": data["hub"].audit(), "scope": "actual metadata HTTP; no weight download, agent or GPU execution"}])


def wire(body, path=MODELS):
    return {"method": "POST", "path": path, "query": "", "body": body}


# Contract: gpuworks/models/huggingface-browser-registration
# Mainline: source rules select real local HF -> blank revision reads browser SHA/config/index ->
# real 257-char name400, draft retained and zero records -> corrected201 -> same-artifact
# duplicate201 reuses exactID -> refresh/detail independent exact metadata -> UI delete404.
def test_huggingface(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale)
    report["step"] = "Hugging Face invalid name and exact browser metadata"
    check(listed(server, token, data) == [], "fixture models not empty")
    dialog, submit = registration(page, labels, "huggingface", data["repo"], "x" * 257, report=report, server=server, token=token, data=data)
    offset = len(data["hub"].audit())
    body = {"source": "huggingface", "source_id": data["repo"], "revision": SHA, "name": "x" * 257,
            "readme": README, "config_raw": CONFIG, "weight_bytes": WEIGHT_BYTES, "logo_data": "", "browser_metadata": True}
    rejected, error = hold_first_create(page, submit, report, body)
    check(rejected.status == 400 and error["code"] == "FIELD_INVALID" and error["params"]["field"] == "name", "expected actual backend name validation")
    strict(report["requests"], [wire(body)], "HF rejected full body/count/order differs")
    expect(dialog.get_by_label(labels["column"]["name"], exact=True)).to_have_value("x" * 257)
    expect(dialog.get_by_label(labels["column"]["sourceID"], exact=True)).to_have_value(data["repo"])
    expect(dialog.get_by_label(labels["detail"]["revision"], exact=True)).to_have_value("")
    expect(page.get_by_text(re.compile(re.escape(labels["register"]["failed"].split("{{detail}}")[0])))).to_be_visible()
    check(listed(server, token, data) == [], "invalid name partially persisted a model")
    dialog.get_by_label(labels["column"]["name"], exact=True).fill(data["name"])
    response, created = submit_create(page, submit, report)
    check(response.status == 201, "HF registration failed")
    body = dict(body, name=data["name"])
    strict(report["requests"], [wire(dict(body, name="x" * 257)), wire(body)], "HF successful full body/count differs")
    ident = created["id"]
    facts(read(server, token, ident), data, "huggingface", SHA)
    dialog, submit = registration(page, labels, "huggingface", data["repo"], data["name"], report=report, server=server, token=token, data=data)
    response, duplicate = submit_create(page, submit, report)
    check(response.status == 201 and duplicate["id"] == ident and len(listed(server, token, data)) == 1, "HF duplicate changed artifact identity")
    audit = data["hub"].audit()[offset:]
    expected_paths = {"/hf/" + data["repo"] + "/raw/main/" + f: hashlib.sha256(v.encode()).hexdigest()
        for f, v in (("README.md", README), ("config.json", CONFIG), ("model.safetensors.index.json", INDEX))}
    expected_paths["/hf/api/models/" + data["repo"] + "?blobs=true"] = None
    strict(sorted(r["target"] for r in audit), sorted(list(expected_paths) * 3), "HF browser did not make exact per-submit reads")
    for request in audit:
        check(request["origin"] == server.base and request["status"] == 200, "HF metadata did not come from real browser source")
        if expected_paths[request["target"]]:
            strict(request["sha256"], expected_paths[request["target"]], "HF fixture sent different authoritative bytes")
    report["observations"] = {"created_id": ident, "duplicate_id": duplicate["id"], "saved": read(server, token, ident), "rejected": error}
    saved_ui(page, server, labels, data, ident, "huggingface", SHA, token)
    delete_ui(page, server, labels, data, ident, token, report, locale)
    strict(report["requests"], [wire(dict(body, name="x" * 257)), wire(body), wire(body),
        *([model_delete_wire(ident)] * 3)], "HF final mutation sequence differs")
    filled_name = data["repo"] + "-typed"
    report["last_step"] = "hub-optional-filled"
    _, filled_submit = registration(page, labels, "huggingface", data["repo"], filled_name, "main",
                                    report=report, server=server, token=token, data=data)
    filled_response, filled = submit_create(page, filled_submit, report)
    check(filled_response.status == 201, "filled optional registration failed")
    filled_body = filled_response.request.post_data_json
    check(filled_body["source"] == "huggingface" and filled_body["source_id"] == data["repo"]
          and filled_body["name"] == filled_name and filled_body["revision"] == "main",
          "filled optional body differs")
    filled_saved = read(server, token, filled["id"])
    check(filled_saved["source"] == "huggingface" and filled_saved["source_id"] == data["repo"]
          and filled_saved["name"] == filled_name and filled_saved["revision"] == "main",
          "filled optional was not stored")
    filled_path = MODELS + "/" + str(filled["id"])
    code, _ = http(server.base, "DELETE", filled_path, token)
    check(code == 204, "filled optional delete failed")
    check(listed(server, token, data) == [], "filled optional model remains listed")
    omit_dialog, omit_submit = registration(page, labels, "huggingface", data["repo"], "",
                                            report=report, server=server, token=token, data=data)
    report["last_step"] = "hub-source-id-outlined"
    source_id = omit_dialog.get_by_label(labels["column"]["sourceID"], exact=True)
    expect(source_id).to_have_value(data["repo"])
    expect(source_id).to_have_class(re.compile(r"\bant-input-outlined\b"), timeout=12000)
    expect(omit_dialog.get_by_label(labels["column"]["name"], exact=True)).to_have_value("")
    expect(omit_dialog.get_by_label(labels["detail"]["revision"], exact=True)).to_have_value("")
    report["source_id_outlined"] = True
    report["last_step"] = "hub-optional-omitted"
    omit_response, omitted = submit_create(page, omit_submit, report)
    check(omit_response.status == 201, "omitted optional registration failed")
    check(omit_response.request.method == "POST" and urlsplit(omit_response.url).path == MODELS,
          "minimal registration method or path differs")
    omit_body = omit_response.request.post_data_json
    check(omit_body["source"] == "huggingface" and omit_body["source_id"] == data["repo"]
          and "name" not in omit_body and omit_body["revision"] == SHA,
          "omitted optional body differs")
    omit_saved = read(server, token, omitted["id"])
    check(omit_saved["source"] == "huggingface" and omit_saved["source_id"] == data["repo"]
          and omit_saved["name"] == data["repo"] and omit_saved["name"] != filled_name
          and omit_saved["revision"] == SHA,
          "omitted display name was not stored as the repository id")
    report["last_step"] = "hub-minimal-detail"
    page.goto(server.base + "/models?detail=" + str(omitted["id"]))
    detail = model_detail_dialog(page, labels, data["repo"])
    expect(detail).to_be_in_viewport(ratio=1)
    heading_repo = detail.get_by_title(labels["column"]["sourceID"], exact=True)
    expect(heading_repo).to_have_text(data["repo"])
    expect(detail.get_by_text(SHA, exact=True)).to_be_visible()
    expect(detail.get_by_text("Qwen2ForCausalLM", exact=True)).to_be_visible()
    expect(detail.get_by_text("float16", exact=True)).to_be_visible()
    expect(detail.get_by_text("4,096", exact=True)).to_be_visible()
    facts(read(server, token, omitted["id"]), data, "huggingface", SHA, name=data["repo"])
    visible_action(detail, labels["action"]["close"]).click()
    expect(detail).not_to_be_visible()
    expect(page.get_by_role("row").filter(has_text=data["repo"])).to_have_count(1)
    check(len(listed(server, token, data)) == 1, "minimal registration list length differs")
    page.reload()
    row = page.get_by_role("row").filter(has_text=data["repo"])
    expect(row).to_have_count(1)
    report["last_step"] = "hub-source-tag"
    source_tag = row.locator(".ant-tag-gold")
    expect(source_tag).to_be_visible(timeout=12000)
    expect(source_tag).to_have_text("HF")
    report["source_tag_gold"] = True
    page.goto(server.base + "/models?detail=" + str(omitted["id"]))
    reloaded = model_detail_dialog(page, labels, data["repo"])
    expect(reloaded).to_be_visible(timeout=12000)
    expect(reloaded.get_by_title(labels["column"]["sourceID"], exact=True)).to_have_text(data["repo"])
    expect(reloaded.get_by_text(SHA, exact=True)).to_be_visible()
    expect(reloaded.get_by_text("Qwen2ForCausalLM", exact=True)).to_be_visible()
    expect(reloaded.get_by_text("float16", exact=True)).to_be_visible()
    expect(reloaded.get_by_text("4,096", exact=True)).to_be_visible()
    facts(read(server, token, omitted["id"]), data, "huggingface", SHA, name=data["repo"])
    visible_action(reloaded, labels["action"]["close"]).click()
    expect(reloaded).not_to_be_visible()
    omit_path = MODELS + "/" + str(omitted["id"])
    code, _ = http(server.base, "DELETE", omit_path, token)
    check(code == 204, "omitted optional delete failed")
    code, _ = http(server.base, "GET", omit_path, token)
    check(code == 404, "omitted optional model remains readable")
    report["filled_optional"] = {"name": filled_name, "revision": "main"}
    report["omitted_optional"] = {"request_name_omitted": True, "stored_name": data["repo"],
                                  "stored_revision": SHA}
    report["last_step"] = "hub-optional-stored"
    finish(report, server, data, {MODELS: [400], MODELS + "/" + str(ident): [409]})


# Contract: gpuworks/models/modelscope-registration-recovery
# Mainline: 257-char name400 keeps the dialog and draft -> corrected name with
# explicit main -> real server converts master; upstream503 still201 with
# notice and unknown metadata -> UI refresh502 preserves same row -> source recovery
# refresh200 exact bytes/sameID -> duplicate201 same artifact -> reload/delete404.
def test_modelscope(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale)
    data["hub"].available(False)
    dialog, submit = registration(page, labels, "modelscope", data["repo"], data["name"], "main", report=report, server=server, token=token, data=data)
    rejected_name = "x" * 257
    dialog.get_by_label(labels["column"]["name"], exact=True).fill(rejected_name)
    report["last_step"] = "hub-name-rejection"
    rejected, error = submit_create(page, submit, report)
    check(rejected.status == 400 and error["code"] == "FIELD_INVALID" and error["params"]["field"] == "name",
          "expected actual backend name validation")
    reason = page.locator(".ant-message-error")
    expect(reason).to_be_visible(timeout=12000)
    expect(reason).to_contain_text(labels["register"]["failed"].split("{{detail}}")[0].strip())
    expect(reason).to_contain_text("at most 256 characters")
    report["name_rejection"] = {"status": 400, "field": "name", "draft_retained": True, "error_notice": True}
    rejected_body = {"source": "modelscope", "source_id": data["repo"], "revision": "main", "name": rejected_name}
    strict(report["requests"], [wire(rejected_body)], "MS rejected body/count differs")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_label(labels["column"]["name"], exact=True)).to_have_value(rejected_name)
    expect(dialog.get_by_label(labels["column"]["sourceID"], exact=True)).to_have_value(data["repo"])
    expect(dialog.get_by_label(labels["detail"]["revision"], exact=True)).to_have_value("main")
    check(listed(server, token, data) == [], "invalid name persisted a model")
    dialog.get_by_label(labels["column"]["name"], exact=True).fill(data["name"])
    report["step"] = "ModelScope metadata unavailable registration remains valid"
    offset = len(data["hub"].audit())
    response, created = submit_create(page, submit, report)
    check(response.status == 201, "best-effort MS registration should succeed")
    ident = created["id"]
    expected = {"source": "modelscope", "source_id": data["repo"], "revision": "main", "name": data["name"]}
    strict(report["requests"], [wire(rejected_body), wire(expected)], "MS create body/count differs")
    facts(read(server, token, ident), data, "modelscope", "master", known=False)
    notice = created["metadata_notice"]
    strict({k: notice[k] for k in ("code", "params")}, {"code": "MODEL_METADATA_UNREACHABLE", "params": {
        "source": "modelscope", "hosts": urlsplit(data["hub"].base).netloc, "route": "/api/v1/pull/source-rules"}}, "MS missing-metadata notice differs")
    common = texts(locale, "common")
    notice_text = common["apiError"]["MODEL_METADATA_UNREACHABLE"].replace("{{hosts}}", urlsplit(data["hub"].base).netloc)
    expect(page.get_by_text(notice_text, exact=True)).to_be_visible()
    initial = read(server, token, ident)
    failed_targets = ["/ms/api/v1/models/" + data["repo"],
        *[f"/ms/models/{data['repo']}/resolve/master/{file}" for file in
          ("config.json", "README.md", "model.safetensors.index.json")],
        "/ms/api/v1/models/" + data["repo"] + "/repo/files?Recursive=true&Revision=master"]
    outage_reads = data["hub"].audit()[offset:]
    strict(sorted(r["target"] for r in outage_reads), sorted(failed_targets), "MS create outage exact nonempty source reads")
    check(all(r["status"] == 503 and r["origin"] == "" for r in outage_reads), "MS outage did not reach real server-side source")
    page.goto(server.base + "/models?detail=" + str(ident))
    detail = model_detail_dialog(page, labels, data["name"])
    refresh = visible_action(detail, labels["detail"]["refreshMetadata"])
    path = MODELS + f"/{ident}/metadata/refresh"
    failure_offset = len(data["hub"].audit())
    failed = hold_refresh(page, refresh, report, path)
    check(failed.status == 502 and failed.json()["code"] == notice["code"], "MS refresh outage not reported")
    strict(read(server, token, ident), initial, "failed refresh partially modified model")
    refresh_failure_reads = data["hub"].audit()[failure_offset:]
    strict(sorted(r["target"] for r in refresh_failure_reads), sorted(failed_targets), "MS refresh outage exact nonempty source reads")
    check(all(r["status"] == 503 and r["origin"] == "" for r in refresh_failure_reads), "MS failed refresh bypassed configured source")
    report["outage_source_evidence"] = {"create": outage_reads, "refresh": refresh_failure_reads}
    expect(page.get_by_text(re.compile(re.escape(labels["detail"]["message"]["refreshFailed"].split("{{detail}}")[0])))).to_be_visible()
    data["hub"].available(True)
    recovery_offset = len(data["hub"].audit())
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as refreshed:
        refresh.click()
    check(refreshed.value.status == 200, "MS metadata refresh recovery failed")
    facts(read(server, token, ident), data, "modelscope", "master")
    visible_action(detail, labels["action"]["close"]).click()
    expect(detail).not_to_be_visible()
    dialog, submit = registration(page, labels, "modelscope", data["repo"], data["name"], "main", report=report, server=server, token=token, data=data)
    response, duplicate = submit_create(page, submit, report)
    check(response.status == 201 and duplicate["id"] == ident and len(listed(server, token, data)) == 1, "MS duplicate did not reuse normalized artifact")
    reads = data["hub"].audit()[recovery_offset:]
    files = {f"/ms/models/{data['repo']}/resolve/master/{file}": value for file, value in
             (("config.json", CONFIG), ("README.md", README), ("model.safetensors.index.json", INDEX))}
    expected_targets = ["/ms/api/v1/models/" + data["repo"], *files]
    strict(sorted(r["target"] for r in reads), sorted(expected_targets * 2), "MS exact recovery/duplicate protocol reads differ")
    for request in reads:
        check(request["origin"] == "" and request["status"] == 200, "MS metadata incorrectly fetched in browser")
        if request["target"] in files:
            strict(request["sha256"], hashlib.sha256(files[request["target"]].encode()).hexdigest(), "MS authority bytes drift")
    report["observations"] = {"degraded": initial, "notice": notice, "refresh_error": failed.json(), "saved": read(server, token, ident), "duplicate_id": duplicate["id"]}
    saved_ui(page, server, labels, data, ident, "modelscope", "master", token)
    delete_ui(page, server, labels, data, ident, token, report, locale)
    strict(report["requests"], [wire(rejected_body), wire(expected), wire(None, path), wire(None, path), wire(expected),
        *([model_delete_wire(ident)] * 3)], "MS final full mutation sequence differs")
    filled_name = data["repo"] + "-typed"
    _, filled_submit = registration(page, labels, "modelscope", data["repo"], filled_name, "master",
                                    report=report, server=server, token=token, data=data)
    report["last_step"] = "hub-optional-filled"
    filled_response, filled = submit_create(page, filled_submit, report)
    check(filled_response.status == 201, "filled optional registration failed")
    filled_body = filled_response.request.post_data_json
    check(filled_body["source"] == "modelscope" and filled_body["source_id"] == data["repo"]
          and filled_body["name"] == filled_name and filled_body["revision"] == "master",
          "filled optional body differs")
    filled_saved = read(server, token, filled["id"])
    check(filled_saved["source"] == "modelscope" and filled_saved["source_id"] == data["repo"]
          and filled_saved["name"] == filled_name and filled_saved["revision"] == "master",
          "filled optional was not stored")
    filled_path = MODELS + "/" + str(filled["id"])
    code, _ = http(server.base, "DELETE", filled_path, token)
    check(code == 204, "filled optional delete failed")
    check(listed(server, token, data) == [], "filled optional model remains listed")
    omit_dialog, omit_submit = registration(page, labels, "modelscope", data["repo"], "",
                                            report=report, server=server, token=token, data=data)
    report["last_step"] = "hub-optional-omitted"
    expect(omit_dialog.get_by_label(labels["column"]["name"], exact=True)).to_have_value("")
    expect(omit_dialog.get_by_label(labels["detail"]["revision"], exact=True)).to_have_value("")
    omit_response, omitted = submit_create(page, omit_submit, report)
    check(omit_response.status == 201, "omitted optional registration failed")
    check(omit_response.request.method == "POST" and urlsplit(omit_response.url).path == MODELS,
          "minimal registration method or path differs")
    omit_body = omit_response.request.post_data_json
    check(omit_body["source"] == "modelscope" and omit_body["source_id"] == data["repo"]
          and "name" not in omit_body and omit_body["revision"] == "",
          "omitted optional body differs")
    omit_saved = read(server, token, omitted["id"])
    check(omit_saved["source"] == "modelscope" and omit_saved["source_id"] == data["repo"]
          and omit_saved["name"] == data["repo"] and omit_saved["name"] != filled_name
          and omit_saved["revision"] == SHA,
          "omitted display name was not stored as the repository id")
    report["last_step"] = "hub-minimal-detail"
    page.goto(server.base + "/models?detail=" + str(omitted["id"]))
    detail = model_detail_dialog(page, labels, data["repo"])
    expect(detail).to_be_in_viewport(ratio=1)
    heading_repo = detail.get_by_title(labels["column"]["sourceID"], exact=True)
    expect(heading_repo).to_have_text(data["repo"])
    expect(detail.get_by_text(SHA, exact=True)).to_be_visible()
    expect(detail.get_by_text("Qwen2ForCausalLM", exact=True)).to_be_visible()
    expect(detail.get_by_text("float16", exact=True)).to_be_visible()
    expect(detail.get_by_text("4,096", exact=True)).to_be_visible()
    facts(read(server, token, omitted["id"]), data, "modelscope", SHA, name=data["repo"])
    visible_action(detail, labels["action"]["close"]).click()
    expect(detail).not_to_be_visible()
    expect(page.get_by_role("row").filter(has_text=data["repo"])).to_have_count(1)
    check(len(listed(server, token, data)) == 1, "minimal registration list length differs")
    page.reload()
    row = page.get_by_role("row").filter(has_text=data["repo"])
    expect(row).to_have_count(1)
    report["last_step"] = "hub-ms-source-tag"
    source_tag = row.locator(".ant-tag-blue", has_text="MS")
    expect(source_tag).to_be_visible(timeout=12000)
    expect(source_tag).to_have_text("MS")
    report["source_tag_blue"] = True
    page.goto(server.base + "/models?detail=" + str(omitted["id"]))
    reloaded = model_detail_dialog(page, labels, data["repo"])
    expect(reloaded).to_be_visible(timeout=12000)
    expect(reloaded.get_by_title(labels["column"]["sourceID"], exact=True)).to_have_text(data["repo"])
    expect(reloaded.get_by_text(SHA, exact=True)).to_be_visible()
    expect(reloaded.get_by_text("Qwen2ForCausalLM", exact=True)).to_be_visible()
    expect(reloaded.get_by_text("float16", exact=True)).to_be_visible()
    expect(reloaded.get_by_text("4,096", exact=True)).to_be_visible()
    facts(read(server, token, omitted["id"]), data, "modelscope", SHA, name=data["repo"])
    visible_action(reloaded, labels["action"]["close"]).click()
    expect(reloaded).not_to_be_visible()
    omit_path = MODELS + "/" + str(omitted["id"])
    code, _ = http(server.base, "DELETE", omit_path, token)
    check(code == 204, "omitted optional delete failed")
    code, _ = http(server.base, "GET", omit_path, token)
    check(code == 404, "omitted optional model remains readable")
    report["filled_optional"] = {"name": filled_name, "revision": "master"}
    report["omitted_optional"] = {"request_name_omitted": True, "request_revision_empty": True,
                                  "stored_name": data["repo"], "stored_revision": SHA}
    report["last_step"] = "hub-optional-stored"
    finish(report, server, data, {MODELS: [400], path: [502], MODELS + "/" + str(ident): [409]})


# Contract: gpuworks/models/template-quick-registration
# Mainline: owned two-source template -> real Deploy action -> explicitly choose MS
# -> quick201 + background source reads -> sameID metadata -> reopen template and
# confirm matching registered model with zero newPOST -> reload model -> UI delete404.
def test_quick(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale)
    templates, deploy = texts(locale, "inferenceTemplates"), texts(locale, "deploy")
    page.goto(server.base + "/inference-templates/" + data["template_model_id"])
    expect(page.get_by_role("heading", name=data["template"]["display_title"], exact=True)).to_be_visible()
    visible_action(page, templates["detail"]["deploy"]).click()
    modal = page.get_by_role("dialog", name=deploy["action"]["selectWeightSource"], exact=True)
    choose = modal.get_by_role("combobox", name=deploy["action"]["selectSourcePlaceholder"], exact=True)
    expect(choose).to_have_value("")
    expect(button(modal, deploy["action"]["registerAndDeploy"])).to_be_disabled()
    inventory = exercise_quick_weight_inventory(page, modal, deploy, data["repo"])
    report["quick_inventory"] = inventory
    report.setdefault("evidence", []).append({"layer": "ui", "quick_inventory": inventory})
    report["last_step"] = "quick-modal-not-centered"
    wrap = modal.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-modal-wrap ')][1]")
    expect(wrap).to_have_count(1)
    expect(wrap).not_to_have_class(re.compile(r"\bant-modal-centered\b"), timeout=12000)
    report["quick_modal_centered"] = False
    path = MODELS + "/quick-register"
    dismissal_posts = []
    page.on(
        "request",
        lambda request: dismissal_posts.append(request.url)
        if request.method == "POST" and urlsplit(request.url).path == path else None,
    )
    box = {"modal": modal}

    def reopen_weight():
        # Closing the dialog leaves prompted set on this deploy page, so the
        # same visit will not open it again. A fresh template deploy remounts it.
        page.goto(server.base + "/inference-templates/" + data["template_model_id"])
        expect(page.get_by_role("heading", name=data["template"]["display_title"], exact=True)).to_be_visible()
        visible_action(page, templates["detail"]["deploy"]).click()
        opened = page.get_by_role("dialog", name=deploy["action"]["selectWeightSource"], exact=True)
        expect(opened).to_be_visible(timeout=12000)
        expect(opened.get_by_role("combobox", name=deploy["action"]["selectSourcePlaceholder"], exact=True)).to_have_value("")
        expect(button(opened, deploy["action"]["registerAndDeploy"])).to_be_disabled()
        return opened

    def dismiss_weight(label, action):
        report["last_step"] = "quick-dismiss-" + label
        action(box["modal"])
        expect(box["modal"]).to_be_hidden(timeout=12000)
        expect(page).to_have_url(re.compile(r"/deploy(?:\?|$)"), timeout=12000)
        expect(page.get_by_role("dialog")).to_have_count(0)
        check(dismissal_posts == [], label + " sent a quick registration")
        check(listed(server, token, data) == [], label + " stored a model")
        box["modal"] = reopen_weight()

    dismiss_weight("cancel", cancel_button(deploy["shared"]["cancel"]))
    dismiss_weight("close", close_button)
    dismiss_weight("escape", escape)
    # The mask is the fourth dismissal. It is driven through the shared helper so the
    # click lands on the wrap rather than somewhere inside the panel, which is where an
    # earlier hand-written attempt was silently ineffective.
    dismiss_weight("mask", mask)
    modal = box["modal"]
    choose = modal.get_by_role("combobox", name=deploy["action"]["selectSourcePlaceholder"], exact=True)
    choose_option(page, choose, deploy["action"]["modelScope"] + " · " + data["repo"])
    first_evaluate_offset = len(report["evaluations"])
    expected = {"source": "modelscope", "source_id": data["repo"], "revision": "main", "name": data["repo"],
                "weight_bytes": WEIGHT_BYTES, "quantization": "fp16", "architecture": "Qwen2ForCausalLM", "context_length": 4096}
    # The loading icon's accessible name is "loading", so the strict button
    # name no longer matches while confirmLoading is on.
    submit = visible_action(modal, deploy["action"]["registerAndDeploy"])
    # Weight-source confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    assert_dialog_shell(page, modal, choose, submit)
    report.setdefault("evidence", []).append({
        "layer": "ui", "dialog_shell": deploy["action"]["selectWeightSource"], "viewport": "1440x900"})
    report["last_step"] = "quick-enter-no-submit"
    posted = False
    try:
        with page.expect_request(
            lambda request: urlsplit(request.url).path == path and request.method == "POST",
            timeout=3000,
        ):
            choose.focus()
            page.keyboard.press("Enter")
        posted = True
    except PlaywrightTimeout:
        posted = False
    check(not posted, "Enter on the source field sent a quick registration")
    expect(modal).to_be_visible()
    expect(submit).not_to_have_class(re.compile(r"\bant-btn-loading\b"), timeout=12000)
    modal.locator(".ant-modal-title").click()
    report["quick_enter_submitted"] = False
    hold_quick(page, submit, report, path, expected)
    expect(modal).to_be_visible()
    expect(modal.locator(".ant-select-selection-item")).to_have_text(deploy["action"]["modelScope"] + " · " + data["repo"])
    expect(page.locator(".ant-message")).to_contain_text(deploy["action"]["modelRegisterFailed"].split("{{detail}}")[0].strip())
    expect(page.locator(".ant-message")).to_contain_text("upstream fixture is unreachable")
    check(listed(server, token, data) == [], "failed quick registration stored a model")
    expect(submit).not_to_have_class(re.compile(r"\bant-btn-loading\b"))
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as registered:
        submit.click()
    check(registered.value.status == 201, "template quick registration failed")
    created = registered.value.json()
    ident = created["id"]
    report.setdefault("owned_model_ids", []).append(ident)
    strict(report["requests"], [wire(expected, path), wire(expected, path)], "quick complete body/count differs")
    report["step"] = "quick background metadata reaches independent source"
    saved = poll(page, lambda: (lambda doc: doc if doc.get("readme") == README else None)(read(server, token, ident)), "quick background metadata")
    data["name"] = data["repo"]
    facts(saved, data, "modelscope", "master")
    applied_model(page, report, ident, data["repo"], "quick initial application", first_evaluate_offset)
    check(len(listed(server, token, data)) == 1, "quick registration duplicated model")
    audit = data["hub"].audit()
    check(any(r["target"] == "/ms/models/" + data["repo"] + "/resolve/master/config.json" and r["sha256"] == hashlib.sha256(CONFIG.encode()).hexdigest() for r in audit), "quick background metadata did not read authoritative config")
    check(all(r["origin"] == "" and r["status"] == 200 for r in audit), "quick source read failed or incorrectly used browser metadata")
    page.goto(server.base + "/inference-templates/" + data["template_model_id"])
    visible_action(page, templates["detail"]["deploy"]).click()
    modal = page.get_by_role("dialog", name=deploy["weightSourceModal"]["confirmTitle"], exact=True)
    matching_radio = modal.locator("label.ant-radio-wrapper").filter(has_text=data["repo"]).get_by_role("radio")
    expect(matching_radio).to_have_count(1)
    expect(matching_radio).to_be_checked()
    reuse_evaluate_offset = len(report["evaluations"])
    button(modal, deploy["weightSourceModal"]["useModel"]).click()
    expect(modal).not_to_be_visible()
    strict(report["requests"], [wire(expected, path), wire(expected, path)], "reusing registered template model emitted extra mutation")
    applied_model(page, report, ident, data["repo"], "existing model confirmation", reuse_evaluate_offset)
    strict(read(server, token, ident), saved, "template reuse overwrote saved metadata")
    report["observations"] = {"saved": saved, "selected_template_id": data["template"]["id"], "reuse": "same model selected with zero new POST"}
    saved_ui(page, server, labels, data, ident, "modelscope", "master", token)
    report["last_step"] = "quick-readme-persisted"
    page.reload()
    row = page.get_by_role("row").filter(has_text=data["repo"])
    expect(row).to_have_count(1)
    page.goto(server.base + "/models?detail=" + str(ident))
    reloaded = model_detail_dialog(page, labels, data["repo"])
    expect(reloaded).to_be_visible(timeout=12000)
    expect(reloaded.get_by_title(labels["column"]["sourceID"], exact=True)).to_have_text(data["repo"])
    expect(reloaded.get_by_text("master", exact=True)).to_be_visible()
    expect(reloaded.get_by_text("Qwen2ForCausalLM", exact=True)).to_be_visible()
    expect(reloaded.get_by_text("float16", exact=True)).to_be_visible()
    expect(reloaded.get_by_text("4,096", exact=True)).to_be_visible()
    readme = reloaded.locator(".ant-card", has_text="README")
    expect(readme).to_be_visible(timeout=12000)
    expect(readme).to_contain_text("Audited browser registration fixture.")
    report["readme_persisted"] = True
    facts(read(server, token, ident), data, "modelscope", "master")
    visible_action(reloaded, labels["action"]["close"]).click()
    expect(reloaded).not_to_be_visible()
    delete_ui(page, server, labels, data, ident, token, report, locale)
    strict(report["requests"], [wire(expected, path), wire(expected, path),
        *([model_delete_wire(ident)] * 3)], "quick final mutation sequence differs")
    finish(report, server, data, {path: [502], MODELS + "/" + str(ident): [409]})
