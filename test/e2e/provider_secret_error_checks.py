"""Provider secret-bearing errors: raw product sinks before artifact redaction.

Provider account UI and its API routes offer no credential download/export.
The source audit covers CloudProvidersPage, ResourceTable, api.ts and provider
route inventory; generic storage/benchmark downloads are separate resources.
This case checks ordinary account GETs, hostile errors, original process logs
and their logstore projection. It makes no credential-download capability up.
"""
import hashlib
import json
import time
from urllib.parse import urlencode, urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import button, check, collection
from provider_default_endpoint_checks import EXISTING_KEY
from provider_form_checks import form_action
from provider_secret_error_fixture import SecretErrorRunPodFixture
from provider_url_checks import close_fixture

WARN_MESSAGE = 'authorize platform ssh key before creating an instance'
WARN_REASON = 'runpod: list SSH keys: status 400'
WARN_LINE = WARN_MESSAGE + ' err=' + WARN_REASON


def secret_errors_enabled(report):
    return report.get('check_family', 'all') in ('all', 'secret-errors', 'secret-errors-http')


def assert_raw_secret_free(artifacts, raw, scope):
    """Read/call this on original bytes, never on Artifacts.redact output."""
    check(not artifacts._scan_bytes(raw), scope + ' contains a registered secret')


def wait_raw_ledger(page, server, artifacts, offset, method, path, status, *, name=None, warning=False):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        raw = (server.work / 'server.log').read_bytes()
        assert_raw_secret_free(artifacts, raw, 'raw product log')
        try:
            lines = raw[offset:].decode('utf-8').splitlines()
        except UnicodeDecodeError:
            page.evaluate('() => new Promise(resolve => requestAnimationFrame(resolve))')
            continue
        ledgers = [line for line in lines if 'msg="http request"' in line
            and f'method={method} ' in line and f'path={path} ' in line
            and f'status={status} ' in line and (name is None or name in line)]
        warnings = [line for line in lines if 'level=WARN' in line and WARN_MESSAGE in line]
        if len(ledgers) == 1 and (not warning or len(warnings) == 1):
            if warning:
                check('err="' + WARN_REASON + '"' in warnings[0], 'raw WARN lost the concrete operation/status')
            return {'http_ledger': ledgers[0], 'raw_warn': warnings[0] if warning else None,
                    'raw_log_bytes': len(raw), 'raw_log_snapshot_sha256': hashlib.sha256(raw).hexdigest()}
        page.evaluate('() => new Promise(resolve => requestAnimationFrame(resolve))')
    raise AssertionError('known raw product HTTP ledger/WARN did not flush; empty logs are not secrecy evidence')


def collected_warning(page, server, artifacts):
    query = urlencode({'source': 'server', 'level': 'warn', 'q': '"' + WARN_MESSAGE + '"', 'with_total': '1'})
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        response = page.request.get(server.base + '/api/v1/logs/search?' + query)
        check(response.status == 200, 'secret-error WARN logstore query failed')
        assert_raw_secret_free(artifacts, response.body(), 'raw logstore GET response')
        body = response.json()
        if body.get('items'):
            check(body.get('total') == 1 and len(body['items']) == 1, 'secret-error WARN is not one unique record')
            row = body['items'][0]
            check(row['line'] == WARN_LINE and row['source'] == 'server' and row['level'] == 'warn'
                  and row['instance'] == '' and row['node'] == '', 'collected WARN differs from the fixed source/status fact')
            check(type(row['id']) is int and row['id'] > 0 and type(row['time_ms']) is int,
                  'collected WARN lacks its server log identity')
            return row
        page.evaluate('() => new Promise(resolve => requestAnimationFrame(resolve))')
    raise AssertionError('real secret-error WARN was not collected; an empty response cannot pass')


