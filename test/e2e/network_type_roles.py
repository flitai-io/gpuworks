#!/usr/bin/env python3
"""Network type/role and conditional-field contracts on an isolated real server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/network_type_roles.py --locale en|zh [--case CASE]
Build first with make build. No agent/GPU required; no communication claim.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from network_storage_lifecycle import locale_strings, mutate, mutations_on, persisted, read
from platform_token_read_revoke import button, check

PREFIX = '/api/v1/networks'
ROLES = ['storage_sync', 'compute', 'serving']
TYPES = ['host_ethernet', 'host_rdma', 'accelerator_rdma', 'accelerator_hccs']
TYPE_ROLES = {'host_ethernet': ROLES, 'host_rdma': ROLES, 'accelerator_rdma': ['compute'], 'accelerator_hccs': ['compute']}
# Fixed contract denominator: the API dictionary must match, never shrink it dynamically.
COMBINATIONS = {
    kind + '--' + '-'.join(roles): (kind, list(roles))
    for kind in TYPES
    for size in range(1, len(TYPE_ROLES[kind]) + 1)
    for roles in itertools.combinations(TYPE_ROLES[kind], size)
}
TRANSITIONS = {f'to-{kind}-and-back': kind for kind in TYPES[1:]}
CASES = list(COMBINATIONS) + list(TRANSITIONS)


def form(page, t, editing=False):
    return page.get_by_role('dialog', name=t['form']['editTitle' if editing else 'createTitle'], exact=True)


def type_control(dialog, t, kind):
    return dialog.get_by_role('radio', name=t['type'][kind], exact=True)


def choose_type(dialog, t, kind):
    # AntD button radios visually expose their label; native input has no box.
    dialog.get_by_text(t['type'][kind], exact=True).click()
    expect(type_control(dialog, t, kind)).to_be_checked()


def role_control(dialog, t, role):
    return dialog.get_by_role('checkbox', name=t['role'][role], exact=True)


def exercise_create_dialog(page, create, t, evidence):
    """Create dialog identity, layout, focus, and dismissals. No network write."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    title = t['form']['createTitle']
    parent = page.get_by_role('heading', name=t['title'], exact=True)
    writes = []

    def on_request(request):
        if urlsplit(request.url).path.startswith(PREFIX) and request.method in {'POST', 'PUT', 'PATCH', 'DELETE'}:
            writes.append(request.method)

    def open_dialog():
        create.click()
        dialog = form(page, t)
        expect(dialog).to_be_visible()
        return dialog

    page.on('request', on_request)
    dialog = open_dialog()
    assert_dialog_shell(page, dialog, dialog.get_by_label(t['form']['name'], exact=True),
                        button(dialog, t['actions']['save']))
    dialog = assert_nested_dismissals(
        dialog, parent, open_dialog,
        [('cancel', cancel_button(t['actions']['cancel'])), ('escape', escape),
         ('close', close_button), ('mask', mask)],
        lambda: len(writes), create)
    check(not writes, 'create dialog dismissal sent a network write')
    evidence.append({'layer': 'ui', 'create_dialog': title,
                     'dismissals': ['cancel', 'escape', 'close', 'mask'], 'write_requests': 0})
    return dialog


def open_create(page, server, t, evidence):
    page.goto(server.base + '/networks')
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    create = button(page, t['actions']['create'])
    expect(create).to_be_visible()
    return exercise_create_dialog(page, create, t, evidence)


def delete_owned(page, server, t, path, name, baseline, evidence):
    button(page.get_by_role('dialog'), t['actions']['delete']).click()
    popup = page.get_by_role('tooltip').filter(has_text=t['actions']['confirmDelete'].replace('{{name}}', name))
    expect(popup).to_be_in_viewport(ratio=1)
    result = mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 200, evidence)
    check(result == {'status': 'deleted'}, 'delete envelope differs')
    expect(page).to_have_url(server.base + '/networks')
    page.reload()
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    expect(page.get_by_role('row').filter(has_text=name)).to_have_count(0)
    check(read(page, server.base, path, 404)['code'] == 'NETWORK_NOT_FOUND', 'deleted resource remains readable')
    check(read(page, server.base, PREFIX) == baseline, 'scoped deletion did not restore baseline')
    evidence.append({'layer': 'cleanup', 'path': path, 'detail_status': 404, 'list_restored': True})


def _posts(page, action):
    try:
        with page.expect_request(
                lambda request: request.method == "POST" and urlsplit(request.url).path == PREFIX,
                timeout=1000):
            action()
        return True
    except PlaywrightTimeout:
        return False


def _box(locator):
    return locator.evaluate("""el => {
      const r = el.getBoundingClientRect();
      return {left: r.left, top: r.top, right: r.right, bottom: r.bottom,
              width: r.width, height: r.height,
              pageLeft: r.left + window.scrollX, pageTop: r.top + window.scrollY};
    }""")


def _overlaps(a, b):
    return not (a["pageLeft"] + a["width"] <= b["pageLeft"]
                or b["pageLeft"] + b["width"] <= a["pageLeft"]
                or a["pageTop"] + a["height"] <= b["pageTop"]
                or b["pageTop"] + b["height"] <= a["pageTop"])


