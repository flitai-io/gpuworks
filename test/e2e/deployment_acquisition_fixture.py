"""Owned public-repository/agent/storage prerequisites; never creates deployments.

Self-tests: python3 -B -m unittest discover -s test/e2e -p test_deployment_acquisition_fixture.py
Two real agents report synthetic A100/L4 facts. Only A100 satisfies the owned
spec. Downloads, filesystem, Docker and control plane remain real.
"""
import json
import os
from pathlib import Path
import subprocess
import time
from urllib.parse import quote

from local_browser_fixture import http
from platform_token_read_revoke import check
from deployment_sleep_wake import wait_online
import deployment_container_fixture as containers
from model_download_fixture import ModelDownload, REVISION
from deployment_batch_checks import DISPLAY_SUFFIX

IMAGE = 'gpuworks/fake-engine:test'
SPEC = 'browser-public-acquisition'
RULES = '/api/v1/pull/source-rules'
STORAGES = '/api/v1/storages'
TEMPLATES = '/api/v1/inference-templates'
CASES = ('vllm-huggingface', 'vllm-modelscope', 'sglang-huggingface', 'sglang-modelscope')


def wait(predicate, label, timeout=60):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        time.sleep(.1)
    raise AssertionError(f'{label}: deadline expired; last={last!r}')


def start_node(server, token, prefix, suffix, gpu, report, holder, agent_bin, artifacts):
    # Name is registered before POST, so a lost create response remains scoped.
    owned = {'name': prefix + '-' + suffix, 'uuid': None, 'gpu': gpu}
    report.setdefault('owned_acquisition_nodes', []).append(owned)
    code, created = http(server.base, 'POST', containers.NODES, token,
                         {'name': owned['name'], 'server_url': server.base})
    node = created.get('node') if isinstance(created, dict) else None
    secret = created.get('token') if isinstance(created, dict) else None
    if isinstance(node, dict) and isinstance(node.get('uuid'), str):
        owned['uuid'] = node['uuid']
        if suffix == 'eligible':
            report['owned_node_uuid'] = node['uuid']
    if isinstance(secret, str) and secret.strip():
        artifacts.remember(secret)
    check(code == 201 and owned['uuid'], f'node {suffix} setup HTTP {code}')
    check(isinstance(secret, str) and secret.strip(), f'node {suffix} returned invalid token')
    state = server.work / ('agent-' + suffix)
    state.mkdir()
    log = (server.work / ('agent.log' if suffix == 'eligible' else 'agent-unfit.log')).open('w')
    handles = {'process': None, 'log': log, 'owned': owned}
    holder.setdefault('acquisition_agents', []).append(handles)
    process = subprocess.Popen([str(agent_bin.resolve()), 'run', '--server', server.base,
        '--token', secret, '--state', str(state)], stdout=log, stderr=subprocess.STDOUT,
        env={**os.environ, 'GPUWORKS_FAKE_GPU': gpu, 'GPUWORKS_FAKE_GPU_NO_INJECT': '1'})
    handles['process'] = process  # Register immediately, before waiting online.
    owned['pid'] = process.pid
    if suffix == 'eligible':
        holder.update(agent=process, log=log)
    wait_online(server.base, token, node['uuid'], process)
    def inventory_ready():
        check(process.poll() is None, 'owned agent exited before its GPU inventory arrived')
        code, fresh = http(server.base, 'GET', containers.NODES + '/' + node['uuid'], token)
        check(code == 200 and fresh['state'] == 'online', 'registered agent not online')
        facts = [{'index': card['index'], 'name': card['name']} for card in fresh.get('gpus', [])]
        owned['last_gpu_inventory'] = facts
        return fresh if facts == [{'index': 0, 'name': gpu.split('x')[0]}] else None
    # Connection registration can precede the separate status.node inventory.
    # Wait for actual independent facts, rather than treating online as GPU-ready.
    return wait(inventory_ready, 'owned node exact GPU inventory')


