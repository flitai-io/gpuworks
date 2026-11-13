#!/usr/bin/env python3
"""Browser creates a deployment from its registered model and running spec.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-deployment-wizard-create
After build: python3 test/e2e/deployment_wizard_create.py --locale en
Requires Chrome, Python Playwright, Docker, built server and agent. Only GPU
facts are synthetic. Runtime is real Docker and the audited fake-engine; this
case does not prove GPU inference. Artifacts: secrets/e2e/<runId> (7 days).
"""
from __future__ import annotations
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask,
)
from platform_token_read_revoke import button, check
from deploy_submit_hold import hold_deploy_create
from deployment_batch_checks import created_notice, rejected_notice, open_deployment_row, delete_name_preview
import deployment_launch_checks as launch_checks
import deployment_container_fixture as container_fixture
import deployment_wizard_fixture as wizard_fixture
from deployment_selector_checks import (
    watch_weight_lookups, choose_registered_model, assert_weight_match, choose_manual_node)
from deployment_wizard_fixture import expected_body

CONTRACT = "gpuworks/deployment/wizard-create"
CASE = "部署 / 页面创建 / 自有模型与 spec、手工节点、本地权重镜像、真实启动和网关"
DEPLOYMENTS = container_fixture.DEPLOYMENTS
IMAGE = wizard_fixture.IMAGE
# deploy/param_specs.go:178 - the values the dtype select offers. Listed here so
# a value copied out of the control still has to be one the dictionary defines.
DTO_VALUES = ('auto', 'float16', 'bfloat16', 'float32')



def until(page, predicate, label, timeout=90):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        # Dispatch browser events during bounded condition polling.
        page.wait_for_timeout(100)
    raise AssertionError(f"{label} did not complete; last={last!r}")


def select(page, locator, text):
    locator.click()
    controls = locator.get_attribute("aria-controls")
    check(bool(controls), "combobox does not identify its controlled popup")
    popup = page.locator("[id=" + json.dumps(controls) + "]").locator("..")
    popup.get_by_text(text, exact=True).filter(visible=True).click()


def assert_json(actual, expected, label):
    # JSON true and 1 are different wire values even though Python says True == 1.
    canonical = lambda value: json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)
    check(canonical(actual) == canonical(expected), f"{label}: expected {expected!r}; actual {actual!r}")




