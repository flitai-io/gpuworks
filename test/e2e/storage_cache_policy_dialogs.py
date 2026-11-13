"""Dialog helpers for the NFS cache node-policy browser case."""
from __future__ import annotations

import json
import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from network_storage_lifecycle import locale_strings, read
from platform_token_read_revoke import button, check

GIB = 1024 ** 3

def mutate(page, action, method, path, body, status, evidence):
    """Record the observed wire contract before checking it, including failures."""
    observation = {'layer': 'request', 'method': method, 'path': path,
                   'expected_body': body, 'expected_status': status, 'status': None, 'sent': []}
    evidence.append(observation)
    def sent(request):
        if request.method == method and urlsplit(request.url).path == path:
            observation['sent'].append({'query': urlsplit(request.url).query, 'raw_body': request.post_data})
    page.on('request', sent)
    try:
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == method) as received:
            action()
    finally:
        page.remove_listener('request', sent)
    response = received.value
    observation.update(status=response.status, query=urlsplit(response.url).query,
                       raw_body=response.request.post_data, response_text=response.text())
    observation['body'] = response.request.post_data_json if response.request.post_data is not None else None
    # Canonical JSON keeps false distinct from 0 and 0.0 from integer bytes;
    # ordinary Python equality would accept both as the same capacity.
    observation['wire_json_exact'] = (json.dumps(observation['body'], sort_keys=True)
                                      == json.dumps(body, sort_keys=True))
    check(observation['wire_json_exact'],
          f'{method} {path}: expected body {body!r}, received {observation["body"]!r}')
    check(response.status == status, f'{method} {path}: expected {status}, received {response.status}')
    check(not observation['query'], f'{method} {path}: unexpected query {observation["query"]!r}')
    result = response.json() if status != 204 else None
    observation['response'] = result
    return result


def exercise_editor_dismissals(page, dialog, reopen, close_selector, cancel_label, snapshot, evidence, label):
    """Cancel, the close button, and Escape send no storage write and keep the GET."""
    expect(dialog.locator(close_selector)).to_have_count(1, timeout=12000)
    writes = []

    def on_request(request):
        request_path = urlsplit(request.url).path
        if request_path.startswith('/api/v1/storages') and request.method in {'POST', 'PUT', 'PATCH', 'DELETE'}:
            writes.append(request.method)

    page.on('request', on_request)
    try:
        baseline = len(writes)
        before = snapshot()
        current = dialog
        focus_report = []

        def dismiss_and_reopen(action_name, action):
            nonlocal current
            action(current)
            expect(current).to_be_hidden()
            # 12.4: closing returns focus to the control that opened the editor.
            focus_after = page.evaluate("""() => {
                const active = document.activeElement;
                if (!active) return null;
                return {tag: active.tagName, text: (active.textContent || '').trim().slice(0, 40),
                        inDrawer: !!active.closest('.ant-drawer')};
            }""")
            focus_report.append({'action': action_name, 'focus': focus_after})
            # The storage detail is itself a drawer and stays open.
            expect(page.get_by_role('dialog')).to_have_count(1)
            check(len(writes) == baseline, f'{label} {action_name} sent a storage write')
            check(snapshot() == before, f'{label} {action_name} changed the saved cache policy')
            current = reopen()
            expect(current).to_be_visible()
            expect(current.locator(close_selector)).to_have_count(1, timeout=12000)

        dismiss_and_reopen('cancel', lambda opened: button(opened, cancel_label).click())
        dismiss_and_reopen('close', lambda opened: opened.locator(close_selector).click())

        def press_escape(opened):
            opened.locator(close_selector).focus()
            page.keyboard.press('Escape')

        dismiss_and_reopen('escape', press_escape)
    finally:
        page.remove_listener('request', on_request)
    lost = [entry['action'] for entry in focus_report
            if not entry['focus'] or not entry['focus']['inDrawer']]
    evidence.append({'layer': 'ui', 'dismissals': label,
                     'actions': ['cancel', 'close', 'escape'], 'write_requests': 0,
                     'focus_return': focus_report, 'focus_lost': lost})
    check(not lost, f'{label} editor left focus outside the detail drawer after: {lost}')
    return current


def _box(locator):
    return locator.evaluate("""element => {
        const rect = element.getBoundingClientRect();
        return {left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom,
                width: rect.width, height: rect.height};
    }""")