def prepare(server, token, run_id, report, holder, agent_bin, artifacts, *, case):
    check(case in CASES, 'unknown acquisition matrix case')
    engine, provider = case.split('-', 1)
    prefix = 'e2e-' + run_id.lower()
    report['owned_model_repo'] = repo = prefix + '/weights'
    report['owned_source_rule_note'] = prefix + '-acquisition'
    report['owned_deployment_name'] = prefix + '-deployment' + DISPLAY_SUFFIX
    hub = holder['acquisition_hub'] = ModelDownload(run_id, repo)
    hub.start()
    report['fixture_origins'] = {'huggingface': hub.base + '/hf', 'modelscope': hub.base + '/ms'}
    report['owned_source_rule_ids'] = []
    for source, origin in (('huggingface', 'https://huggingface.co'), ('modelscope', 'https://modelscope.cn')):
        code, rule = http(server.base, 'POST', RULES, token, {'match': origin,
            'endpoints': [report['fixture_origins'][source]], 'allow_upstream': False,
            'control_plane': True, 'note': report['owned_source_rule_note'], 'node_requirements': {}})
        if isinstance(rule, dict) and type(rule.get('id')) is int:
            report['owned_source_rule_ids'].append(rule['id'])
        check(code == 201 and isinstance(rule, dict) and type(rule.get('id')) is int, 'source rule setup failed')
        code, selected = http(server.base, 'GET', '/api/v1/pull/metadata-sources?source=' + source, token)
        check(code == 200 and selected == {'source': source, 'bases': [report['fixture_origins'][source]]},
              'actual product source rules selected unexpected origins')
    eligible = start_node(server, token, prefix, 'eligible', 'A100x1,24576', report, holder, agent_bin, artifacts)
    unfit = start_node(server, token, prefix, 'unfit', 'L4x1,24576', report, holder, agent_bin, artifacts)
    # The isolated server's seeded default storage is outside its permitted
    # probe root. Disable it; two owned local roots are the only live choices.
    code, seeded = http(server.base, 'GET', STORAGES, token)
    check(code == 200, 'cannot enumerate isolated seeded storages')
    for storage in seeded['items']:
        code, changed = http(server.base, 'PATCH', STORAGES + '/' + str(storage['id']), token,
            {'enabled': False, 'path': str(server.storage_root / 'disabled-seeded')})
        check(code == 200, f'cannot disable isolated seeded storage: HTTP {code}: {changed}')
    stores = []
    for letter in ('a', 'b'):
        root = server.storage_root / ('store-' + letter)
        root.mkdir()
        owned = {'name': prefix + '-store-' + letter, 'path': str(root), 'id': None}
        report.setdefault('owned_acquisition_storages', []).append(owned)
        body = {'name': owned['name'], 'type': 'local', 'path': str(root), 'enabled': True,
                'is_default': letter == 'a', 'all_nodes': False, 'targets': [{'node_uuid': eligible['uuid']}]}
        code, storage = http(server.base, 'POST', STORAGES, token, body)
        if isinstance(storage, dict) and type(storage.get('id')) is int:
            owned['id'] = storage['id']
        check(code == 201 and owned['id'], f'storage setup HTTP {code}: {storage}')
        def ready():
            status, doc = http(server.base, 'GET', STORAGES + '/' + str(owned['id']) + '?all_nodes=1', token)
            check(status == 200, f'storage read HTTP {status}')
            states = doc['nodes'].get('all', [])
            return doc if len(states) == 1 and states[0]['state'] == 'ready' and states[0]['writable'] else None
        stores.append(wait(ready, 'owned storage ready'))
    report['owned_template_model_id'] = model_id = prefix + '/scheme'
    spec = {'name': SPEC, 'display_name_zh_cn': '采集 · BF16', 'display_name_en': 'Acquisition · BF16', 'scenario': 'balanced', 'engine': engine,
        'image': IMAGE, 'weights': {'quantization': 'fp16', 'sources': [
            {'huggingface': {'repo_id': repo, 'revision': REVISION}},
            {'modelscope': {'repo_id': repo, 'revision': REVISION}}]},
        'weight_bytes': len(hub.files['model.safetensors']),
        'gpus': {'vendor': 'nvidia', 'name': 'A100', 'display_target': 'nvidia'},
        'args': ['--dialect', engine], 'env': {'E2E_ACQUISITION': run_id}}
    if engine == 'sglang':
        spec['entrypoint'] = ['sglang', 'serve']  # Explicit supported override; scratch has no Python.
        spec['args'] = ['--model-type', 'llm', '--dialect', engine]
        # Operator-owned logging config disables the real engine's shell pre-start
        # wrapper. fake-engine ignores this; default SGLang logging is not tested.
        spec['env']['SGLANG_LOGGING_CONFIG_PATH'] = '/tmp/e2e-fixture-owned-logging.json'
    model = {'model_id': model_id, 'display_title': prefix + '-scheme',
        'architecture': 'Qwen2ForCausalLM', 'input_modalities': ['text'], 'context_length': 4096,
        'model_config': json.loads(hub.files['config.json']), 'specs': [spec]}
    code, imported = http(server.base, 'POST', TEMPLATES + '/import', token, {'models': [model]})
    check(code == 200, f'template setup HTTP {code}: {imported}')
    code, template = http(server.base, 'GET', TEMPLATES + '/model/' + model_id, token)
    check(code == 200 and len(template['specs']) == 1, 'template readback changed spec count')
    report['owned_key_name'] = prefix + '-key'
    code, key = http(server.base, 'POST', containers.APIKEYS, token, {'name': report['owned_key_name']})
    secret = key.get('key') if isinstance(key, dict) else None
    if isinstance(secret, str) and secret.strip():
        artifacts.remember(secret)
    check(code == 201 and isinstance(secret, str) and secret.strip(), 'invalid inference key response')
    return {'case': case, 'engine': engine, 'provider': provider, 'run_id': run_id, 'repo': repo,
        'hub': hub, 'node': eligible, 'unfit': unfit, 'storages': stores, 'template': template,
        'template_model_id': model_id, 'import_model': model, 'key': key, 'name': report['owned_deployment_name'],
        'served_name': prefix + '-route'}


