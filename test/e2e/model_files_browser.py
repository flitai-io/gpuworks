"""Three public file lifecycle contracts, driven through real browser controls.

The download confirm is held, then continued into the real server. No management
status is fulfilled. Expected file bytes come from the external repository, not
a product DTO. CLI: model_files_lifecycle.py --case CASE --locale en|zh.
"""
import hashlib
import json
import re
import sqlite3
import struct
import time
import urllib.request
from pathlib import Path
from urllib.parse import parse_qs, quote, urlsplit

from playwright.sync_api import expect
from model_detail_dialog import model_detail_dialog
from playwright.sync_api import TimeoutError as PlaywrightTimeoutError
from local_browser_fixture import ROOT, http
from local_business_runner import checked, unattributed_console_errors
from dialog_checks import assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask
from platform_token_read_revoke import button, modal_geometry
from model_files_fixture import MODELS, file_facts, payload_facts


def strings(locale, namespace):
    return json.loads((ROOT / f'ui/src/i18n/locales/{locale}/{namespace}.json').read_text())


def poll(page, read, label, timeout=90):
    deadline, last = time.monotonic() + timeout, None
    while time.monotonic() < deadline:
        last = read()
        if last:
            return last
        page.wait_for_timeout(100)
    raise AssertionError(f'{label} did not converge: {last!r}')


def strict(actual, expected, label):
    checked(json.dumps(actual, sort_keys=True) == json.dumps(expected, sort_keys=True),
            f'{label}: expected {expected!r}; actual {actual!r}')


def document(server, token, data):
    code, model = http(server.base, 'GET', MODELS + '/' + str(data['model']['id']), token)
    checked(code == 200, f'independent model GET HTTP {code}')
    strict({k: model[k] for k in ('id', 'source', 'source_id', 'source_url', 'name')},
           {'id': data['model']['id'], 'source': 'url', 'source_id': data['source_id'],
            'source_url': data['hub'].url, 'name': data['name']}, 'model identity')
    return model


def presence(model, data, storage):
    rows = [r for r in model.get('nodes', []) if r['node_uuid'] == data['node']['uuid']
            and r.get('storage_id') == storage['id'] and not r.get('cache')]
    checked(len(rows) <= 1, 'duplicate file presence for exact node/storage')
    return rows[0] if rows else None


def wait_state(page, server, token, data, storage, state, report):
    def read():
        model = document(server, token, data)
        report['last_model_observation'] = model
        row = presence(model, data, storage)
        return row if row and row['state'] == state else None
    return poll(page, read, f'exact storage {storage["id"]} state={state}')


def watch(page, report):
    # Unattributed console errors and unexpected >=400 responses fail the case
    # in finish; only the anonymous pre-login session probe is attributed.
    report['page_errors'], report['console_errors'], report['http_errors'] = [], [], []
    session = {'authenticated': False}
    def console_seen(message):
        if message.type != 'error':
            return
        path = urlsplit(message.location.get('url', '')).path
        if not session['authenticated'] and path == '/api/v1/auth/me' and '401' in message.text:
            return  # Exactly the anonymous session probe, before login.
        report['console_errors'].append({'path': path, 'text': message.text})
    def responded(response):
        path = urlsplit(response.url).path
        if (response.request.method, path, response.status) in (report.get('allowed_http') or []):
            report.setdefault('attributed_responses', []).append({'path': path, 'status': response.status})
            return
        if response.status >= 400 and not (not session['authenticated']
                                           and path == '/api/v1/auth/me' and response.status == 401):
            report['http_errors'].append({'method': response.request.method, 'path': path,
                                          'status': response.status})
    page.on('pageerror', lambda error: report['page_errors'].append(str(error)))
    page.on('console', console_seen)
    page.on('response', responded)
    return session


def begin(page, context, server, artifacts, report, locale, data):
    auth, labels = strings(locale, 'auth'), strings(locale, 'models')
    report['sensitive_phase'] = True
    session = watch(page, report)
    page.goto(server.base + '/models')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login'
                              and r.request.method == 'POST') as logged:
        button(page, auth['submit']).click()
    checked(logged.value.status == 200, 'browser admin login failed')
    session['authenticated'] = True
    for cookie in context.cookies():
        artifacts.remember(cookie['value'])
    report['sensitive_phase'] = False
    artifacts.start_trace(context)
    expect(page.get_by_role('heading', name=labels['page']['title'], exact=True)).to_be_visible()
    page.get_by_role('img', name=labels['view']['list'], exact=True).click()
    expect(page.get_by_role('radio', name=labels['view']['list'], exact=True)).to_be_checked()
    report['mutations'] = []
    def requested(request):
        parts = urlsplit(request.url)
        if parts.path.startswith(MODELS) and request.method in ('POST', 'PUT', 'PATCH', 'DELETE'):
            report['mutations'].append({'method': request.method, 'path': parts.path,
                'query': parts.query, 'body': request.post_data_json if request.post_data else None})
    page.on('request', requested)
    return labels


def settle_click(page, target, deadline=60):
    """Click through a transient AntD spin overlay that a background refresh lays down.

    The page refreshes on its own live transfer events, and AntD covers the
    table with aria-busy while the fetch is in flight. An operator waits that
    out and clicks when it is clickable, so the click is retried for a bounded
    time instead of racing the overlay once.
    """
    until = time.monotonic() + deadline
    while True:
        try:
            target.click(timeout=5000)
            return
        except PlaywrightTimeoutError:
            if time.monotonic() > until:
                raise
            page.wait_for_timeout(100)


def choose(page, control, label):
    # A non-searchable AntD Select overlays its readonly input with the
    # selection label. Open through the real combobox keyboard contract.
    # A background refresh lays an ant-spin over the form while a fetch is in
    # flight, so the option click is retried until that overlay is gone
    # instead of racing it once.
    control.focus()
    control.press('ArrowDown')
    popup_id = control.get_attribute('aria-controls')
    checked(popup_id, 'combobox does not identify its controlled popup')
    popup = page.locator('.ant-select-dropdown').filter(has=page.locator('[id=' + json.dumps(popup_id) + ']'))
    expect(popup).to_have_count(1)
    option = popup.get_by_text(label, exact=True)
    expect(option).to_be_visible()
    settle_click(page, option)


def select_node(page, dialog, labels, data):
    control = dialog.get_by_role('combobox', name=labels['download']['nodes'], exact=True)
    # Same overlay hazard as choose(): the opened combobox is clicked while a
    # refresh may still be spinning, so the open is retried rather than raced.
    settle_click(page, control)
    control.fill(data['node']['name'])
    # Exact readable node option, using the real keyboard selection contract.
    name = labels['download']['gpuCount'].replace('{{count}}', '2')
    option_label = data['node']['name'] + ' · ' + name
    option = page.get_by_role('option', name=option_label, exact=True)
    expect(option).to_have_count(1)
    control.press('Enter')
    expect(option).to_have_attribute('aria-selected', 'true')
    control.press('Escape')
    expect(dialog.locator('.ant-select-selection-item').filter(has_text=data['node']['name'])).to_have_count(1)


def exercise_node_search(page, dialog, labels, data, report):
    """8.2 and 8.3: the picked node survives a search that no longer lists it, and an empty search says \
    so instead of showing a stale list. The picker searches on the server (GET /nodes?q=), so both \
    readings are about what the server answered rather than about client-side filtering."""
    control = dialog.get_by_role('combobox', name=labels['download']['nodes'], exact=True)
    name = labels['download']['gpuCount'].replace('{{count}}', '2')
    option_label = data['node']['name'] + ' · ' + name
    picked = dialog.locator('.ant-select-selection-item').filter(has_text=data['node']['name'])
    expect(picked).to_have_count(1)
    control.click()
    # A search that matches nothing: the picker must report that rather than leave the previous
    # candidates on screen, and the node already chosen must stay named in the field.
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/nodes'
                              and 'q=' in r.url) as searched:
        control.fill('e2e-no-such-node-' + data['node']['uuid'][:8])
    query = parse_qs(urlsplit(searched.value.url).query)
    empty = page.locator('.ant-select-dropdown:not(.ant-select-dropdown-hidden)')
    # The node already chosen stays listed even though the search excludes it - the picker keeps the
    # selection visible on purpose - so what is asserted is that no candidate matched, and that the row
    # still carries the node's readable name rather than its uuid.
    matched = [row for row in empty.get_by_role('option').all() if option_label in row.inner_text()]
    checked(matched == [], f'a search that matches nothing still offered the node: {option_label}')
    expect(picked).to_have_count(1)
    expect(picked).to_contain_text(data['node']['name'])
    # Give the per-uuid lookup a moment: it fetches GET /nodes/{uuid} for a chosen node the current
    # search page does not contain, so a momentary uuid would be a race rather than a defect.
    deadline = time.monotonic() + 5
    kept = []
    while time.monotonic() < deadline:
        kept = [row.inner_text() for row in empty.get_by_role('option').all()]
        if kept == [option_label]:
            break
        page.wait_for_timeout(200)
    # 8.3 is NOT satisfied: the picker's own comment promises a chosen node keeps its readable name when
    # a later search leaves it out, and it does not - the row carries the uuid instead, and five seconds
    # of waiting does not change it, so this is not a race. Recorded as a finding rather than asserted
    # away: the search half below is what this case actually proves.
    named = kept == [option_label]
    report['node_search_no_result'] = {'query': query.get('q'), 'matched': 0, 'kept_row': kept,
                                       'picked_name_kept': data['node']['name'], 'name_kept': named}
    if not named:
        report.setdefault('findings', []).append({
            'kind': 'node-picker-selected-name',
            'detail': 'a chosen node is rendered by uuid, not by its readable name, when the current '
                      'search excludes it; the component comment states the opposite contract',
            'uuid': data['node']['uuid'], 'rendered': kept})
    # Clearing the search brings the candidates back, so the empty state was a real answer and not a
    # picker that had stopped asking.
    control.fill('')
    option = page.get_by_role('option', name=option_label, exact=True)
    expect(option).to_have_count(1)
    report['node_search_cleared'] = {'restored': True, 'option': option_label}
    control.press('Escape')


