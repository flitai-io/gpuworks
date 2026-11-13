#!/usr/bin/env python3
"""Acquire and remove a real node image through the GPUWorks browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-image-url-runtime
After building: python3 test/e2e/image_runtime.py --case acquire-remove --locale en
Use --locale zh for Chinese; --server-bin/--agent-bin/--artifact-dir are optional.
Requires Linux, Chrome, Python Playwright, sudo -n, unshare, Docker, static
busybox. Runs as an ordinary user with an inaccessible shared Docker archive
directory. Own daemon/socket/data-root and CPU agent; no existing workloads.
The source is a real HTTP docker-save archive, not a Registry or a GPU image.
Private run evidence and failed state live in secrets/e2e/<runId> for 7 days.
"""
from pathlib import Path
from urllib.parse import urlsplit
import json
import re
import threading
import time

from playwright.sync_api import expect
from image_catalog import button, cleanup, observe, open_custom, read_detail, strings
from image_runtime_runner import run
from platform_token_read_revoke import check, collection


def wait_node_state(page, server, series_id, version_id, node_uuid, state):
    deadline = time.monotonic() + 90
    last = None
    while time.monotonic() < deadline:
        detail = read_detail(page, server.base, series_id)
        versions = [v for v in detail["versions"] if v["id"] == version_id]
        if len(versions) == 1:
            nodes = [n for n in versions[0]["nodes"] if n["node_uuid"] == node_uuid]
            if len(nodes) == 1:
                last = nodes[0]["state"]
                if last == state:
                    return {"detail": detail, "version": versions[0], "node": nodes[0]}
        # Wait only while the independent authoritative read differs.
        threading.Event().wait(.1)
    raise AssertionError(f"image state did not reach {state}; last observed {last}")


def open_version(page, text, name, image_ref):
    open_custom(page, text)
    # The custom tab renders from its own list request; clicking before it lands
    # finds no row and the drawer never opens.
    expect(button(page, name)).to_be_visible()
    button(page, name).click()
    # ImageDetailDrawer.tsx:131-132 titles the drawer with the series name alone
    # and puts engine/framework on a subtitle, so the "{{name}} · Details" label
    # this used to wait for no longer exists.
    detail = page.get_by_role("dialog", name=re.compile('^' + re.escape(name)))
    expect(detail).to_be_in_viewport(ratio=1)
    # The drawer has two tabs: versions and nodes. The version picker and node
    # table live on the nodes tab, which a version row opens
    # (ImageDetailDrawer.tsx:191 onRow -> openVersion). Clicking that row replaces
    # the old link click, and landing on the picker is what proves the version is
    # selected.
    tag = detail.locator('[aria-label=' + json.dumps(image_ref) + ']')
    expect(tag).to_have_count(1)
    tag.first.locator('xpath=ancestor::tr[1]').click()
    # Both tabs keep their own picker mounted, so there is more than one; the
    # point of the assertion is that the version is selected and actionable.
    picker = detail.locator('[aria-label=' + json.dumps(text["detail"]["version"]) + ']')
    expect(picker.first).to_be_visible()
    return detail


