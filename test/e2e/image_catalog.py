#!/usr/bin/env python3
"""Image registration and sibling deletion through the real browser and server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Build and run: make test-e2e-image-register / test-e2e-image-version-delete
After building: python3 test/e2e/image_catalog.py --case register --locale en
Choices: --case register or delete-version; --locale en or zh.
Optional CLI: --server-bin <binary>, --artifact-dir <private-directory>.
When a proxy is configured, set NO_PROXY and no_proxy to 127.0.0.1,localhost.

Each case starts its own real server, fresh SQLite and Chrome; no agent, Docker
or control-plane mock. Only catalog metadata is tested, not image availability.
GET /images/{id} addresses a series; DELETE /images/{id} addresses a version.
The parent-series deletion at teardown is cleanup, not business Delete coverage.
Field/CRUD/dimension mappings and gaps: coverage/image-catalog-*.json.

Artifacts default to secrets/e2e/<runId>/ (report, trace, observations, redacted
server log; failure screenshot/ARIA only after login). Success deletes only the
owned series; finally stops the owned server. Failed SQLite is kept for diagnosis.
Remove the private run directory within 7 days; never upload unredacted state.
"""
from __future__ import annotations

import json
import re
from urllib.parse import urlsplit
from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import check, collection
from dialog_checks import assert_drawer_shell, in_viewport, regions_stacked
from image_catalog_runner import run


def button(scope, label):
    # AntD icon names join the accessible name; match the exact visible action.
    text = r"\s*".join(label) if re.fullmatch(r"[\u3400-\u9fff]{2}", label) else re.escape(label)
    return scope.get_by_role("button").filter(has_text=re.compile("^" + text + "$"))


def strings(locale):
    return json.loads((ROOT / f"ui/src/i18n/locales/{locale}/images.json").read_text())


def select(control, label):
    control.click()
    expect(control).to_have_attribute("aria-expanded", "true")
    control.page.get_by_text(label, exact=True).click()
    expect(control).to_have_attribute("aria-expanded", "false")


def open_custom(page, text):
    expect(page.get_by_role("heading", name=text["page"]["title"], exact=True)).to_be_visible()
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()


def read_detail(page, base, series_id):
    response = page.request.get(f"{base}/api/v1/images/{series_id}")
    check(response.status == 200, "image series detail must return 200")
    body = response.json()
    check(body["id"] == series_id and isinstance(body["versions"], list), "invalid series detail envelope")
    return body


def cleanup(page, base, series_id, report):
    response = page.request.delete(f"{base}/api/v1/image-series/{series_id}")
    check(response.status == 204, "owned series cleanup failed")
    check(not any(s["id"] == series_id for s in collection(page.request.get(base + "/api/v1/images"))), "series leaked")
    check(not any(v["series_id"] == series_id for v in collection(page.request.get(base + "/api/v1/images/versions"))), "version leaked")
    report["evidence"].append({"layer": "cleanup", "series_id": series_id, "remaining_owned_series": 0, "remaining_owned_versions": 0})


