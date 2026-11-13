"""Real provider-name boundaries and browser-native single-line paste oracle.

Only the owned provider ID is renamed; restore its run-scoped name before the
original edit/concurrency/delete chain. URL/key fields have no invented text
limits. Name whitespace facts do not claim full required-field coverage.
"""
import json
import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from local_browser_fixture import ROOT
from provider_form_checks import form_action
from platform_token_read_revoke import button, check
from provider_form_checks import family_enabled

DETAIL = 'must contain valid text without control characters and at most 256 characters'
# One fewer than before the whitespace fix: a whitespace-only name is now refused in the browser
# instead of being accepted by the server with the old name retained.
ACCEPTED_UPDATE_COUNT = 7
REJECTED_UPDATE_COUNT = 2


def with_boundary_calls(report, baseline):
    """Insert the independently planned name updates after account creation."""
    calls = list(baseline)
    if family_enabled(report, 'boundaries'):
        position = 4 if family_enabled(report, 'secret') else 3
        calls[position:position] = [('GET', '/proxy-a/v2/pods', 200, True)] * ACCEPTED_UPDATE_COUNT
    return calls


def boundary_http_errors(report, path):
    return ([{'path': path, 'status': 400, 'method': 'PUT'}] * REJECTED_UPDATE_COUNT
            if family_enabled(report, 'boundaries') else [])


