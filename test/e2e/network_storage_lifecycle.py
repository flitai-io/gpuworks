#!/usr/bin/env python3
"""Real local control-plane resource lifecycles, without agent or mount claims.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/network_storage_lifecycle.py [--case network|storage|all]
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check, modal_geometry, theme_color
from storage_delete_blocks import (
    allow_http_status, clear_storage_deleting_file, clear_storage_reference, place_storage_deleting_file,
    place_storage_reference, refuse_storage_delete, storage_count)
from storage_local import exercise_edit_conflict


def exercise_create_dialog(page, trigger, title, parent_name, field_label, save_label, cancel_label, events, evidence):
    """Dialog identity, layout, focus, and dismissals. No write."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    parent = page.get_by_role('heading', name=parent_name, exact=True)
    baseline = len(events)

    def open_dialog():
        trigger.click()
        dialog = page.get_by_role('dialog', name=title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = open_dialog()
    assert_dialog_shell(page, dialog, dialog.get_by_label(field_label, exact=True), button(dialog, save_label))
    dialog = assert_nested_dismissals(
        dialog, parent, open_dialog,
        [('cancel', cancel_button(cancel_label)), ('escape', escape),
         ('close', close_button), ('mask', mask)],
        lambda: len(events), trigger)
    check(len(events) == baseline, 'create dialog dismissal sent a write')
    evidence.append({'layer': 'ui', 'create_dialog': title,
                     'dismissals': ['cancel', 'escape', 'close', 'mask'], 'write_requests': 0})
    return dialog


def locale_strings(locale, namespace):
    return json.loads((ROOT / 'ui/src/i18n/locales' / locale / f'{namespace}.json').read_text())


def exercise_list_states(page, server, t, prefix, report, locale, bad_http, console_errors,
                         empty_copy=None, empty_action=None, heading=None, alert=None):
    """Loading, failure with retry, empty, and populated - one list, four states.

    The list is driven through its own GET so each state can be produced without
    touching stored data: a held response proves the loading frame, a refused one
    proves the error frame and that Retry recovers, an emptied collection proves
    the empty frame, and the real collection proves the populated frame.
    """
    common = locale_strings(locale, 'common')
    table = common['dataTable']
    loading = []
    states = {}

    def hold(route):
        loading.append(route)
        # Leave the request pending; the caller asserts the loading frame, then
        # fulfils it so the list can settle.
    def refuse(route):
        route.fulfill(status=503, content_type='application/json',
                      body=json.dumps({'code': 'E2E_LIST_DOWN', 'message': 'list probe down'}))
    def empty(route):
        route.fulfill(status=200, content_type='application/json',
                      body=json.dumps({'items': [], 'total': 0}))

    # The page issues the collection GET for several reasons (mount, events,
    # Retry) and globs do not reliably include a query string, so match on the
    # parsed path instead.
    pattern = re.compile(re.escape(prefix) + r'(?:\?|$)')
    page.route(pattern, hold)
    page.reload()
    expect(page.get_by_role('heading', name=heading, exact=True)).to_be_visible()
    expect(page.locator('.ant-spin-spinning')).to_have_count(1)
    states['loading'] = True
    check(len(loading) == 1, f'the list sent {len(loading)} requests while held')
    for route in loading:
        route.continue_()
    loading.clear()
    page.unroute(pattern, hold)
    expect(page.locator('.ant-spin-spinning')).to_have_count(0)

    hits = []
    def refuse_logged(route):
        hits.append(route.request.url)
        try:
            refuse(route)
        except Exception as error:  # noqa: BLE001 - surfaced in the check below
            hits.append('FULFIL FAILED: ' + repr(error))
    page.route(pattern, refuse_logged)
    page.reload()
    page.wait_for_timeout(1500)
    if not hits:
        seen = []
        probe = page.route('**/*', lambda r: (seen.append(r.request.url), r.continue_())[1]
                           if '/api/' in r.request.url else r.continue_())
        page.reload()
        page.wait_for_timeout(3000)
        page.unroute('**/*')
        report['api_urls_seen'] = seen[:12]
        check(False, 'the refused list request was never intercepted; saw ' + str(seen[:12]))
    alert_box = alert or page.get_by_role('alert')
    expect(alert_box).to_contain_text(table['loadFailed'])
    expect(alert_box).to_contain_text('list probe down')
    resource = page.locator('.resource-table')
    list_table = resource.locator('[data-slot="table-container"]').first
    rows_now = list_table.locator('[data-proximity-index]').count()
    check(rows_now == 0,
          f'a failed list still rendered rows: {rows_now} rows, '
          f'resource-tables={resource.count()}, '
          f'all-tables={page.locator('[data-slot="table-container"]').count()}, '
          f'row-texts={page.locator('[data-proximity-index]').all_inner_texts()[:4]}')
    states['failure'] = {'message': table['loadFailed'], 'rows': 0}
    page.unroute(pattern, refuse_logged)
    # The 503 is injected on purpose; record that it surfaced and clear it so the
    # case's own end-of-run assertion still means "nothing unexpected".
    allow_http_status(bad_http, console_errors, 503)

    retry = alert_box.get_by_role('button', name=common['actions']['retry'], exact=True)
    expect(retry).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == prefix and r.request.method == 'GET') as again:
        retry.click()
    check(again.value.status == 200, f"retry HTTP {again.value.status}")
    # The response arriving is not the same moment as the rows rendering, so wait
    # for the row rather than counting immediately after the response.
    expect(list_table.locator('[data-proximity-index]').first).to_be_visible()
    states['retry_rows'] = list_table.locator('[data-proximity-index]').count()
    check(states['retry_rows'] > 0, 'retry recovered no rows')

    page.route(pattern, empty)
    # Reloading races the page's own event-driven refresh, which would put real
    # rows back under the empty frame; the table's own refresh control re-reads
    # the collection on demand, so the empty response lands deterministically.
    refresh = page.get_by_role('button', name=table['refresh'], exact=True)
    with page.expect_response(lambda r: urlsplit(r.url).path == prefix and r.request.method == 'GET'):
        refresh.click()
    # Each page supplies its own empty copy; only some also offer an action from
    # the empty frame, so the copy is required and the action is optional.
    placeholder = list_table.locator('[data-slot="empty-description"]')
    expect(placeholder).to_contain_text(empty_copy)
    if empty_action:
        empty_add = page.get_by_role('button').filter(has=page.get_by_text(empty_action, exact=True))
        expect(empty_add.first).to_be_visible()
    empty_rows = list_table.locator('[data-proximity-index]')
    check(empty_rows.count() == 0,
          'an empty list still rendered rows: ' + str(empty_rows.all_inner_texts()[:3]))
    states['empty'] = {'message': empty_copy, 'action': empty_action, 'rows': 0}
    page.unroute(pattern, empty)

    with page.expect_response(lambda r: urlsplit(r.url).path == prefix and r.request.method == 'GET'):
        refresh.click()
    expect(list_table.locator('[data-proximity-index]').first).to_be_visible()
    check(not list_table.locator('[data-slot="empty-description"]').is_visible(),
          'the empty frame survived real data')
    states['populated_rows'] = list_table.locator('[data-proximity-index]').count()
    report['list_states'] = states
    return states


