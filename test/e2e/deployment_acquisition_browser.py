"""Four public-provider/automatic-placement browser contracts.

Mainline: real template Deploy → explicit public provider/revision registration →
auto selects the independently eligible A100 → choose local storage B → real
provider expiry rejection → restore source → create → real agent download bytes,
container mount/view/argv and gateway → reload. Scoped domain cleanup is separate.
Run: deployment_acquisition.py --case vllm-huggingface --locale en
"""
import copy
import hashlib
import json
from pathlib import Path
import re
import time
from urllib.parse import parse_qs, quote, urlsplit

from playwright.sync_api import expect
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
from local_browser_fixture import ROOT, http
from local_business_runner import unattributed_console_errors
from platform_token_read_revoke import button, check
from deploy_submit_hold import hold_deploy_create
from deployment_batch_checks import created_notice, rejected_notice, open_deployment_row, delete_name_preview
from deployment_selector_checks import (
    assert_template_sources, choose_destination_storage, assert_storage_evaluation)
from deployment_wizard_create import select
import deployment_launch_checks as launch_checks
import deployment_container_fixture as containers
from deployment_acquisition_fixture import IMAGE, SPEC, TEMPLATES
from model_download_fixture import REVISION
from deployment_acquisition_oracles import same, expected_body, file_facts, source_transfer, execution


def texts(locale, namespace):
    return json.loads((ROOT / f'ui/src/i18n/locales/{locale}/{namespace}.json').read_text())


def until(page, predicate, label, timeout=120):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        page.wait_for_timeout(100)  # Bounded condition polling dispatches browser events.
    raise AssertionError(f'{label}: deadline expired; last={last!r}')


def login(page, context, server, artifacts, report, locale):
    auth, labels = texts(locale, 'auth'), texts(locale, 'inferenceTemplates')
    report['sensitive_phase'] = True
    page.goto(server.base + '/inference-templates')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login'
                              and r.request.method == 'POST') as logged:
        button(page, auth['submit']).click()
    check(logged.value.status == 200, 'browser login failed')
    for cookie in context.cookies():
        artifacts.remember(cookie['value'])
    report['sensitive_phase'] = False
    artifacts.start_trace(context)
    return labels


def replace_owned_template(server, token, data, model):
    status, result = http(server.base, 'POST', TEMPLATES + '/import?overwrite=true', token, {'models': [model]})
    check(status == 200, f'owned template edit failed: HTTP {status}: {result}')
    status, fresh = http(server.base, 'GET', TEMPLATES + '/model/' + data['template_model_id'], token)
    check(status == 200, 'owned template edit readback failed')
    same(fresh['id'], data['template']['id'], 'concurrent template edit preserved the original identity')
    wanted = [next(iter(source)) for source in model['specs'][0]['weights']['sources']]
    specs = [spec for spec in fresh['specs'] if spec['name'] == SPEC and spec['source'] == 'user']
    check(len(specs) == 1, 'owned template edit did not retain exactly the run spec')
    same([source['provider'] for source in specs[0]['weight_sources']], wanted,
         'actual catalog readback provider set')


def watch(page, report):
    # Unattributed console errors and unexpected >=400 responses fail the case
    # in conclude; only the anonymous pre-login session probe is attributed.
    requests, failures, evaluations = [], [], []
    report.update(browser_mutations=requests, browser_http_errors=failures,
                  page_errors=[], console_errors=[], evaluate_observations=[])
    session = {'authenticated': False}
    def requested(request):
        parts = urlsplit(request.url)
        if request.method in ('POST', 'PUT', 'PATCH', 'DELETE') and (
                parts.path.startswith('/api/v1/models') or parts.path == containers.DEPLOYMENTS):
            requests.append({'method': request.method, 'path': parts.path, 'query': parts.query,
                'body': request.post_data_json if request.post_data else None})
    def responded(response):
        path = urlsplit(response.url).path
        if path == '/api/v1/evaluate' and response.request.method == 'POST':
            evaluations.append(response)
        if response.status >= 400:
            if not session['authenticated'] and path == '/api/v1/auth/me' and response.status == 401:
                return  # Exactly the anonymous session probe, before login.
            failures.append({'method': response.request.method, 'path': path, 'status': response.status})
    def console_seen(message):
        if message.type != 'error':
            return
        path = urlsplit(message.location.get('url', '')).path
        if not session['authenticated'] and path == '/api/v1/auth/me' and '401' in message.text:
            return  # Exactly the anonymous session probe, before login.
        report['console_errors'].append({'path': path, 'text': message.text})
    page.on('request', requested)
    page.on('response', responded)
    page.on('pageerror', lambda error: report['page_errors'].append(str(error)))
    page.on('console', console_seen)
    return requests, evaluations, session