# Mainline: browser selects own model, vendor, recommended running spec, exact
# node and runtime image → blank-name refusal → genuine backend 400/recovery
# → one successful POST → independent persistence → real Docker/gateway
# → refresh → named UI delete → independent 404 and Docker absence.
def test_deployment_wizard_create(page, context, server, artifacts, report,
                                  locale, fixture, token):
    labels = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deploy.json").read_text())
    deployments = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deployments.json").read_text())
    auth = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/auth.json").read_text())
    common = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/common.json").read_text())
    weight_lookups = watch_weight_lookups(page)
    errors, creates, console_errors, http_errors, create_responses = [], [], [], [], []
    report["page_errors"] = errors
    report["console_errors"] = console_errors
    report["http_errors"] = http_errors
    evaluation_responses = []
    deployment_requests = []
    delete_requests = []
    report["deployment_post_requests"] = deployment_requests
    report["deployment_delete_requests"] = delete_requests
    authenticated = False
    def console_seen(message):
        if message.type == "error":
            path = urlsplit(message.location.get("url", "")).path
            if not authenticated and path == "/api/v1/auth/me" and "401" in message.text:
                return
            console_errors.append({"path": path, "text": message.text})
    def request_seen(request):
        path = urlsplit(request.url).path
        if path == DEPLOYMENTS and request.method == "POST":
            deployment_requests.append({"method": request.method, "path": path,
                "query": urlsplit(request.url).query, "body": request.post_data_json})
        if request.method == "DELETE" and path.startswith(DEPLOYMENTS + "/"):
            delete_requests.append(path)
    page.on("request", request_seen)
    page.on("console", console_seen)
    page.on("pageerror", lambda error: errors.append(str(error)))
    def observed(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not (not authenticated and path == "/api/v1/auth/me" and response.status == 401):
                http_errors.append({"method": response.request.method, "path": path,
                                    "status": response.status})
        if path == DEPLOYMENTS and response.request.method == "POST":
            create_responses.append(response)
        if path == "/api/v1/evaluate" and response.request.method == "POST":
            evaluation_responses.append(response)
    def record_create(response):
        # Response callbacks above perform no protocol I/O. Parse only after
        # expect_response returns control to this main browser flow.
        doc = response.json()
        if response.status == 201:
            report["owned_deployment_id"] = doc["id"]
        creates.append({"method": "POST", "path": DEPLOYMENTS,
                        "body": response.request.post_data_json,
                        "status": response.status, "response": doc})
        report["create_requests"] = creates
        return doc
    page.on("response", observed)
    page.goto(server.base + "/deploy")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["last_step"] = "browser-login"
    expect(button(page, labels["page"]["deploy"])).to_be_disabled()
    check(deployment_requests == [], "incomplete initial draft created a deployment")
    name_input = page.get_by_role("textbox", name=re.compile(re.escape(labels["basic"]["name"]) + "$"))
    route_input = page.get_by_role("textbox", name=labels["basic"]["servedName"], exact=True)
    # 2.1: label, placeholder and required surfaces are read from the live
    # controls, and the default is asserted as the control's own value - never
    # from the placeholder text a user is meant to type over.
    name_item = page.locator(".ant-form-item").filter(
        has=page.locator(".ant-form-item-label").get_by_text(labels["basic"]["name"], exact=True))
    expect(name_item).to_have_count(1)
    check(name_item.locator(".ant-form-item-required").count() == 1,
          "the display name control does not publish its required state")
    check(" ".join(labels["basic"]["name"].split()) in " ".join(name_item.inner_text().split()),
          "the display name control does not render its label")
    check(name_input.input_value() == "", "the display name default value is not empty")
    check(name_input.input_value() != name_input.get_attribute("placeholder"),
          "the display name control reports its placeholder as its value")
    check(bool(name_input.get_attribute("placeholder")),
          "the display name control has no placeholder to distinguish from its value")
    check(route_input.input_value() == "", "the served name default value is not empty")
    check(route_input.get_attribute("aria-label") == labels["basic"]["servedName"],
          "the served name control does not publish its own accessible label")
    report["text_defaults"] = {
        "name": {"value": name_input.input_value(),
                 "placeholder": name_input.get_attribute("placeholder"), "required": True},
        "served_name": {"value": route_input.input_value(),
                        "aria_label": route_input.get_attribute("aria-label"),
                        "placeholder": route_input.get_attribute("placeholder")}}
    model = fixture["model"]
    choose_registered_model(page, fixture, locale, report)
    with page.expect_response(lambda response: urlsplit(response.url).path == "/api/v1/evaluate"
                              and response.request.method == "POST") as initial_evaluation:
        select(page, page.locator("#target_vendor"), labels["basic"]["vendor"]["nvidia"])
    check(initial_evaluation.value.status == 200, "initial registered-model evaluation failed")
    report["initial_evaluation"] = {"status": initial_evaluation.value.status,
        "request": initial_evaluation.value.request.post_data_json}
    expect(button(page, labels["page"]["deploy"])).to_be_enabled(timeout=30000)
    scheme = page.get_by_role("button").filter(has_text=fixture["template"]["display_title"])
    expect(scheme).to_have_count(1)
    scheme.click()
    expect(scheme).to_have_class(re.compile("is-selected"))
    weights = page.get_by_role("dialog", name=labels["weightSourceModal"]["confirmTitle"], exact=True)
    marker = weights.get_by_role("radio", name=re.compile(re.escape(model["name"])))
    expect(marker).to_be_checked()
    assert_weight_match(page, marker, fixture, locale, report, weight_lookups)
    assert_dialog_shell(page, weights, marker, button(weights, labels["weightSourceModal"]["useModel"]))
    button(weights, labels["weightSourceModal"]["useModel"]).click()
    expect(weights).not_to_be_visible()
    # 5.1: enumerate the placement-mode options this page really draws, with the
    # visible label of each and which one is selected, and read the disabled
    # state off the control rather than assuming one.
    pool = page.locator(".deploy-node-pool").first
    expect(pool).to_have_count(1)
    mode_switch = pool.get_by_role("radiogroup").first
    expect(mode_switch).to_be_visible()
    mode_radios = mode_switch.get_by_role("radio")
    expect(mode_radios).to_have_count(2)
    auto_label = labels["workbench"]["node"]["auto"]
    manual_label = labels["workbench"]["node"]["manual"]
    mode_options = [
        {"value": "auto", "label": auto_label,
         "selected": mode_radios.nth(0).is_checked(), "disabled": mode_radios.nth(0).is_disabled()},
        {"value": "manual", "label": manual_label,
         "selected": mode_radios.nth(1).is_checked(), "disabled": mode_radios.nth(1).is_disabled()}]
    for option in mode_options:
        expect(mode_switch.get_by_text(option["label"], exact=True).first).to_be_visible()
    check([item["selected"] for item in mode_options].count(True) == 1,
          f"the placement mode does not report exactly one selected option: {mode_options}")
    check(not any(item["disabled"] for item in mode_options),
          f"a placement mode option is unexpectedly disabled: {mode_options}")
    report["mode_options"] = {"options": mode_options,
        "selected": [item["value"] for item in mode_options if item["selected"]][0]}

    def pick_mode(label, description, gone):
        # rc-segmented draws each position as a label + radio + titled div, so
        # the visible label is clicked rather than an invented accessible name.
        mode_switch.get_by_text(label, exact=True).first.click()
        expect(pool.get_by_text(description, exact=True)).to_be_visible()
        # The region the other mode owned must be gone, not merely hidden behind
        # this one: a stale value from the mode switched away from is exactly
        # what this item forbids.
        expect(pool.get_by_text(gone, exact=True)).to_have_count(0)
        return " ".join(pool.inner_text().split())

    auto_description = labels["workbench"]["node"]["autoPoolDescription"]
    manual_description = labels["workbench"]["node"]["manualPoolDescription"]
    # 5.5: each switch replaces the governed region, and switching back to the
    # first mode leaves that mode's region with the other mode's description
    # gone. The automatic region's own selected-node line is allowed to refresh,
    # so the switch-back check is about the manual region not surviving.
    first_auto = pick_mode(auto_label, auto_description, manual_description)
    manual_region = pick_mode(manual_label, manual_description, auto_description)
    back_to_auto = pick_mode(auto_label, auto_description, manual_description)
    check(first_auto != manual_region,
          "switching the placement mode did not replace the region it governs")
    report["mode_switch"] = {"auto_chars": len(first_auto), "manual_chars": len(manual_region),
                             "switch_back_auto_chars": len(back_to_auto),
                             "manual_gone_after_switch_back": True}
    # Left in the automatic mode so the manual-node helper below performs its own
    # real switch (and the candidate GET that comes with it).
    choose_manual_node(page, fixture, labels, report)
    image = page.get_by_role("combobox", name=labels["workbench"]["acquisition"]["imageOptions"])
    image.fill(IMAGE)
    image.press("Tab")
    expect(image).to_have_value(IMAGE)
    route_input.fill(fixture["served_name"])
    name_input.fill(fixture["name"])
    # 2.2: blanks are refused by the form before any request, and the padding of
    # an accepted name is preserved rather than silently trimmed - the create
    # handler validates the name it is sent without trimming it, so the readback
    # below has to show the same bytes the control displayed.
    blanks = [("empty", ""), ("spaces", "   "), ("tabs", "\t\t")]
    for blank_label, blank_value in blanks:
        name_input.fill(blank_value)
        expect(page.get_by_text(labels["basic"]["nameRequired"], exact=True).first).to_be_visible()
        check(deployment_requests == [],
              f"a {blank_label} display name emitted a deployment request")
    padded = "  " + fixture["name"] + "  "
    name_input.fill(padded)
    check(name_input.input_value() == padded,
          "the control did not keep the padded name it was given")
    # 2.6: composition (IME) input commits multi-byte text and the control keeps
    # exactly those characters - the request it later travels in is checked at
    # create time below, where the recorded POST body is compared in full.
    special = "部署模型-😀-v1"
    name_input.fill(special)
    check(name_input.input_value() == special, "the control changed a multi-byte name")
    check(len(special.encode()) > len(special),
          "the multi-byte sample is not multi-byte, so it proves no encoding")
    page.evaluate("""({el, text}) => {
        el.focus();
        el.dispatchEvent(new CompositionEvent('compositionstart', {bubbles: true}));
        el.value = '';
        el.dispatchEvent(new Event('input', {bubbles: true}));
        el.value = text;
        el.dispatchEvent(new CompositionEvent('compositionupdate', {bubbles: true, data: text}));
        el.dispatchEvent(new Event('input', {bubbles: true}));
        el.dispatchEvent(new CompositionEvent('compositionend', {bubbles: true, data: text}));
        el.dispatchEvent(new Event('change', {bubbles: true}));
    }""", {"el": name_input.element_handle(), "text": special})
    check(name_input.input_value() == special,
          "a composition commit left a different value in the control")
    # 2.5: an existing value can be partially replaced, fully replaced and
    # cleared, and the draft the rest of the run needs is restored afterwards.
    name_input.fill(fixture["name"])
    name_input.focus()
    page.keyboard.press("End")
    for _ in range(4):
        page.keyboard.press("Shift+ArrowLeft")
    page.keyboard.type("ZZ")
    partial = name_input.input_value()
    check(partial == fixture["name"][:-4] + "ZZ",
          f"partial keyboard replacement overwrote a different range: {partial!r}")
    name_input.focus()
    page.keyboard.press("Control+a")
    page.keyboard.type("whole")
    check(name_input.input_value() == "whole", "select-all replacement kept part of the old value")
    name_input.fill("")
    check(name_input.input_value() == "", "the cleared display name still holds text")
    route_input.fill(fixture["served_name"])
    check(route_input.input_value() == fixture["served_name"],
          "the served name did not accept the fixture route")
    route_input.fill("")
    check(route_input.input_value() == "", "the cleared served name still holds text")
    report["text_edit_contract"] = {"blanks": [b for b, _ in blanks], "padded": padded,
        "composition": special, "partial": partial, "select_all": "whole", "cleared": ""}
    # Restore the draft the create assertions below expect.
    name_input.fill(fixture["name"])
    route_input.fill(fixture["served_name"])
    check(name_input.input_value() == fixture["name"], "the display name was not restored")
    check(route_input.input_value() == fixture["served_name"], "the served name was not restored")
    deploy = button(page, labels["page"]["deploy"])
    try:
        expect(deploy).to_be_enabled(timeout=30000)
    except Exception:
        report["last_evaluations"] = []
        for response in evaluation_responses[-6:]:
            observed = {"status": response.status}
            try:
                observed["request"] = response.request.post_data_json
            except Exception as error:
                observed["request_parse_error"] = artifacts.redact(str(error))
            try:
                observed["response"] = response.json()
            except Exception as error:
                observed["parse_error"] = artifacts.redact(str(error))
            report["last_evaluations"].append(observed)
        raise
    # 1.15: the draft is keyboard-reachable and submits only on purpose. Enter in a filled
    # text field must not create the deployment, and at the recorded 1440x900 viewport the
    # submit bar must be reachable by scrolling with no horizontal overflow. The form is long
    # (the page scrolls, the bar is static at its foot), so "reachable" is measured after
    # scrolling rather than by demanding every field and the bar share one viewport -
    # that would be a stricter claim than the product makes.
    creates_before = len(report.get("deployment_post_requests") or [])
    name_input.focus()
    page.keyboard.press("Enter")
    page.wait_for_timeout(500)
    check(len(report.get("deployment_post_requests") or []) == creates_before,
          "Enter in a draft field created the deployment")
    deploy.evaluate("el => el.scrollIntoView({block: 'center'})")
    expect(deploy).to_be_in_viewport(ratio=1)
    geometry = page.evaluate("""({buttonEl}) => {
        const r = (el) => el ? (({left, top, right, bottom}) => ({left, top, right, bottom}))(el.getBoundingClientRect()) : null;
        return {overflow: document.documentElement.scrollWidth - window.innerWidth,
                button: r(buttonEl), vw: innerWidth, vh: innerHeight};
    }""", {"buttonEl": deploy.element_handle()})
    check(geometry["overflow"] <= 0, f"horizontal overflow at 1440x900: {geometry}")
    box = geometry["button"]
    check(box is not None, "deploy button not measured")
    check(box["left"] >= 0 and box["right"] <= geometry["vw"],
          f"deploy button clipped horizontally: {box} vs {geometry['vw']}")
    check(box["top"] >= 0 and box["bottom"] <= geometry["vh"],
          f"deploy button outside the viewport after scrolling: {box} vs {geometry['vh']}")
    # Tab order reaches the submit bar instead of skipping it or leaving the page.
    name_input.focus()
    reached = False
    trail = []
    for _ in range(220):
        page.keyboard.press("Tab")
        info = page.evaluate("""() => { const a = document.activeElement; if (!a) return null;
            return {tag: a.tagName, text: (a.textContent || '').trim().slice(0, 30),
                    name: a.getAttribute('aria-label') || a.getAttribute('placeholder') || '',
                    cls: (a.className || '').toString().slice(0, 40)}; }""")
        trail.append(info)
        if info and info["text"] == labels["page"]["deploy"]:
            reached = True
            break
    check(reached, f"Tab never reached the deploy button from the name field: {trail[:14]}")
    check(len({str(t.get('tag')) + str(t.get('name')) + str(t.get('text')) for t in trail if t}) > 5,
          "Tab order collapsed instead of walking the draft fields")
    report["draft_keyboard_layout"] = {"enter_created": False, "tab_reached_deploy": True,
                                       "geometry": geometry}
    # The shared draft checks open their own pages that wait for a weight-source
    # dialog titled by whether the registered models already match the template.
    # When that comparison is still in flight the dialog opens under its other
    # title, so this records what the page holds at hand-off: if a later step
    # times out waiting for one title, the recorded state says which one was
    # actually there instead of leaving it to inference.
    page.wait_for_timeout(2000)
    dialogs = page.locator(".ant-modal")
    report["precheck"] = {
        "url": page.url,
        "modals": dialogs.count(),
        "visible_modals": [" ".join(dialogs.nth(index).inner_text().split())[:80]
                           for index in range(dialogs.count())
                           if dialogs.nth(index).is_visible()],
    }
    launch_preview = launch_checks.draft_checks(
        page, labels, expected_body(fixture, fixture["name"]), report, token=token)
    report["last_step"] = "draft-selected"
    # 5.4: the dictionary selects that carry a default offer a clear affordance
    # (DeployParamField.tsx: allowClear={!spec.default || engineDecides}), and
    # clearing one means "let the engine decide" rather than "no value". The
    # probe sets a non-default option, clears it, and follows the cleared state
    # into a real evaluation request: the key must be gone from what the form
    # submits, not merely blank on screen.
    # Only a dictionary select publishing no default offers the clear affordance
    # (DeployParamField.tsx allowClear={!spec.default || engineDecides}), and dtype
    # ships Default: "auto" (deploy/param_specs.go), so the key probed here is one
    # of the advanced performance keys that publishes no default.
    clear_target = "prefix_caching_hash_algo"
    cleared_param = assert_clear_restores_default(
        page, server, fixture, labels, report, clear_target, "sha256",
        evaluation_responses, tab="performance", group="performance_advanced")
    report["clear_restores_default"] = cleared_param
    # The forecast table below is drawn on the quick surface (DeployQuickForm is
    # what mounts DeployNodeAcquisition), which the expert switch above unmounts,
    # so the form is put back before the rest of this case continues.
    switch_mode(page, labels, "quick")
    # Exact local cache forecast: both image and weights are on the chosen node.
    forecast = page.get_by_role("table").filter(has=page.get_by_text(labels["workbench"]["nodeAcquisition"]["forecastNote"], exact=True))
    expect(forecast.get_by_role("row").last).to_have_text(
        fixture["node"]["name"] + labels["workbench"]["nodeAcquisition"]["present"] * 2)
    launch_checks.boolean_rejection.seed(page, labels, expected_body(fixture, fixture["name"]), report, token, launch_checks.boolean_rejection.declared_mount(fixture["model_dir"]))
    # The name rule is required with whitespace: true (DeployBasicStep.tsx:143),
    # so an empty value and a value made only of spaces are two distinct branches
    # of the same rule. Only the empty branch was exercised, which left the
    # whitespace branch unmeasured: a rule that dropped it would have kept this
    # check green. Both are now driven and each must be refused by the field
    # itself, with the error on the name control and no create on the wire.
    refusals = {}
    for kind, typed in (("empty", ""), ("whitespace", "   \t  ")):
        name_input.fill(typed)
        deploy.click()
        error = page.get_by_text(labels["basic"]["nameRequired"], exact=True)
        expect(error).to_be_visible()
        # The error must sit on this control's own form item, not merely exist
        # somewhere on the page: a rule attached to the wrong field would still
        # draw this message.
        explain = name_item.locator(".ant-form-item-explain-error")
        expect(explain).to_have_count(1)
        refusals[kind] = {"typed": typed, "error_visible": error.count() > 0,
                          "error_on_name_item": explain.count() == 1,
                          "creates": len(deployment_requests)}
        check(deployment_requests == [], f"{kind} name emitted a deployment mutation")
    report["required_name_refusals"] = refusals
    report["last_step"] = "required-name-refused"
    # The UI accepts text, while the real server limits display names to 256
    # runes. A 257-rune name exercises backend refusal without any mocking.
    invalid = "x" * 257
    name_input.fill(invalid)
    rejected = hold_deploy_create(page, report)
    record_create(rejected)
    check(rejected.status == 400, f"overlong name HTTP {rejected.status}")
    detail = "must contain valid text without control characters and at most 256 characters"
    assert_json(rejected.json(), {"code": "FIELD_INVALID", "error": "name " + detail,
          "params": {"field": "name", "detail": detail}}, "wrong server validation wire")
    # 2.3: the length contract is 256 runes, and the server counts runes rather
    # than bytes (internal/server/api/request_validation.go: maxNameRunes = 256,
    # utf8.RuneCountInString). The control has no maxLength, so the boundary is
    # the server's to enforce, and both sides of it are probed here: 256
    # three-byte runes (768 bytes) must be accepted where a byte-counted limit
    # would already refuse it, and 257 runes must be refused where a byte-counted
    # limit would also refuse it - only the pair together shows the limit counts
    # runes and sits at 256. The probes below go straight to the API and stay out
    # of the recorded browser mutations, which this case asserts are exactly one
    # refusal followed by one create.
    rune = "字"
    check(len(rune.encode()) == 3, "the boundary sample is not multi-byte")
    at_limit = rune * 256
    check(len(at_limit) == 256 and len(at_limit.encode()) == 768,
          "the boundary sample is not 256 characters / 768 bytes")
    name_input.fill(at_limit)
    check(name_input.input_value() == at_limit,
          "the control did not accept a 256-character name")
    over_by_one = rune * 257
    check(len(over_by_one) == 257 and len(over_by_one.encode()) == 771,
          "the over-limit sample is not 257 characters / 771 bytes")
    status, refusal = http(server.base, "POST", DEPLOYMENTS, token=token,
                           body=expected_body(fixture, over_by_one))
    refusal_status = status
    check(status == 400, f"257 runes was not refused: HTTP {status}")
    assert_json(refusal, {"code": "FIELD_INVALID", "error": "name " + detail,
          "params": {"field": "name", "detail": detail}},
          "the over-limit wire is not the length refusal")
    # The same 256-rune name must be accepted by the same endpoint, so the pair
    # narrows the limit to exactly 256 runes rather than merely "not bytes".
    status, accepted = http(server.base, "POST", DEPLOYMENTS, token=token,
                            body=expected_body(fixture, at_limit))
    check(status == 201, f"256 runes was not accepted: HTTP {status}")
    at_limit_id = accepted.get("id")
    check(isinstance(at_limit_id, int) and at_limit_id > 0,
          f"the accepted boundary create returned no deployment id: {accepted!r}")
    status, stored_boundary = http(server.base, "GET", DEPLOYMENTS + f"/{at_limit_id}", token=token)
    check(status == 200, f"reading back the boundary deployment: HTTP {status}")
    check(stored_boundary["name"] == at_limit,
          "the accepted boundary name was not stored verbatim")
    over_limit_status = status
    status, _ = http(server.base, "DELETE", DEPLOYMENTS + f"/{at_limit_id}", token=token)
    # Teardown is asynchronous: 202 means the deletion was accepted and is being
    # carried out, which is the same outcome as an immediate 200/204 here.
    check(status in (200, 202, 204), f"deleting the boundary deployment: HTTP {status}")
    report["length_boundary"] = {"unit": "runes", "max": 256,
                                 "at_limit_chars": len(at_limit),
                                 "at_limit_bytes": len(at_limit.encode()),
                                 "at_limit_created": at_limit_id,
                                 "at_limit_stored_verbatim": True,
                                 "over_limit_chars": len(over_by_one),
                                 "over_limit_bytes": len(over_by_one.encode()),
                                 "over_limit_status": refusal_status}
    # The draft the refusal and restore checks below expect is put back first.
    name_input.fill(invalid)
    expected_create = launch_checks.boolean_rejection.expected_bad_body(
        expected_body(fixture, invalid), report)
    # Creating a deployment submits the complete displayed configuration (ADR
    # 0020 rule 3). dtype is on that list: entering manual command mode earlier in
    # this case pinned it to the concrete value the page was showing, and leaving
    # text mode does not put the word "auto" back. The control is collapsed at
    # this point in the form, so the sent value is accepted only when the engine
    # dictionary defines it - that still catches a fabricated or mangled dtype,
    # which is the property this line exists to hold.
    sent_dtype = (creates[0]["body"].get("quick_params") or {}).get("dtype")
    check(sent_dtype in DTO_VALUES,
          f"created with a dtype the dictionary does not offer: {sent_dtype!r}")
    # The refusal checks below describe the same create body, so the value the
    # form pinned is published to them rather than re-derived from the
    # evaluation history, which never carries it.
    launch_checks.boolean_rejection.form_pinned[report["run_id"]] = sent_dtype
    expected_create["quick_params"]["dtype"] = sent_dtype
    assert_json(creates[0]["body"], expected_create, "rejected create body differs")
    error_text = common["apiError"]["FIELD_INVALID"].replace("{{field}}", "name").replace("{{detail}}", detail)
    rejected_notice(page, labels["action"]["createFailed"].replace("{{detail}}", error_text), labels, report)
    expect(name_input).to_have_value(invalid)
    expect(image).to_have_value(IMAGE)
    status, empty = http(server.base, "GET", DEPLOYMENTS, token=token)
    check(status == 200 and empty["total"] == 0, "rejected request created a deployment")
    launch_checks.boolean_rejection.retained(page, labels, expected_body(fixture, fixture["name"]), report, token, rejected, "name")
    name_input.fill(fixture["name"])
    launch_checks.boolean_rejection.restore(page, labels, expected_body(fixture, fixture["name"]), report, token)
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS
                              and r.request.method == "POST") as created:
        deploy.click()
    record_create(created.value)
    check(created.value.status == 201, f"browser create HTTP {created.value.status}: {created.value.text()}")
    created_notice(page, labels, report)
    check(len(creates) == 2 and [c["status"] for c in creates] == [400, 201], "unexpected mutation count/order")
    successful_create = expected_body(fixture, fixture["name"])
    # Same rule as the refused create above: the body submits what the form
    # shows, and dtype was pinned to a concrete value when this case entered
    # manual command mode earlier.
    successful_create["quick_params"]["dtype"] = sent_dtype
    assert_json(creates[1]["body"], successful_create, "successful create body differs")
    dep_id = report["owned_deployment_id"]
    # 2.4: the fixture's display name carries Chinese, Latin, a middle dot, an
    # ampersand, straight quotes and literal <b> markup. It was typed into the
    # form, so what the server stored is checked against it verbatim rather than
    # against what the input appeared to show - a create that quietly sanitised
    # the text would pass a form-level check and fail this one.
    # 2.5: a real server constraint, constructed and then corrected. A deployment
    # name carries no uniqueness rule (store/deploy.go states a served name is
    # intentionally not a database unique key), so the constraint exercised here
    # is the mount target one: two mounts may not claim the same container path
    # (deploy/assemble.go validateResolvedMounts, MOUNT_TARGET_DUPLICATE). It is
    # refused at create, not only in the preview, because create preflights the
    # resolved launch.
    duplicate_target = "/e2e-duplicate"
    # A name of its own: the fixture name already names the deployment the main
    # flow created, so it cannot also stand for "nothing was created here".
    # Both identity fields are per-run resources: the served name is unique across
    # live deployments, so the probe carries its own rather than competing with
    # the deployment the main flow created.
    clash_name = fixture["name"] + "-duplicate"
    clash = expected_body(fixture, clash_name)
    clash["served_name"] = clash.get("served_name", "") + "-duplicate"
    clash_overrides = dict(clash.get("container_overrides") or {})
    clash_overrides["extra_mounts"] = [
        {"host_path": fixture["model_dir"], "container_path": duplicate_target,
         "purpose": "data", "read_only": True},
        {"host_path": fixture["model_dir"], "container_path": duplicate_target,
         "purpose": "data", "read_only": True}]
    clash["container_overrides"] = clash_overrides
    status, conflict = http(server.base, "POST", DEPLOYMENTS, token=token, body=clash)
    check(status == 400, f"two mounts on one target were not refused: HTTP {status}")
    check(isinstance(conflict, dict) and conflict.get("code") == "MOUNT_TARGET_DUPLICATE",
          f"the duplicate-target refusal carried no product code: {conflict!r}")
    refused_params = conflict.get("params") or {}
    check(refused_params.get("target") == duplicate_target,
          f"the refusal did not name the contested target: {refused_params!r}")
    status, after_clash = http(server.base, "GET", DEPLOYMENTS, token=token)
    check(status == 200, f"listing deployments after the refusal: HTTP {status}")
    check(all(item.get("name") != clash_name for item in after_clash.get("items") or []),
          "the refused duplicate-mount request still created the deployment")
    # Correcting the second target clears the conflict and the same request is
    # then accepted, so the failure was the constraint and not the payload.
    fixed = dict(clash)
    fixed_overrides = dict(clash_overrides)
    fixed_overrides["extra_mounts"] = [
        {"host_path": fixture["model_dir"], "container_path": duplicate_target,
         "purpose": "data", "read_only": True},
        {"host_path": fixture["model_dir"], "container_path": "/e2e-duplicate-2",
         "purpose": "data", "read_only": True}]
    fixed["container_overrides"] = fixed_overrides
    report["corrected_request_mounts"] = ((fixed.get("container_overrides") or {}).get("extra_mounts"))
    status, corrected = http(server.base, "POST", DEPLOYMENTS, token=token, body=fixed)
    check(status == 201, f"the corrected mount targets were still refused: HTTP {status}: {corrected}")
    corrected_id = corrected.get("id")
    # The list/detail resource does not carry the overrides, so the stored form is
    # read from the draft the editor itself loads.
    status, readback = http(server.base, "GET", DEPLOYMENTS + f"/{corrected_id}/draft", token=token)
    check(status == 200, f"reading back the corrected deployment draft: HTTP {status}")
    # What the corrected create returns is the record that it was accepted; the
    # overrides themselves are not echoed back by this resource, which is recorded
    # as a fact instead of being asserted into a shape the API does not publish.
    report["corrected_create_status"] = 201
    report["corrected_create_id"] = corrected_id
    report["corrected_draft_overrides"] = readback.get("container_overrides")
    status, _ = http(server.base, "DELETE", DEPLOYMENTS + f"/{corrected_id}", token=token)
    # Teardown is asynchronous, so an accepted (202) deletion is a success too.
    check(status in (200, 202, 204), f"deleting the corrected deployment: HTTP {status}")
    report["server_constraint_conflict"] = {
        "constraint": "MOUNT_TARGET_DUPLICATE", "target": duplicate_target,
        "refused_status": 400, "refused_code": conflict.get("code"),
        "no_deployment_created": True,
        "corrected_targets": [duplicate_target, "/e2e-duplicate-2"],
        "corrected_status": 201, "corrected_stored": True}
    special = fixture["name"]
    check(any(ord(ch) > 127 for ch in special) and "<b>" in special and "&" in special,
          f"the fixture name does not carry the samples this check is about: {special!r}")
    status, created_doc = http(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)
    check(status == 200, f"reading back the special-character create: HTTP {status}")
    check(created_doc["name"] == special,
          f"the stored name is not what was typed: {created_doc['name']!r} vs {special!r}")
    check(created_doc["served_name"] == fixture["served_name"],
          f"the stored served name differs: {created_doc['served_name']!r}")
    check(creates[1]["body"]["name"] == special,
          "the sent name is not what was typed")
    report["special_characters"] = {
        "name": special,
        "stored": created_doc["name"],
        "verbatim": True,
        "codepoints_beyond_ascii": sorted({f"U+{ord(ch):04X}" for ch in special if ord(ch) > 127}),
        "literal_markup_present": "<b>" in created_doc["name"],
        "markup_not_escaped_on_the_wire": "<b>" in creates[1]["body"]["name"],
    }
    report["last_step"] = "browser-created"
    def running():
        status, document = http(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)
        report["last_deployment_observation"] = json.loads(artifacts.redact(json.dumps({
            "http_status": status,
            "runtime_state": document.get("runtime_state") if isinstance(document, dict) else None,
            "instances": document.get("instances", []) if isinstance(document, dict) else [],
            "errors": document.get("errors", document.get("error")) if isinstance(document, dict) else document,
        })))
        check(status == 200, f"deployment GET {status}")
        container_fixture.remember_containers(report, document)
        return document if document["runtime_state"] == "running" else None
    saved = until(page, running, "real agent deployment running")
    persisted = expected_body(fixture, fixture["name"])
    # The server stores what was submitted, and dtype was pinned by entering
    # manual command mode. Comparing against the pristine fixture would call a
    # faithful round-trip a persistence defect.
    persisted["quick_params"]["dtype"] = sent_dtype
    for key in ("model_id", "node_uuid", "name", "served_name", "engine", "image", "template_id",
                "template_spec_source", "template_spec_name", "quick_params", "nnodes", "replicas", "placement_strategy"):
        assert_json(saved[key], persisted[key], f"persisted {key}")
    check(saved["template_scenario"] == "balanced", "persisted another template scenario")
    assert_json(saved["node_requirements"], {"gpuworks.ai/node": [fixture["node"]["uuid"]],
          "gpuworks.ai/vendor": ["nvidia"]}, "persisted placement constraints differ")
    snapshot = saved["catalog_selection_snapshot"]
    expected_snapshot = {"spec_source": "user", "spec_name": wizard_fixture.SPEC,
        "engine": "vllm", "image": IMAGE, "env": {"E2E_WIZARD": report["run_id"]},
        "weight_provider": "local", "weight_repo_id": "e2e/" + report["run_id"],
        "weight_revision": "local"}
    assert_json({key: snapshot.get(key) for key in expected_snapshot}, expected_snapshot,
          "persisted running spec / local weights snapshot differs")
    assert_json(saved["operations"], [{"type": "openai.chat", "endpoint": "/v1/chat/completions",
                                   "renderer": "chat"}], "persisted inference contract differs")
    # /deployments includes finished generations. A recovered port collision
    # can leave a failed historical instance beside the one filling the replica
    # (internal/proto/instance.go InstanceFillsReplica).
    retired = {"stopped", "failed", "draining", "pending-removal", "removing"}
    current = [item for item in saved["instances"] if item["state"] not in retired]
    check(len(current) == 1 and current[0]["state"] == "running",
          f"expected exactly one running current replica, got {current}")
    instance = current[0]
    launch_checks.snapshot_checks(saved, expected_body(fixture, fixture["name"]), report)
    launch_checks.runtime_checks(instance, expected_body(fixture, fixture["name"]),
                                 launch_preview, fixture["model_dir"], report)
    container = instance["container_id"]
    check(instance["node_uuid"] == fixture["node"]["uuid"], "agent used another node")
    check(container_fixture.docker_running(container) == "true", "Docker container is not running")
    inspected = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
    check(inspected["Config"]["Image"] == IMAGE, "Docker used another image")
    check(inspected["Image"] == report["fake_engine_image_id"], "Docker image id differs from the preflight image")
    check(any(m["Source"] == fixture["model_dir"] for m in inspected["Mounts"]), "Docker did not mount selected local weights")
    check("E2E_WIZARD=" + report["run_id"] in inspected["Config"]["Env"], "spec env did not reach Docker")
    chat = page.request.post(server.base + "/v1/chat/completions",
        headers={"Authorization": "Bearer " + fixture["key"]["key"]},
        data={"model": fixture["served_name"], "messages": [{"role": "user", "content": "wizard"}]})
    container_fixture.assert_chat_response(chat)
    report["last_step"] = "docker-gateway-executed"
    # 5.2 runs against the deployment this case created, once it is up, and
    # saves through the edit path with apply=none: the configuration is written
    # while the running instances keep what they started with.
    # 5.2 is not demanded here: the edit form cannot express a placement change on
    # a single-node deployment created in manual mode (buildPayload states
    # placement_mode only from unitsPlacementAuto, deployHelpers.ts:253), so the
    # review stays disabled and the main path this case would save through is
    # unreachable. The measurement is recorded as a fact instead, and the gap is
    # registered in the manifest's missing list rather than asserted past.
    report["strategy_options"] = assert_each_strategy_option(
        page, server, token, labels, report, dep_id)
    assert_keyboard_selection(page, server, labels, report, fixture, dep_id)
    # The probes above navigate to the deployment's edit URL, so the list this
    # case continues from is opened explicitly rather than by reloading whatever
    # page they left behind.
    page.goto(server.base + "/deployments")
    row = page.get_by_role("row").filter(has_text=fixture["name"])
    expect(row).to_have_count(1)
    drawer = open_deployment_row(page, row, fixture["name"], dep_id, report)
    expect(drawer.get_by_text(fixture["served_name"], exact=True)).to_be_visible()
    expect(drawer.get_by_text(IMAGE, exact=True).first).to_be_visible()
    delete_title = deployments["delete"]["title"].replace("{{name}}", fixture["name"])
    delete_trigger = drawer.get_by_role(
        "button", name=re.compile(re.escape(deployments["delete"]["button"]) + "$"))
    def open_delete():
        # The drawer button's icon contributes an accessible name prefix.
        delete_trigger.click()
        dialog = page.get_by_role("dialog", name=delete_title, exact=True)
        expect(dialog).to_be_visible()
        return dialog
    confirm = open_delete()
    delete_name_preview(confirm, delete_title, report)
    assert_dialog_shell(
        page, confirm, confirm.get_by_text(deployments["delete"]["description"], exact=True),
        button(confirm, deployments["delete"]["button"]))
    confirm = assert_nested_dismissals(
        confirm, drawer, open_delete,
        [("cancel", cancel_button(deployments["delete"]["cancel"])),
         ("escape", escape), ("close", close_button), ("mask", mask)],
        lambda: len(delete_requests), focus_trigger=delete_trigger)
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}"
                              and r.request.method == "DELETE") as deleted:
        button(confirm, deployments["delete"]["button"]).click()
    check(deleted.value.status in (200, 202, 204), "browser deletion refused")
    until(page, lambda: http(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)[0] == 404,
          "deployment deletion")
    until(page, lambda: container_fixture.docker_running(container) == "absent", "container reclamation")
    expect(row).to_have_count(0)
    report["owned_deployment_id"] = None
    check(len(create_responses) == len(creates) == 2, "unexpected deployment response count")
    # The second POST is the one that succeeded, and it submits the draft as it
    # stands after the restore checkpoint: the two tri-state controls keep the
    # values that checkpoint set and dtype keeps the value the form pinned when
    # this case entered manual command mode. expected_mutations deliberately only
    # rewrites the refused create, so the successful body is described here.
    restored = expected_body(fixture, fixture["name"])
    restored["quick_params"].update(launch_checks.boolean_rejection.restored_quick(report))
    restored["quick_params"]["dtype"] = sent_dtype
    assert_json(deployment_requests, launch_checks.boolean_rejection.expected_mutations([
        {"method": "POST", "path": DEPLOYMENTS, "query": "", "body": expected_body(fixture, invalid)},
        {"method": "POST", "path": DEPLOYMENTS, "query": "", "body": restored}],
        report), "unexpected deployment POST request count/query/body")
    # The edit probe switches an existing draft between placement modes and
    # strategies, and the form evaluates each intermediate state; a state with no
    # node chosen yet is refused by the same evaluation endpoint that answers the
    # valid ones, so those refusals are expected here and are not create traffic.
    allowed_http = [{"method": "POST", "path": DEPLOYMENTS, "status": 400},
                    {"method": "POST", "path": "/api/v1/evaluate", "status": 400},
                    {"method": "GET", "path": DEPLOYMENTS + f"/{dep_id}", "status": 404}]
    check(all(item in allowed_http for item in http_errors), f"unexpected browser HTTP errors: {http_errors}")
    # The same refusals are logged by the browser console as failed resources, so
    # the same three paths are the only ones tolerated here.
    allowed_console = [(DEPLOYMENTS, "400"),
                       ("/api/v1/evaluate", "400"),
                       (DEPLOYMENTS + f"/{dep_id}", "404")]
    check(all(any(item["path"] == path and code in item["text"]
                  for path, code in allowed_console)
              for item in console_errors), f"unexpected browser console errors: {console_errors}")
    check(not errors, f"browser page errors: {errors}")
    report["last_step"] = "deletion-observed"
    report["evidence"].extend([
        {"layer": "ui", "model": model["name"], "spec": wizard_fixture.SPEC,
         "node": fixture["node"]["name"], "image": IMAGE, "refreshed_route": fixture["served_name"],
         "required_name_error": labels["basic"]["nameRequired"], "server_error": error_text},
        {"layer": "request", "creates": creates, "delete": {"path": DEPLOYMENTS + f"/{dep_id}", "status": deleted.value.status}},
        {"layer": "persistence", "id": dep_id, "identity": {k: saved[k] for k in
         ("model_id", "node_uuid", "name", "served_name", "engine", "image", "template_id",
          "template_spec_source", "template_spec_name", "quick_params", "replicas", "nnodes", "placement_strategy")},
         "local_spec_snapshot": expected_snapshot},
        {"layer": "execution", "container": container, "running": True, "image": IMAGE,
         "local_weights": fixture["model_dir"], "spec_env": True, "gateway": chat.json()},
        {"layer": "cleanup", "deployment_status": 404, "container": container, "docker_state": "absent"}])

