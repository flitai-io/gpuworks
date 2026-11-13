"""Private prerequisites for deployment_wizard_create; no deployment API writes.

GPU identity is synthetic; Docker, model files and engine execution stay real.
"""
import json
import os
import shutil
import subprocess
from urllib.parse import quote

from local_browser_fixture import ROOT, http
from platform_token_read_revoke import check
from deployment_sleep_wake import wait_online
import deployment_container_fixture as containers
from deployment_batch_checks import DISPLAY_SUFFIX

IMAGE = "gpuworks/fake-engine:test"
SPEC = "browser-vllm"


def expected_body(fixture, name):
    return {"model_id": fixture["model"]["id"], "served_name": fixture["served_name"],
        "template_id": fixture["template"]["id"], "template_spec_source": "user",
        "template_spec_name": SPEC, "scenario": "balanced",
        "target_vendor": "nvidia", "node_uuid": fixture["node"]["uuid"],
        "engine": "vllm", "image": IMAGE,
        "quick_params": {"dtype": "auto", "performance_mode": "balanced",
                         "gpu_memory_utilization": 0.9, "enable_cache_report": True, "enable_request_metrics": True,
                         "kv_cache_dtype": "auto"},
        "placement_strategy": "spread",
        "container_overrides": {"env": {"E2E_WIZARD": fixture["run_id"]}},
        "nnodes": 1, "replicas": 1, "placement_mode": "manual", "name": name}



def prepare(server, token, run_id, report, holder, agent_bin, remember):
    name = f"e2e-{run_id}"
    status, created = http(server.base, "POST", containers.NODES, token=token,
                           body={"name": name + "-node", "server_url": server.base})
    node = created.get("node") if isinstance(created, dict) else None
    credential = created.get("token") if isinstance(created, dict) else None
    if isinstance(node, dict) and isinstance(node.get("uuid"), str) and node["uuid"]:
        report["owned_node_uuid"] = node["uuid"]
    if isinstance(credential, str) and credential.strip():
        remember(credential)
    check(status == 201, f"node fixture HTTP {status}")
    check(bool(report.get("owned_node_uuid")), "node fixture returned no valid UUID")
    check(isinstance(credential, str) and bool(credential.strip()),
          "node fixture returned no valid token")
    state = server.work / "agent-state"
    state.mkdir()
    log = (server.work / "agent.log").open("w")
    holder["log"] = log
    holder["agent"] = subprocess.Popen([
        str(agent_bin.resolve()), "run", "--server", server.base,
        "--token", credential, "--state", str(state)],
        stdout=log, stderr=subprocess.STDOUT,
        env={**os.environ, "GPUWORKS_FAKE_GPU": "A100x1,24576",
             "GPUWORKS_FAKE_GPU_NO_INJECT": "1"})
    wait_online(server.base, token, node["uuid"], holder["agent"])
    model_dir = server.storage_root / (name + "-weights")
    shutil.copytree(ROOT / "test/e2e/testdata/fake-model", model_dir)
    source_id = "e2e/" + run_id
    status, model = http(server.base, "POST", containers.MODELS + "/local", token=token,
        body={"node_uuid": node["uuid"], "path": str(model_dir),
              "source_id": source_id, "name": name + "-model"})
    check(status in (200, 201), f"model fixture: {status} {model}")
    report["owned_model_id"] = model["id"]
    spec = {"name": SPEC, "display_name_zh_cn": "向导 · BF16", "display_name_en": "Wizard · BF16", "scenario": "balanced",
            "engine": "vllm", "image": IMAGE,
            "weights": {"quantization": "fp16", "sources": [{"huggingface": source_id}]},
            "weight_bytes": sum(p.stat().st_size for p in model_dir.glob("*.safetensors")),
            "gpus": {"vendor": "nvidia", "name": "A100", "display_target": "nvidia"},
            "args": ["--dtype", "float16"], "env": {"E2E_WIZARD": run_id}}
    # Register identity before the mutating request: a broken response must
    # still leave a scoped deletion path for a successfully imported spec.
    report["owned_template_model_id"] = source_id
    status, imported = http(server.base, "POST", "/api/v1/inference-templates/import",
        token=token, body={"models": [{"model_id": source_id,
            "display_title": name + "-scheme", "architecture": "Qwen2ForCausalLM",
            "input_modalities": ["text"], "context_length": 4096,
            "model_config": json.loads((model_dir / "config.json").read_text()),
            "specs": [spec]}]})
    check(status == 200, f"spec fixture: {status} {imported}")
    status, template = http(server.base, "GET", "/api/v1/inference-templates/model/" + source_id,
                             token=token)
    check(status == 200, f"spec read: {status} {template}")
    report["owned_key_name"] = name + "-key"
    status, key = http(server.base, "POST", containers.APIKEYS, token=token,
                       body={"name": name + "-key"})
    inference_key = key.get("key") if isinstance(key, dict) else None
    if isinstance(inference_key, str) and inference_key.strip():
        remember(inference_key)
    check(status == 201, f"inference key HTTP {status}")
    check(isinstance(key, dict) and key.get("name") == report["owned_key_name"],
          "inference key response changed the run-owned name")
    check(isinstance(inference_key, str) and bool(inference_key.strip()),
          "inference key fixture returned no valid key")
    report["owned_deployment_name"] = name + "-deployment" + DISPLAY_SUFFIX
    return {"run_id": run_id, "node": node, "model": model, "template": template, "key": key,
            "model_dir": str(model_dir), "name": report["owned_deployment_name"],
            "served_name": name + "-route"}


def cleanup(server, token, report, holder):
    # Recover a create whose response failed before its id could be parsed.
    # Name is run-scoped and registered before the browser can submit.
    discovery_residual = []
    name = report.get("owned_deployment_name")
    if name and not report.get("owned_deployment_id"):
        try:
            code, listed = http(server.base, "GET", containers.DEPLOYMENTS + "?q=" + quote(name), token=token)
            check(code == 200, f"deployment recovery lookup: HTTP {code}")
            matches = [item for item in listed["items"] if item["name"] == name]
            check(len(matches) <= 1, "multiple deployments share this run's unique name")
            if matches:
                report["owned_deployment_id"] = matches[0]["id"]
        except Exception as error:
            discovery_residual.append({"kind": "deployment-name", "id": name,
                "state": str(error), "reclaim": "GET /api/v1/deployments?q=" + quote(name) + "; DELETE matched run-owned id"})
    # Template deletion follows deployment deletion (which may refer to it).
    okay = containers.cleanup(server, token, report, holder, request=http,
                              inspect=containers.docker_running)
    report.setdefault("remaining_resources", []).extend(discovery_residual)
    okay = okay and not discovery_residual
    source = report.get("owned_template_model_id")
    if source:
        try:
            path = "/api/v1/inference-templates/spec?model_id=" + quote(source, safe="") + "&spec=" + SPEC
            status, _ = http(server.base, "DELETE", path, token=token)
            code, _ = http(server.base, "GET", "/api/v1/inference-templates/model/" + source,
                           token=token)
            report["template_cleanup"] = {"delete_status": status, "read_status": code}
            check(code == 404, f"template cleanup: GET {code}")
            report["owned_template_model_id"] = None
        except Exception as error:
            okay = False
            report.setdefault("remaining_resources", []).append({
                "kind": "template", "id": source, "state": str(error),
                "reclaim": "DELETE " + path})
    return okay
