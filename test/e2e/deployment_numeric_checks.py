"""Complete reachable numeric inventory for the six fixture deployment drafts.

Run each driver with --check-family numeric-inventory. Independent dictionary
facts are frozen in deployment_numeric_contract.py, not read from the DOM or
an evaluated launch. A separate real page owns tier/multi-node draft changes;
the consumer's original page keeps its exact creation/runtime contract.
"""
import json
import re
import sys
from urllib.parse import urlencode, urljoin, urlsplit

from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check
from deployment_boolean_inventory_contracts import health_contract, health_source
from deployment_numeric_contract import DICTIONARY, PD_NUMBERS, PD_MODES, FIXTURE_GPU, FIXTURE_PARTITION, QUICK_QUOTA_GIB, SURFACES


QUICK_KEYS = {'max_model_len', 'gpu_memory_utilization', 'max_num_batched_tokens', 'max_num_seqs',
              'data_parallel_size',
              'kv_offloading_size', 'kv_offload_read_threads', 'kv_offload_write_threads', 'kv_disk_limit_gib'}
TIER_KEYS = {'kv_offloading_size', 'kv_offload_read_threads', 'kv_offload_write_threads', 'kv_disk_limit_gib'}
CORE = {'health_check_threshold_sec'}
RESOURCE = {'replicas', 'tp', 'pp'}
RUNTIME = {'container-overrides-shm', 'container-overrides-start-timeout'}
ABSENT = {}  # every published numeric key is now reachable
METADATA = ('key', 'type', 'control', 'group', 'layout', 'depends_on', 'min', 'max', 'step', 'default', 'automatic')


def rows_for(engine):
    check(engine in ('vllm', 'sglang'), 'numeric probe has no reviewed dictionary for engine')
    return {row['key']: row for row in DICTIONARY if engine in row['engines']}


def dictionary(page, engine, same, report):
    response = page.request.get(urljoin(page.url, '/api/v1/engines'))
    check(response.status == 200, 'numeric dictionary GET failed')
    profile = next(row for row in response.json()['items'] if row['name'] == engine)
    rows = rows_for(engine)
    numeric_specs = [row for row in profile['param_specs']
                     if row['type'] in ('number', 'integer') or row.get('control') == 'slider']
    actual = {row['key']: row for row in numeric_specs}
    report['last_step'] = 'numeric-inventory-dictionary-complete-set'
    same(sorted(row['key'] for row in numeric_specs), sorted(rows), 'complete reviewed numeric dictionary keys')
    same(sorted(set(profile['quick_form_keys']) & set(rows)), sorted(set(rows) & QUICK_KEYS),
         'complete numeric quick whitelist')
    for key, row in rows.items():
        report['last_step'] = 'numeric-inventory-schema-' + key
        expected = {name: row[name] for name in METADATA}
        if engine == 'sglang' and key == 'gpu_memory_utilization':
            expected['default'] = 0.88
        same({name: actual[key].get(name, False if name == 'automatic' else None)
              for name in METADATA}, expected, report['last_step'])
        for name in ('label', 'help', 'placeholder'):
            expected_text = {locale: text for locale, text in row[name].items() if text is not None}
            same(actual[key].get(name) or {}, expected_text, 'numeric dictionary localized ' + key + ' ' + name)
    same([row['name'] for row in profile.get('pd_modes', []) if 'nvidia' in row['vendors']],
         [PD_MODES[engine]], 'reviewed NVIDIA PD mode availability')
    report['numeric_dictionary_snapshot'] = {'engine': engine, 'numeric_keys': sorted(rows),
        'profile_numeric_specs': actual, 'unrendered': ABSENT}
    return rows


def collapse(page, label):
    # AntD's arrow icon contributes to the header's accessible name. Scope by
    # its exact visible text, then assert the real expanded-state owner.
    header = page.locator('.ant-collapse-header').filter(has=page.get_by_text(label, exact=True))
    expect(header).to_have_count(1)
    if header.get_attribute('aria-expanded') != 'true':
        header.click()
    expect(header).to_have_attribute('aria-expanded', 'true')
    expect(header.locator('..').locator('.ant-collapse-content')).to_be_visible()


def tab(page, labels, name):
    page.get_by_role('tab', name=labels['page']['step'][name], exact=True).click()


def required_contract(control, required, scope):
    # AntD emits true only for required rules; ordinary fields omit the attr.
    # noStyle + explicit aria-label (PD tp) keeps the name free of a star.
    check(control.get_attribute('aria-required') == ('true' if required else None),
          scope + ': exact aria-required contract')