def _separated(first, second):
    return (first["right"] <= second["left"] or second["right"] <= first["left"]
            or first["bottom"] <= second["top"] or second["bottom"] <= first["top"])


def exercise_node_editor_keyboard(page, dialog, cm, cp, cancel_label, snapshot, evidence):
    """Tab, Enter, the invalid-directory alert, and the 1440x900 footer."""
    close = dialog.locator(".ant-drawer-close")
    keep = dialog.get_by_role("radio", name=cm["keep"], exact=True)
    unlimited = dialog.get_by_role("radio", name=cp["unlimited"], exact=True)
    cancel = button(dialog, cancel_label)
    save = button(dialog, cm["save"])
    directory = dialog.get_by_label(cm["pathValue"], exact=True)
    close.focus()
    expect(close).to_be_focused()
    close.press("Tab")
    expect(keep).to_be_focused()
    keep.press("Tab")
    expect(unlimited).to_be_focused()
    unlimited.press("Tab")
    expect(cancel).to_be_focused()
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    for locator in (directory, cancel, save):
        expect(locator).to_be_in_viewport(ratio=1)
    directory_box = _box(directory)
    for other in (cancel, save):
        other_box = _box(other)
        check(directory_box["width"] > 0 and other_box["width"] > 0, "directory or footer action has no box")
        check(_separated(directory_box, other_box), "directory overlaps a footer action")

    writes = []

    def on_request(request):
        request_path = urlsplit(request.url).path
        if request_path.startswith("/api/v1/storages") and request.method in {"POST", "PUT", "PATCH", "DELETE"}:
            writes.append(request.method)

    page.on("request", on_request)
    try:
        baseline = len(writes)
        before = snapshot()
        original = directory.input_value()
        check(original.startswith("/"), "directory value is not absolute")
        dialog.get_by_role("radio", name=cm["customPath"], exact=True).check()
        expect(directory).to_be_enabled()
        directory.focus()
        directory.press("Enter")
        expect(dialog).to_be_visible()
        expect(directory).to_have_value(original)
        check(len(writes) == baseline, "Enter in the directory sent a storage write")
        check(snapshot() == before, "Enter in the directory changed the saved cache policy")
        directory.fill("")
        save.click()
        expect(dialog.get_by_role("alert").filter(has_text=cm["invalidPath"])).to_be_visible()
        check(len(writes) == baseline, "empty directory sent a storage write")
        check(snapshot() == before, "empty directory changed the saved cache policy")
        directory.fill(original)
        keep.check()
        expect(directory).to_be_disabled()
        expect(save).to_be_disabled()
    finally:
        page.remove_listener("request", on_request)
    evidence.append({"layer": "ui", "node_editor_keyboard": True, "write_requests": 0})
    return True


def exercise_defaults_dialog(page, trigger, title, parent, field_label, save_label, cancel_label, evidence):
    """Defaults dialog identity, layout, keyboard trap, and dismissals. No storage write.

    Close does not return focus: the editor unmounts, so focus return is not asserted.
    """
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    writes = []

    def on_request(request):
        request_path = urlsplit(request.url).path
        if request_path.startswith('/api/v1/storages') and request.method in {'POST', 'PUT', 'PATCH', 'DELETE'}:
            writes.append(request.method)

    page.on('request', on_request)
    baseline = len(writes)

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
        lambda: len(writes))
    check(len(writes) == baseline, 'defaults dialog dismissal sent a write')
    evidence.append({'layer': 'ui', 'defaults_dialog': title,
                     'dismissals': ['cancel', 'escape', 'close', 'mask'], 'write_requests': 0})
    return dialog


def exercise_defaults_inflight(page, dialog, cm, api_path, body, evidence):
    """Hold a real stale PATCH: duplicate actions stay blocked until its 409."""
    held = []

    def hold_patch(route):
        if route.request.method != 'PATCH' or urlsplit(route.request.url).path != api_path:
            route.continue_()
            return
        held.append(route)

    page.route('**' + api_path, hold_patch)
    try:
        button(dialog, cm['save']).click()
        inflight = dialog.locator('.ant-modal-footer button.ant-btn-primary')
        expect(inflight).to_be_disabled()
        inflight.click(force=True)
        page.keyboard.press('Enter')
        check(len(held) == 1, 'duplicate defaults save while the first request is held')
        check(held[0].request.post_data_json == body, 'held defaults body differs')
        pending = held.pop()
        result = mutate(page, pending.continue_, 'PATCH', api_path, body, 409, evidence)
    finally:
        for pending in held:
            pending.continue_()
        page.unroute('**' + api_path, hold_patch)
    evidence.append({'layer': 'ui', 'inflight_posts': 1, 'save_disabled': True,
                     'failure_status': 409, 'response_source': 'real server revision conflict'})
    return result


