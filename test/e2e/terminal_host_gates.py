"""Post-mainline host-only gates. No container authority or coverage declaration."""
import copy
import hashlib
import json
import re
import sqlite3
import time
from urllib.parse import urlsplit
from playwright.sync_api import expect
from local_browser_fixture import ADMIN_PASSWORD, ROOT
from platform_token_read_revoke import button, check
from terminal_host_oracles import POLICY, TEXT, NODE_BUSY, GLOBAL_BUSY, OFFLINE, require, policy_contract
from terminal_host_owner import Owner
from terminal_host_sessions import Sessions, poll


def labels(locale, namespace):
    return json.loads((ROOT / 'ui/src/i18n/locales' / locale / (namespace + '.json')).read_text())


def context_for(owner, parent, locale, authenticated=True):
    options = {'locale': 'zh-CN' if locale == 'zh' else 'en-US', 'timezone_id': 'UTC',
               'viewport': {'width': 1440, 'height': 900}}
    if authenticated:
        options['storage_state'] = parent.storage_state()
    result = parent.browser.new_context(**options)
    observed = {'context': result, 'closed': False}
    result.on('close', lambda _: observed.update(closed=True))
    owner.contexts.append(observed)
    return result


def set_policy(owner, page, locale, value):
    policy_contract(value)
    text = labels(locale, 'settings')
    page.goto(owner.server.base + '/settings')
    page.get_by_role('tab', name=text['page']['tabs']['access'], exact=True).click()
    card = page.locator('.ant-card').filter(has_text=text['terminal']['title']).first
    expect(card.get_by_role('switch')).to_be_checked()
    for key, title in [('idle_timeout_min', 'idle'), ('grace_sec', 'grace'),
                       ('max_per_node', 'nodeLimit'), ('max_global', 'globalLimit')]:
        field = card.locator('.ant-form-item').filter(has=page.get_by_text(text['terminal'][title], exact=True))
        field.get_by_role('spinbutton').fill(str(value[key]))
        field.get_by_role('spinbutton').press('Tab')
        require(field.get_by_role('spinbutton').input_value() == str(value[key]),
                'the terminal policy field ' + key + ' did not take the typed value: '
                + field.get_by_role('spinbutton').input_value())
    with page.expect_response(lambda r: r.request.method == 'PUT'
            and urlsplit(r.url).path == '/api/v1/settings/terminal') as changed:
        button(card, text['common']['save']).click()
    require(changed.value.status == 200 and changed.value.request.post_data_json == value,
            'full UI terminal policy PUT differs: '
            + json.dumps({'sent': changed.value.request.post_data_json, 'wanted': value},
                         ensure_ascii=False))
    status, saved = owner.call('GET', '/api/v1/settings')
    require(status == 200 and saved['terminal'] == value, 'policy independent GET differs')
    with sqlite3.connect(owner.db) as db:
        stored = db.execute("SELECT value FROM settings WHERE key='terminal'").fetchone()
    require(stored and json.loads(stored[0]) == value, 'policy SQLite differs from independent declaration')
    owner.report['policy_puts'].append(copy.deepcopy(value))


def node_row(page, owner, node, locale):
    text = labels(locale, 'nodes')
    page.goto(owner.server.base + '/nodes')
    nav = page.get_by_role('complementary', name=labels(locale, 'common')['header']['navigation']).get_by_role('link', name=labels(locale, 'common')['navigation']['nodes'], exact=True)
    expect(nav).to_have_attribute('href', '/nodes')
    nav.click()
    expect(page).to_have_url(owner.server.base + '/nodes')
    expect(nav).to_have_attribute('aria-current', 'page')
    row = page.get_by_role('row').filter(has_text=node['name'])
    expect(row).to_have_count(1)
    control = row.get_by_role('button', name=text['action']['terminal'], exact=True)
    expect(control).to_be_visible()
    expect(control).to_be_enabled()
    return row, text