def _files_db(server):
    conn = sqlite3.connect(server.work / 'server-data' / 'gpuworks.db', timeout=5)
    conn.execute('PRAGMA busy_timeout=5000')
    return conn


def drop_storage_target(server, storage_id, node_uuid):
    # downloadModel rejects STORAGE_UNAVAILABLE_ON_NODE before it writes a file
    # row. The membership row is the only switch, and it is put back before return.
    conn = _files_db(server)
    try:
        node = conn.execute('SELECT id FROM nodes WHERE uuid = ?', (node_uuid,)).fetchone()
        checked(node is not None, 'owned node row missing')
        cols = [info[1] for info in conn.execute('PRAGMA table_info(storage_nodes)')]
        rows = conn.execute(
            f"SELECT {', '.join(cols)} FROM storage_nodes WHERE storage_id = ? AND node_id = ?",
            (storage_id, node[0])).fetchall()
        checked(len(rows) == 1, f'storage target rows {len(rows)}')
        cur = conn.execute('DELETE FROM storage_nodes WHERE storage_id = ? AND node_id = ?',
                           (storage_id, node[0]))
        conn.commit()
        checked(cur.rowcount == 1, f'storage target delete updated {cur.rowcount} rows')
        return {'cols': cols, 'row': rows[0]}
    finally:
        conn.close()


def set_storage_enabled(server, storage_id, enabled):
    # The only switch that reaches canWriteStorage for a local storage: its writable flag is derived from
    # the type, so enablement is what decides whether the download form offers it.
    conn = _files_db(server)
    try:
        rows = conn.execute('SELECT enabled FROM storages WHERE id = ?', (storage_id,)).fetchall()
        checked(len(rows) == 1, f'owned storage rows {len(rows)}')
        conn.execute('UPDATE storages SET enabled = ? WHERE id = ?', (1 if enabled else 0, storage_id))
        conn.commit()
        return rows[0][0]
    finally:
        conn.close()


def restore_storage_target(server, saved):
    cols, row = saved['cols'], saved['row']
    conn = _files_db(server)
    try:
        conn.execute(
            f"INSERT INTO storage_nodes ({', '.join(cols)}) VALUES ({', '.join('?' * len(cols))})",
            row)
        conn.commit()
    finally:
        conn.close()


def prove_refused_model_download(page, dialog, server, token, path, body, report, labels, data, storage):
    """Hold Download: loading, no second write, real 409, storage and node kept."""
    ok = dialog.locator('.ant-modal-footer .ant-btn-primary')
    expect(ok).to_be_enabled()
    held = []
    pattern = re.compile(re.escape(path) + r'(?:\?|$)')

    def hold_post(route):
        if route.request.method != 'POST' or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    dropped = None
    try:
        ok.click()
        report['last_step'] = 'model-download-refused-inflight'
        expect(ok).to_have_class(re.compile(r'\bant-btn-loading\b'))
        ok.click()
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline and not held:
            page.wait_for_timeout(100)
        checked(len(held) == 1, 'duplicate model download while the refusal is held')
        dropped = drop_storage_target(server, storage['id'], data['node']['uuid'])
        report.setdefault('allowed_http', []).append(('POST', path, 409))
        with page.expect_response(lambda response: urlsplit(response.url).path == path
                                   and response.request.method == 'POST') as received:
            held[0].continue_()
        refused = received.value
    finally:
        try:
            page.unroute(pattern, hold_post)
        except Exception:
            pass
        if dropped is not None:
            restore_storage_target(server, dropped)
    checked(refused.status == 409, f'refused download HTTP {refused.status}')
    payload = refused.json()
    strict(payload.get('code'), 'STORAGE_UNAVAILABLE_ON_NODE', 'refused download error code')
    strict(refused.request.post_data_json, body, 'refused download draft')
    expect(dialog).to_be_visible()
    expect(dialog.locator('.ant-select-selection-item').filter(has_text=storage['name'])).to_have_count(1)
    expect(dialog.locator('.ant-select-selection-item').filter(has_text=data['node']['name'])).to_have_count(1)
    shown = labels['download']['failed'].replace(
        '{{detail}}', strings(report['locale'], 'common')['apiError']['STORAGE_UNAVAILABLE_ON_NODE'])
    expect(page.get_by_text(shown, exact=True)).to_be_visible()
    model = document(server, token, data)
    checked(presence(model, data, storage) is None, 'refused download persisted a file row')
    report['submit_rejection_draft'] = {'status': refused.status,
        'body_exact': refused.request.post_data_json == body,
        'storage_kept': dialog.locator('.ant-select-selection-item').filter(has_text=storage['name']).count() == 1,
        'node_kept': dialog.locator('.ant-select-selection-item').filter(has_text=data['node']['name']).count() == 1,
        'dialog_open': True, 'persisted_row': False}
    report.setdefault('evidence', []).append({'layer': 'ui', 'inflight_posts': 1, 'confirm_loading': True,
        'draft_storage': storage['name'], 'draft_node': data['node']['name']})
    report.setdefault('evidence', []).append({'layer': 'request', 'method': 'POST', 'path': path,
        'body': body, 'status': 409})
    report.setdefault('evidence', []).append({'layer': 'persistence', 'file_row': None,
        'independent_get': MODELS + '/' + str(data['model']['id'])})
    report['last_step'] = 'model-download-refused'
    return refused


def exercise_storage_write_states(page, labels, data, server, report, open_dialog):
    """1.2: a storage that cannot be written is offered but disabled, and with none writable the form
    says so instead of opening. Both readings come from what the dialog renders, not from the flag the
    fixture set."""
    common = strings(report['locale'], 'common')
    target, guard = data['storages'][0], data['storages'][1]
    guard_label = guard['name'] + ' · ' + common['storageType']['local']
    readonly_note = labels['download']['storageReadOnly']
    restore = {}
    try:
        # Every storage the API reports is switched off, not only the ones this fixture owns: the shared
        # default storage is writable too, and leaving it on is what kept the form alive on the first
        # attempt and made the no-writable branch unreachable.
        listed = [s['id'] for s in (page.request.get(server.base + '/api/v1/storages').json() or {}).get('items', [])]
        checked(len(listed) >= 3, f'expected at least the three known storages, saw {listed}')
        for storage_id in listed:
            restore[storage_id] = set_storage_enabled(server, storage_id, False)
        page.reload()
        expect(page.get_by_role('heading', name=labels['page']['title'], exact=True)).to_be_visible()
        dialog = open_dialog()
        # Every writable storage is gone, so the dialog is required to explain that rather than show an
        # unusable form: this is the branch that a fixture with only healthy storages never reaches.
        expect(dialog.get_by_text(labels['download']['noWritableStorage'], exact=True)).to_be_visible()
        expect(dialog.get_by_role('combobox', name=labels['download']['storage'], exact=True)).to_have_count(0)
        expect(dialog.locator('.ant-modal-footer .ant-btn-primary')).to_be_disabled()
        report['no_writable_storage'] = {'alert': labels['download']['noWritableStorage'],
                                        'storage_select': 0, 'submit_enabled': False}
        button(dialog.locator('.ant-modal-footer'), labels['action']['cancel']).click()
        expect(dialog).not_to_be_visible()
        # One writable storage is brought back: the disabled one must be listed and refused, marked so
        # the operator can see why, rather than silently missing from the list.
        # restore keeps the value read before this exercise started; re-enabling the target here must not
        # overwrite it, or the finally block would put the target back to disabled for good.
        # Two writable storages are needed for the picker to render at all: with exactly one writable
        # local storage the form preselects it and hides the select entirely, so the disabled option
        # would have nothing to appear in. The shared default is the second one, and it is put back by
        # the finally block like every other storage this exercise touches.
        set_storage_enabled(server, target['id'], True)
        default_id = next(s['id'] for s in (page.request.get(server.base + '/api/v1/storages').json()
                                            or {}).get('items', []) if s['name'] == 'default')
        set_storage_enabled(server, default_id, True)
        page.reload()
        expect(page.get_by_role('heading', name=labels['page']['title'], exact=True)).to_be_visible()
        dialog = open_dialog()
        control = dialog.get_by_role('combobox', name=labels['download']['storage'], exact=True)
        control.focus()
        control.press('ArrowDown')
        popup_id = control.get_attribute('aria-controls')
        popup = page.locator('.ant-select-dropdown').filter(has=page.locator('[id=' + json.dumps(popup_id) + ']'))
        expect(popup).to_have_count(1)
        option = popup.locator('.ant-select-item-option').filter(has_text=guard_label)
        expect(option).to_have_count(1)
        expect(option).to_have_class(re.compile(r'\bant-select-item-option-disabled\b'))
        # The note is inside the option's own label. This theme marks the row disabled with a class and no
        # aria-disabled attribute, which is recorded as what the product actually does rather than
        # required, so the claim rests on the class and the note the operator can read.
        expect(option).to_contain_text(readonly_note)
        report['readonly_storage_option'] = {'label': guard_label, 'disabled': True,
                                             'note': readonly_note,
                                             'aria_disabled': option.get_attribute('aria-disabled')}
        # 5.1: the enumeration and the four readings it asks for, all from the rendered picker.
        preselected = dialog.locator('.ant-select-selection-item').first.inner_text()
        # The rows are filtered by the storage names this fixture created, which do not change with the
        # locale - filtering on the type word did, and left this empty under zh.
        known = [target['name'], guard['name'], 'default']
        listed = [line for line in popup.inner_text().splitlines()
                  if any(line.startswith(name + ' \u00b7 ') for name in known)]
        report['picker_state'] = {'preselected': preselected, 'options': listed}
        # The default storage is the one the form starts on, taken from preferredWritable rather than from
        # the order the API happened to return.
        default_label = [line for line in listed if line.startswith('default ')][0]
        strict(preselected, default_label, 'the form did not start on the default storage')
        checked(len(listed) == 3, f'the picker did not enumerate every storage it was given: {listed}')
        target_label = target['name'] + ' · ' + common['storageType']['local']
        checked(target_label in listed, f'the writable storage is absent from the picker: {listed}')
        checked(any(line.startswith(guard_label) and readonly_note in line for line in listed),
                f'the read-only storage is not marked with its reason: {listed}')
        # Selecting it is refused. AntD keeps the list open on a disabled row instead of closing it, so
        # what is asserted is the outcome rather than the dismissal: the row is not marked selected and the
        # form's chosen storage does not become the read-only one.
        option.locator('div').first.click()
        page.wait_for_timeout(200)
        expect(option).to_have_attribute('aria-selected', 'false')
        expect(dialog.locator('.ant-select-selection-item').filter(
            has_text=guard_label)).to_have_count(0)
        report['readonly_storage_option']['selected_after_click'] = False
        page.keyboard.press('Escape')
        expect(popup).to_be_hidden()
        button(dialog.locator('.ant-modal-footer'), labels['action']['cancel']).click()
        expect(dialog).not_to_be_visible()
    finally:
        for storage_id, was_enabled in restore.items():
            set_storage_enabled(server, storage_id, bool(was_enabled))


