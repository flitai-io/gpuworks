"""Independent UI checks for ordinary replica edits and restart commands."""
import re
import sys
import time
from urllib.parse import urlsplit, parse_qs
from playwright.sync_api import expect
from local_browser_fixture import http
from platform_token_read_revoke import button, check
import deployment_replica_fixture as fixture

def button_name(label):
    """The accessible-name matcher antd actually renders for this button label.

    Reuses the rule already captured in platform_token_read_revoke.button: antd's Button
    defaults autoInsertSpace to true, so a two-character Chinese label renders with a space
    between the characters ("应用" becomes "应 用") and an exact string match finds nothing
    under zh. The pattern is anchored and each character is escaped, so it stays exactly as
    strict as the exact match it replaces; non-matching labels are returned unchanged.
    """
    accessible = r"\s*".join(label) if re.fullmatch(r"[\u3400-\u9fff]{2}", label) else re.escape(label)
    return re.compile("^" + accessible + "$")


FAMILIES = ('all', 'first-open', 'cancel', 'numeric', 'keyboard', 'notice', 'navigation', 'drain')


def active(report, family):
    return report.get('check_family', 'all') in ('all', family)


def integer(value, expected, label):
    check(type(value) is int and value == expected,
          f'{label}: expected JSON integer {expected!r}, actual {value!r}')


def stable(document):
    return {key: document.get(key) for key in (
        'id', 'name', 'model_id', 'served_name', 'engine', 'image', 'replicas', 'routing_policy')}


def get_document(server, token, ident):
    status, document = http(server.base, 'GET', f'/api/v1/deployments/{ident}', token)
    check(status == 200, f'independent replica GET HTTP {status}')
    return document


def bounds(box, viewport, label):
    check(box is not None, label + ' has no layout box')
    check(box['width'] > 0 and box['height'] > 0 and box['x'] >= 0 and box['y'] >= 0
          and box['x'] + box['width'] <= viewport['width'] + 1
          and box['y'] + box['height'] <= viewport['height'] + 1,
          f'{label} outside viewport: {box}')


def layout(page, drawer, spin, apply, report):
    report['last_step'] = 'replica-scale-drawer-layout'
    boxes = {name: control.bounding_box() for name, control in (
        ('title', drawer.locator('.ant-drawer-title')), ('count', spin), ('apply', apply),
        ('close', drawer.locator('.ant-drawer-close')))}
    for index, control in enumerate(drawer.locator('.ant-drawer-extra button').all()):
        boxes['header-action-' + str(index)] = control.bounding_box()
    for name, box in boxes.items():
        bounds(box, page.viewport_size, name)
    check(boxes['title']['y'] + boxes['title']['height'] <= boxes['count']['y'],
          'drawer title overlaps replica editor')
    check(boxes['count']['x'] + boxes['count']['width'] <= boxes['apply']['x'] + 1,
          'replica count overlaps Apply')
    report['replica_editor_layout'] = boxes



def _release_held_reads(page, pattern, handler, held):
    """Release every intercepted read before removing its handler."""
    errors = []
    while held:
        route = held.pop(0)
        try:
            route.continue_()
        except Exception as error:
            errors.append(error)
    try:
        page.unroute(pattern, handler)
    except Exception as error:
        errors.append(error)
    if errors:
        raise ExceptionGroup('could not release replica detail reads', errors)


