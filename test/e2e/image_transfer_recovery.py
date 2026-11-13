#!/usr/bin/env python3
"""Recover real CPU image archive failures and cancel in-flight downloads.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-image-failure-retry / make test-e2e-image-cancel-retry
After building: python3 test/e2e/image_transfer_recovery.py --case failure-retry --locale en
Also --case cancel-retry; --locale zh; --server-bin/--agent-bin/--artifact-dir.
Requires Linux, Chrome, Playwright, sudo -n, unshare, Docker and static busybox.
Every case owns its server/SQLite, CPU agent, Docker daemon and HTTP archive.
No existing node, shared Docker workload, GPU, registry or control-plane mock.
Evidence lives in secrets/e2e/<runId>; remove private state within seven days.
"""
from pathlib import Path
from urllib.parse import urlsplit
import re
import threading
import time

from playwright.sync_api import expect
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
from image_catalog import button, cleanup, observe, open_custom, read_detail, strings
from image_runtime_runner import run
from platform_token_read_revoke import check, collection


def seed(page, server, fixture, report):
    name = "e2e-" + report["run_id"]
    created = collection(page.request.post(server.base + "/api/v1/images/import", data={
        "series": [{"name": name, "engine": "callable", "source": "custom", "versions": [{
            "image_ref": fixture.image_ref, "arch": fixture.arch, "engine_version": "1",
            "source": "custom", "registry_sources": [fixture.archive_url]}]}]}))
    check(len(created) == 1, "fixture series count differs")
    detail = read_detail(page, server.base, created[0]["id"])
    check(len(detail["versions"]) == 1, "fixture version count differs")
    return name, detail["id"], detail["versions"][0]["id"]


def read_state(page, series, version, node, state):
    deadline = time.monotonic() + 90
    last = None
    while time.monotonic() < deadline:
        response = page.request.get(urlsplit(page.url)._replace(path=f"/api/v1/images/{series}", query="", fragment="").geturl())
        check(response.status == 200, "image state read failed")
        detail = response.json()
        versions = [v for v in detail["versions"] if v["id"] == version]
        if len(versions) == 1:
            nodes = [n for n in versions[0]["nodes"] if n["node_uuid"] == node]
            if len(nodes) == 1:
                last = nodes[0]["state"]
                if last == state:
                    return {"detail": detail, "version": versions[0], "node": nodes[0]}
        # Poll only while the authoritative state differs; this is a bounded
        # observable-condition wait, not a transfer-duration assumption.
        threading.Event().wait(.1)
    raise AssertionError(f"image state did not reach {state}; last observed {last}")


def open_row(page, text, name, fixture):
    open_custom(page, text)
    button(page, name).click()
    # The drawer's accessible name is the series name alone: the engine moved out of the title
    # into a subtitle line under it (ImageDetailDrawer.tsx:131-132, ui-kit/SideDrawer.tsx:29-30).
    detail = page.get_by_role("dialog", name=name, exact=True)
    expect(detail).to_contain_text("callable")
    expect(detail).to_be_in_viewport(ratio=1)
    detail.get_by_role("link", name=fixture.image_ref, exact=True).click()
    return detail.get_by_role("row").filter(has_text=fixture.node_name)


def choose_url(page, row, text, fixture):
    source = row.get_by_role("combobox", name=text["nodeTable"]["column"]["source"], exact=True)
    source.focus()
    source.press("ArrowDown")
    page.get_by_text(urlsplit(fixture.archive_url).netloc, exact=True).filter(visible=True).click()
    expect(source).to_have_attribute("aria-expanded", "false")


def open_archive(page, row, text, fixture):
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/preview-archive-url" and r.request.method == "POST") as preview:
        button(row, text["nodeTable"]["pullFromURL"]).click()
    title = text["nodeTable"]["archiveDialog"]["title"].replace("{{ref}}", fixture.image_ref).replace("{{node}}", fixture.node_name)
    return page.get_by_role("dialog", name=title, exact=True), preview.value


