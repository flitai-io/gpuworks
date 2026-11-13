"""RunPod text whitespace contracts through owned browser-created accounts.

No real provider resources are rented. Secrets are entered only with tracing
stopped; raw product log secrecy is deliberately not claimed by this helper.
"""
from contextlib import contextmanager
from urllib.parse import urlsplit
import sys

from playwright.sync_api import expect
from platform_token_read_revoke import button, check, collection
from provider_form_checks import family_enabled, form_action
from provider_endpoint_fixture import RunPodFixture
from provider_url_checks import assert_calls, close_fixture

ROOT = '/api/v1/providers'
AUTH_REASON = 'credential validation failed: runpod: invalid credentials (status 401)'
PODS = {'pods': [], 'pagination': {'nextCursor': None, 'hasNextPage': False}}


def pods_call(prefix='/proxy-a/v2', *, authorized=True):
    status = 200 if authorized else 401
    return {'method': 'GET', 'path': prefix + '/pods', 'status': status, 'authorized': authorized,
            'body': None, 'response': PODS if authorized else
            {'title': 'Unauthorized', 'status': 401, 'detail': 'invalid bearer token'}}


def registration_calls(keys_before, expected_keys, prefix='/proxy-a/v2'):
    """The fixture owns existing keys; platform identity ignores SSH comments."""
    check(bool(expected_keys) and len(expected_keys[-1].split()) >= 2, 'platform SSH key oracle is missing')
    platform = expected_keys[-1].strip()
    before = list(keys_before)
    calls = [pods_call(prefix), {'method': 'GET', 'path': prefix + '/account/ssh-keys',
        'status': 200, 'authorized': True, 'body': None, 'response': {'keys': before}}]
    present = any(key.split()[:2] == platform.split()[:2] for key in before)
    after = before if present else before + [platform]
    if not present:
        calls.append({'method': 'PUT', 'path': prefix + '/account/ssh-keys', 'status': 200,
            'authorized': True, 'body': {'keys': after}, 'response': {'keys': after}})
    return calls, after


def type_secret(control, value, context, artifacts):
    artifacts.stop_trace(context)
    # Empty/whitespace is a boundary value, not a credential. Registering it
    # would redact every space from unrelated evidence. Real keys are remembered.
    if value.strip():
        artifacts.remember(value)
    control.fill(value)
    expect(control).to_have_value(value)
    expect(control).to_have_attribute('type', 'password')


@contextmanager
def external_fixture(secret, report, label, keys):
    fixture, started = RunPodFixture(secret, keys=keys), False
    try:
        fixture.__enter__()
        started = True
        yield fixture
    finally:
        primary = sys.exc_info()[1]
        report.setdefault('provider_text_external_requests', {})[label] = list(fixture.requests)
        try:
            close_fixture(fixture, report, label, started=started)
        except Exception as cleanup:
            if primary is None:
                raise
            primary.add_note('External fixture cleanup also failed: ' + repr(cleanup))