def exercise_create_keyboard(page, dialog, t):
    """Tab reaches the next field. Enter on an empty name shows the name error and does not create."""
    name = dialog.get_by_label(t["form"]["name"], exact=True)
    description = dialog.get_by_label(t["form"]["description"], exact=True)
    footer = dialog.locator(".ant-modal-footer")
    save = button(footer, t["actions"]["save"])
    cancel = button(footer, t["actions"]["cancel"])
    expect(name).to_have_value("")
    name.focus()
    expect(name).to_be_focused()
    name.press("Tab")
    expect(description).to_be_focused()
    name.focus()
    check(not _posts(page, lambda: name.press("Enter")), "Enter on an empty name sent a network create")
    expect(dialog).to_be_visible()
    expect(name).to_have_value("")
    expect(dialog.get_by_text(t["form"]["nameRequired"], exact=True)).to_be_visible()
    expect(name).to_have_attribute("aria-invalid", "true")
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    boxes = {label: _box(locator) for label, locator in (
        ("name", name), ("save", save), ("cancel", cancel))}
    for label, box in boxes.items():
        check(box["width"] > 0 and box["height"] > 0, f"{label} has no box")
        check(-1 <= box["left"] and box["right"] <= view["width"] + 1
              and -1 <= box["top"] and box["bottom"] <= view["height"] + 1,
              f"{label} is outside the viewport {box}")
    check(not _overlaps(boxes["name"], boxes["save"]), "the name overlaps Save")
    check(not _overlaps(boxes["name"], boxes["cancel"]), "the name overlaps Cancel")
    return {"tab": ["name", "description"], "enter_submitted": False,
            "empty_name_error": True, "viewport": view}


def exercise_network_required(page, dialog, t):
    """Name rejects empty and whitespace. Roles reject an empty selection. No create."""
    name = dialog.get_by_label(t["form"]["name"], exact=True)
    save = button(dialog, t["actions"]["save"])

    def save_posts():
        try:
            with page.expect_request(
                    lambda request: request.method == "POST" and urlsplit(request.url).path == PREFIX,
                    timeout=1000):
                save.click()
            return True
        except PlaywrightTimeout:
            return False

    name.fill("")
    check(not save_posts(), "empty name sent a network create")
    expect(dialog.get_by_text(t["form"]["nameRequired"], exact=True)).to_be_visible()
    expect(name).to_have_attribute("aria-invalid", "true")
    name.fill("   ")
    check(not save_posts(), "whitespace name sent a network create")
    expect(dialog.get_by_text(t["form"]["nameRequired"], exact=True)).to_be_visible()
    expect(name).to_have_attribute("aria-invalid", "true")
    name.fill("role-probe")
    for role in ROLES:
        expect(role_control(dialog, t, role)).not_to_be_checked()
    check(not save_posts(), "Save with no role sent a network create")
    expect(dialog.get_by_text(t["form"]["rolesRequired"], exact=True)).to_be_visible()
    expect(name).not_to_have_attribute("aria-invalid", "true")
    name.fill("")
    return {"empty_name": True, "whitespace_name": True, "empty_roles": True, "creates": 0}