def input_contract(page, key, label, value, minimum, maximum, step, placeholder, report, surface,
                   help_text=None, unit=None, disabled=False, required=False):
    report['last_step'] = 'numeric-inventory-' + surface + '-' + key
    control = page.locator('[id=' + json.dumps(key, ensure_ascii=False) + ']').filter(visible=True)
    expect(control).to_have_count(1)
    expect(control).to_have_role('spinbutton')
    expect(control).to_have_accessible_name(label)
    required_contract(control, required, report['last_step'])
    expect(control).to_have_value(str(value) if value is not None else '')
    for name, expected in (('aria-valuemin', minimum), ('aria-valuemax', maximum), ('step', step),
                           ('placeholder', placeholder)):
        if expected is None:
            check(control.get_attribute(name) is None, report['last_step'] + ': unexpected ' + name)
        else:
            expect(control).to_have_attribute(name, str(expected))
    if disabled:
        expect(control).to_be_disabled()
    else:
        expect(control).to_be_enabled()
    row = control.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-form-item ")][1]')
    if help_text:
        expect(row.locator('.ant-form-item-extra')).to_have_text(help_text)
    if unit:
        unit_scope = row if row.count() == 1 else control.locator(
            'xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-input-number-group-wrapper ")][1]')
        expect(unit_scope.get_by_text(unit, exact=True)).to_have_count(1)
    observed = {'field': key, 'surface': surface, 'label': label.pattern if hasattr(label, 'pattern') else label,
        'value': value, 'min': minimum, 'max': maximum, 'step': step,
        'placeholder': placeholder, 'help': help_text, 'unit': unit, 'disabled': disabled, 'required': required}
    report.setdefault('numeric_inventory', []).append(observed)


def slider_contract(page, row, engine, locale, report, surface):
    key = row['key']
    report['last_step'] = 'numeric-inventory-' + surface + '-' + key
    field = page.locator('.ant-form-item').filter(has=page.locator('label').filter(
        has_text=re.compile('^' + re.escape(row['label'][locale]) + '$'))).filter(visible=True)
    expect(field).to_have_count(1)
    slider = field.get_by_role('slider')
    expect(slider).to_have_count(1)
    expect(slider).to_be_enabled()
    required_contract(slider, False, report['last_step'])
    value = 0.88 if engine == 'sglang' else 0.9
    for name, expected in (('aria-valuemin', row['min']), ('aria-valuemax', row['max']),
                           ('aria-valuenow', value)):
        expect(slider).to_have_attribute(name, str(expected))
    expect(field.get_by_text(f'{value:.2f}', exact=True)).to_have_count(1)
    expect(field.locator('.ant-form-item-extra')).to_have_text(row['help'][locale])
    report.setdefault('numeric_inventory', []).append({'field': key, 'surface': surface,
        'label': row['label'][locale], 'value': value, 'min': row['min'], 'max': row['max'],
        'step': row['step'], 'help': row['help'][locale], 'readout': f'{value:.2f}',
        'disabled': False, 'readout_editable': False,
        'step_observation': 'Slider has no step DOM attribute; API step checked against frozen schema'})


def dictionary_controls(page, keys, rows, engine, locale, labels, report, surface, tiers=False):
    for key in sorted(keys):
        row = rows[key]
        if row['control'] == 'slider':
            slider_contract(page, row, engine, locale, report, surface)
            continue
        value = 1 if key == 'kv_offloading_size' and tiers else row['default']
        placeholder = labels['shared']['automatic'] if row['automatic'] else row['placeholder'][locale]
        required = tiers and key == 'kv_offloading_size'
        label = ('* ' if required else '') + row['label'][locale]
        input_contract(page, key, label, value, row['min'], row['max'], row['step'] or 1,
            placeholder, report, surface, row['help'][locale], required=required)


def shm_contract(surface, tier_size, labels):
    # These reviewed drafts have successful ordinary evaluations with a size.
    # The known rejected multi/PD drafts return no shm_bytes; like Expert,
    # their empty control therefore promises engine base + tier, not a
    # resolved total. Never choose the oracle by reading DOM/result values.
    resolved = surface in ('quick-default', 'quick-partition', 'quick-tiers')
    size = str(16 + tier_size if resolved else 16)
    key = 'shmHelpResolved' if resolved else 'shmHelpDefault'
    return size, labels['containerOverrides'][key].replace('{{size}}', size)