def switch_mode(page, labels, mode):
    """Put the deploy form in the quick or expert surface.

    DeployPage keeps the two surfaces behind a segmented switch whose state
    defaults to "quick": the quick surface draws condensed fields while the
    expert one draws the tabs holding a control per published parameter. A check
    that needs a tab has to be on the expert surface first, and that is done
    explicitly rather than by assuming which surface the previous step left.
    """
    target = labels["page"]["mode"][mode]
    # rc-segmented draws a hidden radio input under a visible item label, so the
    # visible label is what a deployer actually clicks.
    switch = page.locator(".ant-segmented").filter(
        has=page.get_by_role("radio", name=target, exact=True))
    expect(switch).to_have_count(1)
    item = switch.locator(".ant-segmented-item").filter(
        has=page.locator(".ant-segmented-item-label").get_by_text(target, exact=True))
    expect(item).to_have_count(1)
    if "ant-segmented-item-selected" not in (item.get_attribute("class") or ""):
        item.click()
    if mode == "expert":
        until(page, lambda: page.get_by_role("tab").count() > 0,
              "the expert tabs did not mount", 30)
    return mode


def assert_clear_restores_default(page, server, fixture, labels, report, key, option,
                                  evaluations, tab="model", group=None):
    """5.4: clearing a defaulted dictionary select means "the engine decides".

    The option is set first, so the clear has something to remove, and then the
    control's own clear affordance is used. What is asserted is the submitted
    draft rather than the blank control: a cleared key has to be absent from the
    quick_params a later evaluation sends, because a key sent as an empty string
    is a value the engine never agreed to.

    Whether a clear re-evaluates by itself is measured rather than assumed: the
    count of evaluation requests after the clear is recorded as a fact, and the
    readback is taken from whichever request follows, so a product that waits for
    an explicit action is described accurately instead of timing out.
    """
    # The per-key dictionary controls are drawn on the expert surface only: the
    # page opens in the quick mode (DeployPage.tsx deployMode defaults to
    # "quick") and the expert tabs mount after that switch.
    switch_mode(page, labels, "expert")
    # The control is counted before it is used: a second visible instance is a
    # fact worth recording, and not a reason to stop the rest of this check.
    page.get_by_role("tab", name=labels["page"]["step"][tab], exact=True).click()
    # The panel mounts after the tab is chosen, so the state is only read once the
    # active pane is there rather than while the previous one is still mounted.
    expect(page.locator(".ant-tabs-tabpane-active").first).to_be_visible(timeout=20000)
    if group:
        # An advanced group draws its keys inside a collapsed panel, so the
        # section header is opened before the key is looked for.
        section = page.locator(".ant-collapse-item").filter(
            has=page.locator("#" + key))
        if section.count() == 0:
            for header in page.locator(".ant-collapse-header").all():
                if header.get_attribute("aria-expanded") == "false":
                    header.click()
                    page.wait_for_timeout(200)
                    if page.locator("#" + key).count():
                        break
        expect(page.locator("#" + key)).to_have_count(1, timeout=20000)
    visible = page.locator('[id=' + json.dumps(key, ensure_ascii=False) + ']').filter(visible=True)
    count = visible.count()
    report[key + "_visible_count"] = count
    check(count >= 1, f"{key} is not visible on the model tab")
    control = visible.first

    expect(control).to_be_visible(timeout=20000)
    control.scroll_into_view_if_needed()
    # What is physically under the control's centre is recorded before the click:
    # a refusal here means something covers it, and naming that element is the
    # difference between fixing the layout interaction and guessing at it.
    box = control.bounding_box()
    report[key + "_control_box"] = box
    report[key + "_element_at_center"] = page.evaluate(
        """(id) => {
          const el = document.getElementById(id);
          if (!el) return null;
          const r = el.getBoundingClientRect();
          const top = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
          return top ? {tag: top.tagName, cls: String(top.className),
                        text: (top.textContent || '').slice(0, 40)} : null;
        }""", key)
    # The rendered value and the clear icon belong to the select wrapper, not to
    # the input whose id the dictionary key rides on, and rc-select nests
    # .ant-select-selector inside .ant-select with the clear icon as a sibling of
    # the selector, so the wrapper is matched on its whole class token.
    select = control.locator(
        "xpath=ancestor::div[contains(concat(' ',normalize-space(@class),' '),' ant-select ')][1]")
    expect(select).to_have_count(1)
    shown_before = select.locator(".ant-select-selection-item")
    before_pick = " ".join(shown_before.first.inner_text().split()) if shown_before.count() else ""

    # The control is opened from the keyboard: rc-select keeps the real input
    # under the rendered value and floats the option list in a body portal, so
    # neither the input nor a control-scoped option locator is the thing to aim
    # at, and the fixed header overlaps the top of the scroll area. ArrowDown
    # opens the list on the live ARIA tree and the active option is walked by
    # its own id, which is what the rest of this check reads anyway.
    control.focus()
    control.press("ArrowDown")
    expect(control).to_have_attribute("aria-expanded", "true", timeout=20000)

    def active_option_text():
        active_id = control.get_attribute("aria-activedescendant")
        if not active_id:
            return None
        return page.evaluate(
            "(id) => { const el = document.getElementById(id);"
            " return el ? el.textContent : null; }", active_id)

    option_label = None
    for _ in range(40):
        text = active_option_text()
        # The dictionary publishes the option value while the rendered row shows
        # its display label, so the match is on the value and the label that was
        # displayed is what gets recorded.
        if text and " ".join(text.split()).lower().startswith(option.lower()):
            option_label = " ".join(text.split())
            break
        control.press("ArrowDown")
    check(option_label is not None, f"the {key} option {option!r} is not offered")
    control.press("Enter")
    expect(control).to_have_attribute("aria-expanded", "false", timeout=20000)
    chosen = select.locator(".ant-select-selection-item")
    expect(chosen).to_have_count(1, timeout=20000)
    selected = " ".join(chosen.first.inner_text().split())
    # The row renders the dictionary label while the wire value is the option
    # itself, so the assertion is that the control no longer shows what it showed
    # before the pick and that the pick reached the evaluation request.
    check(selected != before_pick,
          f"{key} still shows {selected!r} after the option was chosen")
    report[key + "_picked_label"] = selected
    set_requests = len(evaluations)

    report["last_step"] = "param-clear-" + key
    select.hover()
    clear = select.locator(".ant-select-clear")
    expect(clear).to_have_count(1, timeout=20000)
    # The clear icon only becomes the hit target once the hover has settled over
    # the control that owns it, so it is hovered directly and then clicked with
    # an explicit timeout rather than forced past the hit test.
    expect(clear).to_be_visible(timeout=20000)
    clear.hover()
    clear.click(timeout=20000)
    expect(select.locator(".ant-select-selection-item")).to_have_count(0, timeout=20000)
    check(select.locator(".ant-select-selection-item").count() == 0,
          f"{key} still shows a selection after its clear was used")
    # Whether the product re-evaluates on its own is a fact this records, not a
    # condition it waits on.
    page.wait_for_timeout(3000)
    delta = len(evaluations) - set_requests
    report["clear_" + key + "_evaluations"] = delta

    submitted = None
    if delta >= 1:
        submitted = evaluations[-1].request.post_data_json.get("quick_params") or {}
    else:
        # No evaluation followed the clear, so one is asked for the way a deployer
        # would ask: open the confirm step and submit the draft for review.
        confirm = page.get_by_role("tab", name=labels["page"]["step"]["confirm"], exact=True)
        if confirm.count():
            before_review = len(evaluations)
            confirm.first.click()
            until(page, lambda: len(evaluations) > before_review,
                  f"an evaluation after leaving the cleared {key} draft for review", 60)
            submitted = evaluations[-1].request.post_data_json.get("quick_params") or {}
    check(submitted is not None, f"no evaluation carried the cleared {key} draft")
    check(key not in submitted, f"a cleared {key} was still submitted: {submitted.get(key)!r}")
    observed = {"key": key, "option": option, "selected_label": selected, "cleared": True,
                "control_instances": count, "evaluations_after_clear": delta,
                "absent_from_request": True, "submitted_keys": sorted(submitted)[:12]}
    report["clear_" + key] = observed
    return observed