def exercise_defaults_conflict(page, base, dialog, cm, cp, path_value, api_path, stale, conflict_text, errors, evidence, locale):
    """Another writer advances the revision. The open dialog still sends the previewed expected_updated_at."""
    # A separate browser context changes the real resource while the original
    # editor retains its frozen baseline. Both saves run through the UI.
    writer_context = page.context.browser.new_context(
        storage_state=page.context.storage_state(), viewport=page.viewport_size,
        locale='zh-CN' if locale == 'zh' else 'en-US', timezone_id='UTC')
    try:
        writer = writer_context.new_page()
        writer_errors = []
        writer.on('pageerror', lambda error: writer_errors.append(str(error)))
        writer.goto(base + '/storage/' + api_path.rsplit('/', 1)[-1])
        storage_texts = locale_strings(locale, 'storage')
        writer.get_by_role('tab', name=re.compile(
            '^' + re.escape(storage_texts['detail']['tab']['nodes']) + r'\s*25$')).click()
        for gib in (96, 128):
            baseline = read(writer, base, api_path)
            button(writer, cm['defaults']).click()
            editor = writer.get_by_role('dialog', name=cm['defaults'], exact=True)
            editor.get_by_role('radio', name=cp['custom'], exact=True).check()
            editor.get_by_role('spinbutton').fill(str(gib))
            mutate(writer, button(editor, cm['save']).click, 'PATCH', api_path,
                   {'cache_max_bytes': gib * GIB, 'expected_updated_at': baseline['updated_at']},
                   200, evidence)
            expect(editor).to_be_hidden()
            preview = read(writer, base, api_path)
            check(preview['cache_max_bytes'] == gib * GIB, 'other session limit did not persist')
        check(preview.get('updated_at') not in (None, stale), 'other session did not advance revision')
        check(not writer_errors, f'other session browser errors: {writer_errors!r}')
        evidence.append({'layer': 'ui', 'concurrent_writer': 'separate browser context',
                         'limit_cycle_gib': [128, 96, 128], 'revision_changed': True})
    finally:
        writer_context.close()
    result = exercise_defaults_inflight(page, dialog, cm, api_path,
                    {'cache_max_bytes': 0, 'expected_updated_at': stale}, evidence)
    check(result.get('code') == 'STORAGE_CONFIG_CHANGED', f'conflict code {result!r}')
    expect(dialog).to_be_visible()
    expect(dialog.get_by_role('radio', name=cp['unlimited'], exact=True)).to_be_checked()
    expect(dialog.get_by_label(cm['pathValue'], exact=True)).to_have_value(path_value)
    expect(dialog.get_by_role('alert').filter(has_text=conflict_text)).to_be_visible()
    stayed = read(page, base, api_path + '?all_nodes=1')
    evidence.append({'layer': 'persistence', 'conflict': 'STORAGE_CONFIG_CHANGED', 'response': stayed})
    check(stayed.get('cache_max_bytes') == 128 * GIB and stayed.get('updated_at') == preview['updated_at'],
          'stale defaults save changed the stored limit or revision')
    node = next(item for item in stayed['targets'] if item.get('node_uuid') == 'cache-node-02')
    check(node.get('cache_max_bytes') == 64 * GIB, 'stale defaults save changed a node override')
    check(any(item.get('path') == api_path and item.get('status') == 409 for item in errors['http']), '409 was not recorded')
    check(any('409' in item.get('text', '') for item in errors['console']), '409 console line absent')
    errors['http'][:] = [item for item in errors['http'] if not (item.get('path') == api_path and item.get('status') == 409)]
    errors['console'][:] = [item for item in errors['console'] if '409' not in item.get('text', '')]
    evidence.append({'layer': 'ui', 'conflict_code': 'STORAGE_CONFIG_CHANGED', 'draft_retained': True,
                     'dialog_open': True, 'teardown_asserted': False})