def static_controls(page, keys, labels, common, report, surface, nnodes=1, tier_size=0, pd=False):
    shm_placeholder, shm_help = shm_contract(surface, tier_size, labels)
    definitions = {
        'health_check_threshold_sec': (common['healthCheck']['threshold'], None, 1, 600, 1, '60', common['healthCheck']['seconds']),
        'replicas': (labels['catalog']['replicas'], 1, 1, 32 if nnodes == 1 and not pd else 1, 1, None, None),
        'tp': (re.compile('^' + re.escape(labels['workbench']['resource']['tp']) + r'\s*' + re.escape(labels['workbench']['automatic']) + '$')
               if surface.startswith('quick') and surface != 'quick-multi' else (labels['workbench']['resource']['tp'] if surface == 'quick-multi' else labels['resource']['tensorParallel']), None, 1, None, 1, labels['workbench']['automatic'], None),
        'pp': (labels['workbench']['resource']['pp'] if surface.startswith('quick') else labels['resource']['pipelineParallel'],
               None, 1, None, 1, '1', None),
        'nnodes': (labels['workbench']['node']['nodeCount'] if surface == 'quick-multi' else labels['resource']['nodeCount'], nnodes, 2 if surface == 'quick-multi' else 1, 16, 1, None, None),
        'master_port': (labels['resource']['masterPort'], None, 1024, 65535, 1, None, None),
        'vram_limit_gib': (labels['resource']['partition'], None, 1, None, 1, labels['resource']['fullGpu'], None),
        'container-overrides-shm': (labels['containerOverrides']['shm'], None, 0, None, 1, shm_placeholder, 'GiB'),
        'container-overrides-start-timeout': (labels['containerOverrides']['startTimeout'], None, 60, 86400, 60, '1800', labels['containerOverrides']['seconds']),
    }
    for key in sorted(keys):
        label, value, lo, hi, step, placeholder, unit = definitions[key]
        help_text = None
        if key == 'container-overrides-shm':
            help_text = shm_help
        elif key == 'container-overrides-start-timeout':
            help_text = labels['containerOverrides']['startTimeoutHelpDefault'].replace('{{seconds}}', '1800')
        elif key == 'vram_limit_gib':
            help_text = labels['resource']['partitionHelp']
        elif key == 'replicas':
            help_text = labels['pd']['replicasUnavailable'] if pd else labels['catalog']['replicasHelp']
        input_contract(page, key, label, value, lo, hi, step, placeholder, report, surface,
            help_text=help_text, unit=unit, disabled=(pd and key == 'replicas') or
                (key == 'health_check_threshold_sec' and not report['numeric_health_source']['expected_health']))


def distributed_partition_contract(record, same):
    # API eligibility suppresses pool capability for nnodes>1, including its early
    # field-error response. Since automatic placement was extended to multi-node
    # requests the serializer keeps placement_mode auto for them - the server picks the
    # ranks - so the multi-node draft no longer gets rewritten to manual on the wire.
    same({key: record['request'].get(key) for key in ('placement_mode', 'nnodes')},
         {'placement_mode': 'auto', 'nnodes': 2}, 'fixed distributed quota context')
    check('pool_partition_capability' not in record['response'],
          'distributed response must omit single-node pool capability')


def pd_omission_contract(request):
    for key in ('placement_mode', 'node_uuid', 'vram_limit_gib'):
        check(key not in request, 'fixed empty-role PD must omit ' + key)


def distributed_quota_controls(page, labels):
    expect(page.locator('#vram_limit_gib').filter(visible=True)).to_have_count(0)
    expect(page.get_by_text(labels['resource']['dgpuUnavailablePool'], exact=True)).to_be_visible()


def surface_contract(page, engine, surface, rows, locale, labels, common, same, report):
    check(engine in SURFACES and surface in SURFACES[engine], 'unreviewed numeric surface')
    contract = SURFACES[engine][surface]
    if contract['fold']:
        caption = labels['workbench']['advanced']['title'] if contract['fold'] == 'quick' else labels['performance']['advanced']
        collapse(page, caption)
    numbers, sliders = set(contract['numbers']), set(contract['sliders'])
    dictionary_controls(page, numbers | sliders, rows, engine, locale, labels, report, surface,
                        tiers=contract['tier_size'] > 0)
    static = set(contract['static'])
    ordinary = static - {'vram_limit_gib'} if contract['inline_quota'] else static
    static_controls(page, ordinary, labels, common, report, surface,
                    nnodes=contract['nnodes'], tier_size=contract['tier_size'], pd=contract['pd'])
    if contract['inline_quota']:
        input_contract(page, 'vram_limit_gib', '', QUICK_QUOTA_GIB, 1, None, 1, None,
                       report, surface, unit='GiB')
    for key in contract['pd_fields']:
        row = PD_NUMBERS[key]
        label = labels['pd'][row['dimension'] + 'Aria'].replace('{{role}}', labels['pd']['role'][row['role']])
        input_contract(page, key, label, row['value'], row['min'], row['max'], row['step'],
                       row['placeholder'], report, surface, required=row['dimension'] == 'tp')
    complete_set(page, static | set(contract['pd_fields']) | numbers, sliders,
                 rows, locale, same, report, surface)