# Contract: gpuworks/image-runtime/url-acquire-remove
# Mainline: 自有真实CPU agent、空Docker和有效归档 → UI显式选择URL并获取 → agent下载/load、独立GET与刷新present且digest一致 → 实际容器输出marker → UI取消删除无请求、确认删除 → Docker无镜像且GET/刷新absent，目录版本仍在 → 清理自有系列/节点/进程。
def test_url_acquire_remove(page, server, fixture, artifacts, report, locale):
    text = strings(locale)
    name = "e2e-" + report["run_id"]
    payload = {"series": [{"name": name, "engine": "callable", "source": "custom", "versions": [{
        "image_ref": fixture.image_ref, "arch": fixture.arch, "engine_version": "1",
        "source": "custom", "registry_sources": [fixture.archive_url]}]}]}
    created = collection(page.request.post(server.base + "/api/v1/images/import", data=payload))
    check(len(created) == 1, "fixture must create one series")
    series_id = created[0]["id"]
    before = read_detail(page, server.base, series_id)
    check(len(before["versions"]) == 1, "fixture must create one version")
    version_id = before["versions"][0]["id"]
    check(fixture.docker(["image", "ls", "-q"]).stdout.strip() == "", "target daemon is not empty")
    check(fixture.docker(["ps", "-aq"]).stdout.strip() == "", "target daemon has a pre-existing container")
    initial = wait_node_state(page, server, series_id, version_id, fixture.node_uuid, "absent")
    check(initial["version"]["present_nodes"] == 0, "absent image counted as available")
    errors, failures, mutations = observe(page)
    request_failures, navigation_aborts = [], []
    report["browser_observations"] = {"errors": errors, "http_failures": failures,
        "request_failures": request_failures, "expected_cancellations": navigation_aborts}
    reloading = False

    def record_transport(request):
        item = {"method": request.method, "path": urlsplit(request.url).path, "failure": request.failure}
        response = request.response()
        item["response_status"] = response.status if response else None
        session = (request.method, item["path"]) in {
            ("POST", "/api/v1/auth/login"), ("POST", "/api/v1/auth/activity"), ("GET", "/api/v1/auth/me")}
        navigation = reloading and (session or (request.method, item["path"]) in {
            ("GET", "/api/v1/events"), ("GET", "/api/v1/updates")})
        # Auth activity completes with 204; login/me consumers do not read
        # successful bodies. Chrome can report their completion as aborted.
        expected_status = {("POST", "/api/v1/auth/activity"): 204,
            ("POST", "/api/v1/auth/login"): 200, ("GET", "/api/v1/auth/me"): 200}
        unread_session = session and item["response_status"] == expected_status.get((request.method, item["path"]))
        expected = request.failure == "net::ERR_ABORTED" and (navigation or unread_session)
        if expected:
            item["reason"] = "explicit reload" if navigation else "completed session response (no content or unread body)"
        (navigation_aborts if expected else request_failures).append(item)

    def reload_page():
        nonlocal reloading
        reloading = True
        try:
            page.reload()
        finally:
            reloading = False

    page.on("requestfailed", record_transport)
    reload_page()
    detail = open_version(page, text, name, fixture.image_ref)
    row = detail.get_by_role("row").filter(has_text=fixture.node_name)
    expect(row).to_have_count(1)
    expect(row.get_by_text(text["state"]["absent"], exact=True)).to_be_visible()
    source = row.get_by_role("combobox", name=text["nodeTable"]["column"]["source"], exact=True)
    source.focus()
    source.press("ArrowDown")
    host = urlsplit(fixture.archive_url).netloc
    page.get_by_text(host, exact=True).filter(visible=True).click()
    expect(source).to_have_attribute("aria-expanded", "false")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/preview-archive-url" and r.request.method == "POST") as previewed:
        button(row, text["nodeTable"]["pullFromURL"]).click()
    title = text["nodeTable"]["archiveDialog"]["title"].replace("{{ref}}", fixture.image_ref).replace("{{node}}", fixture.node_name)
    dialog = page.get_by_role("dialog", name=title, exact=True)
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(dialog.get_by_label(text["import"]["url"]["label"], exact=True)).to_have_value(fixture.archive_url)
    preview = previewed.value
    check(preview.status == 200 and preview.request.post_data_json == {"url": fixture.archive_url}, "preview request differs")
    check(preview.json()["repo_tags"] == [fixture.image_ref] and preview.json()["size_bytes"] == fixture.archive_size,
          "preview did not read actual archive tags and size")
    expect(dialog.get_by_text(fixture.image_ref, exact=True)).to_be_visible()
    expect(dialog.get_by_role("checkbox", name=text["nodeTable"]["relay"], exact=True)).not_to_be_checked()
    path = f"/api/v1/images/{version_id}/nodes/{fixture.node_uuid}"
    preview_request_count = len(fixture.archive.snapshot())
    with page.expect_response(lambda r: urlsplit(r.url).path == path + "/sync" and r.request.method == "POST") as started:
        button(dialog, text["nodeTable"]["pull"]).click()
    response = started.value
    expected = {"source": "url", "archive_url": fixture.archive_url, "relay": False}
    check(response.status == 202 and urlsplit(response.url).query == "", "image acquisition was not accepted")
    check(response.request.post_data_json == expected, "archive acquisition body differs")
    transfer = response.json()
    check(transfer.get("transfer_id"), "transfer identity missing")
    expect(dialog).not_to_be_visible()
    present = wait_node_state(page, server, series_id, version_id, fixture.node_uuid, "present")
    check(present["node"]["source"] == "url" and present["node"]["image_digest_id"] == fixture.image_id,
          "agent reported a different source/content identity")
    check(present["version"]["present_nodes"] == 1, "completed image not counted exactly once")
    expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible(timeout=30000)
    reload_page()
    detail = open_version(page, text, name, fixture.image_ref)
    row = detail.get_by_role("row").filter(has_text=fixture.node_name)
    expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible()
    persisted = read_detail(page, server.base, series_id)
    saved_node = next(n for n in persisted["versions"][0]["nodes"] if n["node_uuid"] == fixture.node_uuid)
    check(saved_node["state"] == "present" and saved_node["image_digest_id"] == fixture.image_id, "reload lost runtime state")
    inspect = json.loads(fixture.docker(["image", "inspect", fixture.image_ref]).stdout)
    check(len(inspect) == 1 and inspect[0]["Id"] == fixture.image_id, "Docker content identity differs")
    check(inspect[0]["RepoTags"] == [fixture.image_ref], "Docker loaded wrong tag")
    # Consume the acquired image, with no network or implicit Registry fallback.
    executed = fixture.docker(["run", "--rm", "--network", "none", "--pull=never", fixture.image_ref])
    check(executed.stdout == fixture.marker + "\n", "real CPU container did not execute the acquired payload")
    check(fixture.docker(["ps", "-aq"]).stdout.strip() == "", "consumer container leaked")
    staging = fixture.agent_state / "images/gpuworks-image-archives"
    check(staging.is_dir() and not list(staging.iterdir()), "completed transfer left staged archive or sidecars")
    audit = fixture.archive.snapshot()[preview_request_count:]
    check(any(r["method"] == "GET" and r["range"] == "bytes=0-0" and r["status"] == 206 for r in audit),
          "agent download probe missing")
    ranges = []
    for request in audit:
        if request["method"] != "GET" or request["status"] not in (200, 206):
            continue
        if not request["range"]:
            check(request["bytes_sent"] == fixture.archive_size, "incomplete full archive transfer")
            ranges.append((0, fixture.archive_size - 1))
            continue
        match = re.fullmatch(r"bytes=(\d+)-(\d*)", request["range"])
        check(match is not None, "unexpected runtime range")
        start = int(match[1])
        end = min(int(match[2]) if match[2] else fixture.archive_size - 1, fixture.archive_size - 1)
        check(request["bytes_sent"] == end - start + 1, "incomplete archive range")
        ranges.append((start, end))
    next_byte = 0
    for start, end in sorted(ranges):
        check(start <= next_byte, "runtime archive download has a byte gap")
        next_byte = max(next_byte, end + 1)
    check(next_byte == fixture.archive_size, "runtime archive download did not cover full content")
    report["evidence"].extend([
        {"layer": "ui", "explicit_source": fixture.archive_url, "preview": preview.json(), "present_after_reload": True},
        {"layer": "request", "method": "POST", "path": path + "/sync", "status": 202, "body": expected, "response": transfer},
        {"layer": "persistence", "phase": "acquired", "detail": persisted},
        {"layer": "runtime", "docker_id": inspect[0]["Id"], "repo_tags": inspect[0]["RepoTags"],
         "staging_empty": True, "downloaded_range_coverage_bytes": next_byte, "container_exit": executed.returncode, "stdout": executed.stdout, "external_requests": audit}])
    # A named confirmation describes the node-local impact; cancellation cannot
    # remove bytes or change the authoritative node inventory.
    confirmation = text["nodeTable"]["removeConfirm"].replace("{{ref}}", fixture.image_ref).replace("{{node}}", fixture.node_name)
    # The delete control lives one step below the row, behind the expander: the
    # table row itself keeps to what the table is read for and the actions that
    # move bytes sit in the expanded area (ImageNodeTable.tsx:120-125). Opening it
    # is part of reaching the control, not a way of finding it by another name.
    expander = detail.get_by_role("button", name=text["nodeTable"]["expand"], exact=True)
    expect(expander).to_have_count(1)
    expander.click()
    # The table renders the expanded content once; assert on the control it
    # contains rather than on the expander's own attribute, which antd flips on
    # the re-rendered node.
    remove = detail.get_by_role("button", name=re.compile(r"^\s*Delete\s+image\s*$"))
    expect(remove).to_have_count(1)
    remove.click()
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    expect(popup).to_be_in_viewport(ratio=1)
    expect(popup.get_by_text(confirmation, exact=True)).to_be_visible()
    expect(popup.get_by_text(text["nodeTable"]["removeImpact"], exact=True)).to_be_visible()
    button(popup, text["common"]["cancel"]).click()
    expect(popup).not_to_be_visible()
    check(not any(m["method"] == "DELETE" for m in mutations), "cancel sent delete")
    check(fixture.docker(["image", "inspect", "--format", "{{.Id}}", fixture.image_ref]).stdout.strip() == fixture.image_id,
          "cancel removed the node image")
    check(wait_node_state(page, server, series_id, version_id, fixture.node_uuid, "present")["node"]["image_digest_id"] == fixture.image_id,
          "cancel altered node inventory")
    # The expanded area stays open across the cancelled confirmation, so the same
    # control is reachable again.
    detail.get_by_role("button", name=re.compile(r"^\s*Delete\s+image\s*$")).click()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as removed:
        button(popup, text["nodeTable"]["removeConfirmOk"]).click()
    result = removed.value
    check(result.status == 202 and result.request.post_data is None and urlsplit(result.url).query == "", "node remove request differs")
    check(result.json()["state"] == "absent", "node removal response differs")
    absent = wait_node_state(page, server, series_id, version_id, fixture.node_uuid, "absent")
    check(absent["version"]["id"] == version_id and absent["version"]["present_nodes"] == 0, "node deletion changed catalog identity or availability")
    check(fixture.docker(["image", "inspect", fixture.image_ref], check=False).returncode != 0, "deleted node image still exists")
    check(fixture.docker(["image", "ls", "-q"]).stdout.strip() == "", "deleted image layers retained as a tagged/dangling image")
    reload_page()
    detail = open_version(page, text, name, fixture.image_ref)
    row = detail.get_by_role("row").filter(has_text=fixture.node_name)
    expect(row.get_by_text(text["state"]["absent"], exact=True)).to_be_visible()
    check(staging.is_dir() and not list(staging.iterdir()), "removed image left archive staging")
    check(not errors and not failures and not request_failures, "unexpected browser/request error: " + str(request_failures))
    actions = [m for m in mutations if "/nodes/" in m["path"]]
    check(actions == [{"method": "POST", "path": path + "/sync"}, {"method": "DELETE", "path": path}], "duplicate/unexpected node action")
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations, "request_failed": request_failures, "expected_cancellations": navigation_aborts})
    report["evidence"].extend([
        {"layer": "ui", "cancel_no_delete": True, "named_impact": confirmation, "absent_after_reload": True},
        {"layer": "request", "method": "DELETE", "path": path, "status": result.status, "response": result.json()},
        {"layer": "persistence", "phase": "removed", "detail": absent["detail"]},
        {"layer": "runtime", "phase": "removed", "docker_images": [], "docker_containers": []}])
    cleanup(page, server.base, series_id, report)


if __name__ == "__main__":
    raise SystemExit(run({"acquire-remove": test_url_acquire_remove}, Path(__file__), extra_sources=[
        "ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImageNodeTable.tsx",
        "ui/src/pages/images/ImageArchiveURLDialog.tsx", "ui/src/i18n/locales/en/images.json",
        "ui/src/i18n/locales/zh/images.json", "internal/agent/app/image_load_url.go",
        "internal/server/hub/image_remove.go", "internal/server/api/images_nodes.go"]))
