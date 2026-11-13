"""Full current-fixture boolean inventory; no mutation of the original draft.

6.1 includes all dictionary fields, real static checkboxes/switches, capability
placeholders and conditional mount rows. Mode/tier/partition choices remain
explicitly typed as auxiliary buttons, never falsely called ARIA switches.
"""
import copy
import json
from urllib.parse import urljoin

from playwright.sync_api import expect
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import check
from deployment_boolean_contracts import ENGINE_DECIDES, TRISTATE, evaluate_body, reviewed_profile, same
from deployment_boolean_checks import _selected, _select_label
from deployment_boolean_inventory_contracts import (
    FIXTURE_PARTITION, QUICK, SLEEP_SUPPORTED, complete, fixture_hardware, health_contract, health_source, initial, rows,
)
from deployment_boolean_inventory_probe import exercise as conditional_mount

FAMILY = 'boolean-inventory'


def _enabled(control, disabled, record, evidence):
    actual = control.is_disabled()
    record['expected_disabled'], record['actual_disabled'] = disabled, actual
    if actual != disabled:
        evidence['gaps'].append({'field': record['id'], 'expected_disabled': disabled,
            'actual_disabled': actual, 'reason': record.get('disabled_reason')})


def _reason(scope, text, record, evidence):
    actual = scope.inner_text()
    record['expected_disabled_reason'] = text
    record['actual_reason_text'] = actual
    if text not in actual:
        evidence['gaps'].append({'field': record['id'], 'missing_disabled_reason': text,
                                 'actual_text': actual})


def _switch(page, title, checked, disabled, record, evidence, role='switch'):
    control = page.get_by_role(role, name=title, exact=True)
    expect(control).to_have_count(1)
    expect(control).to_be_visible()
    expect(control).to_have_accessible_name(title)
    if checked:
        expect(control).to_be_checked()
    else:
        expect(control).not_to_be_checked()
    record.update(role=role, accessible_name=title, checked=checked, offered=True)
    _enabled(control, disabled, record, evidence)
    return control


def _choice(page, title, description, active, disabled, record, evidence, icon=''):
    control = page.locator('.deploy-choice-card').filter(has=page.get_by_text(title, exact=True))
    expect(control).to_have_count(1)
    expect(control).to_be_visible()
    name = ((icon + ' ') if icon else '') + title + ' ' + description
    expect(control).to_have_accessible_name(name)
    same('is-active' in (control.get_attribute('class') or '').split(), active,
         record['id'] + ' source-defined visual active state')
    record.update(role='button', accessible_name=name, offered=True, active=active)
    _enabled(control, disabled, record, evidence)
    if disabled:
        _reason(control, description, record, evidence)
    return control


def _absent(page, title, record, reason):
    for role in ('switch', 'combobox', 'checkbox'):
        expect(page.get_by_role(role, name=title, exact=True)).to_have_count(0)
    expect(page.get_by_text(title, exact=True)).to_have_count(0)
    record.update(offered=False, source_reason=reason)


def _dictionary(page, labels, body, report, surface, key, evidence):
    row = rows(body['engine'])[key]
    title = row[5] if report['locale'] == 'en' else row[6]
    record = {'id': 'dictionary/' + surface + '/' + key, **initial(body, key)}
    report['last_step'] = 'boolean-inventory-' + key + '-' + surface
    kind = QUICK[key] if surface == 'quick' else 'expert'
    if kind == 'absent':
        _absent(page, title, record,
            'DeployQuickForm quickParamSpecs whitelist excludes this key before layout/advanced grouping; Expert reads full group')
    elif kind == 'tier-choice':
        _choice(page, labels['workbench']['kv']['disk'], labels['workbench']['kv']['memoryRequired'],
            False, True, record, evidence)
        record['representation'] = 'auxiliary KV tier mode button; the actual boolean switch is tested on Expert'
    elif key == 'kv_disk_offload':
        control = _switch(page, labels['performance']['diskTier'], False, True, record, evidence)
        # The CPU tier is independently checked off; a disabled switch must
        # explain that prerequisite rather than only describe disk transport.
        _reason(control.locator('xpath=ancestor::div[contains(@class,"deploy-kv-tier")][1]'),
            labels['workbench']['kv']['memoryRequired'], record, evidence)
    elif kind == 'feature-select' or key in TRISTATE[body['engine']] or key in ENGINE_DECIDES[body['engine']]:
        control = page.get_by_role('combobox', name=title, exact=True)
        expect(control).to_have_count(1)
        expect(control).to_be_visible()
        expect(control).to_have_accessible_name(title)
        _selected(control, _select_label(labels, record['position']))
        record.update(role='combobox', accessible_name=title, offered=True)
        _enabled(control, False, record, evidence)
    else:
        _switch(page, title, record['position'] == 'on', False, record, evidence)
    evidence['records'].append(record)