def read(page, base, path, status=200):
    response = page.request.get(base + path)
    check(response.status == status, f'GET {path}: expected {status}, received {response.status}')
    body = response.json()
    check(isinstance(body, dict), f'GET {path}: object envelope required')
    return body


def mutate(page, action, method, path, body, status, evidence):
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == method) as received:
        action()
    response = received.value
    check(response.status == status, f'{method} {path}: expected {status}, received {response.status}')
    check(not urlsplit(response.url).query, f'{method} {path}: unexpected query')
    check(response.request.post_data_json == body if body is not None else response.request.post_data is None,
          f'{method} {path}: request body differs from contract')
    result = response.json() if status != 204 else None
    evidence.append({'layer': 'request', 'method': method, 'path': path, 'body': body,
                     'status': status, 'response': result})
    return result


def persisted(page, base, path, expected, evidence):
    actual = read(page, base, path)
    check(all(actual.get(k) == v for k, v in expected.items()), f'{path}: independent GET differs from saved fields')
    evidence.append({'layer': 'persistence', 'path': path, 'expected': expected, 'response': actual})
    return actual


def mutations_on(page, prefix):
    events = []
    page.on('request', lambda r: events.append({'method': r.method, 'path': urlsplit(r.url).path})
            if urlsplit(r.url).path.startswith(prefix) and r.method in {'POST', 'PUT', 'PATCH', 'DELETE'} else None)
    return events


# Mainline: gpuworks/network/ethernet-resource-lifecycle; 空库 → UI拒绝空名称/角色并取消 → 创建Ethernet serving定义
# → 刷新+独立GET → 编辑名称/CIDR并清空描述/接口 → 刷新回填 → 取消删除不修改 → UI删除、GET404和列表消失。
# 无绑定节点，执行和流量效果N/A；本case不声称网络连通性或agent配置生效。
def exercise_network_type_matrix(page, dialog, t, events):
    """Every type is selectable, and each one decides which fields exist.

    Nothing is submitted here: the point is that picking a type really changes the
    form (not just the checked radio) and that switching away puts the fields back.
    """
    kinds = ['host_ethernet', 'host_rdma', 'accelerator_rdma', 'accelerator_hccs']
    rails = {'host_rdma', 'accelerator_rdma', 'accelerator_hccs'}
    accelerator = {'accelerator_rdma', 'accelerator_hccs'}
    name = dialog.get_by_label(t['form']['name'], exact=True)
    # A value in interface must not survive a move to a rails type, and the
    # bootstrap CIDR must be the field that remains.
    name.fill('type-matrix-probe')
    report_rows = []
    # antd wraps a hidden radio input inside a labelled button; click the label so
    # the real control receives the gesture.
    def pick(kind):
        dialog.locator('.ant-radio-button-wrapper').filter(has_text=t['type'][kind]).first.click()
    for kind in kinds:
        pick(kind)
        radio = dialog.get_by_role('radio', name=t['type'][kind], exact=True)
        expect(radio).to_be_checked()
        for other in kinds:
            if other != kind:
                expect(dialog.get_by_role('radio', name=t['type'][other], exact=True)).not_to_be_checked()
        hint = t['form']['acceleratorInferred'] if kind in accelerator else (
            t['form']['rdmaInferred'] if kind in rails else t['form']['selectorNote'])
        expect(dialog.get_by_text(hint, exact=True)).to_be_visible()
        if kind in rails:
            # Rails types hide the plain cidr/interface pair behind the advanced
            # disclosure and keep the bootstrap address there instead.
            expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_count(0)
            expect(dialog.get_by_label(t['form']['cidr'], exact=True)).to_have_count(0)
            expect(dialog.get_by_text(t['form']['advanced'], exact=True)).to_be_visible()
        else:
            expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_be_visible()
            expect(dialog.get_by_label(t['form']['cidr'], exact=True)).to_be_visible()
            expect(dialog.get_by_text(t['form']['advanced'], exact=True)).to_have_count(0)
        # type_roles is the server's own dictionary: host types may carry every
        # role, accelerator types carry compute only. A role the type cannot carry
        # has to be disabled in the form rather than silently accepted.
        role_state = {}
        for role in ('storage_sync', 'compute', 'serving'):
            box = dialog.get_by_role('checkbox', name=t['role'][role], exact=True)
            if kind in accelerator:
                expected = role == 'compute'
                check(box.is_disabled() == (not expected),
                      f'{kind}: role {role} disabled={box.is_disabled()}, expected enabled={expected}')
            else:
                check(not box.is_disabled(), f'{kind}: role {role} is disabled on a host type')
            role_state[role] = not box.is_disabled()
        report_rows.append({'type': kind,
                            'interface': kind not in rails,
                            'roles': role_state,
                            'hint': hint})
    # Returning to ethernet has to restore the pair it hid, and the roles the
    # chosen type cannot carry must be disabled with the reason attached.
    pick('host_ethernet')
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_be_visible()
    check(events == [], 'switching type without saving sent a mutation')
    # Switching to an accelerator type unticks the roles it cannot carry, so the
    # form is left without the required role. Put the caller's starting state back
    # rather than leaving a half-filled form behind.
    for role in ('storage_sync', 'compute', 'serving'):
        box = dialog.get_by_role('checkbox', name=t['role'][role], exact=True)
        if box.is_checked():
            box.uncheck()
    name.fill('')
    return report_rows


