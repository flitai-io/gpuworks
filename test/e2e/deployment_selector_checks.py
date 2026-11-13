"""Fixture-backed selector identity checks shared by deployment browser flows.

All responses below are emitted by the page's real requests. Expected names,
repositories, nodes and stores come from the run-owned prerequisite facts.
"""
import json
import re
import time
from urllib.parse import parse_qs, unquote, urlsplit

from playwright.sync_api import expect

from local_browser_fixture import ROOT
from platform_token_read_revoke import check


def same(actual, expected, label):
    canonical = lambda value: json.dumps(value, sort_keys=True, allow_nan=False)
    check(canonical(actual) == canonical(expected),
          f'{label}: expected={expected!r}; actual={actual!r}')


def copy(locale, namespace):
    return json.loads((ROOT / f'ui/src/i18n/locales/{locale}/{namespace}.json').read_text())


def watch_weight_lookups(page):
    """Every lookup the page makes to resolve a spec's weight models.

    The page asks GET /api/v1/inference-templates/weight-models for the models that
    serve the selected spec (useTemplateWeightSource.ts:45, api.go:197). It used to
    ask GET /api/v1/models?source_id=..., which is why a listener keyed on that
    query stopped seeing anything once the page was rewritten and the case failed
    with "did not request the selected spec repository models" while the modal was
    showing exactly the right model. Both shapes are collected here so the case
    asserts on the answer rather than on the route it arrived by.
    """
    responses = []

    def seen(response):
        if response.request.method != 'GET':
            return
        parts = urlsplit(response.url)
        query = parse_qs(parts.query)
        if parts.path == '/api/v1/inference-templates/weight-models':
            responses.append(response)
        elif parts.path == '/api/v1/models' and 'source_id' in query:
            responses.append(response)

    page.on('response', seen)
    return responses


def model_facts(response, model, query, report, label):
    same(parse_qs(urlsplit(response.url).query), query, label + ' query')
    check(response.request.method == 'GET' and response.status == 200, label + ' must be real GET 200')
    body = response.json()
    expected = {'id': model['id'], 'name': model['name'],
                'source_id': model['source_id'], 'source': 'local'}
    same([{key: item.get(key) for key in expected} for item in body['items']],
         [expected], label + ' exact candidate set')
    same(body['total'], 1, label + ' total')
    report.setdefault('selector_requests', []).append({
        'method': 'GET', 'path': '/api/v1/models', 'query': query,
        'status': response.status, 'candidates': [expected]})


def choose_registered_model(page, fixture, locale, report):
    model = fixture['model']
    report['last_step'] = 'selector-model-readable-identity'
    query = {'page': ['1'], 'page_size': ['50'], 'q': [model['name']]}
    control = page.locator('#model_id')
    with page.expect_response(lambda response: response.request.method == 'GET'
                              and urlsplit(response.url).path == '/api/v1/models'
                              and parse_qs(urlsplit(response.url).query).get('q') == [model['name']]) as searched:
        control.fill(model['name'])
    model_facts(searched.value, model, query, report, 'model selector')
    controls = control.get_attribute('aria-controls')
    check(bool(controls), 'model selector has no controlled popup')
    popup = page.locator('[id=' + json.dumps(controls) + ']').locator('..')
    option = popup.locator('.ant-select-item-option').filter(has_text=model['name'])
    expect(option).to_have_count(1, timeout=12000)
    expect(option.get_by_text(model['name'], exact=True)).to_be_visible(timeout=12000)
    expect(option.locator('.ant-tag')).to_have_text(copy(locale, 'models')['source']['local'], timeout=12000)
    option.click()
    report['model_selector_identity'] = {'name': model['name'], 'source': 'local',
                                         'id': model['id'], 'readable_option': True}