def first_open(page, server, token, data, labels, report, requests, expected_count):
    """Hold the real detail read; no response is invented or replayed as UI traffic."""
    ident = data['deployment']['id']
    path = f'/api/v1/deployments/{ident}'
    if active(report, 'navigation'):
        report['last_step'] = 'replica-navigation-entry'
        # A real menu entry and the row action are different UI entry paths.
        page.goto(server.base + '/')
        link = page.locator('[data-slot=sidebar]').get_by_role('link', name=labels['_navigation'], exact=True)
        link.click()
        expect(page).to_have_url(server.base + '/deployments')
        expect(page.get_by_role('heading', name=labels['page']['title'], exact=True)).to_be_visible()
        expect(link).to_have_attribute('aria-current', 'page')
        report['replica_navigation'] = {'menu_path': '/deployments', 'selected': True}
    if not active(report, 'first-open'):
        return
    report['last_step'] = 'replica-detail-loading-no-submit'
    held = []
    releasing = False
    registered = True
    def hold(route):
        if not releasing and route.request.method == 'GET' and urlsplit(route.request.url).path == path:
            held.append(route)
        else:
            route.continue_()
    pattern = re.compile(re.escape(path) + r'(?:\?|$)')
    page.route(pattern, hold)
    try:
        page.goto(server.base + f'/deployments?deployment={ident}')
        deadline = time.monotonic() + 10
        while not held and time.monotonic() < deadline:
            page.wait_for_timeout(50)
        check(bool(held), 'browser did not request selected deployment detail')
        expect(page.get_by_role('spinbutton', name=labels['detail']['replicas'], exact=True)).to_have_count(0)
        expect(page.get_by_role('button', name=button_name(labels['replica']['apply']))).to_have_count(0)
        with page.expect_response(lambda response: response.request.method == 'GET'
                                  and urlsplit(response.url).path == path) as read:
            releasing = True
            registered = False
            _release_held_reads(page, pattern, hold, held)
        check(read.value.status == 200, 'first detail read failed')
        body = read.value.json()
        integer(body.get('id'), ident, 'detail identity')
        integer(body.get('replicas'), expected_count, 'first-open replica count')
        check(body.get('name') == data['body']['name'], 'first-open deployment name differs')
        drawer = page.get_by_role('dialog', name=data['body']['name'], exact=True)
        expect(drawer.get_by_role('spinbutton', name=labels['detail']['replicas'], exact=True)).to_have_value(str(expected_count))
        expect(drawer.get_by_role('button', name=button_name(labels['replica']['apply']))).to_be_disabled()
        integer(get_document(server, token, ident).get('replicas'), expected_count, 'independent first-open count')
        check(requests == [], 'opening detail submitted an incomplete mutation')
        report['replica_first_open'] = {'request': {'method': 'GET', 'path': path},
            'loading_submit_controls': 0, 'id': ident, 'name': data['body']['name'], 'replicas': expected_count}
    finally:
        if registered:
            primary = sys.exc_info()[1]
            releasing = True
            try:
                _release_held_reads(page, pattern, hold, held)
            except Exception as cleanup:
                report.setdefault('replica_route_cleanup_errors', []).append(repr(cleanup))
                if primary is None:
                    raise
                # The failing business assertion stays authoritative; cleanup
                # failure is attached and recorded instead of replacing it.
                primary.add_note('Replica detail read cleanup failed: ' + repr(cleanup))


def navigation_reentry(page, drawer, server, data, labels, report, expected_count):
    if not active(report, 'navigation'):
        return drawer
    report['last_step'] = 'replica-deep-link-identity'
    ident = data['deployment']['id']
    expect(page).to_have_url(server.base + f'/deployments?deployment={ident}')
    check(parse_qs(urlsplit(page.url).query) == {'deployment': [str(ident)]}, 'row opened another deployment')
    page.reload()
    drawer = page.get_by_role('dialog', name=data['body']['name'], exact=True)
    expect(drawer).to_be_visible()
    expect(drawer.get_by_role('spinbutton', name=labels['detail']['replicas'], exact=True)).to_have_value(str(expected_count))
    report.setdefault('replica_navigation', {}).update(deep_link=page.url, reload_same_identity=True)
    return drawer


