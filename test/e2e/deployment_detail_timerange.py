#!/usr/bin/env python3
"""Real-Chrome acceptance for the deployment-detail time-range controls.

The caller provisions a running deployment (fake-engine is enough: the range
controls must exist and must drive the metrics query parameters even while the
engine reports no points). Explicit gpu3/L4 manual job only, never the offline
unit suite.

Verified behavior:
- the deployment drawer shows the time-range select over the metrics panel;
- quick ranges re-issue /api/v1/metrics/query with start/end/step for the window;
- the custom range drives start/end through the RangePicker;
- a range beyond retention surfaces the warning;
- the instance log tab's range picker queries /api/v1/logs/search with bounds.

The absence of the chart dataZoom slider is asserted by the offline unit test
(buildTimeSeriesOption); canvas rendering keeps it opaque to DOM queries here.
"""
from __future__ import annotations

import argparse
import re
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.sync_api import Page, expect, sync_playwright


def login(page: Page, base_url: str, password: str) -> None:
    page.goto(f"{base_url}/deployments")
    page.locator('input[autocomplete="username"]').fill("admin")
    page.locator('input[autocomplete="current-password"]').fill(password)
    page.get_by_role("button", name="Sign In").click()
    expect(page.get_by_role("heading", name="Inference Services")).to_be_visible()


def open_drawer(page: Page, base_url: str, deployment_id: int) -> None:
    page.goto(f"{base_url}/deployments?deployment={deployment_id}")
    expect(page.get_by_role("dialog").get_by_role("tab", name=re.compile(r"^Instance details \("))).to_be_visible()


def metrics_window(page: Page, step: str):
    def matches(request) -> bool:
        url = urlparse(request.url)
        params = parse_qs(url.query)
        return (
            url.path == "/api/v1/metrics/query"
            and params.get("step") == [step]
            and "start" in params and "end" in params
        )
    return page.expect_request(matches)


def choose_range(page: Page, label: str, scope=None) -> None:
    drawer = page.get_by_role("dialog")
    control = (scope if scope is not None else drawer).get_by_role("combobox", name="Time range")
    control.click()
    option = page.get_by_role("option", name=label, exact=True)
    option.click()
    expect(control).to_have_attribute("aria-expanded", "false")
    expect(option).to_be_hidden()


def quick_range_drives_metrics_queries(page: Page) -> None:
    expect(page.get_by_role("tab", name="Deployment Details", exact=True)).to_have_attribute("aria-selected", "true")
    with metrics_window(page, "30s"):
        choose_range(page, "1 hour")


def custom_range_drives_start_end(page: Page, capture) -> None:
    drawer = page.get_by_role("dialog")
    choose_range(page, "Custom")
    inputs = drawer.locator(".ant-picker:visible input")
    expect(inputs.first).to_be_visible()
    inputs.first.click()
    panel = page.locator(".ant-picker-dropdown:visible")
    days = panel.locator("td.ant-picker-cell-in-view[title]")
    confirm = panel.locator(".ant-picker-ok button")
    capture("custom-calendar")
    days.nth(0).click()
    capture("custom-start")
    confirm.click()
    days.nth(7).click()
    capture("custom-end")
    with metrics_window(page, "1h") as issued:
        confirm.click()
    params = parse_qs(urlparse(issued.value.url).query)
    assert int(params["end"][0]) - int(params["start"][0]) == 7 * 24 * 60 * 60 * 1000
    capture("custom-applied")


def beyond_retention_warns(page: Page, capture) -> None:
    choose_range(page, "1 month")
    expect(page.get_by_text(re.compile("exceeds the 7-day retention"))).to_be_visible()
    capture("retention-warning")
    choose_range(page, "15 minutes")
    expect(page.get_by_text(re.compile("exceeds the 7-day retention"))).to_have_count(0)
    capture("restore-default-range")


def assert_instance_filters_uncovered(drawer) -> None:
    tablist = drawer.get_by_role("tablist", name="Deployment Details")
    filters = drawer.locator(".cascade-filters button")
    expect(filters.first).to_be_visible()
    tab_bottom = tablist.evaluate("el => el.getBoundingClientRect().bottom")
    for button in filters.all():
        assert button.evaluate("""(el, tabBottom) => {
            const r = el.getBoundingClientRect();
            return r.top >= tabBottom && el.contains(document.elementFromPoint(r.x + r.width / 2, r.top + 1));
        }""", tab_bottom), f"Instance filter covered by the tab strip: {button.inner_text()}"


def log_range_queries_store(page: Page, capture) -> None:
    drawer = page.get_by_role("dialog")
    drawer.get_by_role("tab", name=re.compile(r"^Instance details \(")).click()
    logs = drawer.get_by_role("region", name="Logs", exact=True)

    assert_instance_filters_uncovered(drawer)
    capture("instance-filters")
    logs.scroll_into_view_if_needed()
    assert_instance_filters_uncovered(drawer)
    capture("instance-live-logs")

    def bounded_log_search(request) -> bool:
        url = urlparse(request.url)
        params = parse_qs(url.query)
        return url.path == "/api/v1/logs/search" and all(key in params for key in ("instance", "from", "to"))

    with page.expect_request(bounded_log_search):
        choose_range(page, "6 hours", logs)
    expect(drawer.get_by_text(re.compile("persisted logs in the selected range"))).to_be_visible()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:8802")
    parser.add_argument("--password", default="gpuworks-test-password-2026")
    parser.add_argument("--deployment-id", type=int, required=True, help="an existing deployment whose drawer is inspected")
    parser.add_argument("--artifact-dir", type=Path, required=True)
    args = parser.parse_args()
    args.artifact_dir.mkdir(parents=True, exist_ok=True)

    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(channel="chrome", headless=True)
        context = browser.new_context(locale="en-US", viewport={"width": 1600, "height": 1000})
        context.add_init_script("localStorage.setItem('gpuworks.locale', 'en')")
        page = context.new_page()
        page.set_default_timeout(10_000)
        def capture(name: str) -> None:
            page.screenshot(path=str(args.artifact_dir / f"{name}.png"), animations="disabled")

        try:
            login(page, args.base_url.rstrip("/"), args.password)
            capture("deployment-list")
            with metrics_window(page, "10s"):
                open_drawer(page, args.base_url.rstrip("/"), args.deployment_id)
            capture("overview")
            quick_range_drives_metrics_queries(page)
            capture("one-hour-range")
            custom_range_drives_start_end(page, capture)
            beyond_retention_warns(page, capture)
            log_range_queries_store(page, capture)
            capture("deployment-detail-time-range")
        except Exception:
            page.screenshot(path=str(args.artifact_dir / "failure.png"), full_page=True)
            print(f"failure URL: {page.url}")
            print((page.locator("body").inner_text())[:4_000])
            raise
        finally:
            context.close()
            browser.close()
    print("Chrome E2E passed: deployment detail time-range controls")


if __name__ == "__main__":
    main()