def exercise_register_full(page, server, text, form, report):
    """Create one custom image with every optional field set, then read it back."""
    report["last_step"] = "register-full-create"
    series_name, source = "register-full", "custom"
    ref = "registry.example/team/register-full:v9.9.9"
    address = "https://containerimages.example/register-full.tar"
    button(page, text["page"]["addCustom"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    dialog.get_by_label(form["seriesName"], exact=True).fill(series_name)
    dialog.get_by_label(form["imageRef"], exact=True).fill(ref)
    options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    select(dialog.get_by_label(form["engine"], exact=True), next(item["label"] for item in options if item["name"] == "vllm"))
    dialog.get_by_label(form["engineVersion"], exact=True).fill("9.9.9")
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    arch = dialog.get_by_label(form["hostArch"], exact=True)
    arch.focus()
    arch.press("ArrowDown")
    expect(arch).to_have_attribute("aria-expanded", "true")
    page.get_by_text("amd64", exact=True).filter(visible=True).click()
    expect(arch).to_have_attribute("aria-expanded", "false")
    rolling = dialog.get_by_role("switch", name=form["rollingTag"], exact=True)
    expect(rolling).not_to_be_checked()
    rolling.click()
    expect(rolling).to_be_checked()
    sources = dialog.get_by_label(form["registrySources"], exact=True)
    sources.click()
    sources.fill(address)
    sources.press("Enter")
    expect(dialog.get_by_text(address, exact=True)).to_be_visible()
    expected = {"series": [{"name": series_name, "engine": "vllm", "framework": "cuda", "source": source, "versions": [
        {"engine_version": "9.9.9", "framework_version": "12.8", "arch": "amd64", "image_ref": ref, "source": source,
         "rolling_tag": True, "registry_sources": [address]}]}]}
    import_path = "/api/v1/images/import"
    with page.expect_response(lambda response: urlsplit(response.url).path == import_path and response.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.status == 200, "full import failed")
    check(imported.value.request.post_data_json == expected, "full import body differs")
    created = collection(imported.value)
    check(len(created) == 1, "full import must create one series")
    series = created[0]
    check(series["name"] == series_name and series["purpose"] == "inference" and series["enabled"] is True
          and series["engine"] == "vllm" and series["framework"] == "cuda" and series["source"] == source
          and series["present_nodes"] == 0, "full response differs")
    detail = read_detail(page, server.base, series["id"])
    saved = detail["versions"][0]
    check(detail["name"] == series_name and detail["purpose"] == "inference" and detail["enabled"] is True
          and detail["present_nodes"] == 0 and saved["image_ref"] == ref and saved["engine_version"] == "9.9.9"
          and saved["framework_version"] == "12.8" and saved["arch"] == "amd64" and saved["rolling_tag"] is True
          and saved.get("registry_sources") == [address], "full detail differs")
    listed = collection(page.request.get(server.base + "/api/v1/images?source=" + source))
    check(any(row["id"] == series["id"] and row["name"] == series_name for row in listed), "full series missing from the list")
    expect(button(page, series_name)).to_be_visible()
    button(page, series_name).click()
    opened = page.get_by_role("dialog", name=series_name, exact=True)
    expect(opened.get_by_role("link", name=ref, exact=True)).to_be_visible()
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    expect(button(page, series_name)).to_be_visible()
    button(page, series_name).click()
    reopened = page.get_by_role("dialog", name=series_name, exact=True)
    expect(reopened.get_by_role("link", name=ref, exact=True)).to_be_visible()
    check(read_detail(page, server.base, series["id"]) == detail, "full reload differs")
    reopened.locator(".ant-drawer-close").click()
    expect(reopened).to_be_hidden()
    report["register_full_create"] = True
    return series["id"]


def exercise_register_minimum(page, server, text, form, report):
    """Create one custom image from the required fields only, then read it back."""
    report["last_step"] = "register-minimum-create"
    ref, derived, source = "registry.example/team/minimum:v1.2.3", "team/minimum", "custom"
    button(page, text["page"]["addCustom"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    dialog.get_by_label(form["imageRef"], exact=True).fill(ref)
    options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    select(dialog.get_by_label(form["engine"], exact=True), next(item["label"] for item in options if item["name"] == "vllm"))
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    expect(dialog.get_by_label(form["seriesName"], exact=True)).to_have_value("")
    expect(dialog.get_by_label(form["engineVersion"], exact=True)).to_have_value("")
    rolling = dialog.get_by_role("switch")
    expect(rolling).to_have_count(1)
    expect(rolling).not_to_be_checked()
    expected = {"series": [{"engine": "vllm", "framework": "cuda", "source": source, "versions": [
        {"engine_version": "", "framework_version": "12.8", "arch": "multi", "image_ref": ref, "source": source,
         "rolling_tag": False, "registry_sources": []}]}]}
    import_path = "/api/v1/images/import"
    with page.expect_response(lambda response: urlsplit(response.url).path == import_path and response.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.status == 200, "minimum import failed")
    check(imported.value.request.post_data_json == expected, "minimum import body differs")
    created = collection(imported.value)
    check(len(created) == 1, "minimum import must create one series")
    series = created[0]
    check(series["name"] == derived and series["purpose"] == "inference" and series["enabled"] is True
          and series["engine"] == "vllm" and series["framework"] == "cuda" and series["source"] == source
          and series["present_nodes"] == 0, "minimum response differs")
    listed = collection(page.request.get(server.base + "/api/v1/images?source=" + source))
    check(any(row["id"] == series["id"] and row["name"] == derived for row in listed), "minimum series missing from the list")
    expect(button(page, derived)).to_be_visible()
    button(page, derived).click()
    details = page.get_by_role("dialog", name=derived, exact=True)
    expect(details.get_by_role("link", name=ref, exact=True)).to_be_visible()
    detail = read_detail(page, server.base, series["id"])
    saved = detail["versions"][0]
    check(detail["name"] == derived and detail["purpose"] == "inference" and detail["enabled"] is True
          and detail["present_nodes"] == 0 and saved["image_ref"] == ref and saved["engine_version"] == "1.2.3"
          and saved["framework_version"] == "12.8" and saved["arch"] == "multi" and saved["rolling_tag"] is False
          and not saved.get("registry_sources"), "minimum detail differs")
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    expect(button(page, derived)).to_be_visible()
    button(page, derived).click()
    expect(page.get_by_role("dialog", name=derived, exact=True).get_by_role("link", name=ref, exact=True)).to_be_visible()
    check(read_detail(page, server.base, series["id"]) == detail, "minimum reload differs")
    report["register_minimum_create"] = True
    return series["id"]


def observe(page):
    errors, failures, mutations = [], [], []
    page.on("pageerror", lambda e: errors.append(str(e)))
    page.on("console", lambda m: errors.append(m.text) if m.type == "error" else None)
    page.on("response", lambda r: failures.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path})
        if r.method in {"POST", "PATCH", "PUT", "DELETE"} and urlsplit(r.url).path.startswith(("/api/v1/images", "/api/v1/image-series")) else None)
    return errors, failures, mutations


# Contract: gpuworks/image-catalog/tagged-registration
# Mainline: 空自定义目录 → UI拒绝空值/无tag/缺框架版本、取消，再登记带tag镜像 → 精确POST/独立GET/刷新证明系列与版本分别持久化；无拉取或运行时声明。
def test_image_registration(page, server, artifacts, report, locale):
    text = strings(locale)
    form = text["import"]
    name = "e2e-" + report["run_id"]
    ref = f"registry.example/{name}/runner:v1"
    errors, failures, mutations = observe(page)
    open_custom(page, text)
    report["last_step"] = "register-header-strong"
    # The header shows the built-in wordmark image and, with no console title
    # configured, no title span at all. There is no default console-title string
    # in either locale and no .app-header-title element in the UI, so the header
    # is asserted through the brand that actually renders.
    logo = page.locator(".app-brand .app-brand-logo")
    expect(logo).to_have_count(1, timeout=12000)
    expect(logo).to_have_attribute("src", "/assets/gpuworks-wordmark.svg")
    expect(page.locator(".app-brand-name")).to_have_count(0)
    report["register_header_brand"] = {
        "logo_src": logo.get_attribute("src"), "title_spans": 0}
    check(collection(page.request.get(server.base + "/api/v1/images?source=custom")) == [], "custom fixture must start empty")
    create = button(page, text["page"]["addCustom"])
    report["active_evidence_layer"] = "ui"
    # 1.2: the engine list is loaded asynchronously, and a submit that races it must not carry an
    # incomplete record. The request is held before the drawer opens, so the drawer is genuinely in
    # its loading state; a submit is then attempted and is required to send nothing at all while the
    # missing engine is reported at its own field. The request is then released and the same submit
    # is required to work, which is what proves the first half was the loading state and not a
    # broken form.
    engines_held = []

    def hold_engines(route):
        engines_held.append(route)
        return

    page.route("**/api/v1/images/engines", hold_engines)
    create.click()
    for _ in range(60):
        if engines_held:
            break
        page.wait_for_timeout(100)
    check(len(engines_held) == 1, "the engine catalogue was not requested when the drawer opened")
    early = page.get_by_role("dialog")
    expect(early).to_be_visible()
    before_early = list(mutations)
    button(early, form["addImage"]).click()
    page.wait_for_timeout(600)
    check(list(mutations) == before_early,
          "a submit during engine loading sent an incomplete image record")
    expect(early.get_by_text(form["engineRequired"], exact=True)).to_be_visible(timeout=8000)
    engines_held[0].continue_()
    engines_held.clear()
    page.unroute("**/api/v1/images/engines", hold_engines)
    report["register_submit_blocked_while_loading"] = True
    # Once the catalogue arrives the field becomes selectable again; the error text stays until the
    # next validation, which is the form library's behaviour and is not asserted here. What matters is
    # that the control is usable, which the rest of this case depends on anyway.
    engine_select = early.get_by_label(form["engine"], exact=True)
    expect(engine_select).to_be_visible(timeout=12000)
    # The blocked submit left its own required-field error behind, which shifts the drawer contents.
    # The drawer is closed and reopened once so the rest of this case starts from a clean form rather
    # than inheriting a layout that belongs to an earlier attempt.
    # The drawer keeps its form state when it is closed, so the error from that attempt would follow
    # the rest of the case. The page is reloaded instead, which is also what a second visit looks
    # like, and the rest of the case then runs against a form that has nothing left over.
    page.reload()
    open_custom(page, text)
    create = button(page, text["page"]["addCustom"])
    create.click()
    early = page.get_by_role("dialog")
    expect(early).to_be_visible()
    expect(early.get_by_text(form["engineRequired"], exact=True)).to_have_count(0, timeout=8000)
    engines = collection(page.request.get(server.base + "/api/v1/images/engines"))
    engine = next(item for item in engines if item["name"] == "vllm")
    dialog = early
    expect(dialog).to_be_in_viewport(ratio=1)
    report["last_step"] = "register-engine-column"
    engine_field = dialog.get_by_label(form["engine"], exact=True)
    column = engine_field.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-col-sm-12 ')][1]")
    expect(column).to_have_count(1, timeout=12000)
    expect(column).to_have_class(re.compile(r"\bant-col-sm-12\b"), timeout=12000)
    report["register_engine_column"] = True
    report["last_step"] = "register-drawer-width"
    wrapper = dialog.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-drawer-content-wrapper ')][1]")
    expect(wrapper).to_have_count(1, timeout=12000)
    expect(wrapper).to_have_css("width", "640px", timeout=12000)
    report["register_drawer_width"] = True
    report["last_step"] = "register-drawer-gap"
    # The drawer's form column stopped being an antd Space: ImageImportDrawer.tsx:91 lays it out
    # as a plain flex column with the same 20px gap, so the gap is read from the element itself.
    space = dialog.locator(".ant-drawer-body > div").first
    expect(space).to_have_count(1, timeout=12000)
    expect(space).to_have_css("row-gap", "20px", timeout=12000)
    expect(space).to_have_css("flex-direction", "column", timeout=12000)
    report["register_drawer_gap"] = True
    # 12.2: at 1440x900 the drawer's own title, its content and the control that commits it are
    # required to sit inside the drawer and inside the viewport without overlapping each other. The
    # geometry is measured rather than eyeballed, so a column that wraps into the submit control
    # fails here rather than being caught by whichever later step happens to click it.
    drawer_box = dialog.evaluate("""element => {
        const rect = (node) => {
            if (!node) return null;
            const box = node.getBoundingClientRect();
            return {top: box.top, left: box.left, right: box.right, bottom: box.bottom,
                    width: box.width, height: box.height};
        };
        const wrapper = element.closest('.ant-drawer-content-wrapper');
        return {header: rect(element.querySelector('.ant-drawer-header')),
                body: rect(element.querySelector('.ant-drawer-body')),
                submit: rect([...element.querySelectorAll('button')]
                             .find(node => node.textContent.trim() === form['addImage'])),
                box: rect(wrapper)};
    }""".replace("form['addImage']", json.dumps(form["addImage"])))
    viewport = page.viewport_size
    check(all(drawer_box.get(k) for k in ("header", "body", "submit", "box")),
          f"the drawer did not expose its parts: {drawer_box}")
    check(regions_stacked([drawer_box["header"], drawer_box["body"]], drawer_box["box"]),
          f"drawer header and body overlap or leave the drawer: {drawer_box}")
    # A tall drawer may legitimately scroll; what must not happen is a submit control the operator
    # cannot reach. So the control is required to be reachable, not merely to start on screen: it is
    # scrolled into view and then re-measured.
    button(dialog, form["addImage"]).scroll_into_view_if_needed()
    page.wait_for_timeout(300)
    reachable = dialog.evaluate("""() => {
        const node = [...document.querySelectorAll('.ant-drawer-body button')]
            .find(n => n.textContent.trim() === MARK);
        if (!node) return null;
        const box = node.getBoundingClientRect();
        return {top: box.top, bottom: box.bottom, left: box.left, right: box.right,
                width: box.width, height: box.height,
                visible: box.top >= 0 && box.bottom <= window.innerHeight};
    }""".replace("MARK", json.dumps(form["addImage"])))
    check(reachable and reachable["visible"],
          f"the submit control cannot be brought into the viewport: {reachable} in {viewport}")
    report["register_submit_reachable"] = reachable
    def disjoint(first, second):
        return (first["bottom"] <= second["top"] + 0.5 or second["bottom"] <= first["top"] + 0.5
                or first["right"] <= second["left"] + 0.5 or second["right"] <= first["left"] + 0.5)

    check(disjoint(drawer_box["header"], drawer_box["submit"]),
          f"the submit control overlaps the drawer header: {drawer_box}")
    check(disjoint(drawer_box["body"], drawer_box["submit"]),
          f"the submit control overlaps the drawer body region: {drawer_box}")
    report["register_drawer_viewport"] = {k: {kk: round(vv, 1) for kk, vv in drawer_box[k].items()}
                                          for k in ("header", "body", "submit", "box")}
    report["evidence"].append(assert_drawer_shell(
        page, dialog, form["title"]["custom"],
        dialog.get_by_text(form["url"]["title"], exact=True),
        dialog.get_by_text(form["community"]["title"], exact=True)))
    dialog.locator(".ant-drawer-close").focus()
    from image_register_inventory import exercise_register_inventory
    report["register_inventory"] = exercise_register_inventory(page, dialog, form, mutations)
    expect(dialog.get_by_role("textbox", name=form["seriesName"], exact=True)).to_have_value("")
    reference = dialog.get_by_label(form["imageRef"], exact=True)
    submit = button(dialog, form["addImage"])
    report["last_step"] = "register-enter-reference"
    reference.press("Enter")
    expect(reference).to_have_value("")
    expect(dialog).to_be_visible()
    check(not mutations, "enter in the empty image reference sent an import")
    reference.fill("registry.example/team/enter:v1")
    reference.press("Enter")
    expect(dialog).to_be_visible()
    expect(reference).to_have_value("registry.example/team/enter:v1")
    check(not mutations, "enter in the filled image reference sent an import")
    reference.fill("")
    expect(reference).to_have_value("")
    report["register_enter_reference"] = True
    # Real Tab navigation must reach the scrollable drawer action and remain modal.
    submit_reached = False
    hidden_focus_stops = 0
    for _ in range(18):
        page.keyboard.press("Tab")
        focus = dialog.evaluate("""el => ({inside: el.contains(document.activeElement),
            tag: document.activeElement.tagName, role: document.activeElement.getAttribute('role'),
            hidden: document.activeElement.getAttribute('aria-hidden'),
            label: document.activeElement.getAttribute('aria-label')})""")
        report["last_tab_focus"] = focus
        # rc-drawer uses aria-hidden DIV sentinels outside role=dialog to wrap Tab.
        # They are not background controls; the next Tab must return to the dialog.
        hidden_focus_stops = 0 if focus["inside"] else hidden_focus_stops + 1
        check(focus["inside"] or (focus["hidden"] == "true" and focus["tag"] == "DIV"
              and focus["role"] is None and hidden_focus_stops <= 1), "Tab reached background content")
        if submit.evaluate("el => el === document.activeElement"):
            expect(submit).to_be_in_viewport(ratio=1)
            submit_reached = True
    check(submit_reached, "submit is not keyboard reachable")
    report["last_step"] = "register-keyboard-layout"
    submit.scroll_into_view_if_needed()
    expect(reference).to_be_in_viewport(ratio=1)
    expect(submit).to_be_in_viewport(ratio=1)
    ref_box, submit_box = reference.bounding_box(), submit.bounding_box()
    separated = (ref_box["y"] + ref_box["height"] <= submit_box["y"] or ref_box["y"] >= submit_box["y"] + submit_box["height"]
                 or ref_box["x"] + ref_box["width"] <= submit_box["x"] or ref_box["x"] >= submit_box["x"] + submit_box["width"])
    check(separated, "image reference overlaps Add Image")
    report["register_keyboard_layout"] = True
    dialog.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(create).to_be_focused()
    check(not mutations, "cancel must not send a mutation")
    create.click()
    expect(dialog).to_be_visible()
    from image_selection import exercise_required_fields
    exercise_required_fields(page, dialog, form, mutations, report, server.base, "register")
    dialog.locator(".ant-drawer-close").click()
    expect(dialog).not_to_be_visible()
    check(not mutations, "required-field rejection sent an image import")
    create.click()
    expect(reference).to_have_value("")
    for value, message in [("", form["imageRefRequired"]), ("   ", form["tagRequired"]),
                           (f"registry.example/{name}/runner", form["tagRequired"])]:
        reference.fill(value)
        submit.click()
        expect(dialog.get_by_text(message, exact=True)).to_be_visible()
        check(not mutations, "invalid reference sent an import")
    reference.fill("  " + ref + "  ")
    dialog.get_by_role("textbox", name=form["seriesName"], exact=True).fill(name)
    select(dialog.get_by_label(form["engine"], exact=True), engine["label"])
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    submit.click()
    expect(dialog.get_by_text(form["frameworkVersionRequired"], exact=True)).to_be_visible()
    check(not mutations, "missing framework version sent an import")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    dialog.get_by_role("textbox", name=form["engineVersion"], exact=True).fill("0.10.2")
    expect(dialog.get_by_role("switch", name=form["rollingTag"], exact=True)).not_to_be_checked()
    expect(dialog.get_by_role("combobox", name=form["hostArch"], exact=True)).to_have_attribute("aria-expanded", "false")
    expect(dialog.get_by_text("multi", exact=True)).to_be_visible()
    report["active_evidence_layer"] = "request"
    expected = {"series": [{"name": name, "engine": "vllm", "framework": "cuda", "source": "custom", "versions": [{
        "engine_version": "0.10.2", "framework_version": "12.8", "arch": "multi", "image_ref": ref,
        "source": "custom", "rolling_tag": False, "registry_sources": []}]}]}
    reason = "upstream fixture is unreachable"
    held = []
    import_path = "/api/v1/images/import"
    import_route = re.compile(re.escape(import_path) + r"(?:\?|$)")

    def hold_import(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != import_path:
            route.continue_()
            return
        held.append(route)

    page.route(import_route, hold_import)
    submit.click()
    report["last_step"] = "register-import-inflight"
    expect(submit).to_have_class(re.compile(r"\bant-btn-loading\b"))
    submit.click()
    check(len(held) == 1, "duplicate image import while the first request is held")
    check(held[0].request.post_data_json == expected, "held import body differs")
    check(urlsplit(held[0].request.url).query == "", "unexpected import query")
    with page.expect_response(lambda r: urlsplit(r.url).path == import_path and r.request.method == "POST") as refused:
        held[0].fulfill(status=502, content_type="application/json", body='{"message":"%s"}' % reason)
    page.unroute(import_route, hold_import)
    check(refused.value.status == 502, "forced image import failure did not surface")
    expect(dialog).to_be_visible()
    expect(page.locator(".ant-message").get_by_text(reason, exact=True)).to_be_visible()
    expect(reference).to_have_value("  " + ref + "  ")
    expect(dialog.get_by_role("textbox", name=form["seriesName"], exact=True)).to_have_value(name)
    expect(dialog.get_by_label(form["frameworkVersion"], exact=True)).to_have_value("12.8")
    expect(dialog.get_by_role("textbox", name=form["engineVersion"], exact=True)).to_have_value("0.10.2")
    check(collection(page.request.get(server.base + "/api/v1/images?source=custom")) == [], "refused import persisted a series")
    check(any("502" in text for text in errors), "502 console line absent: " + str(errors))
    check(failures == [{"path": import_path, "status": 502}], "unexpected HTTP failure before the refused import")
    errors[:] = [text for text in errors if "502" not in text]
    failures[:] = [item for item in failures if item.get("status") != 502]
    expect(submit).not_to_have_class(re.compile(r"\bant-btn-loading\b"))
    with page.expect_response(lambda r: urlsplit(r.url).path == import_path and r.request.method == "POST") as imported:
        submit.click()
    response = imported.value
    check(urlsplit(response.url).query == "", "import query must be empty")
    check(response.request.post_data_json == expected, "import wire body differs")
    series = collection(response)
    check(len(series) == 1, "import must return one series")
    series_id = series[0]["id"]
    check(mutations == [{"method": "POST", "path": import_path}, {"method": "POST", "path": import_path}], "unexpected mutations")
    expect(dialog).not_to_be_visible()
    expect(button(page, name)).to_be_visible()
    report["evidence"].extend([{"layer": "ui", "invalid": ["empty", "whitespace", "missing tag", "missing framework version"],
        "cancel_no_mutation": True, "keyboard_submit_and_modal_focus": True, "engine_catalog": engines,
        "inflight_posts": 1, "confirm_loading": True, "failure_status": 502, "draft_retained": True, "failure_reason_visible": True},
        {"layer": "request", "method": "POST", "path": import_path, "status": response.status, "body": expected}])
    report["active_evidence_layer"] = "persistence"
    detail = read_detail(page, server.base, series_id)
    check(detail["name"] == name and detail["engine"] == "vllm" and detail["framework"] == "cuda" and detail["source"] == "custom", "series persistence differs")
    check(len(detail["versions"]) == 1 and detail["version_count"] == 1, "version count differs")
    version = detail["versions"][0]
    for key, value in expected["series"][0]["versions"][0].items():
        check(version.get(key, [] if key == "registry_sources" else False if key == "rolling_tag" else None) == value, f"persisted version field {key} differs")
    check(version["series_id"] == series_id and version["id"] > 0, "version identity differs")
    check(detail["enabled"] and version["enabled"] and detail["purpose"] == version["purpose"] == "inference", "server defaults differ")
    check(version["present_nodes"] == 0 and version["total_nodes"] == 0, "metadata registration fabricated node availability")
    check([v for v in collection(page.request.get(server.base + "/api/v1/images/versions")) if v["id"] == version["id"]] == [version], "independent versions projection differs")
    page.reload()
    open_custom(page, text)
    button(page, name).click()
    # DetailDrawer names the dialog from the series plus its engine and framework.
    details = page.get_by_role("dialog", name=name, exact=True)
    expect(details).to_be_in_viewport(ratio=1)
    expect(details.get_by_role("link", name=ref, exact=True)).to_be_visible()
    report["last_step"] = "register-reference-code"
    code = details.get_by_role("link", name=ref, exact=True).locator("code")
    expect(code).to_have_count(1, timeout=12000)
    expect(code).to_have_text("v1")
    report["register_reference_code"] = True
    check(read_detail(page, server.base, series_id) == detail, "reload changed saved metadata")
    details.press("Escape")
    expect(details).not_to_be_visible()
    expect(button(page, name)).to_be_focused()
    report["evidence"].append({"layer": "persistence", "series": detail, "version_collection_match": True, "reload": True})
    full_id = exercise_register_full(page, server, text, form, report)
    minimum_id = exercise_register_minimum(page, server, text, form, report)
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page overflows horizontally")
    check(not errors and not failures, "browser errors or unexpected HTTP failures")
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    report["active_evidence_layer"] = "cleanup"
    cleanup(page, server.base, full_id, report)
    cleanup(page, server.base, minimum_id, report)
    cleanup(page, server.base, series_id, report)


# Contract: gpuworks/image-catalog/delete-sibling-version
# Mainline: API准备同系列无引用A/B → UI取消无mutation，再确认DELETE版本A → 独立系列/版本GET和刷新证明A消失、B/父系列不变；父系列仅清理，不声明拉取/运行时效果。
def test_delete_sibling_version(page, server, artifacts, report, locale):
    text = strings(locale)
    name = "e2e-" + report["run_id"]
    refs = [f"registry.example/{name}/runner:{tag}" for tag in ["a", "b"]]
    payload = {"series": [{"name": name, "engine": "vllm", "framework": "cuda", "source": "custom", "versions": [
        {"image_ref": ref, "engine_version": f"0.10.{i}", "framework_version": "12.8", "arch": "multi", "source": "custom"}
        for i, ref in enumerate(refs)]}]}
    seeded = collection(page.request.post(server.base + "/api/v1/images/import", data=payload))
    check(len(seeded) == 1, "fixture must create exactly one series")
    series_id = seeded[0]["id"]
    before = read_detail(page, server.base, series_id)
    check(len(before["versions"]) == 2, "fixture must create two versions")
    original = {v["image_ref"]: v for v in before["versions"]}
    a_id = original[refs[0]]["id"]
    errors, failures, mutations = observe(page)
    page.reload()
    open_custom(page, text)
    trigger = button(page, name)
    trigger.click()
    # The drawer renders as the role=dialog element, and on this page it is the only one; the
    # accessible-name lookup resolved to an outer element that does not contain the version
    # table, which is why the link lookup that followed found a hidden match.
    dialog = page.locator('[role=dialog].ant-drawer-content').filter(visible=True)
    expect(dialog).to_have_count(1)
    expect(dialog.get_by_text(f"{name}", exact=False).first).to_be_visible()
    # The version link is present and has a non-zero box, but sits inside the drawer's scrolling
    # body, so Playwright's visibility test reports hidden until it is scrolled into view. The
    # click below scrolls it in; asserting attachment is what this step needs to prove.
    for ref in refs:
        expect(dialog.locator(f'a[aria-label="{ref}"]')).to_be_attached()
    dialog.locator(f'a[aria-label="{refs[0]}"]').click()
    remove = button(dialog, text["detail"]["deleteVersion"])
    expect(remove).to_be_in_viewport(ratio=1)
    report["active_evidence_layer"] = "ui"
    remove.click()
    confirm = page.get_by_role("tooltip")
    expect(confirm.get_by_text(text["versionTable"]["deleteTitle"].replace("{{ref}}", refs[0]), exact=True)).to_be_visible()
    expect(confirm.get_by_text(text["versionTable"]["deleteDescription"], exact=True)).to_be_visible()
    button(confirm, text["common"]["cancel"]).click()
    expect(confirm).not_to_be_visible()
    check(not mutations and read_detail(page, server.base, series_id) == before, "cancel changed a version")
    remove.click()
    report["active_evidence_layer"] = "request"
    with page.expect_response(lambda r: urlsplit(r.url).path == f"/api/v1/images/{a_id}" and r.request.method == "DELETE") as deleted:
        button(confirm, text["common"]["delete"]).click()
    check(deleted.value.status == 204, "version delete must return 204")
    check(urlsplit(deleted.value.url).query == "", "delete query must be empty")
    check(deleted.value.request.post_data is None, "DELETE unexpectedly has a body")
    check(mutations == [{"method": "DELETE", "path": f"/api/v1/images/{a_id}"}], "unexpected version delete mutations")
    # Choosing a version switches the drawer to its node tab, so the versions table has to be
    # reopened before the remaining rows can be read.
    dialog.get_by_role("tab", name=re.compile(r"^" + re.escape(text["detail"]["tab"]["versions"]))).click()
    expect(dialog.locator(f'a[aria-label="{refs[0]}"]')).to_have_count(0)
    expect(dialog.locator(f'a[aria-label="{refs[1]}"]')).to_be_visible()
    report["evidence"].extend([{"layer": "ui", "cancel_no_mutation": True, "confirmation_ref": refs[0], "impact": text["versionTable"]["deleteDescription"]},
        {"layer": "request", "method": "DELETE", "path": f"/api/v1/images/{a_id}", "status": 204}])
    report["active_evidence_layer"] = "persistence"
    after = read_detail(page, server.base, series_id)
    check(after["id"] == before["id"] and after["name"] == name and after["version_count"] == 1, "parent series not preserved")
    check(after["versions"] == [original[refs[1]]], "sibling version changed or target remains")
    # Version absence must use the version collection, not the series detail route.
    versions = collection(page.request.get(server.base + "/api/v1/images/versions"))
    check(not any(v["id"] == a_id for v in versions), "deleted version remains in catalog")
    check([v for v in versions if v["series_id"] == series_id] == after["versions"], "versions list differs from parent detail")
    page.reload()
    open_custom(page, text)
    button(page, name).click()
    expect(dialog.locator(f'a[aria-label="{refs[0]}"]')).to_have_count(0)
    expect(dialog.locator(f'a[aria-label="{refs[1]}"]')).to_be_visible()
    check(read_detail(page, server.base, series_id) == after, "reload changed preserved sibling")
    dialog.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(button(page, name)).to_be_focused()
    report["evidence"].append({"layer": "persistence", "before": before, "after": after, "reload": True,
        "note": "GET /images/{id} addresses series; deletion addresses versions. Absence is checked in /images/versions."})
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page overflows horizontally")
    check(not errors and not failures, "browser errors or unexpected HTTP failures")
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    report["active_evidence_layer"] = "cleanup"
    cleanup(page, server.base, series_id, report)


if __name__ == "__main__":
    raise SystemExit(run({"register": test_image_registration, "delete-version": test_delete_sibling_version},
        product_sources=("test/e2e/image_register_inventory.py", "ui/src/pages/images/ImageArchiveURLImport.tsx",
            "ui/src/pages/images/ImageVersionColumns.tsx", "ui/src/App.tsx")))