def complete_set(page, keys, slider_keys, rows, locale, same, report, surface):
    report['last_step'] = 'numeric-inventory-complete-set-' + surface
    actual = page.locator('input[role="spinbutton"]:visible').evaluate_all('es => es.map(e => e.id).sort()')
    same(actual, sorted(keys), report['last_step'])
    slider_labels = page.get_by_role('slider').filter(visible=True).evaluate_all('es => es.map(e => e.closest(".ant-form-item").querySelector("label").textContent.trim()).sort()')
    same(slider_labels, sorted(rows[key]['label'][locale] for key in slider_keys),
         report['last_step'] + ' complete slider rows')
    report.setdefault('numeric_inventory_surfaces', []).append({'surface': surface,
        'input_keys': sorted(keys), 'slider_keys': sorted(slider_keys)})


def rejected_submit_gate(page, labels, scope):
    # A rendered Form field owns its inline error; only an unrendered/global
    # evaluate error participates in DeployPage's disabled predicate.
    check(scope in ('field', 'global'), 'unreviewed numeric rejection scope')
    submit = button(page, labels['page']['deploy'])
    if scope == 'field':
        expect(submit).to_be_enabled()
    else:
        expect(submit).to_be_disabled()


def checked_evaluation(page, action, predicate, evaluation, same, report, label, errors=None):
    record = evaluation(page, action, predicate, report, label)
    if 'numeric_expected_identity' in report:
        expected = report['numeric_expected_identity']
        same({key: record['request'].get(key) for key in expected}, expected, label + ' fixed fixture identity')
    same(record['response'].get('errors') or [], errors or [], label + ' exact field errors')
    same(record['response']['ok'], not bool(errors), label + ' success semantics')
    return record


def fixture_hardware(page, fixture_body, same, report):
    report['last_step'] = 'numeric-inventory-fixture-hardware'
    response = page.request.get(urljoin(page.url, '/api/v1/nodes/' + fixture_body['node_uuid']))
    check(response.status == 200, 'numeric fixture node independent GET failed')
    node = response.json()
    same({key: node.get(key) for key in ('uuid', 'gpu_count', 'gpu_vendor', 'runtime_driver')},
         {'uuid': fixture_body['node_uuid'], 'gpu_count': 1, 'gpu_vendor': 'nvidia', 'runtime_driver': 'docker'},
         'independent fixture node hardware/runtime')
    # dto.Node.Caps is optional and nodeDTO (api.go:1012) does not project
    # Hello caps. The public hardware and partition facts below are promised.
    same([{key: card.get(key) for key in FIXTURE_GPU} for card in node['gpus']], [FIXTURE_GPU],
         'fixture A100x1,24576 exact reported GPU identity and MiB-to-bytes')
    actual = [{key: capability.get(key) for key in FIXTURE_PARTITION}
              for capability in node['gpus'][0].get('partition_capabilities', [])]
    same(actual, [FIXTURE_PARTITION], 'fixture embedded NVIDIA backend capability')
    check(QUICK_QUOTA_GIB * 2**30 < FIXTURE_GPU['memory_total_bytes'],
          'reviewed quick quota exceeds independent fixture physical VRAM')
    report['numeric_fixture_hardware'] = {'startup_gpu': 'A100x1,24576', 'no_inject': '1',
        'expected_gpu': FIXTURE_GPU, 'observed_gpu': node['gpus'][0],
        'node_read': {'method': 'GET', 'path': '/api/v1/nodes/' + fixture_body['node_uuid'], 'status': response.status},
        'runtime_driver': node['runtime_driver'], 'quota_on_gib': QUICK_QUOTA_GIB,
        'source_protocol_contract': {'safety_protocol': 1, 'asset_root': 'embedded://dgpu',
            'asset_files': ['libvgpu.so'], 'backend_behavior_probe_exempt': True},
        'scope': 'synthetic GPU facts and control readiness; no real GPU isolation claim'}