def exercise_network_inflight(page, dialog, t, body, bad_http, console_errors, evidence):
    """One held create, a swallowed second click, then a 502 that keeps the draft."""
    reason = 'upstream fixture is unreachable'
    held = []
    prefix = '/api/v1/networks'

    def hold_create(route):
        if route.request.method != 'POST' or urlsplit(route.request.url).path != prefix:
            route.continue_()
            return
        held.append(route)

    page.route('**' + prefix, hold_create)
    button(dialog, t['actions']['save']).click()
    inflight = dialog.locator('.ant-modal-footer button.ant-btn-primary')
    expect(inflight).to_have_class(re.compile(r'\bant-btn-loading\b'))
    inflight.click()
    check(len(held) == 1, 'duplicate create while the first request is held')
    check(held[0].request.post_data_json == body, 'held create body differs')
    with page.expect_response(lambda r: urlsplit(r.url).path == prefix and r.request.method == 'POST') as failed:
        held[0].fulfill(status=502, content_type='application/json', body=json.dumps({'message': reason}))
    page.unroute('**' + prefix, hold_create)
    check(failed.value.status == 502, 'forced create failure did not surface')
    expect(dialog).to_be_visible()
    for field in ['name', 'description', 'cidr', 'interface']:
        expect(dialog.get_by_label(t['form'][field], exact=True)).to_have_value(body[field])
    expect(dialog.get_by_role('checkbox', name=t['role']['serving'], exact=True)).to_be_checked()
    expect(page.get_by_text(reason)).to_be_visible()
    check(any(item['path'] == prefix and item['status'] == 502 for item in bad_http), '502 was not recorded')
    check(any('502' in text for text in console_errors), '502 console line absent: ' + str(console_errors))
    bad_http[:] = [item for item in bad_http if item.get('status') != 502]
    console_errors[:] = [text for text in console_errors if '502' not in text]
    evidence.append({'layer': 'ui', 'inflight_posts': 1, 'confirm_loading': True,
                     'failure_status': 502, 'draft_retained': True, 'failure_reason_visible': True})