def permissions(owner, sessions, admin_context, parent, locale):
    context = context_for(owner, parent, locale, False)
    page = context.new_page()
    owner.pages.append(page)
    auth = labels(locale, 'auth')
    owner.sensitive = True
    page.goto(owner.server.base + '/nodes')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill(owner.username)
    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login'
            and r.request.method == 'POST') as login:
        button(page, auth['submit']).click()
    require(login.value.status == 200, 'real user login failed')
    for cookie in context.cookies():
        owner.artifacts.remember(cookie['value'])
    owner.sensitive = False
    row, text = node_row(page, owner, owner.first, locale)
    # Current product deliberately renders the row entry for readers; access is
    # denied at the admin API boundary. Do not invent a hidden-button contract.
    path = '/api/v1/nodes/' + owner.first['uuid'] + '/terminal/settings'
    sessions.expected_http.add((path, 403))
    before = {r['id'] for r in owner.audits()}
    record = sessions.popup(page, row, text['action']['terminal'], owner.first, context)
    sessions.denied(record, TEXT[locale]['permission'], 403)
    record['page'].close()
    record = sessions.deep(owner.first, context)
    sessions.denied(record, TEXT[locale]['permission'], 403)
    sessions.upgrade_denied(context, owner.first['uuid'], 403)
    require({r['id'] for r in owner.audits()} == before and owner.active(owner.first['uuid'])['active_total'] == 0,
            'nonadmin denial consumed an audit or slot')
    record['page'].close()
    owner.report['permissions'] = {'role': 'user', 'row_entry': 'visible/enabled; actual API denies',
        'popup_preflight': 403, 'deep_preflight': 403, 'upgrade': 403,
        'code': 'ADMINISTRATOR_REQUIRED', 'created_audits': 0}


def keyboard_focus(owner, page, locale):
    """22.6: real Tab keypresses reach the terminal entry and the focused
    control reports the product focus-visible ring through computed style.

    The theme value is read from the stylesheet rule the product ships, never
    hardcoded, and the unfocused control is required to differ from it, so a
    ring that is always painted cannot satisfy this gate.
    """
    row, text = node_row(page, owner, owner.first, locale)
    control = row.get_by_role('button', name=text['action']['terminal'], exact=True)
    # Theme semver: the ring must derive from the product's own primary token,
    # so a hardcoded or foreign colour cannot pass.
    tokens = (ROOT / 'ui/src/theme/tokens.ts').read_text()
    primary = re.search(r'primary:\s*"#([0-9a-fA-F]{6})"', tokens)
    require(primary is not None, 'no theme primary token to compare the focus ring against')
    base = [int(primary[1][index:index + 2], 16) for index in (0, 2, 4)]

    def rgb(value):
        found = re.match(r'rgba?\(([^)]+)\)', value or '')
        require(found is not None, 'focus colour is not a computed rgb value: ' + repr(value))
        return [int(part.strip()) for part in found[1].split(',')[:3]]

    rest = control.evaluate("el => { const s = getComputedStyle(el); "
                            "return {width: s.outlineWidth, style: s.outlineStyle, color: s.outlineColor}; }")
    require(rest['width'] in ('0px', '0') , 'the terminal entry paints an outline before it is focused: ' + str(rest))
    # Real keyboard navigation only: pointer focus does not set :focus-visible.
    page.locator('body').click(position={'x': 2, 'y': 2})
    focused, presses = False, 0
    for presses in range(1, 61):
        page.keyboard.press('Tab')
        if control.evaluate('el => el === document.activeElement'):
            focused = True
            break
    require(focused, 'no Tab sequence from the page body reached the terminal entry')
    seen = control.evaluate("el => { const s = getComputedStyle(el); "
                            "return {width: s.outlineWidth, style: s.outlineStyle, color: s.outlineColor, "
                            "outline: s.outline, matches: el.matches(':focus-visible')}; }")
    require(seen['matches'], 'keyboard-focused control does not match :focus-visible')
    require(seen['style'] == 'solid' and seen['width'] not in ('0px', '0'),
            'keyboard focus paints no visible solid ring: ' + str(seen))
    ring = rgb(seen['color'])
    require(ring != base, 'focus ring uses the raw primary instead of a distinct ring token: ' + str(ring))
    require(all(ring[channel] >= base[channel] for channel in range(3)),
            f'focus ring {ring} is not a lighter derivative of the theme primary {base}')
    require(max(ring) > max(base), f'focus ring {ring} is not lighter than the theme primary {base}')
    owner.report['keyboard_focus'] = {'entry': text['action']['terminal'], 'tab_presses': presses,
        'theme_primary_rgb': base, 'unfocused': rest, 'focused': seen,
        'source': 'real Tab keypresses; getComputedStyle ring compared to the theme primary token'}


