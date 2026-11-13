#!/usr/bin/env python3
"""Cloud create defaults and custom endpoint with real server/SQLite/Chrome.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-provider-endpoint-defaults
Only the external AutoDL API is replaced; no browser management API mocks.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import threading
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from network_storage_lifecycle import read, mutate, locale_strings
from platform_token_read_revoke import button, check


class AutoDLFixture:
    """AutoDL list contract: raw Authorization key, POST JSON pagination, envelope."""
    def __init__(self, key):
        self.key, self.requests = key, []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))) or b'{}')
                valid = self.headers.get('Authorization') == owner.key
                status = 200
                if self.path != '/proxy/api/v1/dev/instance/pro/list':
                    status = 404
                elif not valid:
                    status = 401
                elif body.get('page_index') != 1 or body.get('page_size') not in (1, 100):
                    status = 400
                owner.requests.append({'method': 'POST', 'path': self.path, 'body': body,
                                       'authenticated': valid, 'status': status})
                data = {'code': 'Success', 'data': {'list': [], 'max_page': 1, 'result_total': 0}} if status == 200 else {'code': 'InvalidRequest', 'msg': 'invalid fixture request'}
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.end_headers()
                self.wfile.write(json.dumps(data).encode())

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.endpoint = f'http://127.0.0.1:{self.server.server_port}/proxy'

    def start(self):
        self.thread.start()

    def stop(self):
        if self.thread.is_alive():
            self.server.shutdown()
            self.thread.join(timeout=5)
        self.server.server_close()
        return not self.thread.is_alive()


# Mainline: gpuworks/provider/create-endpoint-defaults. Open every public kind,
# prove actual values and blank secrets; switch/reopen; override AutoDL endpoint,
# reject bad credentials without persistence; retry, reload, UI sync from stored
# credentials, and delete. No cloud rental, agent, or GPU runtime claim.
def test_provider_endpoint_defaults(page, server, fixture, report):
    evidence = report['evidence']
    prefix = '/api/v1/providers'
    page.goto(server.base + '/providers')
    expect(page.get_by_role('heading', name='External Platforms', exact=True)).to_be_visible()
    kinds = read(page, server.base, prefix + '/kinds')['items']
    baseline = read(page, server.base, prefix)['items']
    mutations = []
    page.on('request', lambda r: mutations.append(r.method) if urlsplit(r.url).path == prefix and r.method == 'POST' else None)
    defaults = {}
    kind_labels = locale_strings('en', 'providers')['kind']
    for info in kinds:
        if info['kind'] == 'zstack':
            continue  # A private cloud has no vendor-wide endpoint.
        button(page, 'Add ' + kind_labels[info['kind']]).click()
        dialog = page.get_by_role('dialog')
        expect(dialog).to_be_visible()
        for field in info['credential_fields']:
            control = dialog.get_by_label(field['label']['en'], exact=True)
            want = '' if field['secret'] else field.get('default_value', '')
            expect(control).to_have_value(want)
            if field['name'] == 'endpoint':
                defaults[info['kind']] = want
        button(dialog, 'Cancel').click()
    check(set(defaults) == {'autodl', 'compshare', 'runpod', 'vast'}, 'endpoint-bearing public provider inventory changed')
    check(all(defaults.values()), 'known vendor endpoint defaults must be real input values')
    check(not mutations, 'opening/canceling default forms sent a create request')
    button(page, 'Add AutoDL').click()
    dialog = page.get_by_role('dialog')
    endpoint_label = next(f['label']['en'] for k in kinds if k['kind'] == 'autodl' for f in k['credential_fields'] if f['name'] == 'endpoint')
    key_label = next(f['label']['en'] for k in kinds if k['kind'] == 'autodl' for f in k['credential_fields'] if f['name'] == 'api_key')
    endpoint = dialog.get_by_label(endpoint_label, exact=True)
    endpoint.fill(fixture.endpoint)
    dialog.get_by_label('External platform', exact=True).press('ArrowDown')
    page.get_by_title('RunPod', exact=True).click()
    expect(endpoint).to_have_value(defaults['runpod'])
    button(dialog, 'Cancel').click()
    button(page, 'Add AutoDL').click()
    expect(endpoint).to_have_value(defaults['autodl'])
    name = 'e2e-' + report['run_id'] + '-autodl'
    dialog.get_by_label('Name', exact=True).fill(name)
    endpoint.fill(fixture.endpoint)
    dialog.get_by_label(key_label, exact=True).fill('invalid-fixture-key')
    expected = {'kind': 'autodl', 'name': name, 'endpoint': fixture.endpoint, 'api_key': 'invalid-fixture-key', 'extra': {}}
    mutate(page, button(dialog, 'Add').click, 'POST', prefix, expected, 400, evidence)
    expect(dialog).to_be_visible()
    expect(endpoint).to_have_value(fixture.endpoint)
    check(read(page, server.base, prefix)['items'] == baseline, 'rejected credentials created a provider')
    dialog.get_by_label(key_label, exact=True).fill(fixture.key)
    expected['api_key'] = fixture.key
    created = mutate(page, button(dialog, 'Add').click, 'POST', prefix, expected, 201, evidence)
    expect(dialog).not_to_be_visible()
    path = prefix + '/' + str(created['id'])
    page.reload()
    row = page.get_by_role('row').filter(has=page.get_by_text(name, exact=True))
    expect(row).to_be_visible()
    saved = next(item for item in read(page, server.base, prefix)['items'] if item['id'] == created['id'])
    check(saved['name'] == name and saved['kind'] == 'autodl', 'independent provider read differs')
    before = len(fixture.requests)
    with page.expect_response(lambda r: urlsplit(r.url).path == path + '/sync' and r.request.method == 'POST') as synced:
        button(row, 'Sync').click()
    check(synced.value.status == 200, 'saved provider sync failed')
    check(any(r['authenticated'] and r['body'] == {'page_index': 1, 'page_size': 100} and r['status'] == 200 for r in fixture.requests[before:]),
          'sync did not use persisted custom endpoint and key')
    button(row, 'Delete').click()
    popup = page.locator('.ant-popconfirm')
    mutate(page, button(popup, 'Delete').click, 'DELETE', path, None, 200, evidence)
    expect(row).to_have_count(0)
    check(read(page, server.base, prefix)['items'] == baseline, 'delete did not restore baseline')
    evidence.extend([{'layer': 'ui', 'defaults': defaults, 'checks': ['all public schemas', 'blank secrets', 'kind switch', 'cancel/reopen', 'failed save preserves draft']},
                     {'layer': 'persistence', 'provider': saved, 'proof': 'refresh + independent GET + real driver sync from encrypted stored endpoint/key'},
                     {'layer': 'execution', 'external_requests': fixture.requests},
                     {'layer': 'cleanup', 'baseline_restored': True}])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--artifact-dir', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ').lower() + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    fixture = AutoDLFixture('e2e-key-' + uuid.uuid4().hex)
    artifacts.remember(fixture.key)
    artifacts.remember('invalid-fixture-key')
    server = LocalServer(args.server_bin, artifacts.private)
    report = {'run_id': run_id, 'status': 'NOT_RUN', 'contract_id': 'gpuworks/provider/create-endpoint-defaults',
              'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'lane': 'local-real-server-sqlite-autodl-fixture', 'locale': 'en', 'timezone': 'UTC',
              'test_sha256': digest(Path(__file__)), 'binary_sha256': digest(args.server_bin),
              'manifest_sha256': digest(ROOT / 'test/e2e/coverage/provider-endpoint-defaults.json'), 'evidence': []}
    try:
        manifest = json.loads((ROOT / 'test/e2e/coverage/provider-endpoint-defaults.json').read_text())
        check(manifest['contract_id'] == report['contract_id'] and manifest['test_function'] == test_provider_endpoint_defaults.__name__, 'coverage binding mismatch')
        fixture.start()
        report['health'] = server.start()
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            report['browser_version'] = browser.version
            context = browser.new_context(viewport={'width': 1440, 'height': 900}, locale='en-US', timezone_id='UTC')
            page = context.new_page()
            page.set_default_timeout(10000)
            auth = locale_strings('en', 'auth')
            page.goto(server.base + '/dashboard')
            page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
            page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
            with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login') as login:
                button(page, auth['submit']).click()
            check(login.value.status == 200, 'login failed')
            for cookie in context.cookies():
                artifacts.remember(cookie['value'])
            report['preflight'] = 'PASS'
            errors = []
            page.on('pageerror', lambda e: errors.append(str(e)))
            artifacts.start_trace(context)
            try:
                test_provider_endpoint_defaults(page, server, fixture, report)
                check(not errors, f'browser errors: {errors}')
                check(set(manifest['required_evidence']) <= {item['layer'] for item in report['evidence']}, 'required evidence missing')
                report['status'] = 'PASS'
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = traceback.format_exc()
    finally:
        report['server_stopped'] = server.stop()
        report['fixture_stopped'] = fixture.stop()
        if not report['server_stopped'] or not report['fixture_stopped']:
            report['status'] = 'FAIL_TEST'
        report['external_requests'] = fixture.requests
        artifacts.write_json('report.json', report)
        artifacts.scan()
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
