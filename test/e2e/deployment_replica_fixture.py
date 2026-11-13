"""Real Docker and streaming observations for replica browser contracts.

Only GPU discovery facts are synthetic. Stream completion is the audited
fake-engine protocol: CHUNKS exact content deltas, exact usage, then DONE.
"""
from http.client import HTTPConnection
import json
import os
import re
import socket
import sys
import subprocess
import threading
import time
from urllib.parse import urlsplit, quote

from local_browser_fixture import http
from platform_token_read_revoke import check
import deployment_lifecycle as lifecycle
import deployment_container_fixture as containers

CHUNKS = 120
CONTENT = "hello from fake-engine"


def strict(actual, expected, label):
    encode = lambda v: json.dumps(v, sort_keys=True, separators=(",", ":"), allow_nan=False)
    check(encode(actual) == encode(expected), f"{label}: expected {expected!r}, actual {actual!r}")


def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=20)
    check(result.returncode == 0, f"docker {args[0]}: {result.stderr.strip()}")
    return result.stdout


def counters(instances):
    counts = {}
    for item in instances:
        ident = item["container_id"]
        logs = docker("logs", ident)
        ids = re.findall(r"Received request (req-[0-9a-f]{8}): prompt=16 tok, completion=8 tok", logs)
        check(len(ids) == len(set(ids)), f"duplicate request IDs in {ident}")
        counts[ident] = len(ids)
    return counts


def assert_counter_delta(before, after, target, peer, target_delta, peer_delta, label):
    strict(after, {target: before[target] + target_delta, peer: before[peer] + peer_delta}, label)


def assert_no_surge(document, target, peer, active_containers):
    check(document.get("routing_policy") == "failover", "no-surge probe lost failover routing policy")
    strict(document.get("replicas"), 2, "no-surge desired replicas")
    strict(document.get("ready_replicas"), 1, "no-surge ready replicas while target drains")
    rows = document.get("instances", [])
    check(any(i["uuid"] == target["uuid"] and i["state"] == "draining" for i in rows), "target is not actually draining")
    strict(sorted(i["uuid"] for i in rows if i["state"] == "running"), [peer["uuid"]],
           "replacement must not become running before the held target finishes")
    strict(sorted(active_containers), sorted([target["container_id"], peer["container_id"]]),
           "no-surge exact real Docker running container set")


def running_containers(report):
    result = []
    for ident in docker("ps", "-q", "--filter", "ancestor=" + report["owned_image_tag"]).split():
        item = json.loads(docker("inspect", ident))[0]
        if item["Config"]["Image"] == report["owned_image_tag"]:
            result.append(item["Id"])
            if item["Id"] not in report.setdefault("containers", []):
                report["containers"].append(item["Id"])
    return result


def stream_values(lines):
    payloads = [line[6:] for line in lines if line.startswith("data: ")]
    return payloads


def assert_complete(lines):
    values = stream_values(lines)
    expected_delta = {"id": "fake-engine-completion", "choices": [{"delta": {"content": CONTENT}}]}
    expected = [expected_delta] * CHUNKS + [
        {"choices": [], "usage": {"prompt_tokens": 16, "completion_tokens": 8, "total_tokens": 24}}]
    check(len(values) == CHUNKS + 2, f"stream event count: expected {CHUNKS + 2}, actual {len(values)}")
    check(values[-1:] == ["[DONE]"], f"stream missing DONE; got {len(values)} events")
    strict([json.loads(value) for value in values[:-1]], expected, "full streaming response")


class Stream:
    def __init__(self, base, key, served):
        parsed = urlsplit(base)
        self.conn = HTTPConnection(parsed.hostname, parsed.port, timeout=90)
        self.lines, self.error, self.status, self.response = [], None, None, None
        self.first = threading.Event()
        self.done = threading.Event()
        self.thread = threading.Thread(target=self._run, args=(key, served), daemon=True)
        self.thread.start()

    def _run(self, key, served):
        try:
            self.conn.request("POST", "/v1/chat/completions", json.dumps({"model": served,
                "messages": [{"role": "user", "content": "replica-stream"}], "stream": True}),
                {"Authorization": "Bearer " + key, "Content-Type": "application/json"})
            response = self.conn.getresponse()
            self.response = response
            self.status = response.status
            check(response.status == 200, f"gateway stream HTTP {response.status}")
            check(response.getheader("Content-Type").startswith("text/event-stream"), "stream content type")
            while line := response.readline():
                value = line.decode().rstrip("\r\n")
                self.lines.append(value)
                if value.startswith("data: "):
                    self.first.set()
        except Exception as error:
            self.error = str(error)
        finally:
            self.done.set()
            self.conn.close()

    def stop(self):
        # close() alone cannot interrupt a concurrent buffered readline.
        sock = self.conn.sock
        if sock is None and self.response is not None:
            sock = getattr(getattr(self.response.fp, "raw", None), "_sock", None)
        if sock is not None:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.conn.close()
        self.thread.join(timeout=5)
        return not self.thread.is_alive()