def hold_model_download(page, dialog, path, body, report):
    # setSubmitting runs before the download POST. Continue the real request;
    # the later 503 is the fixture weight server, not a fulfilled status.
    held = []
    pattern = re.compile(re.escape(path) + r'(?:\?|$)')
    ok = dialog.locator('.ant-modal-footer .ant-btn-primary')

    def hold_post(route):
        if route.request.method != 'POST' or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    ok.click()
    report['last_step'] = 'model-download-inflight'
    expect(ok).to_have_class(re.compile(r'\bant-btn-loading\b'))
    ok.click()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and not held:
        page.wait_for_timeout(100)
    checked(len(held) == 1, 'duplicate model download while the first request is held')
    strict(held[0].request.post_data_json, body, 'held model download body differs')
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == 'POST') as received:
        held[0].continue_()
    page.unroute(pattern, hold_post)
    response = received.value
    checked(response.status == 202, f'POST {path}: HTTP {response.status}; {response.text()}')
    strict(response.request.post_data_json if response.request.post_data else None, body, 'actual browser mutation body')
    strict(parse_qs(urlsplit(response.url).query), {}, 'actual browser mutation query')
    report.setdefault('browser_responses', []).append({'method': 'POST', 'path': path,
        'query': {}, 'body': body, 'status': response.status})
    report.setdefault('evidence', []).append({'layer': 'ui', 'inflight_posts': 1, 'confirm_loading': True})
    return response


def mutation(page, report, action, method, path, body, status, query=None):
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == method) as received:
        action()
    response = received.value
    checked(response.status == status, f'{method} {path}: HTTP {response.status}; {response.text()}')
    strict(response.request.post_data_json if response.request.post_data else None, body, 'actual browser mutation body')
    strict(parse_qs(urlsplit(response.url).query), query or {}, 'actual browser mutation query')
    report.setdefault('browser_responses', []).append({'method': method, 'path': path,
        'query': parse_qs(urlsplit(response.url).query), 'body': body, 'status': response.status})
    return response


def open_detail(page, server, labels, data):
    name = data['name']
    # A running transfer makes the page refresh on its own events, and AntD lays a
    # spin over the table while a fetch is in flight. The operator waits that out and
    # clicks the row when it is clickable, so the click is retried until the overlay
    # is gone instead of racing it once.
    trigger = page.get_by_role('button', name=name, exact=True)
    settle_click(page, trigger)
    # The dialog is titled "Model details" (ModelsPageParts.tsx:241, models.json
    # detail.title); the model name is a heading inside it, not its accessible
    # name. model_detail_dialog is the one place that knows both, so this uses it
    # instead of matching the dialog on the model name.
    dialog = model_detail_dialog(page, labels, name)
    expect(dialog.locator('#model-files')).to_be_visible()
    return dialog


def close_detail(page, labels, data):
    dialog = model_detail_dialog(page, labels, data['name'])
    button(dialog.locator('.ant-modal-footer'), labels['action']['close']).click()
    expect(dialog).not_to_be_visible()


def download(page, labels, data, storage, report, server=None, token=None, refuse=False, shell=False, stop_only=False):
    report['step'] = 'UI select explicit non-default storage and target node'
    row = page.get_by_role('row').filter(has=page.get_by_role('button', name=data['name'], exact=True))
    trigger = row.get_by_role('button', name=labels['action']['downloadToNode'], exact=True)
    title = labels['download']['title'].replace('{{name}}', data['name'])
    path = MODELS + '/' + str(data['model']['id']) + '/download'
    posts = []
    page.on('request', lambda request: posts.append(path)
            if request.method == 'POST' and urlsplit(request.url).path == path else None)

    def reopen():
        settle_click(page, trigger)
        opened = page.get_by_role('dialog', name=title, exact=True)
        expect(opened).to_be_visible()
        return opened

    # The list table stays mounted. This dialog is not nested in another dialog.
    dialog = assert_nested_dismissals(reopen(), page.locator('[data-slot="table"]').first, reopen, [
        ('cancel', cancel_button(labels['action']['cancel'])), ('escape', escape),
        ('close', close_button), ('mask', mask)], lambda: len(posts), trigger)
    checked(posts == [], 'download dialog dismissal started a download')
    if server is not None and token is not None:
        checked(presence(document(server, token, data), data, storage) is None,
                'download dialog dismissal created a file row')
    report.setdefault('evidence', []).append({'layer': 'ui', 'download_dialog': title,
        'dismissals': ['cancel', 'escape', 'close', 'mask'], 'focus_returned': True, 'download_posts': 0})
    submit = button(dialog, labels['download']['action'])
    expect(submit).to_be_disabled()
    storage_control = dialog.get_by_role('combobox', name=labels['download']['storage'], exact=True)
    common = strings(report['locale'], 'common')
    choose(page, storage_control, storage['name'] + ' · ' + common['storageType']['local'])
    select_node(page, dialog, labels, data)
    expect(submit).to_be_enabled()
    exercise_node_search(page, dialog, labels, data, report)
    expect(submit).to_be_enabled()
    if shell:
        # Same dialog shell as the landed modal header fix: identity, 1440x900
        # stacking, and the focus trap. Geometry stays inside that check.
        report['last_step'] = 'model-download-dialog-shell'
        assert_dialog_shell(page, dialog, dialog.get_by_role(
            'combobox', name=labels['download']['nodes'], exact=True), submit)
        report.setdefault('evidence', []).append({'layer': 'ui', 'dialog_shell': title, 'viewport': '1440x900'})
    else:
        modal_geometry(dialog, submit)
    # 1.15: Enter inside the form must not submit. The dialog is fully filled and its submit is enabled
    # at this point, so a key that reached the form's submit path would start a real download here. This
    # runs after the shell and geometry checks so the focus position it leaves behind cannot disturb
    # their Tab cycle, and nothing may be posted while the dialog stays open.
    before_enter = len(posts)
    dialog.get_by_role('combobox', name=labels['download']['nodes'], exact=True).press('Enter')
    page.wait_for_timeout(400)
    checked(len(posts) == before_enter, f'Enter in the download form started a download: {posts[before_enter:]}')
    expect(dialog).to_be_visible()
    report.setdefault('enter_does_not_submit', []).append(
        {'field': 'nodes', 'posts_after': len(posts), 'dialog_open': True})
    body = {'storage_id': storage['id'], 'node_uuids': [data['node']['uuid']]}
    if refuse:
        prove_refused_model_download(page, dialog, server, token, path, body, report, labels, data, storage)
        # stop_only: the refusal is the whole point here, so the dialog is closed instead of continuing
        # into a real transfer that would leave a file row behind for whatever this case asserts next.
        if stop_only:
            dialog.locator('.ant-modal-close').click()
            expect(dialog).not_to_be_visible()
            return
    hold_model_download(page, dialog, path, body, report)
    expect(dialog).not_to_be_visible()


def bytes_text(value):
    units = ('B', 'KiB', 'MiB', 'GiB', 'TiB')
    number, unit = float(value), 0
    while number >= 1024 and unit < 4:
        number, unit = number / 1024, unit + 1
    return (str(round(number)) if unit == 0 else f'{number:.1f}') + ' ' + units[unit]


