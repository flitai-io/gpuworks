"""Deployment and deleting-file rows that make a node model-file delete return 409."""
from __future__ import annotations

import json
import sqlite3
import time
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect
from model_detail_dialog import model_detail_dialog
from local_browser_fixture import ROOT
from local_business_runner import checked
from model_files_fixture import MODELS, file_facts
from platform_token_read_revoke import button


def _db(server):
    conn = sqlite3.connect(server.work / 'server-data' / 'gpuworks.db', timeout=5)
    conn.execute('PRAGMA busy_timeout=5000')
    return conn


def _count(server, sql, args):
    conn = _db(server)
    try:
        return conn.execute(sql, args).fetchone()[0]
    finally:
        conn.close()


def _node_file(server, model_id, storage_id):
    conn = _db(server)
    try:
        row = conn.execute(
            'SELECT id, node_id, state FROM model_files WHERE model_id = ? AND storage_id = ?',
            (model_id, storage_id)).fetchone()
    finally:
        conn.close()
    checked(row is not None, 'node model file row missing')
    return row


def place_file_reference(server, model_id, storage_id):
    """An active deployment on the file's node blocks eviction before any byte moves."""
    file_id, node_id, state = _node_file(server, model_id, storage_id)
    checked(state == 'present', f'node model file state {state}')
    conn = _db(server)
    try:
        cur = conn.execute(
            'INSERT INTO deployments (name, model_id, served_name, node_id, desired_state, created_at, updated_at) '
            'VALUES (?, ?, ?, ?, ?, ?, ?)',
            (f'e2e-file-ref-{file_id}', model_id, f'e2e-file-ref-{file_id}', node_id, 'active',
             '2026-10-24 00:00:00', '2026-10-24 00:00:00'))
        dep_id = cur.lastrowid
        conn.commit()
    finally:
        conn.close()
    checked(_count(server, 'SELECT COUNT(*) FROM deployments WHERE id = ? AND model_id = ? AND node_id = ? AND desired_state = ?',
                   (dep_id, model_id, node_id, 'active')) == 1, 'file reference missing after insert')
    return dep_id


def clear_file_reference(server, dep_id):
    conn = _db(server)
    try:
        cur = conn.execute('DELETE FROM deployments WHERE id = ?', (dep_id,))
        conn.commit()
        checked(cur.rowcount == 1, 'file reference deployment was not the one inserted')
    finally:
        conn.close()


def release_file_reference(server, dep_id):
    """Drop the inserted deployment even when a later assertion already failed."""
    conn = _db(server)
    try:
        conn.execute('DELETE FROM deployments WHERE id = ?', (dep_id,))
        conn.commit()
    finally:
        conn.close()


def restore_present(server, model_id, storage_id):
    conn = _db(server)
    try:
        conn.execute(
            "UPDATE model_files SET state = 'present' WHERE model_id = ? AND storage_id = ? AND state = 'deleting'",
            (model_id, storage_id))
        conn.commit()
    finally:
        conn.close()


def set_file_state(server, model_id, storage_id, before, after):
    conn = _db(server)
    try:
        cur = conn.execute(
            'UPDATE model_files SET state = ? WHERE model_id = ? AND storage_id = ? AND state = ?',
            (after, model_id, storage_id, before))
        conn.commit()
        checked(cur.rowcount == 1, f'file state {before} -> {after} updated {cur.rowcount} rows')
    finally:
        conn.close()


def refuse_node_delete(page, report, labels, data, remove, path, storage_id, label):
    title = labels['files']['deleteOnNodeConfirm'].replace('{{node}}', data['node']['name'])
    remove.click()
    popup = page.locator('.ant-popconfirm').filter(has=page.get_by_text(title, exact=True))
    expect(popup.get_by_text(labels['files']['deleteOnNodeDescription'], exact=True)).to_be_visible()
    report.setdefault('allowed_http', []).append(('DELETE', path, 409))
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == 'DELETE') as received:
        button(popup, labels['action']['delete']).click()
    response = received.value
    # Status before .json(): skipping the reference check deletes the row and returns 200.
    checked(response.status == 409, f'{label} status {response.status}')
    checked(parse_qs(urlsplit(response.url).query) == {'storage_id': [str(storage_id)]},
            f'{label} query {response.url}')
    return response.json()


