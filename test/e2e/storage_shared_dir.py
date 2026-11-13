#!/usr/bin/env python3
"""Shared-directory storage: trial mount, create, scan, read-only switch and delete.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-storage-shared-dir
After build: python3 test/e2e/storage_shared_dir.py --locale en
Requires Python Playwright, system Chrome, a real agent and passwordless sudo
for one bind mount. The bind mount is a private directory that stands in for an
already-mounted shared filesystem (CephFS/Lustre/NFS); the agent probes and
scans the real mount, and deleting the storage must leave its files alone.
Artifacts: secrets/e2e/<runId>; the runner stops its own server/agent and
retains the private database/logs for scoped diagnosis (seven days).
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
from platform_token_read_revoke import button, check
from storage_shared_fixture import NODES, STORAGES, cleanup as cleanup_resources

CONTRACT = "gpuworks/storage/shared-dir-trial-mount-lifecycle"
CASE = "存储 / 已挂载共享目录 / 试挂、创建、扫描、只读切换与删除保留文件"
PROBE = STORAGES + "/probe"
MODEL_DIR = ROOT / "test/e2e/testdata/fake-model"
LANE = "local-real-server-agent-bind-mounted-shared-dir"


def strings(locale):
    storage = json.loads((ROOT / "ui/src/i18n/locales" / locale / "storage.json").read_text())
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return storage, common, auth


def name_pattern(text):
    """Role-name regex that tolerates AntD's spacing of two Chinese characters."""
    chars = r"\s*".join(re.escape(char) for char in text)
    return re.compile("^" + chars + "$")


def wait_online(base, token, node_uuid, agent):
    deadline = time.time() + 40
    while time.time() < deadline:
        status, node = http(base, "GET", NODES + f"/{node_uuid}", token=token)
        if status == 200 and node.get("state") == "online":
            return node
        if agent.poll() is not None:
            raise AssertionError("the agent exited before the node came online")
        time.sleep(0.5)
    raise AssertionError("the node never came online")


def wait_json(page, server, path, predicate, timeout=40, label="storage"):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        response = page.request.get(server.base + path)
        if response.status == 404:
            last = "404"
        elif response.status == 200:
            last = response.json()
            if predicate(last):
                return last
        time.sleep(0.5)
    raise AssertionError(f"{label} stayed {last}")


def sudo(*argv):
    result = subprocess.run(["sudo", "-n", *argv], capture_output=True, text=True)
    return result.returncode, (result.stderr or result.stdout).strip()


def mounted_at(path):
    """Whether mountinfo lists exactly this path.

    os.path.ismount cannot see a bind mount whose source is on the same
    filesystem, which is what the fixture uses.
    """
    target = str(Path(path).resolve())
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        fields = line.split(" ")
        if len(fields) > 4 and fields[4] == target:
            return True
    return False


def mount_shared(src, mnt):
    src.mkdir(mode=0o700, exist_ok=True)
    mnt.mkdir(mode=0o700, exist_ok=True)
    code, output = sudo("mount", "--bind", str(src), str(mnt))
    check(code == 0, f"bind mount failed: {output}")
    check(mounted_at(mnt), f"{mnt} is not listed as a mount point after mount --bind")


def unmount_shared(mnt):
    if not mounted_at(mnt):
        return True
    code, output = sudo("umount", str(mnt))
    return code == 0 and not mounted_at(mnt)