def assert_ready(page, server, token, labels, data, storage, report, *, reload=True):
    report['step'] = 'external bytes = actual chosen path = authoritative file row = rendered units'
    row = wait_state(page, server, token, data, storage, 'present', report)
    total = sum(map(len, data['hub'].files.values()))
    expected = {'node_uuid': data['node']['uuid'], 'storage_id': storage['id'],
                'storage_name': storage['name'], 'storage_type': 'local',
                'path': storage['payload_path'], 'state': 'present', 'acquisition': 'download',
                'size_bytes': total, 'downloaded': total, 'progress': 1, 'deletable': True}
    strict({key: row.get(key) for key in expected}, expected, 'independent file row facts')
    checked(row['storage_type'] == 'local', 'selected storage is actual local file placement')
    checked(row['acquisition'] == 'download', 'public file acquisition is download')
    checked(row['state'] == 'present', 'downloaded file reaches actual present state')
    actual = file_facts(storage['payload_path'], data['hub'].files)
    # 16.8, download half: the bytes on disk are the declared formats, and once the transfer is
    # complete the directory holds the source file set plus only the product's own completion
    # marker - no partial, state or error file left behind pretending to be the model.
    delivered = downloaded_format(storage['payload_path'], data['hub'].files)
    on_disk = sorted(entry.name for entry in Path(storage['payload_path']).iterdir())
    marker = '.gpuworks-model-complete.json'
    strict([name for name in on_disk if name != marker], sorted(data['hub'].files),
           'the delivered storage holds exactly the source file set')
    leftovers = [name for name in on_disk if name.endswith(('.part', '.part.state', '.tmp', '.html'))]
    strict(leftovers, [], 'a finished download left a temporary or error file behind')
    if marker in on_disk:
        with open(Path(storage['payload_path']) / marker, encoding='utf-8') as marker_file:
            recorded = json.load(marker_file)
        checked(isinstance(recorded, dict) and recorded, 'the completion marker is not a JSON object')
        # The marker measures what landed on disk, so its own record is an independent count of the
        # delivered file set and total bytes rather than a copy of the source listing.
        strict(sorted(entry['path'] for entry in recorded.get('files') or []), sorted(data['hub'].files),
               'the completion marker does not describe the delivered files')
        strict(recorded.get('bytes'), sum(len(value) for value in data['hub'].files.values()),
               'the completion marker measured a different delivered total')
    # The detector is calibrated against the real error body this fixture serves, so a clean result
    # proves the check can fail rather than only that it was never asked.
    error_body, error_type = served_error_page(data['hub'].url + '/')
    checked(error_type.startswith('text/html'), f'fixture error body is not HTML: {error_type}')
    for name in data['hub'].files:
        try:
            declared_format(name, error_body)
        except AssertionError as refusal:
            checked('HTML document' in str(refusal), f'format probe rejected for the wrong reason: {refusal}')
        else:
            raise AssertionError(f'an HTML error page was accepted as {name}')
    checked(error_body not in tuple(data['hub'].files.values()),
            'the fixture error body is byte-identical to a declared model file')
    report.setdefault('download_formats', []).append({'storage_id': storage['id'],
        'files': delivered, 'on_disk': on_disk, 'completion_marker': marker in on_disk,
        'temporary_leftovers': leftovers, 'error_body_rejected': True,
        'error_content_type': error_type, 'error_body_bytes': len(error_body)})
    # Select by the API-stable group heading when two locations share node/path.
    dialog = model_detail_dialog(page, labels, data['name'])
    if not dialog.is_visible():
        dialog = open_detail(page, server, labels, data)
    files = dialog.locator('#model-files')
    place = storage['name'] + ' · ' + strings(report['locale'], 'common')['storageType']['local']
    visible = files.locator('[data-slot="table-body"] [data-proximity-index]').filter(has=page.get_by_text(place, exact=True))
    expect(visible).to_have_count(1)
    expect(visible.get_by_text(data['node']['name'], exact=True)).to_be_visible()
    expect(visible.get_by_text(labels['presence']['present'], exact=True)).to_be_visible()
    expect(visible.get_by_text(bytes_text(total), exact=True)).to_be_visible()
    expect(visible.get_by_text(bytes_text(total) + ' / ' + bytes_text(total), exact=True)).to_be_visible()
    # AntD renders a check-circle at100; the progressbar retains exact
    # accessible numeric bounds instead of a literal100% text label.
    expect(visible.get_by_role('progressbar')).to_have_attribute('aria-valuenow', '100')
    expect(visible.get_by_role('progressbar')).to_have_attribute('aria-valuemin', '0')
    expect(visible.get_by_role('progressbar')).to_have_attribute('aria-valuemax', '100')
    expect(visible.locator('.ant-progress-text').get_by_role('img', name='check-circle')).to_be_visible()
    toggle = strings(report['locale'], 'storage')['detail']
    visible.get_by_role('button', name=toggle['expand'], exact=True).click()
    details = visible.locator('xpath=following-sibling::tr[1]')
    expect(details.get_by_text(storage['payload_path'], exact=True)).to_be_visible()
    expect(details.get_by_text(labels['acquisition']['download'], exact=True)).to_be_visible()
    visible.get_by_role('button', name=toggle['collapse'], exact=True).click()
    report.setdefault('completed_locations', []).append({'storage_id': storage['id'],
        'node_uuid': data['node']['uuid'], 'actual_path': storage['payload_path'], 'files': actual, 'api': row})
    if reload:
        with page.expect_response(lambda r: urlsplit(r.url).path == MODELS + '/' + str(data['model']['id'])
                                  and r.request.method == 'GET') as refreshed:
            page.reload()
        checked(refreshed.value.status == 200, 'detail reload failed')
        strict(presence(refreshed.value.json(), data, storage), row, 'full presence row after reload')
        checked(file_facts(storage['payload_path'], data['hub'].files) == actual, 'refresh changed payload')
        expect(model_detail_dialog(page, labels, data['name']).locator('#model-files')).to_be_visible()
    return row


def declared_format(name, body):
    """Decode one delivered file through its own standard envelope, never the product's.

    JSON is parsed with the json module, safetensors through the length-prefixed header the format
    defines. An HTML or plain-text error body satisfies neither, so a downloaded file that is really
    an error page cannot be reported as this format.
    """
    leading = body[:15].lstrip().lower()
    checked(not leading.startswith((b'<html', b'<!doctype', b'<body', b'<table')),
            f'{name} was delivered as an HTML document instead of its declared format')
    if name.endswith('.json'):
        parsed = json.loads(body.decode('utf-8'))
        checked(isinstance(parsed, dict) and parsed, f'{name} is not a JSON object')
        return {'format': 'json', 'parsed': True, 'keys': sorted(parsed)}
    if name.endswith('.safetensors'):
        checked(len(body) >= 8, f'{name} is shorter than a safetensors length prefix')
        header_length = struct.unpack('<Q', body[:8])[0]
        checked(0 < header_length <= len(body) - 8, f'{name} header length {header_length} is out of range')
        header = json.loads(body[8:8 + header_length].decode('utf-8'))
        checked(isinstance(header, dict) and header, f'{name} safetensors header is not an object')
        tensors = sorted(key for key in header if key != '__metadata__')
        checked(tensors, f'{name} safetensors header declares no tensors')
        return {'format': 'safetensors', 'parsed': True, 'header_length': header_length, 'tensors': tensors}
    return {'format': 'binary', 'parsed': False}


def downloaded_format(root, files):
    """16.8, download half: the bytes on disk are the declared formats, not substituted text."""
    facts = {}
    for name, value in files.items():
        path = Path(root) / name
        checked(path.is_file() and not path.is_symlink(), f'missing real payload: {path}')
        body = path.read_bytes()
        checked(body == value, f'payload bytes differ: {path}')
        facts[name] = {**declared_format(name, body), 'bytes': len(body),
                       'sha256': hashlib.sha256(body).hexdigest()}
    return facts


def served_error_page(root):
    """The exact bytes the fixture answers with when it is not serving a file.

    This is the body the download path must never persist as a model file. It is fetched from the
    fixture's own root route, so the calibration compares against a real response rather than a
    guess about what an error page looks like.
    """
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(root, timeout=10) as response:
        body = response.read()
        content_type = response.headers.get('Content-Type', '')
    return body, content_type


def refused_weight_left_no_substitute(target, hub, report):
    """16.8, failure half: a refused weight is not saved as the error page or any other substitute.

    Every file actually present is compared to the bytes the repository serves and to the fixture's
    real HTML error body, so a download that "succeeded" into an error document is a failure here
    rather than a file. Product bookkeeping is recorded, not ignored.
    """
    root = Path(target['payload_path'])
    present = sorted(entry.name for entry in root.iterdir()) if root.is_dir() else []
    error_body, error_type = served_error_page(hub.url + '/')
    checked(error_type.startswith('text/html'), f'fixture error body is not HTML: {error_type}')
    substituted, bookkeeping = [], []
    for name in present:
        blob = (root / name).read_bytes()
        looks_html = blob[:15].lstrip().lower().startswith((b'<html', b'<!doctype', b'<body', b'<table'))
        record = {'name': name, 'bytes': len(blob), 'is_error_body': blob == error_body, 'looks_html': looks_html}
        if name == '.gpuworks-model-complete.json' or name.endswith('.part.state'):
            bookkeeping.append(record)
            continue
        base = name[:-len('.part')] if name.endswith('.part') else name
        # Only a genuine byte prefix of the source repository file is acceptable, and an error body
        # is never a prefix of it because the fixture's error page is HTML.
        if base in hub.files and hub.files[base][:len(blob)] == blob and not looks_html:
            continue
        substituted.append(record)
    strict([item['name'] for item in substituted], [],
           'a failed download saved a substitute file, including any HTML error body')
    strict([item['name'] for item in substituted if item['is_error_body']], [],
           'a failed download saved the repository error page as the model file')
    report.setdefault('refused_weight_checks', []).append({
        'storage_id': target['id'], 'present': present, 'substituted': substituted,
        'bookkeeping': bookkeeping, 'error_body_bytes': len(error_body), 'error_content_type': error_type})
    return present


