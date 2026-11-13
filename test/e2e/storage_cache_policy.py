#!/usr/bin/env python3
"""NFS cache node policy on an isolated server and SQLite.

Run: python3 test/e2e/storage_cache_policy.py --locale en
Fixture: 25 offline nodes, one nfs_cache storage, one non-admin user. No agent.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import re
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest

# The sizes the table offers, from components/resourceTable/columnPrefs.ts:27.
PAGE_SIZE_OPTIONS = [10, 20, 50, 100]
from network_storage_lifecycle import locale_strings, read
from platform_token_read_revoke import button, check
from storage_cache_policy_inventory import exercise_node_path_required, exercise_node_policy_inventory
from storage_cache_policy_dialogs import (
    GIB, exercise_cache_limit_bounds, exercise_cache_limit_defaults, exercise_cache_limit_steps,
    exercise_server_limit_range,
    exercise_defaults_conflict,
    exercise_defaults_dialog,
    exercise_editor_dismissals,
    exercise_node_editor_keyboard,
    mutate)

CONTRACT = 'gpuworks/storage/nfs-cache-node-policy'
CASE = '存储 / NFS Cache 节点缓存策略 / 单节点与跨页批量覆盖、默认值继承、离线重试、搜索筛选与只读账号'
VIEWPORT = {'width': 1440, 'height': 900}

def cleanup_stage(report, name, action):
    """Run one cleanup stage on its own: a failure is recorded and never skips the next stage."""
    try:
        result = action()
        done = result is None or result is True
        report.setdefault('cleanup_stages', {})[name] = 'ok' if done else f'incomplete: {result!r}'
        return done
    except Exception:
        report.setdefault('cleanup_stages', {})[name] = 'error: ' + traceback.format_exc(limit=2).strip().splitlines()[-1]
        return False


def copy_redacted(artifacts, name):
    """Copy one private log into the artifacts, redacted; a missing or empty log is not a failure."""
    log = artifacts.private / name
    if log.exists():
        (artifacts.directory / name).write_text(artifacts.redact(log.read_text()))


def observe_browser(page, report, identity='admin'):
    errors = {'identity': identity, 'page': [], 'console': [], 'http': []}
    report.setdefault('browser_errors', []).append(errors)
    page.on('pageerror', lambda error: errors['page'].append(str(error)))
    page.on('console', lambda message: errors['console'].append({
        'path': urlsplit(message.location.get('url', '')).path, 'text': message.text})
        if message.type == 'error' else None)
    page.on('response', lambda response: errors['http'].append({
        'method': response.request.method, 'path': urlsplit(response.url).path, 'status': response.status})
        if response.status >= 400 else None)
    return errors


def check_browser(errors):
    check(not any(errors[key] for key in ('page', 'console', 'http')), f'unexpected browser errors: {errors!r}')


def capture(page, artifacts, name):
    """Failure screenshot that can itself fail without replacing the original failure."""
    if page is None:
        return
    try:
        page.screenshot(path=artifacts.directory / name, full_page=True)
    except Exception:
        pass


def seed(server, name):
    """Preexisting resources/facts only; tested operations always go through UI."""
    databases = list((server.work / 'server-data').glob('*.db'))
    for path in databases:
        with sqlite3.connect(path) as db:
            if db.execute("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='storages'").fetchone()[0]:
                break
    else:
        raise RuntimeError('isolated main database not found')
    now = datetime.now(timezone.utc).isoformat()
    with sqlite3.connect(path) as db:
        db.execute("INSERT INTO users(username,password_hash,role,created_at,updated_at) SELECT 'cache-viewer',password_hash,'user',?,? FROM users WHERE username='admin'", (now, now))
        ids = []
        for index in range(1, 26):
            cursor = db.execute("INSERT INTO nodes(uuid,name,hostname,state,labels,created_at,updated_at) VALUES(?,?,?,'offline',?,?,?)",
                                (f'cache-node-{index:02}', f'Cache node {index:02}', f'cache-host-{index:02}', json.dumps({'rack': 'a' if index <= 20 else 'b'}), now, now))
            ids.append(cursor.lastrowid)
        # Paths sit below the isolated server's only storage root, as a real definition must.
        cursor = db.execute("INSERT INTO storages(name,type,enabled,is_default,all_nodes,path,export,cache_path,cache_max_bytes,created_at,updated_at) VALUES(?,'nfs_cache',1,0,1,?,'fixture.invalid:/models',?,?,?,?)",
                            (name, str(server.storage_root / name), str(server.storage_root / 'cache'), 128 * GIB, now, now))
        storage_id = cursor.lastrowid
        for node_id, limit in ((ids[0], 0), (ids[1], 64 * GIB)):
            db.execute("INSERT INTO storage_nodes(storage_id,node_id,override_only,cache_max_bytes,cache_path,created_at) VALUES(?,?,1,?,'',?)", (storage_id, node_id, limit, now))
    return storage_id


def login(page, server, artifacts, locale, username='admin'):
    t = locale_strings(locale, 'auth')
    page.goto(server.base + '/dashboard')
    page.get_by_role('textbox', name=t['username'], exact=True).fill(username)
    page.get_by_label(t['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login' and r.request.method == 'POST') as result:
        button(page, t['submit']).click()
    check(result.value.status == 200, 'browser login failed')
    for cookie in page.context.cookies():
        artifacts.remember(cookie['value'])
    check(read(page, server.base, '/api/v1/auth/me')['username'] == username, 'identity mismatch')



def test_storage_cache_policy(page, server, artifacts, report, storage_id, storage_name, locale):
    t = locale_strings(locale, 'storage')
    cm, cp, draft, form = (t[key] for key in ('cacheManage', 'cachePolicy', 'cacheDraft', 'form'))
    path = f'/api/v1/storages/{storage_id}'
    evidence = report['evidence']
    def saved():
        value = read(page, server.base, path + '?all_nodes=1')
        evidence.append({'layer': 'persistence', 'response': value})
        return value
    def targets():
        return {x['node_uuid']: x for x in saved()['targets'] if x.get('node_uuid')}
    def policy_snapshot():
        body = read(page, server.base, path + '?all_nodes=1')
        by_node = {x.get('node_uuid'): (x.get('cache_max_bytes'), x.get('cache_path'))
                   for x in body.get('targets') or [] if x.get('node_uuid')}
        return (body.get('cache_max_bytes'), body.get('cache_path'),
                by_node.get('cache-node-01'), by_node.get('cache-node-02'))
    def nodes():
        page.goto(server.base + f'/storage/{storage_id}')
        page.get_by_role('tab', name=re.compile(r'^' + re.escape(t['detail']['tab']['nodes']) + r'\s*25$')).click()
        page.get_by_role('columnheader', name=t['nodes']['column']['node'], exact=True).click()
        expect(page.get_by_role('link', name='Cache node 01', exact=True)).to_be_visible()
    def row(name):
        return page.get_by_role('row').filter(has=page.get_by_role('link', name=name, exact=True))
    def policy_details(name):
        record = row(name)
        toggle = button(record, t['detail']['expand'])
        if toggle.count():
            toggle.click()
        expect(button(record, t['detail']['collapse'])).to_be_visible()
        detail = record.locator('xpath=following-sibling::tr[1]')
        expect(detail).to_have_count(1)
        return detail
    node_links = page.get_by_role('link', name=re.compile(r'^Cache node \d\d$'))
    def effective(limit):
        return cm['effectiveLimit'].replace('{{value}}', limit)
    unlimited_text = t['overview']['cacheUnlimited']
    def radio(dialog, name):
        dialog.get_by_role('radio', name=name, exact=True).check()
    def shot(name):
        page.screenshot(path=artifacts.directory / (name + '.png'), full_page=True)
    # Mainline: existing offline NFS definition -> real node overrides -> refresh -> defaults -> cross-page batch -> inheritance.
    nodes()
    expect(policy_details('Cache node 01')).to_contain_text(cm['mode']['unlimited'])
    expect(policy_details('Cache node 02')).to_contain_text('64.0 GiB')
    expect(policy_details('Cache node 03')).to_contain_text(cm['mode']['inherit'])
    # An inheriting node's effective limit is the storage default (128 GiB seeded).
    expect(policy_details('Cache node 03')).to_contain_text(effective('128.0 GiB'))
    retry_button = button(row('Cache node 01'), cm['retry'])
    retry_button.scroll_into_view_if_needed()
    expect(retry_button).to_be_in_viewport()
    check(retry_button.evaluate('el => el.scrollWidth <= el.clientWidth'), 'retry label clipped inside button')
    shot('01-node-management')
    report['last_step'] = 'table-loaded'
    expect(page.locator('.storage-cache-management .ant-btn-block')).to_have_count(5, timeout=12000)
    report['cache_metric_blocks'] = True

    # 11.5/11.7: the batch buttons act on the selection, so selection is driven
    # before any single-node edit. Both stay disabled until something is selected.
    batch = button(page, cm['batch'])
    restore = button(page, cm['restore'])
    expect(batch).to_be_disabled()
    expect(restore).to_be_disabled()
    sel = {}

    def box(node):
        return page.get_by_role('checkbox', name=cm['selectNode'].replace('{{node}}', node), exact=True)

    box('Cache node 01').check()
    expect(batch).to_be_enabled()
    expect(restore).to_be_enabled()
    sel['single_enables_batch'] = True

    box('Cache node 02').check()
    sel['multi_rows'] = page.get_by_role('checkbox', checked=True).count()

    box('Cache node 01').uncheck()
    expect(batch).to_be_enabled()
    sel['deselect_keeps_rest'] = True

    # Select-all is scoped to what the table is showing, not to every node: the
    # header control takes the visible rows, and select-filtered takes the filtered
    # set. Both are driven here with a search that narrows the list, so a control
    # that ignored the filter would select too many and fail.
    sel['rows_in_table'] = page.locator('[data-slot="table-body"] [data-proximity-index]').count()
    sel['pages'] = int(re.search(r"of\s+(\d+)",
        page.get_by_text(re.compile(r"^Page\s+\d+\s+of\s+\d+$")).first.inner_text()).group(1))
    page.locator('[data-slot="table-header"]').get_by_role('checkbox').first.check()
    page.wait_for_timeout(300)
    sel['select_all_count'] = page.locator('[data-slot="table-body"]').get_by_role('checkbox', checked=True).count()
    # The header control is page-scoped: with 22 nodes paged at 20 it takes the
    # page, not the set, so the count has to stay below the table total.
    check(sel['select_all_count'] < sel['rows_in_table'],
          f"select-all took {sel['select_all_count']} of {sel['rows_in_table']} - it is not page-scoped")

    page.locator('[data-slot="table-header"]').get_by_role('checkbox').first.uncheck()
    page.wait_for_timeout(250)
    search_box = page.locator('.storage-cache-management').get_by_placeholder(cm['search'])
    search_box.fill('Cache node 0')
    page.wait_for_timeout(400)
    sel['filtered_rows'] = page.locator('[data-slot="table-body"] [data-proximity-index]').count()
    check(sel['filtered_rows'] < sel['rows_in_table'],
          f"the search did not narrow the table ({sel['filtered_rows']} of {sel['rows_in_table']})")
    page.locator('[data-slot="table-header"]').get_by_role('checkbox').first.check()
    sel['filtered_select_all'] = page.locator('[data-slot="table-body"]').get_by_role('checkbox', checked=True).count()
    # Under a filter the header control still takes at most one page; what matters
    # is that it never reaches the rows the filter excluded, and that the filtered
    # count is what the select-filtered action advertises.
    check(sel['filtered_select_all'] <= sel['filtered_rows'],
          f"select-all under a filter ticked {sel['filtered_select_all']} of {sel['filtered_rows']} filtered rows")
    check(sel['filtered_select_all'] < sel['rows_in_table'],
          'select-all under a filter reached rows the filter excluded')
    sel['filter_limits_select_all'] = True
    # The dedicated control names the filtered total, so the operator can tell the
    # two scopes apart before acting.
    alert = page.locator('.storage-cache-management').get_by_role('alert')
    expect(alert).to_be_visible()
    sel['selection_alert_text'] = alert.inner_text()
    sel['select_filtered_present'] = cm['selectFiltered'].split('{{')[0].strip() in alert.inner_text()

    search_box.fill('')
    page.wait_for_timeout(400)

    # Clearing disables the batch actions again. The header control only reaches the
    # current page, so the dedicated clear action is what empties the whole set.
    button(page, cm['deselect']).click()
    page.wait_for_timeout(300)
    expect(batch).to_be_disabled()
    sel['clear_selection_disables_batch'] = True
    sel['checked_after_clear'] = page.get_by_role('checkbox', checked=True).count()
    report['row_selection'] = sel
    evidence.append({'layer': 'ui', 'row_selection': sel})

    # 10.5/10.6/10.7: the node table pages at 20 of 22, so page-size switching and
    # first/prev/next/last navigation are all exercised against a real second page.
    pages = {}
    report['pagination'] = pages  # bind early so diagnostics survive an assertion
    # The node table is a ResourceTable whose footer is DataTablePagination
    # (components/ui/data-table.tsx:657+): rows carry data-row-key, the size changer is a
    # Select named by labels.rowsPerPage, the position is a "Page X of N" summary, and the
    # navigation controls are named buttons.
    tbody = page.locator('.storage-cache-management [data-slot="table-body"] > tr[data-row-key]')
    pages['default_rows'] = tbody.count()
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    labels = common["resourceTable"]
    scope = page.locator('.storage-cache-management')
    size_select = scope.get_by_role("combobox", name=labels["rowsPerPage"], exact=True)
    expect(size_select.first).to_be_visible()
    report['size_changer_count'] = size_select.count()

    def summary():
        text = scope.get_by_text(re.compile(r"^Page\s+\d+\s+of\s+\d+$")).first.inner_text().strip()
        matched = re.match(r"^Page\s+(\d+)\s+of\s+(\d+)$", text)
        return (int(matched.group(1)), int(matched.group(2))) if matched else (None, None)

    def go(name):
        scope.get_by_role("button", name=labels[name], exact=True).click()
        page.wait_for_timeout(400)

    size_select.first.click(force=True)
    # The options are the bare numbers of PAGE_SIZE_OPTIONS, so the first is matched exactly.
    options = page.get_by_role("option")
    pages['size_option_labels'] = options.all_inner_texts()
    page.get_by_role("option", name=str(PAGE_SIZE_OPTIONS[0]), exact=True).click(force=True)
    # The click closes the popover and the table re-renders; retry the measurement rather
    # than reading the row count once and calling a slow commit a failure.
    for _ in range(20):
        page.wait_for_timeout(100)
        if tbody.count() == PAGE_SIZE_OPTIONS[0]:
            break
    pages['rows_at_10'] = tbody.count()
    check(pages['rows_at_10'] == PAGE_SIZE_OPTIONS[0],
          f"page size {PAGE_SIZE_OPTIONS[0]} shows {pages['rows_at_10']} rows")
    current, page_count = summary()
    pages['pagination_total_text'] = scope.get_by_text(
        re.compile(r"^\d+ (items?|条)$")).first.inner_text()
    total = int(''.join(ch for ch in pages['pagination_total_text'] if ch.isdigit()) or 0)
    pages['total_nodes'] = total
    check(page_count == 3, f"the table reports {page_count} pages, not 3")

    # With 10 per page there are 3 pages, so every navigation control is live.
    go("lastPage")
    pages['last_page_rows'] = tbody.count()
    pages['last_page_pages'] = summary()[1]
    check(pages['last_page_rows'] == total - 2 * PAGE_SIZE_OPTIONS[0],
          f"the last page shows {pages['last_page_rows']} rows, "
          f"expected {total - 2 * PAGE_SIZE_OPTIONS[0]}")

    go("previousPage")
    pages['prev_page_rows'] = tbody.count()
    go("nextPage")
    pages['next_page_rows'] = tbody.count()
    check(pages['next_page_rows'] == total - 2 * PAGE_SIZE_OPTIONS[0],
          'next did not return to the last page')
    check(summary()[0] == 3, f"the last page is not reported as current: {summary()}")
    pages['back_to_last'] = True

    # Back to the first page through the named control, then the size returns to the
    # default so the rest of the case sees the table as it started.
    go("firstPage")
    pages['first_page_rows'] = tbody.count()
    size_select.first.click(force=True)
    page.get_by_role("option", name=str(PAGE_SIZE_OPTIONS[1]), exact=True).click(force=True)
    page.wait_for_timeout(400)
    pages['restored_rows'] = tbody.count()
    check(pages['restored_rows'] == pages['default_rows'],
          f"restoring the page size left {pages['restored_rows']} rows, expected {pages['default_rows']}")
    report['pagination'] = pages
    evidence.append({'layer': 'ui', 'pagination': pages})

    # 10.9: each row in the viewport opens its own detail, addressed by the row's own
    # node name rather than by position, and the detail that opens is a definition
    # list belonging to that row. policy_details already encodes how this table opens
    # and closes a row, so the sweep reuses it instead of a second spelling of it.
    view = {}
    report['viewport_rows'] = view
    visible_rows = scope.locator('[data-slot="table-body"] > tr[data-row-key]')
    names = [visible_rows.nth(i).locator('a').first.inner_text().strip()
             for i in range(visible_rows.count())]
    view['row_names'] = names
    check(len(names) >= 10, f"the viewport only rendered {len(names)} rows")
    check(len(names) == len(set(names)), f"row names are not unique: {names}")
    opened = 0
    for name in names:
        detail = policy_details(name)
        terms = [item.strip() for item in detail.locator('dt').all_inner_texts() if item.strip()]
        check(len(terms) >= 3, f"the detail for {name} rendered {len(terms)} labelled fields")
        record = row(name)
        button(record, t['detail']['collapse']).click()
        expect(button(record, t['detail']['expand'])).to_be_visible()
        opened += 1
    view['rows_opened'] = opened
    check(opened == len(names), f"opened {opened} of {len(names)} row details")

    button(row('Cache node 01'), cm['configure']).click()
    dialog = page.get_by_role('dialog', name=cm['nodeTitle'].replace('{{node}}', 'Cache node 01'), exact=True)
    report['node_inventory'] = exercise_node_policy_inventory(dialog, cm, cp)
    evidence.append({'layer': 'ui', 'node_inventory': report['node_inventory']})

    def reopen_node():
        trigger = button(row('Cache node 01'), cm['configure'])
        trigger.focus()
        trigger.click()
        opened = page.get_by_role('dialog', name=cm['nodeTitle'].replace('{{node}}', 'Cache node 01'), exact=True)
        expect(opened).to_be_visible()
        return opened

    dialog = exercise_editor_dismissals(
        page, dialog, reopen_node, '.ant-drawer-close', t['actions']['cancel'],
        policy_snapshot, evidence, 'node')
    report['node_editor_dismissals'] = True
    report['node_editor_keyboard'] = exercise_node_editor_keyboard(
        page, dialog, cm, cp, t['actions']['cancel'], policy_snapshot, evidence)
    report['path_required'] = exercise_node_path_required(
        page, dialog, cm, cp, saved)
    evidence.append({'layer': 'ui', 'path_required': report['path_required']})
    expect(dialog.get_by_role('radio', name=cp['unlimited'], exact=True)).to_be_checked()
    # Native radio arrow behavior and keyboard focus, not DOM mutation.
    dialog.get_by_role('radio', name=cp['unlimited'], exact=True).focus()
    page.keyboard.press('ArrowLeft')
    expect(dialog.get_by_role('radio', name=cp['custom'], exact=True)).to_be_checked()
    limit_input = dialog.get_by_role('spinbutton', name=cm['limit'], exact=True)
    report['limit_defaults'] = exercise_cache_limit_defaults(
        page, dialog, limit_input, cp['unit'],
        lambda: targets()['cache-node-01']['cache_max_bytes'])
    report['limit_bounds'] = exercise_cache_limit_bounds(
        page, dialog, limit_input, cm['save'], cm['invalidLimit'],
        lambda: targets()['cache-node-01']['cache_max_bytes'])
    report['last_step'] = 'cache-capacity-unit-precision'
    report['limit_steps'] = exercise_cache_limit_steps(
        page, dialog, limit_input, cp['unit'],
        lambda: targets()['cache-node-01']['cache_max_bytes'],
        precision=report.get('check_family', 'all') != 'wire')
    report['last_step'] = 'cache-limit-server-range'
    report['limit_server_range'] = exercise_server_limit_range(
        page, server.base, path, targets, evidence)
    report['last_step'] = 'cache-json-wire-types'
    limit_input.fill('32')
    expect(dialog.get_by_text(cm['unknownImpact'].replace('{{count}}', '1'), exact=True)).to_be_visible()
    mutate(page, button(dialog, cm['save']).click, 'PATCH', path + '/node-overrides', {'node_uuids': ['cache-node-01'], 'cache_max_bytes': 32 * GIB}, 200, evidence)
    expect(dialog).not_to_be_visible()
    check(targets()['cache-node-01']['cache_max_bytes'] == 32 * GIB, 'custom limit not persisted')
    nodes()
    expect(policy_details('Cache node 01')).to_contain_text('32.0 GiB')
    button(row('Cache node 01'), cm['configure']).click()
    expect(dialog.get_by_role('spinbutton', name=cm['limit'], exact=True)).to_have_value('32')
    radio(dialog, cp['unlimited'])
    mutate(page, button(dialog, cm['save']).click, 'PATCH', path + '/node-overrides', {'node_uuids': ['cache-node-01'], 'cache_max_bytes': 0}, 200, evidence)
    expect(dialog).not_to_be_visible()
    check(targets()['cache-node-01']['cache_max_bytes'] == 0, 'explicit zero lost')
    nodes()
    expect(policy_details('Cache node 01')).to_contain_text(cm['mode']['unlimited'])
    expect(policy_details('Cache node 01')).to_contain_text(effective(unlimited_text))
    report['last_step'] = 'single-node-overrides'
    # Cross-page selection uses real pagination and retains exact identities.
    checkbox = page.get_by_role('checkbox', name=cm['selectNode'].replace('{{node}}', 'Cache node 01'), exact=True)
    checkbox.focus()
    page.keyboard.press('Space')
    expect(checkbox).to_be_checked()
    page.locator('.storage-cache-management').get_by_title('2', exact=True).click()
    expect(page.get_by_role('link', name='Cache node 25', exact=True)).to_be_visible()
    page.get_by_role('checkbox', name=cm['selectNode'].replace('{{node}}', 'Cache node 25'), exact=True).check()
    batch = button(page, cm['batch'])
    batch.focus()
    page.keyboard.press('Tab')
    page.keyboard.press('Shift+Tab')
    expect(batch).to_be_focused()
    page.keyboard.press('Enter')
    bulk = page.get_by_role('dialog', name=cm['batchTitle'].replace('{{count}}', '2'), exact=True)
    expect(bulk).to_be_visible()

    def reopen_batch():
        button(page, cm['batch']).click()
        opened = page.get_by_role('dialog', name=cm['batchTitle'].replace('{{count}}', '2'), exact=True)
        expect(opened).to_be_visible()
        return opened

    bulk = exercise_editor_dismissals(
        page, bulk, reopen_batch, '.ant-modal-close', t['actions']['cancel'],
        policy_snapshot, evidence, 'batch')
    report['batch_editor_dismissals'] = True
    bulk.get_by_role('checkbox', name=cm['changeLimit'], exact=True).check()
    radio(bulk, cp['custom'])
    bulk.get_by_role('spinbutton', name=cm['limit'], exact=True).fill('16')
    shot('02-cross-page-batch')
    mutate(page, button(bulk, cm['save']).click, 'PATCH', path + '/node-overrides', {'node_uuids': ['cache-node-01', 'cache-node-25'], 'cache_max_bytes': 16 * GIB}, 200, evidence)
    expect(bulk).not_to_be_visible()
    result = targets()
    check(result['cache-node-01']['cache_max_bytes'] == result['cache-node-25']['cache_max_bytes'] == 16 * GIB, 'cross-page selection lost')
    check(result['cache-node-02']['cache_max_bytes'] == 64 * GIB, 'unselected node changed')
    button(page, cm['restore']).click()
    expect(bulk.get_by_role('radio', name=cp['inherit'], exact=True)).to_be_checked()
    mutate(page, button(bulk, cm['save']).click, 'PATCH', path + '/node-overrides', {'node_uuids': ['cache-node-01', 'cache-node-25'], 'cache_max_bytes': None}, 200, evidence)
    expect(bulk).not_to_be_visible()
    result = targets()
    check(all(result.get(key, {}).get('cache_max_bytes') is None for key in ('cache-node-01', 'cache-node-25')), 'inheritance not restored')
    button(page, cm['deselect']).click()
    expect(button(page, cm['batch'])).to_be_disabled()
    nodes()
    expect(policy_details('Cache node 01')).to_contain_text(cm['mode']['inherit'])
    expect(policy_details('Cache node 01')).to_contain_text(effective('128.0 GiB'))
    report['last_step'] = 'batch-and-restore'
    # Defaults change and independent node exception survive full edit+refresh.
    trigger = button(page, cm['defaults'])
    expect(trigger).to_be_visible()
    parent = page.get_by_role('tab', name=re.compile(r'^' + re.escape(t['detail']['tab']['nodes']) + r'\s*25$'))
    before_defaults = policy_snapshot()
    default_dialog = exercise_defaults_dialog(
        page, trigger, cm['defaults'], parent, cm['pathValue'], cm['save'], t['actions']['cancel'], evidence)
    check(policy_snapshot() == before_defaults, 'defaults dismissal changed the saved cache policy')
    report['defaults_editor_dismissals'] = True
    radio(default_dialog, cp['unlimited'])
    before = saved()
    report['last_step'] = 'defaults-concurrent-conflict'
    exercise_defaults_conflict(
        page, server.base, default_dialog, cm, cp, str(server.storage_root / 'cache'), path, before['updated_at'],
        locale_strings(locale, 'common')['apiError']['STORAGE_CONFIG_CHANGED'], report['browser_errors'][-1], evidence, locale)
    # Recover through the page's existing reopen action, rather than silently
    # replacing the frozen revision of an editor that still holds a draft.
    after_conflict = saved()
    check(policy_snapshot() == before_defaults, 'conflicting save partially changed the policy')
    report['last_step'] = 'defaults-conflict-recovery'
    nodes()
    trigger.click()
    default_dialog = page.get_by_role('dialog', name=cm['defaults'], exact=True)
    expect(default_dialog).to_be_visible()
    expect(default_dialog.get_by_role('radio', name=cp['custom'], exact=True)).to_be_checked()
    expect(default_dialog.get_by_role('spinbutton')).to_have_value('128')
    expect(default_dialog.get_by_label(cm['pathValue'], exact=True)).to_have_value(str(server.storage_root / 'cache'))
    expect(default_dialog.get_by_role('alert').filter(has_text=
           locale_strings(locale, 'common')['apiError']['STORAGE_CONFIG_CHANGED'])).to_have_count(0)
    radio(default_dialog, cp['unlimited'])
    before = saved()
    check(before['updated_at'] == after_conflict['updated_at'], 'reopening changed the revision')
    mutate(page, button(default_dialog, cm['save']).click, 'PATCH', path, {'cache_max_bytes': 0, 'expected_updated_at': before['updated_at']}, 200, evidence)
    expect(default_dialog).not_to_be_visible()
    recovered = saved()
    check(recovered['updated_at'] != after_conflict['updated_at'], 'retry did not advance the revision')
    for key in ('id', 'name', 'type', 'enabled', 'is_default', 'all_nodes', 'path', 'export', 'cache_path', 'targets'):
        check(recovered.get(key) == after_conflict.get(key), f'recovered save changed unrelated {key}')
    # The DTO omits a zero default (omitempty), so absence and 0 both mean unlimited.
    check(saved().get('cache_max_bytes', 0) == 0 and targets()['cache-node-02']['cache_max_bytes'] == 64 * GIB, 'default zero changed custom node')
    nodes()
    expect(policy_details('Cache node 03')).to_contain_text(effective(unlimited_text))
    expect(policy_details('Cache node 02')).to_contain_text(effective('64.0 GiB'))
    report['defaults_conflict_recovery'] = {
        'status': 409, 'code': 'STORAGE_CONFIG_CHANGED', 'draft_retained': True,
        'failed_write_unchanged': True, 'reopened_limit_gib': 128,
        'reopened_error_cleared': True, 'retry_status': 200,
        'retry_revision': before['updated_at'], 'saved_revision': recovered['updated_at'],
        'saved_bytes': recovered.get('cache_max_bytes', 0),
        'unrelated_fields_unchanged': True, 'refreshed_inherit_unlimited': True,
        'refreshed_exception_gib': 64,
    }
    evidence.append({'layer': 'ui', 'defaults_conflict_recovery': report['defaults_conflict_recovery']})
    button(page.get_by_role('dialog'), t['actions']['edit']).click()
    edit = page.get_by_role('dialog', name=form['editTitle'].replace('{{name}}', storage_name), exact=True)
    expect(edit.get_by_role('radio', name=cp['unlimited'], exact=True)).to_be_checked()
    edit.get_by_role('searchbox', name=t['nodes']['search'], exact=True).fill('Cache node 02')
    expect(edit.get_by_role('row').filter(has_text='Cache node 02')).to_contain_text('64.0 GiB')
    shot('03-edit-zero-and-exceptions')
    button(edit, form['cancel']).click()
    report['last_step'] = 'defaults-and-edit'
    # Real offline reconcile request: accepted intent, NOT a successful cleanup RPC.
    mutate(page, button(row('Cache node 01'), cm['retry']).click, 'POST', path + '/cache/reconcile', {'node_uuids': ['cache-node-01'], 'mode': 'trim'}, 202, evidence)
    check(next(n for n in saved()['nodes']['all'] if n['node_uuid'] == 'cache-node-01')['cache']['state'] == 'offline', 'offline node must remain awaiting connection')
    report['last_step'] = 'offline-retry'
    # Search and policy filter compose against 25 distinguishable records whose
    # expected sets follow from the seed: node 02 is the only custom node, every
    # other node inherits, and the table shows 20 rows per page.
    def shown(indices):
        expect(node_links).to_have_text([f'Cache node {index:02}' for index in indices])
    search = page.get_by_role('searchbox', name=t['nodes']['search'], exact=True)
    shown(range(1, 21))
    search.fill('Cache node 02')
    shown([2])
    search.fill('cache NODE 2')  # case-insensitive partial name match: nodes 20..25
    shown(range(20, 26))
    search.fill('no-such-cache-node')
    shown([])
    expect(page.get_by_text(t['nodes']['empty'], exact=True)).to_be_visible()
    search.fill('')
    shown(range(1, 21))
    picker = page.get_by_role('combobox', name=cm['filterMode'], exact=True)
    picker.focus()
    page.keyboard.press('ArrowDown')
    expect(picker).to_have_attribute('aria-expanded', 'true')
    # AntD renders role=option only for a small window of the list (a hidden a11y
    # mirror), so the visible items are read by their title, which carries the label.
    labels = [cm['mode'][m] for m in ('inherit', 'custom', 'unlimited')]
    # Options are looked up inside the popup the combobox controls (aria-controls names the
    # a11y list; its parent also holds the rendered items), never by class name.
    popup = page.locator(f"#{picker.get_attribute('aria-controls')}").locator('xpath=..')
    def offered(*names):
        expect(popup.locator('[title]')).to_have_count(len(names))
        for label in labels:
            expect(popup.get_by_title(label, exact=True)).to_have_count(1 if label in names else 0)
    offered(*labels)
    first_word = cm['mode']['inherit'].split()[0] if ' ' in cm['mode']['inherit'] else cm['mode']['inherit'][:1]
    page.keyboard.type(first_word)  # partial term
    offered(*[label for label in labels if first_word.lower() in label.lower()])
    for _ in first_word:
        page.keyboard.press('Backspace')
    page.keyboard.type('zz-no-such-mode')
    offered()
    for _ in 'zz-no-such-mode':
        page.keyboard.press('Backspace')
    offered(*labels)  # clearing the term restores every option
    page.keyboard.type(cm['mode']['inherit'])
    offered(cm['mode']['inherit'])
    page.keyboard.press('Enter')
    page.keyboard.press('Escape')
    expect(picker).to_have_attribute('aria-expanded', 'false')
    expect(picker).to_be_focused()
    shown([1, *range(3, 22)])
    page.locator('.storage-cache-management').get_by_title('2', exact=True).click()
    shown(range(22, 26))  # 24 inheriting nodes in all
    page.locator('.storage-cache-management').get_by_title('1', exact=True).click()
    search.fill('Cache node 02')  # each condition alone matches; their intersection is empty
    shown([])
    search.fill('cache NODE 2')
    shown(range(20, 26))
    button(page, cm['clearFilters']).click()
    expect(search).to_have_value('')
    shown(range(1, 21))
    evidence.append({'layer': 'ui', 'checks': ['cache-limit-defaults', 'cache-limit-bounds', 'three-state-zero-refresh', 'default-inheritance-effective-limit', 'search-exact-partial-none-clear', 'mode-filter-keyboard', 'retry-action-not-clipped', 'cross-page-fixed-selection', 'keyboard-space-enter-radio-arrows', 'independent-limit-survives-default', 'edit-roundtrip', 'offline-capacity-unknown', 'offline-retry', 'search-and-mode-filter-intersection'],
                     'limit_defaults': report.get('limit_defaults'),
                     'limit_bounds': report.get('limit_bounds'),
                     'limit_steps': report.get('limit_steps')})
    shot('04-management-complete')


def viewer_read_only(browser, server, artifacts, report, storage_id, locale):
    """The seeded non-admin account reads the node table without any mutation control."""
    context = browser.new_context(viewport=VIEWPORT, locale='zh-CN' if locale == 'zh' else 'en-US', timezone_id='UTC')
    page = None
    try:
        page = context.new_page()
        page.set_default_timeout(15000)
        login(page, server, artifacts, locale, 'cache-viewer')
        errors = observe_browser(page, report, 'user')
        t = locale_strings(locale, 'storage')
        page.goto(server.base + f'/storage/{storage_id}')
        page.get_by_role('tab', name=re.compile(r'^' + re.escape(t['detail']['tab']['nodes']) + r'\s*25$')).click()
        page.get_by_role('searchbox', name=t['nodes']['search'], exact=True).fill('Cache node 25')
        expect(page.get_by_role('link', name='Cache node 25', exact=True)).to_be_visible()
        for key in ['configure', 'batch', 'restore', 'retry', 'defaults', 'clear', 'editScope']:
            expect(button(page, t['cacheManage'][key])).to_have_count(0)
        expect(page.get_by_role('checkbox')).to_have_count(0)
        page.screenshot(path=artifacts.directory / '05-viewer-read-only.png', full_page=True)
        report['evidence'].append({'layer': 'ui', 'identity': 'user', 'checks': ['readable-node-table', 'no-mutation-controls', 'no-selection']})
        # Hidden controls are not the authorization: the same session's write is refused and changes nothing.
        path = f'/api/v1/storages/{storage_id}'
        before = read(page, server.base, path + '?all_nodes=1')
        writes = [('PATCH', path + '/node-overrides', {'node_uuids': ['cache-node-02'], 'cache_max_bytes': GIB}),
                  ('PATCH', path, {'cache_max_bytes': GIB, 'expected_updated_at': before['updated_at']}),
                  ('POST', path + '/cache/reconcile', {'node_uuids': ['cache-node-02'], 'mode': 'clear'}),
                  ('DELETE', path + '/cache', None)]
        for method, target, data in writes:
            denied = page.request.fetch(server.base + target, method=method, data=data)
            body = denied.json()
            check(denied.status == 403 and body.get('code') == 'ADMINISTRATOR_REQUIRED',
                  f'non-admin {method} {target}: expected 403 ADMINISTRATOR_REQUIRED, received {denied.status} {body.get("code")}')
            report['evidence'].append({'layer': 'request', 'identity': 'user', 'method': method, 'path': target,
                                       'status': denied.status, 'code': body.get('code')})
        after = read(page, server.base, path + '?all_nodes=1')
        check({k: after.get(k) for k in ('targets', 'updated_at', 'cache_max_bytes')}
              == {k: before.get(k) for k in ('targets', 'updated_at', 'cache_max_bytes')},
              'a refused non-admin write changed the storage')
        report['evidence'].append({'layer': 'persistence', 'identity': 'user', 'unchanged_after_refusals': True})
        # UpdateNotice explicitly treats its admin-only update check's 403 as a
        # silent non-admin result; account for that exact shell request and Chrome line.
        expected_update = {'method': 'GET', 'path': '/api/v1/updates', 'status': 403}
        update_http = [item for item in errors['http'] if item == expected_update]
        update_console = [item for item in errors['console'] if item['path'] == '/api/v1/updates'
                          and item['text'] == 'Failed to load resource: the server responded with a status of 403 (Forbidden)']
        check(len(update_http) == len(update_console) == 1,
              f'expected exactly one non-admin UpdateNotice refusal: {errors!r}')
        errors['expected_refusals'] = {'http': update_http, 'console': update_console}
        errors['http'].remove(update_http[0])
        errors['console'].remove(update_console[0])
        check_browser(errors)
    except Exception:
        capture(page, artifacts, 'failure-viewer.png')
        raise
    finally:
        context.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--artifact-dir', type=Path)
    parser.add_argument('--locale', choices=['en', 'zh'], default='en')
    parser.add_argument('--check-family', choices=['all', 'precision', 'wire'], default='all')
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    manifest_path = ROOT / 'test/e2e/coverage/storage-nfs-cache-policy.json'
    manifest = json.loads(manifest_path.read_text())
    products = {name: ROOT / name.split(' (', 1)[0] for name in manifest.get('product_contracts', [])}
    bound = manifest['contract_id'] == CONTRACT and manifest['test_function'] == test_storage_cache_policy.__name__
    report = {'contract': CONTRACT, 'case': CASE, 'run_id': run_id, 'status': 'NOT_RUN',
              'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'target_branch': 'wei/master',
              'target_sha': subprocess.check_output(['git', 'rev-parse', 'wei/master'], cwd=ROOT, text=True).strip(),
              'lane': 'local-real-server-sqlite', 'identity': 'admin; role=user for the read-only phase', 'locale': args.locale,
              'timezone': 'UTC', 'viewport': VIEWPORT, 'selection': [CONTRACT], 'retries': 0, 'evidence': [],
              'check_family': args.check_family, 'probe_only': args.check_family != 'all',
              'product_sha256': {name: digest(path) for name, path in products.items()},
              'coverage_manifest_sha256': digest(manifest_path), 'test_sha256': digest(Path(__file__)),
              'helper_sha256': digest(ROOT / 'test/e2e/local_browser_fixture.py'),
              'dialog_checks_sha256': digest(ROOT / 'test/e2e/dialog_checks.py'),
              'dialogs_sha256': digest(ROOT / 'test/e2e/storage_cache_policy_dialogs.py'),
              'policy_inventory_sha256': digest(ROOT / 'test/e2e/storage_cache_policy_inventory.py'),
              'execution': 'N/A: no connected agent; no mount, eviction or capacity is claimed',
              'runtime': 'N/A: no agent, container, GPU or engine',
              'limits': ['No connected agent: no mount/eviction/runtime success claim.',
                         'Online capacity and failure-recovery RPC execution require separate integration/runtime tests.',
                         'Successful NFS creation requires a connected agent and real probe.'],
              'command': ['python3', 'test/e2e/storage_cache_policy.py', *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    if not bound:
        report['status'], report['failure'] = 'FAIL_TEST', 'coverage manifest does not bind this case'
        artifacts.write_json('report.json', report)
        print(json.dumps({'status': report['status'], 'contract': CONTRACT, 'run_id': run_id}, ensure_ascii=False))
        return 1
    try:
        report['binary_sha256'] = digest(args.server_bin)
        report['health'] = server.start()
        report['base_url'] = server.base
        report['server_environment'] = server.environment
        report['storage_name'] = storage_name = f"e2e-{run_id.lower()}-cache"
        storage_id = seed(server, storage_name)
        report['preflight'] = 'PASS'
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            report['browser_version'] = browser.version
            context = browser.new_context(viewport=VIEWPORT, locale='zh-CN' if args.locale == 'zh' else 'en-US', timezone_id='UTC')
            page = context.new_page()
            page.set_default_timeout(15000)
            try:
                login(page, server, artifacts, args.locale)
                errors = observe_browser(page, report)
                artifacts.start_trace(context)
                test_storage_cache_policy(page, server, artifacts, report, storage_id, storage_name, args.locale)
                artifacts.stop_trace(context)
                viewer_read_only(browser, server, artifacts, report, storage_id, args.locale)
                check_browser(errors)
                layers = {item['layer'] for item in report['evidence']}
                check(set(manifest['required_evidence']) <= layers, 'required evidence is missing')
                report['status'] = 'PASS'
            except Exception:
                report['status'] = 'FAIL_TEST'
                report['failure'] = artifacts.redact(traceback.format_exc())
                capture(page, artifacts, 'failure.png')
            finally:
                cleanup_stage(report, 'trace', lambda: artifacts.stop_trace(context))
                cleanup_stage(report, 'context', context.close)
                cleanup_stage(report, 'browser', browser.close)
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = artifacts.redact(traceback.format_exc())
    finally:
        report['server_stopped'] = cleanup_stage(report, 'server', server.stop)
        def frozen_sources():
            sources = {'test_sha256': Path(__file__), 'helper_sha256': ROOT / 'test/e2e/local_browser_fixture.py',
                       'dialog_checks_sha256': ROOT / 'test/e2e/dialog_checks.py',
                       'dialogs_sha256': ROOT / 'test/e2e/storage_cache_policy_dialogs.py',
                       'policy_inventory_sha256': ROOT / 'test/e2e/storage_cache_policy_inventory.py'}
            check(all(digest(path) == report[name] for name, path in sources.items()), 'storage runner/helper changed')
            check({name: digest(path) for name, path in products.items()} == report['product_sha256'],
                  'storage product source changed during execution')
            check(digest(manifest_path) == report['coverage_manifest_sha256'], 'storage manifest changed')
            if 'binary_sha256' in report:
                check(digest(args.server_bin) == report['binary_sha256'], 'storage server binary changed')
            report['source_hashes_unchanged'] = True
        cleanup_stage(report, 'frozen-sources', frozen_sources)
        cleanup_stage(report, 'server-log', lambda: copy_redacted(artifacts, 'server.log'))
        if report['status'] == 'PASS' and any(v != 'ok' for v in report['cleanup_stages'].values()):
            report['status'] = 'FAIL_TEST'
        report['remaining_resources'] = [] if report['server_stopped'] else [
            {'kind': 'process', 'pid': server.process.pid if server.process else None,
             'reclaim': f"kill {server.process.pid}" if server.process else 'none: never started'}]
        report['private_state'] = ('private/ retains the isolated DB and logs (seeded records only); '
                                   'remove only this run directory after diagnosis (7 days)')
        artifacts.write_json('report.json', report)
        try:
            artifacts.scan()
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_error'] = 'credential scan rejected artifacts; private directory must not be uploaded'
            artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'contract': CONTRACT, 'run_id': run_id,
                      'report': str(artifacts.directory / 'report.json')}, ensure_ascii=False))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