def exercise_create_inflight(page, dialog, t, name, roles, body, bad_http, console_errors, evidence):
    """One held create, a swallowed second click, then a 502 that keeps the draft."""
    reason = "upstream fixture is unreachable"
    held = []

    def hold_create(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != PREFIX:
            route.continue_()
            return
        held.append(route)

    page.route("**" + PREFIX, hold_create)
    button(dialog, t["actions"]["save"]).click()
    inflight = dialog.locator(".ant-modal-footer button.ant-btn-primary")
    expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
    inflight.click()
    check(len(held) == 1, "duplicate create while the first request is held")
    check(held[0].request.post_data_json == body, "held create body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == PREFIX and r.request.method == "POST") as failed:
        held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
    page.unroute("**" + PREFIX, hold_create)
    check(failed.value.status == 502, "forced create failure did not surface")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_label(t["form"]["name"], exact=True)).to_have_value(name)
    for role in roles:
        expect(role_control(dialog, t, role)).to_be_checked()
    expect(page.get_by_text(reason)).to_be_visible()
    check(any(item["path"] == PREFIX and item["status"] == 502 for item in bad_http), "502 was not recorded")
    check(any("502" in text for text in console_errors), "502 console line missing: " + str(console_errors))
    bad_http.clear()
    console_errors.clear()
    evidence.append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
                     "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})


# Mainline: gpuworks/network/type-role/<type--roles>; 空库字典固定四类型/三角色 → UI逐项选择指定非空角色组合
# → 精确POST+独立GET → 刷新打开编辑确认类型/角色回填 → UI删除+GET404+列表恢复；仅定义，无网络运行时声明。
def exercise_minimum_network(page, server, t, name, network_id, result):
    """Minimum host-ethernet create: empty optionals, list row, and one small attach button."""
    resolved_title = t['detail']['resolved'].replace('{{count}}', '0')
    resolved = page.get_by_role('dialog')
    expect(resolved.get_by_role('heading', name=resolved_title, exact=True)).to_be_visible()
    table = resolved.locator('.ant-table')
    expect(table).to_have_count(1)
    expect(table).to_have_class(re.compile(r'\bant-table-small\b'))
    attach = button(page, t['actions']['attach'])
    expect(attach).to_have_count(1)
    expect(attach).to_have_class(re.compile(r'\bant-btn-sm\b'))
    cidr = resolved.get_by_role('rowheader', name=t['selector']['cidr'], exact=True).locator('..')
    interface = resolved.get_by_role('rowheader', name=t['selector']['interface'], exact=True).locator('..')
    expect(cidr).to_have_count(1)
    expect(interface).to_have_count(1)
    expect(cidr).to_contain_text('—')
    expect(interface).to_contain_text('—')
    expect(page.get_by_text(t['detail']['noAttachments'], exact=True)).to_be_visible()
    result['last_step'] = 'selector-table'
    descriptions = table
    expect(descriptions).to_have_count(1)
    expect(descriptions).to_have_class(re.compile(r'\bant-table-small\b'))
    page.goto(server.base + '/networks')
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    row = page.get_by_role('row').filter(has_text=name)
    expect(row).to_have_count(1)
    expect(row.get_by_role('link', name=name, exact=True)).to_be_visible()
    expect(row.get_by_text(t['selector']['none'], exact=True)).to_be_visible()
    edit = button(row, t['actions']['edit'])
    expect(edit).to_have_count(1)
    expect(edit).to_have_class(re.compile(r'\bant-btn-text\b'))
    edit.click()
    editing = form(page, t, True)
    expect(editing.get_by_label(t['form']['name'], exact=True)).to_have_value(name)
    expect(editing.get_by_label(t['form']['description'], exact=True)).to_have_value('')
    expect(editing.get_by_label(t['form']['cidr'], exact=True)).to_have_value('')
    expect(editing.get_by_label(t['form']['interface'], exact=True)).to_have_value('')
    expect(type_control(editing, t, 'host_ethernet')).to_be_checked()
    expect(role_control(editing, t, 'compute')).to_be_checked()
    expect(role_control(editing, t, 'storage_sync')).not_to_be_checked()
    expect(role_control(editing, t, 'serving')).not_to_be_checked()
    expect(editing.locator('input[type=password]')).to_have_count(0)
    button(editing, t['actions']['cancel']).click()
    expect(editing).not_to_be_visible()
    page.goto(server.base + '/networks?network=' + str(network_id))
    expect(page.get_by_role('heading', name=re.compile(r'^' + re.escape(name) + r'(?:\s|$)'))).to_be_visible()
    return {
        'attach_button_small': True,
        'list_name': True,
        'list_selector_empty': True,
        'detail_cidr_empty': True,
        'detail_interface_empty': True,
        'not_attached': True,
        'edit_button_text': True,
        'edit_backfill': True,
        'edit_password_fields': 0,
        'resolved_table_small': True,
        'selector_table_small': True,
    }


def exercise_full_optional(page, server, t, name, evidence):
    """Second host-ethernet create with description, CIDR, interface, and every role."""
    full_name = name + '-full'
    body = {
        'name': full_name,
        'description': 'east lane',
        'roles': list(ROLES),
        'type': 'host_ethernet',
        'cidr': '10.77.0.0/16',
        'interface': 'ens*',
    }
    page.goto(server.base + '/networks')
    expect(page.get_by_role('heading', name=t['title'], exact=True)).to_be_visible()
    button(page, t['actions']['create']).click()
    dialog = form(page, t)
    expect(dialog).to_be_visible()
    expect(type_control(dialog, t, 'host_ethernet')).to_be_checked()
    dialog.get_by_label(t['form']['name'], exact=True).fill(full_name)
    dialog.get_by_label(t['form']['description'], exact=True).fill(body['description'])
    dialog.get_by_label(t['form']['cidr'], exact=True).fill(body['cidr'])
    dialog.get_by_label(t['form']['interface'], exact=True).fill(body['interface'])
    for role in ROLES:
        role_control(dialog, t, role).click()
        expect(role_control(dialog, t, role)).to_be_checked()
    created = mutate(page, button(dialog, t['actions']['save']).click, 'POST', PREFIX, body, 201, evidence)
    path = PREFIX + '/' + str(created['id'])
    expect(page).to_have_url(server.base + '/networks?network=' + str(created['id']))
    stored = persisted(page, server.base, path, body, evidence)
    expect(page.get_by_text(body['description'], exact=True).first).to_be_visible()
    expect(page.get_by_text(body['cidr'], exact=True).first).to_be_visible()
    expect(page.get_by_text(body['interface'], exact=True).first).to_be_visible()
    button(page.get_by_role('dialog'), t['actions']['delete']).click()
    popup = page.get_by_role('tooltip').filter(has_text=t['actions']['confirmDelete'].replace('{{name}}', full_name))
    expect(popup).to_be_in_viewport(ratio=1)
    deleted = mutate(page, button(popup, t['actions']['delete']).click, 'DELETE', path, None, 200, evidence)
    check(deleted == {'status': 'deleted'}, 'full network delete envelope differs')
    check(read(page, server.base, path, 404)['code'] == 'NETWORK_NOT_FOUND', 'full network remains readable')
    return {
        'name': full_name,
        'path': path,
        'description': body['description'],
        'cidr': body['cidr'],
        'interface': body['interface'],
        'roles': stored['roles'],
        'status': 201,
    }


def exercise_role_tooltip(page, dialog, t):
    """One tooltip on a role Accelerator RDMA cannot carry, then restore ethernet."""
    reason = t['form']['roleNotOnType']
    label = t['role']['storage_sync']
    choose_type(dialog, t, 'accelerator_rdma')
    span = dialog.get_by_label(f'{label}: {reason}', exact=True)
    expect(span).to_be_visible()
    span.hover()
    tip = page.locator('.ant-tooltip')
    expect(tip).to_have_count(1, timeout=12000)
    expect(tip).to_be_visible()
    expect(tip).to_contain_text(reason)
    choose_type(dialog, t, 'host_ethernet')
    dialog.get_by_label(t['form']['name'], exact=True).focus()
    for role in ROLES:
        box = role_control(dialog, t, role)
        expect(box).to_be_enabled()
        expect(box).not_to_be_checked()
    expect(type_control(dialog, t, 'host_ethernet')).to_be_checked()
    return {'tooltips': 1, 'reason': True, 'restored_type': 'host_ethernet'}


def exercise_network_edit_conflict(page, dialog, server, t, path, body, events, posts, evidence):
    """Another session writes the description. The open edit keeps its draft and save writes that draft back."""
    check(events == [{'method': 'POST', 'path': PREFIX}] * posts, 'read/cancel unexpectedly mutated resource')
    other = {**body, 'description': 'Other session'}
    written = page.request.put(server.base + path, data=other)
    check(written.status == 200, f'the other network session was rejected: HTTP {written.status}')
    persisted(page, server.base, path, other, evidence)
    expect(dialog.get_by_label(t['form']['description'], exact=True)).to_have_value('')
    expect(dialog.get_by_label(t['form']['name'], exact=True)).to_have_value(body['name'])
    expect(type_control(dialog, t, body['type'])).to_be_checked()
    expect(role_control(dialog, t, 'storage_sync')).to_be_checked()
    expect(role_control(dialog, t, 'compute')).not_to_be_checked()
    expect(role_control(dialog, t, 'serving')).not_to_be_checked()
    check(dialog.locator('.ant-alert-error').count() == 0, 'the edit dialog shows a conflict')
    check(events == [{'method': 'POST', 'path': PREFIX}] * posts, 'the other network session was counted as a page save')
    mutate(page, button(dialog, t['actions']['save']).click, 'PUT', path, body, 200, evidence)
    expect(dialog).not_to_be_visible()
    persisted(page, server.base, path, body, evidence)
    return {
        'other_session': 200,
        'other_description': 'Other session',
        'draft_description': '',
        'saved_status': 200,
        'conflict_status': False,
    }


def test_type_role_combination(page, server, result, locale, key, baseline, bad_http=None, console_errors=None):
    t, evidence = locale_strings(locale, 'networks'), result['evidence']
    kind, roles = COMBINATIONS[key]
    result['mainline'] = f'空库 → UI选择 {kind} / {roles} 创建 → GET及刷新编辑回填 → UI删除和GET404/列表恢复'
    events = mutations_on(page, PREFIX)
    dialog = open_create(page, server, t, evidence)
    if key == 'host_ethernet--storage_sync':
        result['last_step'] = 'role-tooltip'
        result['role_tooltip'] = exercise_role_tooltip(page, dialog, t)
    from network_create_inventory import exercise_network_create_inventory
    result["create_inventory"] = exercise_network_create_inventory(dialog, t)
    evidence.append({"layer": "ui", "create_inventory": result["create_inventory"]})
    expect(type_control(dialog, t, TYPES[0])).to_be_checked()
    expect(dialog.get_by_role('radio')).to_have_count(4)
    expect(dialog.get_by_role('checkbox')).to_have_count(3)
    for role in ROLES:
        expect(role_control(dialog, t, role)).not_to_be_checked()
    result["required_fields"] = exercise_network_required(page, dialog, t)
    result["last_step"] = "network-required"
    if key == 'host_ethernet--compute':
        result["create_keyboard"] = exercise_create_keyboard(page, dialog, t)
        result["last_step"] = "create-keyboard"
    choose_type(dialog, t, kind)
    expect(type_control(dialog, t, kind)).to_be_checked()
    for role in ROLES:
        control = role_control(dialog, t, role)
        if role in TYPE_ROLES[kind]:
            expect(control).to_be_enabled()
        else:
            expect(control).to_be_disabled()
        if role in roles:
            control.click()
            expect(control).to_be_checked()
    name = 'e2e-' + result['run_id'] + '-' + str(CASES.index(key))
    dialog.get_by_label(t['form']['name'], exact=True).fill(name)
    if kind != 'host_ethernet':
        expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_count(0)
        expect(dialog.get_by_label(t['form']['bootstrapCidr'], exact=True)).not_to_be_visible()
        expect(dialog.get_by_role('button').filter(has_text=t['form']['advanced'])).to_have_attribute('aria-expanded', 'false')
    body = {'name': name, 'description': '', 'roles': roles, 'type': kind, 'cidr': '', 'interface': ''}
    if key == 'host_ethernet--compute':
        check(not _posts(page, lambda: dialog.get_by_label(t['form']['name'], exact=True).press('Enter')),
              'Enter on the minimum form sent a network create')
        expect(dialog).to_be_visible()
        expect(dialog.get_by_label(t['form']['name'], exact=True)).to_have_value(name)
        expect(role_control(dialog, t, 'compute')).to_be_checked()
        result['create_keyboard']['enter_on_minimum'] = False
    if key == 'host_ethernet--storage_sync':
        exercise_create_inflight(page, dialog, t, name, roles, body, bad_http, console_errors, evidence)
    created = mutate(page, button(dialog, t['actions']['save']).click, 'POST', PREFIX, body, 201, evidence)
    path = PREFIX + '/' + str(created['id'])
    expect(page).to_have_url(server.base + '/networks?network=' + str(created['id']))
    actual = persisted(page, server.base, path, body, evidence)
    check(actual['type_roles'] == TYPE_ROLES, 'detail type_roles differs')
    page.reload()
    expect(page.get_by_role('heading', name=re.compile(r'^' + re.escape(name) + r'(?:\s|$)'))).to_be_visible()
    if key == 'host_ethernet--compute':
        result['minimum_create'] = exercise_minimum_network(page, server, t, name, created['id'], result)
        result['last_step'] = 'minimum-create'
    button(page.get_by_role('dialog'), t['actions']['edit']).click()
    dialog = form(page, t, True)
    expect(type_control(dialog, t, kind)).to_be_checked()
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in roles)
        if role in TYPE_ROLES[kind]:
            expect(role_control(dialog, t, role)).to_be_enabled()
        else:
            expect(role_control(dialog, t, role)).to_be_disabled()
    if kind != 'host_ethernet':
        expect(dialog.get_by_role('button').filter(has_text=t['form']['advanced'])).to_have_attribute('aria-expanded', 'false')
    posts = 2 if key == 'host_ethernet--storage_sync' else 1
    if key == 'host_ethernet--storage_sync':
        result['last_step'] = 'edit-conflict'
        result['edit_conflict'] = exercise_network_edit_conflict(
            page, dialog, server, t, path, body, events, posts, evidence)
    else:
        button(dialog, t['actions']['cancel']).click()
        expect(dialog).not_to_be_visible()
        check(events == [{'method': 'POST', 'path': PREFIX}] * posts, 'read/cancel unexpectedly mutated resource')
    evidence.append({'layer': 'ui', 'type': kind, 'selected_roles': roles, 'disabled_roles': [r for r in ROLES if r not in TYPE_ROLES[kind]], 'refresh_edit_backfill': True})
    full_path = None
    if key == 'host_ethernet--compute':
        result['last_step'] = 'full-optional'
        result['full_optional'] = exercise_full_optional(page, server, t, name, evidence)
        full_path = result['full_optional']['path']
        page.goto(server.base + '/networks?network=' + str(created['id']))
        expect(page.get_by_role('heading', name=re.compile(r'^' + re.escape(name) + r'(?:\s|$)'))).to_be_visible()
    delete_owned(page, server, t, path, name, baseline, evidence)
    expected = [{'method': 'POST', 'path': PREFIX}] * posts + [{'method': 'DELETE', 'path': path}]
    if key == 'host_ethernet--storage_sync':
        expected = [{'method': 'POST', 'path': PREFIX}] * posts + [
            {'method': 'PUT', 'path': path},
            {'method': 'DELETE', 'path': path},
        ]
    if full_path is not None:
        expected = [{'method': 'POST', 'path': PREFIX}] * posts + [
            {'method': 'POST', 'path': PREFIX},
            {'method': 'DELETE', 'path': full_path},
            {'method': 'DELETE', 'path': path},
        ]
    check(events == expected, 'unexpected mutation sequence')
    # 12.7's last two halves, measured after the strict mutation sequence above so that list
    # keeps describing only the UI-driven writes. The fixtures below are driven over the API.
    exercise_delete_impact(page, server, name, evidence)