# Contract: gpuworks/storage/shared-dir-trial-mount-lifecycle
# Mainline: 独立 server/SQLite + 真实 agent + 私有 bind mount（代替已挂载共享目录）
#           → UI 选择共享目录类型/真实节点/路径并试挂 → 创建后 agent 上报 ready 与容量
#           → UI 扫描真实目录得到模型候选 → UI 关闭再开启（只读/可写）
#           → UI 删除后存储 404 且共享目录里的文件不被删除 → 停止 agent、删除节点、卸载
def test_shared_dir_lifecycle(page, context, server, artifacts, report, locale,
                              node, mount_path, model_rel):
    storage, common, auth = strings(locale)
    form, probe_text, scope = storage["form"], storage["probe"], storage["form"]["scope"]
    actions, scan_text = storage["actions"], storage["scan"]
    name = report["storage_name"]
    page_errors, console_errors, bad_responses = [], [], []
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

    page.on("console", console_seen)
    page.on("response", response_seen)

    page.goto(server.base + "/storage")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=storage["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "storage-list-open"

    # The add button carries a plus icon whose label prefixes the accessible name.
    page.get_by_role("button", name=re.compile(re.escape(storage["page"]["add"]) + "$")).click()
    modal = page.get_by_role("dialog").last
    expect(modal.get_by_text(form["addTitle"], exact=True)).to_be_visible()
    # AntD hides the radio input behind its label; click the visible label text.
    modal.get_by_text(common["storageType"]["shared_dir"], exact=True).click()
    expect(modal.get_by_label(form["name"], exact=True)).to_be_visible()
    expect(modal.get_by_text(form["sharedPathHelp"], exact=True)).to_be_visible()
    expect(modal.get_by_label(form["export"], exact=True)).to_have_count(0)
    modal.get_by_label(form["name"], exact=True).fill(name)
    modal.get_by_label(form["sharedPath"], exact=True).fill(str(mount_path))
    modal.get_by_text(scope["nodes"], exact=True).click()
    picker = modal.get_by_role("combobox", name=scope["chooseNodes"])
    picker.click()
    option = page.locator(
        f'.ant-select-dropdown:not(.ant-select-dropdown-hidden) '
        f'.ant-select-item-option[title="{node["name"]}"]')
    expect(option).to_be_visible()
    option.click()
    expect(modal.locator(".ant-select-selection-item")).to_have_text(node["name"])
    report["last_step"] = "storage-form-filled"

    with page.expect_response(lambda r: urlsplit(r.url).path == PROBE
                              and r.request.method == "POST") as probed:
        button(modal, form["probe"]).click()
    probe = probed.value
    check(probe.status == 200, f"probe HTTP {probe.status}")
    probe_body = probe.request.post_data_json
    check(probe_body == {"type": "shared_dir", "path": str(mount_path),
                         "node_uuid": node["uuid"]},
          f"unexpected probe request: {probe_body}")
    probe_result = probe.json()
    check(probe_result["ok"] and probe_result["readable"] and probe_result["writable"],
          f"probe did not find a writable mount: {probe_result}")
    check(probe_result["node_uuid"] == node["uuid"], "the probe ran on another node")
    expect(modal.get_by_text(probe_text["ok"].replace("{{node}}", node["name"]),
                             exact=True)).to_be_visible()
    expect(modal.get_by_text(probe_text["readable"], exact=True)).to_be_visible()
    expect(modal.get_by_text(probe_text["writable"], exact=True)).to_be_visible()
    expect(modal.get_by_text(probe_text["capacity"], exact=True)).to_be_visible()
    report["last_step"] = "storage-probe-passed"

    save = button(modal, form["save"])
    expect(save).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as created:
        save.click()
    response = created.value
    check(response.status == 201, f"create HTTP {response.status} {response.text()[:300]}")
    body = response.request.post_data_json
    check(body == {"name": name, "type": "shared_dir", "all_nodes": False,
                   "targets": [{"node_uuid": node["uuid"]}],
                   "path": str(mount_path), "probe_node": node["uuid"]},
          f"unexpected create request: {body}")
    doc = response.json()
    storage_id = doc["id"]
    report["owned_storage_id"] = storage_id
    expect(page.locator(".ant-message").get_by_text(
        form["created"].replace("{{name}}", name), exact=True)).to_be_visible()
    # The detail is a drawer over the list now, and which one is open lives in the list page's own
    # query string (?storage=<id>), not in a /storage/<id> route. Asserting the old route failed on a
    # product that had already moved on: the create itself was answering 201 with the exact body.
    expect(page).to_have_url(re.compile(r"\?storage=" + str(storage_id) + r"(?:&|$)"))
    # The create opens the detail as a drawer over the list; it is identified by its own surface
    # class rather than by a translated title, so the check holds under both locales.
    drawer = page.locator(".ant-drawer-content").filter(has_text=name)
    expect(drawer).to_have_count(1)
    expect(drawer.get_by_text(name, exact=False).first).to_be_visible()
    report["last_step"] = "storage-created"

    ready = wait_json(page, server, STORAGES + f"/{storage_id}?all_nodes=1",
                      lambda d: d.get("nodes", {}).get("ready") == 1
                      and d.get("writable") is True, label="ready storage")
    check(ready["type"] == "shared_dir" and ready["path"] == str(mount_path)
          and ready["enabled"] is True and ready["all_nodes"] is False,
          f"stored definition differs: {ready}")
    check(ready["targets"] == [{"node_uuid": node["uuid"], "node_name": node["name"]}],
          f"selected node not stored: {ready['targets']}")
    node_state = ready["nodes"]["all"][0]
    check(node_state["state"] == "ready" and node_state["writable"] is True,
          f"agent did not report the mount ready: {node_state}")
    # The probe's own measurement is the capacity the agent found.
    check(probe_result.get("total_bytes", 0) > 0
          and probe_result.get("available_bytes", 0) >= 0,
          f"agent reported no capacity in the probe: {probe_result}")
    report["last_step"] = "storage-ready-on-agent"

    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES + f"/{storage_id}/scan"
                              and r.request.method == "POST") as scanned:
        button(page, actions["scan"]).click()
    scan = scanned.value
    check(scan.status == 200, f"scan HTTP {scan.status}")
    scan_result = scan.json()
    paths = [candidate.get("rel_path") for candidate in scan_result.get("candidates") or []]
    check(model_rel in paths, f"the agent did not scan {model_rel}: {paths}")
    # The detail is a drawer, which also answers to role=dialog; the scan result is the modal that sits
    # above it. Binding to .last picked whichever came last in the DOM and closed the wrong surface.
    scan_modal = page.locator(".ant-modal").filter(has=page.get_by_text(
        scan_text["found"].replace("{{count}}", "1"), exact=True))
    expect(scan_modal).to_have_count(1)
    expect(scan_modal.get_by_text(model_rel, exact=True)).to_be_visible(timeout=15000)
    expect(scan_modal.get_by_text(scan_text["found"].replace("{{count}}", "1"),
                                  exact=True)).to_be_visible()
    # The header X shares the name "Close"; the footer's first button is the
    # dialog's own close action.
    scan_modal.locator(".ant-modal-footer button").first.click()
    expect(scan_modal).to_be_hidden()
    report["last_step"] = "storage-scanned"

    enabled_switch = page.get_by_role("switch", name=re.compile(re.escape(
        actions["enabledSwitch"].replace("{{name}}", name))))
    expect(enabled_switch).to_be_checked()
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES + f"/{storage_id}"
                              and r.request.method == "PATCH") as disabled:
        enabled_switch.click()
    check(disabled.value.status == 200, f"disable HTTP {disabled.value.status}")
    check(disabled.value.request.post_data_json == {"enabled": False},
          f"unexpected disable request: {disabled.value.request.post_data_json}")
    expect(page.locator(".ant-message").get_by_text(
        actions["disabledMessage"].replace("{{name}}", name), exact=True)).to_be_visible()
    expect(enabled_switch).not_to_be_checked()
    check(wait_json(page, server, STORAGES + f"/{storage_id}?all_nodes=1",
                    lambda d: d.get("enabled") is False, label="disabled storage")["enabled"] is False,
          "the disabled state did not persist")
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES + f"/{storage_id}"
                              and r.request.method == "PATCH") as enabled:
        enabled_switch.click()
    check(enabled.value.status == 200, f"enable HTTP {enabled.value.status}")
    check(enabled.value.request.post_data_json == {"enabled": True},
          f"unexpected enable request: {enabled.value.request.post_data_json}")
    expect(enabled_switch).to_be_checked()
    check(wait_json(page, server, STORAGES + f"/{storage_id}?all_nodes=1",
                    lambda d: d.get("enabled") is True, label="enabled storage")["enabled"] is True,
          "the enabled state did not persist")
    report["last_step"] = "storage-read-only-roundtrip"

    # The more button keeps its icon after the text in the accessible name.
    more = page.get_by_role("button", name=re.compile("^" + re.escape(actions["more"])))
    more.click()
    page.get_by_role("menuitem", name=name_pattern(actions["delete"])).click()
    confirm = page.locator(".ant-modal-confirm")
    expect(confirm).to_be_visible()
    expect(confirm.locator(".ant-modal-confirm-title")).to_contain_text(
        actions["deleteConfirm"].replace("{{name}}", name))
    expect(confirm.locator(".ant-modal-confirm-content")).to_contain_text(
        actions["deleteSharedHelp"])
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES + f"/{storage_id}"
                              and r.request.method == "DELETE") as deleted:
        confirm.locator(".ant-modal-confirm-btns .ant-btn-primary").click()
    check(deleted.value.status == 204, f"delete HTTP {deleted.value.status}")
    expect(page.locator(".ant-message").get_by_text(
        actions["deleted"].replace("{{name}}", name), exact=True)).to_be_visible()
    expect(page).to_have_url(re.compile(r"/storage$"))
    expect(page.get_by_role("row").filter(has_text=name)).to_have_count(0, timeout=15000)
    check(page.request.get(server.base + STORAGES + f"/{storage_id}").status == 404,
          "the deleted storage is still readable")
    report["owned_storage_id"] = None
    check((mount_path / model_rel / "config.json").is_file(),
          "deleting the storage removed files from the shared directory")
    check(mounted_at(mount_path), "deleting the storage unmounted the shared directory")
    report["last_step"] = "storage-deleted-files-kept"

    expected_404 = {"method": "GET", "path": STORAGES + f"/{storage_id}", "status": 404}
    unexpected = [item for item in bad_responses if item != expected_404]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/storage", "add": storage["page"]["add"],
         "type": common["storageType"]["shared_dir"], "name": name,
         "path": str(mount_path), "scope": scope["nodes"], "node": node["name"],
         "probe": form["probe"], "save": form["save"], "scan": actions["scan"],
         "switch": actions["enabledSwitch"].replace("{{name}}", name),
         "delete": actions["delete"]},
        {"layer": "request",
         "probe": {"path": PROBE, "status": probe.status, "body": probe_body},
         "create": {"path": STORAGES, "status": response.status, "body": body},
         "disable": {"status": disabled.value.status,
                     "body": disabled.value.request.post_data_json},
         "enable": {"status": enabled.value.status,
                    "body": enabled.value.request.post_data_json},
         "scan": {"path": STORAGES + f"/{storage_id}/scan", "status": scan.status},
         "delete": {"path": STORAGES + f"/{storage_id}", "status": deleted.value.status}},
        {"layer": "persistence",
         "created": {"type": ready["type"], "path": ready["path"],
                     "enabled": ready["enabled"], "all_nodes": ready["all_nodes"],
                     "targets": ready["targets"], "nodes": ready["nodes"]},
         "read_only": False, "re_enabled": True, "after_delete": 404},
        {"layer": "execution",
         "probe": {"node": probe_result["node_uuid"], "ok": probe_result["ok"],
                   "readable": probe_result["readable"],
                   "writable": probe_result["writable"],
                   "total_bytes": probe_result.get("total_bytes"),
                   "available_bytes": probe_result.get("available_bytes")},
         "scan_candidates": paths, "model": model_rel,
         "files_kept_after_delete": True, "mount_kept_after_delete": True},
        {"layer": "cleanup", "storage_deleted": True, "files_kept": True},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}