def exercise_node_delete_blocks(page, report, server, token, labels, locale, data, target, remove, path, document, presence):
    common = json.loads((ROOT / 'ui/src/i18n/locales' / locale / 'common.json').read_text())
    model_id, storage_id = data['model']['id'], target['id']
    facts = file_facts(target['payload_path'], data['hub'].files)
    dep_id = None
    flipped = False
    try:
        dep_id = place_file_reference(server, model_id, storage_id)
        blocked = refuse_node_delete(page, report, labels, data, remove, path, storage_id, 'reference block')
        checked(blocked.get('code') == 'MODEL_IN_USE', f"reference block code {blocked.get('code')}")
        expect(page.locator('.ant-message')).to_contain_text(common['apiError']['MODEL_IN_USE'])
        current = document(server, token, data)
        checked(presence(current, data, target)['state'] == 'present', 'referenced file row changed')
        checked(file_facts(target['payload_path'], data['hub'].files) == facts, 'referenced file bytes changed')
        checked(_count(server, 'SELECT COUNT(*) FROM deployments WHERE id = ?', (dep_id,)) == 1, 'file reference disappeared')
        clear_file_reference(server, dep_id)
        dep_id = None
        set_file_state(server, model_id, storage_id, 'present', 'deleting')
        flipped = True
        pending = refuse_node_delete(page, report, labels, data, remove, path, storage_id, 'cache deletion')
        checked('cache deletion is awaiting confirmation' in pending.get('error', ''),
                f"cache deletion error {pending.get('error')}")
        expect(page.locator('.ant-message')).to_contain_text('cache deletion is awaiting confirmation')
        current = document(server, token, data)
        checked(presence(current, data, target)['state'] == 'deleting', 'deleting file row changed')
        checked(file_facts(target['payload_path'], data['hub'].files) == facts, 'deleting file bytes changed')
        set_file_state(server, model_id, storage_id, 'deleting', 'present')
        flipped = False
        report['evidence'].append({
            'layer': 'ui', 'delete_dialog': labels['files']['deleteOnNodeConfirm'].replace('{{node}}', data['node']['name']),
            'impact': labels['files']['deleteOnNodeDescription'], 'reference_block_code': 'MODEL_IN_USE',
            'cache_deletion_status': 409})
    finally:
        if dep_id is not None:
            release_file_reference(server, dep_id)
        if flipped:
            restore_present(server, model_id, storage_id)


def delete_present_node_copy(page, report, server, token, labels, locale, data, target, document, presence, mutate):
    """Refuse a present node copy, then delete it once both blocks are gone."""
    dialog = model_detail_dialog(page, labels, data['name'])
    files = dialog.locator('#model-files')
    target_row = files.locator('[data-slot="table-body"] [data-proximity-index]').filter(has_text=target['name'] + ' · ')
    remove = target_row.get_by_role('button', name=labels['files']['deleteOnNode'], exact=True)
    path = f"{MODELS}/{data['model']['id']}/nodes/{data['node']['uuid']}"
    exercise_node_delete_blocks(page, report, server, token, labels, locale, data, target, remove, path,
                                document, presence)
    remove.click()
    title = labels['files']['deleteOnNodeConfirm'].replace('{{node}}', data['node']['name'])
    popup = page.locator('.ant-popconfirm').filter(has=page.get_by_text(title, exact=True))
    expect(popup.get_by_text(title, exact=True)).to_be_visible()
    mutate(button(popup, labels['action']['delete']).click, 'DELETE', path, None, 200,
           {'storage_id': [str(target['id'])]})
    deadline = time.monotonic() + 30
    while Path(target['payload_path']).exists():
        checked(time.monotonic() <= deadline, 'present file directory still exists')
        page.wait_for_timeout(100)
    current = document(server, token, data)
    checked(presence(current, data, target) is None, 'present file row remains')
    page.reload()
    expect(model_detail_dialog(page, labels, data['name']).locator(
        '#model-files [data-slot="table-body"] [data-proximity-index]')).to_have_count(0)
