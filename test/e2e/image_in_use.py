#!/usr/bin/env python3
"""Refuse UI deletion of a real in-use CPU image, then remove it after release.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-image-in-use-remove
After building: python3 test/e2e/image_in_use.py --case in-use-remove --locale en
Use --locale zh; --server-bin/--agent-bin/--artifact-dir are optional.
Requires Linux, Chrome, Playwright, sudo -n, unshare, Docker and static busybox.
Own real server/SQLite, CPU agent, Docker socket/data-root and HTTP archive.
The retained consumer is a fixture container, not a managed GPU deployment.
Failed scene and trace remain private under secrets/e2e/<runId> for seven days.
"""
from pathlib import Path
from urllib.parse import urlsplit
import json
import re

from playwright.sync_api import expect
from image_catalog import button, cleanup, read_detail, strings
from image_runtime_runner import run
from image_transfer_recovery import seed, read_state, open_row, observe_navigation
from platform_token_read_revoke import check


# Contract: gpuworks/image-runtime/in-use-remove
# Mainline: 自有CPU agent实际获取归档，真实容器占用镜像 → UI确认删除被Docker拒绝409并显示对象/原因 → 独立GET和刷新仍present、digest与容器PID/启动时间不变且exec可用 → 释放自有消费者后UI再次删除 → GET/刷新absent且Docker镜像消失，目录版本保留 → 清理自有资源。
def open_row_actions(page, row, text):
    """The row's inspect, repull and remove live in its expanded detail, which the table renders
    outside the row element, so the action is reached through the container itself."""
    expand = row.locator(".detail-expand").first
    if expand.get_attribute("aria-expanded") != "true":
        expand.click()
    scope = page.locator(".detail-extra-actions").filter(has_text=text["nodeTable"]["remove"])
    expect(scope).to_have_count(1)
    return scope