class AccountFlow:
    """Share exact UI/wire/GET/ownership checks between the two new contracts."""
    def __init__(self, page, context, server, artifacts, report, copy, label, scope):
        self.page, self.context, self.server = page, context, server
        self.artifacts, self.report, self.copy, self.label = artifacts, report, copy, label
        self.scope, self.writes, self.errors = scope, [], []
        self.external_expected = {}
        self.records = report.setdefault(scope, [])
        self.dialog = page.get_by_role('dialog')
        self.name = self.dialog.get_by_label(copy['config']['name'], exact=True)
        self.endpoint = self.dialog.get_by_label(label('endpoint'), exact=True)
        self.key = self.dialog.get_by_label(label('api_key'), exact=True)
        check(collection(page.request.get(server.base + ROOT)) == [], scope + ': baseline is not empty')
        page.on('request', self.observe_write)
        page.on('response', self.observe_create)

    def observe_write(self, request):
        path = urlsplit(request.url).path
        if request.method in ('POST', 'PUT', 'PATCH', 'DELETE') and (path == ROOT or path.startswith(ROOT + '/')):
            self.writes.append({'method': request.method, 'path': path})

    def observe_create(self, response):
        if response.request.method != 'POST' or urlsplit(response.url).path != ROOT or response.status != 201:
            return
        try:
            body = response.json()
            ident = body.get('id') if isinstance(body, dict) else None
        except Exception:
            ident = None
        ledger = self.report.setdefault('remaining_resources', [])
        if ident is None or not any(row.get('kind') == 'provider' and row.get('id') == ident for row in ledger):
            ledger.append({'kind': 'provider', 'id': ident, 'owner': self.report['run_id'], 'check_scope': self.scope,
                'scope': 'owned browser POST in isolated private database',
                'reclaim': 'restore only this run private database and inspect the recorded provider id'})

    def assert_no_owned_accounts(self):
        check(not any(row.get('kind') == 'provider' and row.get('check_scope') == self.scope
                      for row in self.report.get('remaining_resources', [])),
              self.scope + ': an owned or ambiguous create response is not independently reclaimed')

    def finish(self):
        self.page.remove_listener('request', self.observe_write)
        self.page.remove_listener('response', self.observe_create)

    def read(self, account):
        path = ROOT + '/' + str(account['id'])
        expected = {'id': account['id'], 'kind': 'runpod', 'name': account['name'],
                    'endpoint': account['endpoint'], 'public_key': '', 'extra': {}}
        response = self.page.request.get(self.server.base + path)
        check(response.status == 200 and response.json() == expected,
              self.scope + ': independent GET differs or reveals secret fields')
        self.report['evidence'].append({'layer': 'persistence', 'path': path, 'body': expected, 'status': 200})
        return expected

    def row(self, account):
        row = self.page.locator('tr[data-row-key="' + str(account['id']) + '"]')
        expect(row).to_have_count(1)
        # CSS/accessibility text normalizes whitespace; textContent preserves
        # the actual raw name, including an account named only three spaces.
        expect(row.locator('.resource-table-name button')).to_have_js_property('textContent', account['name'])
        return row

    def new(self, name, endpoint, secret):
        self.artifacts.stop_trace(self.context)
        self.page.reload()
        button(self.page, self.copy['page']['addKind'].replace('{{kind}}', self.copy['kind']['runpod'])).click()
        expect(self.endpoint).to_have_value('https://api.runpod.io/v2')
        self.name.fill(name)
        self.endpoint.fill(endpoint)
        type_secret(self.key, secret, self.context, self.artifacts)

    def edit(self, account):
        expected = self.read(account)
        self.page.reload()
        path = ROOT + '/' + str(account['id'])
        with self.page.expect_response(lambda r: r.request.method == 'GET' and urlsplit(r.url).path == path) as loaded:
            button(self.row(account), self.copy['table']['edit']).click()
        check(loaded.value.status == 200 and loaded.value.json() == expected, 'browser edit projection differs')
        expect(self.name).to_have_value(account['name'])
        expect(self.endpoint).to_have_value(account['endpoint'])
        expect(self.key).to_have_value('')
        expect(form_action(self.dialog, self.copy['config']['save'])).to_be_enabled()

    def required(self, control, message, stage, external):
        self.report['last_step'] = stage
        before_writes, before_calls = len(self.writes), list(external.requests)
        control.fill('')
        form_action(self.dialog, self.copy['config']['add' if stage.endswith('create-empty') else 'save']).click()
        item = control.locator("xpath=ancestor::*[contains(concat(' ',normalize-space(@class),' '),' ant-form-item ')][1]")
        expect(item.get_by_text(message, exact=True)).to_be_visible()
        expect(control).to_have_value('')
        check(len(self.writes) == before_writes and external.requests == before_calls,
              'empty required field sent a write or external validation')
        self.records.append({'stage': stage, 'front_refused': True, 'provider_writes': 0})

    def submit(self, wire, *, stage, account=None, saved_name=None, saved_endpoint=None,
               rejected=False, expected_reason=AUTH_REASON):
        creating = account is None
        check(('kind' in wire) == creating, 'create/update wire oracle mode mismatch')
        method, path = ('POST', ROOT) if creating else ('PUT', ROOT + '/' + str(account['id']))
        self.report['last_step'] = stage
        expect(self.name).to_have_value(wire['name'])
        expect(self.endpoint).to_have_value(wire['endpoint'])
        before = len(self.writes)
        with self.page.expect_response(lambda r: r.request.method == method and urlsplit(r.url).path == path) as changed:
            form_action(self.dialog, self.copy['config']['add' if creating else 'save']).click()
        response, status = changed.value, 400 if rejected else 201 if creating else 200
        body = response.json()
        self.report['last_request'] = {'method': method, 'path': path, 'status': response.status,
            'expected_status': status, 'body': response.request.post_data_json, 'response': body}
        check(response.request.post_data_json == wire, stage + ': exact browser wire differs')
        check(self.writes[before:] == [{'method': method, 'path': path}], stage + ': duplicate/unrelated writes')
        check(response.status == status, stage + ': unexpected response status')
        if rejected:
            check(body == {'error': expected_reason}, stage + ': exact credential rejection differs')
            caption = self.copy['config']['operation']['add' if creating else 'update']
            text = self.copy['config']['operationFailed'].replace('{{operation}}', caption).replace('{{detail}}', expected_reason)
            expect(self.page.locator('.ant-message-error').filter(has_text=text)).to_have_text(text)
            expect(self.dialog).to_be_visible()
            expect(self.key).to_have_value(wire['api_key'])
            if creating:
                check(collection(self.page.request.get(self.server.base + ROOT)) == [], 'rejected create left a provider')
            else:
                self.read(account)
            self.errors.append({'method': method, 'path': path, 'status': 400})
            self.records.append({'stage': stage, 'exact_error': expected_reason, 'draft_retained': True})
        else:
            ident = body.get('id') if creating else account['id']
            check(type(ident) is int and ident > 0, 'accepted account has no positive integer id')
            name = wire['name'] if saved_name is None else saved_name
            endpoint = wire['endpoint'] if saved_endpoint is None else saved_endpoint
            check(body == {'id': ident, 'kind': 'runpod', 'name': name}, stage + ': response projection differs')
            account = {'id': ident, 'name': name, 'endpoint': endpoint}
            expect(self.dialog).to_be_hidden()
            if not self.artifacts.tracing:
                self.artifacts.start_trace(self.context)
            self.read(account)
        self.records.append({'stage': stage, 'method': method, 'status': status,
            'body_without_secret': {k: v for k, v in wire.items() if k != 'api_key'},
            'api_key_present': 'api_key' in wire, 'exact_wire_checked': True})
        return account

    def watch_external(self, fixture):
        self.external_expected[id(fixture)] = {'fixture': fixture, 'start': len(fixture.requests), 'expected': []}

    def verify_external(self, fixture, before, expected, stage):
        assert_calls(fixture.requests[before:], expected, stage)
        self.external_expected[id(fixture)]['expected'].extend(expected)

    def verify_all_external(self):
        for entry in self.external_expected.values():
            assert_calls(entry['fixture'].requests[entry['start']:], entry['expected'],
                         self.scope + ': complete external call ledger')

    def sync(self, account, fixture, stage):
        self.report['last_step'] = stage
        self.page.reload()
        path, before = ROOT + '/' + str(account['id']) + '/sync', len(fixture.requests)
        before_writes = len(self.writes)
        with self.page.expect_response(lambda r: r.request.method == 'POST' and urlsplit(r.url).path == path) as synced:
            button(self.row(account), self.copy['table']['sync']).click()
        check(synced.value.status == 200 and synced.value.request.post_data is None
              and synced.value.json() == {'count': 0, 'instances': []}, 'actual UI Sync contract differs')
        check(self.writes[before_writes:] == [{'method': 'POST', 'path': path}], 'UI Sync sent duplicate/unrelated writes')
        self.verify_external(fixture, before, [pods_call()], stage)
        self.read(account)
        self.records.append({'stage': stage, 'sync_status': 200, 'stored_secret_authenticated': True})

    def remove(self, account):
        self.report['last_step'] = self.scope + '-ui-delete'
        self.page.reload()
        row = self.row(account)
        button(row, self.copy['table']['delete']).click()
        path = ROOT + '/' + str(account['id'])
        before_writes = len(self.writes)
        with self.page.expect_response(lambda r: r.request.method == 'DELETE' and urlsplit(r.url).path == path) as deleted:
            button(self.page.get_by_role('tooltip'), self.copy['table']['delete']).click()
        check(deleted.value.status == 200 and deleted.value.request.post_data is None, 'owned provider UI deletion failed')
        check(self.writes[before_writes:] == [{'method': 'DELETE', 'path': path}], 'UI delete sent duplicate/unrelated writes')
        expect(row).to_have_count(0)
        check(self.page.request.get(self.server.base + path).status == 404, 'deleted account detail still readable')
        check(all(row['id'] != account['id'] for row in collection(self.page.request.get(self.server.base + ROOT))),
              'owned provider still appears in independent list')
        self.report['remaining_resources'] = [row for row in self.report.get('remaining_resources', [])
            if not (row.get('kind') == 'provider' and row.get('id') == account['id'])]
        self.report['evidence'].append({'layer': 'cleanup', 'provider_id': account['id'], 'ui_delete': 200, 'detail': 404})