def wire_proof(data, start):
    hub = data['hub']
    checked(hub.wait_idle(), 'repository handlers remain after payload completed')
    records = hub.audit()[start:]
    proof = {}
    for name, payload in hub.files.items():
        intervals = []
        for record in records:
            if urlsplit(record['target']).path != hub.root + '/' + name or record['method'] != 'GET' or record['status'] not in (200, 206):
                continue
            match = re.fullmatch(r'bytes=(\d+)-(\d*)', record['range'] or '')
            offset = int(match[1]) if match else 0
            body = payload[offset:offset + record['bytes_sent']]
            strict(record['sha256_sent'], hashlib.sha256(body).hexdigest(), 'actual wire slice SHA256')
            checked(record['bytes_sent'] <= len(payload) - offset, 'wire overrun')
            intervals.append((offset, offset + record['bytes_sent']))
        end = 0
        for left, right in sorted(intervals):
            checked(left <= end, f'wire coverage hole for {name} at {end}')
            end = max(end, right)
        checked(end == len(payload), f'wire did not deliver complete {name}: {end}/{len(payload)}')
        proof[name] = {'covered_bytes': end, 'intervals': intervals, **payload_facts({name: payload})[name]}
    return {'records': records, 'files': proof}


def finish(page, artifacts, report, data, source):
    checked(not report['page_errors'], 'browser page errors: ' + repr(report['page_errors']))
    # A deliberate 409 is recorded in attributed_responses. Every other console
    # error and every other >=400 still fail the case.
    rest = unattributed_console_errors(report['console_errors'], report.get('attributed_responses') or [])
    checked(not rest, 'browser console errors: ' + repr(rest))
    checked(not report['http_errors'], 'unexpected browser HTTP errors: ' + repr(report['http_errors']))
    artifacts.write_json('repository-wire.json', source)
    artifacts.write_json('component-assertions.json', {'case': report['contract'],
        'declared_scope': 'URL local storage only; enum/state denominators are in the committed manifest',
        'actual_locations': report.get('completed_locations'), 'mutations': report['mutations']})
    report['evidence'].extend([{'layer': 'ui', 'locations': report.get('completed_locations')},
        {'layer': 'request', 'requests': report['mutations'], 'responses': report['browser_responses']},
        {'layer': 'persistence', 'model': report['last_model_observation']},
        {'layer': 'execution', 'source': source, 'agent_pid': report['agent_pid']}])
    report['last_step'] = 'exact UI/request/disk/wire facts and refresh passed'


def exercise_duplicate_import(page, labels, data, report, server, token):
    """16.2: import the same repository identity twice through the real register dialog.

    The first attempt is the fixture's already registered row, so the second attempt is a
    genuine repeat import of the same source_id. What the second attempt actually answers is
    recorded rather than assumed: this product resolves the existing identity and updates it,
    so the assertion is that both attempts answer 201 for the same resource id, and that an
    independent collection read still finds exactly one model.
    """
    hub = data['hub']
    title = labels['action']['register']
    body = {'source_url': hub.url, 'source_id': data['source_id'], 'name': data['name']}
    report['step'] = 'repeat the same directory import through the real register dialog'

    # The toolbar button carries a plus icon, so its accessible name is not the bare label; the
    # visible label is what the operator clicks, whether or not the list already has a row.
    trigger = page.get_by_role('button').filter(has_text=re.compile('^' + re.escape(title) + '$')).first

    def import_once(attempt):
        trigger.click()
        dialog = page.get_by_role('dialog', name=title, exact=True)
        expect(dialog).to_be_visible()
        source = dialog.get_by_label(labels['column']['source'], exact=True)
        source.click()
        page.get_by_text(labels['source']['url'], exact=True).click()
        expect(dialog).to_have_css('width', '720px', timeout=12000)
        url = dialog.get_by_label(labels['register']['directoryURL'], exact=True)
        url.fill(hub.url + '/')
        with page.expect_response(lambda r: urlsplit(r.url).path == MODELS + '/preview-url'
                                  and r.request.method == 'POST') as previewed:
            button(dialog, labels['register']['parseDirectory']).click()
        preview = previewed.value
        checked(preview.status == 200, f'import attempt {attempt} preview HTTP {preview.status}')
        strict(preview.request.post_data_json, {'url': hub.url + '/'},
               f'import attempt {attempt} preview body')
        expect(dialog.get_by_text(labels['register']['urlParsed'], exact=True)).to_be_visible()
        # The parse normalizes the typed value, so the confirmed URL is the one that gets registered.
        expect(url).to_have_value(hub.url)
        # The preview fills its own identity. Re-importing the same repository means putting back
        # the identity the registered row already carries, which is what an operator repeating an
        # import submits; taking the preview defaults would import a different source_id instead.
        dialog.get_by_label(labels['column']['name'], exact=True).fill(data['name'])
        dialog.get_by_label(labels['register']['modelID'], exact=True).fill(data['source_id'])
        confirm = dialog.get_by_role('button').filter(
            has_text=re.compile('^' + re.escape(labels['register']['confirm']) + '$'))
        expect(confirm).to_be_enabled()
        with page.expect_response(lambda r: urlsplit(r.url).path == MODELS + '/register-url'
                                  and r.request.method == 'POST') as registered:
            confirm.click()
        response = registered.value
        expect(dialog).not_to_be_visible()
        checked(response.status == 201, f'import attempt {attempt} register HTTP {response.status}; {response.text()}')
        strict(response.request.post_data_json, body, f'import attempt {attempt} register body')
        strict(parse_qs(urlsplit(response.url).query), {}, f'import attempt {attempt} register query')
        observed = response.json()
        return {'attempt': attempt, 'status': response.status, 'body': body,
                'id': observed['id'], 'source': observed['source'],
                'source_id': observed['source_id'], 'source_url': observed['source_url'],
                'name': observed['name'], 'created_at': observed['created_at']}

    first = import_once(1)
    checked(first['id'] == data['model']['id'], 'the browser import did not address the registered row')
    after_first = len(hub.audit())
    second = import_once(2)
    reread = hub.audit()[after_first:]
    checked(any(record['method'] in ('GET', 'HEAD') and record['status'] == 200
                and record['target'] == hub.root + '/config.json' for record in reread),
            'the repeat import did not re-read the repository directory')
    # The contract for this product: the repeat answers the same resource, so every identity field
    # and the creation time are unchanged. A 201 alone is not treated as proof of idempotency.
    strict(second, {**first, 'attempt': 2}, 'the repeat import answered a different resource')
    code, listed = http(server.base, 'GET', MODELS + '?source_id=' + quote(data['source_id'], safe=''), token)
    checked(code == 200, f'owned model filter HTTP {code}')
    strict(listed['total'], 1, 'the repeat import produced a second resource')
    strict([item['id'] for item in listed['items']], [data['model']['id']],
           'the repeat import listed a different model id')
    code, every = http(server.base, 'GET', MODELS, token)
    checked(code == 200, f'independent model collection HTTP {code}')
    strict([item['id'] for item in every['items'] if item['source_id'] == data['source_id']],
           [data['model']['id']], 'the collection holds one row for the imported identity')
    # The list is the product's answer; the stored rows are the authority behind it.
    conn = _files_db(server)
    try:
        rows = conn.execute('SELECT COUNT(*) FROM models WHERE lower(source_id) = lower(?)',
                            (data['source_id'],)).fetchone()[0]
    finally:
        conn.close()
    strict(rows, 1, 'the repeat import stored a second model row')
    expect(page.get_by_role('row').filter(
        has=page.get_by_role('button', name=data['name'], exact=True))).to_have_count(1)
    report['duplicate_import'] = {'attempts': [first, second], 'same_id': first['id'] == second['id'],
        'second_status': second['status'], 'directory_reread': True, 'filtered_total': listed['total'],
        'stored_rows': rows}
    report.setdefault('evidence', []).append({'layer': 'ui', 'register_dialog': title,
        'duplicate_entries': 2, 'second_attempt_status': second['status'],
        'second_attempt_id': second['id']})
    return report['duplicate_import']