def exercise_delete_impact(page, server, name, evidence):
    """12.7 items 3 and 4 for the scoped network this case just deleted.

    This case owns its own evidence; the sibling network case is not borrowed from. Item 3 has
    no object on this resource - no delete path consults attachments - so what is measured is
    the product's actual behaviour: an attached network is deleted and its attachment goes with
    it. Item 4's allowed-cleanup half is that cascade plus the name being immediately reusable,
    which is what proves no attachment row survived; its failed-delete half is an id that does
    not exist answering 404 while a second network and its attachment are re-read unchanged.
    """
    node_uuid = None
    nets = []
    try:
        created_node = page.request.post(server.base + '/api/v1/nodes',
                                         data={'name': 'e2e-type-role-delete-node',
                                               'server_url': server.base})
        check(created_node.status in (200, 201),
              f"attach fixture node HTTP {created_node.status}")
        node_body = created_node.json()
        node_uuid = (node_body.get('node') or node_body).get('uuid')
        check(bool(node_uuid), 'attach fixture node has no uuid')

        def make_network(suffix, cidr):
            response = page.request.post(server.base + PREFIX,
                                         data={'name': f'{name}-{suffix}',
                                               'description': '12.7 scoped probe',
                                               'roles': ['compute'], 'type': 'host_ethernet',
                                               'cidr': cidr, 'interface': ''})
            check(response.status in (200, 201),
                  f"attach fixture network {suffix} HTTP {response.status}")
            body = response.json()
            ident = (body.get('network') or body).get('id')
            check(isinstance(ident, int) and ident > 0, 'attach fixture network has no id')
            nets.append(ident)
            return ident

        target = make_network('impact', '10.96.0.0/16')
        witness = make_network('witness', '10.97.0.0/16')
        target_path = f'{PREFIX}/{target}'
        witness_path = f'{PREFIX}/{witness}'
        for one in (target_path, witness_path):
            attached = page.request.post(server.base + one + '/attachments',
                                         data={'node_uuid': node_uuid})
            check(attached.status in (200, 201), f"attach on {one} HTTP {attached.status}")

        def detail(one, status=200):
            response = page.request.get(server.base + one)
            check(response.status == status,
                  f'GET {one}: expected {status}, received {response.status}')
            return response.json()

        before = detail(target_path)
        check(len(before.get('attachments') or []) == 1, 'the scoped target is not attached')
        check(before.get('resolved_node_count') == 1,
              f"target resolved to {before.get('resolved_node_count')} nodes, expected 1")
        witness_before = detail(witness_path)
        witness_count_before = len(witness_before.get('attachments') or [])

        removed = page.request.delete(server.base + target_path)
        check(removed.status == 200, f"DELETE attached scoped network HTTP {removed.status}")
        check(removed.json() == {'status': 'deleted'}, 'attached scoped delete envelope differs')
        after_status = page.request.get(server.base + target_path).status
        check(after_status == 404, f'deleted scoped network answers {after_status}, expected 404')

        nodes_after = page.request.get(server.base + '/api/v1/nodes').json()
        rows = nodes_after.get('items') if isinstance(nodes_after, dict) else nodes_after
        check(any(row.get('uuid') == node_uuid for row in (rows or [])),
              'deleting the network also removed the node')

        reused = page.request.post(server.base + PREFIX,
                                   data={'name': f'{name}-impact',
                                         'description': 'reuse after cascade',
                                         'roles': ['compute'], 'type': 'host_ethernet',
                                         'cidr': '10.98.0.0/16', 'interface': ''})
        check(reused.status in (200, 201),
              f"reusing the deleted scoped name HTTP {reused.status}")
        reused_body = reused.json()
        reused_id = (reused_body.get('network') or reused_body).get('id')
        nets.append(reused_id)
        check(not (detail(f'{PREFIX}/{reused_id}').get('attachments') or []),
              'the reused name came back carrying the dead scoped attachment')

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

        missing = page.request.delete(server.base + f'{PREFIX}/999999')
        check(missing.status == 404, f"DELETE missing scoped network HTTP {missing.status}")
        check(missing.json().get('code') == 'NETWORK_NOT_FOUND',
              'a missing scoped delete did not answer NETWORK_NOT_FOUND')

        witness_after = detail(witness_path)
        witness_count_after = len(witness_after.get('attachments') or [])
        unchanged = all(witness_after.get(k) == witness_before.get(k)
                        for k in ('name', 'description', 'cidr', 'interface', 'roles', 'type'))
        check(witness_count_before == witness_count_after,
              f'the witness attachment count moved {witness_count_before} -> '
              f'{witness_count_after} across a failed scoped delete')
        check(unchanged, 'a failed scoped delete altered the witness network')

        evidence.append({'layer': 'request', 'failed_delete_leaves_no_partial_change': {
            'missing_delete_status': 404,
            'missing_delete_code': missing.json().get('code'),
            'witness_id': witness,
            'witness_attachments_before': witness_count_before,
            'witness_attachments_after': witness_count_after,
            'witness_fields_unchanged': unchanged,
        }})
    finally:
        for ident in nets:
            page.request.delete(server.base + f'{PREFIX}/{ident}')
        if node_uuid:
            page.request.delete(server.base + f'/api/v1/nodes/{node_uuid}')