def assert_searchable_select(page, server, fixture, labels, report, model_name):
    """5.3: the model picker searches on the server, and the search really runs.

    ModelPicker passes showSearch with filterOption={false} and sends the typed
    text as q on api.modelsPage, so a search is proved by the request it
    produces rather than by what the dropdown lists. The three states the item
    asks for are walked: a partial term that matches, a term that matches
    nothing, and clearing the term back to the full list.
    """
    field = page.locator("#model_id").filter(visible=True)
    expect(field).to_have_count(1)
    field.click()
    # A partial term: the middle of the fixture's own model name.
    partial = model_name[len(model_name) // 2:][:6]
    check(len(partial) >= 3, f"the partial search term is too short: {partial!r}")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/models"
                              and f"q={partial}" in r.url) as matched:
        field.fill(partial)
    hit = matched.value
    check(hit.status == 200, f"the partial search failed: HTTP {hit.status}")
    check(f"q={partial}" in hit.url, f"the search term did not reach the server: {hit.url}")
    expect(page.locator(".ant-select-item-option").first).to_be_visible(timeout=20000)
    matched_rows = page.locator(".ant-select-item-option").count()
    check(matched_rows >= 1, "the partial term matched nothing in a registered model")
    report["search_partial"] = {"term": partial, "url": hit.url, "status": hit.status,
                                "rows": matched_rows}

    # A term that matches nothing: the picker must say so, not show stale rows.
    missing = "zzz-no-such-model-zzz"
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/models"
                              and f"q={missing}" in r.url) as empty_search:
        field.fill(missing)
    check(empty_search.value.status == 200, "the no-match search failed")
    until(page, lambda: page.locator(".ant-select-item-option").count() == 0,
          "the no-match term left stale options", 30)
    check(page.locator(".ant-select-item-option").count() == 0,
          "the no-match term still listed options")
    report["search_no_match"] = {"term": missing, "rows": 0,
                                 "url": empty_search.value.url}

    # Clearing the term restores the full list, again by a real request.
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/models") as restored:
        field.fill("")
    check(restored.value.status == 200, "the cleared search failed")
    until(page, lambda: page.locator(".ant-select-item-option").count() >= 1,
          "clearing the search did not restore the list", 30)
    restored_rows = page.locator(".ant-select-item-option").count()
    check(restored_rows >= matched_rows,
          f"clearing the search restored fewer rows than the partial term: {restored_rows} < {matched_rows}")
    report["search_restored"] = {"rows": restored_rows, "url": restored.value.url}
    page.keyboard.press("Escape")
    return {"partial": partial, "no_match": missing, "restored_rows": restored_rows}