def test_in_use_remove(page, server, fixture, artifacts, report, locale):
    text = strings(locale)
    name, series, version = seed(page, server, fixture, report)
    path = f"/api/v1/images/{version}/nodes/{fixture.node_uuid}"
    # Acquisition is a fixture precondition. The operation under test is UI
    # deletion while Docker, rather than the catalog, owns a live reference.
    read_state(page, series, version, fixture.node_uuid, "absent")
    response = page.request.post(server.base + path + "/sync", data={
        "source": "url", "archive_url": fixture.archive_url, "relay": False})
    check(response.status == 202 and response.json().get("transfer_id"), "fixture image acquisition failed")
    before = read_state(page, series, version, fixture.node_uuid, "present")
    check(before["node"]["image_digest_id"] == fixture.image_id and before["version"]["present_nodes"] == 1,
          "fixture image content or availability differs")
    image_before = json.loads(fixture.docker(["image", "inspect", fixture.image_ref]).stdout)[0]
    check(image_before["Id"] == fixture.image_id and image_before["RepoTags"] == [fixture.image_ref], "fixture Docker image differs")
    consumer_name = "e2e-" + report["run_id"] + "-consumer"
    container_id = fixture.docker(["run", "-d", "--network", "none", "--pull=never", "--name", consumer_name,
        "--entrypoint", "/bin/busybox", fixture.image_ref, "tail", "-f", "/dev/null"]).stdout.strip()
    check(re.fullmatch(r"[0-9a-f]{64}", container_id) is not None, "fixture consumer did not start")
    container_before = json.loads(fixture.docker(["container", "inspect", container_id]).stdout)[0]
    check(container_before["State"]["Running"] and container_before["Image"] == fixture.image_id, "consumer not running from owned image")
    check(fixture.docker(["exec", container_id, "/bin/busybox", "printf", fixture.marker + "\\n"]).stdout == fixture.marker + "\n",
          "consumer did not execute acquired archive payload")
    report["fixture_consumer"] = {"id": container_id, "name": consumer_name, "image_id": fixture.image_id,
        "pid": container_before["State"]["Pid"], "started_at": container_before["State"]["StartedAt"]}
    errors, console_errors, http_failures, mutations = [], [], [], []
    delete_responses = []
    transport_failures, navigation_aborts, reload_page = observe_navigation(page)
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("console", lambda message: console_errors.append({"text": message.text, "url": message.location.get("url", "")})
        if message.type == "error" else None)
    def observe_response(response):
        request_path = urlsplit(response.url).path
        if response.status >= 400:
            http_failures.append({"method": response.request.method, "path": request_path, "status": response.status})
        if response.request.method == "DELETE" and request_path == path:
            delete_responses.append(response)

    def version_refresh_after(response, mutation_number):
        if response.request.method != "GET" or urlsplit(response.url).path != "/api/v1/images/versions":
            return False
        if len(delete_responses) < mutation_number:
            return False
        timing = delete_responses[mutation_number - 1].request.timing
        # The page starts versions.refetch after its DELETE and detail refresh.
        # Exclude any initial/periodic request begun before this mutation reply.
        return timing["responseStart"] >= 0 and response.request.timing["startTime"] >= timing["startTime"] + timing["responseStart"]

    page.on("response", observe_response)
    page.on("request", lambda request: mutations.append({"method": request.method, "path": urlsplit(request.url).path})
        if request.method in {"POST", "PATCH", "PUT", "DELETE"} and urlsplit(request.url).path.startswith(("/api/v1/images", "/api/v1/image-series")) else None)
    report["browser_observations"] = {"errors": errors, "console_errors": console_errors, "http_failures": http_failures,
        "transport_failures": transport_failures, "navigation_aborts": navigation_aborts, "mutations": mutations}
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible()
    confirmation = text["nodeTable"]["removeConfirm"].replace("{{ref}}", fixture.image_ref).replace("{{node}}", fixture.node_name)
    open_row_actions(page, row, text).get_by_role("button", name=text["nodeTable"]["remove"], exact=True).click()
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    expect(popup).to_be_in_viewport(ratio=1)
    expect(popup.get_by_text(confirmation, exact=True)).to_be_visible()
    expect(popup.get_by_text(text["nodeTable"]["removeImpact"], exact=True)).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as refused, \
            page.expect_response(lambda r: version_refresh_after(r, 1)) as refusal_refresh:
        button(popup, text["nodeTable"]["removeConfirmOk"]).click()
    rejected = refused.value
    check(rejected.status == 409 and rejected.request.post_data is None and not urlsplit(rejected.url).query,
          "in-use removal must be rejected by actual runtime with HTTP409")
    reason = rejected.json().get("error", "")
    check("conflict" in reason.lower() and "container" in reason.lower() and container_id[:12] in reason,
          "runtime refusal omitted actual referencing container identity")
    check(fixture.image_ref in reason, "refusal omitted target image")
    expect(page.get_by_text(reason, exact=True)).to_be_visible()
    expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible()
    refreshed_present = refusal_refresh.value
    check(refreshed_present.status == 200 and not urlsplit(refreshed_present.url).query and refreshed_present.finished() is None,
          "post-refusal version refresh did not finish successfully")
    refreshed_present_version = next(v for v in refreshed_present.json()["items"] if v["id"] == version)
    check(refreshed_present_version["image_ref"] == fixture.image_ref and refreshed_present_version["present_nodes"] == 1,
          "post-refusal version refresh lost the protected image")
    unchanged = read_state(page, series, version, fixture.node_uuid, "present")
    for key in ("state", "source", "image_digest_id", "transfer_id"):
        check(unchanged["node"].get(key) == before["node"].get(key), f"refused deletion partially changed node {key}")
    check(unchanged["version"]["present_nodes"] == 1, "refused deletion lost available replica")
    image_after = json.loads(fixture.docker(["image", "inspect", fixture.image_ref]).stdout)[0]
    check(all(image_after[key] == image_before[key] for key in ("Id", "RepoTags", "RootFS", "Config")), "refused deletion modified image")
    container_after = json.loads(fixture.docker(["container", "inspect", container_id]).stdout)[0]
    check(container_after["State"]["Running"] and container_after["Image"] == fixture.image_id,
          "refused deletion interrupted the consumer")
    check(all(container_after["State"][key] == container_before["State"][key] for key in ("Pid", "StartedAt")),
          "consumer restarted during refused deletion")
    executed = fixture.docker(["exec", container_id, "/bin/busybox", "printf", fixture.marker + "\\n"])
    check(executed.stdout == fixture.marker + "\n", "consumer became unusable after refused deletion")
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible()
    persisted = read_detail(page, server.base, series)
    saved_version = next(v for v in persisted["versions"] if v["id"] == version)
    saved_node = next(n for n in saved_version["nodes"] if n["node_uuid"] == fixture.node_uuid)
    check(saved_node["state"] == "present" and saved_node["image_digest_id"] == fixture.image_id, "reload lost protected image")
    report["evidence"].extend([
        {"layer": "ui", "refused_delete_reason": reason, "named_impact": confirmation, "present_after_reload": True},
        {"layer": "request", "method": "DELETE", "path": path, "status": rejected.status, "body": None,
         "response": rejected.json(), "request_id": rejected.headers.get("x-request-id")},
        {"layer": "request", "phase": "post_refusal_refresh", "method": "GET", "path": "/api/v1/images/versions",
         "status": refreshed_present.status, "finished": True, "target": refreshed_present_version},
        {"layer": "persistence", "phase": "refused", "detail": persisted},
        {"layer": "runtime", "phase": "refused", "image_id": image_after["Id"], "container_id": container_id,
         "pid_unchanged": container_after["State"]["Pid"], "started_at_unchanged": container_after["State"]["StartedAt"],
         "exec_stdout": executed.stdout, "exec_exit": executed.returncode}])
    # Release only the fixture-owned consumer. This is not a claim that the UI
    # stopped a managed deployment; the second tested operation remains Delete.
    fixture.docker(["stop", "--time", "1", container_id])
    fixture.docker(["container", "rm", container_id])
    check(fixture.docker(["container", "inspect", container_id], check=False).returncode != 0, "fixture consumer was not released")
    check(fixture.docker(["image", "inspect", "--format", "{{.Id}}", fixture.image_ref]).stdout.strip() == fixture.image_id,
          "releasing consumer unexpectedly removed image before UI retry")
    open_row_actions(page, row, text).get_by_role("button", name=text["nodeTable"]["remove"], exact=True).click()
    expect(popup).to_be_in_viewport(ratio=1)
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as removed, \
            page.expect_response(lambda r: version_refresh_after(r, 2)) as removal_refresh:
        button(popup, text["nodeTable"]["removeConfirmOk"]).click()
    success = removed.value
    check(success.status == 202 and success.request.post_data is None and not urlsplit(success.url).query, "delete after release request differs")
    check(success.json()["state"] == "absent", "successful delete response was not absent")
    # Complete the product's post-mutation read before navigating away. The
    # transport gate below still rejects every failed image/business request.
    refreshed_absent = removal_refresh.value
    check(refreshed_absent.status == 200 and not urlsplit(refreshed_absent.url).query and refreshed_absent.finished() is None,
          "post-removal version refresh did not finish successfully")
    refreshed_absent_version = next(v for v in refreshed_absent.json()["items"] if v["id"] == version)
    check(refreshed_absent_version["image_ref"] == fixture.image_ref and refreshed_absent_version["present_nodes"] == 0,
          "post-removal version refresh retained the removed replica")
    expect(row.get_by_text(text["state"]["absent"], exact=True)).to_be_visible()
    absent = read_state(page, series, version, fixture.node_uuid, "absent")
    check(absent["version"]["present_nodes"] == 0 and absent["version"]["image_ref"] == fixture.image_ref,
          "deletion changed catalog identity or retained availability")
    check(fixture.docker(["image", "inspect", fixture.image_ref], check=False).returncode != 0, "deleted image remains in Docker")
    check(fixture.docker(["image", "ls", "-q"]).stdout.strip() == "" and fixture.docker(["ps", "-aq"]).stdout.strip() == "",
          "owned image or consumer leaked after successful deletion")
    reload_page()
    row = open_row(page, text, name, fixture)
    expect(row.get_by_text(text["state"]["absent"], exact=True)).to_be_visible()
    persisted_removed = read_detail(page, server.base, series)
    saved_version = next(v for v in persisted_removed["versions"] if v["id"] == version)
    saved_node = next(n for n in saved_version["nodes"] if n["node_uuid"] == fixture.node_uuid)
    check(saved_node["state"] == "absent" and saved_version["present_nodes"] == 0, "reload resurrected removed image")
    check(http_failures == [{"method": "DELETE", "path": path, "status": 409}], "unexpected HTTP failure")
    check(not errors and not transport_failures, "unexpected browser exception/transport error")
    check(all(e["url"] == rejected.url and re.fullmatch(r"Failed to load resource: the server responded with a status of 409 \(Conflict\)", e["text"])
              for e in console_errors), "unexpected browser console error")
    check(mutations == [{"method": "DELETE", "path": path}, {"method": "DELETE", "path": path}], "duplicate/unexpected image mutation")
    report["evidence"].extend([
        {"layer": "ui", "phase": "released_then_removed", "absent_after_reload": True},
        {"layer": "request", "method": "DELETE", "path": path, "status": success.status, "body": None, "response": success.json()},
        {"layer": "request", "phase": "post_removal_refresh", "method": "GET", "path": "/api/v1/images/versions",
         "status": refreshed_absent.status, "finished": True, "target": refreshed_absent_version},
        {"layer": "persistence", "phase": "removed", "detail": persisted_removed},
        {"layer": "runtime", "phase": "removed", "images": [], "containers": []}])
    artifacts.write_json("browser-observations.json", report["browser_observations"])
    cleanup(page, server.base, series, report)


if __name__ == "__main__":
    raise SystemExit(run({"in-use-remove": test_in_use_remove}, Path(__file__), extra_sources=[
        "test/e2e/image_transfer_recovery.py", "ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImageNodeTable.tsx",
        "ui/src/i18n/locales/en/images.json", "ui/src/i18n/locales/zh/images.json", "internal/agent/runtime/docker/docker.go",
        "internal/server/hub/image_remove.go", "internal/server/api/images_nodes.go",
        "internal/agent/conn/conn.go", "internal/agent/conn/status_image_order.go", "internal/agent/app/control.go",
        "internal/server/hub/status_image_order.go", "internal/server/hub/statusqueue.go"]))
