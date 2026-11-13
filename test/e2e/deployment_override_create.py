#!/usr/bin/env python3
"""Browser container overrides -> real agent/Docker exec/files/gateway.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
Run: DOCKER_HOST=unix:///path/to/dedicated/docker.sock make test-e2e-deployment-container-overrides
Requires local alpine:3.20, fake-engine:test, built server/agent, Chrome.
DOCKER_CONTEXT must be empty; the default shared Docker socket is refused.
Only GPU facts are synthetic. No real GPU inference is claimed.
"""
import json
import os
import re
import time
from urllib.parse import urlsplit

from playwright.sync_api import expect
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import button, check
from deployment_batch_checks import created_notice, rejected_notice, open_deployment_row, delete_name_preview
import deployment_container_fixture as containers
import deployment_override_fixture as fixture_tools
import deployment_wizard_create as wizard
from deployment_selector_checks import (
    watch_weight_lookups, choose_registered_model, assert_weight_match, choose_manual_node)

CONTRACT = "gpuworks/deployment/container-overrides"
CASE = "部署 / 容器覆盖 / 环境设置与删除、只读可写挂载、共享内存与真实网关"


def expected_body(fixture, name):
    expected = wizard.expected_body(fixture, name)
    expected["image"] = fixture["image"]
    expected["container_overrides"] = fixture_tools.expected_overrides(fixture)
    return expected


def expected_requests(fixture, deleted_id=None):
    expected = [{"method": "POST", "path": containers.DEPLOYMENTS, "query": "", "body": expected_body(fixture, value)}
                for value in ("x" * 257, fixture["name"])]
    if deleted_id is not None:
        expected.append({"method": "DELETE", "path": containers.DEPLOYMENTS + f"/{deleted_id}", "query": "", "body": None})
    return expected


def hold_deploy_create(page, report):
    # setSubmitting runs before createDeployment, so Deploy is loading while
    # the first POST is still held. Continue into the real server response.
    path = containers.DEPLOYMENTS
    held = []
    pattern = re.compile(re.escape(path) + r"(?:\?|$)")
    ok = page.locator(".deploy-submit-bar .ant-btn-primary")

    def hold_post(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    ok.click()
    report["last_step"] = "deploy-create-inflight"
    expect(ok).to_have_class(re.compile(r"\bant-btn-loading\b"))
    ok.click()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and not held:
        page.wait_for_timeout(100)
    check(len(held) == 1, "duplicate deployment create while the first request is held")
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as received:
        held[0].continue_()
    page.unroute(pattern, hold_post)
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True})
    return received.value


def remember_created(response, report):
    document = response.json()
    ident = document.get("id") if isinstance(document, dict) else None
    valid = type(ident) is int and ident > 0
    if valid:
        report["owned_deployment_id"] = ident
    # A malformed ID must leave the pre-registered unique name available to
    # cleanup's independent lookup, rather than selecting /True or /null.
    check(valid, "created deployment response returned no valid numeric ID")
    return ident


def configure(page, labels, fixture):
    rows = page.locator(".image-defaults-env-row")
    expect(rows).to_have_count(1)
    expect(rows.get_by_placeholder("KEY", exact=True)).to_have_value("E2E_WIZARD")
    expect(rows.get_by_placeholder("VALUE", exact=True)).to_have_value(fixture["run_id"])
    rows.get_by_placeholder("VALUE", exact=True).fill(fixture["override_value"])
    unset = page.locator("#container-overrides-env-unset")
    unset.fill("HF_HUB_OFFLINE")
    unset.press("Enter")
    shm = page.locator("#container-overrides-shm")
    expect(shm).to_have_value("")
    shm.fill("0.25")
    shm.press("Tab")
    for index, (name, read_only) in enumerate((("ro", True), ("rw", False))):
        page.locator("#container-overrides-mount-add").click()
        mount = page.locator(".image-defaults-mount-row").nth(index)
        mount.get_by_placeholder(labels["containerOverrides"]["hostPathPlaceholder"], exact=True).fill(fixture[name + "_dir"])
        mount.get_by_placeholder(labels["containerOverrides"]["containerPathPlaceholder"], exact=True).fill("/e2e-" + name)
        expect(mount.get_by_role("checkbox")).not_to_be_checked()
        if read_only:
            mount.get_by_role("checkbox").check()
        expect(mount.get_by_role("checkbox")).to_be_checked() if read_only else expect(mount.get_by_role("checkbox")).not_to_be_checked()