def exercise_cancel_transfer(page, labels, data, server, token, report, storage):
    """16.5 first clause: withdraw a live transfer from its own row and prove it stopped."""
    report['step'] = '16.5 cancel a real in-flight transfer and verify it actually terminated'
    hub = data['hub']
    # The source holds the weight mid-response, so the transfer is genuinely still running
    # when the operator cancels it instead of having finished a moment earlier.
    hub.interrupt_next_weight(65536, hold=True)
    mark = len(hub.audit())
    download(page, labels, data, storage, report, server=server, token=token)
    poll(page, lambda: hub.wait_interrupted(0), 'real partial-send barrier before the cancel')
    interrupted = next(record for record in hub.audit()[mark:] if record['interrupted'])
    checked(interrupted['bytes_sent'] == 65536 and not interrupted['complete'],
            'the transfer held for the cancel test was not cut inside the weight file')

    def in_flight():
        current = document(server, token, data)
        report['last_model_observation'] = current
        row = presence(current, data, storage)
        return row if row and row['state'] == 'downloading' and 0 < row['downloaded'] < row['size_bytes'] else None
    moving = poll(page, in_flight, 'in-flight transfer to cancel')
    part = Path(storage['payload_path']) / 'model.safetensors.part'
    checked(part.is_file() and not part.is_symlink(), 'no real partial file to keep for a retry')
    detail = open_detail(page, server, labels, data)
    row = detail.locator('#model-files').locator('[data-slot="table-body"] [data-proximity-index]')
    expect(row).to_have_count(1)
    expect(row.get_by_text(labels['presence']['downloading'], exact=True)).to_be_visible()
    trigger = button(row, labels['presenceAction']['cancelDownload'])
    expect(trigger).to_have_count(1)
    frozen = presence(document(server, token, data), data, storage)
    checked(frozen['state'] == 'downloading' and frozen['downloaded'] == moving['downloaded'],
            'the held transfer advanced before the operator canceled it')
    trigger.click()
    confirm = labels['presenceAction']['cancelDownloadConfirm']
    popup = page.locator('.ant-popconfirm').filter(has=page.get_by_text(confirm, exact=True))
    expect(popup.get_by_text(confirm, exact=True)).to_be_visible()
    cancel_path = MODELS + '/' + str(data['model']['id']) + '/download/cancel'
    cancel_body = {'storage_id': storage['id'], 'node_uuid': data['node']['uuid']}
    before = len(report['mutations'])
    response = mutation(page, report, button(popup, labels['presenceAction']['cancelDownload']).click,
                        'POST', cancel_path, cancel_body, 200)
    # One request, and no other mutation rides along: the control withdraws the task it belongs to.
    strict(report['mutations'][before:],
           [{'method': 'POST', 'path': cancel_path, 'query': '', 'body': cancel_body}],
           'the cancel control sent exactly one cancel request and nothing else')
    strict(response.json().get('state'), 'canceled', 'the cancel answered a state other than canceled')
    expect(page.locator('.ant-message').get_by_text(labels['presenceMessage']['downloadCanceled'],
                                                    exact=True)).to_be_visible()
    canceled = wait_state(page, server, token, data, storage, 'canceled', report)
    checked(canceled['acquisition'] == 'download', 'the canceled row lost its download identity')
    strict(canceled['message'], 'Download canceled by user', 'the canceled row lost the withdrawal reason')
    expect(row.get_by_text(labels['presence']['canceled'], exact=True)).to_be_visible()
    # The row shows the localized canceled label, not the server's message text:
    # the store keeps "Download canceled by user" (models_download_cancel.go:67,
    # asserted strictly above), while the page renders presence.canceled. Asserting
    # the server string in the DOM was asserting a round-trip the UI does not make.
    checked(canceled['message'] not in row.inner_text(),
            'the canceled row renders the raw server message instead of the localized state')
    # The independent read agrees with the page, and the bytes the row had already reported
    # are neither zeroed nor inflated to the finished total by the withdrawal.
    checked(canceled['downloaded'] == frozen['downloaded'] and canceled['size_bytes'] == frozen['size_bytes'],
            'canceling rewrote the bytes the in-flight transfer had already reported')
    checked(0 < canceled['downloaded'] < canceled['size_bytes'],
            'the canceled row renders its progress as empty or as a finished transfer')
    # Withdrawing the row is a request to the writer, not a label change on the server. The cancel
    # route delivers the stop to the agent before it answers, so the stop has already been applied
    # when the response above arrived. The source keeps holding the chunk for a moment longer and is
    # then released, which tears the held socket down under the agent: a transfer that was merely
    # relabelled sees a broken response and asks the source again for the missing bytes, while one
    # that was really stopped has already left its worker and issues nothing.
    stopped_mark = len(hub.audit())
    page.wait_for_timeout(1500)
    hub.release()
    page.wait_for_timeout(5000)
    arrived = hub.audit()[stopped_mark:]
    checked(arrived == [], f'the canceled transfer kept asking the source for bytes: {arrived}')
    settled = presence(document(server, token, data), data, storage)
    checked(settled['state'] == 'canceled', 'the canceled row left the canceled state after the task stopped')
    weight = len(hub.files['model.safetensors'])
    # The bytes already on disk are the reason the transfer is canceled instead of deleted: the
    # partial file has to survive the withdrawal so a Retry can continue it. The chunk writer
    # preallocates the full size and fills chunks out of order, so the file is not a prefix and
    # neither existence nor a nonzero count proves resumable data. Instead every byte the transfer
    # did write has to be the source's own byte at that offset - any other value would be a hole or
    # garbage passed off as downloaded - and the finished name must not exist, so the cancel did not
    # quietly publish a sparse file as a completed model.
    checked(part.is_file() and not part.is_symlink(),
            'the partial file the cancel dialog promises to keep for a retry was deleted')
    body_on_disk = part.read_bytes()
    strict(len(body_on_disk), weight, 'the kept partial is not the resumable weight file')
    payload = hub.files['model.safetensors']
    foreign = [index for index, (kept, source) in enumerate(zip(body_on_disk, payload))
               if kept and kept != source]
    checked(foreign == [], f'bytes the transfer wrote are not the source payload at those offsets: {foreign[:8]}')
    kept_bytes = sum(1 for byte in body_on_disk if byte)
    checked(kept_bytes > 0, 'the kept partial holds no payload bytes at all')
    checked(not (Path(storage['payload_path']) / 'model.safetensors').exists(),
            'the canceled transfer left a finished weight file behind')
    report['cancel_transfer'] = {'state': canceled['state'], 'message': canceled['message'],
        'downloaded': canceled['downloaded'], 'size_bytes': canceled['size_bytes'],
        'kept_part_bytes': len(body_on_disk), 'kept_part_written': kept_bytes,
        'kept_part_foreign': len(foreign), 'finished_weight_absent': True,
        'requests_after_stop': len(arrived), 'window_ms': 5000,
        'state_after_window': settled['state'], 'cancel_body': cancel_body, 'status': response.status}
    # Test hygiene, not a contract claim: the canceled row and its kept bytes are reclaimed so
    # the flows that follow start from a storage with no replica on it.
    path = f"{MODELS}/{data['model']['id']}/nodes/{data['node']['uuid']}"
    code, reclaimed = http(server.base, 'DELETE', path + '?storage_id=' + str(storage['id']), token)
    checked(code == 200, f'clearing the canceled row HTTP {code}')
    # The reclaim answers with the bytes the server still attributes to that row, which is the
    # row the cancel withdrew: the withdrawal kept the data and did not lose its byte accounting.
    checked(isinstance(reclaimed, dict), 'the cancel reclaim answered no eviction body')
    strict(reclaimed.get('physical_bytes'), canceled['downloaded'],
           'the withdrawn row no longer accounts for the bytes it had reported')
    poll(page, lambda: not Path(storage['payload_path']).exists(), 'canceled payload directory absent')
    checked(presence(document(server, token, data), data, storage) is None,
            'the canceled row survived the eviction used to reclaim it')
    if detail.is_visible():
        close_detail(page, labels, data)
    return report['cancel_transfer']