def exercise_provider_secret_errors(page, context, server, artifacts, report, copy, label, expected_keys):
    if not secret_errors_enabled(report):
        return []
    # Deliberately strict: family_enabled(all, http) would skip WARN in formal runs.
    http_only = report.get('check_family') == 'secret-errors-http'
    secret = 'e2e-secret-error-' + report['run_id']
    marker = 'e2e-' + report['run_id'] + '-secret-error-boundary'
    name = 'e2e-' + report['run_id'] + '-secret-errors'
    artifacts.remember(secret)
    artifacts.stop_trace(context)
    check(collection(page.request.get(server.base + '/api/v1/providers')) == [], 'prior provider account remains')
    fixture = SecretErrorRunPodFixture(secret, marker, keys=[EXISTING_KEY], key_error=not http_only)
    started = False
    try:
        fixture.__enter__()
        started = True
        endpoint = fixture.base + '/proxy-a/v2'
        offset = (server.work / 'server.log').stat().st_size
        page.reload()
        button(page, copy['page']['addKind'].replace('{{kind}}', copy['kind']['runpod'])).click()
        dialog = page.get_by_role('dialog')
        dialog.get_by_label(copy['config']['name'], exact=True).fill(name)
        dialog.get_by_label(label('endpoint'), exact=True).fill(endpoint)
        key = dialog.get_by_label(label('api_key'), exact=True)
        key.fill(secret)
        expect(key).to_have_attribute('type', 'password')
        report['last_step'] = 'provider-secret-errors-create'
        with page.expect_response(lambda response: urlsplit(response.url).path == '/api/v1/providers'
                                  and response.request.method == 'POST') as created:
            form_action(dialog, copy['config']['add']).click()
        response = created.value
        body = response.json()
        provider_id = body.get('id') if isinstance(body, dict) else None
        if response.status == 201:
            report.setdefault('remaining_resources', []).append({'kind': 'provider', 'id': provider_id,
                'name': name, 'scope': 'isolated SQLite database in private/',
                'reclaim': 'restore only this run private archive/DB for diagnosis'})
        check(response.status == 201 and type(provider_id) is int and provider_id > 0,
              'best-effort key registration error did not leave a valid created account')
        check(response.request.post_data_json == {'name': name, 'kind': 'runpod', 'endpoint': endpoint, 'api_key': secret}
              and body == {'id': provider_id, 'kind': 'runpod', 'name': name}, 'secret-error create wire/projection differs')
        expect(dialog).to_be_hidden(timeout=12000)
        path = f'/api/v1/providers/{provider_id}'
        sync_path = path + '/sync'
        report['evidence'].append({'layer': 'request', 'method': 'POST', 'path': '/api/v1/providers',
                                  'body': response.request.post_data_json, 'status': 201})

        def read(state):
            detail = page.request.get(server.base + path)
            assert_raw_secret_free(artifacts, detail.body(), 'raw provider detail GET')
            expected = {'id': provider_id, 'kind': 'runpod', 'name': name,
                        'endpoint': endpoint, 'public_key': '', 'extra': {}}
            check(detail.status == 200 and detail.json() == expected, 'ordinary provider detail exposed extra/cipher fields')
            listing = page.request.get(server.base + '/api/v1/providers')
            assert_raw_secret_free(artifacts, listing.body(), 'raw provider list GET')
            rows = collection(listing)
            check(len(rows) == 1 and set(rows[0])
                  == {'id', 'name', 'kind', 'category', 'state', 'instance_count',
                      'created_at', 'updated_at'},
                  'ordinary provider list exposed credential/cipher fields or extra accounts')
            row = rows[0]
            check((row['id'], row['name'], row['kind'], row['state'], row['instance_count'])
                  == (provider_id, name, 'runpod', state, 0), 'provider secret-error state differs')
            report['evidence'].append({'layer': 'persistence', 'secret_error_detail': expected, 'state': state})

        report['last_step'] = 'provider-secret-errors-raw-warn' if not http_only else 'provider-secret-errors-create-log'
        creation_log = wait_raw_ledger(page, server, artifacts, offset, 'POST', '/api/v1/providers', 201,
                                      name=name, warning=not http_only)
        row = collected_warning(page, server, artifacts) if not http_only else None
        read('valid')
        if not http_only:
            check(len(fixture.emitted_errors) == 1 and fixture.emitted_errors[0]['status'] == 400
                  and fixture.emitted_errors[0]['authorization'] == 'Bearer ' + secret
                  and fixture.emitted_errors[0]['body']['detail'] == marker + ': upstream echoed Bearer ' + secret,
                  'key-error fixture did not emit the precise authenticated poisoned source')
            check(fixture.keys == [EXISTING_KEY], 'best-effort failure unexpectedly replaced account keys')
        else:
            check(fixture.emitted_errors == [] and fixture.keys == expected_keys,
                  'HTTP-only diagnostic failed ordinary key registration before Sync')
        report['provider_secret_creation_log'] = creation_log
        report['provider_secret_collected_warn'] = row
        # Remain untraced across the intentionally poisoned error response and
        # possible UI leak. Trace resumes only after those sinks prove safe.
        fixture.pods_error = True
        raw_before_sync = (server.work / 'server.log').stat().st_size
        row_locator = page.get_by_role('row').filter(has=page.get_by_text(name, exact=True))
        report['last_step'] = 'provider-secret-errors-sync-response'
        with page.expect_response(lambda response: urlsplit(response.url).path == sync_path
                                  and response.request.method == 'POST') as failed:
            button(row_locator, copy['table']['sync']).click()
        failure = failed.value
        report['provider_secret_sync_response'] = {'status': failure.status, 'body': failure.json(),
                                                   'sha256': hashlib.sha256(failure.body()).hexdigest()}
        assert_raw_secret_free(artifacts, failure.body(), 'raw Sync HTTP424 response')
        check(failure.status == 424 and failure.json() == {'error': 'runpod: list pods: status 403'},
              'Sync failed without its exact safe operation/status error')
        check(failure.request.post_data is None, 'Sync unexpectedly sent credential JSON')
        message = copy['message']['syncFailed'].replace('{{detail}}', 'runpod: list pods: status 403')
        expect(page.locator('.ant-message-error').filter(has_text=message)).to_have_text(message, timeout=12000)
        report['last_step'] = 'provider-secret-errors-sync-log'
        failure_log = wait_raw_ledger(page, server, artifacts, raw_before_sync, 'POST', sync_path, 424)
        read('invalid')
        page.reload()
        expect(row_locator.get_by_text(copy['table']['state']['invalid'], exact=True)).to_be_visible(timeout=12000)
        artifacts.start_trace(context)
        fixture.pods_error = False
        raw_before_recovery = (server.work / 'server.log').stat().st_size
        report['last_step'] = 'provider-secret-errors-sync-recovery'
        with page.expect_response(lambda response: urlsplit(response.url).path == sync_path
                                  and response.request.method == 'POST') as restored:
            button(row_locator, copy['table']['sync']).click()
        recovery = restored.value
        check(recovery.status == 200 and recovery.json() == {'count': 0, 'instances': []}, 'corrected Sync did not recover')
        success = copy['message']['syncDone'].replace('{{count}}', '0')
        expect(page.locator('.ant-message-success').filter(has_text=success)).to_have_text(success, timeout=12000)
        recovery_log = wait_raw_ledger(page, server, artifacts, raw_before_recovery, 'POST', sync_path, 200)
        read('valid')
        page.reload()
        expect(row_locator.get_by_text(copy['table']['state']['valid'], exact=True)).to_be_visible(timeout=12000)
        context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=server.base)
        check(secret not in page.evaluate('() => navigator.clipboard.readText()'), 'secret entered clipboard without a copy action')
        def wire(method, path, status, body, response):
            return {'method': method, 'path': path, 'status': status, 'authorized': True,
                    'body': body, 'response': response}

        poisoned_detail = marker + ': upstream echoed Bearer ' + secret
        key_problem = {'title': 'Bad Request', 'status': 400, 'detail': poisoned_detail}
        pods_problem = {'title': 'Forbidden', 'status': 403, 'detail': poisoned_detail}
        empty_pods = {'pods': [], 'pagination': {'nextCursor': None, 'hasNextPage': False}}
        expected = [wire('GET', '/proxy-a/v2/pods', 200, None, empty_pods),
                    wire('GET', '/proxy-a/v2/account/ssh-keys', 200 if http_only else 400, None,
                         {'keys': [EXISTING_KEY]} if http_only else key_problem)]
        if http_only:
            expected.append(wire('PUT', '/proxy-a/v2/account/ssh-keys', 200,
                                 {'keys': expected_keys}, {'keys': expected_keys}))
        expected.extend([wire('GET', '/proxy-a/v2/pods', 403, None, pods_problem),
                         wire('GET', '/proxy-a/v2/pods', 200, None, empty_pods)])
        check(fixture.requests == expected, 'secret-error external full request/response/auth ledger differs')
        expected_errors = ([('/proxy-a/v2/pods', 403, pods_problem)] if http_only else
                           [('/proxy-a/v2/account/ssh-keys', 400, key_problem),
                            ('/proxy-a/v2/pods', 403, pods_problem)])
        check(len(fixture.emitted_errors) == len(expected_errors), 'poisoned error source count differs')
        for actual, (error_path, error_status, error_body) in zip(fixture.emitted_errors, expected_errors):
            raw_body = json.dumps(error_body).encode('utf-8')
            check(actual == {'method': 'GET', 'path': error_path, 'authorization': 'Bearer ' + secret,
                'status': error_status, 'content_type': 'application/problem+json', 'body': error_body,
                'body_sha256': hashlib.sha256(raw_body).hexdigest(), 'bytes': len(raw_body)},
                'fixture did not emit the exact independently planned secret-bearing bytes')
        check(fixture.keys == (expected_keys if http_only else [EXISTING_KEY]),
              'secret-error flow changed unrelated external account keys')
        report['last_step'] = 'provider-secret-errors-delete'
        button(row_locator, copy['table']['delete']).click()
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                  and response.request.method == 'DELETE') as deleted:
            button(page.get_by_role('tooltip'), copy['table']['delete']).click()
        check(deleted.value.status == 200 and page.request.get(server.base + path).status == 404,
              'secret-error account UI cleanup/detail absence failed')
        expect(row_locator).to_have_count(0, timeout=12000)
        check(collection(page.request.get(server.base + '/api/v1/providers')) == [], 'secret-error account remains listed')
        report['remaining_resources'] = [entry for entry in report.get('remaining_resources', [])
            if not (entry.get('kind') == 'provider' and entry.get('id') == provider_id)]
        assert_raw_secret_free(artifacts, (server.work / 'server.log').read_bytes(), 'final raw product log')
        report['provider_secret_error_evidence'] = {'http_only': http_only, 'creation': creation_log,
            'failed_sync': failure_log, 'recovered_sync': recovery_log,
            'requests': list(fixture.requests), 'emitted_errors': list(fixture.emitted_errors),
            'credential_download': 'N/A: provider account UI/API have no credential download/export; source audit includes CloudProvidersPage, ResourceTable, api.ts and provider route inventory',
            'raw_logs_checked_before_artifact_redaction': True, 'trace_stopped_during_secret_and_hostile_response': True}
        report['evidence'].extend([{'layer': 'runtime', 'secret_errors': report['provider_secret_error_evidence']},
            {'layer': 'cleanup', 'secret_error_provider_id': provider_id, 'independent_get': 404, 'list': []}])
        return [{'path': sync_path, 'status': 424, 'method': 'POST'}]
    finally:
        report['provider_secret_fixture_requests'] = list(fixture.requests)
        report['provider_secret_fixture_emitted_errors'] = list(fixture.emitted_errors)
        close_fixture(fixture, report, 'secret-errors-http' if http_only else 'secret-errors', started=started)