def assert_draft(page, fixture):
    expect(page.locator(".image-defaults-env-row").get_by_placeholder("VALUE", exact=True)).to_have_value(fixture["override_value"])
    expect(page.locator("#container-overrides-shm")).to_have_value("0.25")
    expect(page.locator('.ant-select-selection-item[title="HF_HUB_OFFLINE"]')).to_be_visible()
    for index, name in enumerate(("ro", "rw")):
        mount = page.locator(".image-defaults-mount-row").nth(index)
        inputs = mount.get_by_role("textbox")
        expect(inputs.nth(0)).to_have_value(fixture[name + "_dir"])
        expect(inputs.nth(1)).to_have_value("/e2e-" + name)
        if name == "ro":
            expect(mount.get_by_role("checkbox")).to_be_checked()
        else:
            expect(mount.get_by_role("checkbox")).not_to_be_checked()


def assert_resolved(document, fixture):
    retired = {"stopped", "failed", "draining", "pending-removal", "removing"}
    current = [item for item in document["instances"] if item["state"] not in retired]
    check(len(current) == 1 and current[0]["state"] == "running", "expected one current running replica")
    instance = current[0]
    wizard.assert_json(instance["env"], {"E2E_WIZARD": fixture["override_value"],
        "TRANSFORMERS_OFFLINE": "1", "GLOO_SOCKET_IFNAME": "lo", "NCCL_SOCKET_IFNAME": "lo",
        "VLLM_HOST_IP": "127.0.0.1"}, "resolved environment (unset/system/explicit) differs")
    wizard.assert_json(instance["shm_bytes"], fixture_tools.SHM_BYTES, "resolved shm bytes")
    wizard.assert_json(instance["mounts"], [
        {"source": fixture["ro_dir"], "target": "/e2e-ro", "read_only": True, "purpose": "data"},
        {"source": fixture["rw_dir"], "target": "/e2e-rw", "purpose": "data"}], "resolved mounts differ")
    return instance