def exercise_text_values(page, context, server, artifacts, report, copy, label, default_fixture, expected_keys):
    if not family_enabled(report, 'text-values'):
        return []
    check(default_fixture is not None, 'text-values requires default TLS fixture before server startup')
    secret = 'e2e-text-values-' + report['run_id']
    artifacts.remember(secret)
    flow = AccountFlow(page, context, server, artifacts, report, copy, label, 'provider_text_values')
    base_name = 'e2e-' + report['run_id'] + '-text'
    try:
        with external_fixture(secret, report, 'text-values', expected_keys) as fixture:
            endpoint = fixture.base + '/proxy-a/v2'
            flow.watch_external(fixture)
            flow.watch_external(default_fixture)
            default_start = len(default_fixture.requests)

            def create(name, raw_endpoint, key, stage, *, default=False):
                upstream = default_fixture if default else fixture
                protocol = default_fixture.protocol if default else fixture
                prefix = '/v2' if default else '/proxy-a/v2'
                before, keys = len(upstream.requests), list(protocol.keys)
                flow.new(name, raw_endpoint, key)
                account = flow.submit({'kind': 'runpod', 'name': name, 'endpoint': raw_endpoint, 'api_key': key}, stage=stage)
                calls, after = registration_calls(keys, expected_keys, prefix)
                flow.verify_external(upstream, before, calls, stage)
                check(protocol.keys == after, 'creation changed existing external SSH keys unexpectedly')
                flow.edit(account)
                button(flow.dialog, copy['config']['cancel']).click()
                expect(flow.dialog).to_be_hidden()
                return account

            # Name creation retains whitespace; the empty name is front-refused.
            flow.new('', endpoint, secret)
            flow.required(flow.name, copy['config']['nameRequired'], 'provider-text-name-create-empty', fixture)
            check(collection(page.request.get(server.base + ROOT)) == [], 'empty create name persisted an account')
            # A whitespace-only name is refused in the browser. Before the form declared its
            # required rule with whitespace:true the value validated, a create went out, and the
            # server stored an account whose name is three spaces.
            report['last_step'] = 'provider-text-name-create-whitespace'
            flow.name.fill('   ')
            before, keys = len(fixture.requests), list(fixture.keys)
            before_writes = len(flow.writes)
            form_action(flow.dialog, copy['config']['add']).click()
            name_item = flow.name.locator("xpath=ancestor::*[contains(concat(' ',normalize-space(@class),' '),' ant-form-item ')][1]")
            expect(name_item.get_by_text(copy['config']['nameRequired'], exact=True)).to_be_visible()
            expect(flow.name).to_have_value('   ')
            check(len(flow.writes) == before_writes, 'whitespace-only create name sent a provider mutation')
            check(len(fixture.requests) == before and fixture.keys == keys,
                  'whitespace-only create name contacted the provider or altered account SSH keys')
            check(collection(page.request.get(server.base + ROOT)) == [],
                  'whitespace-only create name persisted an account')
            flow.records.append({'stage': 'provider-text-name-create-whitespace',
                                 'front_refused': True, 'provider_writes': 0})
            account = create('e2e-' + report['run_id'] + '-after-whitespace', endpoint, secret,
                             'provider-text-name-create-after-whitespace-refused')
            flow.edit(account)
            flow.required(flow.name, copy['config']['nameRequired'], 'provider-text-name-edit-empty', fixture)
            flow.read(account)
            clean_name = base_name + '-renamed'
            padded_name = '  ' + clean_name + '  '
            flow.name.fill(padded_name)
            before = len(fixture.requests)
            account = flow.submit({'name': padded_name, 'endpoint': endpoint}, account=account, saved_name=clean_name,
                                  stage='provider-text-name-edit-outerspace')
            flow.verify_external(fixture, before, [pods_call()], 'name edit trims outer spaces')
            # Same rule on the edit side: a whitespace-only name is refused in the browser, so the
            # stored name is untouched and no update is sent.
            report['last_step'] = 'provider-text-name-edit-whitespace'
            flow.edit(account)
            flow.name.fill('   ')
            before, before_writes = len(fixture.requests), len(flow.writes)
            form_action(flow.dialog, copy['config']['save']).click()
            edit_item = flow.name.locator("xpath=ancestor::*[contains(concat(' ',normalize-space(@class),' '),' ant-form-item ')][1]")
            expect(edit_item.get_by_text(copy['config']['nameRequired'], exact=True)).to_be_visible()
            expect(flow.name).to_have_value('   ')
            check(len(flow.writes) == before_writes, 'whitespace-only edit name sent a provider mutation')
            check(len(fixture.requests) == before, 'whitespace-only edit name contacted the provider')
            check(collection(page.request.get(server.base + ROOT))[0]['name'] == clean_name,
                  'whitespace-only edit name changed the stored name')
            flow.records.append({'stage': 'provider-text-name-edit-whitespace',
                                 'front_refused': True, 'provider_writes': 0})
            button(flow.dialog, copy['config']['cancel']).click()
            expect(flow.dialog).to_be_hidden()

            for suffix, raw in [('leading', '  ' + clean_name), ('trailing', clean_name + '  ')]:
                flow.edit(account)
                flow.name.fill(raw)
                before = len(fixture.requests)
                account = flow.submit({'name': raw, 'endpoint': endpoint}, account=account, saved_name=clean_name,
                                      stage='provider-text-name-edit-' + suffix)
                flow.verify_external(fixture, before, [pods_call()], 'name edit ' + suffix)

            # Endpoint edit clears/whitespace retain; outer spaces persist raw.
            for suffix, raw, saved in [('empty', '', endpoint), ('whitespace', '   ', endpoint),
                                       ('leading', '  ' + endpoint, '  ' + endpoint),
                                       ('trailing', endpoint + '  ', endpoint + '  '),
                                       ('outerspace', '  ' + endpoint + '  ', '  ' + endpoint + '  ')]:
                flow.edit(account)
                flow.endpoint.fill(raw)
                before = len(fixture.requests)
                account = flow.submit({'name': clean_name, 'endpoint': raw}, account=account, saved_endpoint=saved,
                                      stage='provider-text-endpoint-edit-' + suffix)
                flow.verify_external(fixture, before, [pods_call()], 'endpoint edit ' + suffix)
                flow.edit(account)
                button(flow.dialog, copy['config']['cancel']).click()

            # An empty edit stays undefined in form state and is absent in JSON.
            flow.edit(account)
            before = len(fixture.requests)
            account = flow.submit({'name': clean_name, 'endpoint': account['endpoint']}, account=account, stage='provider-text-key-edit-empty')
            flow.verify_external(fixture, before, [pods_call()], 'empty edit retains key')
            flow.sync(account, fixture, 'provider-text-key-edit-empty-sync')
            for suffix, key, rejected in [('whitespace', '   ', False), ('leading', ' ' + secret, True),
                                           ('trailing', secret + ' ', False), ('outerspace', ' ' + secret + ' ', True)]:
                flow.edit(account)
                type_secret(flow.key, key, context, artifacts)
                before = len(fixture.requests)
                wire = {'name': clean_name, 'endpoint': account['endpoint'], 'api_key': key}
                result = flow.submit(wire, account=account, rejected=rejected,
                                     stage='provider-text-key-edit-' + suffix)
                flow.verify_external(fixture, before, [pods_call(authorized=not rejected)], 'key edit ' + suffix)
                if rejected:
                    type_secret(flow.key, secret, context, artifacts)
                    before = len(fixture.requests)
                    account = flow.submit({**wire, 'api_key': secret}, account=account,
                                          stage='provider-text-key-edit-' + suffix + '-recovered')
                    flow.verify_external(fixture, before, [pods_call()], 'key edit recovery')
                else:
                    account = result
                flow.sync(account, fixture, 'provider-text-key-edit-' + suffix + '-sync')
            flow.remove(account)

            # Additional creates cover raw padded name and each optional endpoint boundary.
            for suffix, name, raw_endpoint, key, default in [
                ('name-leading', '  ' + base_name + '-created-left', endpoint, secret, False),
                ('name-trailing', base_name + '-created-right  ', endpoint, secret, False),
                ('name-outerspace', '  ' + base_name + '-created  ', endpoint, secret, False),
                ('endpoint-empty', base_name + '-ep-empty', '', default_fixture.protocol.api_key, True),
                ('endpoint-whitespace', base_name + '-ep-white', '   ', default_fixture.protocol.api_key, True),
                ('endpoint-leading', base_name + '-ep-left', '  ' + endpoint, secret, False),
                ('endpoint-trailing', base_name + '-ep-right', endpoint + '  ', secret, False),
                ('endpoint-outerspace', base_name + '-ep-pad', '  ' + endpoint + '  ', secret, False),
            ]:
                account = create(name, raw_endpoint, key, 'provider-text-create-' + suffix, default=default)
                flow.remove(account)

            required_key = copy['config']['fieldRequired'].replace('{{field}}', label('api_key'))
            for suffix, key, rejected in [('empty', '', False), ('whitespace', '   ', True),
                                           ('leading', ' ' + secret, True), ('trailing', secret + ' ', False),
                                           ('outerspace', ' ' + secret + ' ', True)]:
                name = base_name + '-key-' + suffix
                flow.new(name, endpoint, key)
                if suffix == 'empty':
                    flow.required(flow.key, required_key, 'provider-text-key-create-empty', fixture)
                    check(collection(page.request.get(server.base + ROOT)) == [], 'empty create key persisted an account')
                    type_secret(flow.key, secret, context, artifacts)
                    key = secret
                before, keys = len(fixture.requests), list(fixture.keys)
                wire = {'kind': 'runpod', 'name': name, 'endpoint': endpoint, 'api_key': key}
                stage = 'provider-text-key-create-' + suffix + ('-recovered' if suffix == 'empty' else '')
                account = flow.submit(wire, rejected=rejected, stage=stage)
                if rejected:
                    flow.verify_external(fixture, before, [pods_call(authorized=False)], 'key create ' + suffix)
                    type_secret(flow.key, secret, context, artifacts)
                    before, keys = len(fixture.requests), list(fixture.keys)
                    account = flow.submit({**wire, 'api_key': secret}, stage='provider-text-key-create-' + suffix + '-recovered')
                calls, after = registration_calls(keys, expected_keys)
                flow.verify_external(fixture, before, calls, 'accepted key create ' + suffix)
                check(fixture.keys == after, 'create key changed external SSH keys')
                flow.edit(account)
                button(flow.dialog, copy['config']['cancel']).click()
                flow.sync(account, fixture, 'provider-text-key-create-' + suffix + '-sync')
                flow.remove(account)
            flow.verify_all_external()
            flow.assert_no_owned_accounts()
            report['provider_text_default_requests'] = default_fixture.requests[default_start:]
            check(collection(page.request.get(server.base + ROOT)) == [], 'text-values chain left a provider')
            report['provider_text_value_scope'] = {'fields': ['name', 'endpoint', 'api_key'],
                'modes': ['create', 'edit'], 'boundaries': ['empty', 'whitespace', 'leading', 'trailing', 'outerspace'],
                'trailing_key_semantics': 'HTTP header trailing whitespace is stripped; secret never returned by GET',
                'length_or_7_4_claim': False}
            return list(flow.errors)
    finally:
        flow.finish()