def _mount_rows(page, labels, body, evidence, surface):
    expected = body['container_overrides'].get('extra_mounts', [])
    actual = page.locator('.image-defaults-mount-row')
    expect(actual).to_have_count(len(expected))
    observed = []
    for index, mount in enumerate(expected):
        check(type(mount['read_only']) is bool, 'fixture mount readonly is not boolean')
        checkbox = actual.nth(index).get_by_role('checkbox', name=labels['containerOverrides']['readOnly'], exact=True)
        expect(checkbox).to_have_count(1)
        expect(checkbox).to_be_visible()
        expect(checkbox).to_be_enabled()
        if mount['read_only']:
            expect(checkbox).to_be_checked()
        else:
            expect(checkbox).not_to_be_checked()
        observed.append({'read_only': mount['read_only'], 'role': 'checkbox',
                         'accessible_name': labels['containerOverrides']['readOnly']})
    evidence['records'].append({'id': surface + '/mount-rows', 'offered': True,
        'fixture_rows': observed, 'empty_rows_not_claimed_as_checkbox_proof': not expected})


def _quick_static(page, labels, common, body, report, evidence):
    for key, title in [('health_check', common['healthCheck']['enable']),
                       ('tokenize_enabled', common['tokenize']['enable'])]:
        report['last_step'] = 'boolean-inventory-' + key + '-global'
        health = evidence['health_source']['expected_health'] if key == 'health_check' else True
        record = {'id': 'global/' + key, 'wire': 'omitted',
            'value_source': evidence['health_source'] if key == 'health_check' else 'TokenizeField default on'}
        check(key not in body, key + ' fixture must be originally unstated')
        control = _switch(page, title, health, not health, record, evidence, role='checkbox')
        if key == 'health_check' and not health:
            _reason(control.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]"),
                    common['healthCheck']['unsupported'], record, evidence)
        evidence['records'].append(record)
    report['last_step'] = 'boolean-inventory-sleepable-quick'
    record = {'id': 'quick/sleepable', 'wire': 'omitted', 'disabled_reason': labels['workbench']['engineUnsupported']}
    supported = SLEEP_SUPPORTED[body['engine']]
    _choice(page, labels['workbench']['resource']['sleepable'],
        labels['workbench']['resource']['sleepableDescription'] if supported else labels['workbench']['engineUnsupported'],
        False, not supported, record, evidence, icon='moon')
    evidence['records'].append(record)
    report['last_step'] = 'boolean-inventory-partition-capability-quick'
    record = {'id': 'quick/partition-capability', 'representation': 'auxiliary numeric-partition choice, not a boolean parameter'}
    _choice(page, labels['workbench']['resource']['partition'],
        labels['workbench']['resource']['fullGpu'], False, False, record, evidence)
    evidence['records'].append(record)
    report['last_step'] = 'boolean-inventory-expert-parallel-capability-quick'
    record = {'id': 'quick/expert-parallel-capability', 'representation': 'unsupported capability placeholder; no boolean spec in this non-MoE fixture'}
    _choice(page, labels['workbench']['parallel']['expert'], labels['workbench']['parallel']['moeOnly'],
            False, True, record, evidence)
    evidence['records'].append(record)
    _mount_rows(page, labels, body, evidence, 'quick')


def _memory_tier(page, labels, body, evidence):
    record = {'id': 'expert/performance/memory-tier'}
    if body['engine'] == 'vllm':
        _switch(page, labels['performance']['cpuTier'], False, False, record, evidence)
        record.update(wire='omitted', representation='real UI memory-tier-enabled switch; capacity remains numeric')
        expect(page.get_by_text(labels['performance']['cpuTierHelp'], exact=True)).to_be_visible()
    else:
        expect(page.get_by_role('switch', name=labels['performance']['cpuTier'], exact=True)).to_have_count(0)
        expect(page.get_by_text(labels['performance']['cpuTier'], exact=True)).to_have_count(0)
        record.update(offered=False, source_reason='SGLang has no kv_offloading_size dictionary spec, so Performance renders no memory tier')
    evidence['records'].append(record)


def _expert_static(page, labels, body, report, evidence):
    page.get_by_role('tab', name=labels['page']['step']['resource'], exact=True).click()
    report['last_step'] = 'boolean-inventory-sleepable-expert'
    record = {'id': 'expert/resource/sleepable', 'wire': 'omitted',
              'disabled_reason': labels['workbench']['engineUnsupported']}
    control = _switch(page, labels['resource']['sleepable'], False,
                      not SLEEP_SUPPORTED[body['engine']], record, evidence)
    if not SLEEP_SUPPORTED[body['engine']]:
        _reason(control.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]"),
                labels['workbench']['engineUnsupported'], record, evidence)
    evidence['records'].append(record)
    page.get_by_role('tab', name=labels['page']['step']['runtime'], exact=True).click()
    _mount_rows(page, labels, body, evidence, 'expert/runtime')