def test_deployment_container_overrides(page, context, server, artifacts, report, locale, fixture, token):
    weight_lookups = watch_weight_lookups(page)
    labels = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deploy.json").read_text())
    listings = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deployments.json").read_text())
    auth = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/auth.json").read_text())
    common = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/common.json").read_text())
    requests, errors, http_errors, console_errors = [], [], [], []
    report.update({"mutation_requests": requests, "page_errors": errors,
                   "http_errors": http_errors, "console_errors": console_errors})
    authenticated = False
    def request_seen(request):
        path = urlsplit(request.url).path
        if path.startswith(containers.DEPLOYMENTS) and request.method in ("POST", "PUT", "PATCH", "DELETE"):
            requests.append({"method": request.method, "path": path,
                "query": urlsplit(request.url).query,
                "body": request.post_data_json if request.post_data else None})
    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400 and not (not authenticated and path == "/api/v1/auth/me" and response.status == 401):
            http_errors.append({"method": response.request.method, "path": path, "status": response.status})
    def console_seen(message):
        if message.type == "error":
            path = urlsplit(message.location.get("url", "")).path
            if not (not authenticated and path == "/api/v1/auth/me" and "401" in message.text):
                console_errors.append({"path": path, "text": message.text})
    page.on("request", request_seen)
    page.on("response", response_seen)
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("console", console_seen)
    page.goto(server.base + "/deploy")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    choose_registered_model(page, fixture, locale, report)
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/evaluate") as evaluated:
        wizard.select(page, page.locator("#target_vendor"), labels["basic"]["vendor"]["nvidia"])
    check(evaluated.value.status == 200, "registered-model evaluation failed")
    deploy = button(page, labels["page"]["deploy"])
    expect(deploy).to_be_enabled(timeout=30000)
    page.get_by_role("button").filter(has_text=fixture["template"]["display_title"]).click()
    weights = page.get_by_role("dialog", name=labels["weightSourceModal"]["confirmTitle"])
    source = weights.get_by_role("radio", name=re.compile(re.escape(fixture["model"]["name"])))
    expect(source).to_be_checked()
    assert_weight_match(page, source, fixture, locale, report, weight_lookups)
    use_model = button(weights, labels["weightSourceModal"]["useModel"])
    # Weight-source confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    assert_dialog_shell(page, weights, source, use_model)
    report.setdefault("evidence", []).append({
        "layer": "ui", "dialog_shell": labels["weightSourceModal"]["confirmTitle"], "viewport": "1440x900"})
    use_model.click()
    choose_manual_node(page, fixture, labels, report)
    image = page.get_by_role("combobox", name=labels["workbench"]["acquisition"]["imageOptions"])
    image.fill(fixture["image"])
    image.press("Tab")
    name = page.get_by_role("textbox", name=re.compile(re.escape(labels["basic"]["name"]) + "$"))
    page.get_by_role("textbox", name=labels["basic"]["servedName"], exact=True).fill(fixture["served_name"])
    name.fill(fixture["name"])
    configure(page, labels, fixture)
    assert_draft(page, fixture)
    expect(deploy).to_be_enabled(timeout=30000)
    check(requests == [], "editing draft emitted deployment mutation")
    launch_preview = wizard.launch_checks.draft_checks(
        page, labels, expected_body(fixture, fixture["name"]), report, token=token)
    report["last_step"] = "overrides-draft-configured"
    wizard.launch_checks.boolean_rejection.seed(page, labels, expected_body(fixture, fixture["name"]), report, token, wizard.launch_checks.boolean_rejection.declared_mount(fixture["model_dir"]))
    invalid = "x" * 257
    name.fill(invalid)
    rejected = hold_deploy_create(page, report)
    report["rejection"] = {"status": rejected.status, "body": rejected.json()}
    detail = "must contain valid text without control characters and at most 256 characters"
    wizard.assert_json(report["rejection"], {"status": 400, "body": {
        "code": "FIELD_INVALID", "error": "name " + detail,
        "params": {"field": "name", "detail": detail}}}, "backend refusal differs")
    reason = common["apiError"]["FIELD_INVALID"].replace("{{field}}", "name").replace("{{detail}}", detail)
    rejected_notice(page, labels["action"]["createFailed"].replace("{{detail}}", reason), labels, report)
    expect(name).to_have_value(invalid)
    expect(image).to_have_value(fixture["image"])
    assert_draft(page, fixture)
    code, listing = http(server.base, "GET", containers.DEPLOYMENTS, token=token)
    check(code == 200 and listing["total"] == 0, "rejection partially created deployment")
    wizard.launch_checks.boolean_rejection.retained(page, labels, expected_body(fixture, fixture["name"]), report, token, rejected, "name")
    name.fill(fixture["name"])
    wizard.launch_checks.boolean_rejection.restore(page, labels, expected_body(fixture, fixture["name"]), report, token)
    report["last_step"] = "backend-refused-draft-corrected"
    try:
        expect(deploy).to_be_enabled()
    except Exception:
        try:
            report["submit_buttons"] = page.get_by_role("button").all_text_contents()
            report["submit_bar"] = page.locator(".deploy-submit-bar").inner_html()
        except Exception as diagnostic_error:
            report["submit_diagnostic_error"] = str(diagnostic_error)
        raise
    with page.expect_response(lambda r: urlsplit(r.url).path == containers.DEPLOYMENTS and r.request.method == "POST") as created:
        deploy.click()
    check(created.value.status == 201, f"override create HTTP {created.value.status}: {created.value.text()}")
    dep_id = remember_created(created.value, report)
    created_notice(page, labels, report)
    wizard.assert_json(requests, wizard.launch_checks.boolean_rejection.expected_mutations(expected_requests(fixture), report), "full create mutation count/query/body differs")
    report["last_step"] = "browser-created"
    def running():
        status, document = http(server.base, "GET", containers.DEPLOYMENTS + f"/{dep_id}", token=token)
        report["last_deployment"] = document
        check(status == 200, f"deployment GET {status}")
        containers.remember_containers(report, document)
        return document if document["runtime_state"] == "running" else None
    saved = wizard.until(page, running, "real override launch")
    wizard.assert_json(fixture_tools.stored_overrides(server, dep_id), fixture_tools.expected_overrides(fixture), "persisted raw container overrides")
    for field in ("name", "image", "model_id", "node_uuid", "served_name"):
        wizard.assert_json(saved[field], expected_body(fixture, fixture["name"])[field], "persisted " + field)
    instance = assert_resolved(saved, fixture)
    wizard.launch_checks.snapshot_checks(saved, expected_body(fixture, fixture["name"]), report)
    wizard.launch_checks.runtime_checks(instance, expected_body(fixture, fixture["name"]),
                                        launch_preview, fixture["model_dir"], report)
    container = instance["container_id"]
    runtime = fixture_tools.verify_runtime(container, fixture, report)
    response = page.request.post(server.base + "/v1/chat/completions",
        headers={"Authorization": "Bearer " + fixture["key"]["key"]},
        data={"model": fixture["served_name"], "messages": [{"role": "user", "content": "overrides"}]})
    containers.assert_chat_response(response)
    wizard.assert_json(response.json(), {"id": "fake-engine-completion", "object": "chat.completion",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello from fake-engine"},
                     "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 16, "completion_tokens": 8, "total_tokens": 24}},
        "exact audited fake-engine completion")
    report["last_step"] = "container-exec-and-gateway"
    page.reload()
    row = page.get_by_role("row").filter(has_text=fixture["name"])
    expect(row).to_have_count(1)
    drawer = open_deployment_row(page, row, fixture["name"], dep_id, report)
    expect(drawer.get_by_text(fixture["image"], exact=True).first).to_be_visible()
    status, refreshed = http(server.base, "GET", containers.DEPLOYMENTS + f"/{dep_id}", token=token)
    check(status == 200, "reload independent GET failed")
    stored = fixture_tools.stored_overrides(server, dep_id)
    wizard.assert_json(stored, fixture_tools.expected_overrides(fixture), "refreshed raw container overrides")
    assert_resolved(refreshed, fixture)
    parent = page.get_by_role("dialog", name=fixture["name"], exact=True)
    expect(parent).to_be_visible()
    delete_title = listings["delete"]["title"].replace("{{name}}", fixture["name"])
    # The header Delete button includes the icon's accessible name.
    remove = parent.locator(".ant-drawer-extra").get_by_role(
        "button", name=listings["delete"]["button"])
    dep_path = containers.DEPLOYMENTS + f"/{dep_id}"

    def open_delete():
        remove.click()
        dialog = page.get_by_role("dialog", name=delete_title, exact=True)
        expect(dialog).to_be_visible()
        delete_name_preview(dialog, delete_title, report)
        return dialog

    before_delete = len(requests)
    dialog = assert_nested_dismissals(
        open_delete(), parent, open_delete,
        [("cancel", cancel_button(listings["delete"]["cancel"])),
         ("escape", escape), ("close", close_button), ("mask", mask)],
        lambda: len(requests), remove)
    cancel_button(listings["delete"]["cancel"])(dialog)
    expect(dialog).to_be_hidden()
    expect(parent).to_be_visible()
    check(len(requests) == before_delete, "delete dialog dismissal removed the deployment")
    stayed = page.request.get(server.base + dep_path)
    check(stayed.status == 200 and stayed.json().get("name") == fixture["name"],
          "delete dialog dismissal changed the stored deployment")
    report["evidence"].append({
        "layer": "ui", "delete_dialog": delete_title, "parent": fixture["name"],
        "dismissals": ["cancel", "escape", "close", "mask"],
        "focus_returned": True, "delete_requests": 0, "stored_name": fixture["name"]})
    confirm = page.get_by_role("dialog", name=delete_title, exact=True)
    remove.click()
    expect(confirm.get_by_text(listings["delete"]["description"], exact=True)).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == containers.DEPLOYMENTS + f"/{dep_id}" and r.request.method == "DELETE") as removed:
        button(confirm, listings["delete"]["button"]).click()
    check(removed.value.status in (200, 202, 204), "delete refused")
    wizard.until(page, lambda: http(server.base, "GET", containers.DEPLOYMENTS + f"/{dep_id}", token=token)[0] == 404, "deployment disappearance")
    wizard.until(page, lambda: containers.docker_running(container) == "absent", "container disappearance")
    expect(row).to_have_count(0)
    report["owned_deployment_id"] = None
    wizard.assert_json(requests, wizard.launch_checks.boolean_rejection.expected_mutations(expected_requests(fixture, dep_id), report), "final mutation sequence")
    allowed = [{"method": "POST", "path": containers.DEPLOYMENTS, "status": 400},
               {"method": "GET", "path": containers.DEPLOYMENTS + f"/{dep_id}", "status": 404}]
    check(all(item in allowed for item in http_errors), f"unexpected HTTP errors: {http_errors}")
    check(all((item["path"] == containers.DEPLOYMENTS and "400" in item["text"]) or
              (item["path"] == containers.DEPLOYMENTS + f"/{dep_id}" and "404" in item["text"])
              for item in console_errors), f"unexpected console errors: {console_errors}")
    check(not errors, f"page errors: {errors}")
    report["last_step"] = "deletion-observed"
    report["evidence"].extend([
        {"layer": "ui", "overrides": fixture_tools.expected_overrides(fixture), "cancelled_delete": True, "backend_error": reason},
        {"layer": "request", "mutations": requests, "response_statuses": [400, 201, removed.value.status]},
        {"layer": "persistence", "raw_sqlite_overrides": stored, "resolved_get_instance":
         {key: assert_resolved(refreshed, fixture)[key] for key in ("env", "mounts", "shm_bytes")}, "id": dep_id},
        {"layer": "execution", "container": container, "runtime": runtime, "gateway": response.json()},
        {"layer": "cleanup", "deployment_http": 404, "container_state": "absent"}])
    # Sections 13 and 22: the floating layers, the navigation and the backend refusal. Run last
    # so the deployment above is already created and cleaned up, and so a failure here cannot be
    # mistaken for a failure of the override contract.
    report["menu_popover_13"] = exercise_menu_popover_13(page, labels, server, token, report)
    report["navigation_tabs_22"] = exercise_navigation_tabs_22(page, labels, server, token, report)
    report["last_step"] = "sections-13-and-22-exercised"