def _weight_lookup_facts(response, model, report):
    """The weight lookup has to name this model, and say it serves the spec."""
    parts = urlsplit(response.url)
    query = parse_qs(parts.query)
    check(response.request.method == 'GET' and response.status == 200,
          'weight-source lookup must be a real GET 200')
    body = response.json()
    if parts.path == '/api/v1/inference-templates/weight-models':
        # {models: [ModelListItem], sources: [{provider, repo_id, revision,
        # subfolder, reuse_model_id}]} - api.go:197. The model that serves the
        # selected spec has to appear in models, and the source that matches it
        # has to point back at it, which is the rule the create path enforces.
        ids = [item.get('id') for item in body.get('models') or []]
        check(model['id'] in ids,
              f"weight lookup did not offer the registered model: ids={ids!r} expected {model['id']!r}")
        offered = [item for item in body.get('models') or [] if item.get('id') == model['id']][0]
        check(offered.get('name') == model['name'] and offered.get('source_id') == model['source_id'],
              f"weight lookup offered a different model: {offered!r}")
        # reuse_model_id is only set when the source's provider equals the model's
        # source (inference_template_weight_models.go:55). A locally registered
        # model serving a template whose declared source is, say, huggingface
        # therefore reuses nothing - correct product behaviour, and the reason this
        # case must not demand a non-zero reuse_model_id. What it can require is
        # that every reuse id points at a model the same answer actually offered.
        offered_ids = {item.get('id') for item in body.get('models') or []}
        for match in body.get('sources') or []:
            reuse = match.get('reuse_model_id') or 0
            check(reuse == 0 or reuse in offered_ids,
                  f"weight source reuses a model the answer did not offer: {reuse!r}")
        report.setdefault('selector_requests', []).append({
            'method': 'GET', 'path': parts.path, 'query': query,
            'status': response.status, 'candidates': [offered]})
        return
    model_facts(response, model, {'page': ['1'], 'page_size': ['50'],
                                  'source_id': [model['source_id']]}, report,
                'weight-source model lookup')


def assert_weight_match(page, radio, fixture, locale, report, responses):
    model = fixture['model']
    check(bool(responses), 'page did not request the selected spec repository models')
    _weight_lookup_facts(responses[-1], model, report)
    report['last_step'] = 'selector-weight-model-readable-identity'
    label = radio.locator('xpath=ancestor::label[1]')
    expect(label.get_by_text(model['name'], exact=True)).to_be_visible(timeout=12000)
    expect(label.locator('.ant-tag')).to_have_text(copy(locale, 'models')['source']['local'], timeout=12000)
    expect(label.get_by_text(model['source_id'], exact=True)).to_be_visible(timeout=12000)
    report['weight_model_selector_identity'] = {'name': model['name'], 'source': 'local',
                                                'source_id': model['source_id']}


def choose_manual_node(page, fixture, labels, report):
    query = {'page': ['1'], 'page_size': ['50'], 'state': ['online']}
    report['last_step'] = 'selector-node-readable-identity'
    with page.expect_response(lambda response: response.request.method == 'GET'
                              and urlsplit(response.url).path == '/api/v1/nodes'
                              and parse_qs(urlsplit(response.url).query) == query) as listed:
        page.get_by_text(labels['workbench']['node']['manual'], exact=True).click()
    check(listed.value.status == 200, 'node selector GET did not succeed')
    body = listed.value.json()
    expected = {'uuid': fixture['node']['uuid'], 'name': fixture['node']['name'], 'gpu_count': 1}
    same([{key: item.get(key) for key in expected} for item in body['items']],
         [expected], 'node selector exact online candidate set')
    same(body['total'], 1, 'node selector total')
    control = page.get_by_text(labels['workbench']['node']['selectNodeAndGpu'], exact=True).locator(
        '..').locator('..').get_by_role('combobox')
    control.click()
    controls = control.get_attribute('aria-controls')
    check(bool(controls), 'node selector has no controlled popup')
    popup = page.locator('[id=' + json.dumps(controls) + ']').locator('..')
    label = fixture['node']['name'] + ' · 1 GPU'
    option = popup.get_by_text(label, exact=True).filter(visible=True)
    expect(option).to_have_count(1, timeout=12000)
    expect(option).to_be_visible(timeout=12000)
    option.click()
    report.setdefault('selector_requests', []).append({
        'method': 'GET', 'path': '/api/v1/nodes', 'query': query,
        'status': listed.value.status, 'candidates': [expected]})
    report['node_selector_identity'] = {**expected, 'label': label}


def assert_template_sources(response, data, report):
    path = '/api/v1/inference-templates/model/' + data['template_model_id']
    same(unquote(urlsplit(response.url).path), path, 'template selector path')
    same(urlsplit(response.url).query, '', 'template selector query')
    check(response.request.method == 'GET' and response.status == 200, 'template selector GET did not succeed')
    body = response.json()
    same(body['id'], data['template']['id'], 'template selector database identity')
    same(body['model_id'], data['import_model']['model_id'], 'template selector model identity')
    source_spec = data['import_model']['specs'][0]
    specs = [spec for spec in body['specs'] if spec['name'] == source_spec['name'] and spec['source'] == 'user']
    same(len(specs), 1, 'template selector exact owned spec count')
    expected = []
    for source in source_spec['weights']['sources']:
        check(len(source) == 1, 'fixture source must declare exactly one provider')
        provider, value = next(iter(source.items()))
        expected.append({'provider': provider, 'repo_id': value['repo_id'], 'revision': value['revision']})
    same(specs[0]['engine'], data['engine'], 'template selector spec engine')
    same([{key: source.get(key) for key in ('provider', 'repo_id', 'revision')}
          for source in specs[0]['weight_sources']], expected, 'template selector source identities')
    report.setdefault('selector_requests', []).append({
        'method': 'GET', 'path': path, 'query': '', 'status': response.status,
        'model_id': body['model_id'], 'spec': source_spec['name'], 'sources': expected})