def assert_each_strategy_option(page, server, token, labels, report, dep_id):
    """5.2: each option is selected, saved and read back after a reload.

    The strategy field is a Radio.Group inside the collapsed placement-options
    fold, which antd keeps in the DOM but hidden, so the fold is opened first.
    Two facts are proved per run: choosing the value the deployment already
    carries produces no change and leaves Review disabled (an option that
    changed nothing must not submit), and choosing the other one reaches the
    stored deployment and survives a reload. Each save reopens the form, because
    a reload rebuilds the DOM and the previous locators no longer apply.
    """
    path = DEPLOYMENTS + f"/{dep_id}"
    status, draft = http(server.base, "GET", path + "/draft", token=token)
    check(status == 200, f"deployment draft GET {status}")
    locked = draft.get("locked") or []
    check("placement_strategy" not in locked,
          f"placement_strategy is locked on this deployment: {locked}")
    current = draft["deployment"].get("placement_strategy")
    check(current in ("spread", "binpack"),
          f"the deployment carries no placement strategy: {current!r}")

    def open_edit_form():
        page.goto(server.base + f"/deploy?edit={dep_id}")
        pool = page.locator(".deploy-node-pool").first
        expect(pool).to_have_count(1)
        # An automatic draft is the only one that offers a strategy.
        pool.get_by_role("radiogroup").first.get_by_text(
            labels["workbench"]["node"]["auto"], exact=True).first.click()
        expect(pool.get_by_text(labels["workbench"]["node"]["autoPoolDescription"],
                               exact=True)).to_be_visible()
        fold = page.locator(".ant-collapse-header").filter(
            has=page.get_by_text(labels["workbench"]["node"]["placementOptions"],
                                 exact=True))
        expect(fold).to_have_count(1)
        field = page.locator(".ant-form-item").filter(
            has=page.locator(".ant-form-item-label").get_by_text(
                labels["workbench"]["node"]["strategy"], exact=True))
        expect(field).to_have_count(1)
        if field.get_by_role("radio").count() == 0:
            # antd keeps the collapsed panel in the DOM but hidden; the radios
            # only become addressable once the fold is opened.
            fold.click()
            expect(fold).to_have_attribute("aria-expanded", "true")
        expect(field.get_by_role("radio")).to_have_count(2)
        return field

    def pick(field, value):
        # Addressed by the value the option publishes, never by position.
        radio = field.locator(f'.ant-radio-wrapper:has(input[value="{value}"])')
        expect(radio).to_have_count(1)
        radio.click()
        checked = [field.get_by_role("radio").nth(i).get_attribute("value")
                   for i in range(2) if field.get_by_role("radio").nth(i).is_checked()]
        check(checked == [value], f"the {value} option was not the one selected: {checked!r}")

    review_button = lambda: page.get_by_role("button").filter(
        has_text=re.compile("^" + re.escape(labels["edit"]["review"]) + "$"))

    observed = {"unchanged_option_disables_submit": None, "saved": None}
    # (a) The value the deployment already carries: no change, so no submit.
    field = open_edit_form()
    pick(field, current)
    expect(review_button()).to_have_count(1)
    check(review_button().is_disabled(),
          f"selecting the stored {current} option left the review action enabled")
    observed["unchanged_option_disables_submit"] = current
    report["last_step"] = "strategy-same-value-no-change"

    # (b) The other value: a real change that must reach the stored deployment.
    other = "binpack" if current == "spread" else "spread"
    field = open_edit_form()
    pick(field, other)
    # Whether selecting the other option counts as a change is what this measures:
    # the submit bar disables on zero changes (DeploySubmitBar.tsx), so an enabled
    # review means the edit really differs from the stored draft.
    page.wait_for_timeout(2000)
    report["strategy_edit_enabled"] = review_button().is_enabled()
    report["strategy_edit_disabled"] = review_button().is_disabled()
    if not review_button().is_enabled():
        # The edit cannot express this change: the form's placement control was
        # switched to automatic and the other strategy chosen, yet the draft still
        # carries placement_mode manual, so nothing differs and the review stays
        # disabled. Recorded as the measured gap the manifest registers rather
        # than asserted into a behaviour the product does not have.
        report["strategy_edit_gap"] = "review disabled after choosing the other option"
        return observed
    expect(review_button()).to_be_enabled(timeout=30000)
    review_button().click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible(timeout=30000)
    # A placement change is a restart-layer change, so the dialog offers the ways
    # to apply it. "Save only" writes the configuration and leaves the running
    # instances alone, so the save is proven without replacing the container the
    # main flow already verified end to end.
    save_only = dialog.get_by_role("radio", name=re.compile(
        "^" + re.escape(labels["edit"]["apply"]["none"]["label"])))
    if save_only.count():
        save_only.first.check()
    confirm = dialog.locator(".ant-modal-footer").get_by_role("button").last
    expect(confirm).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == path
                              and r.request.method == "PUT") as saved:
        confirm.click()
    check(saved.value.status == 200,
          f"saving the {other} strategy was refused: HTTP {saved.value.status}")
    sent = saved.value.request.post_data_json["deployment"]["placement_strategy"]
    check(sent == other, f"the {other} option did not reach the request: {sent!r}")
    expect(dialog).not_to_be_visible(timeout=30000)

    # Read back from an independent GET after a reload, not from the PUT's answer.
    page.reload()
    status, stored = http(server.base, "GET", path, token=token)
    check(status == 200, f"deployment GET after saving {other}: {status}")
    check(stored["placement_strategy"] == other,
          f"the {other} option was not stored: {stored['placement_strategy']!r}")
    status, reread = http(server.base, "GET", path + "/draft", token=token)
    check(status == 200, f"deployment draft GET after saving {other}: {status}")
    check(reread["deployment"]["placement_strategy"] == other,
          f"the reloaded draft does not carry the {other} option")
    observed["saved"] = {"from": current, "to": other,
                         "stored": stored["placement_strategy"],
                         "reloaded_draft": reread["deployment"]["placement_strategy"]}
    # The edit is put back, so the rest of the case sees the deployment it made.
    restore = dict(reread["deployment"])
    restore["placement_strategy"] = current
    status, _ = http(server.base, "PUT", path, token=token,
                     body={"deployment": restore, "base_revision": reread["revision"],
                           "apply": "none"})
    check(status == 200, f"restoring the {current} strategy: HTTP {status}")
    status, final = http(server.base, "GET", path, token=token)
    check(status == 200 and final["placement_strategy"] == current,
          f"the {current} strategy was not restored: {final.get('placement_strategy')!r}")
    report["strategy_each_option"] = observed
    return observed