def create_node(server, token, run_id, report, holder, agent_bin,
                inject_prepare_failure=False):
    status, created = http(server.base, "POST", NODES, token=token,
                           body={"name": f"e2e-{run_id}-shared-node",
                                 "server_url": server.base})
    check(status == 201, f"node fixture HTTP {status}")
    node, node_token = created["node"], created["token"]
    report["owned_node_uuid"] = node["uuid"]
    state = server.work / "agent-state"
    state.mkdir()
    log = (server.work / "agent.log").open("w")
    holder["log"] = log
    env = {**os.environ, "GPUWORKS_FAKE_GPU": "A100x1,24576"}
    agent = subprocess.Popen([str(agent_bin), "run",
                              "--server", server.base, "--token", node_token,
                              "--state", str(state), "--driver", "none"],
                             stdout=log, stderr=subprocess.STDOUT, text=True, env=env)
    holder["agent"], holder["log"] = agent, log
    wait_online(server.base, token, node["uuid"], agent)
    if inject_prepare_failure:
        # Self-test: the agent is already running and owned when preparation
        # fails; the caller must still learn about both.
        raise RuntimeError("injected fixture preparation failure")
    return node, node_token


def cleanup(server, token, report, holder, mount_path, inject_timeout=False,
            inject_node_delete_timeout=False):
    return cleanup_resources(server, token, report, holder, mount_path,
                             inject_timeout, inject_node_delete_timeout,
                             http=http, unmount_shared=unmount_shared)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--inject-prepare-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-cleanup-timeout", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-node-delete-timeout", action="store_true",
                        help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    code, output = sudo("true")
    check(code == 0, f"passwordless sudo is required for the bind-mount fixture: {output}")
    check(shutil.which("mount") is not None and shutil.which("umount") is not None,
          "mount/umount are required")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    slug = run_id.replace("T", "-").replace("Z", "-").lower()
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/storage-shared-dir.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_shared_dir_lifecycle.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": LANE, "identity": "admin browser / node agent / private bind mount",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0,
        "storage_name": f"e2e-{slug}-shared"[:63],
        "model_dir": f"e2e-{slug}-model",
        "evidence": [], "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "real agent over WS probing and scanning a private bind-mounted directory; real server/SQLite",
        "runtime": "the bind mount stands in for an already-mounted shared filesystem; no network filesystem, GPU or container is used",
        "not_covered": ["real CephFS/Lustre/NFS backing and its failure modes",
                        "storage edit (name/path/targets) and node overrides",
                        "model import from scan candidates and downloads into the storage",
                        "reference-protected deletion while a deployment uses the storage",
                        "read-only enforcement against a real download",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/storage_shared_dir.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    token = None
    mount_path = server.storage_root / "shared-mnt"
    source_path = server.storage_root / "shared-src"
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["server_environment"] = server.environment
        source_path.mkdir(mode=0o700)
        model_dir = source_path / report["model_dir"]
        shutil.copytree(MODEL_DIR, model_dir)
        mount_shared(source_path, mount_path)
        report["mount"] = {"source": str(source_path), "target": str(mount_path),
                           "mountinfo": next(line for line in
                                             Path("/proc/self/mountinfo").read_text().splitlines()
                                             if f" {mount_path} " in line)}
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "gpuworks-test-password-2026"})
        check(status == 200, f"fixture login HTTP {status}")
        token = login["token"]
        report["preflight"] = "PASS"
        node, node_token = create_node(server, token, run_id, report, holder,
                                       agent_bin,
                                       inject_prepare_failure=args.inject_prepare_failure)
        artifacts.remember(token)
        artifacts.remember(node_token)
        artifacts.write_json("report.json", report)
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_shared_dir_lifecycle(page, context, server, artifacts, report,
                                          args.locale, node, mount_path, report["model_dir"])
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                close_ok = True
                try:
                    context.close()
                except Exception:
                    close_ok = False
                    report["context_close_error"] = artifacts.redact(
                        traceback.format_exc())
                try:
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
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = cleanup(server, token, report, holder, mount_path,
                                 inject_timeout=args.inject_cleanup_timeout,
                                 inject_node_delete_timeout=args.inject_node_delete_timeout)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
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
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            (artifacts.directory / "agent.log").write_text(artifacts.redact(agent_log_path.read_text()))
        residual = report.get("remaining_resources") or []
        if (residual or not report.get("server_stopped")
                or not report.get("agent_stopped", True)
                or report.get("mount_unmounted") is False):
            listed = "; ".join(
                f"{item['kind']}:{item.get('id')} ({item['state']}; {item['reclaim']})"
                for item in residual) or "server, agent or mount not confirmed stopped"
            report["private_state"] = (
                "private/ retains isolated DB/logs; residual resources: " + listed
                + "; use the scoped reclaim entries; remove only this run directory "
                  "after diagnosis (7 days)")
        else:
            report["private_state"] = (
                "private/ retains isolated DB/logs and agent state; no process or mount "
                "kept; remove only this run directory after diagnosis (7 days)")
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
