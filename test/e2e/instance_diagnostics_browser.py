"""Instance diagnostics / live stdout → stored logs, engine gauges and launch facts.

Run: make test-e2e-instance-diagnostics E2E_LOCALE=en
Synthetic GPU facts schedule a real Alpine/fake-engine container; no GPU claim.
"""
import json
import re
import shlex
import time
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import button, check
import instance_diagnostics_fixture as fixture

RANGES = {"15m": (900000, "10s"), "1h": (3600000, "30s"),
          "6h": (21600000, "2m"), "1d": (86400000, "10m"),
          "1w": (604800000, "1h"), "1mo": (2592000000, "4h"),
          "1y": (31536000000, "24h")}


def strings(locale, namespace="deployments"):
    return json.loads((ROOT / "ui/src/i18n/locales" / locale / (namespace + ".json")).read_text())


def login(page, context, server, artifacts, report, locale, username="admin"):
    auth = strings(locale, "auth")
    report["sensitive_phase"] = True
    page.goto(server.base + "/deployments")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill(username)
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        if cookie.get("value"):
            artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=strings(locale)["page"]["title"], exact=True)).to_be_visible()
    report["sensitive_phase"] = False


def open_instance(page, server, data, report):
    instance = fixture.running(page, server, data, report)
    row = page.get_by_role("row").filter(has_text=data["name"])
    expect(row).to_have_count(1)
    path = "/api/v1/instances/" + instance["uuid"] + "/logs"
    with page.expect_response(lambda r: urlsplit(r.url).path == path) as streamed:
        row.get_by_role("button", name=data["name"], exact=True).click()
    request_contract(streamed.value, path, {})
    check(streamed.value.headers.get("content-type") == "text/event-stream", "live logs are not SSE")
    report["evidence"].append({"layer": "request", "live_logs": {"method": "GET", "path": path,
        "query": {}, "body": None, "status": 200, "content_type": "text/event-stream"}})
    drawer = page.get_by_role("dialog", name=data["name"], exact=True)
    expect(drawer).to_be_in_viewport(ratio=1)
    panel = drawer.locator('[id="instance-' + instance["uuid"] + '"]')
    expect(panel).to_be_visible()
    return drawer, panel


def prove_refused_delete(page, dialog, server, dep_id, delete, report):
    """Hold Delete: loading, no second write, a 502 keeps the dialog and the deployment."""
    reason = "upstream fixture is unreachable"
    path = "/api/v1/deployments/" + str(dep_id)
    held = []

    def hold_delete(route):
        if route.request.method != "DELETE" or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route("**" + path, hold_delete)
    refused = None
    try:
        ok = dialog.locator(".ant-modal-footer .ant-btn-primary")
        ok.click()
        expect(ok).to_have_class(re.compile(r"\bant-btn-loading\b"))
        ok.click()
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline and not held:
            page.wait_for_timeout(100)
        check(len(held) == 1, "duplicate delete while the first request is held")
        check(urlsplit(held[0].request.url).query == "", "held delete changed the force query")
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as failed:
            held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
        refused = failed.value
    finally:
        page.unroute("**" + path, hold_delete)
    check(refused.status == 502, f"refused delete HTTP {refused.status}")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_role("checkbox", name=delete["force"], exact=True)).not_to_be_checked()
    expect(page.get_by_text(delete["failed"].replace("{{detail}}", reason), exact=True)).to_be_visible()
    stayed = page.request.get(server.base + path)
    body = stayed.json()
    check(stayed.status == 200 and body.get("id") == dep_id and body.get("desired_state") != "deleting",
          "refused delete changed the deployment")
    report["evidence"].append({"layer": "ui", "inflight_deletes": 1, "confirm_loading": True,
        "failure_status": 502, "force_checked": False, "failure_reason_visible": True, "dialog_open": True})
    report["evidence"].append({"layer": "request", "method": "DELETE", "path": path, "query": "",
        "status": 502, "body": {"message": reason}})
    report["evidence"].append({"layer": "persistence", "independent_get": path,
        "desired_state": body.get("desired_state"), "unchanged": True})
    cancel_button(delete["cancel"])(dialog)
    expect(dialog).to_be_hidden()


def prove_force_delete(page, server, name, dep_id, text, report):
    """Later force delete matches an independent 404 and the reloaded empty row."""
    delete = text["delete"]
    page.goto(server.base + "/deployments")
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=name, exact=True).click()
    drawer = page.get_by_role("dialog", name=name, exact=True)
    expect(drawer).to_be_visible()
    drawer.get_by_role("button", name=re.compile(re.escape(delete["button"]) + "$")).click()
    dialog = page.get_by_role("dialog", name=delete["title"].replace("{{name}}", name), exact=True)
    expect(dialog).to_be_visible()
    dialog.get_by_role("checkbox", name=delete["force"], exact=True).check()
    path = "/api/v1/deployments/" + str(dep_id)
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as deleted:
        button(dialog, delete["forceButton"]).click()
    check(deleted.value.status == 200 and urlsplit(deleted.value.url).query == "force=true",
          f"force delete HTTP {deleted.value.status} {deleted.value.url}")
    expect(page.get_by_text(delete["forced"], exact=True).first).to_be_visible()
    gone = page.request.get(server.base + path)
    check(gone.status == 404, f"force-deleted deployment GET HTTP {gone.status}")
    page.reload()
    expect(page.get_by_role("heading", name=text["page"]["title"], exact=True)).to_be_visible()
    expect(page.get_by_role("row").filter(has_text=name)).to_have_count(0)
    report["evidence"].append({"layer": "ui", "force_delete_reloaded_row": 0})
    report["evidence"].append({"layer": "request", "method": "DELETE", "path": path,
        "query": "force=true", "status": 200})
    report["evidence"].append({"layer": "persistence", "independent_get": path, "status": 404})