def create(server, token, run_id, report, holder, agent_bin, remember, replicas):
    image = "gpuworks/e2e-replicas:" + run_id.lower()
    report["owned_image_tag"] = image
    docker("tag", report["fake_engine_image_id"], image)
    strict(docker("image", "inspect", "--format", "{{.Id}}", image).strip(),
           report["fake_engine_image_id"], "run-owned image identity")
    report["owned_node_name"] = f"e2e-{run_id}-replica-node"
    status, created = http(server.base, "POST", containers.NODES, token, {
        "name": report["owned_node_name"], "server_url": server.base})
    check(status == 201, f"fixture node HTTP {status}")
    node = created["node"]
    report["owned_node_uuid"] = node["uuid"]
    node_token = created.get("token")
    if isinstance(node_token, str) and bool(node_token.strip()):
        remember(node_token)
    check(isinstance(node_token, str) and bool(node_token.strip()), "invalid node credential")
    state = server.work / "agent-state"
    state.mkdir()
    holder["log"] = (server.work / "agent.log").open("w")
    holder["agent"] = subprocess.Popen([str(agent_bin.resolve()), "run", "--server", server.base,
        "--token", node_token, "--state", str(state)], stdout=holder["log"],
        stderr=subprocess.STDOUT, env={**os.environ, "GPUWORKS_FAKE_GPU": "A100x2,24576",
                                      "GPUWORKS_FAKE_GPU_NO_INJECT": "1"})
    lifecycle.wait_online(server.base, token, node["uuid"], holder["agent"])
    report["owned_model_name"] = f"e2e-{run_id}-model"
    status, model = http(server.base, "POST", containers.MODELS + "/local", token, {
        "node_uuid": node["uuid"], "path": str(lifecycle.MODEL_DIR),
        "source_id": f"e2e/{run_id}-replicas", "name": report["owned_model_name"]})
    check(status in (200, 201), f"fixture model HTTP {status}: {model}")
    report["owned_model_id"] = model["id"]
    name = f"e2e-{run_id}-key"
    report["owned_key_name"] = name
    status, key = http(server.base, "POST", containers.APIKEYS, token, {"name": name})
    check(status == 201, f"fixture key HTTP {status}")
    if isinstance(key.get("key"), str) and bool(key["key"].strip()):
        remember(key["key"])
    check(isinstance(key.get("key"), str) and bool(key["key"].strip()), "invalid inference credential")
    body = {"model_id": model["id"], "node_uuid": node["uuid"], "name": f"e2e-{run_id}-deployment",
        "served_name": f"e2e-{run_id}-served", "engine": "vllm", "image": image,
        "placement_mode": "manual", "replicas": replicas, "routing_policy": "failover",
        "container_overrides": {"command_mode": "engine", "env": {
            "FAKE_ENGINE_STREAM_CHUNKS": str(CHUNKS), "FAKE_ENGINE_TPOT_DELAY": "500ms"}}}
    report["owned_deployment_name"] = body["name"]
    report["fixture_deployment_body"] = body
    status, deployment = http(server.base, "POST", containers.DEPLOYMENTS, token, body)
    if status == 201:
        report["owned_deployment_id"] = deployment["id"]
    check(status == 201, f"fixture deployment HTTP {status}: {deployment}")
    report["fixture_deployment_body"] = body
    report["fixture_model_file_sha256"] = {str(p.relative_to(lifecycle.MODEL_DIR)): lifecycle.digest(p)
        for p in lifecycle.MODEL_DIR.rglob("*") if p.is_file()}
    return {"node": node, "deployment": deployment, "key": key["key"], "body": body}


def current(document):
    return [item for item in document.get("instances", []) if item.get("replica") is not None]


def wait(page, server, token, report, dep_id, predicate, label, timeout=100):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        status, last = http(server.base, "GET", containers.DEPLOYMENTS + f"/{dep_id}", token)
        report["last_observation"] = {"status": status, "deployment": last}
        if status == 200:
            containers.remember_containers(report, last)
            # Register deterministic names before the container ID is reported;
            # a failed/overlapping generation may disappear between API polls.
            for instance in last.get("instances", []):
                name = "gpuworks-" + instance["uuid"][:12]
                if name not in report.setdefault("containers", []):
                    report["containers"].append(name)
            if predicate(last):
                return last
        page.wait_for_timeout(100)
    raise AssertionError(f"{label}: last={last!r}")