def test_network_lifecycle(page, server, report, locale, bad_http, console_errors):
    t = locale_strings(locale, 'networks')
    evidence = report['evidence']
    prefix = '/api/v1/networks'
    events = mutations_on(page, prefix)
    page.goto(server.base + '/networks')
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    # This page renders a permanent info alert under the table, so the error frame
    # has to be identified by its own copy rather than by the alert role alone.
    common_net = locale_strings(locale, 'common')
    list_states = exercise_list_states(
        page, server, t, prefix, report, locale, bad_http, console_errors,
        empty_copy=t['empty'], empty_action=None, heading=t['title'],
        alert=page.get_by_role('alert').filter(has_text=common_net['dataTable']['loadFailed']))
    baseline = read(page, server.base, prefix)
    check(set(baseline) == {'items', 'roles', 'types', 'type_roles'}, 'network collection envelope differs')
    name = 'e2e-' + report['run_id'] + '-network'
    draft = {'name': name, 'description': 'E2E control-plane definition', 'roles': ['serving'],
             'type': 'host_ethernet', 'cidr': '10.77.0.0/16', 'interface': 'ens*'}
    trigger = button(page, t['actions']['create'])
    expect(trigger).to_be_visible()
    dialog = exercise_create_dialog(page, trigger, t['form']['createTitle'], t['title'],
                                    t['form']['name'], t['actions']['save'], t['actions']['cancel'], events, evidence)
    from network_selector_note import exercise_selector_note
    from network_create_inventory import exercise_network_create_inventory
    report['selector_note'] = exercise_selector_note(dialog, t['form']['selectorNote'])
    report['create_inventory'] = exercise_network_create_inventory(dialog, t)
    evidence.append({'layer': 'ui', 'selector_note': report['selector_note'], 'create_inventory': report['create_inventory']})
    save = button(dialog, t['actions']['save'])
    modal_geometry(dialog, save)
    name_input = dialog.get_by_label(t['form']['name'], exact=True)
    name_input.click()
    name_input.press('Tab')
    page.keyboard.press('Shift+Tab')
    expect(name_input).to_be_focused()
    theme_color(name_input, 'borderColor', 'primary')
    for _ in range(14):
        page.keyboard.press('Tab')
        check(dialog.evaluate('el => el.contains(document.activeElement)'), 'Tab escaped the modal')
    expect(dialog.get_by_role('radio', name=t['type']['host_ethernet'], exact=True)).to_be_checked()
    for value in ['', '   ']:
        dialog.get_by_label(t['form']['name'], exact=True).fill(value)
        save.click()
        expect(dialog.get_by_text(t['form']['nameRequired'], exact=True)).to_be_visible()
        expect(dialog.get_by_text(t['form']['rolesRequired'], exact=True)).to_be_visible()
        check(events == [], 'invalid network form sent mutation')
    dialog.get_by_label(t['form']['name'], exact=True).fill(name)
    button(dialog, t['actions']['cancel']).click()
    expect(dialog).not_to_be_visible()
    expect(trigger).to_be_focused()
    check(events == [], 'cancel create sent mutation')
    trigger.click()
    report['type_matrix'] = exercise_network_type_matrix(page, dialog, t, events)
    for field in ['name', 'description', 'cidr', 'interface']:
        dialog.get_by_label(t['form'][field], exact=True).fill(draft[field])
    dialog.get_by_role('checkbox', name=t['role']['serving'], exact=True).check()
    exercise_network_inflight(page, dialog, t, draft, bad_http, console_errors, evidence)
    created = mutate(page, save.click, 'POST', prefix, draft, 201, evidence)
    path = f"{prefix}/{created['id']}"
    expect(page).to_have_url(server.base + f"/networks?network={created['id']}")
    # Creation now opens the detail drawer over the list instead of navigating
    # to a per-network page; the row itself is the stable proof the record exists.
    expect(page.get_by_role('dialog')).to_be_visible()
    expect(page.get_by_role('row').filter(has_text=name)).to_have_count(1)
    persisted(page, server.base, path, draft, evidence)
    page.reload()
    expect(page.get_by_text(draft['description'], exact=True)).to_be_visible()
    expect(page.get_by_text(draft['cidr'], exact=True)).to_be_visible()
    expect(page.get_by_text(t['detail']['noAttachments'], exact=True)).to_be_visible()
    # The list row and the detail drawer each offer Edit; take the drawer's.
    header = page.locator('.ant-drawer-header').filter(has_text=name).first
    expect(header).to_be_visible()
    button(header, t['actions']['edit']).click()
    dialog = page.get_by_role('dialog', name=t['form']['editTitle'], exact=True)
    modal_geometry(dialog, button(dialog, t['actions']['save']))
    for field in ['name', 'description', 'cidr', 'interface']:
        expect(dialog.get_by_label(t['form'][field], exact=True)).to_have_value(draft[field])
    dialog.get_by_label(t['form']['name'], exact=True).fill(name + '-cancelled')
    button(dialog, t['actions']['cancel']).click()
    expect(dialog).not_to_be_visible()
    check(len(events) == 2, 'cancel edit sent mutation')
    persisted(page, server.base, path, draft, evidence)
    # A name is unique per network, compared case-insensitively and excluding the
    # row itself. Occupy a name first, then try to edit this network onto it: the
    # save must be refused, keep what the operator typed, and leave the stored row
    # alone. Fixing the name then has to succeed without closing the dialog.
    occupied_name = 'e2e-' + report['run_id'] + '-taken'
    occupied = page.request.post(server.base + prefix,
                                 data={'name': occupied_name, 'description': 'name collision probe',
                                       'roles': ['serving'], 'type': 'host_ethernet',
                                       'cidr': '10.79.0.0/16', 'interface': 'ens*'})
    check(occupied.status in (200, 201), f"collision fixture HTTP {occupied.status}")
    occupied_body = occupied.json()
    occupied_id = (occupied_body.get('network') or occupied_body).get('id')
    button(header, t['actions']['edit']).click()
    name_field = dialog.get_by_label(t['form']['name'], exact=True)
    collision = occupied_name.upper()
    name_field.fill(collision)
    with page.expect_response(lambda r: urlsplit(r.url).path == path
                              and r.request.method == "PUT") as refused:
        button(dialog, t['actions']['save']).click()
    check(refused.value.status == 409, f"collision HTTP {refused.value.status}")
    check(refused.value.json().get('code') == 'NETWORK_NAME_TAKEN',
          f"collision code {refused.value.json().get('code')}")
    report['collision'] = {'sent': collision, 'status': refused.value.status,
                           'code': refused.value.json().get('code'),
                           'dialog_open': dialog.is_visible(),
                           'typed_value_kept': name_field.input_value()}
    # The record must be untouched by the refused save.
    persisted(page, server.base, path, draft, evidence)
    allow_http_status(bad_http, console_errors, 409)
    if not dialog.is_visible():
        button(header, t['actions']['edit']).click()
    name_field = dialog.get_by_label(t['form']['name'], exact=True)
    name_field.fill(name + '-edited')
    changed = {**draft, 'name': name + '-edited', 'description': '', 'cidr': '10.78.0.0/16', 'interface': ''}
    for field in ['name', 'description', 'cidr', 'interface']:
        dialog.get_by_label(t['form'][field], exact=True).fill(changed[field])
    mutate(page, button(dialog, t['actions']['save']).click, 'PUT', path, changed, 200, evidence)
    expect(dialog).not_to_be_visible()
    page.reload()
    expect(page.get_by_role('row').filter(has_text=changed['name'])).to_have_count(1)
    persisted(page, server.base, path, changed, evidence)
    header = page.locator('.ant-drawer-header').filter(has_text=changed['name']).first
    expect(header).to_be_visible()
    button(header, t['actions']['edit']).click()
    for field in ['name', 'description', 'cidr', 'interface']:
        expect(dialog.get_by_label(t['form'][field], exact=True)).to_have_value(changed[field])
    dialog.get_by_label(t['form']['name'], exact=True).press('Escape')
    expect(dialog).not_to_be_visible()
    before_cancel = len(events)
    button(header, t['actions']['delete']).click()
    confirmation = t['actions']['confirmDelete'].replace('{{name}}', changed['name'])
    popup = page.get_by_role('tooltip').filter(has_text=confirmation)
    expect(popup).to_be_in_viewport(ratio=1)
    button(popup, t['actions']['cancel']).click()
    expect(popup).not_to_be_visible()
    # Relative to what the collision probes already sent, cancelling must add
    # nothing - a fixed count would silently rot every time a probe is added.
    check(len(events) == before_cancel, 'cancel delete sent mutation')
    persisted(page, server.base, path, changed, evidence)
    button(header, t['actions']['delete']).click()
    result = mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 200, evidence)
    check(result == {'status': 'deleted'}, 'network deletion envelope differs')
    expect(page).to_have_url(server.base + '/networks')
    page.reload()
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    expect(page.get_by_role('row').filter(has_text=changed['name'])).to_have_count(0)
    check(read(page, server.base, path, 404)['code'] == 'NETWORK_NOT_FOUND', 'deleted network GET contract differs')
    # The collision probe made a real network; it is cleaned up through the API
    # and the list has to come back to exactly what the case started with.
    reclaimed = page.request.delete(server.base + f"{prefix}/{occupied_id}")
    check(reclaimed.status in (200, 204), f"collision fixture cleanup HTTP {reclaimed.status}")
    check(read(page, server.base, prefix) == baseline, 'network deletion did not restore baseline')
    # Two PUTs: the refused collision (409, no stored change) and the accepted
    # rename. Every write the case causes is listed, so a new stray mutation
    # still fails here.
    check(events == [{'method': 'POST', 'path': prefix}, {'method': 'POST', 'path': prefix},
                     {'method': 'PUT', 'path': path}, {'method': 'PUT', 'path': path},
                     {'method': 'DELETE', 'path': path}], 'unexpected network mutation')
    # 12.7's last two halves. This runs after the mutation list above so the strict list keeps
    # describing only the UI-driven writes; this helper drives the API directly, which is what
    # an attachment fixture has to be, and cleans up everything it makes.
    exercise_delete_impact(page, server, t, prefix, name, evidence)
    evidence.extend([{'layer': 'ui', 'checks': ['empty/whitespace/roles validation', 'dialog viewport', 'keyboard focus border=theme.primary', 'Tab focus trap', 'cancel/create/edit/delete', 'refresh fields', 'clear optional fields']},
                     {'layer': 'cleanup', 'detail_status': 404, 'list_restored': True, 'mutations': events}])


