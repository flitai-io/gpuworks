#!/usr/bin/env python3
"""Deployment replica scale and targeted restart through real browser controls.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-deployment-replicas REPLICA_CASE=scale LOCALE=en
After build: python3 test/e2e/deployment_replicas.py --case scale --locale en
Cases: scale, drain, immediate (independent server/DB/agent per invocation).
Requires explicit DOCKER_HOST for a dedicated local unix-socket Docker daemon
(shared default socket is refused), Chrome, Playwright and gpuworks/fake-engine:test with
STREAM_CHUNKS support. Synthetic A100x2 facts enable scheduling only; drain fills both whole-card
claims to prevent surge. Real
containers use host networking; no GPU compute or vendor isolation claimed.
Artifacts secrets/e2e/<runId> retain private DB/logs for seven days.
"""
import argparse
from datetime import datetime, timezone
import json
import os
import re
from pathlib import Path
from urllib.parse import urlsplit
import shutil
import subprocess
import sys
import traceback
import uuid
from playwright.sync_api import sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import check
import deployment_container_fixture as container_fixture
import deployment_replica_fixture as replica_fixture
from deployment_replica_browser import test_replicas
from deployment_replica_checks import FAMILIES
IMAGE = "gpuworks/fake-engine:test"

def require_dedicated_daemon(host, context=""):
    check(not context, "DOCKER_CONTEXT must be empty so it cannot override the dedicated DOCKER_HOST")
    check(isinstance(host, str) and bool(host.strip()), "explicit dedicated DOCKER_HOST is required")
    parsed = urlsplit(host)
    path = parsed.path if parsed.scheme == "unix" else host
    defaults = {os.path.realpath(value) for value in ("/var/run/docker.sock", "/run/docker.sock")}
    check(os.path.realpath(path) not in defaults,
          "the shared default Docker daemon is forbidden for this lane")
    check(parsed.scheme == "unix" and bool(parsed.path), "this local lane requires a dedicated unix Docker socket")

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--case", choices=["scale", "drain", "immediate"], required=True)
    parser.add_argument("--mutation", help="Product mutation label recorded in evidence")
    parser.add_argument("--check-family", choices=FAMILIES, default="all",
                        help="Isolate new checks for mutation diagnostics; formal en/zh must use all")
    args = parser.parse_args()
    require_dedicated_daemon(os.environ.get("DOCKER_HOST"), os.environ.get("DOCKER_CONTEXT", ""))
    os.umask(0o077)
    check(shutil.which("docker") is not None, "docker is required for this lane")
    check(subprocess.run(["docker", "image", "inspect", IMAGE],
                         capture_output=True, timeout=20).returncode == 0,
          f"{IMAGE} is missing; run make fake-engine-image")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    contract = "gpuworks/deployment/replicas-" + args.case
    manifest_path = ROOT / f"test/e2e/coverage/deployment-replicas-{args.case}.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == contract
          and manifest["test_function"] == test_replicas.__name__
          and Path(test_replicas.__code__.co_filename).resolve() == (ROOT / manifest["test_file"]).resolve()
          and manifest["driver_file"] == str(Path(__file__).resolve().relative_to(ROOT)),
          "coverage manifest does not bind this case")
    report = {"contract": contract, "case": manifest["name"], "run_id": run_id, "status": "NOT_RUN", "step": "preflight", "last_step": None,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-agent-docker-fake-gpu-facts", "identity": "admin browser / node agent / Docker",
        "product_mutation": args.mutation,
        "check_family": args.check_family, "probe_only": args.check_family != "all",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [contract], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(ROOT / manifest["test_file"]),
        "driver_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "container_fixture_sha256": digest(ROOT / "test/e2e/deployment_container_fixture.py"),
        "replica_fixture_sha256": digest(ROOT / "test/e2e/deployment_replica_fixture.py"),
        "browser_case_sha256": digest(ROOT / "test/e2e/deployment_replica_browser.py"),
        "batch_checks_sha256": digest(ROOT / "test/e2e/deployment_replica_checks.py"),
        "target_sha": subprocess.check_output(["git", "rev-parse", "wei/master"], cwd=ROOT, text=True).strip(),
        "execution": "API creates the initial fixture deployment; browser mutates replicas; real agent and Docker launch the audited fake-engine; synthetic GPU facts only",
        "runtime": "real engine and gateway requests; no real GPU compute or vendor driver",
        "not_covered": manifest["missing"],
        "command": ["python3", "test/e2e/deployment_replicas.py", *sys.argv[1:]]}
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
        report["docker_host"] = os.environ.get("DOCKER_HOST", "default local daemon")
        report["docker_daemon"] = json.loads(subprocess.check_output(["docker", "info", "--format",
            '{"id":{{json .ID}},"root":{{json .DockerRootDir}},"name":{{json .Name}},"driver":{{json .Driver}}}'], text=True, timeout=20))
        report["fake_engine_image_id"] = subprocess.check_output(
            ["docker", "image", "inspect", "--format", "{{.Id}}", IMAGE], text=True, timeout=20).strip()
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        report["step"] = "isolated server start"
        try:
            report["health"] = server.start()
        finally:
            server_pid = getattr(getattr(server, "process", None), "pid", None)
            if type(server_pid) is int:
                report["server_pid"] = server_pid
        report["base_url"] = server.base
        report["last_step"] = "isolated server ready"
        report["step"] = "fixture admin login"
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "gpuworks-test-password-2026"})
        check(status == 200, f"fixture login HTTP {status}")
        token = login["token"]
        check(isinstance(token, str) and bool(token.strip()), "fixture login returned no valid token")
        artifacts.remember(token)
        report["preflight"] = "PASS"
        report["last_step"] = "fixture admin authenticated"
        report["step"] = "real agent and fixture resources creation"
        data = replica_fixture.create(server, token, run_id, report, holder,
                                         agent_bin, artifacts.remember, 1 if args.case == "scale" else 2)
        report["last_step"] = "fixture resources created"
        report["step"] = "browser setup and login"
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
                test_replicas(page, context, server, artifacts, report,
                                           args.locale, data, token, holder, args.case)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) - {"cleanup"} <= layers,
                      "required evidence is missing")
                check(report["batch_checks_sha256"] == digest(ROOT / "test/e2e/deployment_replica_checks.py"),
                      "replica checks changed during execution")
                check(report["browser_case_sha256"] == digest(ROOT / "test/e2e/deployment_replica_browser.py"),
                      "replica browser changed during execution")
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
        stream_ok = True
        if holder.get("stream") is not None:
            stream = holder["stream"]
            report["stream"] = {"status": stream.status, "lines": list(stream.lines), "error": stream.error}
            try:
                stream_ok = holder["stream"].stop()
            except Exception:
                stream_ok = False
            report["stream_stopped"] = stream_ok
        try:
            cleanup_ok = replica_fixture.cleanup(server, token, report, holder)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
            report.setdefault("remaining_resources", []).append({"kind": "cleanup", "id": run_id,
                "state": "cleanup aborted; ownership not released", "reclaim": "use owned IDs/names and container ledger in this report",
                "owned": {key: report[key] for key in ("owned_deployment_id", "owned_deployment_name",
                    "owned_model_id", "owned_node_uuid", "owned_key_name", "containers") if report.get(key)},
                "last_observation": report.get("last_observation")})
        # A resource-cleanup exception must not skip process/log shutdown.
        try:
            agent = holder.get("agent")
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
        if not report["agent_stopped"]:
            cleanup_ok = False
            pid = getattr(holder.get("agent"), "pid", "unknown")
            report.setdefault("remaining_resources", []).append({"kind": "agent", "id": pid,
                "state": "not confirmed stopped", "reclaim": f"kill {pid}"})
        try:
            log = holder.get("log")
            if log is not None and not log.closed:
                log.close()
        except Exception:
            cleanup_ok = False
            report["agent_log_close_error"] = artifacts.redact(traceback.format_exc())
        if not stream_ok:
            report.setdefault("remaining_resources", []).append({"kind": "stream", "id": run_id,
                "state": "handler not confirmed stopped", "reclaim": "stop only this run server/agent"})
        cleanup_ok = cleanup_ok and stream_ok
        if cleanup_ok:
            report["evidence"].append({"layer": "cleanup", "steps": report["cleanup_steps"]})
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        if not report["server_stopped"]:
            server_pid = report.get("server_pid", "unknown")
            report.setdefault("remaining_resources", []).append({"kind": "server", "id": server_pid,
                "state": "not confirmed stopped", "reclaim": f"kill {server_pid}"})
        image = report.get("owned_image_tag")
        if image:
            try:
                replica_fixture.docker("image", "rm", image)
                observed = subprocess.run(["docker", "image", "inspect", image], capture_output=True, text=True, timeout=10)
                check(observed.returncode != 0 and re.fullmatch(
                    r"(?:Error response from daemon: |Error: )?No such (?:image|object): " + re.escape(image),
                    observed.stderr.strip(), re.IGNORECASE), "run-owned image tag removal not confirmed: " + observed.stderr)
                report["owned_image_tag"] = None
                report["image_tag_removed"] = True
            except Exception:
                cleanup_ok = False
                report.setdefault("remaining_resources", []).append({"kind": "docker-image-tag", "id": image,
                    "state": "not confirmed absent", "reclaim": "docker image rm " + image})
                report["image_cleanup_error"] = artifacts.redact(traceback.format_exc())
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
    print(json.dumps({"status": report["status"], "contract": contract, "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