def conclude(report):
    # The deliberate expired-provider create rejection is the only expected >=400.
    expected = {'method': 'POST', 'path': containers.DEPLOYMENTS, 'status': 400}
    failures = report['browser_http_errors']
    unexpected = [failure for failure in failures if failure != expected]
    check(not unexpected, 'unexpected browser HTTP failure: ' + str(failures))
    check(not report['page_errors'], 'browser page errors: ' + str(report['page_errors']))
    rest = unattributed_console_errors(report['console_errors'],
                                       [failure for failure in failures if failure == expected])
    check(not rest, 'unexpected browser console errors: ' + str(rest))


def exercise(page, context, server, artifacts, report, locale, data, token, holder):
    labels, common = texts(locale, 'deploy'), texts(locale, 'common')
    requests, evaluations, session = watch(page, report)
    templates = login(page, context, server, artifacts, report, locale)
    session['authenticated'] = True
    report['step'] = 'template real provider selector and registration'
    template_path = '/api/v1/inference-templates/model/' + data['template_model_id']
    with page.expect_response(lambda response: response.request.method == 'GET'
                              and urlsplit(response.url).path == template_path) as loaded_template:
        page.goto(server.base + '/inference-templates/' + data['template_model_id'])
    assert_template_sources(loaded_template.value, data, report)
    expect(page.get_by_role('heading', name=data['template']['display_title'], exact=True)).to_be_visible()
    # The owned template has one spec; the detail action preserves its real
    # template/spec URL identity instead of manufacturing form internals.
    deploy_text = templates['detail']['deploy']
    deploy_pattern = r'\s*'.join(deploy_text) if re.fullmatch(r'[\u3400-\u9fff]{2}', deploy_text) else re.escape(deploy_text)
    # The source chooser depends on this lookup. Capture its real response
    # before checking the dialog, rather than spending the render assertion's
    # budget while the template/spec and source lookup are still loading.
    report['last_step'] = 'template-source-lookup-ready'
    # The source chooser asks the template's own weight lookup
    # (api.ts:4206, useTemplateWeightSource.ts:105); the older /api/v1/models
    # ?source_id= query no longer exists, so waiting for it timed out rather than
    # observing anything about the fixture.
    # spec_source/spec_name name the owned spec, which assert_template_sources
    # above just read out of the real template document.
    owned_spec = next(spec for spec in loaded_template.value.json()['specs']
                      if spec['name'] == SPEC and spec['source'] == 'user')
    source_query = {'template_id': [str(data['template']['id'])],
                    'spec_source': [owned_spec['source']],
                    'spec_name': [owned_spec['name']]}
    with page.expect_response(lambda response: response.request.method == 'GET'
                              and urlsplit(response.url).path == '/api/v1/inference-templates/weight-models'
                              and parse_qs(urlsplit(response.url).query) == source_query) as source_lookup:
        page.get_by_role('button').filter(has_text=re.compile('^' + deploy_pattern + '$')).click()
    response = source_lookup.value
    body = response.json()
    # The fixture repository is independently unregistered, so the lookup offers
    # the template's declared source with no model attached to it.
    check(response.status == 200 and body['models'] == [],
          'template source lookup must return the independently unregistered fixture repository')
    # The owned spec declares both providers against the one fixture repository,
    # so the lookup echoes both and attaches no model to either.
    report['acquisition_lookup'] = {'query': source_query, 'body': body}
    check([source['provider'] for source in body['sources']] == ['huggingface', 'modelscope']
          and all(source['repo_id'] == data['repo'] for source in body['sources']),
          'template source lookup did not describe the fixture sources: ' + json.dumps(body))
    check(all(source.get('reuse_model_id', 0) == 0 for source in body['sources']),
          'an unregistered repository must not offer a model to reuse')
    report['selector_requests'].append({'method': 'GET',
        'path': '/api/v1/inference-templates/weight-models',
        'query': source_query, 'status': response.status, 'candidates': []})
    modal = page.get_by_role('dialog', name=labels['action']['selectWeightSource'], exact=True)
    choice = modal.get_by_role('combobox', name=labels['action']['selectSourcePlaceholder'], exact=True)
    expect(choice).to_have_value('')
    submit_source = button(modal, labels['action']['registerAndDeploy'])
    expect(submit_source).to_be_disabled()
    choice.click()
    controls = choice.get_attribute('aria-controls')
    check(bool(controls), 'provider selector did not identify popup')
    popup = page.locator('[id=' + json.dumps(controls) + ']').locator('..')
    provider_labels = [labels['action']['huggingFace'] + ' · ' + data['repo'],
                       labels['action']['modelScope'] + ' · ' + data['repo']]
    for value in provider_labels:
        expect(popup.get_by_text(value, exact=True).filter(visible=True)).to_have_count(1)
    selected_label = provider_labels[0 if data['provider'] == 'huggingface' else 1]
    popup.get_by_text(selected_label, exact=True).filter(visible=True).click()
    expect(submit_source).to_be_enabled()
    # Weight-source confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    assert_dialog_shell(page, modal, choice, submit_source)
    report.setdefault('evidence', []).append({
        'layer': 'ui', 'dialog_shell': labels['action']['selectWeightSource'], 'viewport': '1440x900'})
    path = containers.MODELS + '/quick-register'
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == 'POST') as registered:
        submit_source.click()
    check(registered.value.status == 201, 'public model quick registration failed')
    created_model = registered.value.json()
    data['model_id'] = report['owned_model_id'] = created_model['id']
    expected_register = {'source': data['provider'], 'source_id': data['repo'], 'revision': REVISION,
        'name': data['repo'], 'weight_bytes': len(data['hub'].files['model.safetensors']),
        'quantization': 'fp16', 'architecture': 'Qwen2ForCausalLM', 'context_length': 4096}
    same(registered.value.request.post_data_json, expected_register, 'real provider registration full wire')
    expect(modal).not_to_be_visible()
    report['step'] = 'automatic node and explicit destination storage'
    auto_mode = page.locator('.deploy-node-pool').get_by_role(
        'radio', name=labels['workbench']['node']['auto'], exact=True)
    expect(auto_mode).to_be_checked()
    auto_card = page.locator('.deploy-node-card.is-selected')
    expect(auto_card.locator('.deploy-node-item strong')).to_have_text(data['node']['name'])
    gpu = auto_card.get_by_role('group', name=labels['workbench']['gpu']['selectionAria'], exact=True).get_by_role('button')
    expect(gpu).to_have_count(1)
    expect(gpu).to_have_text('0')
    expect(gpu).to_have_class(re.compile('is-picked'))
    expect(gpu).to_be_disabled()
    storage = page.get_by_role('combobox', name=labels['workbench']['acquisition']['saveTo'], exact=True)
    choose_destination_storage(page, storage, evaluations, data, labels, locale, report)
    name = page.get_by_role('textbox', name=re.compile(re.escape(labels['basic']['name']) + '$'))
    route = page.get_by_role('textbox', name=labels['basic']['servedName'], exact=True)
    name.fill(data['name'])
    route.fill(data['served_name'])
    deploy = button(page, labels['page']['deploy'])
    expect(deploy).to_be_enabled(timeout=30000)
    def final_evaluation():
        for response in reversed(evaluations):
            body = response.request.post_data_json
            if (body.get('model_id') == data['model_id'] and body.get('engine') == data['engine']
                    and body.get('model_source', {}).get('storage_id') == data['storages'][1]['id']):
                check(response.status == 200, 'selected-storage evaluate failed')
                return response
        return None
    evaluated = until(page, final_evaluation, 'page selected-storage evaluation')
    evaluation = assert_storage_evaluation(evaluated, data, report, selected=True)
    same(evaluation['selected_node']['uuid'], data['node']['uuid'], 'auto independent unique eligible node')
    same(evaluation['plan']['plan']['gpus'], [0], 'auto independent GPU zero')
    same(evaluation['plan']['plan']['tp'], 1, 'auto independent single GPU plan')
    eligible = {node['uuid']: node['eligible'] for node in evaluation['eligible_nodes']}
    same(eligible, {data['node']['uuid']: True, data['unfit']['uuid']: False},
         'A100 spec unique eligibility versus online L4')
    same(sorted(t['storage_id'] for t in evaluation['acquisition']['targets']),
         sorted(s['id'] for s in data['storages']), 'actual save-to enum has exactly A/B')
    forecast = evaluation['acquisition']['nodes']
    check(len(forecast) == 1, 'auto forecast did not contain exactly selected node')
    same({k: forecast[0][k] for k in ('node_uuid', 'image_plan', 'weights_plan', 'weights_provider')},
         {'node_uuid': data['node']['uuid'], 'image_plan': 'present', 'weights_plan': 'download',
          'weights_provider': data['provider']}, 'independent owned image-present public-download forecast')
    forecast_table = page.get_by_role('table').filter(has=page.get_by_text(
        labels['workbench']['nodeAcquisition']['forecastNote'], exact=True))
    forecast_row = forecast_table.get_by_role('row').filter(has_text=data['node']['name'])
    expect(forecast_row).to_have_count(1)
    expect(forecast_row.get_by_role('cell').nth(0)).to_have_text(data['node']['name'])
    expect(forecast_row.get_by_role('cell').nth(1)).to_have_text(labels['workbench']['nodeAcquisition']['present'])
    source_host = urlsplit(data['hub'].base).netloc
    expect(forecast_row.get_by_role('cell').nth(2)).to_have_text(
        labels['workbench']['nodeAcquisition']['downloadHost'].replace('{{hosts}}', source_host))
    report['evaluate_observations'].append({'status': evaluated.status,
        'request': evaluated.request.post_data_json, 'response': evaluation})
    launch_preview = launch_checks.draft_checks(page, labels, expected_body(data), report, token=token)
    launch_checks.boolean_rejection.seed(page, labels, expected_body(data), report, token,
        launch_checks.boolean_rejection.declared_mount(str(server.storage_root / 'store-b')), data['storages'][1]['id'])
    # A real second session removes the selected provider from this owned
    # spec after evaluation. Browser retains its original valid draft.
    report['step'] = 'real selected provider expires before create'
    changed = copy.deepcopy(data['import_model'])
    changed['specs'][0]['weights']['sources'] = [source for source in changed['specs'][0]['weights']['sources']
                                               if data['provider'] not in source]
    with launch_checks.boolean_rejection.expiry_guard(page, report):
        replace_owned_template(server, token, data, changed)
        try:
            stale = hold_deploy_create(page, report)
            same(stale.status, 400, 'expired selected provider HTTP')
            rejected = stale.json()
            same(rejected['code'], 'ACQUISITION_CANDIDATE_STALE', 'expired provider error code')
            same(rejected['params'], {'provider': data['provider']}, 'expired provider error parameters')
            same(stale.request.post_data_json, launch_checks.boolean_rejection.expected_bad_body(expected_body(data), report), 'expired provider original full draft')
            expect(name).to_have_value(data['name'])
            expect(route).to_have_value(data['served_name'])
            error = labels['action']['createFailed'].replace('{{detail}}', common['apiError']['ACQUISITION_CANDIDATE_STALE'])
            rejected_notice(page, error, labels, report)
            status, listed = http(server.base, 'GET', containers.DEPLOYMENTS + '?q=' + quote(data['name'], safe=''), token)
            check(status == 200 and listed['items'] == [] and listed['total'] == 0, 'rejected expiry wrote a deployment')
            launch_checks.boolean_rejection.retained(page, labels, expected_body(data), report, token, stale, 'provider')
        finally:
            replace_owned_template(server, token, data, data['import_model'])
    launch_checks.boolean_rejection.restore(page, labels, expected_body(data), report, token)
    report['step'] = 'restore selected provider and create exactly one deployment'
    with page.expect_response(lambda r: urlsplit(r.url).path == containers.DEPLOYMENTS
                              and r.request.method == 'POST') as created:
        deploy.click()
    check(created.value.status == 201, f'acquisition create HTTP {created.value.status}: {created.value.text()}')
    created_notice(page, labels, report)
    dep_id = report['owned_deployment_id'] = created.value.json()['id']
    same(created.value.request.post_data_json, expected_body(data), 'complete automatic acquisition create wire')
    report['step'] = 'real acquisition reaches running container'
    def running():
        code, saved = http(server.base, 'GET', containers.DEPLOYMENTS + '/' + str(dep_id), token)
        check(code == 200, f'deployment read HTTP {code}')
        containers.remember_containers(report, saved)
        report['last_deployment_observation'] = saved
        return saved if saved['runtime_state'] == 'running' else None
    saved = until(page, running, 'real agent public download and container running', timeout=180)
    for key in ('model_id', 'node_uuid', 'name', 'served_name', 'engine', 'image', 'template_id',
                'template_spec_source', 'template_spec_name', 'replicas', 'nnodes', 'placement_strategy'):
        same(saved[key], expected_body(data)[key], 'persisted deployment ' + key)
    snapshot_expected = {'engine': data['engine'], 'image': IMAGE, 'spec_source': 'user', 'spec_name': SPEC,
        'weight_provider': data['provider'], 'weight_repo_id': data['repo'], 'weight_revision': REVISION}
    same({k: saved['catalog_selection_snapshot'].get(k) for k in snapshot_expected}, snapshot_expected,
         'persisted acquisition snapshot')
    code, model = http(server.base, 'GET', containers.MODELS + '/' + str(data['model_id']), token)
    check(code == 200, 'independent acquired model read failed')
    same({k: model[k] for k in ('source', 'source_id', 'revision')},
         {'source': data['provider'], 'source_id': data['repo'], 'revision': REVISION}, 'persisted model artifact identity')
    copies = [n for n in model['nodes'] if n['state'] == 'present']
    check(len(copies) == 1, 'acquisition did not produce exactly one present replica')
    copy_info = copies[0]
    total_bytes = sum(len(value) for value in data['hub'].files.values())
    same({k: copy_info[k] for k in ('node_uuid', 'storage_id', 'acquisition', 'size_bytes', 'downloaded', 'progress')},
         {'node_uuid': data['node']['uuid'], 'storage_id': data['storages'][1]['id'],
          'acquisition': 'download', 'size_bytes': total_bytes, 'downloaded': total_bytes, 'progress': 1},
         'actual acquired replica target')
    # Model-file revision is an optional observed field; Model.Revision and the
    # actual source URLs above/below prove the immutable requested artifact.
    if 'revision' in copy_info:
        same(copy_info['revision'], REVISION, 'observed model-file revision')
    report['model_file_revision_observation'] = {
        'known': 'revision' in copy_info, 'value': copy_info.get('revision'),
        'basis': 'optional model-file status; artifact/snapshot/source-transfer revisions asserted independently'}
    root = Path(copy_info['path']).resolve()
    check(root.is_relative_to(Path(data['storages'][1]['path']).resolve()), 'download escaped explicitly chosen B')
    disk = file_facts(root, data['hub'].files)
    check(not list(Path(data['storages'][0]['path']).rglob('model.safetensors')), 'unselected A received weights')
    current = [i for i in saved['instances'] if i['state'] not in ('failed', 'stopped', 'draining', 'pending-removal', 'removing')]
    check(len(current) == 1 and current[0]['state'] == 'running', 'expected one running current replica')
    same(current[0]['node_uuid'], data['node']['uuid'], 'actual instance independent eligible node')
    same(current[0]['gpu_indexes'], [0], 'actual instance GPU zero')
    launch_checks.snapshot_checks(saved, expected_body(data), report)
    launch_checks.runtime_checks(current[0], expected_body(data), launch_preview, str(root), report)
    runtime = execution(current[0]['container_id'], current[0], str(root), data, report)
    check(data['hub'].wait_idle(), 'source handlers still active after acquired files completed')
    transfer = source_transfer(data['hub'].audit(), data)
    chat = page.request.post(server.base + '/v1/chat/completions',
        headers={'Authorization': 'Bearer ' + data['key']['key']},
        data={'model': data['served_name'], 'messages': [{'role': 'user', 'content': 'acquisition'}]})
    containers.assert_chat_response(chat)
    report['step'] = 'reload real deployment list and detail'
    page.goto(server.base + '/deployments')
    page.reload()
    row = page.get_by_role('row').filter(has_text=data['name'])
    expect(row).to_have_count(1)
    drawer = open_deployment_row(page, row, data['name'], dep_id, report)
    expect(drawer.get_by_text(data['served_name'], exact=True)).to_be_visible()
    expect(drawer.get_by_text(IMAGE, exact=True).first).to_be_visible()
    listings = texts(locale, 'deployments')
    parent = page.get_by_role('dialog', name=data['name'], exact=True)
    expect(parent).to_be_visible()
    delete_title = listings['delete']['title'].replace('{{name}}', data['name'])
    # The header Delete button includes the icon's accessible name.
    remove = parent.locator('.ant-drawer-extra').get_by_role(
        'button', name=listings['delete']['button'])
    dep_path = containers.DEPLOYMENTS + '/' + str(dep_id)
    deletes = []

    def on_delete(request):
        if request.method == 'DELETE' and urlsplit(request.url).path == dep_path:
            deletes.append(request.method)

    page.on('request', on_delete)

    def open_delete():
        remove.click()
        dialog = page.get_by_role('dialog', name=delete_title, exact=True)
        expect(dialog).to_be_visible()
        delete_name_preview(dialog, delete_title, report)
        return dialog

    dialog = assert_nested_dismissals(
        open_delete(), parent, open_delete,
        [('cancel', cancel_button(listings['delete']['cancel'])),
         ('escape', escape), ('close', close_button), ('mask', mask)],
        lambda: len(deletes), remove)
    cancel_button(listings['delete']['cancel'])(dialog)
    expect(dialog).to_be_hidden()
    expect(parent).to_be_visible()
    check(deletes == [], 'delete dialog dismissal removed the deployment')
    stayed = page.request.get(server.base + dep_path)
    check(stayed.status == 200 and stayed.json().get('name') == data['name'],
          'delete dialog dismissal changed the stored deployment')
    report['evidence'].append({
        'layer': 'ui', 'delete_dialog': delete_title, 'parent': data['name'],
        'dismissals': ['cancel', 'escape', 'close', 'mask'],
        'focus_returned': True, 'delete_requests': 0, 'stored_name': data['name']})
    same(requests, [{'method': 'POST', 'path': path, 'query': '', 'body': expected_register},
                   {'method': 'POST', 'path': containers.DEPLOYMENTS, 'query': '',
                    'body': launch_checks.boolean_rejection.expected_bad_body(expected_body(data), report)},
                   {'method': 'POST', 'path': containers.DEPLOYMENTS, 'query': '', 'body': expected_body(data)}],
         'browser mutation full bodies/count/order')
    launch_checks.frozen(report)
    conclude(report)
    report['last_step'] = 'acquisition-runtime-and-refresh-proven'
    report['evidence'].extend([
        {'layer': 'ui', 'provider': selected_label, 'storage': data['storages'][1]['name'],
         'auto_node': data['node']['name'], 'gpu': 0, 'refreshed_route': data['served_name'], 'expiry_error': error},
        {'layer': 'request', 'mutations': requests, 'evaluation': report['evaluate_observations']},
        {'layer': 'persistence', 'deployment_id': dep_id, 'snapshot': snapshot_expected, 'model_copy': copy_info},
        {'layer': 'execution', 'disk_files': disk, 'source_transfers': transfer, **runtime, 'gateway': chat.json()}])