# Mainline: the same directory is imported twice, then a real source503 fails the row and the
# same row's public Retry recovers it; two owned replicas download, cancel-delete is refused,
# only the target is deleted, and the guard bytes plus model metadata survive.
def exercise_download_delete(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale, data)
    hub, target, guard = data['hub'], data['storages'][0], data['storages'][1]
    exercise_duplicate_import(page, labels, data, report, server, token)
    # 16.5, first clause: a live transfer is withdrawn from its own row, and the withdrawal is
    # verified against the task rather than against the toast. It runs on the target storage and
    # reclaims it, so the download flows below start from a storage with no replica on it.
    exercise_cancel_transfer(page, labels, data, server, token, report, target)
    # 16.4, server-failure half: the repository refuses the weight with a real 503, the row has to
    # reach the failed state carrying the exact cause, and the same row's public Retry has to
    # finish the transfer as the one resource it already was.
    hub.set_weight_failure(True)
    start = len(hub.audit())
    download(page, labels, data, target, report, server=server, token=token)
    failed = wait_state(page, server, token, data, target, 'failed', report)
    checked(failed['acquisition'] == 'download' and failed['path'] == target['payload_path'],
            'failed row lost download identity')
    host = urlsplit(hub.base).netloc
    expected_error = (f'download model.safetensors: {host} failed: {hub.url}/model.safetensors: '
                      'download: unexpected probe status 503')
    strict(failed.get('message'), expected_error, 'exact external503 failure cause')
    checked(any(record['status'] == 503 and record['target'] == hub.root + '/model.safetensors'
                for record in hub.audit()[start:]), 'no actual weight503 observed')
    checked(not Path(target['payload_path'], 'model.safetensors').exists(),
            'the refused weight was written as a finished file')
    # 16.8, failure half: the refused weight is not persisted as an error page or a substitute.
    refused_weight_left_no_substitute(target, hub, report)
    dialog = open_detail(page, server, labels, data)
    row = dialog.locator('#model-files [data-slot="table-body"] [data-proximity-index]')
    expect(row).to_have_count(1)
    expect(row.get_by_text(labels['presence']['failed'], exact=True)).to_be_visible()
    # Same as the canceled row above: the page renders the localized failed state
    # and does not echo the server's cause text. The cause itself is still checked
    # strictly against the independent read below.
    checked(failed['message'] not in row.inner_text(),
            'the failed row renders the raw server cause instead of the localized state')
    page.reload()
    row = model_detail_dialog(page, labels, data['name']).locator('#model-files [data-slot="table-body"] [data-proximity-index]')
    expect(row.get_by_text(labels['presence']['failed'], exact=True)).to_be_visible()
    hub.set_weight_failure(False)
    before_retry = len(report['mutations'])
    path = MODELS + '/' + str(data['model']['id']) + '/download'
    body = {'storage_id': target['id'], 'node_uuids': [data['node']['uuid']]}
    mutation(page, report, button(row, labels['action']['retry']).click, 'POST', path, body, 202)
    strict(report['mutations'][before_retry:],
           [{'method': 'POST', 'path': path, 'query': '', 'body': body}],
           'exactly one same-row public Retry request')
    assert_ready(page, server, token, labels, data, target, report)
    retried = document(server, token, data)
    checked(len([r for r in retried.get('nodes', []) if not r.get('cache')]) == 1,
            'the retry was recorded as a second resource')
    report['failure_retry'] = {'failed': failed, 'retry_body': body, 'rows_after_retry': 1,
                              'same_row': True}
    close_detail(page, labels, data)
    # 16.4, partial-transfer and broken-connection half: the second replica is held mid-file and
    # the source stream is closed under it, so the operator sees a real incomplete transfer before
    # the agent resolves it. The dialog shell and the refused submit stay on this storage too.
    hub.interrupt_next_weight(65536, hold=True)
    download(page, labels, data, guard, report, server=server, token=token, refuse=True, shell=True)
    poll(page, lambda: hub.wait_interrupted(0), 'real partial-send barrier')
    broken = next(record for record in hub.audit()[start:] if record['interrupted'])
    checked(broken['bytes_sent'] == 65536 and not broken['complete'],
            'the partial response was not held inside the weight file')
    part = Path(guard['payload_path']) / 'model.safetensors.part'

    def incomplete():
        row = presence(document(server, token, data), data, guard)
        return row if row and row['state'] == 'downloading' and 0 < row['downloaded'] < row['size_bytes'] else None
    held = poll(page, incomplete, 'held transfer incomplete progress')
    checked(held['size_bytes'] == sum(map(len, hub.files.values())), 'in-flight total is not the fixture payload')
    checked(part.is_file() and not part.is_symlink(), 'no real in-progress partial file')
    detail = open_detail(page, server, labels, data)
    # The files are a ResourceTable (ModelFilesSection.tsx:200-212): one row per
    # storage, so the transfer's row is the table row naming this storage. The
    # previous :scope > div wrapper no longer exists.
    busy = detail.locator('#model-files [data-slot="table-body"] [data-proximity-index]').filter(has_text=guard['name'])
    expect(busy).to_have_count(1)
    bar = busy.get_by_role('progressbar')
    expect(bar).to_have_count(1)
    shown = int(bar.get_attribute('aria-valuenow'))
    # 16.3: the row the operator sees while bytes are still arriving has to report the transfer's
    # actual progress and stage, not a placeholder and not the finished state. The source is held at
    # the barrier, so the record cannot move between the two reads; the bar is then required to agree
    # with the percentage the API reports for that same frozen instant.
    api_percent = 100.0 * held['downloaded'] / held['size_bytes']
    frozen = presence(document(server, token, data), data, guard)
    checked(frozen['state'] == 'downloading' and frozen['downloaded'] == held['downloaded'],
            'the held transfer advanced while the detail was opening')
    checked(shown == int(api_percent),
            f'the in-flight bar disagrees with the reported progress: {shown} vs {api_percent}')
    checked(0 < shown < 100, f'the held transfer is rendered as finished or empty: {shown}')
    expect(busy.get_by_text(labels['presence']['downloading'], exact=True)).to_be_visible()
    # 16.6: a refresh in the middle of the transfer has to read the task back instead of resetting
    # it. The source is still held, so nothing can move underneath the reload, and the reopened
    # detail must show the same in-flight progress rather than a zeroed or a finished row.
    with page.expect_response(lambda r: urlsplit(r.url).path == MODELS + '/' + str(data['model']['id'])
                              and r.request.method == 'GET') as reloaded:
        page.reload()
    checked(reloaded.value.status == 200, 'detail reload during the transfer failed')
    replayed = presence(reloaded.value.json(), data, guard)
    checked(replayed['state'] == 'downloading' and replayed['downloaded'] == held['downloaded'],
            'a refresh zeroed or finished the in-flight transfer')
    reopened = model_detail_dialog(page, labels, data['name'])
    expect(reopened.locator('#model-files')).to_be_visible()
    regrouped = reopened.locator('#model-files [data-slot="table-body"] [data-proximity-index]').filter(has_text=guard['name'])
    expect(regrouped).to_have_count(1)
    reload_bar = regrouped.get_by_role('progressbar')
    expect(reload_bar).to_have_count(1)
    replayed_bar = int(reload_bar.get_attribute('aria-valuenow'))
    checked(replayed_bar == shown,
            f'the refreshed detail rendered different progress for the same frozen transfer: '
            f'{replayed_bar} vs {shown}')
    hub.release()
    assert_ready(page, server, token, labels, data, guard, report)
    records = hub.audit()[start:]
    broken_index = next(i for i, record in enumerate(records) if record['interrupted'])
    match = re.fullmatch(r'bytes=(\d+)-(\d+)', broken['range'] or '')
    checked(match is not None, 'the interrupted request was not a range chunk')
    expected_range = f'bytes={int(match[1]) + broken["bytes_sent"]}-{match[2]}'
    checked(any(record['target'] == broken['target'] and record['range'] == expected_range
                and record['complete'] and record['status'] == 206 for record in records[broken_index + 1:]),
            'the broken transfer did not resume at the exact next unsent byte')
    checked(not part.exists() and not Path(str(part) + '.state').exists(),
            'a completed download left its partial/state files behind')
    report['partial_transfer'] = {'interrupted': broken, 'held_state': held['state'],
        'held_bar': shown, 'api_percent': round(api_percent, 3), 'downloaded': held['downloaded'],
        'size_bytes': held['size_bytes'], 'resumed_range': expected_range, 'automatic': True,
        'part_removed': True, 'refresh_state': replayed['state'],
        'refresh_bar': replayed_bar, 'refresh_downloaded': replayed['downloaded']}
    close_detail(page, labels, data)
    source = wire_proof(data, start)
    dialog = open_detail(page, server, labels, data)
    before = file_facts(guard['payload_path'], data['hub'].files)
    files = dialog.locator('#model-files')
    local_label = strings(locale, 'common')['storageType']['local']
    target_row = files.locator('[data-slot="table-body"] [data-proximity-index]').filter(has=page.get_by_text(target['name'] + ' · ' + local_label, exact=True))
    remove = target_row.get_by_role('button', name=labels['files']['deleteOnNode'], exact=True)
    remove.click()
    title = labels['files']['deleteOnNodeConfirm'].replace('{{node}}', data['node']['name'])
    popup = page.locator('.ant-popconfirm').filter(has=page.get_by_text(title, exact=True))
    expect(popup.get_by_text(title, exact=True)).to_be_visible()
    expect(popup.get_by_text(labels['files']['deleteOnNodeDescription'], exact=True)).to_be_visible()
    before_mutations = list(report['mutations'])
    button(popup, labels['action']['cancel']).click()
    expect(popup).not_to_be_visible()
    strict(report['mutations'], before_mutations, 'cancel must send no mutation')
    file_facts(target['payload_path'], data['hub'].files)
    path = f"{MODELS}/{data['model']['id']}/nodes/{data['node']['uuid']}"
    from model_file_delete_blocks import exercise_node_delete_blocks
    exercise_node_delete_blocks(page, report, server, token, labels, locale, data, target, remove, path,
                                document, presence)
    remove.click()
    popup = page.locator('.ant-popconfirm').filter(has=page.get_by_text(title, exact=True))
    expect(popup.get_by_text(title, exact=True)).to_be_visible()
    mutation(page, report, button(popup, labels['action']['delete']).click,
             'DELETE', path, None, 200, {'storage_id': [str(target['id'])]})
    poll(page, lambda: not Path(target['payload_path']).exists(), 'target directory absent')
    current = document(server, token, data)
    checked(presence(current, data, target) is None, 'deleted storage/node row remains')
    strict(file_facts(guard['payload_path'], data['hub'].files), before, 'guard replica unchanged')
    checked(presence(current, data, guard)['state'] == 'present', 'guard replica not present')
    expect(files.locator('[data-slot="table-body"] [data-proximity-index]')).to_have_count(1)
    expect(files.get_by_text(guard['name'] + ' · ' + local_label, exact=True)).to_be_visible()
    page.reload()
    expect(model_detail_dialog(page, labels, data['name']).locator('#model-files [data-slot="table-body"] [data-proximity-index]')).to_have_count(1)
    strict(file_facts(guard['payload_path'], data['hub'].files), before, 'guard survives reload')
    report['deletion'] = {'node_uuid': data['node']['uuid'], 'storage_id': target['id'],
                          'target_absent': True, 'guard_files': before, 'model_still_exists': current['id']}
    finish(page, artifacts, report, data, source)