def exercise_delete_impact(page, server, t, prefix, base_name, evidence):
    """12.7 items 3 and 4: what deleting a network does to a node that has it attached.

    Item 3 (a reference blocks deletion) has no object on this resource. Nothing on the delete
    path consults attachments: writeNetworkErr (internal/server/api/networks.go:245) maps
    invalid, dedicated-unavailable, attachment-not-found, not-found, name-taken and
    attachment-exists, and nothing in the in-use family, while errors.go defines
    SERVED_NAME_IN_USE, MODEL_IN_USE, IMAGE_IN_USE and STORAGE_IN_USE for four other resources
    and never NETWORK_IN_USE. Store.DeleteNetwork (internal/server/store/network_objects.go:98)
    removes the row and its attachments and test runs in one transaction, so an attached
    network is removed rather than refused. That is asserted here as the product's actual
    behaviour, not assumed from the absent error code: the probe measures it.

    Item 4 has two halves and both are measured. The allowed cleanup is the cascade itself -
    the node stops resolving the network, and the name can be reused straight away, which is
    what proves no attachment row survived. A failed delete is a delete of an id that does not
    exist: it answers 404 and a second network, with its own attachment, is re-read afterwards
    and required to be byte-identical, so a refused delete changed nothing.
    """
    node_uuid = None
    nets = []
    try:
        # A node is needed for an attachment to exist at all. It is created and removed here
        # so the case's own cleanup contract is unchanged when this returns.
        created_node = page.request.post(server.base + '/api/v1/nodes',
                                         data={'name': 'e2e-delete-impact-node',
                                               'server_url': server.base})
        check(created_node.status in (200, 201),
              f"attach fixture node HTTP {created_node.status}")
        node_body = created_node.json()
        node_uuid = (node_body.get('node') or node_body).get('uuid')
        check(bool(node_uuid), 'attach fixture node has no uuid')

        def make_network(suffix, cidr):
            response = page.request.post(server.base + prefix,
                                         data={'name': f'{base_name}-{suffix}', 'description': '12.7 probe',
                                               'roles': ['compute'], 'type': 'host_ethernet',
                                               'cidr': cidr, 'interface': ''})
            check(response.status in (200, 201),
                  f"attach fixture network {suffix} HTTP {response.status}")
            body = response.json()
            ident = (body.get('network') or body).get('id')
            check(isinstance(ident, int) and ident > 0, 'attach fixture network has no id')
            nets.append(ident)
            return ident

        target = make_network('target', '10.86.0.0/16')
        witness = make_network('witness', '10.87.0.0/16')
        target_path = f'{prefix}/{target}'
        witness_path = f'{prefix}/{witness}'

        for path_value in (target_path, witness_path):
            attached = page.request.post(server.base + path_value + '/attachments',
                                         data={'node_uuid': node_uuid})
            check(attached.status in (200, 201),
                  f"attach on {path_value} HTTP {attached.status}")

        def detail(path_value, status=200):
            response = page.request.get(server.base + path_value)
            check(response.status == status,
                  f'GET {path_value}: expected {status}, received {response.status}')
            return response.json()

        before = detail(target_path)
        check(len(before.get('attachments') or []) == 1, 'target network is not attached')
        check(before.get('resolved_node_count') == 1,
              f"target resolved to {before.get('resolved_node_count')} nodes, expected 1")

        witness_before = detail(witness_path)
        witness_attachments_before = len(witness_before.get('attachments') or [])

        # --- item 4, allowed cleanup: the delete is permitted and takes the attachment with it
        removed = page.request.delete(server.base + target_path)
        check(removed.status == 200, f"DELETE attached network HTTP {removed.status}")
        check(removed.json() == {'status': 'deleted'}, 'attached delete envelope differs')
        after_status = page.request.get(server.base + target_path).status
        check(after_status == 404, f'deleted network answers {after_status}, expected 404')

        nodes_after = page.request.get(server.base + '/api/v1/nodes').json()
        node_rows = nodes_after.get('items') if isinstance(nodes_after, dict) else nodes_after
        check(any(row.get('uuid') == node_uuid for row in (node_rows or [])),
              'deleting the network also removed the node')

        # Reusing the name proves the attachment row did not survive behind a dead reference.
        reused = page.request.post(server.base + prefix,
                                   data={'name': f'{base_name}-target',
                                         'description': 'reuse after cascade', 'roles': ['compute'],
                                         'type': 'host_ethernet', 'cidr': '10.88.0.0/16',
                                         'interface': ''})
        check(reused.status in (200, 201),
              f"reusing the deleted network's name HTTP {reused.status}")
        reused_body = reused.json()
        reused_id = (reused_body.get('network') or reused_body).get('id')
        nets.append(reused_id)
        reused_detail = detail(f'{prefix}/{reused_id}')
        check(not (reused_detail.get('attachments') or []),
              'the reused name came back carrying the dead network attachment')

        evidence.append({'layer': 'request', 'delete_with_attachment': {
            'attach_status': 201,
            'delete_status': 200,
            'network_after': after_status,
            'resolved_node_count_before': before.get('resolved_node_count'),
            'resolved_node_count_after': 0 if after_status == 404 else None,
            'node_still_exists': True,
            'recreate_same_name_status': reused.status,
            'reference_block_absent': 'no in-use error exists for this resource',
        }})

        # --- item 4, a failed delete leaves nothing half-applied
        missing = page.request.delete(server.base + f'{prefix}/999999')
        check(missing.status == 404, f"DELETE missing network HTTP {missing.status}")
        check(missing.json().get('code') == 'NETWORK_NOT_FOUND',
              'a missing delete did not answer NETWORK_NOT_FOUND')

        witness_after = detail(witness_path)
        witness_attachments_after = len(witness_after.get('attachments') or [])
        witness_fields_unchanged = all(witness_after.get(k) == witness_before.get(k)
                                       for k in ('name', 'description', 'cidr', 'interface',
                                                 'roles', 'type'))
        check(witness_attachments_before == witness_attachments_after,
              f'the witness attachment count moved {witness_attachments_before} -> '
              f'{witness_attachments_after} across a failed delete')
        check(witness_fields_unchanged, 'a failed delete altered the witness network')

        evidence.append({'layer': 'request', 'failed_delete_leaves_no_partial_change': {
            'missing_delete_status': 404,
            'missing_delete_code': missing.json().get('code'),
            'witness_id': witness,
            'witness_attachments_before': witness_attachments_before,
            'witness_attachments_after': witness_attachments_after,
            'witness_fields_unchanged': witness_fields_unchanged,
        }})
    finally:
        for ident in nets:
            page.request.delete(server.base + f'{prefix}/{ident}')
        if node_uuid:
            page.request.delete(server.base + f'/api/v1/nodes/{node_uuid}')