def count_editor(page, drawer, server, token, data, labels, report, requests, reopen, expected_count):
    spin = drawer.get_by_role('spinbutton', name=labels['detail']['replicas'], exact=True)
    apply = drawer.get_by_role('button', name=button_name(labels['replica']['apply']))
    before = stable(get_document(server, token, data['deployment']['id']))
    integer(before['replicas'], expected_count, 'saved editor count')
    if active(report, 'numeric'):
        report['last_step'] = 'replica-count-metadata'
        # The ceiling is a product constant (planning.MaxReplicas), not a test fact.
        # Pinning 32 here went red the day the server raised it to 1024 and said
        # nothing about whether the control agrees with the server - which is the
        # only thing this line is for. Read the server's number, then require the
        # control to publish the same one.
        ceiling = get_document(server, token, data['deployment']['id']).get('max_replicas')
        check(type(ceiling) is int and ceiling >= 1,
              f'server replica range is not a usable JSON integer: {ceiling!r}')
        expect(spin).to_have_attribute('aria-valuemin', '1')
        expect(spin).to_have_attribute('aria-valuemax', str(ceiling))
        expect(spin).to_have_value(str(expected_count))
        spin.press('ArrowUp')
        expect(spin).to_have_value(str(expected_count + 1))
        spin.press('ArrowDown')
        expect(spin).to_have_value(str(expected_count))
        spin.fill('')
        expect(apply).to_be_disabled()
        check(requests == [], 'empty replica draft submitted a mutation')
        spin.fill(str(expected_count))
        expect(apply).to_be_disabled()
        report['replica_count_metadata'] = {'saved': expected_count, 'min': 1, 'max': 32, 'step': 1,
            'unit': 'replicas', 'empty_submit_disabled': True}
    if active(report, 'keyboard'):
        report['last_step'] = 'replica-keyboard-no-implicit-order'
        spin.fill(str(expected_count + 1))
        spin.focus()
        spin.press('Enter')
        expect(spin).to_be_focused()
        page.keyboard.press('Tab')
        check(drawer.evaluate('element => element.contains(document.activeElement)'), 'Tab left detail editor')
        check(requests == [], 'Enter implicitly submitted replica draft')
        spin.fill(str(expected_count))
        layout(page, drawer, spin, apply, report)
    if active(report, 'cancel'):
        report['last_step'] = 'replica-draft-cancel-no-write'
        observed = []
        for dismissal in ('close', 'escape'):
            spin = drawer.get_by_role('spinbutton', name=labels['detail']['replicas'], exact=True)
            spin.fill(str(expected_count + 1))
            expect(drawer.get_by_role('button', name=button_name(labels['replica']['apply']))).to_be_enabled()
            if dismissal == 'close':
                drawer.locator('.ant-drawer-close').click()
            else:
                page.keyboard.press('Escape')
            expect(drawer).to_be_hidden()
            check(requests == [], 'discarding replica draft wrote to server')
            check(stable(get_document(server, token, data['deployment']['id'])) == before,
                  'discarding replica draft changed persisted deployment configuration')
            drawer = reopen()
            expect(drawer.get_by_role('spinbutton', name=labels['detail']['replicas'], exact=True)).to_have_value(str(expected_count))
            expect(drawer.get_by_role('button', name=button_name(labels['replica']['apply']))).to_be_disabled()
            observed.append(dismissal)
        report['replica_draft_cancel'] = {'dismissals': observed, 'cancel_button': 'not provided by inline editor',
            'mutation_count': 0, 'saved_config': before, 'reopened_count': expected_count}
    return drawer


def numeric_wire(report, wire, expected):
    if not active(report, 'numeric'):
        return
    report['last_step'] = 'replica-count-wire-type'
    integer(wire['body'].get('replicas'), expected, 'browser replica request')
    report.setdefault('replica_numeric_wire', []).append(wire)


def persisted_count(report, document, expected):
    if not active(report, 'numeric'):
        return
    report['last_step'] = 'replica-count-persisted-runtime'
    integer(document.get('replicas'), expected, 'persisted desired count')
    integer(document.get('ready_replicas'), expected, 'persisted ready count')
    current = fixture.current(document)
    check(len(current) == expected and len({item['container_id'] for item in current}) == expected,
          'saved replica count differs from unique real container identities')
    report.setdefault('replica_numeric_persistence', []).append({'desired': expected, 'ready': expected,
        'containers': sorted(item['container_id'] for item in current)})


def success(page, report, text):
    if active(report, 'notice'):
        report['last_step'] = 'replica-notice-success'
        notice = page.locator('.ant-message-success').filter(has_text=text)
        expect(notice).to_have_count(1)
        expect(notice.get_by_text(text, exact=True)).to_be_visible()
        report.setdefault('replica_success_notices', []).append(text)


def drain_control(page, modal, checkbox, labels, report, requests):
    if active(report, 'drain'):
        report['last_step'] = 'replica-drain-control-contract'
        expect(checkbox).to_be_checked()
        expect(checkbox).to_be_enabled()
        expect(checkbox).to_have_accessible_name(labels['replica']['drain'])
        report['replica_drain_default'] = {'checked': True, 'enabled': True, 'name': labels['replica']['drain']}
    if active(report, 'keyboard'):
        report['last_step'] = 'replica-restart-keyboard-no-implicit-order'
        checkbox.focus()
        checkbox.press('Enter')
        expect(checkbox).to_be_focused()
        page.keyboard.press('Tab')
        check(modal.evaluate('element => element.contains(document.activeElement)'), 'Tab left restart modal')
        check(requests == [], 'Enter on drain implicitly submitted restart')