def make_cleanup_deployment(page, server, token, data):
    """A second deployment, so 12.7's cleanup half does not consume the one the force path needs.

    It shares the model, node and image of the diagnostic fixture and is created over the API
    because it is a fixture rather than something this case is proving. It is deliberately left
    to start for real, because the delete has to be shown to release a live instance rather than
    a row that never had one; the caller waits for that instance before deleting.
    """
    body = {"model_id": data["model"]["id"], "node_uuid": data["node"]["uuid"],
            "name": f"e2e-{data['deployment']['name']}-cleanup",
            "served_name": f"cleanup-{data['deployment']['served_name']}"[:60],
            "engine": data["deployment"].get("engine") or "vllm",
            "image": data["deployment"]["image"],
            "placement_mode": "manual", "health_check": False,
            "container_overrides": {"command_mode": "engine",
                                    "env": dict(data["deployment"].get("env") or {})}}
    status, created = http(server.base, "POST", "/api/v1/deployments", token=token, body=body)
    check(status in (200, 201), f"cleanup fixture deployment HTTP {status}: {created}")
    ident = created.get("id") if isinstance(created, dict) else None
    check(isinstance(ident, int) and ident != data["deployment"]["id"],
          'the cleanup fixture did not get its own deployment id')
    return {"name": body["name"], "id": ident}


def prove_allowed_cleanup(page, server, name, dep_id, text, report):
    """12.7 item 4, the allowed-cleanup half: a normal delete really does remove the deployment.

    The case has already stopped the deployment, which retires its instance rows, so the
    instance side of the impact is captured before the delete rather than read afterwards -
    a deployment that answers 404 can no longer report what it used to run. What the delete
    itself must then establish is: the product answers 202 with the deleting status rather
    than claiming the row is gone, the row is really gone on an independent GET, the reloaded
    list no longer shows it, and the collection's own total drops by exactly one.
    """
    path = "/api/v1/deployments/" + str(dep_id)

    def deploys():
        answer = page.request.get(server.base + "/api/v1/deployments?page=1&page_size=200")
        check(answer.status == 200, f"deployment list HTTP {answer.status}")
        body = answer.json()
        return body.get("total"), [row["id"] for row in body.get("items") or []]

    def instances():
        answer = page.request.get(server.base + "/api/v1/instances")
        check(answer.status == 200, f"instance list HTTP {answer.status}")
        body = answer.json()
        rows = body.get("items") if isinstance(body, dict) else body
        return [row for row in (rows or []) if row.get("deployment_id") == dep_id]

    total_before, ids_before = deploys()
    # The deployment under test is required to have really been running something, otherwise the
    # instance half of "allowed cleanup" would compare zero with zero and establish nothing.
    deadline = time.monotonic() + 120
    instances_before = instances()
    while not instances_before and time.monotonic() < deadline:
        page.wait_for_timeout(500)
        instances_before = instances()
    check(len(instances_before) >= 1,
          'the cleanup fixture never got an instance, so its removal proves nothing about '
          'instance cleanup')
    container_before = instances_before[0].get('container_id')
    check(dep_id in ids_before, 'the deployment under test is not listed before the delete')

    # Open the drawer and take the ordinary (not force) delete.
    page.goto(server.base + "/deployments")
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=name, exact=True).click()
    drawer = page.get_by_role("dialog", name=name, exact=True)
    expect(drawer).to_be_visible()
    delete = text["delete"]
    drawer.get_by_role("button", name=re.compile(re.escape(delete["button"]) + "$")).click()
    dialog = page.get_by_role("dialog", name=delete["title"].replace("{{name}}", name), exact=True)
    expect(dialog).to_be_visible()
    # The force box stays unchecked: this is the normal path, the one the confirmation copy
    # describes.
    expect(dialog.get_by_role("checkbox", name=delete["force"], exact=True)).not_to_be_checked()
    with page.expect_response(lambda r: urlsplit(r.url).path == path
                              and r.request.method == "DELETE") as deleted:
        button(dialog, delete["button"]).click()
    accepted = deleted.value
    check(accepted.status == 202,
          f"normal delete answered {accepted.status}, expected the asynchronous 202")
    check(accepted.json() == {"status": "deleting"}, 'normal delete envelope differs')
    check(urlsplit(accepted.url).query == "", 'normal delete carried a force query')

    # The deployment leaves the readable surface and the list; the total drops by exactly one.
    deadline = time.monotonic() + 90
    status_after = None
    while time.monotonic() < deadline:
        status_after = page.request.get(server.base + path).status
        if status_after == 404:
            break
        page.wait_for_timeout(500)
    check(status_after == 404, f'deleted deployment still answers {status_after}')

    total_after, ids_after = deploys()
    check(dep_id not in ids_after, 'the deleted deployment is still listed')
    check(total_after == total_before - 1,
          f'the deployment count went {total_before} -> {total_after}, expected exactly one fewer')

    instances_after = instances()
    check(instances_after == [],
          f'instances survived the deployment delete: {[(i.get("uuid"), i.get("state")) for i in instances_after]}')

    # The row going away is not the same as the thing it controlled being torn down. The copy
    # promises that a normal delete stops the instances first, so the container this deployment
    # was running is followed to absent.
    from deployment_container_fixture import docker_running
    container_after = 'unknown'
    if container_before:
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            container_after = docker_running(container_before)
            if container_after == 'absent':
                break
            page.wait_for_timeout(500)
        check(container_after == 'absent',
              f'the deleted deployment left its container behind: {container_after}')

    page.reload()
    expect(page.get_by_role("heading", name=text["page"]["title"], exact=True)).to_be_visible()
    expect(page.get_by_role("row").filter(has_text=name)).to_have_count(0)

    report["evidence"].append({'layer': 'request', 'allowed_cleanup': {
        'delete_status': accepted.status,
        'delete_envelope': accepted.json(),
        'detail_after': status_after,
        'row_absent_from_list': dep_id not in ids_after,
        'list_total_before': total_before,
        'list_total_after': total_after,
        'list_total_delta': total_after - total_before,
        'instances_before_delete': len(instances_before),
        'instances_after': len(instances_after),
        'container_id_before': container_before,
        'container_after': container_after,
        'force_unchecked': True,
    }})
    report["evidence"].append({'layer': 'persistence', 'independent_get_after_cleanup': path,
                               'status': status_after})