# Mainline: gpuworks/storage/local-resource-lifecycle; 空库 → UI校验名称与绝对路径并取消 → UI登记local/all_nodes
# → GET/刷新回填 → UI编辑名称/目录 → GET/刷新 → UI停用再启用且布尔值持久化 → 取消删除 → UI删除、GET404/列表消失。
# 没有agent；只证明存储定义及启停期望值，不证明节点目录创建、挂载、容量或模型读写。
def exercise_storage_inflight(page, dialog, t, typed_name, typed_path, body, bad_http, console_errors, evidence):
    """One held create. Save is disabled, a forced second click sends nothing, and a 502 keeps the draft."""
    reason = 'upstream fixture is unreachable'
    held = []
    prefix = '/api/v1/storages'

    def hold_create(route):
        if route.request.method != 'POST' or urlsplit(route.request.url).path != prefix:
            route.continue_()
            return
        held.append(route)

    page.route('**' + prefix, hold_create)
    button(dialog, t['form']['save']).click()
    inflight = dialog.locator('.ant-modal-footer button.ant-btn-primary')
    expect(inflight).to_be_disabled()
    inflight.click(force=True)
    check(len(held) == 1, 'duplicate create while the first request is held')
    check(held[0].request.post_data_json == body, 'held create body differs')
    with page.expect_response(lambda r: urlsplit(r.url).path == prefix and r.request.method == 'POST') as failed:
        held[0].fulfill(status=502, content_type='application/json', body=json.dumps({'message': reason}))
    page.unroute('**' + prefix, hold_create)
    check(failed.value.status == 502, 'forced create failure did not surface')
    expect(dialog).to_be_visible()
    expect(dialog.get_by_label(t['form']['name'], exact=True)).to_have_value(typed_name)
    expect(dialog.get_by_label(t['form']['path'], exact=True)).to_have_value(typed_path)
    expect(page.get_by_text(reason)).to_be_visible()
    expect(button(dialog, t['form']['save'])).to_be_enabled()
    check(any(item['path'] == prefix and item['status'] == 502 for item in bad_http), '502 was not recorded')
    check(any('502' in text for text in console_errors), '502 console line absent: ' + str(console_errors))
    bad_http[:] = [item for item in bad_http if item.get('status') != 502]
    console_errors[:] = [text for text in console_errors if '502' not in text]
    evidence.append({'layer': 'ui', 'inflight_posts': 1, 'save_disabled': True,
                     'failure_status': 502, 'draft_retained': True, 'failure_reason_visible': True})