def exercise_name_boundaries(page, context, server, fixture, copy, label, path, original_name,
                             endpoint, writes, errors, report):
    if not family_enabled(report, 'boundaries'):
        return
    provider_id = int(path.rsplit('/', 1)[1])
    current_name = original_name
    initial_calls = len(fixture.requests)
    initial_writes = len(writes)
    records = report['provider_name_boundaries'] = []
    common = json.loads((ROOT / 'ui/src/i18n/locales' / report['locale'] / 'common.json').read_text())
    dialog = page.get_by_role('dialog')
    name_input = dialog.get_by_label(copy['config']['name'], exact=True)
    last_clipboard = None
    context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=server.base)

    def read(expected_name):
        response = page.request.get(server.base + path)
        expected = {'id': provider_id, 'name': expected_name, 'kind': 'runpod',
                    'endpoint': endpoint, 'public_key': '', 'extra': {}}
        check(response.status == 200 and response.json() == expected,
              'name boundary independent GET differs or exposes a secret')
        report['evidence'].append({'layer': 'persistence', 'path': path,
                                  'body': expected, 'status': 200})

    def open_editor():
        page.reload()
        row = page.get_by_role('row').filter(has=page.get_by_text(current_name, exact=True))
        expect(row.get_by_role('cell', name=current_name, exact=True)).to_have_text(current_name, timeout=12000)
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                  and response.request.method == 'GET') as loaded:
            button(row, copy['table']['edit']).click()
        check(loaded.value.status == 200, 'name boundary edit GET failed')
        expect(name_input).to_have_value(current_name, timeout=12000)
        expect(dialog.get_by_label(label('endpoint'), exact=True)).to_have_value(endpoint, timeout=12000)
        expect(dialog.get_by_label(label('api_key'), exact=True)).to_have_value('', timeout=12000)
        expect(form_action(dialog, copy['config']['save'])).to_be_enabled(timeout=12000)

    def save(value, *, saved=None, rejected=False, stage):
        nonlocal current_name
        report['last_step'] = 'provider-name-' + stage
        expect(name_input).to_have_value(value, timeout=12000)
        before_calls = len(fixture.requests)
        before_writes = len(writes)
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                  and response.request.method == 'PUT') as updated:
            form_action(dialog, copy['config']['save']).click()
        response = updated.value
        expected_wire = {'name': value, 'endpoint': endpoint}
        expected_status = 400 if rejected else 200
        report['last_request'] = {'method': 'PUT', 'path': path, 'body': response.request.post_data_json,
            'status': response.status, 'expected_status': expected_status, 'response': response.json()}
        check(response.request.post_data_json == expected_wire, 'name boundary browser PUT differs')
        check(response.status == expected_status,
              f'name boundary {stage}: expected {expected_status}, received {response.status}')
        check(len(writes) == before_writes + 1 and writes[-1]
              == {'method': 'PUT', 'path': path, 'body': expected_wire},
              'name boundary sent missing, duplicate or unrelated writes')
        if rejected:
            expected_error = {'code': 'FIELD_INVALID', 'error': 'name ' + DETAIL,
                              'params': {'field': 'name', 'detail': DETAIL}}
            check(response.json() == expected_error, 'name boundary FIELD_INVALID wire differs')
            reason = common['apiError']['FIELD_INVALID'].replace('{{detail}}', DETAIL).replace('{{field}}', 'name')
            message = copy['config']['operationFailed'].replace('{{operation}}', copy['config']['operation']['update'])
            message = message.replace('{{detail}}', reason)
            notice = page.locator('.ant-message-error').filter(has_text=message)
            expect(notice).to_be_visible(timeout=12000)
            expect(notice).to_have_text(message, timeout=12000)
            expect(dialog).to_be_visible(timeout=12000)
            expect(name_input).to_have_value(value, timeout=12000)
            check(len(fixture.requests) == before_calls, 'rejected name contacted the external provider')
            read(current_name)
            button(dialog, copy['config']['cancel']).click()
            expect(dialog).to_be_hidden(timeout=12000)
        else:
            check(saved is not None, 'accepted name scenario lacks an independent persisted oracle')
            check(response.json() == {'id': provider_id, 'kind': 'runpod', 'name': saved},
                  'accepted name update response differs')
            expect(dialog).to_be_hidden(timeout=12000)
            check([(request['method'], request['path'], request['status'], request['authorized'])
                   for request in fixture.requests[before_calls:]]
                  == [('GET', '/proxy-a/v2/pods', 200, True)],
                  'accepted name update did not validate the retained endpoint and secret exactly once')
            current_name = saved
            read(current_name)
            page.reload()
            expect(page.get_by_role('cell', name=current_name, exact=True)).to_have_text(current_name, timeout=12000)
        records.append({'stage': stage, 'input': value, 'input_code_points': len(value),
            'input_utf8_bytes': len(value.encode('utf-8')), 'status': expected_status,
            'saved_name': current_name, 'outbound_calls': 0 if rejected else 1})
        report['evidence'].append({'layer': 'request', 'method': 'PUT', 'path': path,
                                  'body': expected_wire, 'status': expected_status})

    def paste(raw):
        """Independent native input supplies platform paste normalization only."""
        nonlocal last_clipboard
        page.evaluate('value => navigator.clipboard.writeText(value)', raw)
        last_clipboard = raw
        check(page.evaluate('() => navigator.clipboard.readText()') == raw,
              'clipboard did not retain the independent raw boundary text')
        native = context.new_page()
        try:
            native.set_content('<label for="native-text">Native platform oracle</label><input id="native-text" type="text">')
            oracle = native.get_by_label('Native platform oracle', exact=True)
            oracle.focus()
            oracle.press('Control+V')
            expect(oracle).not_to_have_value('', timeout=12000)
            # The clipboard source is browser platform state, independent of
            # the provider component and management API. Never derive expected
            # persistence from the product input or its response.
            normalized = oracle.input_value()
        finally:
            native.close()
        check(normalized != '', 'native platform paste oracle returned no text')
        check(page.evaluate('() => navigator.clipboard.readText()') == raw,
              'clipboard changed between native and product paste')
        name_input.focus()
        name_input.press('Control+A')
        name_input.press('Backspace')
        name_input.press('Control+V')
        expect(name_input).to_have_value(normalized, timeout=12000)
        return normalized

    try:
        open_editor()
        report['last_step'] = 'provider-name-empty-required'
        name_input.fill('')
        before_writes, before_calls = len(writes), len(fixture.requests)
        form_action(dialog, copy['config']['save']).click()
        item = name_input.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
        expect(item.get_by_text(copy['config']['nameRequired'], exact=True)).to_be_visible(timeout=12000)
        check(len(writes) == before_writes and len(fixture.requests) == before_calls,
              'empty name validation sent a mutation or external request')
        check(not errors, f'empty name validation caused a browser page error: {errors}')
        read(current_name)
        records.append({'stage': 'empty-required', 'input': '', 'mutation_count': 0, 'outbound_calls': 0})
        name_input.fill('   ')
        # A whitespace-only name is not a name. The form's required rule is declared with
        # whitespace:true, so the client refuses it: no PUT leaves the browser and the field
        # carries the nameRequired message. Without that flag antd validates the value, the
        # update goes out, and the server silently keeps the old name - which is what this
        # stage used to assert.
        report['last_step'] = 'provider-name-whitespace-refused-locally'
        before_writes = len(writes)
        before_calls = len(fixture.requests)
        form_action(dialog, copy['config']['save']).click()
        name_item = name_input.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
        expect(name_item.get_by_text(copy['config']['nameRequired'], exact=True)).to_be_visible(timeout=12000)
        expect(name_item).to_have_class(re.compile(r'ant-form-item-has-error'), timeout=12000)
        check(len(writes) == before_writes, 'a whitespace-only name sent a provider mutation')
        check(len(fixture.requests) == before_calls,
              'a whitespace-only name contacted the external provider')
        check(not errors, f'whitespace name validation caused a browser page error: {errors}')
        expect(dialog).to_be_visible(timeout=12000)
        read(current_name)
        records.append({'stage': 'whitespace-refused-locally', 'input': '   ',
                        'mutation_count': 0, 'outbound_calls': 0})
        name_input.fill(current_name)
        open_editor()
        padded = '  ' + original_name + '  '
        name_input.fill(padded)
        save(padded, saved=original_name, stage='padded-name-trimmed-on-update')
        # This existing owned ID temporarily has a one-code-point name. It is
        # never discovered/cleaned by that short name; the isolated DB and
        # registered provider ID remain its ownership/reclaim authority.
        for size, character in ((1, '界'), (255, '界'), (256, '𠀀')):
            open_editor()
            prefix = '' if size == 1 else 'e2e-' + report['run_id'] + '-'
            value = prefix + character * (size - len(prefix))
            check(len(value) == size, 'name boundary fixture is not the declared code-point length')
            name_input.fill(value)
            save(value, saved=value, stage=f'length-{size}-code-points')
        open_editor()
        oversized = current_name + '界'
        check(len(oversized) == 257, 'name boundary oversized fixture is not 257 code points')
        name_input.fill(oversized)
        save(oversized, rejected=True, stage='length-257-rejected')
        open_editor()
        mixed = 'e2e-' + report['run_id'] + '-中文English09 · <&>"\'!?，。'
        name_input.fill(mixed)
        save(mixed, saved=mixed, stage='unicode-and-punctuation')
        open_editor()
        raw_lines = 'e2e-' + report['run_id'] + '-first\r\nsecond\n第三行'
        report['last_step'] = 'provider-name-crlf-platform-paste'
        normalized = paste(raw_lines)
        check('\r' not in normalized and '\n' not in normalized
              and all(part in normalized for part in ('first', 'second', '第三行')),
              'native platform oracle did not yield the complete single-line text')
        report['provider_name_clipboard_oracle'] = {'raw_crlf': raw_lines, 'native_single_line': normalized,
                                                   'product_input_exact': normalized}
        save(normalized, saved=normalized, stage='crlf-single-line-saved')
        open_editor()
        raw_control = 'e2e-' + report['run_id'] + '-禁止\x01字符'
        report['last_step'] = 'provider-name-control-platform-paste'
        normalized_control = paste(raw_control)
        check(normalized_control == raw_control, 'native clipboard platform did not preserve the prohibited control')
        save(normalized_control, rejected=True, stage='control-character-rejected')
        open_editor()
        name_input.fill(original_name)
        save(original_name, saved=original_name, stage='original-name-restored')
        read(original_name)
        check(len(fixture.requests) == initial_calls + ACCEPTED_UPDATE_COUNT,
              'name boundary total external calls differ from the fixed scenario plan')
        check(len(writes) == initial_writes + ACCEPTED_UPDATE_COUNT + REJECTED_UPDATE_COUNT,
              'name boundary total browser writes differ from the fixed scenario plan')
        check(not errors, f'name boundary browser page errors: {errors}')
        report['provider_name_boundary_scope'] = {'field': 'name', 'maximum': 256, 'counting': 'Unicode code points',
            'original_name_restored': True, 'required_whitespace_front_refusal': False,
            'other_text_fields_whitespace_matrix': False, 'optional_field_length_limits': None}
        report['evidence'].append({'layer': 'ui', 'name_boundary_records': records,
                                  'clipboard_oracle': report['provider_name_clipboard_oracle']})
    finally:
        if last_clipboard is not None and page.evaluate('() => navigator.clipboard.readText()') == last_clipboard:
            page.evaluate('() => navigator.clipboard.writeText("")')