def exercise_cache_limit_defaults(page, dialog, limit_input, unit_name, persisted):
    """A custom limit opens empty, in GiB, with step 1 and a minimum of 0.

    The unit choices are GiB and TiB. This is a byte capacity: no percent suffix,
    no slider, and no maximum on the control. Nothing here sends a node override.
    """
    writes = []

    def on_request(request):
        if request.method == 'PATCH' and urlsplit(request.url).path.endswith('/node-overrides'):
            writes.append(request.method)

    page.on('request', on_request)
    baseline = len(writes)
    expect(limit_input).to_have_value('')
    expect(limit_input).to_have_attribute('step', '1')
    expect(limit_input).to_have_attribute('aria-valuemin', '0')
    check(limit_input.get_attribute('aria-valuemax') is None, 'the cache limit control has a maximum')
    check(dialog.locator('.ant-input-number-suffix').count() == 0, 'the cache limit has a suffix')
    check(dialog.locator('.ant-slider').count() == 0, 'the cache limit rendered a slider')
    selected = dialog.locator('.ant-select-selection-item')
    expect(selected).to_have_count(1)
    expect(selected).to_have_attribute('title', 'GiB')
    check(dialog.locator(f'[aria-label="{unit_name}"]').count() >= 1,
          'the unit control has no capacity label')
    selector = dialog.locator('.ant-select-selector')
    selector.click()
    options = page.locator('.ant-select-dropdown:visible .ant-select-item-option')
    expect(options).to_have_count(2)
    expect(options.nth(0)).to_have_attribute('title', 'GiB')
    expect(options.nth(1)).to_have_attribute('title', 'TiB')
    selector.click()
    expect(page.locator('.ant-select-dropdown:visible')).to_have_count(0)
    expect(dialog).to_be_visible()
    expect(selected).to_have_attribute('title', 'GiB')
    check(persisted() == 0, 'reading the default limit changed the stored bytes')
    check(len(writes) == baseline, 'reading the default limit sent a node override')
    page.remove_listener('request', on_request)
    return {'value': '', 'unit': 'GiB', 'step': '1', 'min': '0', 'max': None,
            'units': ['GiB', 'TiB'], 'suffix': False, 'slider': False,
            'persisted': 0, 'patches': 0}


def exercise_cache_limit_bounds(page, dialog, limit_input, save_name, invalid_text, persisted):
    """Custom GiB limit: reject 0, empty, non-numeric and a byte count past the safe integer.

    1 GiB, 1.5 GiB and 8388607 GiB stay displayed. Nothing here sends a node-overrides PATCH.
    """
    writes = []

    def on_request(request):
        if request.method == 'PATCH' and urlsplit(request.url).path.endswith('/node-overrides'):
            writes.append(request.method)

    page.on('request', on_request)
    baseline = len(writes)

    def reject(text, shown):
        limit_input.fill(text)
        limit_input.blur()
        expect(limit_input).to_have_value(shown)
        button(dialog, save_name).click()
        expect(dialog.get_by_role('alert').filter(has_text=invalid_text)).to_be_visible()
        check(persisted() == 0, f'{text!r} changed the persisted limit')
        check(len(writes) == baseline, f'{text!r} sent a node override')

    reject('-1', '')
    reject('0', '')
    reject('', '')
    reject('abc', '')
    limit_input.fill('1.5')
    limit_input.blur()
    expect(limit_input).to_have_value('1.5')
    limit_input.fill('1')
    limit_input.blur()
    expect(limit_input).to_have_value('1')
    limit_input.fill('8388607')
    limit_input.blur()
    expect(limit_input).to_have_value('8388607')
    reject('8388608', '8388608')
    check(len(writes) == baseline, 'limit bounds sent a node override')
    page.remove_listener('request', on_request)
    return {'negative': '', 'zero': '', 'empty': '', 'non_numeric': '',
            'decimal': '1.5', 'minimum_shown': '1', 'maximum_shown': '8388607',
            'above_maximum': '8388608', 'persisted': 0, 'patches': 0}