def ready(page, server, token, report, dep_id, count):
    document = wait(page, server, token, report, dep_id, lambda d:
        d.get("desired_state") == "active" and d.get("replicas") == count
        and d.get("ready_replicas") == count and len(current(d)) == count
        and all(i.get("state") == "running" and i.get("container_id") for i in current(d)),
        f"exactly {count} running replicas")
    strict(document.get("routing_policy"), "failover", "persisted fixture routing policy")
    status, node = http(server.base, "GET", containers.NODES + "/" + report["owned_node_uuid"], token)
    check(status == 200, f"fixture hardware GET HTTP {status}")
    strict(node.get("gpu_count"), 2, "exact two-card no-surge fixture")
    strict(sorted(g["index"] for g in node["gpus"]), [0, 1], "exact fixture GPU indices")
    if count == 2:
        strict(sorted(i["gpu_indexes"] for i in current(document)), [[0], [1]],
               "both whole GPU claims occupy the two-card fixture")
    report["hardware_observation"] = {"node_uuid": node["uuid"], "gpu_count": node["gpu_count"],
        "gpu_indices": sorted(g["index"] for g in node["gpus"])}
    for item in current(document):
        check(containers.docker_running(item["container_id"]) == "true", "replica container not running")
        inspected = json.loads(docker("inspect", item["container_id"]))[0]
        check(inspected["Image"] == report["fake_engine_image_id"], "runtime image differs from preflight image ID")
        if sys.platform.startswith("linux"):
            check(inspected["HostConfig"]["NetworkMode"] == "host", "expected host networking")
        else:
            port = str(item["api_port"])
            strict(inspected["HostConfig"]["PortBindings"].get(port + "/tcp"),
                   [{"HostIp": "127.0.0.1", "HostPort": port}], "loopback engine port on Docker Desktop")
        check(inspected["Config"]["Image"] == report["fixture_deployment_body"]["image"], "unexpected runtime image")
    return document


def inventory(report):
    image = report.get("owned_image_tag")
    if not image:
        return
    # ancestor matches an image ID shared with other tags. Confirm the exact
    # run-owned Config.Image string before taking ownership of any container.
    candidates = docker("ps", "-aq", "--filter", "ancestor=" + image).split()
    for ident in candidates:
        item = json.loads(docker("inspect", ident))[0]
        if item["Config"]["Image"] == image:
            if ident not in report.setdefault("containers", []):
                report["containers"].append(ident)


def agent_inventory(server, report):
    agent_log = server.work / "agent.log"
    if agent_log.exists():
        names = re.findall(r'args="\[run -d --name (gpuworks-[0-9a-f]{12})\b', agent_log.read_text())
        report["agent_log_container_names"] = sorted(set(names))
        for name in names:
            if name not in report.setdefault("containers", []):
                report["containers"].append(name)


def cleanup(server, token, report, holder):
    residuals = []
    try:
        agent_inventory(server, report)
    except Exception as error:
        residuals.append({"kind": "agent-log-inventory", "id": str(server.work / "agent.log"),
            "state": str(error), "reclaim": "inspect this run agent log and exact image tag"})
    for kind, path, name_key, id_key, wire_id in (
            ("deployment", containers.DEPLOYMENTS, "owned_deployment_name", "owned_deployment_id", "id"),
            ("model", containers.MODELS, "owned_model_name", "owned_model_id", "id"),
            ("node", containers.NODES, "owned_node_name", "owned_node_uuid", "uuid")):
        name = report.get(name_key)
        if not name or report.get(id_key):
            continue
        try:
            status, result = http(server.base, "GET", path + "?q=" + quote(name), token)
            check(status == 200, f"{kind} recovery HTTP {status}")
            matches = [item for item in result["items"] if item["name"] == name]
            check(len(matches) <= 1, "multiple resources match run-owned name")
            if matches:
                report[id_key] = matches[0][wire_id]
        except Exception as error:
            residuals.append({"kind": kind + "-name", "id": name, "state": str(error),
                "reclaim": "GET " + path + "?q=" + quote(name) + "; DELETE exact matching id"})
    try:
        inventory(report)
    except Exception as error:
        residuals.append({"kind": "container-inventory", "id": report.get("owned_image_tag"),
            "state": str(error), "reclaim": "inspect containers whose Config.Image equals this run-owned tag"})
    okay = containers.cleanup(server, token, report, holder, request=http, inspect=containers.docker_running)
    # Deployment 404 and stopped agent close creation races. Inventory again
    # independently, including generations never observed in a UI poll.
    try:
        agent_inventory(server, report)
        inventory(report)
        for ident in report.get("containers", []):
            check(containers.docker_running(ident) == "absent", "container remains after cleanup: " + ident)
    except Exception as error:
        residuals.append({"kind": "container-inventory", "id": report.get("owned_image_tag"),
            "state": str(error), "reclaim": "inspect and remove only containers with this exact Config.Image"})
    report.setdefault("remaining_resources", []).extend(residuals)
    return okay and not residuals