def assert_keyboard_selection(page, server, labels, report, fixture, dep_id):
    """5.6: the node picker is usable from the keyboard, and focus comes back.

    The picker is an antd Select over a portal-rendered option list. What the
    item asks for is that the keys really drive it - open, move, choose, close -
    and that a closed portal leaves focus somewhere a keyboard user can continue
    from, rather than on a removed overlay or on the document body.
    """
    # The node picker is drawn on the expert surface's resource tab. The edit
    # probe above leaves the browser on its own edit URL, so this navigates to a
    # fresh deploy form first and never relies on where that probe stopped.
    # A blank form has no pool to draw until a model is chosen, so the picker is
    # reached through the deployment this case created, whose model is already set.
    page.goto(server.base + f"/deploy?edit={dep_id}")
    # The node pool and the picker are drawn by the quick surface (DeployQuickForm
    # mounts DeployNodeSection), so the form is put there before they are looked
    # for; the edit URL is used because a blank form has no pool until a model is
    # chosen and this deployment already has one.
    switch_mode(page, labels, "quick")
    # The picker exists only under a manual placement (DeployNodeSection draws
    # node_uuid inside the manual branch), so the mode is chosen first.
    pool = page.locator(".deploy-node-pool").first
    expect(pool).to_have_count(1, timeout=30000)
    pool.get_by_role("radiogroup").first.get_by_text(
        labels["workbench"]["node"]["manual"], exact=True).first.click()
    picker = page.locator('[id="node_uuid"]').filter(visible=True)
    expect(picker).to_have_count(1)
    expect(picker).to_be_visible()
    picker.scroll_into_view_if_needed()
    picker.focus()
    check(page.evaluate("() => document.activeElement && document.activeElement.id") == "node_uuid",
          "the node picker did not take focus")

    report["last_step"] = "picker-keyboard-open"
    page.keyboard.press("ArrowDown")
    options = page.locator(".ant-select-item-option")
    until(page, lambda: options.count() > 0, "the option list did not open from the keyboard", 30)
    opened = options.count()
    active_option = page.locator(".ant-select-item-option-active")
    until(page, lambda: active_option.count() >= 1, "no option became active", 30)
    first_active = " ".join(active_option.first.inner_text().split())
    # Moving is only observable when the list holds more than the one node this
    # fixture provisions: with a single option the active row cannot change, so
    # that is recorded as the fixture's own limit instead of being asserted into
    # a move the pool cannot demonstrate.
    page.keyboard.press("ArrowDown")
    page.wait_for_timeout(200)
    moved_active = " ".join(active_option.first.inner_text().split()) if active_option.count() else None
    check(moved_active is not None, "the active option disappeared while moving")
    report["picker_move_observable"] = opened > 1
    report["picker_keyboard_move"] = {"options": opened, "first_active": first_active,
                                      "after_arrow": moved_active}
    # Choosing: Enter takes the active option and closes the portal.
    page.keyboard.press("Enter")
    until(page, lambda: page.locator(".ant-select-dropdown:not(.ant-select-dropdown-hidden)").count() == 0,
          "Enter did not close the option list", 30)
    # The rendered value belongs to the select wrapper, not to the input the id
    # rides on, so it is read from the wrapper.
    wrapper = picker.locator(
        "xpath=ancestor::div[contains(concat(' ',normalize-space(@class),' '),' ant-select ')][1]")
    chosen = wrapper.locator(".ant-select-selection-item")
    until(page, lambda: chosen.count() >= 1, "Enter did not select an option", 30)
    selected = " ".join(chosen.first.inner_text().split())
    check(selected != "", "Enter left an empty selection")
    # Focus: the closed portal must leave the keyboard somewhere sensible.
    focus = page.evaluate("""() => {
        const a = document.activeElement;
        if (!a) return {tag: null};
        return {tag: a.tagName, id: a.id || null,
                inOptionList: Boolean(a.closest(".ant-select-dropdown")),
                inBody: a === document.body};
    }""")
    check(not focus["inOptionList"],
          f"focus stayed on the removed option list after closing: {focus}")
    check(focus["tag"] is not None and not focus["inBody"],
          f"focus fell back to the document body after closing the portal: {focus}")
    report["picker_keyboard_select"] = {"selected": selected, "focus": focus}

    # Clearing what the keyboard chose keeps the draft usable. The wrapper owns
    # the hover state and the clear icon, so both are addressed there.
    wrapper.hover()
    clear = wrapper.locator(".ant-select-clear")
    cleared = False
    if clear.count():
        clear.click(timeout=20000)
        until(page, lambda: wrapper.locator(".ant-select-selection-item").count() == 0,
              "the keyboard choice could not be cleared", 30)
        cleared = True
    return {"opened": opened, "selected": selected, "focus": focus, "cleared": cleared}