def inventory(parent, labels, body, report, mode, evaluation, same):
    locale, engine = report['locale'], body['engine']
    rows = dictionary(parent, engine, same, report)
    fixture_hardware(parent, body, same, report)
    report['numeric_health_source'] = health_source(body)
    model_response = parent.request.get(urljoin(parent.url, '/api/v1/models/' + str(body['model_id'])))
    template_response = parent.request.get(urljoin(parent.url, '/api/v1/inference-templates/model/' + report['owned_template_model_id']))
    check(model_response.status == 200 and template_response.status == 200, 'numeric health fixture reads failed')
    health_contract(body, model_response.json(), template_response.json())
    common = json.loads((ROOT / f'ui/src/i18n/locales/{locale}/common.json').read_text())
    page = parent.context.new_page()
    page.set_default_timeout(10000)
    creates, console_errors, page_errors, responses = [], [], [], []

    def observe_request(request):
        if request.method == 'POST' and urlsplit(request.url).path == '/api/v1/deployments':
            creates.append({'method': request.method, 'path': urlsplit(request.url).path,
                            'body': request.post_data_json})

    def observe_response(response):
        path = urlsplit(response.url).path
        if response.request.method == 'POST' and path == '/api/v1/evaluate':
            responses.append({'method': 'POST', 'path': path, 'status': response.status,
                'request': response.request.post_data_json, 'response': response.json()})

    def observe_console(message):
        if message.type == 'error':
            console_errors.append({'text': message.text, 'location': message.location})

    def observe_pageerror(error):
        page_errors.append(str(error))

    parent.context.on('request', observe_request)
    page.on('response', observe_response)
    page.on('console', observe_console)
    page.on('pageerror', observe_pageerror)
    report['numeric_probe_page'] = {'creates': creates, 'console_errors': console_errors,
        'page_errors': page_errors, 'evaluate_responses': responses, 'closed': False,
        'scope': 'independent draft only; original page owns creation/GET/runtime'}
    try:
        query = urlencode({'model': body['model_id'], 'template': body['template_id'],
            'spec': body['template_spec_name'], 'spec_source': body['template_spec_source']})
        page.goto(urljoin(parent.url, '/deploy?' + query))
        report['last_step'] = 'numeric-inventory-probe-fixture-model'
        modal = page.get_by_role('dialog', name=labels['weightSourceModal']['confirmTitle'], exact=True)
        # The confirmation is raised by a lookup the page runs after mount, so it can
        # arrive well after load: the draft asks the server which registered models
        # serve this spec's declared sources (useTemplateWeightSource.ts:177 holds
        # until lookup.key === key). Waiting for the element to appear is different
        # from asserting it is already there - the latter turns a slow lookup into a
        # failure on a surface that would have answered correctly a moment later.
        expect(modal).to_be_visible(timeout=60000)
        expect(modal.locator('input[type="radio"][value=' + json.dumps(str(body['model_id']), ensure_ascii=False) + ']')).to_be_checked()
        button(modal, labels['weightSourceModal']['useModel']).click()
        expect(modal).not_to_be_visible()
        route = page.get_by_role('textbox', name=labels['basic']['servedName'], exact=True)
        marker = 'e2e-' + report['run_id'] + '-numeric'
        report['numeric_expected_identity'] = {key: body[key] for key in
            ('model_id', 'template_id', 'template_spec_source', 'template_spec_name', 'engine', 'target_vendor', 'scenario')}
        report['numeric_expected_identity'].update(image='gpuworks/fake-engine:test', served_name=marker)
        initial = checked_evaluation(page, lambda: route.fill(marker), lambda b: b.get('served_name') == marker,
            evaluation, same, report, 'numeric-inventory-initial-fixture')
        fixture_identity(initial['request'], body, same, 'numeric initial real fixture identity')
        expect(button(page, labels['page']['deploy'])).to_be_enabled(timeout=30000)
        mode(page, labels, 'quick')
        surface_contract(page, engine, 'quick-default', rows, locale, labels, common, same, report)
        # A100 fake GPU facts still publish ready NVIDIA dGPU capability;
        # NO_INJECT only prevents actual device injection at execution time.
        same({key: initial['response'].get('pool_partition_capability', {}).get(key)
              for key in ('backend', 'vendor', 'status')},
             FIXTURE_PARTITION, 'independent fixture NVIDIA partition capability')
        quota_card = page.get_by_role('button').filter(has=page.get_by_text(labels['workbench']['resource']['partition'], exact=True))
        expect(quota_card).to_have_count(1)
        expect(quota_card).to_be_enabled()
        expect(page.locator('#vram_limit_gib').filter(visible=True)).to_have_count(0)
        checked_evaluation(page, quota_card.click, lambda b: b.get('vram_limit_gib') == QUICK_QUOTA_GIB,
            evaluation, same, report, 'numeric-inventory-quick-partition-enable')
        # This inline quick number has no Form.Item label; its real contextual
        # caption is the partition card. Do not invent an accessible name or
        # advertise checklist 2.1 from this numeric contract.
        surface_contract(page, engine, 'quick-partition', rows, locale, labels, common, same, report)
        checked_evaluation(page, lambda: page.locator('#vram_limit_gib').fill(''),
            lambda b: 'vram_limit_gib' not in b, evaluation, same, report, 'numeric-inventory-quick-partition-clear')
        expect(page.locator('#vram_limit_gib').filter(visible=True)).to_have_count(0)
        multi_card = page.get_by_role('button').filter(has=page.get_by_text(labels['workbench']['node']['multi'], exact=True))
        expect(multi_card).to_have_count(1)
        expect(multi_card).to_be_enabled()
        # Automatic placement checks placement.tp before the node count: racking a multi-node
        # request without saying how many GPUs each node gives is the earlier problem, and
        # the server reports it on placement_mode. The node-count error is still covered in
        # the manual path below, so nothing about it is dropped here.
        quick_multi = checked_evaluation(page, multi_card.click, lambda b: b.get('nnodes') == 2,
            evaluation, same, report, 'numeric-inventory-quick-multi-empty-nodes',
            [{'field': 'placement_mode', 'code': 'AUTO_PLACEMENT_TP_REQUIRED'}])
        distributed_partition_contract(quick_multi, same)
        # Unique controls and the nnodes 2..16 contract are fixture-independent
        # domain expectations. A second unbounded nnodes input is a real gap,
        # never an excuse to change this expected multiset from the DOM.
        report['numeric_quick_multi_controls'] = page.locator('input[role="spinbutton"]:visible').evaluate_all(
            'es => es.map(e => ({id:e.id,value:e.value,min:e.getAttribute("aria-valuemin"),max:e.getAttribute("aria-valuemax"),step:e.getAttribute("step")}))')
        surface_contract(page, engine, 'quick-multi', rows, locale, labels, common, same, report)
        single_card = page.get_by_role('button').filter(has=page.get_by_text(labels['workbench']['node']['single'], exact=True))
        checked_evaluation(page, single_card.click, lambda b: b.get('nnodes') == 1,
            evaluation, same, report, 'numeric-inventory-quick-single-restore')
        mode(page, labels, 'expert')
        tab(page, labels, 'model')
        surface_contract(page, engine, 'expert-model', rows, locale, labels, common, same, report)
        tab(page, labels, 'performance')
        surface_contract(page, engine, 'expert-performance', rows, locale, labels, common, same, report)
        if engine == 'vllm':
            # CPU on only reveals its empty required capacity; then a real edit
            # supplies 1 GiB before the disk tier's dependent rows can appear.
            page.get_by_role('switch', name=labels['performance']['cpuTier'], exact=True).check()
            checked_evaluation(page, lambda: page.locator('#kv_offloading_size').fill('1'),
                lambda b: b.get('quick_params', {}).get('kv_offloading_size') == 1,
                evaluation, same, report, 'numeric-inventory-kv-memory-enable')
            checked_evaluation(page, lambda: page.get_by_role('switch', name=labels['performance']['diskTier'], exact=True).check(),
                lambda b: b.get('quick_params', {}).get('kv_disk_offload') is True,
                evaluation, same, report, 'numeric-inventory-kv-disk-enable',
                [{'field': 'shared_kv_dir', 'code': 'QUICK_PARAM_PATH_INVALID',
                  'params': {'key': 'shared_kv_dir'}}])
            report['last_step'] = 'numeric-inventory-kv-disk-field-rejection'
            directory = page.locator('#shared_kv_dir')
            directory_row = directory.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-form-item ")][1]')
            expected_error = common['apiError']['QUICK_PARAM_PATH_INVALID'].replace('{{key}}', 'shared_kv_dir')
            expect(directory).to_have_attribute('aria-invalid', 'true')
            expect(directory_row.locator('.ant-form-item-explain-error')).to_have_text(expected_error)
            rejected_submit_gate(page, labels, 'field')
            report['numeric_kv_directory_rejection'] = {'field': 'shared_kv_dir', 'error': expected_error,
                'aria_invalid': True, 'submit_enabled': True,
                'scope': 'field attribution only; no submit/create action or successful-save claim'}
            checked_evaluation(page, lambda: directory.fill('/e2e-kv-cache'),
                lambda b: b.get('quick_params', {}).get('shared_kv_dir') == '/e2e-kv-cache',
                evaluation, same, report, 'numeric-inventory-kv-disk-directory')
            expect(directory_row.locator('.ant-form-item-explain-error')).to_have_count(0)
            expect(directory).not_to_have_attribute('aria-invalid', 'true')
            surface_contract(page, engine, 'expert-tiers', rows, locale, labels, common, same, report)
            mode(page, labels, 'quick')
            surface_contract(page, engine, 'quick-tiers', rows, locale, labels, common, same, report)
            mode(page, labels, 'expert')
        tab(page, labels, 'runtime')
        surface_contract(page, engine, 'expert-runtime', rows, locale, labels, common, same, report)
        tab(page, labels, 'resource')
        surface_contract(page, engine, 'expert-resource', rows, locale, labels, common, same, report)
        # Same ordering as the quick path: with placement_mode auto the server asks for
        # placement.tp before it counts the named nodes.
        distributed = checked_evaluation(page, lambda: page.locator('#nnodes').fill('2'), lambda b: b.get('nnodes') == 2,
            evaluation, same, report, 'numeric-inventory-distributed-empty-nodes',
            [{'field': 'placement_mode', 'code': 'AUTO_PLACEMENT_TP_REQUIRED'}])
        distributed_partition_contract(distributed, same)
        distributed_quota_controls(page, labels)
        surface_contract(page, engine, 'expert-distributed', rows, locale, labels, common, same, report)
        # A multi-node group is placed by the platform unless the deployer names the nodes,
        # so the named-node field only exists once units_placement is manual. Switch that
        # control rather than dropping the field-attribution check below.
        page.locator('#units_placement').get_by_text(
            labels['resource']['manualSelect'], exact=True).click()
        distributed_control = page.locator('#distributed_nodes')
        expect(distributed_control).to_be_visible()
        expect(distributed_control).to_have_attribute('aria-invalid', 'true')
        distributed_row = distributed_control.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-form-item ")][1]')
        distributed_error = common['apiError']['DISTRIBUTED_NODE_COUNT'].replace('{{required}}', '2').replace('{{actual}}', '0')
        expect(distributed_row.locator('.ant-form-item-explain-error')).to_have_text(distributed_error)
        rejected_submit_gate(page, labels, 'field')
        report['numeric_distributed_rejection'] = {'wire_field': 'node_uuids', 'form_field': 'distributed_nodes',
            'error': distributed_error, 'aria_invalid': True, 'submit_enabled': True,
            'scope': 'field attribution only; no submit/create action or successful-save claim'}
        # PD is an offered topology for these NVIDIA fixtures, even though the
        # original consumer creates one standalone rank. Its four distinct
        # number fields must be inventoried, not declared absent by scope.
        checked_evaluation(page, lambda: page.locator('#nnodes').fill('1'), lambda b: b.get('nnodes') == 1,
            evaluation, same, report, 'numeric-inventory-return-single')
        mode(page, labels, 'quick')
        pd_card = page.get_by_role('button').filter(has=page.get_by_text(labels['workbench']['node']['pd'], exact=True))
        expect(pd_card).to_have_count(1)
        expect(pd_card).to_be_enabled()
        pd_record = checked_evaluation(page, pd_card.click,
            lambda b: b.get('placement', {}).get('pd', {}).get('mode') == PD_MODES[engine],
            evaluation, same, report, 'numeric-inventory-pd-empty-nodes', pd_errors())
        same(pd_record['request']['placement']['pd'], expected_pd(engine), 'complete independent initial PD placement')
        pd_omission_contract(pd_record['request'])
        check('pool_partition_capability' not in pd_record['response'], 'PD response must omit automatic pool capability')
        expect(quota_card).to_be_disabled()
        expect(quota_card.get_by_text(labels['workbench']['resource']['dgpuUnavailable'], exact=True)).to_be_visible()
        expect(page.locator('#vram_limit_gib').filter(visible=True)).to_have_count(0)
        expect(page.locator('.ant-select').filter(has=page.locator('#pd_mode')).locator('.ant-select-selection-item')).to_have_text(PD_MODES[engine])
        # All dictionary fields remain applicable in PD. Reopen the fresh
        # Quick fold after its Expert remount before properties and full set.
        surface_contract(page, engine, 'quick-pd', rows, locale, labels, common, same, report)
        pd_section = page.get_by_test_id('pd-section')
        expect(pd_section).to_be_visible()
        # A registered hidden field anchors the section-level error; it is
        # not a numeric input and is distinct from the role TP/PP controls.
        expect(pd_section.locator('input#pd')).to_have_count(1)
        pd_error = common['apiError']['PD_NODE_REQUIRED'].replace('{{role}}', 'decode')
        expect(pd_section.locator('.ant-alert-error .ant-alert-message')).to_have_text(pd_error)
        rejected_submit_gate(page, labels, 'field')
        report['numeric_pd_rejection'] = {'wire_field': 'placement.pd', 'form_field': 'pd',
            'error': pd_error, 'submit_enabled': True,
            'scope': 'section attribution only; no submit/create action or successful-save claim'}
        same(creates, [], 'numeric inventory emitted a deployment create in browser context')
        same(console_errors, [], 'numeric inventory browser console errors')
        same(page_errors, [], 'numeric inventory page errors')
        for response in responses:
            same(response['status'], 200, 'numeric inventory actual evaluate HTTP status')
            if response['request'].get('served_name') == marker:
                fixture_identity(response['request'], body, same, 'numeric marked draft fixture identity')
            errors = response['response'].get('errors') or []
            expected = expected_errors(response['request'])
            same(errors, expected, 'numeric inventory unrecognized evaluate field errors')
        report['numeric_inventory_complete'] = {'engine': engine, 'reachable_dictionary_keys': sorted(set(rows) - set(ABSENT)),
            'static_keys': sorted(CORE | RESOURCE | RUNTIME | {'nnodes', 'master_port', 'vram_limit_gib'} | set(PD_NUMBERS)),
            'unrendered_published_keys': ABSENT, 'create_posts': [],
            'conditional_absences': {}, 'partition_branch': 'ready NVIDIA capability; quick enabled16GiB then cleared; expert default empty/full GPU',
            'not_claimed': ['3.3 complete numeric interaction', '3.4 complete numeric wire semantics',
                'Expert multi/PD tab combinations beyond source audit', 'new persistence or topology runtime']}
    finally:
        close_probe(page, parent.context, observe_request, observe_response, observe_console,
                    observe_pageerror, report, sys.exc_info()[0] is not None)


