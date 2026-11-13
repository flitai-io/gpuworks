"""Real UI Auto/on/off probes; original one-create lifecycle stays authoritative.

6.3 only: every offered tri-state boolean on Quick and Expert, restored before
existing refusal/create/GET/process/cleanup. 6.1/6.5 are not claimed by this subset.
"""
import copy
from urllib.parse import urljoin

from playwright.sync_api import expect
from platform_token_read_revoke import check
from deployment_boolean_contracts import (
    ENGINE_DECIDES, POSITIONS, SURFACES, TRISTATE, assert_wire, evaluate_body,
    evaluate_body_after_edits, position_body, reviewed_profile, same,
)

FAMILY = 'boolean-tristate'


def _select_label(labels, position):
    return labels['workbench']['triState' + {'auto': 'Auto', 'on': 'On', 'off': 'Off'}[position]]


def _selected(control, text):
    selector = control.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-select-selector ')][1]")
    expect(selector.locator('.ant-select-selection-item')).to_have_text(text)


def _probe(page, labels, body, report, control, key, position, surface, launch):
    label = f'boolean-tristate-{key}-{position}-{surface}'
    marker = body['served_name'] + '-bool-' + key + '-' + position + '-' + surface
    route = page.get_by_role('textbox', name=labels['basic']['servedName'], exact=True)
    text = _select_label(labels, position)
    def action():
        launch.choose(page, control, text)
        # A current position need not change Form. This visible independent
        # field edit guarantees observation of the exact selected draft.
        route.fill(marker)
    record = launch.evaluation(page, action, lambda wire: wire.get('served_name') == marker, report, label)
    assert_wire(record['request'], position_body(body, key, position, marker), key, position, label)
    same(record['response'].get('ok'), True, label + ' schedulable preview')
    _selected(control, text)
    report.setdefault('boolean_tristate_probes', []).append({
        'key': key, 'position': position, 'surface': surface,
        'request': copy.deepcopy(record['request']), 'checkpoint': label})


def _restore(page, labels, body, report, control, key, surface, launch):
    baseline = body['quick_params'].get(key)
    check(baseline is None or type(baseline) is bool, 'boolean fixture baseline is not boolean/unset')
    position = 'auto' if key not in body['quick_params'] else 'on' if baseline else 'off'
    text = _select_label(labels, position)
    route = page.get_by_role('textbox', name=labels['basic']['servedName'], exact=True)
    def action():
        launch.choose(page, control, text)
        route.fill(body['served_name'])
    label = 'boolean-tristate-' + key + '-restore-' + surface
    record = launch.evaluation(page, action,
        lambda wire: wire.get('served_name') == body['served_name'], report, label)
    # The control was edited during this case, and ADR 0020 keeps an edited key
    # explicit for the rest of the session, so restoring the original value still
    # travels - with the original value, which is the point: the draft returns to
    # the fixture, not to the key's absence. A key the case never touched must
    # stay absent, so any other quick value is still an error.
    expected = evaluate_body(body)
    authored = {key: body['quick_params'][key]} if key in body['quick_params'] else {}
    if authored:
        expected['quick_params'] = authored
    else:
        expected.pop('quick_params', None)
    same(record['request'], expected, label + ' complete original draft')
    _selected(control, text)


def exercise(page, labels, body, report):
    import deployment_launch_checks as launch
    if not launch.enabled(report, FAMILY):
        return
    report['last_step'] = 'boolean-tristate-reviewed-universe'
    response = page.request.get(urljoin(page.url, '/api/v1/engines'))
    check(response.status == 200, 'boolean engine dictionary GET failed')
    profiles = [p for p in response.json()['items'] if p['name'] == body['engine']]
    check(len(profiles) == 1, 'boolean engine dictionary identity is ambiguous')
    reviewed = reviewed_profile(profiles[0])
    # Read-only metadata is checked against the owned image/fixture facts; it
    # does not decide which keys to test or become an expected wire value.
    stable = launch.probe(page, labels, body, report,
        'boolean-tristate-initial-draft', body['quick_params'])
    same(stable['request'], evaluate_body(body), 'boolean exact initial evaluate body')
    same(stable['response'].get('engine_decides_param_keys', []),
        list(ENGINE_DECIDES[body['engine']]), 'boolean fixture engine-decided metadata')
    keys = TRISTATE[body['engine']]
    same(sorted(key for key, row in reviewed.items() if row[1] is None or key in ENGINE_DECIDES[body['engine']]),
        sorted(keys), 'boolean complete offered tri-state universe')
    check(keys, 'empty tri-state universe cannot prove 6.3')
    evidence = {'expected': list(keys), 'expected_surfaces': SURFACES[body['engine']], 'tested': [],
        'not_offered_on_quick': [],
        'dictionary_booleans': sorted(reviewed),
        'two_state': sorted(set(reviewed) - set(keys)),
        'static_not_tristate': ['health_check', 'tokenize_enabled', 'sleepable',
            'partition capability choice', 'extra mount read_only'],
        'scope': '6.3 only; current owned standalone model/image; all offered Auto controls'}
    report['boolean_tristate_universe'] = evidence
    for surface in ('quick', 'expert'):
        launch.mode(page, labels, surface)
        if surface == 'expert':
            page.get_by_role('tab', name=labels['page']['step']['performance'], exact=True).click()
        for key in keys:
            row = reviewed[key]
            title = row[5] if report['locale'] == 'en' else row[6]
            report['last_step'] = f'boolean-tristate-{key}-control-{surface}'
            control = page.get_by_role('combobox', name=title, exact=True)
            if key not in SURFACES[body['engine']][surface]:
                expect(control).to_have_count(0)
                expect(page.get_by_role('switch', name=title, exact=True)).to_have_count(0)
                expect(page.get_by_text(title, exact=True)).to_have_count(0)
                evidence['not_offered_on_quick'].append({'key': key,
                    'reason': 'DeployQuickForm quickParamSpecs applies the reviewed profile.quick_form_keys whitelist before layout; Expert Performance uses full visible performance specs'})
                continue
            expect(control).to_have_count(1)
            expect(control).to_be_visible()
            expect(control).to_be_enabled()
            for position in POSITIONS:
                _probe(page, labels, body, report, control, key, position, surface, launch)
            _restore(page, labels, body, report, control, key, surface, launch)
            evidence['tested'].append({'surface': surface, 'key': key, 'positions': list(POSITIONS)})
    expected = [{'surface': surface, 'key': key, 'positions': list(POSITIONS)}
                for surface in ('quick', 'expert') for key in SURFACES[body['engine']][surface]]
    same(evidence['tested'], expected, 'boolean complete tri-state evidence matrix')
    launch.mode(page, labels, 'quick')
    # Mode changes are not assumed to emit requests; the real served-name
    # roundtrip provides an exact final wire checkpoint before old lifecycle.
    restored = launch.probe(page, labels, body, report,
        'boolean-tristate-restored-fixture', body['quick_params'])
    edited = [k for surface in ('quick', 'expert')
              for k in SURFACES[body['engine']][surface]]
    same(restored['request'], evaluate_body_after_edits(body, edited),
         'boolean final complete independent draft')
    report['last_step'] = 'boolean-tristate-complete-and-restored'