def exercise_cache_limit_steps(page, dialog, limit_input, unit_name, persisted, *, precision=True):
    """Keyboard and step buttons change the custom GiB limit. The unit select shows the same bytes."""
    writes = []

    def on_request(request):
        if request.method == 'PATCH' and urlsplit(request.url).path.endswith('/node-overrides'):
            writes.append(request.method)

    page.on('request', on_request)
    baseline = len(writes)
    limit_input.fill('2')
    limit_input.blur()
    expect(limit_input).to_have_value('2')
    limit_input.press('ArrowUp')
    expect(limit_input).to_have_value('3')
    limit_input.press('ArrowDown')
    expect(limit_input).to_have_value('2')
    increase = dialog.get_by_role('button', name='Increase Value', exact=True)
    decrease = dialog.get_by_role('button', name='Decrease Value', exact=True)
    expect(increase).to_have_count(1)
    expect(decrease).to_have_count(1)

    def click_step(target):
        dialog.locator('.ant-input-number').first.hover()
        target.click(force=True)

    click_step(increase)
    expect(limit_input).to_have_value('3')
    click_step(decrease)
    expect(limit_input).to_have_value('2')
    check(dialog.locator('.ant-slider').count() == 0, 'the cache limit rendered a slider')
    check(dialog.get_by_role('spinbutton').count() == 1, 'the cache limit has a second spinbutton')
    selector = dialog.locator('.ant-select-selector')
    expect(selector).to_have_count(1)
    # Keep the existing exact conversion alongside a sub-TiB precision check.
    limit_input.fill('1024')
    limit_input.blur()
    expect(limit_input).to_have_value('1024')
    # The role=option node is a hidden accessibility mirror. The visible item carries title.
    selector.click()
    page.get_by_title('TiB', exact=True).click()
    expect(limit_input).to_have_value('1')
    selector.click()
    page.get_by_title('GiB', exact=True).click()
    expect(limit_input).to_have_value('1024')
    if precision:
        limit_input.fill('2')
        limit_input.blur()
        selector.click()
        page.get_by_title('TiB', exact=True).click()
        expect(limit_input).to_have_value('0.001953125')
        limit_input.blur()
        expect(limit_input).to_have_value('0.001953125')
        selector.click()
        page.get_by_title('GiB', exact=True).click()
        expect(limit_input).to_have_value('2')
    limit_input.fill('1')
    limit_input.blur()
    expect(limit_input).to_have_value('1')
    limit_input.press('ArrowDown')
    expect(limit_input).to_have_value('')
    check(persisted() == 0, 'stepping the limit changed the stored bytes')
    check(len(writes) == baseline, 'stepping the limit sent a node override')
    page.remove_listener('request', on_request)
    return {'from': 2, 'keyboard': 3, 'button': 3, 'at_one_step_down': '',
            'tib_from_1024gib': '1', 'restored_gib': '1024',
            'tib_from_2gib': '0.001953125' if precision else None,
            'slider': False, 'spinbuttons': 1, 'patches': 0}




def exercise_server_limit_range(page, base, path, targets, evidence):
    """The server enforces the same lower bound the control shows, on its own.

    The form refuses a negative capacity before sending anything, so the UI can
    never reach the server's guard. It is still the server's rule to keep: a
    client that skips the form must be refused, and the refusal must not disturb
    the value already stored.
    """
    before = targets()['cache-node-01']['cache_max_bytes']
    attempts = []
    for label, body in (('negative', {'node_uuids': ['cache-node-01'], 'cache_max_bytes': -1}),
                        ('negative-gib', {'node_uuids': ['cache-node-01'], 'cache_max_bytes': -1 * GIB})):
        response = page.request.patch(base + path + '/node-overrides', data=body)
        detail = response.json() if response.text() else None
        attempts.append({'case': label, 'sent': body, 'status': response.status,
                         'code': (detail or {}).get('code')})
        check(response.status == 400,
              f'the server accepted a {label} cache limit: HTTP {response.status}')
        check((detail or {}).get('code') == 'STORAGE_CACHE_LIMIT_NEGATIVE',
              f'the {label} refusal used an unexpected code: {(detail or {}).get("code")}')
    after = targets()['cache-node-01']['cache_max_bytes']
    check(after == before,
          f'a refused cache limit changed the stored value: {before} then {after}')
    evidence.append({'layer': 'request', 'server_limit_range': attempts,
                     'stored_before': before, 'stored_after': after})
    return {'attempts': attempts, 'stored_before': before, 'stored_after': after}