def session_expiry(owner, page, locale, result):
    """22.4: what keeps a login alive, and what deliberately does not.

    A read-only poll must not extend the idle deadline, the absolute deadline
    must bound every extension, and once the session is gone the page must stop
    hammering the server until a human re-authenticates.
    """
    with sqlite3.connect(owner.db) as db:
        rows = db.execute("SELECT token_hash, expires_at, absolute_expires_at FROM management_sessions").fetchall()
    require(rows, 'no management session row to inspect')
    # Several logins can be live at once, so follow the row the browser holds:
    # the cookie value is the token and the row is keyed by its sha256.
    cookie = [c for c in page.context.cookies() if c['name'] == 'gpuworks_session'][0]['value']
    mine = [row for row in rows if row[0] == hashlib.sha256(cookie.encode()).hexdigest()]
    require(mine, 'the browser cookie has no matching session row')
    idle_before, absolute = mine[0][1], mine[0][2]
    result['session_rows'] = len(rows)
    key = mine[0][0]

    def session_deadline(column):
        with sqlite3.connect(owner.db) as db:
            value = db.execute(
                f"SELECT {column} FROM management_sessions WHERE token_hash = ?", (key,)).fetchone()
        return None if value is None else value[0]

    # A real read-only poll, the shape a background refresh uses.
    status, _ = owner.call('GET', '/api/v1/settings')
    require(status == 200, 'read-only poll did not succeed')
    after_poll = session_deadline('expires_at')
    require(after_poll == idle_before,
            'a read-only poll extended the idle deadline, so background refresh keeps a login alive: '
            + str((idle_before, after_poll)))

    # Only a human gesture renews: the page probes /auth/activity on pointer and
    # key events, and that POST is the one call that slides the deadline. A
    # background read must not, which is the whole point of this item.
    cookies = [c for c in page.context.cookies() if c['name'] == 'gpuworks_session']
    require(cookies, 'the browser holds no session cookie to check renewal with')
    idle_before_gesture = session_deadline('expires_at')
    probes = []
    page.on('request', lambda request: probes.append(urlsplit(request.url).path))
    state = page.evaluate("() => ({visibility: document.visibilityState, url: location.pathname})")
    result['activity_diagnostics'] = state
    page.wait_for_timeout(62000)
    page.mouse.move(420, 320)
    page.mouse.click(420, 320)
    deadline = time.monotonic() + 30
    idle_after_gesture = idle_before_gesture
    while time.monotonic() < deadline:
        idle_after_gesture = session_deadline('expires_at')
        if idle_after_gesture != idle_before_gesture:
            break
        page.wait_for_timeout(200)
    result['activity_probes'] = [p for p in probes if 'auth' in p]
    require(idle_after_gesture != idle_before_gesture,
            'a real human gesture did not slide the idle deadline, so the page cannot keep a login alive: '
            + json.dumps({'state': state, 'probes': [p for p in probes if 'auth' in p]}))
    require(idle_after_gesture <= absolute,
            'the slid idle deadline passed the absolute deadline: ' + str((idle_after_gesture, absolute)))
    # A pure background read leaves the deadline alone.
    settled = session_deadline('expires_at')
    read_status = page.evaluate("""async () => {
      const response = await fetch('/api/v1/settings', {credentials: 'include'});
      return response.status;
    }""")
    require(read_status == 200, 'the browser session read failed')
    idle_after_read = session_deadline('expires_at')
    require(idle_after_read == settled,
            'a background read slid the idle deadline, so polling alone keeps a login alive')
    result['session_renewal'] = {'idle_before_poll': str(idle_before), 'idle_after_poll': str(after_poll),
                                 'idle_after_gesture': str(idle_after_gesture),
                                 'idle_after_read': str(idle_after_read),
                                 'absolute': str(absolute)}

    # An unsaved edit has to survive: the product asks for the password in place
    # and keeps the operator's work, rather than throwing the page away.
    settings_text = labels(locale, 'settings')
    auth_text = labels(locale, 'auth')
    page.goto(owner.server.base + '/settings')
    access_tab = page.get_by_role('tab', name=settings_text['page']['tabs']['access'], exact=True)
    expect(access_tab).to_be_visible(timeout=30000)
    access_tab.click()
    card = page.locator('.ant-card').filter(has_text=settings_text['terminal']['title']).first
    grace = card.locator('.ant-form-item').filter(
        has=page.get_by_text(settings_text['terminal']['grace'], exact=True)).get_by_role('spinbutton')
    expect(grace).to_be_visible(timeout=30000)
    draft_value = '9'
    # The field can ignore the first keystroke while the form is still binding,
    # so retype until the visible value is the draft, then commit it.
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        grace.fill('')
        grace.fill(draft_value)
        grace.press('Tab')
        if grace.input_value() == draft_value:
            break
        page.wait_for_timeout(200)
    require(grace.input_value() == draft_value,
            'the draft value was never accepted: ' + repr(grace.input_value()))
    draft_url = page.url

    # Expire the live session, then let a real gesture reach the activity probe.
    past = '2000-01-01 00:00:00.000000000+00:00'
    with sqlite3.connect(owner.db) as db:
        db.execute("UPDATE management_sessions SET expires_at = ?, absolute_expires_at = ? WHERE token_hash = ?",
                   (past, past, key))
        db.commit()
    expiry_probes = []
    page.on('response', lambda response: expiry_probes.append(
        [urlsplit(response.url).path, response.status])
        if urlsplit(response.url).path == '/api/v1/auth/activity' else None)
    page.wait_for_timeout(62000)
    page.mouse.move(420, 320)
    page.mouse.click(420, 320)
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline and not any(p[1] == 401 for p in expiry_probes):
        page.wait_for_timeout(100)
    require(any(p[1] == 401 for p in expiry_probes),
            'the activity probe did not report the expired session: ' + json.dumps(expiry_probes))

    # The expiry dialog appears over the same page, with the form still behind it.
    modal = page.get_by_role('dialog', name=auth_text['sessionExpiredTitle'], exact=True)
    expect(modal).to_be_visible(timeout=20000)
    require(urlsplit(page.url).path == urlsplit(draft_url).path,
            'the expired session navigated away instead of asking in place: ' + page.url)
    check(not page.get_by_role('button', name='Close').count(),
          'the expiry dialog can be dismissed')
    modal.get_by_role('textbox', name=auth_text['username'], exact=True).fill('admin')
    modal.get_by_label(auth_text['password'], exact=True).fill(ADMIN_PASSWORD)
    button(modal, auth_text['submit']).click()
    expect(modal).not_to_be_visible(timeout=30000)
    require(urlsplit(page.url).path == urlsplit(draft_url).path,
            're-authentication did not return to the same page: ' + page.url)
    fresh = [c for c in page.context.cookies() if c['name'] == 'gpuworks_session']
    require(fresh and fresh[0]['value'] != cookie, 're-authentication did not issue a new session')
    require(grace.input_value() == draft_value,
            'the unsaved draft was lost across the expiry: ' + repr(grace.input_value()))
    result['expiry_in_place'] = {'url': page.url, 'draft_kept': grace.input_value(),
                                 'probes': expiry_probes}