def pd_errors():
    # ValidatePD checks decode first; its one independent empty unit has no
    # node. This is different from both ordinary distributed-node count and
    # the disk tier's required container path.
    return [{'field': 'placement.pd', 'code': 'PD_NODE_REQUIRED', 'params': {'role': 'decode'}}]


def expected_pd(engine):
    placement = {'mode': PD_MODES[engine],
        'roles': {'decode': {'tp': 1, 'units': [{}]}, 'prefill': {'tp': 1, 'units': [{}]}}}
    if engine == 'vllm':
        placement['fallback'] = 'decode'
        placement['kv_load_failure'] = 'fail'
    elif engine == 'sglang':
        placement['fallback'] = 'fail'
    return placement


def expected_errors(request):
    if request.get('placement', {}).get('pd') is not None:
        return pd_errors()
    if request.get('nnodes') == 2:
        # Automatic placement asks for placement.tp before it counts named nodes, so that
        # is the error a multi-node request without nodes reports first.
        if request.get('placement_mode') != 'manual':
            return [{'field': 'placement_mode', 'code': 'AUTO_PLACEMENT_TP_REQUIRED'}]
        return [{'field': 'node_uuids', 'code': 'DISTRIBUTED_NODE_COUNT',
                 'params': {'actual': 0, 'required': 2}}]
    quick = request.get('quick_params', {})
    if quick.get('kv_disk_offload') is True and not quick.get('shared_kv_dir'):
        return [{'field': 'shared_kv_dir', 'code': 'QUICK_PARAM_PATH_INVALID',
                 'params': {'key': 'shared_kv_dir'}}]
    return []