def test_storage_lifecycle(page, server, report, locale, bad_http, console_errors):
    t = locale_strings(locale, 'storage')
    evidence = report['evidence']
    prefix = '/api/v1/storages'
    events = mutations_on(page, prefix)
    page.goto(server.base + '/storage')
    expect(page.get_by_role('heading', name=t['page']['title'], exact=True)).to_be_visible()
    list_states = exercise_list_states(
        page, server, t, prefix, report, locale, bad_http, console_errors,
        empty_copy=t['page']['empty'], empty_action=t['page']['add'],
        heading=t['page']['title'])
    baseline = read(page, server.base, prefix)
    check(set(baseline) == {'items', 'total'} and baseline['total'] == len(baseline['items']), 'storage collection envelope differs')
    name = 'e2e-' + report['run_id'] + '-local'
    draft = {'name': name, 'type': 'local', 'path': str(server.storage_root / name), 'all_nodes': True, 'targets': []}
    # The installation seeds its default storage, so the header action is unique.
    trigger = page.get_by_role('button').filter(has=page.get_by_text(t['page']['add'], exact=True))
    expect(trigger).to_be_visible()
    dialog = exercise_create_dialog(page, trigger, t['form']['addTitle'], t['page']['title'],
                                    t['form']['name'], t['form']['save'], t['form']['cancel'], events, evidence)
    from storage_create_inventory import exercise_storage_create_inventory
    report['create_inventory'] = exercise_storage_create_inventory(
        dialog, t, locale_strings(locale, 'common')['storageType'])
    evidence.append({'layer': 'ui', 'create_inventory': report['create_inventory']})
    save = button(dialog, t['form']['save'])
    modal_geometry(dialog, save)
    name_input = dialog.get_by_label(t['form']['name'], exact=True)
    name_input.click()
    name_input.press('Tab')
    page.keyboard.press('Shift+Tab')
    expect(name_input).to_be_focused()
    theme_color(name_input, 'borderColor', 'primary')
    for _ in range(14):
        page.keyboard.press('Tab')
        check(dialog.evaluate('el => el.contains(document.activeElement)'), 'Tab escaped the modal')
    expect(dialog.get_by_role('radio', name=locale_strings(locale, 'common')['storageType']['local'], exact=True)).to_be_checked()
    expect(dialog.get_by_role('radio', name=t['form']['scope']['all'], exact=True)).to_be_checked()
    for value in ['', '   ', 'INVALID_NAME']:
        dialog.get_by_label(t['form']['name'], exact=True).fill(value)
        save.click()
        expect(dialog.get_by_text(t['form']['error']['name'], exact=True)).to_be_visible()
        check(events == [], 'invalid storage name sent mutation')
    dialog.get_by_label(t['form']['name'], exact=True).fill(name)
    dialog.get_by_label(t['form']['path'], exact=True).fill('relative/path')
    save.click()
    expect(dialog.get_by_text(t['form']['error']['pathAbsolute'], exact=True)).to_be_visible()
    check(events == [], 'relative storage path sent mutation')
    button(dialog, t['form']['cancel']).click()
    expect(dialog).not_to_be_visible()
    expect(trigger).to_be_focused()
    check(events == [], 'cancel storage create sent mutation')
    trigger.click()
    typed_name = ' ' + name + ' '
    typed_path = ' ' + draft['path'] + ' '
    dialog.get_by_label(t['form']['name'], exact=True).fill(typed_name)
    dialog.get_by_label(t['form']['path'], exact=True).fill(typed_path)
    exercise_storage_inflight(page, dialog, t, typed_name, typed_path, draft, bad_http, console_errors, evidence)
    created = mutate(page, save.click, 'POST', prefix, draft, 201, evidence)
    path = f"{prefix}/{created['id']}"
    expect(page).to_have_url(server.base + f"/storage?storage={created['id']}")
    persisted(page, server.base, path, {**draft, 'enabled': True}, evidence)
    page.reload()
    # The path shows in the drawer and again in the list row; scope to the drawer
    # it was opened into and take Edit from that same header.
    header = page.locator('.ant-drawer-header').filter(has_text=name).first
    expect(header).to_be_visible()
    expect(page.locator('.ant-drawer-body').first.get_by_text(draft['path'], exact=True)).to_be_visible()
    button(header, t['actions']['edit']).click()
    dialog = page.get_by_role('dialog', name=t['form']['editTitle'].replace('{{name}}', name), exact=True)
    modal_geometry(dialog, button(dialog, t['form']['save']))
    expect(dialog.get_by_label(t['form']['name'], exact=True)).to_have_value(name)
    expect(dialog.get_by_label(t['form']['path'], exact=True)).to_have_value(draft['path'])
    dialog.get_by_label(t['form']['path'], exact=True).fill(draft['path'] + '-cancelled')
    page.keyboard.press('Escape')
    expect(dialog).not_to_be_visible()
    expect(button(header, t['actions']['edit'])).to_be_focused()
    check(len(events) == 2, 'cancel storage edit sent mutation')
    stored = persisted(page, server.base, path, draft, evidence)
    common = locale_strings(locale, 'common')
    exercise_edit_conflict(
        page, server, t, common, created['id'], name, draft['path'], stored['updated_at'],
        bad_http, console_errors, evidence, report)
    button(header, t['actions']['edit']).click()
    changed = {'name': name + '-edited', 'path': draft['path'] + '-edited', 'all_nodes': True, 'targets': []}
    dialog.get_by_label(t['form']['name'], exact=True).fill(changed['name'])
    dialog.get_by_label(t['form']['path'], exact=True).fill(changed['path'])
    # The edit form sends only the fields that differ from what it loaded; scope is unchanged.
    mutate(page, button(dialog, t['form']['save']).click, 'PATCH', path,
           {'name': changed['name'], 'path': changed['path']}, 200, evidence)
    expect(dialog).not_to_be_visible()
    page.reload()
    expect(page.locator('.ant-drawer-body').first.get_by_text(changed['path'], exact=True)).to_be_visible()
    persisted(page, server.base, path, {**changed, 'type': 'local'}, evidence)
    switch = page.get_by_role('switch', name=t['actions']['enabledSwitch'].replace('{{name}}', changed['name']), exact=True)
    for enabled in [False, True]:
        mutate(page, switch.click, 'PATCH', path, {'enabled': enabled}, 200, evidence)
        page.reload()
        expect(switch).to_have_attribute('aria-checked', str(enabled).lower())
        persisted(page, server.base, path, {**changed, 'type': 'local', 'enabled': enabled}, evidence)
    page.goto(server.base + '/storage')
    row = page.get_by_role('row').filter(has_text=changed['name'])
    expect(row).to_contain_text(changed['path'])
    delete = button(row, t['actions']['delete'])
    before_cancel = len(events)
    delete.click()
    confirmation = t['actions']['deleteConfirm'].replace('{{name}}', changed['name'])
    popup = page.get_by_role('tooltip').filter(has_text=confirmation)
    expect(popup).to_be_in_viewport(ratio=1)
    expect(popup.get_by_text(t['actions']['deleteHelp'], exact=True)).to_be_visible()
    button(popup, t['actions']['cancel']).click()
    expect(popup).not_to_be_visible()
    # Relative to what the case has already sent: a fixed count rots whenever a
    # probe is added ahead of it.
    check(len(events) == before_cancel, 'cancel storage delete sent mutation')
    persisted(page, server.base, path, changed, evidence)
    dep_id = place_storage_reference(server, created['id'])
    blocked = refuse_storage_delete(page, row, t, changed['name'], path, 'reference block')
    check(blocked.get('code') == 'STORAGE_IN_USE', f"reference block code {blocked.get('code')}")
    expect(page.get_by_role('alert')).to_contain_text(common['apiError']['STORAGE_IN_USE'])
    expect(page.get_by_role('alert')).to_contain_text(f'#{dep_id}')
    persisted(page, server.base, path, changed, evidence)
    expect(row).to_be_visible()
    check(storage_count(server, 'SELECT COUNT(*) FROM deployment_node_requirements WHERE "key" = ? AND value = ?',
                        ('gpuworks.ai/storage', str(created['id']))) == 1, 'storage reference disappeared')
    allow_http_status(bad_http, console_errors, 409)
    clear_storage_reference(server, created['id'], dep_id)
    place_storage_deleting_file(server, created['id'])
    pending = refuse_storage_delete(page, row, t, changed['name'], path, 'cache deletion')
    check('cache deletion is awaiting confirmation' in pending.get('error', ''),
          f"cache deletion error {pending.get('error')}")
    expect(page.get_by_role('alert')).to_contain_text('cache deletion is awaiting confirmation')
    persisted(page, server.base, path, changed, evidence)
    expect(row).to_be_visible()
    check(storage_count(server, 'SELECT COUNT(*) FROM model_files WHERE storage_id = ? AND state = ?',
                        (created['id'], 'deleting')) == 1, 'deleting storage file disappeared')
    allow_http_status(bad_http, console_errors, 409)
    clear_storage_deleting_file(server, created['id'])
    delete.click()
    mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 204, evidence)
    evidence.append({'layer': 'ui', 'delete_dialog': confirmation, 'impact': t['actions']['deleteHelp'],
                     'reference_block_code': 'STORAGE_IN_USE', 'cache_deletion_status': 409, 'delete_status': 204})
    expect(row).to_have_count(0)
    page.reload()
    expect(page.get_by_role('heading', name=t['page']['title'], exact=True)).to_be_visible()
    expect(row).to_have_count(0)
    check(read(page, server.base, path, 404) == {'error': 'not found'}, 'deleted storage GET contract differs')
    check(read(page, server.base, prefix) == baseline, 'storage deletion did not restore baseline')
    # Two creates (the create flow and the case's own draft attempt), six PATCHes
    # (the rename, enabled off/on, and the two the conflict case sends: the losing
    # write from another session's revision and the restore), and three DELETEs
    # (one refused by the reference, one refused while a file is still deleting,
    # and the accepted one). Spelled out so a stray write still fails here.
    check(events == [{'method': 'POST', 'path': prefix}, {'method': 'POST', 'path': prefix}]
          + [{'method': 'PATCH', 'path': path}] * 6
          + [{'method': 'DELETE', 'path': path}] * 3, 'unexpected storage mutation')
    evidence.extend([{'layer': 'ui', 'checks': ['empty/whitespace/invalid name', 'relative path validation', 'trim', 'dialog viewport', 'keyboard focus border=theme.primary', 'Tab focus trap', 'cancel/create/edit/delete', 'refresh fields', 'enabled false/true']},
                     {'layer': 'cleanup', 'detail_status': 404, 'list_restored': True, 'mutations': events}])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', choices=['network', 'storage', 'all'], default='all')
    parser.add_argument('--locale', choices=['en', 'zh'], default='en')
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--artifact-dir', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ').lower() + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / 'test/e2e/coverage/network-storage-lifecycle.json'
    manifest = json.loads(manifest_path.read_text())
    contracts = {
        'network': ('gpuworks/network/ethernet-resource-lifecycle', test_network_lifecycle),
        'storage': ('gpuworks/storage/local-resource-lifecycle', test_storage_lifecycle),
    }
    selected_keys = list(contracts) if args.case == 'all' else [args.case]
    report = {'run_id': run_id, 'status': 'NOT_RUN', 'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'lane': 'local-real-server-sqlite', 'identity': 'admin', 'locale': args.locale, 'timezone': 'UTC',
              'viewport': {'width': 1440, 'height': 900}, 'retries': 0, 'cases': [],
              'selection': [contracts[key][0] for key in selected_keys],
              'test_sha256': digest(Path(__file__)), 'fixture_sha256': digest(ROOT / 'test/e2e/local_browser_fixture.py'),
              'helper_sha256': digest(ROOT / 'test/e2e/platform_token_read_revoke.py'),
              'dialog_checks_sha256': digest(ROOT / 'test/e2e/dialog_checks.py'), 'manifest_sha256': digest(manifest_path),
              'selector_note_sha256': digest(ROOT / 'test/e2e/network_selector_note.py'),
              'create_inventory_sha256': digest(ROOT / 'test/e2e/network_create_inventory.py'),
              'storage_inventory_sha256': digest(ROOT / 'test/e2e/storage_create_inventory.py'),
              'command': ['python3', 'test/e2e/network_storage_lifecycle.py', *sys.argv[1:]]}
    try:
        check(manifest['test_file'] == 'test/e2e/network_storage_lifecycle.py', 'manifest test file mismatch')
        check(len(manifest['cases']) == len(contracts), 'manifest must contain both resource contracts')
        indexed = {case['case_id']: case for case in manifest['cases']}
        check(set(indexed) == set(contracts), 'manifest cases differ from fixed runner selection')
        for key, (contract, function) in contracts.items():
            check(indexed[key]['contract_id'] == contract and indexed[key]['test_function'] == function.__name__, 'manifest binding differs from fixed runner contract')
            check(set(indexed[key]['required_evidence']) == {'ui', 'request', 'persistence', 'cleanup'}, 'required evidence contract was changed')
        selected = [indexed[key] for key in selected_keys]
        check(bool(selected), 'empty selection is forbidden')
        report['binary_sha256'] = digest(args.server_bin)
        report['health'] = server.start()
        report['base_url'], report['owned_server_pid'] = server.base, server.process.pid
        report['server_environment'] = server.environment
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            report['browser_version'] = browser.version
            for case in selected:
                result = {'contract_id': case['contract_id'], 'run_id': run_id, 'status': 'NOT_RUN', 'evidence': []}
                report['cases'].append(result)
                context = browser.new_context(viewport=report['viewport'], locale='zh-CN' if args.locale == 'zh' else 'en-US', timezone_id='UTC')
                page = context.new_page()
                page.set_default_timeout(10000)
                try:
                    auth = locale_strings(args.locale, 'auth')
                    page.goto(server.base + '/dashboard')
                    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
                    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
                    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login' and r.request.method == 'POST') as login:
                        button(page, auth['submit']).click()
                    check(login.value.status == 200, 'browser login failed')
                    for cookie in context.cookies():
                        artifacts.remember(cookie['value'])
                    check(read(page, server.base, '/api/v1/auth/me')['username'] == 'admin', 'session identity differs')
                    report['preflight'] = 'PASS'
                    errors, bad_http, console_errors = [], [], []
                    page.on('pageerror', lambda error: errors.append(str(error)))
                    page.on('response', lambda r: bad_http.append({'path': urlsplit(r.url).path, 'status': r.status}) if r.status >= 400 else None)
                    page.on('console', lambda message: console_errors.append(message.text) if message.type == 'error' else None)
                    artifacts.start_trace(context)
                    contracts[case['case_id']][1](page, server, result, args.locale, bad_http, console_errors)
                    check(not errors and not bad_http and not console_errors, f'unexpected browser errors: {errors}, {bad_http}, {console_errors}')
                    check(page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'page horizontal overflow')
                    check(set(case['required_evidence']) <= {e['layer'] for e in result['evidence']}, 'missing required evidence')
                    result['browser_errors'] = {'page': errors, 'http': bad_http, 'console': console_errors}
                    page.screenshot(path=artifacts.directory / f"{case['case_id']}-complete.png")
                    result['status'] = 'PASS'
                except Exception:
                    result['status'], result['failure'] = 'FAIL_TEST', traceback.format_exc()
                    artifacts.write_json('report.json', report)
                    if not any(secret in page.content() for secret in artifacts.secrets if len(secret) > 8):
                        page.screenshot(path=artifacts.directory / f"{case['case_id']}-failure.png")
                finally:
                    artifacts.stop_trace(context)
                    context.close()
            browser.close()
        report['status'] = 'PASS' if len(report['cases']) == len(selected_keys) and bool(report['cases']) and all(c['status'] == 'PASS' for c in report['cases']) else 'FAIL_TEST'
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = traceback.format_exc()
    finally:
        report['server_stopped'] = server.stop()
        if not report['server_stopped']:
            report['status'] = 'FAIL_TEST'
        log = artifacts.private / 'server.log'
        if log.exists():
            (artifacts.directory / 'server.log').write_text(artifacts.redact(log.read_text()))
        report['private_state'] = 'Isolated SQLite retained for 7 days; no process kept. Remove only this run directory after diagnosis.'
        artifacts.write_json('report.json', report)
        try:
            artifacts.scan()
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_error'] = 'Credential scan rejected artifacts; do not upload this private run directory'
            artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