def dismiss_archive(page, row, text, fixture, name, series, version, path, report):
    """Cancel, close, Escape and the mask return to Get from URL and start no transfer."""
    transfers = []

    def seen(request):
        if request.method == "POST" and urlsplit(request.url).path.startswith(path):
            transfers.append(urlsplit(request.url).path)

    page.on("request", seen)
    parent = page.get_by_role("dialog", name=name, exact=True)
    pull = button(row, text["nodeTable"]["pullFromURL"])
    title = text["nodeTable"]["archiveDialog"]["title"].replace("{{ref}}", fixture.image_ref).replace("{{node}}", fixture.node_name)
    last = {}

    def reopen():
        opened, preview = open_archive(page, row, text, fixture)
        expect(opened).to_be_visible()
        last["dialog"] = opened
        last["preview"] = preview
        return opened

    dialog = assert_nested_dismissals(
        reopen(), parent, reopen,
        [("cancel", cancel_button(text["common"]["cancel"])),
         ("escape", escape), ("close", close_button), ("mask", mask)],
        lambda: len(transfers), pull)
    check(transfers == [], "archive dialog dismissal started a transfer")
    stayed = read_state(page, series, version, fixture.node_uuid, "absent")
    report.setdefault("evidence", []).append({
        "layer": "ui", "archive_dialog": title, "parent": f"{name} callable",
        "dismissals": ["cancel", "escape", "close", "mask"],
        "focus_returned": True, "transfer_posts": 0, "node_state": stayed["node"]["state"]})
    return dialog, last["preview"]


def observe_navigation(page):
    failures, navigation_aborts = [], []
    reloading = False
    origin = urlsplit(page.url).netloc
    allowed = {("POST", "/api/v1/auth/activity"), ("GET", "/api/v1/auth/me"),
        ("GET", "/api/v1/events"), ("GET", "/api/v1/updates")}
    session_status = {("POST", "/api/v1/auth/activity"): 204, ("POST", "/api/v1/auth/login"): 200,
        ("GET", "/api/v1/auth/me"): 200}

    def record(request):
        parsed = urlsplit(request.url)
        response = request.response()
        status = response.status if response else None
        item = {"method": request.method, "path": parsed.path, "error": request.failure, "response_status": status}
        same_origin_abort = parsed.netloc == origin and request.failure == "net::ERR_ABORTED"
        if same_origin_abort and reloading and (request.method, parsed.path) in allowed:
            navigation_aborts.append({**item, "reason": "explicit-page-reload"})
        elif same_origin_abort and status == session_status.get((request.method, parsed.path), -999):
            # These session consumers intentionally use only success status;
            # Chrome may abort the unused body after the exact successful HTTP
            # response. No image/business request is excluded by this rule.
            navigation_aborts.append({**item, "reason": "completed-session-status-body-unused"})
        else:
            failures.append(item)

    def reload_page():
        nonlocal reloading
        reloading = True
        try:
            page.reload()
        finally:
            reloading = False

    page.on("requestfailed", record)
    return failures, navigation_aborts, reload_page


def wait_staging_clean(fixture, *, require_directory=False):
    directory = fixture.agent_state / "images/gpuworks-image-archives"
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        remaining = list(directory.glob("*"))
        if not remaining and (directory.is_dir() or not require_directory):
            return []
        threading.Event().wait(.1)
    raise AssertionError("owned agent left incomplete archive staging: " + ", ".join(p.name for p in remaining))


def hold_archive_sync(page, ok_button, path, expected, report):
    # setStarting runs before syncImage, so the confirm button is loading
    # while the first POST /sync is still held.
    held = []
    sync = path + "/sync"
    pattern = re.compile(re.escape(sync) + r"(?:\?|$)")

    def hold_post(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != sync:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    ok_button.click()
    report["last_step"] = "archive-sync-inflight"
    expect(ok_button).to_have_class(re.compile(r"\bant-btn-loading\b"))
    ok_button.click()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and not held:
        page.wait_for_timeout(100)
    check(len(held) == 1, "duplicate archive sync while the first request is held")
    check(held[0].request.post_data_json == expected, "held archive sync body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == sync and r.request.method == "POST") as started:
        held[0].continue_()
    page.unroute(pattern, hold_post)
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True})
    return started.value