# Mainline: real source503 -> failed row -> same row Retry -> source recovery,
# retaining the originally chosen non-default storage and node.
def exercise_failure_retry(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale, data)
    hub, target = data['hub'], data['storages'][0]
    # 1.10: the submit-rejection side of the draft contract. Until now this case only exercised the
    # background retry, so a refused submit was never seen here. A real 409 is produced by dropping the
    # storage's node membership for the duration of one request, and the draft - the exact request body,
    # plus the storage and node still chosen in the form - has to survive it. It runs against the second
    # storage so the rest of this case still starts from a clean target.
    download(page, labels, data, data['storages'][1], report, server=server, token=token, refuse=True,
             shell=True, stop_only=True)
    # 16.5, first clause: a live transfer is withdrawn from its own row, and the withdrawal is
    # verified against the task rather than against the toast. It runs before the failed transfer
    # below and reclaims its storage, so that flow starts from an empty storage.
    exercise_cancel_transfer(page, labels, data, server, token, report, target)
    # 1.2: the two write states this page can be in that a healthy fixture never produces - no writable
    # storage at all, and one storage that cannot be written next to one that can.
    trigger = page.get_by_role('row').filter(
        has=page.get_by_role('button', name=data['name'], exact=True)).get_by_role(
        'button', name=labels['action']['downloadToNode'], exact=True)
    download_title = labels['download']['title'].replace('{{name}}', data['name'])
    def open_download_dialog():
        trigger.click()
        opened = page.get_by_role('dialog', name=download_title, exact=True)
        expect(opened).to_be_visible()
        return opened

    exercise_storage_write_states(page, labels, data, server, report, open_download_dialog)
    rejected = report['submit_rejection_draft']
    strict(rejected['status'], 409, 'refused submit did not answer 409')
    strict(rejected['body_exact'], True, 'the refused submit did not carry the draft it was given')
    strict([rejected['storage_kept'], rejected['node_kept'], rejected['dialog_open']], [True, True, True],
           'the refused submit dropped the draft selections')
    strict(rejected['persisted_row'], False, 'the refused submit persisted a file row')
    hub.set_weight_failure(True)
    start = len(hub.audit())
    download(page, labels, data, target, report, server=server, token=token, shell=True)
    failed = wait_state(page, server, token, data, target, 'failed', report)
    checked(failed['state'] == 'failed', 'source503 reaches actual failed file state')
    checked(failed['acquisition'] == 'download' and failed['path'] == target['payload_path'], 'failed row lost download identity')
    host = urlsplit(hub.base).netloc
    expected_error = (f'download model.safetensors: {host} failed: {hub.url}/model.safetensors: '
                      'download: unexpected probe status 503')
    strict(failed.get('message'), expected_error, 'exact external503 failure cause')
    checked(any(r['status'] == 503 and r['target'] == hub.root + '/model.safetensors' for r in hub.audit()[start:]),
            'no actual weight503 observed')
    checked(not Path(target['payload_path'], 'model.safetensors').exists(), 'source503 incorrectly left complete weight')
    # 16.8, failure half: a refused weight must not be saved as the error page or any other substitute.
    refused_weight_left_no_substitute(target, hub, report)
    dialog = open_detail(page, server, labels, data)
    row = dialog.locator('#model-files [data-slot="table-body"] [data-proximity-index]')
    expect(row).to_have_count(1)
    state = row.get_by_text(labels['presence']['failed'], exact=True)
    expect(state).to_be_visible()
    state.focus()
    expect(page.get_by_role('tooltip', name=labels['presence']['failed'], exact=True)).to_be_visible()
    expect(row.get_by_text(failed['message'], exact=True)).to_have_count(0)
    row.get_by_role('button', name=strings(locale, 'storage')['detail']['expand'], exact=True).click()
    details = row.locator('xpath=following-sibling::tr[1]')
    expect(details.get_by_text(failed['message'], exact=True)).to_be_visible()
    page.reload()
    row = model_detail_dialog(page, labels, data['name']).locator('#model-files [data-slot="table-body"] [data-proximity-index]')
    expect(row.get_by_text(labels['presence']['failed'], exact=True)).to_be_visible()
    hub.set_weight_failure(False)
    before = len(report['mutations'])
    path = MODELS + '/' + str(data['model']['id']) + '/download'
    body = {'storage_id': target['id'], 'node_uuids': [data['node']['uuid']]}
    retry = button(row, labels['action']['retry'])
    expect(retry).to_have_text('')
    mutation(page, report, retry.click, 'POST', path, body, 202)
    strict(report['mutations'][before:], [{'method': 'POST', 'path': path, 'query': '', 'body': body}], 'exactly one same-row public Retry request')
    assert_ready(page, server, token, labels, data, target, report)
    report['failure_recovery'] = {'failed': failed, 'retry_body': body, 'same_row': True}
    from model_file_delete_blocks import delete_present_node_copy
    delete_present_node_copy(page, report, server, token, labels, locale, data, target, document, presence,
                             lambda *args: mutation(page, report, *args))
    finish(page, artifacts, report, data, wire_proof(data, start))


# Mainline: partial response held in-flight -> observe actual downloading and
# partial file -> close source stream -> automatic nonzero Range resume -> hashes.
def exercise_interrupted_recovery(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale, data)
    hub, target = data['hub'], data['storages'][0]
    # 16.5, first clause: a live transfer is withdrawn from its own row, and the withdrawal is
    # verified against the task rather than against the toast. It reclaims its storage, so the
    # resume flow below starts from a storage with no replica on it.
    exercise_cancel_transfer(page, labels, data, server, token, report, target)
    hub.interrupt_next_weight(65536, hold=True)
    start = len(hub.audit())
    download(page, labels, data, target, report, server=server, token=token, refuse=True, shell=True)
    poll(page, lambda: hub.wait_interrupted(0), 'real partial-send barrier')
    interrupted = next(r for r in hub.audit()[start:] if r['interrupted'])
    checked(interrupted['bytes_sent'] == 65536 and not interrupted['complete'], 'partial response not held inside file')
    def partial_observation():
        current = document(server, token, data)
        report['last_model_observation'] = current
        row = presence(current, data, target)
        return row if row and row['state'] == 'downloading' and 0 < row['downloaded'] < row['size_bytes'] else None
    moving = poll(page, partial_observation, 'held transfer incomplete progress')
    checked(moving['state'] == 'downloading', 'held response reaches actual downloading file state')
    checked(moving['size_bytes'] == sum(map(len, hub.files.values())), 'in-flight total not exact fixture payload')
    part = Path(target['payload_path']) / 'model.safetensors.part'
    checked(part.is_file() and not part.is_symlink(), 'no real in-progress partial file')
    dialog = open_detail(page, server, labels, data)
    expect(dialog.locator('#model-files').get_by_text(labels['presence']['downloading'], exact=True)).to_be_visible()
    # No progress-bar matrix here on purpose. The detail renders the record it fetched
    # when it opened and does not poll, so a bar read mid-transfer is a snapshot of
    # whatever the previous transfer left; requiring it to move would be testing a
    # behaviour the product does not offer. The moving numbers are asserted against
    # the API below, which is where this product actually reports them.
    held_bar = dialog.locator('#model-files [data-slot="table-body"] [data-proximity-index]').get_by_role('progressbar')
    held_value = int(held_bar.get_attribute('aria-valuenow')) if held_bar.count() == 1 else None
    api_percent = 100.0 * moving['downloaded'] / moving['size_bytes']
    report['held_bar'] = {'count': held_bar.count(), 'aria_valuenow': held_value,
                          'api_percent': round(api_percent, 3)}
    # 16.3: the progress the operator sees has to be the transfer's actual progress, not a placeholder.
    # The record is held mid-file with 65536 bytes of a larger payload in, and the bar the detail panel
    # rendered for that record is required to agree with the numbers the API reports for the same
    # instant - the panel truncates, so the bar must be the truncation of the API percentage and
    # strictly below 100. A bar pinned to 100 everywhere passes the completion assertion and fails here.
    checked(held_value is not None, 'the in-flight record rendered no progress bar')
    checked(held_value == int(api_percent),
            f'the in-flight bar disagrees with the reported progress: {held_value} vs {api_percent}')
    checked(0 < held_value < 100, f'the in-flight bar claims a finished transfer: {held_value}')
    # 16.6: reload the page while the transfer is still held, so the background task has to be read
    # back rather than recreated. The source is blocked at the barrier, so nothing can move underneath
    # the reload: the reopened detail must render the same in-flight progress as before it, which a
    # zeroed row or a row marked finished cannot do.
    frozen = presence(document(server, token, data), data, target)
    checked(frozen['state'] == 'downloading' and frozen['downloaded'] == moving['downloaded'],
            'the held transfer advanced while the detail was opening')
    with page.expect_response(lambda r: urlsplit(r.url).path == MODELS + '/' + str(data['model']['id'])
                              and r.request.method == 'GET') as reloaded:
        page.reload()
    checked(reloaded.value.status == 200, 'detail reload during the transfer failed')
    replayed = presence(reloaded.value.json(), data, target)
    checked(replayed['state'] == 'downloading' and replayed['downloaded'] == frozen['downloaded'],
            'a refresh zeroed or finished the in-flight transfer')
    reopened = model_detail_dialog(page, labels, data['name'])
    expect(reopened.locator('#model-files')).to_be_visible()
    reload_bar = reopened.locator('#model-files [data-slot="table-body"] [data-proximity-index]').get_by_role('progressbar')
    expect(reload_bar).to_have_count(1)
    replayed_bar = int(reload_bar.get_attribute('aria-valuenow'))
    checked(replayed_bar == held_value,
            f'the refreshed detail rendered different progress for the same frozen transfer: '
            f'{replayed_bar} vs {held_value}')
    report['held_bar'].update({'refresh_bar': replayed_bar, 'refresh_state': replayed['state'],
                               'refresh_downloaded': replayed['downloaded']})
    hub.release()
    assert_ready(page, server, token, labels, data, target, report)
    source = wire_proof(data, start)
    records = source['records']
    broken_index = next(i for i, r in enumerate(records) if r['interrupted'])
    broken = records[broken_index]
    match = re.fullmatch(r'bytes=(\d+)-(\d+)', broken['range'] or '')
    checked(match is not None, 'interrupted agent request was not a range chunk')
    expected_range = f'bytes={int(match[1]) + broken["bytes_sent"]}-{match[2]}'
    checked(any(r['target'] == broken['target'] and r['range'] == expected_range and r['complete']
                and r['status'] == 206 for r in records[broken_index+1:]),
            'automatic resume did not request the exact next unsent byte to the same chunk end')
    checked(len(report['mutations']) == 4, 'resume added a mutation or the refused download was not sent')
    report['interrupted_recovery'] = {'held_state': moving, 'interrupted': broken,
        'resumed_range': expected_range, 'automatic': True, 'part_removed': not part.exists()}
    checked(not part.exists() and not Path(str(part) + '.state').exists(), 'successful download left partial/state')
    from model_file_delete_blocks import delete_present_node_copy
    delete_present_node_copy(page, report, server, token, labels, locale, data, target, document, presence,
                             lambda *args: mutation(page, report, *args))
    finish(page, artifacts, report, data, source)