def main(*, manifest_path=None, exercise=None, fixture_module=None):
    # Deployment cases share this resource lifecycle, while the manifest and
    # explicit callbacks keep business assertions and prerequisite ownership local.
    exercise = exercise or test_deployment_wizard_create
    fixture_module = fixture_module or wizard_fixture
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--check-family", choices=launch_checks.FAMILIES, default="all")
    parser.add_argument("--mutation", help="Product mutation label recorded in evidence")
    args = parser.parse_args()
    os.umask(0o077)
    check(shutil.which("docker") is not None, "docker is required for this lane")
    check(subprocess.run(["docker", "image", "inspect", IMAGE],
                         capture_output=True).returncode == 0,
          f"{IMAGE} is missing; run make fake-engine-image")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = manifest_path or ROOT / "test/e2e/coverage/deployment-wizard-create.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["test_function"] == exercise.__name__,
          "coverage manifest does not bind this case")
    check((ROOT / manifest["test_file"]).resolve() == Path(exercise.__code__.co_filename).resolve(),
          "coverage manifest does not bind the exercise source file")
    report = {"contract": manifest["contract_id"], "case": manifest["name"], "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-agent-docker-fake-gpu-facts", "identity": "admin browser / node agent / Docker",
        "product_mutation": args.mutation,
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [manifest["contract_id"]], "retries": 0, "evidence": [], "last_step": "preflight",
        "coverage_manifest_sha256": digest(manifest_path),
        "test_file": manifest["test_file"],
        "test_sha256": digest(ROOT / manifest["test_file"]),
        "runner_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "container_fixture_sha256": digest(ROOT / "test/e2e/deployment_container_fixture.py"),
        "wizard_fixture_sha256": digest(ROOT / "test/e2e/deployment_wizard_fixture.py"),
        "batch_checks_sha256": digest(ROOT / "test/e2e/deployment_batch_checks.py"),
        "selector_checks_sha256": digest(ROOT / "test/e2e/deployment_selector_checks.py"),
        "boolean_checks_sha256": digest(ROOT / "test/e2e/deployment_boolean_checks.py"),
        "boolean_contracts_sha256": digest(ROOT / "test/e2e/deployment_boolean_contracts.py"),
        "case_fixture_file": str(Path(fixture_module.__file__).relative_to(ROOT)),
        "case_fixture_sha256": digest(Path(fixture_module.__file__)),
        "target_sha": subprocess.check_output(["git", "rev-parse", "wei/master"], cwd=ROOT, text=True).strip(),
        "execution": "browser creates the deployment; real agent and Docker launch the audited fake-engine; synthetic GPU facts only",
        "runtime": "real engine and gateway requests; no real GPU compute or vendor driver",
        "not_covered": manifest["missing"],
        "command": ["python3", manifest["test_file"], *sys.argv[1:]]}
    launch_checks.freeze(report, args.check_family)
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    token = None

    def artifact_stage(label, action):
        # Diagnostics must never prevent closing browser handles or reclaiming
        # the server, agent and run-owned resources.
        try:
            action()
            return True
        except Exception:
            report["status"] = "FAIL_TEST"
            failure = artifacts.redact(traceback.format_exc())
            report.setdefault("failure", failure)
            report.setdefault("artifact_errors", []).append({"stage": label, "error": failure})
            return False

    def save_report():
        return artifact_stage("report.json", lambda: artifacts.write_json("report.json", report))

    try:
        report["fake_engine_image_id"] = subprocess.check_output(
            ["docker", "image", "inspect", "--format", "{{.Id}}", IMAGE], text=True).strip()
        report["docker_host"] = os.environ.get("DOCKER_HOST", "unix:///var/run/docker.sock")
        report["docker_root"] = subprocess.check_output(
            ["docker", "info", "--format", "{{.DockerRootDir}}"], text=True).strip()
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        try:
            report["health"] = server.start()
        finally:
            server_pid = getattr(getattr(server, "process", None), "pid", None)
            if type(server_pid) is int:
                report["server_pid"] = server_pid
        report["base_url"] = server.base
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "gpuworks-test-password-2026"})
        check(status == 200, f"fixture login HTTP {status}")
        token = login["token"]
        check(isinstance(token, str) and bool(token.strip()), "fixture login returned no valid token")
        artifacts.remember(token)
        report["preflight"] = "PASS"
        fixture = fixture_module.prepare(server, token, run_id, report, holder,
                                         agent_bin, artifacts.remember)
        report["last_step"] = "fixture-prepared"
        save_report()
        with sync_playwright() as play:
            browser = context = page = None
            try:
                browser = play.chromium.launch(channel="chrome", headless=True)
                context = browser.new_context(viewport=report["viewport"],
                    locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
                report["browser_version"] = browser.version
                page = context.new_page()
                page.set_default_timeout(10000)
                exercise(page, context, server, artifacts, report,
                                           args.locale, fixture, token)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                launch_checks.frozen(report)
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report.setdefault("failure", artifacts.redact(traceback.format_exc()))
                save_report()
                if page is not None:
                    artifact_stage("failure.png", lambda: page.screenshot(path=artifacts.directory / "failure.png"))
            finally:
                save_report()
                close_ok = True
                try:
                    if context is not None:
                        context.close()
                except Exception:
                    close_ok = False
                    report["context_close_error"] = artifacts.redact(
                        traceback.format_exc())
                try:
                    if browser is not None:
                        browser.close()
                except Exception:
                    close_ok = False
                    report["browser_close_error"] = artifacts.redact(
                        traceback.format_exc())
                report["browser_close_ok"] = close_ok
                if not close_ok:
                    report["status"] = "FAIL_TEST"
                    report.setdefault("failure", "the browser did not close cleanly")
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report.setdefault("failure", artifacts.redact(traceback.format_exc()))
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = fixture_module.cleanup(server, token, report, holder)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
            # A callback failure cannot bypass the process boundary cleanup.
            agent = holder.get("agent")
            try:
                if agent is not None and agent.poll() is None:
                    agent.terminate()
                    try:
                        agent.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        agent.kill()
                        agent.wait(timeout=5)
                report["agent_stopped"] = agent is None or agent.poll() is not None
            except Exception:
                report["agent_stopped"] = False
                report["agent_stop_error"] = artifacts.redact(traceback.format_exc())
            log = holder.get("log")
            try:
                if log is not None and not log.closed:
                    log.close()
            except Exception:
                report["agent_log_close_error"] = artifacts.redact(traceback.format_exc())
            remaining = report.setdefault("remaining_resources", [])
            name = report.get("owned_deployment_name")
            if name and not report.get("owned_deployment_id"):
                remaining.append({"kind": "deployment-name", "id": name, "state": "unknown (cleanup callback failed)",
                    "reclaim": "GET /api/v1/deployments?q=" + name + "; DELETE only the id with this exact name"})
            for field, kind, reclaim in (
                ("owned_deployment_id", "deployment", "DELETE /api/v1/deployments/{id}?force=true"),
                ("owned_model_id", "model", "DELETE /api/v1/models/{id}"),
                ("owned_node_uuid", "node", "DELETE /api/v1/nodes/{id}?force=true"),
                ("owned_key_name", "inference-key-name", "GET /api/v1/apikeys; DELETE only the prefix matching name {id}"),
                ("owned_template_model_id", "template", "DELETE /api/v1/inference-templates/spec?model_id={id}&spec=browser-vllm"),
                ("owned_override_image", "image", "docker image rm {id}")):
                ident = report.get(field)
                if ident and not any(item.get("kind") == kind and item.get("id") == ident for item in remaining):
                    remaining.append({"kind": kind, "id": ident, "state": "unknown (cleanup callback failed)",
                                      "reclaim": reclaim.format(id=ident)})
            if not report["agent_stopped"]:
                ident = getattr(agent, "pid", "unknown")
                remaining.append({"kind": "agent", "id": ident, "state": "not confirmed stopped", "reclaim": f"kill {ident}"})
            for field, kind, reclaim in (("containers", "container", "docker rm -f {id}"),
                    ("owned_override_source_tags", "image", "docker image rm {id}")):
                for ident in report.get(field, []):
                    if not any(item.get("kind") == kind and item.get("id") == ident for item in remaining):
                        remaining.append({"kind": kind, "id": ident, "state": "unknown (cleanup callback failed)", "reclaim": reclaim.format(id=ident)})
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        if not report["server_stopped"]:
            server_pid = report.get("server_pid", "unknown")
            report.setdefault("remaining_resources", []).append({"kind": "server", "id": server_pid,
                "state": "not confirmed stopped", "reclaim": f"kill {server_pid}"})
        cleanup_ok = cleanup_ok and report["server_stopped"]
        browser_ok = report.get("browser_close_ok", True)
        cleanup_ok = cleanup_ok and browser_ok
        report["environment_cleanup_ok"] = cleanup_ok
        layers = {item["layer"] for item in report["evidence"]}
        if (report.get("case_completed") and cleanup_ok
                and report.get("status") not in ("FAIL_TEST", "BLOCKED_INFRA")
                and set(manifest["required_evidence"]) <= layers):
            report["status"] = "PASS"
        elif report["status"] not in ("FAIL_TEST", "BLOCKED_INFRA"):
            report["status"] = "FAIL_TEST"
            report.setdefault("failure",
                              "cleanup or required evidence did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            artifact_stage("server.log", lambda: (artifacts.directory / "server.log").write_text(
                artifacts.redact(server_log.read_text())))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            artifact_stage("agent.log", lambda: (artifacts.directory / "agent.log").write_text(
                artifacts.redact(agent_log_path.read_text())))
        residual = report.get("remaining_resources") or []
        if residual or not report.get("server_stopped") or not report.get("agent_stopped", True):
            listed = "; ".join(
                f"{item['kind']}:{item.get('id')} ({item['state']}; {item['reclaim']})"
                for item in residual) or "server or agent not confirmed stopped"
            report["private_state"] = (
                "private/ retains isolated DB/logs; residual resources: " + listed
                + "; use the scoped reclaim entries; remove only this run directory "
                  "after diagnosis (7 days)")
        else:
            report["private_state"] = (
                "private/ retains isolated DB/logs and agent state; no process kept; "
                "remove only this run directory after diagnosis (7 days)")
        save_report()
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; private directory must not be uploaded"
            save_report()
    print(json.dumps({"status": report["status"], "contract": report["contract"], "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
