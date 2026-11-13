"""Provider kind changes: schema-driven controls, defaults and discarded drafts.

Call before tracing or entering real credentials. The caller must still verify
its real RunPod create POST exactly; this helper never submits a provider.
"""
from urllib.parse import urlsplit
import json
import re

from playwright.sync_api import expect
from platform_token_read_revoke import check


def _localized(value, locale, fallback=''):
    if isinstance(value, str):
        return value
    if not isinstance(value, dict):
        return fallback
    return value.get(locale) or value.get('en') or fallback


def _field_rows(info, locale):
    """Expected controls come from the already captured kind schema, not DOM."""
    rows, seen = [], set()
    for field in info['credential_fields']:
        name = field['name']
        check(isinstance(name, str) and bool(name), 'credential schema has an empty field name')
        secret, required, extra = field['secret'], field['required'], field.get('extra', False)
        check(all(type(value) is bool for value in (secret, required, extra)),
              'credential schema flags must be JSON booleans')
        path = ('extra', name) if extra else (name,)
        check(path not in seen, 'credential schema repeats a form field path')
        seen.add(path)
        label = _localized(field.get('label'), locale, name)
        default = '' if secret else field.get('default_value', '')
        check(isinstance(default, str), 'credential default is not a string')
        rows.append({'path': list(path), 'label': label, 'secret': secret, 'required': required,
                     'type': 'password' if secret else 'text', 'default': default})
    return rows


def _public_schema(kinds, locale):
    infos = [info for info in kinds if info['kind'] != 'zstack']
    names = [info['kind'] for info in infos]
    check(len(names) == len(set(names)) and 'runpod' in names and len(names) > 1,
          'conditional checks require unique public kinds including RunPod and another kind')
    tables = {info['kind']: _field_rows(info, locale) for info in infos}
    families = {
        'api_key': any(row['path'] == ['api_key'] for rows in tables.values() for row in rows),
        'public_private': any({'public_key', 'private_key'} <= {
            row['path'][0] for row in rows if len(row['path']) == 1} for rows in tables.values()),
        'extra': any(len(row['path']) == 2 for rows in tables.values() for row in rows),
        'endpoint': any(row['path'] == ['endpoint'] for rows in tables.values() for row in rows),
    }
    check(all(families.values()), 'real public schemas do not expose all four assigned conditional field families')
    return infos, tables, families



def _switch_plan(names):
    """Cover every directed kind change, then return to RunPod."""
    others = [name for name in names if name != 'runpod']
    candidates = [target for kind in others for target in (kind, 'runpod')]
    candidates += [target for left in others for right in others if left != right
                   for target in (left, right)]
    candidates.append('runpod')
    current, plan = 'runpod', []
    for kind in candidates:
        if kind != current:
            plan.append(kind)
            current = kind
    return plan


def _provider_write(request):
    path = urlsplit(request.url).path
    root = '/api/v1/providers'
    return request.method in ('POST', 'PUT', 'PATCH', 'DELETE') and (path == root or path.startswith(root + '/'))


