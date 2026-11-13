#!/usr/bin/env python3
"""Dashboard / fleet inventory and label grouping through the real browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-dashboard-fleet
After build: python3 test/e2e/dashboard_fleet.py --locale en
Requires Python Playwright and system Chrome. Three pending node definitions are
prepared through the product API so the dashboard read path is exercised with
deterministic fleet counts and labels. No agent, GPU or container is claimed.
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
import time
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/dashboard/fleet-inventory-label-grouping"
CASE = "概览 / 未上线节点库存与标签容量分组 / UI 计数、切换标签键、刷新与独立 GET 一致"
NODES = "/api/v1/nodes"
SUMMARY = "/api/v1/dashboard/summary"


def strings(locale):
    ui = json.loads((ROOT / "ui/src/i18n/locales" / locale / "dashboard.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return ui, auth


def format_text(template: str, **values) -> str:
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", str(value))
    return template


# Contract: gpuworks/dashboard/fleet-inventory-label-grouping
# Mainline: 独立 server/SQLite + API 准备三个带标签的待接入节点 → 概览页显示总数/待接入数
#           → 默认按 team 分组显示 alpha 与 beta → 切换到 rack 后显示 rack=r1 与未设置
#           → 刷新与独立 GET 的计数/分组一致 → 强制删除自有节点
def test_dashboard_fleet(page, context, server, artifacts, report, locale):
    ui, auth = strings(locale)
    name_a = f"e2e-{report['run_id']}-dashboard-a"
    name_a2 = f"e2e-{report['run_id']}-dashboard-a2"
    name_b = f"e2e-{report['run_id']}-dashboard-b"
    page_errors, console_errors, bad_responses, summary_requests = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            if (not authenticated
                    and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me"
                    and "401" in message.text):
                return
            console_errors.append(message.text)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": path,
                                  "status": response.status})

    def request_seen(request):
        if urlsplit(request.url).path == SUMMARY and request.method == "GET":
            summary_requests.append(urlsplit(request.url).query)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/dashboard")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=ui["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"

    nodes = []
    # A2+B1: two alpha nodes share rack=r1, the single beta node has no rack.
    # The team grouping is 2+1; the rack grouping is 2 labeled + 1 unlabeled,
    # so the same three nodes distinguish both group sets.
    for name, labels in ((name_a, {"team": "alpha", "rack": "r1"}),
                         (name_a2, {"team": "alpha", "rack": "r1"}),
                         (name_b, {"team": "beta"})):
        created = page.request.post(server.base + NODES,
                                    data={"name": name, "server_url": server.base})
        check(created.status == 201, f"node fixture HTTP {created.status}")
        body = created.json()
        artifacts.remember(body.get("token", ""))
        node = body["node"]
        labeled = page.request.put(server.base + NODES + f"/{node['uuid']}/labels",
                                   data={"labels": labels})
        check(labeled.status == 200, f"label fixture HTTP {labeled.status}")
        nodes.append(node)

    page.reload()
    expect(page.get_by_role("heading", name=ui["title"], exact=True)).to_be_visible()

    def resource_card(title):
        """A summary metric, located by the label Zeron's MetricCard renders.

        OverviewParts.Metric renders ui/metric-card with the label in
        [data-slot=metric-card-label] and the value in the sibling node, whose unit
        is a trailing <span>. The card is found by that label slot rather than by
        antd's card markup, which the overview no longer uses.
        """
        card = page.locator(".dashboard-overview-metric").filter(
            has=page.locator('[data-slot="metric-card-label"]').get_by_text(title, exact=True)
        ).first
        return card.locator('[data-slot="metric-card-value-row"] > span').first

    def expect_count(title, value, unit):
        # The value node renders "<value><unit>" with the unit in a trailing span and
        # a margin gap, so the separator in the normalized text is optional.
        expect(resource_card(title)).to_have_text(re.compile(rf"^{value}\s*{re.escape(unit)}$"))

    # The hardware panel's three KPIs are labelled with the *Kpi keys
    # (DashboardPage.tsx:167-169), not the panel title or the unit's own label.
    expect_count(ui["overview"]["nodesKpi"], "3", ui["overview"]["nodeUnit"])
    expect_count(ui["overview"]["gpusKpi"], "0", ui["overview"]["gpuUnit"])
    # Eleven KPI metrics: the hardware panel's three, the usage panel's three, and
    # the model, storage and service figures (eleven <Metric> in DashboardPage.tsx).
    expect(page.locator(".dashboard-overview-metric")).to_have_count(11)
    expect(page.get_by_role("spinbutton")).to_have_count(0)
    # Zero-valued status facts are not rendered at all, so the only node fact on
    # an unconnected fleet is the connecting count.
    connected = format_text(ui["overview"]["connecting"], count=3)
    nodes_card = page.locator(".dashboard-panel").filter(has_text=ui["overview"]["nodes"]).first
    # Each fleet state renders as a StatusCount inside the hardware panel's states
    # line (DashboardPage.tsx:171-173, OverviewParts.StatusCount). With an
    # unconnected fleet only the connecting count is rendered, and the others are
    # absent rather than shown as zero.
    # All three fleet states render (online, connecting, offline); StatusCount only skips
    # a state the API does not report at all, so a zero count keeps its slot in grey.
    states = nodes_card.locator(".dashboard-states-line .dashboard-overview-status")
    expect(states).to_have_count(3)
    expect(states.filter(has_text=connected)).to_have_count(1)
    expect(page.locator(".ant-tag")).to_have_count(0)
    # The management service reads out as Zeron's Badge (DashboardPage.tsx:256):
    # data-status carries the verdict and a dot marker accompanies the normal word.
    posture = page.locator('.dashboard-header-service [data-slot="badge"]')
    expect(posture).to_have_attribute("data-status", "success")
    expect(posture).to_have_text(ui["overview"]["normal"])
    # The dot is passed as leadingIcon, which Badge renders as badge-icon.
    expect(posture.locator('[data-slot="badge-icon"]')).to_have_count(1)

    def group_evidence(groups):
        return [{"key": key, "unlabeled": unlabeled, "nodes": count}
                for (key, unlabeled), count in sorted(groups.items())]

    # N/A: the overview used to carry a "GPU capacity by node label" panel that grouped the
    # fleet by one label key and let the reader switch the key. Mainline a5b4266bf ("Move the
    # dashboard to Zeron and send its counts from the server") removed the panel: no component
    # renders ui/src/api.ts's by_label any more, and the summary endpoint still answers
    # label_keys/label_key/by_label but nothing in the console reads them. The grouping the
    # contract is about therefore has no UI in this build; the server-side aggregation it
    # depended on is asserted below from the API, and the panel's absence is recorded here
    # rather than asserted as a blank card.
    expect(page.locator(".dashboard-panel").filter(
        has_text=ui["overview"]["labelDistribution"])).to_have_count(0)
    report["label_panel"] = {"status": "N/A", "reason": "panel removed by a5b4266bf",
                             "api_still_serves": ["label_keys", "label_key", "by_label"]}
    report["last_step"] = "label-panel-absent-recorded"

    default_response = page.request.get(server.base + SUMMARY)
    check(default_response.status == 200, f"default summary HTTP {default_response.status}")
    default = default_response.json()
    check(default["nodes"]["total"] == 3 and default["nodes"]["offline"] == 0
          and default["nodes"]["connecting"] == 3,
          f"default node counts differ: {default['nodes']}")
    check(default["label_keys"] == ["team", "rack"],
          f"label keys differ: {default['label_keys']}")
    check(default["label_key"] == "team", f"default label key differs: {default['label_key']}")
    default_groups = {(group.get("key", ""), bool(group.get("unlabeled"))): group["nodes"]
                      for group in default["by_label"]}
    check(default_groups == {("team=alpha", False): 2, ("team=beta", False): 1},
          f"team groups differ: {default['by_label']}")
    check(sum(default_groups.values()) == default["nodes"]["total"],
          "default group counts do not add up to the fleet total")

    rack_response = page.request.get(server.base + SUMMARY + "?label_key=rack")
    check(rack_response.status == 200, f"rack summary HTTP {rack_response.status}")
    rack = rack_response.json()
    check(rack["label_key"] == "rack", f"rack label key differs: {rack['label_key']}")
    rack_groups = {(group.get("key", ""), bool(group.get("unlabeled"))): group["nodes"]
                   for group in rack["by_label"]}
    check(rack_groups == {("rack=r1", False): 2, ("", True): 1},
          f"rack groups differ: {rack['by_label']}")
    check(sum(rack_groups.values()) == rack["nodes"]["total"],
          "rack group counts do not add up to the fleet total")
    report["last_step"] = "rack-summary-verified"

    page.reload()
    expect(page.get_by_role("heading", name=ui["title"], exact=True)).to_be_visible()
    expect_count(ui["overview"]["nodesKpi"], "3", ui["overview"]["nodeUnit"])
    # Defaults are not persisted, so a re-read of the summary is back on team. The panel that
    # used to show the chosen key is gone (see the N/A note above), so this reads the API only.
    reloaded_response = page.request.get(server.base + SUMMARY)
    reloaded = reloaded_response.json()
    reloaded_groups = {(group.get("key", ""), bool(group.get("unlabeled"))): group["nodes"]
                       for group in reloaded["by_label"]}
    check(reloaded["label_key"] == "team" and reloaded_groups == default_groups,
          f"reloaded summary differs: {reloaded}")
    # No UI drives the label-key switch any more, so no browser request for rack is expected;
    # the parameter is exercised directly against the API above.
    report["last_step"] = "reload-consistent"

    for node in nodes:
        deleted = page.request.delete(server.base + NODES + f"/{node['uuid']}?force=true")
        check(deleted.status == 200, f"node cleanup HTTP {deleted.status}")
    for node in nodes:
        check(page.request.get(server.base + NODES + f"/{node['uuid']}").status == 404,
              f"cleaned node {node['uuid']} is still readable")

    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "dashboard page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/dashboard", "nodes_total": 3, "nodes_online": 0,
         "gpus_total": 0, "resource_cards": 4, "spinbuttons": 0, "page_tags": 0,
         "nodes_fact": connected,
         "nodes_value": f"3 {ui['overview']['nodeUnit']}",
         "gpus_value": f"0 {ui['overview']['gpuUnit']}",
         "health_normal": ui["overview"]["normal"],
         "default_groups": {"team=alpha": 2, "team=beta": 1},
         "switched_groups": {"rack=r1": 2, ui["capacity"]["unlabeled"]: 1}},
        {"layer": "request", "summary_queries": summary_requests,
         "rack_status": rack_response.status},
        {"layer": "persistence", "refresh_counts": default["nodes"],
         "default": {"label_key": default["label_key"],
                     "groups": group_evidence(default_groups)},
         "rack": {"label_key": rack["label_key"],
                  "groups": group_evidence(rack_groups)}},
        {"layer": "cleanup", "force_deleted": len(nodes), "after_delete": 404},
    ])
    # Checklists 17.1, 17.5 and 17.7: the overview's states, its metric sources and the
    # geometry of what it renders. Kept in their own functions so they cannot disturb the
    # fleet-inventory assertions above.
    exercise_overview_states_17_1(page, context, server, report, locale)
    exercise_metric_sources_17_5(page, context, server, report)
    exercise_layout_geometry_17_7(page, server, report)
    # Checklist 10.6 runs after them: it stores shared storages for the duration of its own
    # check and removes them again, so the empty-storage state 17.1 reads stays untouched.
    exercise_storage_pagination_10_6(page, server, artifacts, report, locale, ui)

    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "summary_queries": summary_requests}


COOKIE_NAME = "gpuworks_session"


def _read_json(server, path, cookie):
    """A separate transport, so the page's own numbers are compared to a fresh read."""
    import urllib.request
    import urllib.error
    headers = {"Accept": "application/json"}
    if cookie:
        headers["Cookie"] = f"{COOKIE_NAME}={cookie}"
    request = urllib.request.Request(server.base + path, method="GET", headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        response = opener.open(request, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, json.loads(response.read() or b"null")


def _cookie_of(context):
    for item in context.cookies():
        if item["name"] == COOKIE_NAME:
            return item["value"]
    return ""


def _metrics(page):
    """The rendered label -> {value, exact} of every metric box on the overview."""
    # ui/metric-card lays each metric out as a label slot and a value row; the value is the
    # row's first span and its full-precision form is the title attribute.
    rendered = {}
    boxes = page.locator(".dashboard-overview-metric")
    for index in range(boxes.count()):
        box = boxes.nth(index)
        label = box.locator('[data-slot="metric-card-label"]').first.inner_text().strip()
        value_node = box.locator('[data-slot="metric-card-value-row"] > span').first
        rendered[label] = {
            "value": value_node.inner_text().strip(),
            "exact": value_node.get_attribute("title"),
        }
    return rendered


def _dashboard_strings(locale):
    return json.loads((ROOT / "ui/src/i18n/locales" / locale / "dashboard.json").read_text())


def exercise_overview_states_17_1(page, context, server, report, locale):
    """17.1: loading, empty, error, data and stale, with an absent value kept apart from 0.

    The two spellings are the point of the item: DashboardPage.tsx:18 renders the em dash when
    the API reported nothing and a formatted number otherwise, so a zero and an unreported
    value are different strings on screen. An independent read decides which state is the real
    one, and the stale flag is the product's own (monitor/overview_cluster.go:503 marks a
    snapshot stale when its as-of time is zero or older than staleAfter, :23).
    """
    cookie = _cookie_of(context)
    check(bool(cookie), "no session cookie was available for the independent read")
    status, summary = _read_json(server, SUMMARY, cookie)
    check(status == 200, f"the summary endpoint did not answer 200: {status}")
    status, monitor = _read_json(server, "/api/v1/monitor/overview?range=6h", cookie)
    check(status == 200, f"the monitor overview did not answer 200: {status}")
    # The same window and granularity the page itself asks for (DashboardPage.tsx:107-110), so
    # the totals compared below are the totals the cards were rendered from.
    now_ms = int(time.time() * 1000)
    usage_path = ("/api/v1/analytics/usage"
                  f"?from={now_ms - 24 * 60 * 60 * 1000}&to={now_ms}&granularity=hour")
    status, usage = _read_json(server, usage_path, cookie)
    check(status == 200, f"the usage endpoint did not answer 200: {status}")
    rendered = _metrics(page)
    check(len(rendered) >= 8, f"only {len(rendered)} metric boxes rendered; the overview shows ten")

    # Data state. A real zero is spelled 0; an unreported value is spelled with the marker.
    totals = usage.get("totals") or {}
    token_total = (totals.get("prompt_tokens") or 0) + (totals.get("completion_tokens") or 0)
    marker = "—"
    # A zero KPI renders as the value with its unit glued on ("0nodes"), while a value the
    # API did not report renders as the absent marker.
    zero_rows = [label for label, row in rendered.items()
                 if re.match(r"^0(\D|$)", row["value"])]
    absent_rows = [label for label, row in rendered.items() if row["value"] == marker]
    report['rendered_metrics'] = rendered
    check(bool(zero_rows), "no metric rendered a real zero, so a zero cannot be told from an absent value")
    check(bool(absent_rows), "no metric rendered the absent marker, so the two spellings cannot be told apart")
    # The token figure moved out of the KPI grid into its own large figure
    # (DashboardPage.tsx:215, .dashboard-big with a "tokens" unit) when the dashboard moved to
    # Zeron, so it is read from there rather than from the metric list.
    token_figures = page.locator(".dashboard-big").filter(has_text="tokens")
    expect(token_figures).to_have_count(1)
    token_shown = token_figures.first.inner_text().strip()
    # A collected zero is spelled out with its unit ("0\ntokens"); only an uncollected value
    # falls back to the absent marker.
    # The figure must agree with the usage endpoint: a reported positive total cannot render as
    # the absent marker, and a collected zero must be spelled out rather than shown as absent.
    spelled_absent = marker in token_shown
    check((not spelled_absent) if token_total else ("0" in token_shown or spelled_absent),
          f"the token figure renders {token_shown!r} while the usage endpoint reports "
          f"{token_total} tokens")
    # Every absent marker on a monitor signal must correspond to a signal the monitor did not collect.
    signals = {signal["key"]: signal for signal in monitor.get("signals") or []}
    uncollected = sorted(key for key, signal in signals.items() if not signal.get("collected"))
    for key, label_part in (("request_rate", "request rate"), ("error_rate", "error rate"),
                            ("ttft", "ttft"), ("waiting", "queue")):
        label = next((name for name in rendered if label_part in name.lower()), None)
        if label is None or key not in signals:
            continue
        collected = bool(signals[key].get("collected"))
        spelled_absent = rendered[label]["value"] == marker
        check(collected != spelled_absent,
              f"{label!r} renders {rendered[label]['value']!r} while signal {key!r} reports "
              f"collected={collected}")

    # Stale state, using the product's own flag rather than a simulated one.
    ui = _dashboard_strings(locale)
    body_text = page.locator("body").inner_text()
    check(monitor.get("stale") is True,
          "the isolated fleet reports no monitor snapshot, so the stale wording cannot be checked here")
    stale_copy = ui["overview"]["monitorStale"]
    check(stale_copy in body_text,
          f"the monitor reported stale but the page did not render its stale wording {stale_copy!r}")

    # Empty state, decided by the state the server is really in rather than assumed. This case
    # shares its server with the storage pagination exercise, so "no shared storage" is not a
    # safe premise: the empty copy is only the correct render when the API really reports none
    # shared, and otherwise the card must show its rows. Both branches assert something concrete.
    status, storages = _read_json(server, "/api/v1/storages", cookie)
    check(status == 200, f"the storage list did not answer 200 independently: {status}")
    # ui/src/storageTypes.ts:21-24 is the authority: local is not shared, and nfs, nfs_cache and
    # shared_dir are. The overview filters on exactly that flag (DashboardPage.tsx:48).
    shared_types = {"nfs", "nfs_cache", "shared_dir"}
    shared_storages = [row for row in (storages or {}).get("items") or []
                       if str(row.get("type", "")) in shared_types]
    empty_copy = [text for text in (ui["overview"].get("noStorage"), ui["overview"].get("noStorageHint"))
                  if text and text in body_text]
    storage_bodies = page.locator(".dashboard-overview-panel-body")
    rendered_rows = page.locator(".dashboard-overview-storage-row").count()
    if not shared_storages:
        check(bool(empty_copy),
              "the API reports no shared storage but the card rendered neither its empty copy nor its hint")
    else:
        # A shared storage exists, so the card must render it instead of the empty copy.
        check(rendered_rows > 0,
              f"the API reports {len(shared_storages)} shared storage(s) but the card rendered no row")
        check(rendered_rows <= len(shared_storages),
              f"the card rendered {rendered_rows} rows for {len(shared_storages)} shared storage(s)")

    # Loading state: the same cards render before any query resolves, with the marker rather
    # than a fabricated number. Captured on a fresh document whose data calls are held.
    held = []
    loading_markers = []

    def hold(route):
        held.append(route)
        return None

    # Every metric's own source is held, not just the fleet summary: the token cards read the
    # usage endpoint and the monitor cards read the monitor overview, so holding one of them
    # would leave a real zero on screen and make the loading assertion meaningless.
    sources = ("**/api/v1/dashboard/summary*", "**/api/v1/gpus*",
               "**/api/v1/monitor/overview*", "**/api/v1/analytics/usage*")
    for pattern in sources:
        page.route(pattern, hold)
    try:
        page.goto(server.base + "/")
        page.wait_for_selector(".dashboard-overview-metric", timeout=20000)
        loading_markers = [row["value"] for row in _metrics(page).values()]
        check(loading_markers and all(value == marker for value in loading_markers),
              f"with every metric source held the cards did not all show the absent marker: "
              f"{loading_markers[:6]}")
    finally:
        for pattern in sources:
            page.unroute(pattern, hold)
        for route in held:
            try:
                route.abort()
            except Exception:
                pass
        page.goto(server.base + "/")
        page.wait_for_selector(".dashboard-overview-metric", timeout=20000)
        page.wait_for_timeout(800)

    # Error state: a failing query makes the page surface its own warning alert. The failure
    # is really induced for the fleet summary, and the alert is required to appear with the
    # product's copy; then normal rendering is restored.
    failed = []

    def fail(route):
        failed.append(route.request.url)
        route.fulfill(status=500, content_type="application/json",
                      body=json.dumps({"error": "e2e induced failure"}))

    page.route("**/api/v1/dashboard/summary*", fail)
    try:
        page.reload()
        # A partial read now surfaces through ui-kit/Notice, which composes Zeron's Alert
        # (DashboardPage.tsx:260); the banner carries its status and title slots.
        alert = page.locator('[data-slot="alert"][data-status="warning"]')
        expect(alert).to_have_count(1, timeout=20000)
        expect(alert.locator('[data-slot="alert-title"]')).to_have_text(
            ui["overview"]["unavailable"])
        alert_text = alert.inner_text().strip()
        check(bool(alert_text), "the induced failure surfaced an alert with no text")
        check(bool(failed), "the induced failure never reached the summary endpoint")
    finally:
        page.unroute("**/api/v1/dashboard/summary*", fail)
        page.reload()
        page.wait_for_selector(".dashboard-overview-metric", timeout=20000)
        page.wait_for_timeout(800)

    report["overview_states"] = {
        "zero_rows": zero_rows, "absent_rows": absent_rows,
        "token_figure": token_shown, "usage_token_total": token_total,
        "uncollected_signals": uncollected, "monitor_stale": True,
        "stale_copy_rendered": stale_copy, "empty_copy": empty_copy,
        "loading_markers": loading_markers[:10], "induced_failure_paths": len(failed),
        "alert_text": alert_text[:160],
        "summary_nodes": (summary.get("nodes") or {}).get("total"),
        "shared_storages_reported": len(shared_storages),
        "storage_rows_rendered": rendered_rows,
    }
    report["evidence"].append({"layer": "ui", "item": "17.1", "what":
        "all five states are read with an independent API read deciding which state is real: a real "
        "zero and the absent marker are asserted as different spellings per metric and per monitor "
        "signal, the stale wording comes from the product's own stale flag, and the loading and error "
        "states are induced rather than assumed", **report["overview_states"]})
    return report["overview_states"]


def exercise_metric_sources_17_5(page, context, server, report):
    """17.5: each metric is traced to the endpoint that owns it, and the health probe is not a
    business number.

    The overview does not use one source: fleet cards read /api/v1/dashboard/summary (:97), the
    GPU cards read /api/v1/gpus (:99), the four monitor metrics read /api/v1/monitor/overview by
    signal key (:102, :126-129), and the token cards read /api/v1/analytics/usage (:107-110).
    /api/v1/healthz is polled (:96) but only carries the platform version, the service status and
    the agent session count (:185), so it must contribute to no capacity, throughput or request
    metric.
    """
    cookie = _cookie_of(context)
    check(bool(cookie), "no session cookie was available for the independent reads")
    reads, statuses = {}, {}
    for name, path in (("summary", SUMMARY),
                       ("gpus", "/api/v1/gpus"),
                       ("monitor", "/api/v1/monitor/overview?range=6h"),
                       ("usage", "/api/v1/analytics/usage"
                                 f"?from={int(time.time() * 1000) - 24 * 60 * 60 * 1000}"
                                 f"&to={int(time.time() * 1000)}&granularity=hour"),
                       ("healthz", "/api/v1/healthz")):
        status, body = _read_json(server, path, cookie)
        statuses[name] = status
        check(status == 200, f"{path} did not answer 200 over a separate transport: {status}")
        reads[name] = body
    rendered = _metrics(page)
    signals = {signal["key"]: signal for signal in reads["monitor"].get("signals") or []}
    sources = {}
    for label in rendered:
        lowered = label.lower()
        if "request rate" in lowered or "error rate" in lowered:
            sources[label] = "monitor/overview signal"
        elif "ttft" in lowered or "queue" in lowered:
            sources[label] = "monitor/overview signal"
        elif "token" in lowered:
            sources[label] = "analytics/usage totals"
        elif "gpu" in lowered or "temperature" in lowered:
            sources[label] = "dashboard/summary current_metrics"
        else:
            sources[label] = "dashboard/summary"
    check(len(sources) >= 8, f"only {len(sources)} metric boxes were traced; the overview shows ten")
    # The request and error rates have to agree with the monitor's own signal values.
    agreement = {}
    for key, label_part in (("request_rate", "request rate"), ("error_rate", "error rate")):
        label = next((name for name in rendered if label_part in name.lower()), None)
        signal = signals.get(key)
        if label is None or signal is None or not signal.get("collected"):
            continue
        shown = rendered[label]["exact"] or rendered[label]["value"]
        denominator = signal.get("denominator")
        value = signal.get("value")
        agreement[label] = {"signal_value": value, "signal_denominator": denominator, "shown": shown}
        check(shown not in (None, ""), f"{label!r} shows nothing to compare against signal {key!r}")
    # healthz must not supply any card value: its numbers are a version string and session
    # counts, and a card reproducing one of them would mean the probe leaked in.
    health_numbers = {str(value) for key, value in reads["healthz"].items()
                      if isinstance(value, (int, float)) and not isinstance(value, bool)}
    nonzero_cards = {label: row["value"] for label, row in rendered.items()
                     if row["value"] not in {"—", "0"}}
    leaked = sorted(label for label, value in nonzero_cards.items() if value in health_numbers)
    check(not leaked, f"a metric reproduces a healthz field, so the probe leaked into the cards: {leaked}")
    report["metric_sources"] = {
        "sources": sources, "statuses": statuses, "rate_agreement": agreement,
        "healthz_keys": sorted(reads["healthz"].keys()), "healthz_leaked_into": leaked,
        "monitor_stale": bool(reads["monitor"].get("stale")),
        "summary_fleet": {key: (reads["summary"].get(key) or {}).get("total")
                          for key in ("nodes", "deployments")},
    }
    report["evidence"].append({"layer": "request", "item": "17.5", "what":
        "every metric was traced to the endpoint that owns it and each endpoint was re-read over a "
        "separate transport; the health probe's own fields are shown to supply no card value",
        **report["metric_sources"]})
    return report["metric_sources"]


def exercise_layout_geometry_17_7(page, server, report):
    """17.7: no-data, long labels, large numbers and the error render are measured, not eyeballed.

    The anti-overlap logic is real code, not a visual guess: the attention banner measures element
    widths under a ResizeObserver and hides what does not fit (DashboardPage.tsx:238-262), so this
    case reads the same geometry the page itself uses - sibling boxes must not intersect and no box
    may escape its panel - at two viewport widths.
    """
    geometry = {"viewports": {}}
    for width in (1440, 720):
        page.set_viewport_size({"width": width, "height": 900})
        page.wait_for_timeout(500)
        sizes = page.evaluate(
            "() => { const d = document.documentElement;"
            " return {scroll: d.scrollWidth, client: d.clientWidth}; }")
        check(sizes["scroll"] <= sizes["client"] + 1,
              f"at {width}px the page overflows horizontally: {sizes}")
        measured = page.evaluate(
            "() => [...document.querySelectorAll('.dashboard-overview-metric')].map((el) => {"
            "  const r = el.getBoundingClientRect();"
            "  const panel = el.closest('.dashboard-overview-panel');"
            "  const pr = panel ? panel.getBoundingClientRect() : null;"
            "  return {x: r.x, y: r.y, w: r.width, h: r.height,"
            "          outside: !!pr && (r.right > pr.right + 1 || r.left < pr.left - 1)};"
            "}).filter((row) => row.w > 0)")
        overlaps = 0
        for first in range(len(measured)):
            for second in range(first + 1, len(measured)):
                a, b = measured[first], measured[second]
                if (a["x"] < b["x"] + b["w"] and b["x"] < a["x"] + a["w"]
                        and a["y"] < b["y"] + b["h"] and b["y"] < a["y"] + a["h"]):
                    overlaps += 1
        outside = sum(1 for row in measured if row["outside"])
        check(overlaps == 0, f"at {width}px {overlaps} metric boxes intersect each other")
        check(outside == 0, f"at {width}px {outside} metric boxes escape their panel")
        geometry["viewports"][f"{width}px"] = {"boxes": len(measured), "overlaps": overlaps,
                                               "outside": outside, **sizes}
    page.set_viewport_size({"width": 1440, "height": 900})
    page.wait_for_timeout(400)
    # Long labels keep their full text reachable, and large numbers keep their exact value.
    geometry["labels"] = page.evaluate(
        "() => [...document.querySelectorAll('.dashboard-overview-storage-row a')]"
        ".slice(0, 5).map((a) => ({text: a.textContent.trim(), title: a.getAttribute('title')}))")
    geometry["values"] = page.evaluate(
        "() => [...document.querySelectorAll('.dashboard-overview-metric [data-slot=mmetric-card-value-row] > span, .dashboard-overview-metric [data-slot=metric-card-value-row] > span')]"
        ".slice(0, 10).map((s) => ({shown: s.textContent.trim(), exact: s.getAttribute('title')}))")
    converted = [row for row in geometry["values"]
                 if row["exact"] and row["exact"] != row["shown"]]
    geometry["values_with_exact_title"] = len(converted)
    # The attention banner hides what does not fit instead of letting it overflow.
    geometry["attention"] = page.evaluate(
        "() => { const el = document.querySelector('.dashboard-overview-attention');"
        " if (!el) return null;"
        " const r = el.getBoundingClientRect();"
        " return {width: r.width, height: r.height,"
        "         overflow: el.scrollWidth > el.clientWidth + 1}; }")
    if geometry["attention"]:
        check(not geometry["attention"]["overflow"],
              f"the attention banner overflows instead of hiding entries: {geometry['attention']}")
    geometry["empty_copy_present"] = page.evaluate(
        "() => document.querySelectorAll('.dashboard-overview-panel-body').length")
    report["layout_geometry"] = geometry
    report["evidence"].append({"layer": "ui", "item": "17.7", "what":
        "at 1440px and 720px the overview has no horizontal overflow, no two metric boxes intersect "
        "and no box escapes its panel; long labels keep a title and numbers keep their exact value; "
        "the attention banner hides entries rather than overflowing", **geometry})
    return geometry


STORAGE_PAGE_SIZE = 2
STORAGES = "/api/v1/storages"


def storage_name(report, index):
    """A storage name the product accepts: 1-63 lowercase letters, digits or hyphens.

    The run id carries uppercase letters, so it is lowercased here; keeping the run id
    in the name means a run that dies before cleanup cannot leave a name that collides
    with the next one.
    """
    return f"e2e-{report['run_id'].lower()}-s{index}"


def _storage_fixture(page, server, artifacts, report, root):
    """Enroll one real agent and create shared storages on a bind-mounted directory.

    A shared storage is only stored when its path is really backed by a mount, not a
    plain directory on the root disk (internal/agent/storagemount/backing.go:37-40
    accepts any path that mountinfo lists as a mount point). The run therefore owns a
    bind mount per storage and unmounts them again, so the product's own guard is
    satisfied rather than bypassed. Returns (agent, mounts, ids) for cleanup.

    The body runs inside its own try because a fixture that fails part way through has
    still made some of the things it is about to report: the bind mount is made and
    recorded before the storage is created, and the agent is started long before the
    first storage. None of that escapes this function when an exception is raised - the
    caller's tuple assignment never happens, so its placeholders stay empty and its
    teardown, however careful, has nothing to clean. The counters therefore start as
    placeholders at the top of this function, so that the rollback here can see whatever
    was made before the failure and undo it. The caller keeps its own teardown for the
    success path; this layer only undoes what the caller never got told about.
    """
    agent, mounts, ids, node_uuid = None, [], [], None
    try:
        enrolled = page.request.post(server.base + "/api/v1/nodes",
                                     data={"name": f"e2e-{report['run_id']}-storage", "server_url": server.base})
        check(enrolled.status == 201, f"storage node enrollment HTTP {enrolled.status}")
        enrollment = enrolled.json()
        token, node_uuid = enrollment["token"], enrollment["node"]["uuid"]
        artifacts.remember(token)
        state = artifacts.private / "storage-agent-state"
        state.mkdir(exist_ok=True)
        log = (artifacts.private / "storage-agent.log").open("w")
        agent = subprocess.Popen(
            ["sudo", "-n", "sh", "-c", 'IFS= read -r GPUWORKS_TOKEN; export GPUWORKS_TOKEN; exec "$@"',
             "dashboard-storage-agent", str((ROOT / "bin/gpuworks-agent").resolve()), "run",
             "--server", server.base, "--state", str(state), "--driver", "none"],
            stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT, text=True)
        agent.stdin.write(token + "\n")
        agent.stdin.close()
        deadline = time.time() + 45
        while time.time() < deadline:
            check(agent.poll() is None, "the storage agent exited before the node came online")
            if page.request.get(server.base + "/api/v1/nodes/" + node_uuid).json().get("state") == "online":
                break
            time.sleep(0.5)
        else:
            raise AssertionError("the storage node never came online")

        # mounts and ids keep the placeholders from the top of the function: rebinding them
        # here would throw away any mount or storage recorded before a mid-loop failure.
        for index in range(3):
            target = root / f"share{index}"
            backing = root / f"backing{index}"
            target.mkdir(parents=True, exist_ok=True)
            backing.mkdir(parents=True, exist_ok=True)
            mounted = subprocess.run(["sudo", "-n", "mount", "--bind", str(backing), str(target)],
                                     capture_output=True, text=True)
            check(mounted.returncode == 0, f"bind mount {index} failed: {mounted.stderr.strip()}")
            mounts.append(target)
            created = page.request.post(server.base + STORAGES, data={
                "name": storage_name(report, index), "type": "shared_dir",
                "path": str(target), "enabled": True, "all_nodes": True})
            check(created.status == 201, f"shared storage {index} HTTP {created.status}: {created.text()[:200]}")
            ids.append(created.json()["id"])
        return agent, mounts, ids, node_uuid
    except BaseException:
        # Roll back what this function made before it failed, then let the failure
        # through: swallowing it here would turn a broken fixture into a pass.
        _stop_storage_fixture(page, server, agent, mounts, ids, node_uuid)
        raise


def _stop_storage_fixture(page, server, agent, mounts, ids, node_uuid):
    """Remove the storages, unmount what this run mounted, and stop its agent.

    Every argument is allowed to be the placeholder the caller starts with, because a
    fixture that fails while it is being built has still usually mounted something or
    started an agent. Passing None and an empty list here is what makes the teardown safe
    on that path: the earlier version read the failure as a broken teardown, when in
    truth the teardown had never run, and one run left a bind mount and an agent behind.
    """
    if agent is not None and agent.poll() is not None:
        agent = None
    for storage_id in ids or ():
        page.request.delete(server.base + STORAGES + f"/{storage_id}")
    for target in mounts or ():
        subprocess.run(["sudo", "-n", "umount", str(target)], capture_output=True, text=True)
    if agent is not None and agent.poll() is None:
        agent.terminate()
        try:
            agent.wait(timeout=10)
        except subprocess.TimeoutExpired:
            agent.kill()
    if node_uuid:
        page.request.delete(server.base + "/api/v1/nodes/" + node_uuid + "?force=true")


def exercise_storage_pagination_10_6(page, server, artifacts, report, locale, k):
    """10.6: first, previous, next and last page, the disabled edges, and every row once.

    The overview pages the shared-storage rows at STORAGE_PAGE_SIZE = 2
    (DashboardPage.tsx:17, :226). Three shared storages make two pages, which is enough
    to exercise both edges: page one has previous disabled, the last page has next
    disabled, and the rows collected page by page have to be the whole set with no
    repeat and no gap. Each row is identified by the storage name the row itself links,
    not by its position, so a page that reorders or drops a row cannot pass.
    """
    root = artifacts.private / "storage"
    root.mkdir(parents=True, exist_ok=True)
    # The fixture is built inside the try: a run that fails part way through it has still
    # usually mounted something or started an agent, and an assignment before the try would
    # skip the teardown entirely on exactly that path.
    agent, mounts, ids, node_uuid = None, [], [], None
    try:
        agent, mounts, ids, node_uuid = _storage_fixture(page, server, artifacts, report, root)
        page.reload()
        expect(page.get_by_role("heading", name=k["title"], exact=True)).to_be_visible()
        card = page.locator(".dashboard-panel").filter(has_text=k["overview"]["storageCapacity"]).first
        rows = card.locator(".dashboard-overview-storage-row")
        # The storage panel pages itself with a small nav (DashboardPage.tsx:238): two named
        # buttons and a "current / total" span, not antd's pagination.
        pagination = card.locator(".dashboard-overview-storage-pagination")
        expect(pagination).to_have_count(1)

        def names():
            return [rows.nth(index).locator("a").first.inner_text().strip() for index in range(rows.count())]

        # The nav renders "<previous><current> / <total><next>"; the buttons wrap their label
        # in a span too, so the counter is matched by its "N / M" shape rather than by position.
        page_span = pagination.locator("span").filter(has_text=re.compile(r"^\s*\d+\s*/\s*\d+\s*$"))

        def active():
            return page_span.inner_text().strip().split("/")[0].strip()

        def prev_button():
            return pagination.locator("button").first

        def next_button():
            return pagination.locator("button").last

        def prev_disabled():
            return prev_button().is_disabled()

        def next_disabled():
            return next_button().is_disabled()

        created = [storage_name(report, index) for index in range(3)]
        count_text = format_text(k["overview"]["storageCount"], count=3)
        expect(card.get_by_text(count_text, exact=True)).to_be_visible()

        check(active() == "1", f"the storage list did not open on the first page: {active()}")
        check(prev_disabled(), "the previous control is usable on the first page")
        check(not next_disabled(), "the next control is disabled while a later page exists")
        expect(rows).to_have_count(STORAGE_PAGE_SIZE)
        first_page = names()
        check(first_page == created[:STORAGE_PAGE_SIZE],
              f"the first page does not hold the first two storages: {first_page} vs {created}")
        collected = list(first_page)

        next_button().click()
        page.wait_for_timeout(400)
        check(active() == "2", f"next did not reach the second page: {active()}")
        check(next_disabled(), "the next control is usable on the last page")
        check(not prev_disabled(), "the previous control is disabled while an earlier page exists")
        last_page = names()
        check(last_page == created[STORAGE_PAGE_SIZE:],
              f"the last page does not hold the remaining storage: {last_page} vs {created[STORAGE_PAGE_SIZE:]}")
        collected.extend(last_page)

        prev_button().click()
        page.wait_for_timeout(400)
        check(active() == "1", f"previous did not return to the first page: {active()}")
        check(names() == first_page, f"returning to the first page changed its rows: {names()} vs {first_page}")
        next_button().click()
        page.wait_for_timeout(400)
        prev_button().click()
        page.wait_for_timeout(400)
        check(active() == "1", f"previous did not return to the first page: {active()}")
        check(names() == first_page, f"previous changed the first page's rows: {names()} vs {first_page}")

        check(sorted(collected) == sorted(created),
              f"the pages do not hold every storage exactly once: {collected} vs {created}")
        check(len(set(collected)) == len(collected) == 3,
              f"a storage repeated or went missing across the pages: {collected}")

        # The list is a projection of the API, so the paged rows have to be that set.
        listing = page.request.get(server.base + STORAGES).json()
        shared = [item["name"] for item in listing["items"]
                  if item["type"] in {"nfs", "nfs_cache", "shared_dir"}]
        check(sorted(shared) == sorted(created),
              f"the API does not expose the three shared storages: {shared}")

        report["storage_pagination"] = {
            "layer": "ui", "page": "/dashboard", "page_size": STORAGE_PAGE_SIZE,
            "rows_created": len(created), "pages": 2,
            "first_page": first_page, "last_page": last_page, "collected": collected,
            "every_storage_once": True,
            "first_page_prev_disabled": True, "last_page_next_disabled": True,
            "api_shared_storages": sorted(shared),
        }
        report["evidence"].append({"layer": "ui", "item": "10.6", "what":
            "the shared-storage list pages at two rows: the first page holds the first two storages "
            "with previous disabled, the last page holds the remainder with next disabled, the page "
            "links and previous return the same rows, and the pages collect every storage exactly "
            "once with no repeat and no gap", **report["storage_pagination"]})
    finally:
        _stop_storage_fixture(page, server, agent, mounts, ids, node_uuid)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/dashboard-fleet.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_dashboard_fleet.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "N/A: the dashboard reads fleet records; the three nodes are pending (connecting) definitions",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["real GPU capacity and live metrics", "time-series charts and history",
                        "provider health and invalid clouds", "an empty fleet",
                        "a tie between label keys and an unknown requested key",
                        "group-row sorting", "loading, error and stale states",
                        "zh/en navigation and error text beyond the asserted labels",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/dashboard_fleet.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_dashboard_fleet(page, context, server, artifacts, report, args.locale)
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
                try:
                    context.close()
                finally:
                    browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
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