def exercise(parent, server, artifacts, report, locale, first, agent, agent_bin, token):
    result = report['host_gates'] = {'status': 'NOT_RUN', 'candidate_items': ['19.3', '19.5', '19.6', '22.1', '22.3', '22.5', '22.6', '22.7'],
        'scope': 'real Linux host PTY and navigation only; no container/GPU/runtime claim', 'coverage_credit': 0,
        'gaps': ['22.5: server preflight reasons remain English in zh; no generic translated empty-target screen',
                 'each invocation covers only its selected locale; final en+zh and scoped product mutants required']}
    owner = Owner(server, token, artifacts, result, first, agent, agent_bin)
    sessions, primary = None, None
    try:
        owner.prepare(report['run_id'])
        context = context_for(owner, parent, locale)
        page = context.new_page()
        owner.pages.append(page)
        sessions = Sessions(owner, context, locale)
        set_policy(owner, page, locale, dict(POLICY))
        keyboard_focus(owner, page, locale)
        row, text = node_row(page, owner, owner.first, locale)
        popup = sessions.connected(sessions.popup(page, row, text['action']['terminal'], owner.first))
        sessions.utf8_resize(popup)
        sessions.send(popup, 'exit')
        expect(popup['page'].get_by_text(TEXT[locale]['exited'], exact=True)).to_be_visible()
        expect(popup['page'].locator('.xterm-rows')).to_contain_text(TEXT[locale]['ended'])
        sessions.released(popup, 'shell_exited')
        popup['page'].close()
        # Deep-link and actual reload must establish distinct audited PTYs on
        # the same declared target. Reload is never treated as a SPA200 success.
        deep = sessions.connected(sessions.deep(owner.first))
        # Bind the new document's events before reload, retaining the old audit.
        refreshed = sessions.observe(deep['page'], owner.first, wait_for_document=True)
        deep['page'].reload()
        sessions.released(deep, 'client_closed')
        sessions.connected(refreshed)
        refreshed['page'].close()
        sessions.released(refreshed, 'client_closed')
        disconnected_context = context_for(owner, parent, locale)
        disconnected = sessions.connected(sessions.deep(owner.first, disconnected_context))
        disconnected_context.close()
        sessions.released(disconnected, 'client_closed')
        result['context_disconnect_released'] = True
        # One slot per node, two total: each guard gets a distinct real denial.
        a = sessions.connected(sessions.deep(owner.first))
        denied = sessions.deep(owner.first)
        sessions.denied(denied, NODE_BUSY)
        sessions.upgrade_denied(context, owner.first['uuid'], 429, NODE_BUSY)
        denied['page'].close()
        b = sessions.connected(sessions.deep(owner.node))
        denied = sessions.deep(owner.node)
        sessions.denied(denied, GLOBAL_BUSY)
        sessions.upgrade_denied(context, owner.node['uuid'], 429, GLOBAL_BUSY)
        denied['page'].close()
        a['page'].close(); sessions.released(a, 'client_closed')
        c = sessions.connected(sessions.deep(owner.first))
        c['page'].close(); sessions.released(c, 'client_closed')
        b['page'].close(); sessions.released(b, 'client_closed')
        idle = sessions.connected(sessions.deep(owner.first))
        expect(idle['page'].get_by_text(TEXT[locale]['exited'], exact=True)).to_be_visible(timeout=95000)
        sessions.released(idle, 'idle_timeout', timeout=15)
        exits = [x for x in idle['frames'].controls if x['type'] == 'exit']
        require(len(exits) == 1 and exits[0]['reason'] == 'idle timeout' and exits[0]['code'] == -1,
                'real idle exit frame differs')
        require(owner.active(owner.first['uuid'])['active_total'] == 0, 'idle left hidden host sessions')
        require(owner.report['audits'][-1].get('duration_sec', 0) >= 60, 'idle ended before the configured real minute')
        idle['page'].close()
        revived = sessions.connected(sessions.deep(owner.first))
        sessions.send(revived, 'exit')
        sessions.released(revived, 'shell_exited')
        revived['page'].close()
        permissions(owner, sessions, context, parent, locale)
        missing = {'uuid': 'e2e-' + report['run_id'] + '-missing', 'hostname': 'unused'}
        sessions.expected_http.add(('/api/v1/nodes/' + missing['uuid'], 404))
        unknown = sessions.deep(missing)
        sessions.denied(unknown, OFFLINE)
        unknown['page'].close()
        result['unknown_target'] = {'node_detail': 404, 'preflight': 200, 'can_open': False,
                                    'reason': OFFLINE, 'websocket_count': 0}
        # Runs last: expiring the session deliberately makes later calls 401, so
        # nothing after this point may depend on the login.
        session_expiry(owner, page, locale, result)
        result['status'] = 'PASS'
        report['last_step'] = 'host-gates-real-PTY-policy-audit-navigation-complete'
    except Exception as error:
        primary = error
        result['status'] = 'FAIL'
        result['primary_error'] = artifacts.redact(type(error).__name__ + ': ' + str(error))
        if sessions and sessions.last_page and not sessions.last_page.is_closed():
            try:
                saved = artifacts.capture_failure(sessions.last_page, getattr(owner, 'sensitive', False))
                if saved == 'failure.png':
                    (artifacts.directory / saved).replace(artifacts.directory / 'host-gates-failure.png')
                (artifacts.directory / 'host-gates-failure.aria.txt').write_text(artifacts.redact(sessions.last_page.locator('body').aria_snapshot()))
            except Exception as capture_error:
                result['capture_error'] = artifacts.redact(str(capture_error))
        raise
    finally:
        clean = owner.cleanup()
        if not clean:
            result['status'] = 'FAIL'
            if primary is None:
                raise AssertionError('host gate cleanup did not complete; scoped identities retained')