def cleanup(server, token, report, holder):
    """Each failure leaves scope and continues; secondary agents are not runner-owned."""
    okay = True
    residuals = []
    report.setdefault('cleanup_steps', [])
    def stage(kind, ident, reclaim, action):
        nonlocal okay
        try:
            check(action() is True, kind + ' not independently observed reclaimed')
            report['cleanup_steps'].append({'stage': kind, 'id': ident, 'ok': True})
        except Exception as error:
            okay = False
            report['cleanup_steps'].append({'stage': kind, 'id': ident, 'ok': False})
            residuals.append({'kind': kind, 'id': ident, 'state': str(error), 'reclaim': reclaim})
    hub = holder.get('acquisition_hub')
    if hub:
        hub.release()
    # Recover partial browser writes, using only this run's exact identities.
    repo = report.get('owned_model_repo')
    if repo:
        def recover_models():
            code, listed = http(server.base, 'GET', containers.MODELS + '?source_id=' + quote(repo, safe=''), token)
            check(code == 200, 'cannot discover run models')
            models = [m for m in listed['items'] if m['source_id'] == repo]
            check(len(models) <= 1, 'unexpected multiple models for owned repo')
            if models:
                report['owned_model_id'] = models[0]['id']
            return True
        stage('model-discovery', repo, 'GET /api/v1/models?source_id=' + quote(repo, safe=''), recover_models)
    name = report.get('owned_deployment_name')
    if name and not report.get('owned_deployment_id'):
        def recover_deployment():
            code, listed = http(server.base, 'GET', containers.DEPLOYMENTS + '?q=' + quote(name), token)
            check(code == 200, 'cannot discover owned deployment')
            matches = [d for d in listed['items'] if d['name'] == name]
            check(len(matches) <= 1, 'nonunique owned deployment name')
            if matches:
                report['owned_deployment_id'] = matches[0]['id']
            return True
        stage('deployment-discovery', name, 'GET /api/v1/deployments?q=' + quote(name), recover_deployment)
    def primary_cleanup():
        # The shared legacy helper replaces cleanup_steps. Isolate its list so
        # earlier browser/discovery stages survive, including on partial failure.
        previous = list(report['cleanup_steps'])
        report['cleanup_steps'] = []
        try:
            return containers.cleanup(server, token, report, holder, request=http, inspect=containers.docker_running)
        finally:
            report['cleanup_steps'] = previous + list(report['cleanup_steps'])
    stage('primary-domain', name, 'inspect this report owned IDs', primary_cleanup)
    # containers.cleanup owns the primary agent/node. Always close every other
    # process independently even when its online wait or the primary cleanup failed.
    for handles in holder.get('acquisition_agents', []):
        process, log, owned = handles['process'], handles['log'], handles['owned']
        def stop(process=process, log=log):
            stopped = True
            try:
                if process is not None and process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
                stopped = process is None or process.poll() is not None
            finally:
                if not log.closed:
                    log.close()
            return stopped
        stage('acquisition-agent', owned.get('pid', owned['name']), 'stop only this recorded PID', stop)
    for owned in report.get('owned_acquisition_nodes', []):
        def delete_node(owned=owned):
            ident = owned['uuid']
            if not ident:
                code, listed = http(server.base, 'GET', containers.NODES + '?q=' + quote(owned['name']), token)
                check(code == 200, 'node recovery lookup failed')
                matches = [n for n in listed['items'] if n['name'] == owned['name']]
                check(len(matches) <= 1, 'node recovery not unique')
                if not matches:
                    return True
                ident = owned['uuid'] = matches[0]['uuid']
            path = containers.NODES + '/' + ident
            http(server.base, 'DELETE', path + '?force=true', token)
            check(http(server.base, 'GET', path, token)[0] == 404, 'node remains')
            owned['uuid'] = None
            return True
        stage('acquisition-node', owned['uuid'] or owned['name'], 'DELETE only matching owned node?force=true', delete_node)
    model_id = report.get('owned_template_model_id')
    if model_id:
        def delete_template():
            http(server.base, 'DELETE', TEMPLATES + '/spec?model_id=' + quote(model_id, safe='') + '&spec=' + SPEC, token)
            check(http(server.base, 'GET', TEMPLATES + '/model/' + model_id, token)[0] == 404, 'template remains')
            report['owned_template_model_id'] = None
            return True
        stage('template', model_id, 'DELETE only owned template spec', delete_template)
    owned_storages = report.get('owned_acquisition_storages', [])
    if owned_storages:
        def restore_seeded_default():
            # The default storage cannot be deleted. The seeded row takes the
            # default back so both owned stores are ordinary and removable.
            code, listed = http(server.base, 'GET', STORAGES, token)
            check(code == 200, 'storage discovery failed')
            owned_names = {owned['name'] for owned in owned_storages}
            seeded = [row for row in listed['items'] if row['name'] not in owned_names]
            check(len(seeded) == 1, 'isolated seeded storage is not unique')
            code, changed = http(server.base, 'PATCH', STORAGES + '/' + str(seeded[0]['id']), token,
                                 {'is_default': True})
            check(code == 200 and changed.get('is_default') is True, f'seeded default restore HTTP {code}')
            return True
        stage('acquisition-storage-default', 'seeded',
              'PATCH the seeded storage back to default before deleting owned stores', restore_seeded_default)
    for owned in owned_storages:
        def delete_storage(owned=owned):
            code, listed = http(server.base, 'GET', STORAGES, token)
            check(code == 200, 'storage discovery failed')
            matches = [s for s in listed['items'] if s['name'] == owned['name']]
            check(len(matches) <= 1, 'nonunique owned storage')
            for storage in matches:
                owned['id'] = storage['id']
                http(server.base, 'DELETE', STORAGES + '/' + str(storage['id']), token)
                check(http(server.base, 'GET', STORAGES + '/' + str(storage['id']), token)[0] == 404, 'storage remains')
            owned['id'] = None
            return True
        stage('acquisition-storage', owned['id'] or owned['name'], 'DELETE only storage with exact owned name', delete_storage)
    note = report.get('owned_source_rule_note')
    if note:
        def delete_rules():
            code, rules = http(server.base, 'GET', RULES, token)
            check(code == 200, 'source rule discovery failed')
            for rule in rules['items']:
                if rule['note'] == note:
                    http(server.base, 'DELETE', RULES + '/' + str(rule['id']), token)
            code, rules = http(server.base, 'GET', RULES, token)
            check(code == 200 and not any(r['note'] == note for r in rules['items']), 'source rules remain')
            report['owned_source_rule_note'], report['owned_source_rule_ids'] = None, []
            return True
        stage('source-rules', note, 'delete only exact owned note rules', delete_rules)
    if hub:
        def stop_hub():
            try:
                observed = report['fixture_stop'] = hub.stop()
                return observed['stopped'] is True and observed['handlers'] == 0 and observed['listener_stopped'] is True
            finally:
                # stop releases held handlers and finalizes socket byte/hash audit.
                # Preserve that final observation even when stop itself raises.
                report['fixture_requests'] = hub.audit()
        stage('acquisition-source', hub.base, 'stop this run-owned fixture process', stop_hub)
    report.setdefault('remaining_resources', []).extend(residuals)
    return okay and not report['remaining_resources']
