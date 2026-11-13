"""Run-owned source rules/catalog and independently observed domain cleanup.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
"""
from urllib.parse import quote
from local_browser_fixture import http
from platform_token_read_revoke import check
from model_hub_fixture import CONFIG, WEIGHT_BYTES, ModelHub
import json

MODELS = "/api/v1/models"
RULES = "/api/v1/pull/source-rules"
TEMPLATES = "/api/v1/inference-templates"
SPEC = "e2e-model-hub"


def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
    prefix = "e2e-" + run_id.lower()
    report["owned_model_repo"] = repo = prefix + "/weights"
    report["owned_source_rule_ids"] = []
    report["owned_source_rule_note"] = prefix + "-metadata"
    hub = ModelHub(repo)
    holder["model_hub"] = hub
    hub.start()
    report["fixture_origins"] = {"huggingface": hub.base + "/hf", "modelscope": hub.base + "/ms"}
    for source, origin in (("huggingface", "https://huggingface.co"), ("modelscope", "https://modelscope.cn")):
        endpoint = report["fixture_origins"][source]
        body = {"match": origin, "endpoints": [endpoint], "allow_upstream": False,
                "control_plane": True, "note": report["owned_source_rule_note"], "node_requirements": {}}
        status, rule = http(server.base, "POST", RULES, token, body)
        if isinstance(rule, dict) and type(rule.get("id")) is int:
            report["owned_source_rule_ids"].append(rule["id"])
        check(status == 201 and type(rule.get("id")) is int, "source rule setup failed")
        status, candidates = http(server.base, "GET", "/api/v1/pull/metadata-sources?source=" + source, token)
        check(status == 200 and candidates == {"source": source, "bases": [endpoint]}, "fixture origins not selected by actual source rules")
    model_id = prefix + "/template"
    report["owned_template_model_id"] = model_id
    model = {"model_id": model_id, "display_title": prefix + "-template",
        "architecture": "Qwen2ForCausalLM", "input_modalities": ["text"], "context_length": 4096,
        "model_config": json.loads(CONFIG), "specs": [{"name": SPEC, "display_name_zh_cn": "模型 · BF16", "display_name_en": "Model · BF16",
        "scenario": "balanced", "engine": "vllm", "image": "gpuworks/fake-engine:test",
        "weights": {"quantization": "fp16", "sources": [
            {"huggingface": {"repo_id": repo, "revision": "main"}},
            {"modelscope": {"repo_id": repo, "revision": "main"}}]},
        "weight_bytes": WEIGHT_BYTES, "gpus": {"vendor": "nvidia", "name": "A100", "display_target": "nvidia"},
        "args": ["--max-model-len", "4096"], "api": {"health_endpoint": "/health", "operations": [
            {"type": "openai.chat", "endpoint": "/v1/chat/completions", "mode": "stream"}]}}]}
    status, imported = http(server.base, "POST", TEMPLATES + "/import", token, {"models": [model]})
    check(status == 200, f"owned template import HTTP {status}: {imported}")
    status, template = http(server.base, "GET", TEMPLATES + "/model/" + model_id, token)
    check(status == 200 and len(template["specs"]) == 1, "owned template readback failed")
    return {"hub": hub, "repo": repo, "name": prefix + "-model", "template": template,
            "template_model_id": model_id}


def cleanup(server, token, report, holder):
    okay = True
    steps = report.setdefault("observed_cleanup_steps", [])
    remaining = report.setdefault("remaining_resources", [])
    repo = report.get("owned_model_repo")
    if repo:
        try:
            code, listed = http(server.base, "GET", MODELS + "?source_id=" + quote(repo, safe=""), token)
            check(code == 200, "model discovery cleanup failed")
            for model in listed["items"]:
                check(model["source_id"] == repo, "model discovery escaped owned repository")
                path = MODELS + "/" + str(model["id"])
                code, _ = http(server.base, "DELETE", path, token)
                status, _ = http(server.base, "GET", path, token)
                steps.append({"kind": "model", "id": model["id"], "delete": code, "read": status})
                check(code in (204, 404) and status == 404, "owned model remains")
            code, final = http(server.base, "GET", MODELS + "?source_id=" + quote(repo, safe=""), token)
            check(code == 200 and final["total"] == 0 and final["items"] == [], "model residue remains")
            report["owned_model_repo"] = None
        except Exception as error:
            okay = False
            remaining.append({"kind": "model-repo", "id": repo, "state": str(error), "reclaim": "GET /api/v1/models?source_id=" + quote(repo, safe="") + "; delete only matched source_id"})
    model_id = report.get("owned_template_model_id")
    if model_id:
        path = TEMPLATES + "/spec?model_id=" + quote(model_id, safe="") + "&spec=" + SPEC
        try:
            code, _ = http(server.base, "DELETE", path, token)
            status, _ = http(server.base, "GET", TEMPLATES + "/model/" + model_id, token)
            steps.append({"kind": "template", "id": model_id, "delete": code, "read": status})
            check(code in (204, 200, 404) and status == 404, "owned template remains")
            report["owned_template_model_id"] = None
        except Exception as error:
            okay = False
            remaining.append({"kind": "template", "id": model_id, "state": str(error), "reclaim": "DELETE " + path})
    note = report.get("owned_source_rule_note")
    if note:
        try:
            code, rules = http(server.base, "GET", RULES, token)
            check(code == 200, "source rule discovery failed")
            for rule in rules["items"]:
                if rule["note"] != note:
                    continue
                code, _ = http(server.base, "DELETE", RULES + "/" + str(rule["id"]), token)
                check(code in (204, 404), "source rule delete failed")
            code, rules = http(server.base, "GET", RULES, token)
            check(code == 200 and not any(r["note"] == note for r in rules["items"]), "source rules remain")
            steps.append({"kind": "source-rules", "note": note, "absent": True})
            report["owned_source_rule_ids"] = []
            report["owned_source_rule_note"] = None
        except Exception as error:
            okay = False
            remaining.append({"kind": "source-rules", "id": note, "state": str(error), "reclaim": "GET " + RULES + "; delete only exact note matches"})
    hub = holder.get("model_hub")
    if hub is not None:
        try:
            report["fixture_requests"] = hub.audit()
            report["fixture_stopped"] = observed = hub.stop()
            steps.append({"kind": "fixture", **observed})
            check(observed["stopped"] and observed["handlers"] == 0 and observed["listener_stopped"], "metadata fixture did not stop")
        except Exception as error:
            okay = False
            remaining.append({"kind": "model-hub-listener", "id": hub.base, "state": str(error), "reclaim": "stop only this run-owned Python fixture process"})
    return okay