def exercise_delete_dialog(page, drawer, server, data, text, report):
    """Delete confirm over the detail drawer: identity, layout, focus, and dismissals."""
    name = data["name"]
    delete = text["delete"]
    title = delete["title"].replace("{{name}}", name)
    requests = []

    def seen(request):
        path = urlsplit(request.url).path
        if request.method == "DELETE" and path.startswith("/api/v1/deployments/"):
            requests.append(path)

    page.on("request", seen)
    trigger = drawer.get_by_role("button", name=re.compile(re.escape(delete["button"]) + "$"))
    expect(trigger).to_be_visible()

    def open_delete():
        trigger.click()
        dialog = page.get_by_role("dialog", name=title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = open_delete()
    assert_dialog_shell(page, dialog, dialog.get_by_text(delete["description"], exact=True),
                        button(dialog, delete["button"]))
    dialog = assert_nested_dismissals(
        dialog, drawer, open_delete,
        [("cancel", cancel_button(delete["cancel"])), ("escape", escape),
         ("close", close_button), ("mask", mask)],
        lambda: len(requests), trigger)
    check(requests == [], "delete dialog dismissal sent DELETE")
    report["evidence"].append({"layer": "ui", "delete_dialog": title,
        "dismissals": ["cancel", "escape", "close", "mask"], "delete_requests": 0})
    prove_refused_delete(page, dialog, server, data["deployment"]["id"], delete, report)
    expect(drawer).to_be_visible()


def request_contract(response, path, query, method="GET", body=None):
    request = response.request
    check(request.method == method and urlsplit(request.url).path == path, "browser request route differs")
    check(parse_qs(urlsplit(request.url).query) == {k: [str(v)] for k, v in query.items()},
          "browser query fields differ: " + request.url)
    check(request.post_data_json == body if body is not None else request.post_data is None,
          "browser request body differs")
    check(response.status == 200, f"browser {path}: HTTP {response.status}")


def select_range(page, panel, label, value):
    control = panel.get_by_role("combobox", name=label, exact=True)
    expect(control).to_have_count(1)
    select_control(page, control, value)


def select_control(page, control, value):
    # AntD non-searchable Select overlays its readonly input with the value.
    # Open using the input's real keyboard interface, then select visible text.
    control.focus()
    control.press("ArrowDown")
    expect(control).to_have_attribute("aria-expanded", "true")
    list_id = control.get_attribute("aria-controls")
    check(bool(list_id) and " " not in list_id, "range control must identify its own popup list")
    popup = page.locator(".ant-select-dropdown").filter(has=page.locator("[id=" + json.dumps(list_id) + "]"))
    expect(popup).to_have_count(1)
    option = popup.get_by_text(value, exact=True)
    expect(option).to_be_visible()
    option.click()
    expect(control).to_have_attribute("aria-expanded", "false")


def metric_query(response, instance, range_key):
    query = parse_qs(urlsplit(response.url).query)
    metric = query.get("metric", [None])[0]
    duration, step = RANGES[range_key]
    check(set(query) == {"metric", "instance", "start", "end", "step"}, "metric query fields differ")
    check(query["instance"] == [instance] and query["step"] == [step], "metric identity or step differs")
    check(int(query["end"][0]) - int(query["start"][0]) == duration, "metric window duration differs")
    request_contract(response, "/api/v1/metrics/query", {k: v[0] for k, v in query.items()})
    return metric


def gauges(page, drawer, panel, server, data, report, text):
    inst = data["instance"]
    wire = fixture.metric_wire(inst["api_port"], data["served_name"])
    # Await real collector samples via the authoritative API before exercising UI queries.
    deadline = time.monotonic() + 45
    while True:
        reply = page.request.get(server.base + "/api/v1/metrics/query", params={
            "metric": "gpuworks_infer_requests_running", "instance": inst["uuid"], "range": "15m", "step": "10s"})
        check(reply.status == 200, "known metric query failed")
        if any(s.get("points") for s in reply.json()["items"]):
            fixture.assert_series(reply.json(), inst["uuid"], wire["gpuworks_infer_requests_running"])
            break
        check(time.monotonic() < deadline, "real collector did not deliver engine gauges")
        page.wait_for_timeout(150)
    panel.get_by_role("tab", name=text["instance"]["tab"]["metrics"], exact=True).click()
    expect(panel.locator(".ant-statistic")).to_have_count(5)
    # The drawer has a separate deployment-wide metric panel above instances.
    # It legitimately polls the same instance gauges. Move its own control to
    # the annual window (30-minute cadence), so its default 15m timer cannot
    # be mistaken for an extra request from the instance control under test.
    # A replaced attempt stays on the logs tab and keeps its own range control.
    # The deployment header is the first range control in the drawer.
    panel_control = panel.get_by_role("combobox", name=text["timeRange"]["ariaLabel"], exact=True)
    expect(panel_control).to_have_count(1)
    outer = drawer.get_by_role("combobox", name=text["timeRange"]["ariaLabel"], exact=True).first
    check(outer.get_attribute("id") != panel_control.get_attribute("id"),
          "deployment and instance range controls must remain distinct")
    outer_replies = fixture.capture_gauges(page, inst["uuid"], RANGES["1y"],
        lambda: select_control(page, outer, text["timeRange"]["1y"]))
    check({metric_query(response, inst["uuid"], "1y") for response in outer_replies} == set(fixture.GAUGES),
          "outer deployment range did not complete all gauges")
    report["deployment_metric_range_fixture"] = "1y; separate deployment panel, all three new requests completed"
    checked_ranges = []
    for key in ["1h", "6h", "1d", "1w", "1mo", "1y", "15m"]:
        replies = fixture.capture_gauges(page, inst["uuid"], RANGES[key],
            lambda: select_range(page, panel, text["timeRange"]["ariaLabel"], text["timeRange"][key]))
        report.setdefault("metric_range_requests", []).append({"selection": key,
            "requests": [{"method": r.request.method, "path": urlsplit(r.url).path,
                "query": parse_qs(urlsplit(r.url).query), "status": r.status} for r in replies]})
        check(len(replies) == 3, "range change duplicated a gauge request")
        seen = []
        for response in replies:
            metric = metric_query(response, inst["uuid"], key)
            check(metric not in seen, "range change duplicated a metric")
            seen.append(metric)
            query = parse_qs(urlsplit(response.url).query)
            step = RANGES[key][1]
            step_ms = int(step[:-1]) * {"s": 1000, "m": 60000, "h": 3600000}[step[-1]]
            points = fixture.assert_series(response.json(), inst["uuid"], wire[metric],
                (int(query["start"][0]), int(query["end"][0]), step_ms))
            report.setdefault("metric_range_samples", []).append({"selection": key, "metric": metric, "points": points})
        check(set(seen) == set(fixture.GAUGES), "range change omitted a gauge")
        checked_ranges.append(key)
    # Statistic's current precision=0 formatter truncates its decimal digits.
    for title, value, unit in [("running", "2", ""), ("waitingRequests", "3", ""), ("gpuCache", "37", "%")]:
        card = panel.locator(".ant-statistic").filter(has=page.get_by_text(text["instanceMetrics"][title], exact=True))
        expect(card.locator(".ant-statistic-content-value")).to_have_text(value)
        if unit:
            expect(card.locator(".ant-statistic-content-suffix")).to_have_text(unit)
    unavailable = text["instanceMetrics"]["metricUnavailable"]
    expect(panel.get_by_text("— " + unavailable, exact=True)).to_have_count(2)
    report["evidence"].append({"layer": "request", "metric_ranges": checked_ranges,
        "gauge_wire": wire, "units": {"cache": "fraction 0.375 -> 37.5 percent -> Statistic precision=0 truncates to 37%"}})
    report["evidence"].append({"layer": "execution", "engine_metric_wire": wire,
        "absent_metrics": ["prefix cache ratio", "speculative acceptance ratio"]})
    report["evidence"].append({"layer": "persistence", "engine_metric_samples": report["metric_range_samples"]})

    # 9.3: choosing the Custom entry must not re-query on its own. The product keeps the
    # current window until the revealed picker is confirmed, and cancelling leaves that
    # window untouched. Only confirming a pair emits a new custom window.
    custom_label = text["timeRange"]["custom"]
    before_custom = [item for item in report.get("metric_range_requests", [])]
    control = panel.get_by_role("combobox", name=text["timeRange"]["ariaLabel"], exact=True)
    pending = []

    def watch_custom(request):
        if "/metrics" in urlsplit(request.url).path:
            pending.append(request)

    page.on("request", watch_custom)
    select_control(page, control, custom_label)
    page.wait_for_timeout(600)
    check(not pending, "choosing Custom issued a metric query before the range was confirmed")
    # The select keeps showing the active window while the custom pair is still a draft;
    # what appears is the range picker the Custom entry reveals.
    picker = panel.locator(".ant-picker-range")
    expect(picker).to_have_count(1)
    expect(picker).to_be_visible()
    # Dismiss the revealed picker by choosing a quick window again: no custom pair was ever
    # confirmed, so the draft must be abandoned rather than saved. The quick choice is the
    # only request this stage is allowed to produce.
    pending.clear()
    replies = fixture.capture_gauges(page, inst["uuid"], RANGES["15m"],
        lambda: select_range(page, panel, text["timeRange"]["ariaLabel"], text["timeRange"]["15m"]))
    check(len(replies) == 3, "returning from an abandoned custom draft did not restore a quick window")
    report["metric_custom_range"] = {"unconfirmed_requests": 0, "cancel_requests": 0}
    page.remove_listener("request", watch_custom)
    report["evidence"].append({"layer": "request", "custom_range": report["metric_custom_range"],
        "ranges_before": [item["selection"] for item in before_custom]})

    # 9.7: the window has one hard rule the server enforces rather than merely hinting at -
    # start must be strictly before end. An inverted window is refused with a typed 400 and
    # no samples, while the same length the right way round is served. The retention side is
    # deliberately not claimed: this picker carries no disabledDate or retention hint, so the
    # only illegal window the product defines here is the inverted one.
    moment = int(time.time() * 1000)
    inverted = page.request.get(server.base + "/api/v1/metrics/query", params={
        "metric": "gpuworks_infer_requests_running", "instance": inst["uuid"],
        "start": moment, "end": moment - 600000, "step": "10s"})
    check(inverted.status == 400, f"an inverted metric window was not refused: {inverted.status}")
    inverted_body = inverted.json()
    check(inverted_body.get("code") == "QUERY_WINDOW_INVALID"
          and inverted_body.get("error") == "start must be before end",
          f"inverted metric window refusal differs: {inverted_body}")
    forward = page.request.get(server.base + "/api/v1/metrics/query", params={
        "metric": "gpuworks_infer_requests_running", "instance": inst["uuid"],
        "start": moment - 600000, "end": moment, "step": "10s"})
    check(forward.status == 200, f"the same window the right way round was refused: {forward.status}")
    report["metric_window_rule"] = {"inverted_status": inverted.status,
        "inverted_code": inverted_body.get("code"), "inverted_error": inverted_body.get("error"),
        "forward_status": forward.status, "retention_hint_claim": False}
    report["evidence"].append({"layer": "request", "metric_window_rule": report["metric_window_rule"]})


def launch_facts(page, panel, server, data, report, text):
    inst, container = data["instance"], data["container"]
    path = "/api/v1/instances/" + inst["uuid"] + "/launch-facts"
    deadline = time.monotonic() + 30
    while True:
        response = page.request.get(server.base + path)
        check(response.status == 200, "launch facts read failed")
        if response.json()["runtime_observation_relation"] == {"status": "current"}:
            break
        check(time.monotonic() < deadline, "runtime observation never became current")
        page.wait_for_timeout(100)
    labels = text["instance"]["launchFacts"]
    trigger = panel.get_by_role("button").filter(has=page.get_by_text(labels["title"], exact=True))
    with page.expect_response(lambda r: urlsplit(r.url).path == path) as loaded:
        trigger.click()
    request_contract(loaded.value, path, {})
    actual = fixture.inspect(container)
    image = json.loads(fixture.docker("image", "inspect", actual["Image"]))[0]
    expected, expected_image = fixture.assert_launch(loaded.value.json(), actual, image)
    facts = panel.locator(".ant-collapse-item").filter(has=page.get_by_role("button").filter(has=page.get_by_text(labels["title"], exact=True))).first
    for layer, values in [("container", expected), ("image", expected_image)]:
        facts.get_by_role("button").filter(has=page.get_by_text(labels["layer"][layer], exact=True)).click()
        block = facts.locator(".ant-collapse-item").filter(has_text=labels["layer"][layer]).last
        fields = {"container_id": "containerId", "image_id": "imageId", "entrypoint": "entrypoint",
                  "cmd": "arguments", "env": "containerConfigEnv" if layer == "container" else "imageConfigEnv",
                  "mounts": "mounts", "devices": "devices", "volumes": "volumes"}
        for field, label in fields.items():
            if field not in values:
                continue
            cell = block.locator(".ant-descriptions-row").filter(has=page.get_by_text(labels["field"][label], exact=True)).locator(".ant-descriptions-item-content")
            value = values[field]
            if (isinstance(value, dict) or isinstance(value, list) and value) and field not in {"entrypoint", "cmd"}:
                rendered = cell.inner_text()
                fixture.strict_json(json.loads(rendered), value, "rendered launch " + field)
            else:
                rendered = shlex.join(value) if isinstance(value, list) and value else value if value else "—"
                expect(cell).to_have_text(rendered)
    facts.get_by_role("button").filter(has=page.get_by_text(labels["layer"]["process"], exact=True)).click()
    expect(facts.get_by_text(labels["reason"]["not_collected"], exact=True)).to_have_count(2)
    report["evidence"].append({"layer": "execution", "launch_facts": {"container": expected,
        "image": expected_image, "process": {"status": "unknown", "reason": "not_collected"}}})
    report["evidence"].append({"layer": "persistence", "launch_observation_relation": "current"})


def seed_stdout(container, prefix, count):
    """Write distinct real stdout lines into the owned engine's PID 1 stream."""
    lines = ["INFO " + prefix + "-%02d 中文诊断" % index for index in range(count)]
    # Positional arguments are iterated rather than referenced one by one: a bare $10 is $1
    # followed by a literal 0 in a POSIX shell, so an unbraced chain would silently overwrite
    # the same line for every argument from the tenth on.
    fixture.docker("exec", container, "/bin/sh", "-c",
                  'for line in "$@"; do printf "%s\\n" "$line" > /proc/1/fd/1; done', "sh", *lines)
    deadline = time.monotonic() + 20
    while True:
        recorded = fixture.docker("logs", container).decode().splitlines()
        counts = [recorded.count(line) for line in lines]
        if all(count == 1 for count in counts):
            return lines
        check(time.monotonic() < deadline,
              "owned engine stdout did not receive the seeded lines: %s" % counts)
        time.sleep(0.2)


def scroll_state(panel):
    """Measured scroll position of the log viewport, in CSS pixels."""
    return panel.locator("pre").evaluate(
        "el => ({top: el.scrollTop, height: el.scrollHeight, client: el.clientHeight})")


def log_follow_and_modes(page, panel, server, data, report, text):
    """18.2: live/query switching, follow pause and resume, and the return to the default view.

    The instance log surface exposes exactly two controls: a range select whose first entry is
    the live default, and a follow checkbox that is checked by default. Follow is asserted the
    way the product implements it - it pins the viewport to the newest line on every arrival and
    never gates the stream - so pausing is proven through the checkbox: an unchecked follow must
    keep the reader's scroll position while new stdout lines still arrive and still persist.
    """
    inst, container = data["instance"], data["container"]
    pre = panel.locator("pre")
    expect(pre).to_have_count(1)
    hint = panel.get_by_text(text["instance"]["logsHistoryHint"], exact=True)
    control = panel.locator(".ant-tabs-extra-content")
    range_label = text["timeRange"]["ariaLabel"]
    follow = control.get_by_role("checkbox", name=text["instance"]["followLogs"], exact=True)
    shown = control.locator(".ant-select-selection-item")

    report["step"] = "log view control surface before any mode change"
    expect(control).to_have_count(1)
    expect(control.get_by_role("checkbox")).to_have_count(1)
    expect(control.get_by_role("combobox")).to_have_count(1)
    expect(follow).to_be_checked()
    expect(shown).to_have_count(1)
    expect(shown).to_have_text(text["timeRange"]["live"])
    expect(hint).to_have_count(0)

    report["step"] = "default follow pins a live tail to the newest stdout line"
    seeded = seed_stdout(container, "e2e-" + report["run_id"] + "-follow", 60)
    expect(pre).to_contain_text(seeded[-1], timeout=45000)
    page.wait_for_timeout(400)
    pinned = scroll_state(panel)
    check(pinned["height"] > pinned["client"] + 100,
          "seeded live stdout does not overflow the log viewport: %s" % pinned)
    check(pinned["top"] + pinned["client"] >= pinned["height"] - 2,
          "the default follow control left the live view off the newest line: %s" % pinned)

    report["step"] = "an enabled follow returns the view to the newest line"
    pre.hover()
    page.mouse.wheel(0, -100000)
    page.wait_for_timeout(300)
    away = scroll_state(panel)
    check(away["top"] < pinned["top"] - 1, "the live view could not be scrolled off the newest line")
    tracked = fixture.write_marker(container, "e2e-" + report["run_id"] + "-follow-on")
    expect(pre).to_contain_text(tracked, timeout=45000)
    page.wait_for_timeout(400)
    repinned = scroll_state(panel)
    check(repinned["top"] + repinned["client"] >= repinned["height"] - 2,
          "an enabled follow control did not return the view to the newest line: %s" % repinned)

    report["step"] = "follow unchecked keeps the reader's position while stdout keeps arriving"
    check(control.get_by_role("button", name=re.compile("pause|暂停", re.IGNORECASE)).count() == 0,
          "the log view control surface unexpectedly gained a stream pause button")
    follow.uncheck()
    expect(follow).not_to_be_checked()
    pre.hover()
    page.mouse.wheel(0, -100000)
    page.wait_for_timeout(300)
    held_at = scroll_state(panel)
    check(held_at["top"] < repinned["top"] - 1, "the view did not move when follow was unchecked")
    lower = int(time.time() * 1000)
    paused = fixture.write_marker(container, "e2e-" + report["run_id"] + "-follow-off")
    expect(pre).to_contain_text(paused, timeout=45000)
    page.wait_for_timeout(800)
    stayed = scroll_state(panel)
    check(abs(stayed["top"] - held_at["top"]) < 1,
          "an unchecked follow still moved the reader's position: %s -> %s" % (held_at, stayed))
    check(stayed["height"] > held_at["height"] + 2,
          "the paused log view never received the new stdout line: %s -> %s" % (held_at, stayed))
    paused_hit = fixture.persisted_marker(page, server, inst["uuid"], data["node"]["uuid"], paused, lower)

    report["step"] = "follow re-enabled snaps back to the newest line"
    follow.check()
    expect(follow).to_be_checked()
    page.wait_for_timeout(400)
    resumed = scroll_state(panel)
    check(resumed["top"] > stayed["top"] + 1, "re-enabling follow did not move the paused view")
    check(resumed["top"] + resumed["client"] >= resumed["height"] - 2,
          "re-enabling follow did not snap the view to the newest line: %s" % resumed)
    expect(pre).to_contain_text(paused, timeout=5000)

    query_path = "/api/v1/logs/search"
    stream_path = "/api/v1/instances/" + inst["uuid"] + "/logs"
    held, searches = [], []

    def hold_one_hour(route):
        query = parse_qs(urlsplit(route.request.url).query)
        if not held and query.get("from") and query.get("to") and \
                int(query["to"][0]) - int(query["from"][0]) == 3600000:
            held.append(route)
            return
        route.continue_()

    def watch_search(request):
        if urlsplit(request.url).path == query_path:
            searches.append(request.url)

    pattern = re.compile(re.escape(server.base + query_path) + r"(\?|$)")
    page.route(pattern, hold_one_hour)
    try:
        report["step"] = "live mode replaced by a real one-hour query"
        select_range(page, panel, range_label, text["timeRange"]["1h"])
        deadline = time.monotonic() + 20
        while not held:
            check(time.monotonic() < deadline, "the one-hour window issued no log search")
            page.wait_for_timeout(100)
        older_query = parse_qs(urlsplit(held[0].request.url).query)
        check(held[0].request.method == "GET", "the historical log query is not a GET")
        check(set(older_query) == {"instance", "from", "to", "limit"},
              "historical log query fields differ: " + str(sorted(older_query)))
        check(older_query["instance"] == [inst["uuid"]] and older_query["limit"] == ["2000"]
              and int(older_query["to"][0]) - int(older_query["from"][0]) == 3600000,
              "historical log query window differs: " + str(older_query))
        expect(shown).to_have_text(text["timeRange"]["1h"])
        expect(control.get_by_role("checkbox", name=text["instance"]["followLogs"], exact=True)) \
            .to_have_count(1)
        # Independent snapshot of the very same window while the browser's copy is still held,
        # so the held response is a genuinely older view of the store than the one that follows.
        withheld = page.request.get(server.base + query_path + "?" + urlsplit(held[0].request.url).query)
        check(withheld.status == 200, "independent one-hour log read HTTP %s" % withheld.status)
        older = withheld.json()
        check(isinstance(older.get("items"), list) and bool(older["items"]),
              "independent one-hour log read returned no rows")

        report["step"] = "a newer stdout line appears while the one-hour response is still held"
        lower = int(time.time() * 1000)
        probe = fixture.write_marker(container, "e2e-" + report["run_id"] + "-stale-probe")
        probe_hit = fixture.persisted_marker(page, server, inst["uuid"], data["node"]["uuid"], probe, lower)
        check(all(row["line"] != probe for row in older["items"]),
              "the independently read older window already holds the newer probe line")

        report["step"] = "the still-held one-hour window is replaced by the six-hour window"

        def six_hours(response):
            query = parse_qs(urlsplit(response.url).query)
            if urlsplit(response.url).path != query_path or query.get("instance") != [inst["uuid"]]:
                return False
            return bool(query.get("from") and query.get("to")) \
                and int(query["to"][0]) - int(query["from"][0]) == 21600000

        with page.expect_response(six_hours) as newer:
            select_range(page, panel, range_label, text["timeRange"]["6h"])
        newer_query = parse_qs(urlsplit(newer.value.url).query)
        check(set(newer_query) == {"instance", "from", "to", "limit"},
              "six-hour log query fields differ: " + str(sorted(newer_query)))
        check(newer_query["instance"] == [inst["uuid"]] and newer_query["limit"] == ["2000"]
              and int(newer_query["to"][0]) - int(newer_query["from"][0]) == 21600000,
              "six-hour log query window differs: " + str(newer_query))
        check(int(newer_query["from"][0]) < int(older_query["from"][0]),
              "the six-hour window does not start before the one-hour window")
        request_contract(newer.value, query_path, {k: v[0] for k, v in newer_query.items()})
        expect(shown).to_have_text(text["timeRange"]["6h"])
        expect(hint).to_have_count(1)
        expect(pre).to_contain_text(probe, timeout=20000)
        # The persisted copy carries its server arrival time; the live copy does not, so
        # the prefix is asserted against the very line under test rather than the head of
        # the view, which may legitimately open with a system line.
        stamped = re.compile(r"\[[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\] " + re.escape(probe))
        check(stamped.search(pre.inner_text()) is not None,
              "the historical view does not prefix the line with its arrival time")

        report["step"] = "the held one-hour response cannot overwrite the six-hour view"
        before_release = pre.inner_text()
        held[0].fulfill(status=200, content_type="application/json", body=json.dumps(older))
        page.wait_for_timeout(2000)
        after_release = pre.inner_text()
        check(probe in after_release, "the stale one-hour response overwrote the six-hour view")
        expect(shown).to_have_text(text["timeRange"]["6h"])
        view_changed_after_release = after_release != before_release

        report["step"] = "the historical view does not consume the live stream"
        lower = int(time.time() * 1000)
        during = fixture.write_marker(container, "e2e-" + report["run_id"] + "-query-mode")
        page.wait_for_timeout(5000)
        check(during not in pre.inner_text(), "the historical view consumed a live stream line")
        during_hit = fixture.persisted_marker(page, server, inst["uuid"], data["node"]["uuid"], during, lower)

        report["step"] = "return to the default live view"
        page.on("request", watch_search)
        try:
            with page.expect_response(lambda r: urlsplit(r.url).path == stream_path) as reopened:
                select_control(page, control.get_by_role("combobox", name=range_label, exact=True),
                               text["timeRange"]["live"])
            check(reopened.value.headers.get("content-type") == "text/event-stream",
                  "returning to the default view did not reopen the live stream")
            check(parse_qs(urlsplit(reopened.value.url).query) == {},
                  "the reopened live stream carries a query: " + reopened.value.url)
            page.wait_for_timeout(1000)
            check(not searches, "returning to the default view issued a log search: " + str(searches))
        finally:
            page.remove_listener("request", watch_search)
        expect(shown).to_have_text(text["timeRange"]["live"])
        expect(hint).to_have_count(0)
        check(probe in pre.inner_text(), "returning to the default view dropped the rows it showed")
        lower = int(time.time() * 1000)
        back = fixture.write_marker(container, "e2e-" + report["run_id"] + "-live-again")
        expect(pre).to_contain_text(back, timeout=45000)
        back_hit = fixture.persisted_marker(page, server, inst["uuid"], data["node"]["uuid"], back, lower)
    finally:
        page.unroute(pattern, hold_one_hour)

    report["log_follow_and_modes"] = {
        "follow_default": True, "pinned_top": pinned["top"], "paused_top": stayed["top"],
        "resumed_top": resumed["top"], "view_text_changed_after_release": view_changed_after_release,
        "live_stream_reopened": True, "log_searches_when_returning_to_live": len(searches)}
    report["evidence"].append({"layer": "ui", "log_follow": {
        "default_checked": True, "pinned_to_newest": True, "kept_tracking": tracked,
        "paused_at_px": stayed["top"], "received_while_unchecked": paused,
        "resumed_at_px": resumed["top"], "stream_pause_button": False}})
    report["evidence"].append({"layer": "ui", "log_modes": {
        "live": "default streaming tail, no history hint",
        "one_hour": "persisted rows of the real one-hour window",
        "six_hours": "replaced the still-held one-hour response",
        "back_to_live": "same rows kept and the stream resumed",
        "stale_one_hour_overwrote_view": False}})
    report["evidence"].append({"layer": "request", "log_modes": {
        "one_hour": {k: v[0] for k, v in older_query.items()},
        "six_hours": {k: v[0] for k, v in newer_query.items()},
        "return_to_live": {"method": "GET", "path": stream_path, "query": {},
                           "content_type": "text/event-stream", "log_searches": 0}}})
    report["evidence"].append({"layer": "persistence", "log_modes": {
        "unchecked_follow_marker": paused_hit, "stale_probe": probe_hit,
        "query_mode_marker": during_hit, "live_again_marker": back_hit}})
    report["last_step"] = "log-follow-and-view-modes"


# Mainline: real container stdout marker → browser live SSE → known engine
# gauges / all metric quick ranges → independent Docker launch observations
# → browser Stop → persisted historical marker after reload; no real GPU claim.
def test_instance_diagnostics(page, context, server, artifacts, report, locale, data, token, holder):
    text = strings(locale)
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    login(page, context, server, artifacts, report, locale)
    drawer, panel = open_instance(page, server, data, report)
    # 18.5: a refused live stream must say so and offer a way back, and the stream must be released
    # when the drawer closes. The stream is answered with a 403 on a fresh open, and the panel is
    # required to show the refusal with a retry control rather than sit on "waiting for logs", with
    # no unhandled page error. The route is then released and the retry is required to take the
    # message away and bring the panel back to its normal state.
    stream_path = "/api/v1/instances/" + data["instance"]["uuid"] + "/logs"
    refused = []

    def refuse_stream(route):
        refused.append(route.request.url)
        route.fulfill(status=403, content_type="application/json",
                      body='{"code":"FORBIDDEN","error":"injected stream refusal"}')

    drawer.locator(".ant-drawer-close").click()
    expect(page.get_by_role("dialog", name=data["name"], exact=True)).to_have_count(0)
    report["last_step"] = "stream-drawer-closed"
    page.route("**" + stream_path, refuse_stream)
    try:
        # Opened without the 200 stream contract this helper normally asserts: the point of this
        # step is a refused stream, so only the drawer and its panel are located.
        row = page.get_by_role("row").filter(has_text=data["name"])
        expect(row).to_have_count(1)
        row.get_by_role("button", name=data["name"], exact=True).click()
        drawer = page.get_by_role("dialog", name=data["name"], exact=True)
        expect(drawer).to_be_in_viewport(ratio=1)
        panel = drawer.locator('[id="instance-' + data["instance"]["uuid"] + '"]')
        expect(panel).to_be_visible()
        expect(panel).to_contain_text(text["instance"]["log"]["streamFailed"], timeout=12000)
        expect(panel.get_by_role("button", name=text["instance"]["log"]["retry"], exact=True)).to_be_visible()
        check(len(refused) > 0, "the injected stream refusal was never requested")
        report["last_step"] = "refused-stream-reported"
    finally:
        page.unroute("**" + stream_path, refuse_stream)
    panel.get_by_role("button", name=text["instance"]["log"]["retry"], exact=True).click()
    expect(panel).not_to_contain_text(text["instance"]["log"]["streamFailed"], timeout=12000)
    expect(panel.get_by_role("button", name=text["instance"]["log"]["retry"], exact=True)).to_have_count(0)
    report["stream_refusal"] = {"reported": True, "retry_visible": True, "recovered": True,
                                "injected": len(refused)}
    report["last_step"] = "refused-stream-recovered"
    drawer.locator(".ant-drawer-close").click()
    expect(page.get_by_role("dialog", name=data["name"], exact=True)).to_have_count(0)
    drawer, panel = open_instance(page, server, data, report)

    report["step"] = "delete dialog over the detail drawer"
    exercise_delete_dialog(page, drawer, server, data, text, report)
    inst = data["instance"]
    log_follow_and_modes(page, panel, server, data, report, text)
    report["step"] = "real stdout marker through browser SSE"
    lower = int(time.time() * 1000)
    line = fixture.write_marker(data["container"], "e2e-" + report["run_id"] + "-diagnostics")
    expect(panel.locator("pre")).to_contain_text(line)
    hit = fixture.persisted_marker(page, server, inst["uuid"], data["node"]["uuid"], line, lower)
    report["last_step"] = "live-and-persisted-marker"
    report["step"] = "engine metric wire and browser ranges"
    gauges(page, drawer, panel, server, data, report, text)
    report["step"] = "Docker facts and exact rendering"
    launch_facts(page, panel, server, data, report, text)
    report["step"] = "persisted log range then stop"
    panel.get_by_role("tab", name=text["instance"]["tab"]["logs"], exact=True).click()
    log_path = "/api/v1/logs/search"
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path
                              and parse_qs(urlsplit(r.url).query).get("instance") == [inst["uuid"]]) as searched:
        select_range(page, panel, text["timeRange"]["ariaLabel"], text["timeRange"]["1h"])
    query = parse_qs(urlsplit(searched.value.url).query)
    check(set(query) == {"instance", "from", "to", "limit"}, "log range query fields differ")
    check(query["instance"] == [inst["uuid"]] and query["limit"] == ["2000"]
          and int(query["to"][0]) - int(query["from"][0]) == 3600000, "log range query differs")
    request_contract(searched.value, log_path, {k: v[0] for k, v in query.items()})
    check([x for x in searched.value.json()["items"] if x["line"] == line] == [hit], "browser log range lost or changed known marker")
    expect(panel.locator("pre")).to_contain_text(line)
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/deployments/" + str(data["deployment"]["id"]) + "/stop") as stopped:
        button(drawer, text["detail"]["stop"]).click()
        popup = page.get_by_role("tooltip").filter(has_text=text["detail"]["stopConfirm"])
        button(popup, text["detail"]["stop"]).click()
    request_contract(stopped.value, "/api/v1/deployments/" + str(data["deployment"]["id"]) + "/stop", {}, "POST")
    deadline = time.monotonic() + 60
    from deployment_container_fixture import docker_running
    while docker_running(data["container"]) != "absent":
        check(time.monotonic() < deadline, "stopped diagnostic container remains")
        page.wait_for_timeout(150)
    # Stop intentionally retires the instance row, while logstore preserves its
    # history. Use the log center's real row-instance filter; never manufacture
    # a hidden instance panel or seed a resource UUID into a URL/input.
    report["step"] = "retired instance history in log center after reload"
    lower_search = int(time.time() * 1000) - 86400000
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path) as historical:
        page.goto(server.base + "/logs")
    upper_search = int(time.time() * 1000) - 86400000
    history_query = parse_qs(urlsplit(historical.value.url).query)
    check(set(history_query) == {"source", "from", "limit", "with_total"}
          and lower_search <= int(history_query["from"][0]) <= upper_search,
          "log center default 24h window differs")
    history_params = {"source": "instance", "from": history_query["from"][0], "limit": 500, "with_total": 1}
    request_contract(historical.value, log_path, history_params)
    check([x for x in historical.value.json()["items"] if x["line"] == line] == [hit],
          "stopped log center did not read the persisted marker")
    row = page.get_by_test_id("log-row").filter(has_text=line)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(line)
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path
                              and parse_qs(urlsplit(r.url).query).get("instance") == [inst["uuid"]]) as filtered:
        row.get_by_title(inst["uuid"], exact=True).click()
    request_contract(filtered.value, log_path, {**history_params, "instance": inst["uuid"]})
    check([x for x in filtered.value.json()["items"] if x["line"] == line] == [hit],
          "retired instance filter changed the persisted marker")
    expect(page.locator(".ant-select-auto-complete").get_by_role("combobox")).to_have_value(inst["uuid"])
    # Filters are intentionally unsaved; reloading resets to default 24h/all
    # instances. The exact stored row must still come from a fresh browser GET.
    lower_search = int(time.time() * 1000) - 86400000
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path) as recent:
        page.reload()
    reloaded = parse_qs(urlsplit(recent.value.url).query)
    check(set(reloaded) == {"source", "from", "limit", "with_total"}
          and lower_search <= int(reloaded["from"][0]) <= int(time.time() * 1000) - 86400000,
          "reloaded log center default window differs")
    request_contract(recent.value, log_path, {**history_params, "from": reloaded["from"][0]})
    check([x for x in recent.value.json()["items"] if x["line"] == line] == [hit],
          "stopped log center reload lost or changed the stored marker")
    expect(page.get_by_test_id("log-row").filter(has_text=line)).to_have_count(1)
    # 12.7 item 4 needs a deployment that is still there, and a normal delete removes it, so this
    # runs before the force path below. The deployment it removes is a second one made for this
    # purpose; the force delete that follows keeps its own evidence intact.
    report["step"] = "allowed cleanup after the diagnostic"
    cleanup_dep = make_cleanup_deployment(page, server, token, data)
    prove_allowed_cleanup(page, server, cleanup_dep["name"], cleanup_dep["id"], text, report)
    report["step"] = "force delete after the diagnostic"
    prove_force_delete(page, server, data["name"], data["deployment"]["id"], text, report)
    check(errors == [], "diagnostics page JS errors: " + str(errors))
    report["evidence"].extend([
        {"layer": "ui", "stdout_marker": line, "metric_card_values": [2, 3, "37%"],
         "unknown_ratio_metrics": True, "retired_instance_history": "log center exact stored row"},
        {"layer": "request", "logs_search": {k: v[0] for k, v in query.items()}, "stop_status": 200,
         "history_filter": {**history_params, "instance": inst["uuid"]}, "history_reload": reloaded},
        {"layer": "persistence", "log_row": hit, "after_stop_reload": "same marker"},
        {"layer": "execution", "container_stopped": data["container"], "observed": "absent"}])
    report["last_step"] = "historical-logs-after-stop-and-reload"