def exercise(page, labels, body, report, token):
    import deployment_launch_checks as launch
    if not launch.enabled(report, FAMILY):
        return
    check(bool(token), 'boolean inventory needs explicit readonly collection authentication')
    common = json.loads((ROOT / f'ui/src/i18n/locales/{report["locale"]}/common.json').read_text())
    evidence = report['boolean_inventory'] = {'records': [], 'gaps': [],
        'scope': '6.1 current owned standalone model/image; every dictionary and actual static/conditional boolean surface'}
    response = page.request.get(urljoin(page.url, '/api/v1/engines'))
    check(response.status == 200, 'inventory dictionary GET failed')
    profile = next(p for p in response.json()['items'] if p['name'] == body['engine'])
    evidence['observed_boolean_profile'] = {'name': profile['name'], 'sleep': profile['sleep'],
        'param_specs': [spec for spec in profile['param_specs'] if spec['type'] == 'boolean']}
    evidence['independent_fixture_body'] = copy.deepcopy(body)
    evidence['health_source'] = health_source(body)
    report['last_step'] = 'boolean-inventory-effective-health-source'
    base = urljoin(page.url, '/').rstrip('/')
    status, model = http(base, 'GET', '/api/v1/models/' + str(body['model_id']), token=token)
    check(status == 200, 'inventory independent registered model GET failed')
    status, template = http(base, 'GET', '/api/v1/inference-templates/model/' + report['owned_template_model_id'], token=token)
    check(status == 200, 'inventory independent effective template GET failed')
    evidence['observed_health_model'] = {key: model.get(key) for key in ('id', 'source', 'operations')}
    evidence['observed_health_template'] = {'id': template['id'], 'specs': [
        {key: spec.get(key) for key in ('name', 'source', 'display_name_en', 'api')}
        for spec in template['specs']]}
    health_contract(body, model, template)
    reviewed_profile(profile)
    report['last_step'] = 'boolean-inventory-fixture-hardware'
    status, node = http(urljoin(page.url, '/').rstrip('/'), 'GET',
        '/api/v1/nodes/' + body['node_uuid'], token=token)
    check(status == 200, 'boolean inventory node independent GET failed')
    fixture_hardware(node, body['node_uuid'])
    evidence['observed_fixture_hardware'] = {'gpu_count': node['gpu_count'],
        'gpu_vendor': node['gpu_vendor'], 'runtime_driver': node['runtime_driver'],
        'gpus': node['gpus'], 'source_contract': {
            'startup_gpu': 'A100x1,24576', 'no_inject_dispatch_only': '1',
            'safety_protocol': 1, 'asset_files': ['libvgpu.so'],
            'NVIDIA_behavior_probe_exempt': True,
            'scope': 'synthetic facts and control readiness, not real GPU isolation'}}
    same(profile['sleep']['supported'], SLEEP_SUPPORTED[body['engine']], 'inventory reviewed engine sleep support')
    stable = launch.probe(page, labels, body, report, 'boolean-inventory-initial-wire', body['quick_params'])
    same(stable['request'], evaluate_body(body), 'inventory independent initial full wire')
    same(stable['response'].get('engine_decides_param_keys', []),
        list(ENGINE_DECIDES[body['engine']]), 'inventory independent Auto-source metadata')
    if body['placement_mode'] == 'auto':
        same({key: stable['response'].get('pool_partition_capability', {}).get(key)
              for key in FIXTURE_PARTITION}, FIXTURE_PARTITION,
             'inventory automatic eligible-pool ready NVIDIA capability')
    launch.mode(page, labels, 'quick')
    _quick_static(page, labels, common, body, report, evidence)
    header = page.locator('.deploy-advanced-collapse .ant-collapse-header')
    expect(header).to_have_count(1)
    was_open = header.get_attribute('aria-expanded') == 'true'
    if not was_open:
        header.click()
    for key in rows(body['engine']):
        _dictionary(page, labels, body, report, 'quick', key, evidence)
    if not was_open:
        header.click()
    launch.mode(page, labels, 'expert')
    for group in ('model', 'performance', 'performance_advanced'):
        selected = [key for key, row in rows(body['engine']).items() if row[2] == group]
        if not selected:
            continue
        page.get_by_role('tab', name=labels['page']['step']['model' if group == 'model' else 'performance'], exact=True).click()
        if group == 'performance':
            _memory_tier(page, labels, body, evidence)
        if group == 'performance_advanced':
            header = page.locator('.deploy-expert-card .ant-collapse-header')
            expect(header).to_have_count(1)
            expect(header).to_have_text(labels['performance']['advanced'])
            if header.get_attribute('aria-expanded') != 'true':
                header.click()
        for key in selected:
            _dictionary(page, labels, body, report, 'expert', key, evidence)
    _expert_static(page, labels, body, report, evidence)
    evidence['records'].append(conditional_mount(page, labels, report, token, launch))
    launch.mode(page, labels, 'quick')
    restored = launch.probe(page, labels, body, report, 'boolean-inventory-restored-wire', body['quick_params'])
    # This case reads each control's state and never edits one, so nothing is
    # authored and the evaluation carries no quick_params (ADR 0020 rule 3); the
    # fixture's values are still the draft, which the response and the controls
    # are checked against elsewhere.
    same(restored['request'], evaluate_body(body),
         'inventory retained original complete fixture draft')
    report['last_step'] = 'boolean-inventory-complete-contract'
    complete(evidence['records'], body['engine'])
    check(not evidence['gaps'], 'boolean inventory complete-contract gaps: ' + str(evidence['gaps']))