# Contract: gpuworks/image-runtime/url-failure-retry
# Mainline: 自有CPU agent与可预览归档 → UI确认URL获取后真实上游503 → UI/GET失败原因503、无镜像与残留 → 恢复上游并UI重试 → 新transfer、刷新present、精确digest且真实容器输出marker → 清理自有目录/节点/进程。
def test_url_failure_retry(page, server, fixture, artifacts, report, locale):
    text = strings(locale)
    name, series, version = seed(page, server, fixture, report)
    path = f"/api/v1/images/{version}/nodes/{fixture.node_uuid}"
    expected = {"source": "url", "archive_url": fixture.archive_url, "relay": False}
    read_state(page, series, version, fixture.node_uuid, "absent")
    check(fixture.docker(["image", "ls", "-q"]).stdout.strip() == "", "daemon contains an image before UI transfer")
    errors, failures, mutations = observe(page)
    transport_failures, navigation_aborts, reload_page = observe_navigation(page)
    report["browser_observations"] = {"errors": errors, "failures": failures, "transport_failures": transport_failures,
        "navigation_aborts": navigation_aborts, "mutations": mutations}
    reload_page()
    row = open_row(page, text, name, fixture)
    choose_url(page, row, text, fixture)
    dialog, preview = dismiss_archive(page, row, text, fixture, name, series, version, path, report)
    expect(dialog).to_be_in_viewport(ratio=1)
    url_field = dialog.get_by_label(text["import"]["url"]["label"], exact=True)
    expect(url_field).to_have_value(fixture.archive_url)
    check(preview.status == 200 and preview.request.post_data_json == {"url": fixture.archive_url}, "preview request differs")
    check(preview.json()["repo_tags"] == [fixture.image_ref], "preview did not inspect owned archive")
    # Archive URL confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    submit = dialog.locator(".ant-modal-footer .ant-btn-primary")
    assert_dialog_shell(page, dialog, url_field, submit)
    title = text["nodeTable"]["archiveDialog"]["title"].replace("{{ref}}", fixture.image_ref).replace("{{node}}", fixture.node_name)
    report.setdefault("evidence", []).append({"layer": "ui", "dialog_shell": title, "viewport": "1440x900"})
    # Change the actual external server only after successful browser preview.
    first_audit_start = len(fixture.archive.snapshot())
    fixture.archive.failure_status = 503
    first = hold_archive_sync(page, submit, path, expected, report)
    check(first.status == 202 and first.request.post_data_json == expected, "initial acquisition request differs")
    first_id = first.json()["transfer_id"]
    check(first_id and not urlsplit(first.url).query, "initial transfer identity/query differs")
    failed = read_state(page, series, version, fixture.node_uuid, "failed")
    check("503" in failed["node"]["message"] and failed["node"]["source"] == "url", "real download rejection reason lost")
    expect(row.get_by_text(text["state"]["failed"], exact=True)).to_be_visible(timeout=30000)
    # The failure reason is the expanded node detail, not a cell of the node row.
    expand = row.get_by_role("button", name=text["nodeTable"]["expand"], exact=True)
    if expand.count():
        expand.click()
    expect(page.get_by_role("dialog", name=name, exact=True).get_by_text(failed["node"]["message"], exact=True)).to_be_visible()
    check(fixture.docker(["image", "inspect", fixture.image_ref], check=False).returncode != 0, "failed download created an image")
    wait_staging_clean(fixture)
    failed_audit = fixture.archive.snapshot()[first_audit_start:]
    check(any(r["method"] == "GET" and r["range"] == "bytes=0-0" and r["status"] == 503 for r in failed_audit), "agent never received fixture 503")
    fixture.archive.failure_status = None
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["failed"], exact=True)).to_be_visible()
    choose_url(page, row, text, fixture)
    dialog, preview = open_archive(page, row, text, fixture)
    check(preview.status == 200 and preview.json()["repo_tags"] == [fixture.image_ref], "restored archive preview failed")
    expect(dialog.get_by_label(text["import"]["url"]["label"], exact=True)).to_have_value(fixture.archive_url)
    retry_audit_start = len(fixture.archive.snapshot())
    with page.expect_response(lambda r: urlsplit(r.url).path == path + "/sync/retry" and r.request.method == "POST") as retried:
        button(dialog, text["nodeTable"]["pull"]).click()
    second = retried.value
    check(second.status == 202 and not urlsplit(second.url).query and second.request.post_data_json == expected, "retry lost URL/direct source")
    check(second.json()["transfer_id"] and second.json()["transfer_id"] != first_id, "retry reused failed transfer identity")
    present = read_state(page, series, version, fixture.node_uuid, "present")
    check(present["node"]["image_digest_id"] == fixture.image_id and present["version"]["present_nodes"] == 1, "retry availability/digest differs")
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible()
    persisted = read_detail(page, server.base, series)
    saved_version = next(v for v in persisted["versions"] if v["id"] == version)
    saved_node = next(n for n in saved_version["nodes"] if n["node_uuid"] == fixture.node_uuid)
    check(saved_node["image_digest_id"] == fixture.image_id and saved_node["state"] == "present", "refresh lost content identity")
    check(fixture.docker(["image", "inspect", "--format", "{{.Id}}", fixture.image_ref]).stdout.strip() == fixture.image_id, "Docker recovered wrong content")
    executed = fixture.docker(["run", "--rm", "--network", "none", "--pull=never", fixture.image_ref])
    check(executed.stdout == fixture.marker + "\n", "recovered archive cannot execute")
    check(fixture.docker(["ps", "-aq"]).stdout.strip() == "", "consumer container leaked")
    wait_staging_clean(fixture, require_directory=True)
    check(not errors and not failures and not transport_failures, "unexpected browser error")
    check([m for m in mutations if "/nodes/" in m["path"]] == [
        {"method": "POST", "path": path + "/sync"}, {"method": "POST", "path": path + "/sync/retry"}], "unexpected or duplicate node mutations")
    retry_audit = fixture.archive.snapshot()[retry_audit_start:]
    downloaded_bytes = sum(r["bytes_sent"] for r in retry_audit if r["method"] == "GET"
        and r["status"] == 206 and r["range"] != "bytes=0-0")
    check(downloaded_bytes == fixture.archive_size, "runtime retry did not deliver exact archive bytes independently of preview")
    report["evidence"].extend([
        {"layer": "ui", "failure_message": failed["node"]["message"], "reload_failed_then_present": True, "retry_url": fixture.archive_url},
        {"layer": "request", "initial": {"status": 202, "body": expected, "response": first.json()}, "retry": {"status": 202, "body": expected, "response": second.json()}},
        {"layer": "persistence", "failed": failed["detail"], "recovered": persisted},
        {"layer": "runtime", "failed_archive_requests": failed_audit, "recovered_archive_requests": retry_audit, "image_id": fixture.image_id,
         "stdout": executed.stdout, "exit": executed.returncode, "staging_remaining": []}])
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "transport_failures": transport_failures, "navigation_aborts": navigation_aborts, "mutations": mutations})
    cleanup(page, server.base, series, report)