def exercise_conditional_fields(page, dialog, copy, kinds, locale, report, artifacts):
    """Exercise every directed public-kind change, discarding each typed draft.

    ProviderConfigModal.onChange clears credentials/extra, then applies only
    non-secret schema defaults. Name stays untouched. CredentialFieldInput is
    unmounted for absent fields (preserve=false), not merely hidden with CSS.
    The final real POST's exact body remains the caller's independent oracle
    for hidden values: no synthetic request or Form internals are read here.
    """
    check(getattr(artifacts, 'tracing', None) is False and getattr(artifacts, 'trace_number', None) == 0,
          'conditional secret drafts must be exercised before start_trace')
    check(locale in ('en', 'zh'), 'conditional checks require an en/zh contract locale')
    infos, tables, families = _public_schema(kinds, locale)
    by_kind = {info['kind']: info for info in infos}
    labels = {row['label'] for rows in tables.values() for row in rows}
    name_control = dialog.get_by_label(copy['config']['name'], exact=True)
    original_name = name_control.input_value()
    kind_control = dialog.get_by_label(copy['config']['type'], exact=True)
    writes, transitions = [], []

    def sent(request):
        if _provider_write(request):
            writes.append({'method': request.method, 'path': urlsplit(request.url).path})

    def caption(kind):
        return copy['kind'].get(kind) or _localized(by_kind[kind].get('label'), locale, kind)

    def assert_defaults(kind):
        report['last_step'] = 'provider-conditional-defaults-' + kind
        expect(dialog.locator('.ant-select-selection-item')).to_have_text(caption(kind))
        expect(name_control).to_have_value(original_name)
        expected = tables[kind]
        current_labels = {row['label'] for row in expected}
        for row in expected:
            control = dialog.get_by_label(row['label'], exact=True)
            expect(control).to_have_count(1)
            expect(control).to_be_visible()
            expect(control).to_be_enabled()
            expect(control).to_have_attribute('type', row['type'])
            expect(control).to_have_value(row['default'])
            check((control.get_attribute('aria-required') == 'true') == row['required'],
                  'conditional required flag differs from the real schema')
        for label in labels - current_labels:
            expect(dialog.get_by_label(label, exact=True)).to_have_count(0)
        # The kind combobox and name input are the two stable controls. This
        # also detects obsolete credential inputs left mounted without labels.
        expect(dialog.locator('.provider-config-form input')).to_have_count(len(expected) + 2)
        for hint, present in (('autodlProOnly', kind == 'autodl'), ('runpodSecureOnly', kind == 'runpod')):
            hint_control = dialog.get_by_text(copy['config'][hint], exact=True)
            if present:
                expect(hint_control).to_be_visible()
            else:
                expect(hint_control).to_have_count(0)
        expect(dialog.get_by_text(copy['config']['credentialsRetained'], exact=True)).to_have_count(0)
        check(not writes, 'switching provider credential controls sent a provider mutation')

    def fill_draft(kind, visit):
        report['last_step'] = 'provider-conditional-draft-' + kind
        check(artifacts.tracing is False, 'tracing started while entering conditional secret drafts')
        for row in tables[kind]:
            suffix = '-'.join(row['path'])
            value = f'e2e-conditional-{report["run_id"]}-{kind}-{visit}-{suffix}'
            if row['path'] == ['endpoint']:
                value = 'https://conditional-' + kind + '.invalid/' + str(visit)
            artifacts.remember(value)
            control = dialog.get_by_label(row['label'], exact=True)
            control.fill(value)
            expect(control).to_have_value(value)
        check(not writes, 'typing a conditional provider draft sent a provider mutation')

    def select(kind):
        report['last_step'] = 'provider-conditional-select-' + kind
        if 'provider_kind_control_probe' not in report:
            report['provider_kind_control_probe'] = {
                'expected_caption': copy['config']['type'],
                'named_count': kind_control.count(),
                'all_count': dialog.get_by_role('combobox').count(),
                'with_hidden_count': dialog.get_by_role('combobox', include_hidden=True).count(),
                'aria_snapshot': dialog.get_by_role('combobox').aria_snapshot(),
                'inputs': dialog.locator('input[role="combobox"]').evaluate_all(
                    '(elements) => elements.map(e => ({id:e.id, ariaLabel:e.getAttribute("aria-label"), '
                    'labelledBy:e.getAttribute("aria-labelledby"), ariaHidden:e.getAttribute("aria-hidden"), '
                    'labels:Array.from(e.labels || []).map(l => l.textContent), '
                    'display:getComputedStyle(e).display, visibility:getComputedStyle(e).visibility, '
                    'width:e.getBoundingClientRect().width,height:e.getBoundingClientRect().height}))')}
        expect(kind_control).to_have_attribute('role', 'combobox')
        selector = dialog.locator('.ant-select-selector')
        expect(selector).to_have_count(1)
        expect(selector).to_be_visible()
        selector.click()
        controls = kind_control.get_attribute('aria-controls')
        check(bool(controls), 'provider kind select has no controlled popup')
        popup = page.locator('[id=' + json.dumps(controls) + ']').locator('..')
        option = popup.locator('.ant-select-item-option').filter(
            has_text=re.compile('^' + re.escape(caption(kind)) + '$'))
        expect(option).to_have_count(1)
        option.click()
        expect(kind_control).to_have_attribute('aria-expanded', 'false')
        # 1.5: the chosen option has to actually take the selection, not merely close the popup.
        # The combobox shows that option's caption and no other, and reopening the popup marks
        # exactly that one option as selected.
        chosen = caption(kind)
        expect(dialog.locator('.ant-select-selection-item')).to_have_count(1)
        expect(dialog.locator('.ant-select-selection-item')).to_have_text(chosen)
        selector.click()
        popup = page.locator('[id=' + json.dumps(kind_control.get_attribute('aria-controls')) + ']').locator('..')
        selected = popup.locator('.ant-select-item-option-selected')
        expect(selected).to_have_count(1)
        expect(selected).to_have_text(chosen)
        page.keyboard.press('Escape')
        expect(kind_control).to_have_attribute('aria-expanded', 'false')
        assert_defaults(kind)

    page.on('request', sent)
    try:
        assert_defaults('runpod')
        previous = 'runpod'
        fill_draft(previous, 0)
        names = [info['kind'] for info in infos]
        plan = _switch_plan(names)
        for visit, kind in enumerate(plan, 1):
            select(kind)
            transitions.append({'from': previous, 'to': kind,
                                'nonsecret_defaults_restored': True, 'secret_drafts_cleared': True,
                                'absent_controls_unmounted': True, 'name_unchanged': True})
            if visit < len(plan):
                fill_draft(kind, visit)
            previous = kind
        assert_defaults('runpod')
        expected_pairs = {(left, right) for left in names for right in names if left != right}
        observed_pairs = {(step['from'], step['to']) for step in transitions}
        check(observed_pairs == expected_pairs, 'conditional checks omitted a public-kind transition')
        final_roots = {row['path'][0] for row in tables['runpod']}
        hidden_roots = {row['path'][0] for rows in tables.values() for row in rows} - final_roots
        report['provider_conditional_fields'] = {
            'kinds': [info['kind'] for info in infos], 'fields_by_kind': tables,
            'families': families, 'transitions': transitions, 'provider_mutations': writes,
            'directed_kind_pairs_checked': len(observed_pairs),
            'final_kind': 'runpod', 'name_unchanged': True, 'trace_started': False,
            'final_post_contract': {'credential_roots': sorted(final_roots),
                                    'must_omit_roots': sorted(hidden_roots),
                                    'proof': 'caller must check its actual full RunPod create POST'},
        }
        report['last_step'] = 'provider-conditional-pristine-runpod'
    finally:
        page.remove_listener('request', sent)