# Mainline: gpuworks/network/type-role/<to-RDMA-and-back>; UI创建含全部角色/CIDR/接口Ethernet → UI切RDMA
# → CIDR保持、interface隐藏、非法roles剔除禁用，切回不复活 → PUT RDMA空interface → GET/刷新advanced自动展开
# → UI切Ethernet确认空interface并保存 → GET/刷新持久化 → UI删除+GET404。无节点/流量效果。
def test_type_transition(page, server, result, locale, key, baseline):
    t, evidence = locale_strings(locale, 'networks'), result['evidence']
    target = TRANSITIONS[key]
    result['mainline'] = f'UI创建Ethernet全部角色/CIDR/接口 → 切换{target}、回切后不复活被剔除角色 → PUT {target} 隐藏接口为空 → GET/刷新advanced自动展开 → PUT Ethernet保留CIDR及角色 → UI删除及GET404'
    events = mutations_on(page, PREFIX)
    name = 'e2e-' + result['run_id'] + '-' + str(CASES.index(key))
    body = {'name': name, 'description': 'type transition', 'type': 'host_ethernet', 'roles': ROLES, 'cidr': '10.77.0.0/16', 'interface': 'ens*'}
    dialog = open_create(page, server, t, evidence)
    for field in ['name', 'description', 'cidr', 'interface']:
        dialog.get_by_label(t['form'][field], exact=True).fill(body[field])
    for role in ROLES:
        role_control(dialog, t, role).click()
    created = mutate(page, button(dialog, t['actions']['save']).click, 'POST', PREFIX, body, 201, evidence)
    path = PREFIX + '/' + str(created['id'])
    expect(page).to_have_url(server.base + '/networks?network=' + str(created['id']))
    persisted(page, server.base, path, body, evidence)
    page.reload()
    button(page.get_by_role('dialog'), t['actions']['edit']).click()
    dialog = form(page, t, True)
    choose_type(dialog, t, target)
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_count(0)
    advanced = dialog.get_by_role('button').filter(has_text=t['form']['advanced'])
    expect(advanced).to_have_attribute('aria-expanded', 'true')
    cidr = dialog.get_by_label(t['form']['bootstrapCidr'], exact=True)
    expect(cidr).to_have_value(body['cidr'])
    advanced.click()
    expect(cidr).not_to_be_visible()
    advanced.click()
    expect(cidr).to_be_visible()
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
        if role in TYPE_ROLES[target]:
            expect(role_control(dialog, t, role)).to_be_enabled()
        else:
            expect(role_control(dialog, t, role)).to_be_disabled()
    choose_type(dialog, t, 'host_ethernet')
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_value('ens*')
    expect(dialog.get_by_label(t['form']['cidr'], exact=True)).to_have_value(body['cidr'])
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_enabled()
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
    choose_type(dialog, t, target)
    changed = {**body, 'type': target, 'roles': TYPE_ROLES[target], 'interface': ''}
    mutate(page, button(dialog, t['actions']['save']).click, 'PUT', path, changed, 200, evidence)
    expect(dialog).not_to_be_visible()
    persisted(page, server.base, path, changed, evidence)
    page.reload()
    button(page.get_by_role('dialog'), t['actions']['edit']).click()
    dialog = form(page, t, True)
    expect(type_control(dialog, t, target)).to_be_checked()
    expect(dialog.get_by_role('button').filter(has_text=t['form']['advanced'])).to_have_attribute('aria-expanded', 'true')
    expect(dialog.get_by_label(t['form']['bootstrapCidr'], exact=True)).to_have_value(body['cidr'])
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
    choose_type(dialog, t, 'host_ethernet')
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_value('')
    back = {**changed, 'type': 'host_ethernet'}
    mutate(page, button(dialog, t['actions']['save']).click, 'PUT', path, back, 200, evidence)
    expect(dialog).not_to_be_visible()
    persisted(page, server.base, path, back, evidence)
    page.reload()
    button(page.get_by_role('dialog'), t['actions']['edit']).click()
    dialog = form(page, t, True)
    expect(type_control(dialog, t, 'host_ethernet')).to_be_checked()
    expect(dialog.get_by_label(t['form']['interface'], exact=True)).to_have_value('')
    expect(dialog.get_by_label(t['form']['cidr'], exact=True)).to_have_value(body['cidr'])
    for role in ROLES:
        expect(role_control(dialog, t, role)).to_be_checked(checked=role in TYPE_ROLES[target])
    button(dialog, t['actions']['cancel']).click()
    expect(dialog).not_to_be_visible()
    evidence.append({'layer': 'ui', 'transition': ['host_ethernet', target, 'host_ethernet'], 'advanced_toggle_and_saved_expansion': True, 'cidr_retained': body['cidr'], 'interface_draft_retained_but_saved_empty': True, 'roles_not_resurrected': TYPE_ROLES[target]})
    delete_owned(page, server, t, path, name, baseline, evidence)
    check(events == [{'method': 'POST', 'path': PREFIX}] + [{'method': 'PUT', 'path': path}] * 2 + [{'method': 'DELETE', 'path': path}], 'unexpected transition mutations')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', choices=CASES + ['all'], default='all')
    parser.add_argument('--locale', choices=['en', 'zh'], default='en')
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--artifact-dir', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ').lower() + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    selected = CASES if args.case == 'all' else [args.case]
    manifest_path = ROOT / 'test/e2e/coverage/network-type-roles.json'
    manifest = json.loads(manifest_path.read_text())
    report = {'run_id': run_id, 'status': 'NOT_RUN', 'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'lane': 'local-real-server-sqlite', 'identity': 'admin', 'locale': args.locale, 'timezone': 'UTC',
              'viewport': {'width': 1440, 'height': 900}, 'retries': 0, 'cases': [], 'selection': selected,
              'source_digests': {str(p.relative_to(ROOT)): digest(p) for p in [Path(__file__).resolve(), manifest_path,
                  ROOT / 'test/e2e/local_browser_fixture.py', ROOT / 'test/e2e/network_storage_lifecycle.py', ROOT / 'test/e2e/platform_token_read_revoke.py', ROOT / 'test/e2e/dialog_checks.py', ROOT / 'test/e2e/network_create_inventory.py', ROOT / 'ui/src/pages/networks/NetworkFormModal.tsx', ROOT / 'ui/src/pages/networks/NetworkDetailDrawer.tsx', ROOT / 'ui/src/pages/networks/NetworksPage.tsx']},
              'command': ['python3', 'test/e2e/network_type_roles.py', *sys.argv[1:]]}
    try:
        check(manifest['test_file'] == 'test/e2e/network_type_roles.py', 'manifest file mismatch')
        indexed = {case['case_id']: case for case in manifest['cases']}
        check(len(manifest['cases']) == len(CASES) and list(indexed) == CASES, 'manifest denominator differs')
        for key in CASES:
            expected_function = 'test_type_role_combination' if key in COMBINATIONS else 'test_type_transition'
            check(indexed[key]['test_function'] == expected_function and indexed[key]['contract_id'] == 'gpuworks/network/type-role/' + key, 'manifest binding differs')
            check(indexed[key]['required_evidence'] == ['ui', 'request', 'persistence', 'cleanup'], 'evidence contract differs')
        report['binary_sha256'] = digest(args.server_bin)
        report['health'] = server.start()
        expected_version = subprocess.check_output(['git', 'describe', '--tags', '--always', '--dirty'], cwd=ROOT, text=True).strip()
        check(report['health']['version'] == expected_version, 'binary health version does not match current source; rebuild first')
        report['base_url'], report['owned_server_pid'] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            report['browser_version'] = browser.version
            for key in selected:
                result = {'contract_id': indexed[key]['contract_id'], 'run_id': run_id, 'status': 'NOT_RUN', 'evidence': []}
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
                    baseline = read(page, server.base, PREFIX)
                    check(set(baseline) == {'items', 'roles', 'types', 'type_roles'}, 'collection envelope differs')
                    check(baseline['roles'] == ROLES and baseline['types'] == TYPES and baseline['type_roles'] == TYPE_ROLES, 'type/role dictionary changed; explicitly review denominator')
                    check(all(item['builtin'] for item in baseline['items']), 'previous case left owned resources')
                    errors, bad_http, console_errors = [], [], []
                    page.on('pageerror', lambda error: errors.append(str(error)))
                    page.on('response', lambda r: bad_http.append({'path': urlsplit(r.url).path, 'status': r.status}) if r.status >= 400 else None)
                    page.on('console', lambda message: console_errors.append(message.text) if message.type == 'error' else None)
                    artifacts.start_trace(context)
                    function = test_type_role_combination if key in COMBINATIONS else test_type_transition
                    if key in COMBINATIONS:
                        function(page, server, result, args.locale, key, baseline, bad_http, console_errors)
                    else:
                        function(page, server, result, args.locale, key, baseline)
                    check(not errors and not bad_http and not console_errors, f'browser errors: {errors}, {bad_http}, {console_errors}')
                    check(page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'page horizontal overflow')
                    check(set(indexed[key]['required_evidence']) <= {e['layer'] for e in result['evidence']}, 'missing required evidence')
                    result['browser_errors'] = {'page': errors, 'http': bad_http, 'console': console_errors}
                    page.screenshot(path=artifacts.directory / f'{key}-complete.png')
                    result['status'] = 'VERIFIED_PENDING_ARTIFACT_SCAN'
                except Exception:
                    result['status'], result['failure'] = 'FAIL_TEST', traceback.format_exc()
                    if not any(secret in page.content() for secret in artifacts.secrets if len(secret) > 8):
                        page.screenshot(path=artifacts.directory / f'{key}-failure.png')
                finally:
                    artifacts.stop_trace(context)
                    context.close()
                if result['status'] == 'FAIL_TEST':
                    # Freeze this isolated database; do not contaminate subsequent cases.
                    break
            browser.close()
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = traceback.format_exc()
    finally:
        report['server_stopped'] = server.stop()
        executed = {c['contract_id'] for c in report['cases']}
        for key in selected:
            contract_id = 'gpuworks/network/type-role/' + key
            if contract_id not in executed:
                report['cases'].append({'contract_id': contract_id, 'run_id': run_id, 'status': 'NOT_RUN', 'evidence': [], 'reason': 'Earlier failure froze this run'})
        log = artifacts.private / 'server.log'
        if log.exists():
            (artifacts.directory / 'server.log').write_text(artifacts.redact(log.read_text()))
        report['private_state'] = 'Isolated SQLite retained for 7 days; no process kept. Remove only this run directory after diagnosis.'
        artifacts.write_json('report.json', report)
        try:
            check(report['source_sha'] == subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(), 'HEAD changed during execution')
            check(all(digest(ROOT / path) == value for path, value in report['source_digests'].items()), 'source changed during execution')
            if 'binary_sha256' in report:
                check(digest(args.server_bin) == report['binary_sha256'], 'binary changed during execution')
            report['source_integrity'] = 'PASS'
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['source_integrity'] = {'status': 'FAIL', 'detail': traceback.format_exc()}
        try:
            artifacts.scan()
            report['artifact_scan'] = 'PASS'
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_scan'] = 'FAIL: do not upload this private run directory'
        if report['status'] == 'NOT_RUN':
            verified = len(report['cases']) == len(selected) and all(c['status'] == 'VERIFIED_PENDING_ARTIFACT_SCAN' for c in report['cases'])
            report['status'] = 'PASS' if verified and report['server_stopped'] else 'FAIL_TEST'
            if report['status'] == 'PASS':
                for result in report['cases']:
                    result['status'] = 'PASS'
        if not report['server_stopped']:
            report['status'] = 'FAIL_TEST'
        artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