def fixture_identity(request, fixture_body, same, label):
    # fixture_body is the consumer's literal expected_body fixture oracle,
    # never a request read from its existing page. All six imported specs name
    # the same audited fake image; the override consumer only changes its own
    # separate page's image after selecting the spec.
    keys = ('model_id', 'template_id', 'template_spec_source', 'template_spec_name',
            'engine', 'target_vendor', 'scenario')
    expected = {key: fixture_body[key] for key in keys}
    expected['image'] = 'gpuworks/fake-engine:test'
    same({key: request.get(key) for key in expected}, expected, label)


def close_probe(page, context, request_listener, response_listener, console_listener,
                pageerror_listener, report, primary_failure):
    cleanup_errors = []
    try:
        page.close()
        report['numeric_probe_page']['closed'] = page.is_closed()
        check(report['numeric_probe_page']['closed'] is True, 'numeric probe page remained open after close')
    except Exception as error:
        cleanup_errors.append({'stage': 'close-page', 'error': str(error)})
    for owner, event, listener in ((context, 'request', request_listener),
            (page, 'response', response_listener), (page, 'console', console_listener),
            (page, 'pageerror', pageerror_listener)):
        try:
            owner.remove_listener(event, listener)
        except Exception as error:
            cleanup_errors.append({'stage': 'remove-listener-' + event, 'error': str(error)})
    if cleanup_errors:
        report['numeric_probe_page']['cleanup_errors'] = cleanup_errors
        if not primary_failure:
            raise AssertionError('numeric probe page cleanup failed: ' + repr(cleanup_errors))