# Contract: gpuworks/image-runtime/url-cancel-retry
# Mainline: 自有CPU agent与有效归档 → UI获取且4条真实HTTP连接各传部分内容后阻塞 → UI取消 → 真实连接断开、GET/刷新failed并清理暂存且Docker无镜像 → 释放旧响应仍不复活 → UI重试新transfer并真实容器执行 → 清理自有资源。
def test_url_cancel_retry(page, server, fixture, artifacts, report, locale):
    text = strings(locale)
    name, series, version = seed(page, server, fixture, report)
    path = f"/api/v1/images/{version}/nodes/{fixture.node_uuid}"
    expected = {"source": "url", "archive_url": fixture.archive_url, "relay": False}
    read_state(page, series, version, fixture.node_uuid, "absent")
    errors, failures, mutations = observe(page)
    transport_failures, navigation_aborts, reload_page = observe_navigation(page)
    report["browser_observations"] = {"errors": errors, "failures": failures, "transport_failures": transport_failures,
        "navigation_aborts": navigation_aborts, "mutations": mutations}
    reload_page()
    row = open_row(page, text, name, fixture)
    choose_url(page, row, text, fixture)
    dialog, preview = dismiss_archive(page, row, text, fixture, name, series, version, path, report)
    expect(dialog).to_be_in_viewport(ratio=1)
    url_field = dialog.get_by_label(text["import"]["url"]["label"], exact=True)
    expect(url_field).to_have_value(fixture.archive_url)
    check(preview.status == 200 and preview.json()["repo_tags"] == [fixture.image_ref], "preview did not read owned archive")
    # Archive URL confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    submit = dialog.locator(".ant-modal-footer .ant-btn-primary")
    assert_dialog_shell(page, dialog, url_field, submit)
    title = text["nodeTable"]["archiveDialog"]["title"].replace("{{ref}}", fixture.image_ref).replace("{{node}}", fixture.node_name)
    report.setdefault("evidence", []).append({"layer": "ui", "dialog_shell": title, "viewport": "1440x900"})
    # Same held-confirm family as the failure case: 503 keeps the URL and reason, then this case continues.
    fixture.archive.failure_status = 503
    refused = hold_archive_sync(page, submit, path, expected, report)
    check(refused.status == 202 and refused.request.post_data_json == expected, "refused acquisition request differs")
    refused_row = read_state(page, series, version, fixture.node_uuid, "failed")
    check("503" in refused_row["node"]["message"] and refused_row["node"]["source"] == "url", "503 reason lost")
    expect(row.get_by_text(text["state"]["failed"], exact=True)).to_be_visible(timeout=30000)
    expand = row.get_by_role("button", name=text["nodeTable"]["expand"], exact=True)
    if expand.count():
        expand.click()
    expect(page.get_by_role("dialog", name=name, exact=True).get_by_text(
        refused_row["node"]["message"], exact=True)).to_be_visible()
    check(fixture.docker(["image", "inspect", fixture.image_ref], check=False).returncode != 0, "503 download created an image")
    wait_staging_clean(fixture)
    fixture.archive.failure_status = None
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["failed"], exact=True)).to_be_visible()
    choose_url(page, row, text, fixture)
    dialog, preview = open_archive(page, row, text, fixture)
    expect(dialog.get_by_label(text["import"]["url"]["label"], exact=True)).to_have_value(fixture.archive_url)
    check(preview.status == 200 and preview.json()["repo_tags"] == [fixture.image_ref], "archive preview after 503 failed")
    first_audit_start = len(fixture.archive.snapshot())
    fixture.archive.hold_partial_downloads()
    with page.expect_response(lambda r: urlsplit(r.url).path == path + "/sync/retry" and r.request.method == "POST") as started:
        button(dialog, text["nodeTable"]["pull"]).click()
    first = started.value
    check(first.status == 202 and first.request.post_data_json == expected, "acquisition request differs")
    first_id = first.json()["transfer_id"]
    check(first_id and not urlsplit(first.url).query, "initial transfer identity/query differs")
    fixture.archive.wait_barrier()
    syncing = read_state(page, series, version, fixture.node_uuid, "syncing")
    check(syncing["node"]["transfer_id"] == first_id, "active transfer identity differs")
    staging = fixture.agent_state / "images/gpuworks-image-archives"
    check(any(staging.glob("*.part")), "partial download did not stage real bytes")
    expect(row.get_by_text(text["state"]["syncing"], exact=True)).to_be_visible(timeout=30000)
    expect(button(row, text["nodeTable"]["cancel"])).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == path + "/cancel" and r.request.method == "POST") as cancelled:
        button(row, text["nodeTable"]["cancel"]).click()
    response = cancelled.value
    check(response.status == 202 and response.request.post_data is None and not urlsplit(response.url).query, "cancel request differs")
    check(response.json()["state"] == "failed" and not response.json().get("transfer_id"), "cancel retained active transfer")
    fixture.archive.wait_barrier(disconnected=True)
    cancelled_audit = fixture.archive.snapshot()[first_audit_start:]
    partial = [r for r in cancelled_audit if r["disconnected"] and r["bytes_sent"] == 32768]
    check(len(partial) == 4, "cancellation did not close all four partial HTTP transfers")
    wait_staging_clean(fixture, require_directory=True)
    check(fixture.docker(["image", "inspect", fixture.image_ref], check=False).returncode != 0, "cancelled transfer loaded an image")
    fixture.archive.release_downloads()
    failed = read_state(page, series, version, fixture.node_uuid, "failed")
    check("cancel" in failed["node"]["message"].lower() and not failed["node"].get("transfer_id"), "cancel reason/terminal identity lost")
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["failed"], exact=True)).to_be_visible()
    expect(button(row, text["nodeTable"]["cancel"])).to_have_count(0)
    check(fixture.docker(["image", "ls", "-q"]).stdout.strip() == "", "released cancelled response resurrected image")
    choose_url(page, row, text, fixture)
    dialog, preview = open_archive(page, row, text, fixture)
    check(preview.status == 200 and preview.json()["repo_tags"] == [fixture.image_ref], "retry archive unavailable")
    retry_audit_start = len(fixture.archive.snapshot())
    with page.expect_response(lambda r: urlsplit(r.url).path == path + "/sync/retry" and r.request.method == "POST") as retried:
        button(dialog, text["nodeTable"]["pull"]).click()
    second = retried.value
    check(second.status == 202 and not urlsplit(second.url).query and second.request.post_data_json == expected, "cancel retry body differs")
    check(second.json()["transfer_id"] and second.json()["transfer_id"] != first_id, "cancel retry reused stale transfer")
    present = read_state(page, series, version, fixture.node_uuid, "present")
    check(present["node"]["image_digest_id"] == fixture.image_id and present["version"]["present_nodes"] == 1, "retry did not converge to unique content")
    executed = fixture.docker(["run", "--rm", "--network", "none", "--pull=never", fixture.image_ref])
    check(executed.stdout == fixture.marker + "\n", "retry image failed real CPU execution")
    check(fixture.docker(["image", "inspect", "--format", "{{.Id}}", fixture.image_ref]).stdout.strip() == fixture.image_id, "retry Docker content digest differs")
    check(fixture.docker(["ps", "-aq"]).stdout.strip() == "", "consumer container leaked")
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible()
    persisted = read_detail(page, server.base, series)
    saved_version = next(v for v in persisted["versions"] if v["id"] == version)
    saved_node = next(n for n in saved_version["nodes"] if n["node_uuid"] == fixture.node_uuid)
    check(saved_node["state"] == "present" and saved_node["image_digest_id"] == fixture.image_id, "reload lost recovered image")
    wait_staging_clean(fixture, require_directory=True)
    check(not errors and not failures and not transport_failures, "unexpected browser errors")
    check([m for m in mutations if "/nodes/" in m["path"]] == [
        {"method": "POST", "path": path + "/sync"}, {"method": "POST", "path": path + "/sync/retry"},
        {"method": "POST", "path": path + "/cancel"}, {"method": "POST", "path": path + "/sync/retry"}],
        "unexpected or duplicate transfer mutation")
    retry_audit = fixture.archive.snapshot()[retry_audit_start:]
    downloaded_bytes = sum(r["bytes_sent"] for r in retry_audit if r["method"] == "GET"
        and r["status"] == 206 and r["range"] != "bytes=0-0")
    check(downloaded_bytes == fixture.archive_size, "runtime retry did not deliver exact archive bytes independently of preview")
    report["evidence"].extend([
        {"layer": "ui", "syncing_cancel_available": True, "cancelled_reload_failed": True, "retry_reload_present": True,
         "failure_status": 503, "failure_message": refused_row["node"]["message"], "archive_url_retained": fixture.archive_url},
        {"layer": "request", "initial": first.json(), "cancel": response.json(), "retry": second.json(), "acquisition_body": expected},
        {"layer": "persistence", "cancelled": failed["detail"], "recovered": persisted},
        {"layer": "runtime", "cancelled_requests": cancelled_audit, "four_partial_connections_disconnected": True,
         "cancelled_image_absent": True, "staging_remaining": [], "released_old_responses_no_resurrection": True,
         "image_id": fixture.image_id, "stdout": executed.stdout, "exit": executed.returncode, "archive_requests": retry_audit}])
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "transport_failures": transport_failures, "navigation_aborts": navigation_aborts, "mutations": mutations})
    cleanup(page, server.base, series, report)


if __name__ == "__main__":
    raise SystemExit(run({"failure-retry": test_url_failure_retry, "cancel-retry": test_url_cancel_retry}, Path(__file__), extra_sources=[
        "ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImageNodeTable.tsx", "ui/src/pages/images/ImageArchiveURLDialog.tsx",
        "ui/src/i18n/locales/en/images.json", "ui/src/i18n/locales/zh/images.json", "internal/agent/app/image_load_url.go",
        "internal/server/hub/image_cancel.go", "internal/server/api/images_nodes.go"]))