def assert_storage_evaluation(response, data, report, *, selected=False):
    same(urlsplit(response.url).path, '/api/v1/evaluate', 'storage selector evaluate path')
    same(urlsplit(response.url).query, '', 'storage selector evaluate query')
    check(response.request.method == 'POST' and response.status == 200,
          'storage selector evaluate did not succeed')
    body = response.request.post_data_json
    expected = {'model_id': data['model_id'], 'engine': data['engine'],
                'template_id': data['template']['id'], 'template_spec_source': 'user',
                'template_spec_name': data['import_model']['specs'][0]['name']}
    same({key: body.get(key) for key in expected}, expected, 'storage selector evaluate draft identity')
    same(body.get('model_source', {}).get('provider'), data['provider'], 'storage selector requested provider')
    if selected:
        same(body['model_source'].get('storage_id'), data['storages'][1]['id'], 'storage selector requested B')
    result = response.json()
    actual = [{key: target.get(key) for key in ('storage_id', 'label', 'storage_type')}
              for target in result['acquisition']['targets']]
    wanted = [{'storage_id': item['id'], 'label': item['name'], 'storage_type': 'local'}
              for item in data['storages']]
    same(sorted(actual, key=lambda item: item['storage_id']),
         sorted(wanted, key=lambda item: item['storage_id']), 'storage selector exact A/B candidate facts')
    report.setdefault('selector_requests', []).append({
        'method': 'POST', 'path': '/api/v1/evaluate', 'status': response.status,
        'request': body, 'storage_candidates': wanted, 'selected_B': selected})
    return result


def choose_destination_storage(page, storage, evaluations, data, labels, locale, report):
    report['last_step'] = 'selector-storage-evaluation-ready'
    deadline = time.monotonic() + 12
    while True:
        matching = [response for response in evaluations
                    if response.request.post_data_json.get('model_id') == data['model_id']
                    and response.request.post_data_json.get('engine') == data['engine']
                    and response.request.post_data_json.get('model_source', {}).get('provider') == data['provider']]
        if matching:
            break
        check(time.monotonic() < deadline, 'explicit provider evaluation did not settle')
        page.wait_for_timeout(50)  # Dispatch pending response callbacks; no request is replayed.
    assert_storage_evaluation(matching[-1], data, report)
    report['last_step'] = 'selector-storage-readable-identity'
    storage.focus()
    storage.press('ArrowDown')
    controls = storage.get_attribute('aria-controls')
    check(bool(controls), 'storage selector has no controlled popup')
    popup = page.locator('[id=' + json.dumps(controls) + ']').locator('..')
    options = popup.locator('.ant-select-item-option')
    expect(options).to_have_count(2, timeout=12000)
    type_text = copy(locale, 'common')['storageType']['local']
    selected = None
    for index, item in enumerate(data['storages']):
        check(item['type'] == 'local', 'fixture storage type changed')
        label = labels['workbench']['acquisition']['storageTarget'].replace(
            '{{name}}', item['name']).replace('{{type}}', type_text)
        option = options.filter(has_text=label)
        expect(option).to_have_count(1, timeout=12000)
        expect(option).to_be_visible(timeout=12000)
        expect(option).to_have_attribute('title', label, timeout=12000)
        recommended = labels['workbench']['acquisition']['storageRecommended'] if index == 0 else ''
        content = re.compile(r'^\s*' + re.escape(label) + r'\s*' + re.escape(recommended) + r'\s*$')
        expect(option.locator('.ant-select-item-option-content')).to_have_text(content, timeout=12000)
        if index == 1:
            selected = option
    selected.click()
    report['storage_selector_identity'] = {'type': 'local', 'type_label': type_text,
                                           'names': [item['name'] for item in data['storages']],
                                           'selected_id': data['storages'][1]['id']}