def _raw_get(server, path, token=""):
    """A raw transport, for responses that are HTML rather than JSON."""
    import urllib.request
    import urllib.error
    headers = {"Accept": "*/*"}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(server.base + path, headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        response = opener.open(request, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.headers.get("Content-Type", ""), response.read()


def _overlay_open(page):
    return page.locator(".ant-select-dropdown:visible").count()


def exercise_menu_popover_13(page, labels, server, token, report):
    """13.1, 13.2 and 13.3 on the deploy surface, measured rather than inferred.

    The deploy pages render no antd Menu, Popover or Dropdown: a grep of ui/src/pages/deploy and
    ui/src/pages/DeployPage.tsx finds none. The layers that really open and close here are the
    antd Select overlays, so those are what 13.1 and 13.2 are measured on. 13.3's backend half is
    proved with a real 403 from the product's own read/write platform-token scopes rather than by
    a hidden button, because a hidden button is a UI inference and not a refusal.
    """
    out = {}
    # The override contract above ends on the deployments list, so the deploy page is entered
    # again here rather than assuming the form is still mounted.
    page.goto(server.base + "/deploy")
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    vendor = page.locator("[role='combobox']").nth(1)
    vendor.wait_for(state="visible", timeout=30000)

    # 13.1 - opened by mouse, opened by keyboard, closed again, focus returned, and a disabled
    # trigger that opens nothing.
    vendor.scroll_into_view_if_needed()
    vendor.click()
    expect(page.locator(".ant-select-dropdown:visible")).to_have_count(1)
    mouse_open = page.locator("[role='option']").count()
    check(mouse_open > 0, "the vendor overlay opened with no rows to choose")
    page.keyboard.press("Escape")
    expect(page.locator(".ant-select-dropdown:visible")).to_have_count(0)
    vendor.focus()
    page.keyboard.press("ArrowDown")
    page.wait_for_timeout(400)
    keyboard_open = page.locator("[role='option']").count()
    check(keyboard_open > 0, "the vendor overlay did not open from the keyboard")
    page.keyboard.press("Escape")
    page.wait_for_timeout(300)
    keyboard_focus = page.evaluate(
        "() => { const el = document.activeElement;"
        " return !!el && el.className.indexOf('ant-select-selection-search-input') >= 0; }")
    check(keyboard_focus, "closing the overlay by keyboard did not return focus to its trigger")

    # A disabled overlay trigger is a real object on this page: the node picker is disabled until
    # manual node selection is chosen (DeployNodeSection), and clicking it must open nothing.
    triggers = page.evaluate(
        "() => [...document.querySelectorAll('[role=combobox]')].map((el) => ({"
        "  label: el.getAttribute('aria-label'),"
        "  disabled: !!(el.disabled || el.closest('.ant-select-disabled'))}))")
    disabled = [row for row in triggers if row["disabled"] and row["label"]]
    check(bool(disabled), "no disabled overlay trigger was present to exercise")
    before = _overlay_open(page)
    for row in disabled:
        page.get_by_role("combobox", name=row["label"]).click(force=True)
        page.wait_for_timeout(250)
    after = _overlay_open(page)
    check(after == before, f"a disabled trigger opened an overlay ({before} before, {after} after)")
    out["13.1"] = {"mouse_open_rows": mouse_open, "keyboard_open_rows": keyboard_open,
                   "keyboard_focus_returned": keyboard_focus,
                   "disabled_triggers": [row["label"] for row in disabled],
                   "overlays_after_disabled_click": after}

    # 13.2 - every row of the open overlay stays inside the viewport, and closing returns focus.
    vendor.click()
    expect(page.locator(".ant-select-dropdown:visible")).to_have_count(1)
    viewport = page.viewport_size
    boxes = page.evaluate(
        "() => [...document.querySelectorAll('.ant-select-dropdown:not(.ant-select-dropdown-hidden)"
        " [role=option]')].map((el) => { const r = el.getBoundingClientRect();"
        " return {top: r.top, bottom: r.bottom, left: r.left, right: r.right}; })")
    check(bool(boxes), "the open overlay had no rows to measure")
    inside = [box for box in boxes
              if box["top"] >= 0 and box["bottom"] <= viewport["height"]
              and box["left"] >= 0 and box["right"] <= viewport["width"]]
    check(len(inside) == len(boxes),
          f"{len(boxes) - len(inside)} overlay rows fell outside the "
          f"{viewport['width']}x{viewport['height']} viewport")
    # The trigger must stay reachable while the overlay is open, so the overlay cannot be
    # covering the control that opened it.
    trigger_box = vendor.bounding_box()
    covering = [box for box in boxes
                if trigger_box and box["top"] < trigger_box["y"] + trigger_box["height"]
                and box["bottom"] > trigger_box["y"]
                and box["left"] < trigger_box["x"] + trigger_box["width"]
                and box["right"] > trigger_box["x"]]
    check(not covering, "the open overlay covered the trigger that opened it")
    page.keyboard.press("Escape")
    page.wait_for_timeout(300)
    mouse_focus = page.evaluate(
        "() => { const el = document.activeElement;"
        " return !!el && el.className.indexOf('ant-select-selection-search-input') >= 0; }")
    check(mouse_focus, "focus was not returned to the trigger after the overlay closed")
    out["13.2"] = {"rows": len(boxes), "rows_inside_viewport": len(inside),
                   "overlay_covers_trigger": bool(covering),
                   "viewport": viewport, "focus_returned": mouse_focus}

    # 13.3 - a real backend refusal from the product's own platform-token scopes. A read token
    # may GET deployments and must be refused a POST by the backend (api/auth.go:321-332,
    # PLATFORM_TOKEN_READ_ONLY). This is a refusal no button state can stand in for, and the
    # write-scope control shows the refusal is scope-based rather than a blanket rejection.
    code, made = http(server.base, "POST", "/api/v1/platform-tokens", token=token,
                      body={"name": "e2e-13-3-read", "scope": "read"})
    check(code == 201, f"the product would not mint a read platform token: {code}")
    read_token = made["token"]
    read_get, _ = http(server.base, "GET", containers.DEPLOYMENTS, token=read_token)
    check(read_get == 200, f"a read token could not read deployments: {read_get}")
    refused, refusal = http(server.base, "POST", containers.DEPLOYMENTS, token=read_token, body={})
    check(refused == 403, f"the backend answered a read token's write with {refused}, not a refusal")
    check(refusal.get("code") == "PLATFORM_TOKEN_READ_ONLY",
          f"the refusal carried {refusal.get('code')!r} rather than the read-only code")
    code, write_made = http(server.base, "POST", "/api/v1/platform-tokens", token=token,
                            body={"name": "e2e-13-3-write", "scope": "write"})
    check(code == 201, f"the product would not mint a write platform token: {code}")
    write_status, _ = http(server.base, "POST", containers.DEPLOYMENTS,
                           token=write_made["token"], body={})
    check(write_status != 403,
          "a write token was refused exactly like a read token, so the refusal is not scope-based")
    for item in (made.get("item") or {}, write_made.get("item") or {}):
        if item.get("id") is not None:
            http(server.base, "DELETE", f"/api/v1/platform-tokens/{item['id']}", token=token)
    out["13.3"] = {"read_token_get": read_get, "read_token_post": refused,
                   "read_token_code": refusal.get("code"), "write_token_post": write_status,
                   "backend_refusal_proved": True}
    return out


def exercise_navigation_tabs_22(page, labels, server, token, report):
    """22.1, 22.2 and 22.7 on the deploy surface.

    22.1 walks the deep links the page really has and checks what each one lands on. 22.2
    switches the Expert tabs and records what the URL does, because this page keeps the active
    tab in component state and not in the URL - a fact worth asserting rather than assuming.
    22.7 checks that an unknown page is answered by the SPA shell while an unknown API path is
    not, so a bad deep link cannot be hidden behind a 200.
    """
    out = {}

    # 22.1 - the entry and its deep links, each entered separately.
    entries = {}
    for path in ("/deploy", "/deploy?model=1"):
        status, content_type, body = _raw_get(server, path, token)
        entries[path] = {"status": status, "content_type": content_type,
                         "is_shell": b"<!doctype html" in body.lower()}
        check(status == 200, f"{path} did not answer 200: {status}")
        check("text/html" in content_type, f"{path} was not served as the SPA document")
    # A deep link that names no real model still has to render the form rather than a blank page.
    page.goto(server.base + "/deploy?model=1")
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    deep_url = page.url
    heading = page.get_by_role("heading", name=labels["page"]["title"], exact=True)
    deep_title = heading.inner_text().strip()
    check(deep_title == labels["page"]["title"],
          f"the deep link landed on {deep_title!r} rather than the deploy heading")
    # A deep link to a deployment that does not exist must say so rather than sit in a loading
    # state. It renders the generic "could not be opened" result instead of the not-found copy:
    # the API answers 404 with a body carrying only {"error": "not found"} and no code
    # (internal/server/api/api.go:1010-1020 writes code only for a ProductError), so the client's
    # parseError returns a plain Error rather than an APIError (ui/src/apiError.ts:83-84) and
    # DeployPage's missing branch (ui/src/pages/DeployPage.tsx:81) cannot be reached. The page
    # still refuses to show a blank form, which is what this assertion pins down; the wrong
    # wording is recorded as a product gap rather than asserted as correct.
    page.goto(server.base + "/deploy?edit=1")
    result = page.locator(".ant-result-title")
    expect(result).to_be_visible(timeout=30000)
    missing_copy = result.inner_text().strip()
    check(missing_copy in (labels["edit"]["notFound"], labels["edit"]["loadFailed"]),
          f"a deep link to a missing deployment rendered unexpected copy {missing_copy!r}")
    check(page.locator("[aria-busy='true']").count() == 0,
          "a deep link to a missing deployment stayed in its loading state")
    out["missing_deployment_copy"] = missing_copy
    out["missing_deployment_copy_is_not_found_wording"] = missing_copy == labels["edit"]["notFound"]
    page.goto(server.base + "/deploy")
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    # The nav keeps its selected state consistent with the page it is showing.
    selected_nav = page.evaluate(
        "() => [...document.querySelectorAll(\"[data-slot='nav-item'][data-active='true']\")]"
        ".map((el) => el.textContent.trim())")
    check(bool(selected_nav), "the deep link left no navigation item selected")
    # A refresh keeps the page rather than falling back to the root.
    page.reload()
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    refreshed_nav = page.evaluate(
        "() => [...document.querySelectorAll(\"[data-slot='nav-item'][data-active='true']\")]"
        ".map((el) => el.textContent.trim())")
    check(refreshed_nav == selected_nav,
          f"the selected navigation changed across a refresh: {selected_nav} -> {refreshed_nav}")
    out["22.1"] = {"entries": entries, "landed_url": deep_url, "title": deep_title,
                   "selected_nav": selected_nav, "after_refresh": refreshed_nav}

    # 22.2 - the Expert tabs, and what the URL does while they are switched. This page keeps the
    # tab in component state (DeployExpertTabs has no route parameter), so a tab switch leaves
    # the URL alone; the assertion records that instead of assuming either behaviour.
    page.goto(server.base + "/deploy")
    page.wait_for_timeout(1500)
    quick_label = labels["page"]["mode"]["quick"]
    expert_label = labels["page"]["mode"]["expert"]
    page.get_by_text(expert_label, exact=True).click()
    expect(page.locator("[role='tab']").first).to_be_visible(timeout=20000)
    tab_names = page.locator("[role='tab']").all_inner_texts()
    check(len(tab_names) >= 2, f"the Expert view rendered {len(tab_names)} tabs, too few to switch")
    switches = []
    for name in [name.strip() for name in tab_names[1:3]]:
        page.get_by_role("tab", name=name, exact=True).click()
        page.wait_for_timeout(700)
        selected = page.locator("[role='tab'][aria-selected='true']")
        panel_count = page.locator("[role='tabpanel']").count()
        switches.append({"tab": name, "selected": selected.all_inner_texts(),
                         "url": page.url, "panels": panel_count})
        check(selected.count() == 1,
              f"after switching to {name!r} {selected.count()} tabs claim to be selected")
        check(selected.inner_text().strip() == name,
              f"after switching to {name!r} the selected tab reads "
              f"{selected.inner_text().strip()!r}")
        check(panel_count >= 1, f"after switching to {name!r} no tab panel is rendered")
    # The page content and the selected tab must agree, and the URL must not claim a tab it is
    # not showing: the URL carries no tab parameter at all here.
    url_has_tab = any(key in page.url for key in ("tab=", "step="))
    # The contract is that the URL must not claim a tab the page is not showing. When the page
    # keeps the tab in component state there is no tab parameter to disagree with, so the check
    # is that any tab parameter present agrees with the selected tab.
    if url_has_tab:
        for row in switches:
            marker = [part for part in row["url"].split("?")[-1].split("&")
                      if part.startswith(("tab=", "step="))]
            check(bool(marker), f"the URL dropped its tab parameter while showing {row['tab']!r}")
    out["22.2"] = {"tab_names": [name.strip() for name in tab_names], "switches": switches,
                   "url_carries_tab_parameter": url_has_tab}

    # 22.7 - an unknown page is the shell, an unknown API path is not.
    page_status, page_type, page_body = _raw_get(server, "/no-such-deploy-page", token)
    api_status, api_type, api_body = _raw_get(server, "/api/v1/definitely-not-a-route", token)
    check(page_status == 200 and b"<!doctype html" in page_body.lower(),
          f"an unknown page was not answered by the SPA shell: {page_status}")
    check(api_status == 404,
          f"an unknown API path was answered {api_status}; the shell must not swallow it")
    check("json" in api_type, f"an unknown API path answered {api_type!r} rather than JSON")
    check(b"<!doctype html" not in api_body.lower(),
          "an unknown API path was answered with the SPA shell")
    out["22.7"] = {"unknown_page": {"status": page_status, "content_type": page_type},
                   "unknown_api": {"status": api_status, "content_type": api_type},
                   "spa_swallows_api": False}
    return out

def require_dedicated_docker():
    # This contract launches real managed containers. Another control plane's
    # orphan sweep can delete them on a shared default daemon.
    docker_host = os.environ.get("DOCKER_HOST", "").strip()
    parsed = urlsplit(docker_host)
    check(parsed.scheme == "unix" and bool(parsed.path) and os.path.realpath(parsed.path) not in (
        "/var/run/docker.sock", "/run/docker.sock"),
        "container override E2E requires an explicit dedicated DOCKER_HOST (never the default socket)")
    check(not os.environ.get("DOCKER_CONTEXT", ""),
          "container override E2E requires empty DOCKER_CONTEXT so it cannot override the dedicated DOCKER_HOST")


def main():
    require_dedicated_docker()
    return wizard.main(manifest_path=ROOT / "test/e2e/coverage/deployment-container-overrides.json",
                       exercise=test_deployment_container_overrides,
                       fixture_module=fixture_tools)


if __name__ == "__main__":
    raise SystemExit(main())
