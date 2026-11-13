"""6.5: preserve the complete boolean draft through each real save rejection.

Three explicit UI actions seed pending state, including an owned mount row.
All other expected state comes
from the immutable fixture and reviewed v2 field contract, never a DOM/request
snapshot. Existing bad-name or expired-provider400 and sole201 remain the only
create attempts; original persistence/runtime/cleanup oracles are unchanged.
"""
import copy
from contextlib import contextmanager
import json
from pathlib import Path, PurePosixPath
import re
from urllib.parse import urljoin, urlsplit

from playwright.sync_api import expect
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import check
from deployment_batch_checks import rejected_notice
from deployment_boolean_contracts import evaluate_body, reviewed_profile, same
from deployment_boolean_inventory_contracts import (
    CARRIED_MARK, SLEEP_SUPPORTED, expected_ids, fixture_hardware, health_contract, rows,
)
import deployment_boolean_inventory as inventory

FAMILY = 'boolean-rejection'
CACHE, PREFIX = 'enable_cache_report', 'enable_prefix_caching'
INVALID_NAME = 'x' * 257
NAME_DETAIL = 'must contain valid text without control characters and at most 256 characters'
MOUNT_TARGET = '/e2e-pending-ro'


def declared_mount(host_path):
    check(type(host_path) is str and host_path.startswith('/') and host_path != '/',
          '6.5 pending mount needs an independently declared absolute host path')
    check(str(PurePosixPath(host_path)) == host_path and '..' not in PurePosixPath(host_path).parts,
          '6.5 pending mount path must be normalized')
    return {'host_path': host_path, 'container_path': MOUNT_TARGET,
            'purpose': 'data', 'read_only': True}


def mount_contract(body, pending_mount):
    same(pending_mount, declared_mount(pending_mount['host_path']), '6.5 fixed pending mount literal')
    existing = body['container_overrides'].get('extra_mounts', [])
    check(type(existing) is list and len(existing) in (0, 2), '6.5 unknown fixture mount count')
    for mount in existing:
        check(type(mount.get('read_only')) is bool, '6.5 original mount read_only must be literal')
        check(mount.get('purpose') == 'data', '6.5 original mount purpose must remain data')
        target = mount.get('container_path')
        check(target in ('/e2e-ro', '/e2e-rw'), '6.5 unreviewed original mount target')
    same([mount['container_path'] for mount in existing],
         ['/e2e-ro', '/e2e-rw'] if existing else [], '6.5 original mount order')
    reserved = ['/model', '/e2e-ro', '/e2e-rw']
    check(all(MOUNT_TARGET != target and not MOUNT_TARGET.startswith(target + '/')
              and not target.startswith(MOUNT_TARGET + '/') for target in reserved),
          '6.5 pending mount collides with a reserved target')
    return len(existing)


def _pending(report):
    check('pending_mount' in report.get('boolean_rejection', {}), '6.5 seed must declare pending mount first')
    return report['boolean_rejection']['pending_mount']


def enabled(report):
    return report.get('deployment_check_family', 'all') in ('all', FAMILY)


def plan(body):
    params = body['quick_params']
    check(CACHE in params and type(params[CACHE]) is bool,
          '6.5 requires a literal fixture cache boolean; its two-position UI cannot restore original omission')
    check(PREFIX not in params or type(params[PREFIX]) is bool,
          '6.5 prefix baseline must be literally true/false or omitted, not null/coerced')
    return {'seed': {CACHE: 'off', PREFIX: 'on'},
            'restore': {CACHE: 'on' if params[CACHE] else 'off',
                PREFIX: 'auto' if PREFIX not in params else 'on' if params[PREFIX] else 'off'},
            'baseline_prefix_wire': 'omitted' if PREFIX not in params else params[PREFIX],
            'baseline_cache_wire': params[CACHE]}


# Values the form itself pinned that no evaluation reports: entering manual command
# mode rewrites dtype in the form (ADR 0020 rule 6) without sending it as an
# evaluation, so the create body is the first place it surfaces. Keyed by run id
# so a stale entry cannot leak into another run.
form_pinned = {}


def seeded_body(body, pending_mount, report=None):
    """The create body this case submits: the fixture, seeded, plus anything
    this session already made explicit.

    Creating submits the whole displayed configuration (ADR 0020 rule 3), so it
    carries the values earlier steps pinned - entering manual command mode fixes
    dtype to the concrete value the page showed (rule 6) and leaving text mode
    does not restore "auto". Reading them from the live wire keeps every caller
    (create assertions and refusal checks alike) on the same description.
    """
    plan(body)
    mount_contract(body, pending_mount)
    result = copy.deepcopy(body)
    if report is not None:
        # Carried values first, then the seed: the seed is what this case set on
        # the controls, so it has to win over anything the warm read reported.
        result['quick_params'].update(_carried_quick(report))
        form_dtype = form_pinned.get(report.get('run_id'))
        if form_dtype:
            result['quick_params']['dtype'] = form_dtype
    result['quick_params'].update({CACHE: False, PREFIX: True})
    result['container_overrides']['extra_mounts'] = [
        *copy.deepcopy(body['container_overrides'].get('extra_mounts', [])), copy.deepcopy(pending_mount)]
    return result


def expected_bad_body(body, report):
    if not enabled(report):
        return copy.deepcopy(body)
    return seeded_body(body, _pending(report), report)



def restored_quick(report):
    """The quick values the form holds after the restore checkpoint.

    The restore checkpoint puts CACHE/PREFIX back to the fixture's own values and
    a later create submits them, because a stated value stays stated (ADR 0020
    rule 3). Computed from the restore plan so it describes the values that were
    actually asked for rather than copying back whatever the wire happened to say.
    """
    body = (report.get('boolean_rejection') or {}).get('baseline')
    if not body:
        return {}
    values = plan(body)['restore']
    return {key: values[key] == 'on' for key in (CACHE, PREFIX)
            if values[key] != 'auto'}


def expected_mutations(original, report):
    expected = copy.deepcopy(original)
    if enabled(report):
        check(expected and expected[0]['method'] == 'POST'
              and expected[0]['path'] == '/api/v1/deployments'
              and expected[0]['query'] == '' and expected[0]['body']['name'] == INVALID_NAME,
              '6.5 rejected-mutation binding must be the literal first invalid-name create')
        expected[0]['body'] = seeded_body(expected[0]['body'], _pending(report), report)
        # dtype is pinned in the form by entering manual command mode and every
        # later create submits it (ADR 0020 rules 3 and 6). A case can publish the
        # exact value it pinned; without one the fixture's own value stands.
        pinned = form_pinned.get(report.get('run_id'))
        if pinned:
            for mutation in expected:
                body = mutation.get('body')
                if isinstance(body, dict) and isinstance(body.get('quick_params'), dict):
                    body['quick_params']['dtype'] = pinned
    return expected


def refusal(body, kind, pending_mount, report=None):
    acquisition = 'E2E_ACQUISITION' in body['container_overrides']['env']
    same(kind, 'provider' if acquisition else 'name', '6.5 fixed fixture save-refusal trigger')
    bad = seeded_body(body, pending_mount, report)
    if kind == 'name':
        bad['name'] = INVALID_NAME
        error = {'code': 'FIELD_INVALID', 'error': 'name ' + NAME_DETAIL,
                 'params': {'field': 'name', 'detail': NAME_DETAIL}}
    else:
        error = {'code': 'ACQUISITION_CANDIDATE_STALE',
                 'error': 'selected provider is not declared by this spec',
                 'params': {'provider': body['model_source']['provider']}}
    return bad, error


def assert_refusal(record, body, kind, pending_mount, report=None):
    bad, error = refusal(body, kind, pending_mount, report)
    same(record, {'method': 'POST', 'path': '/api/v1/deployments', 'query': '',
                  'body': bad, 'status': 400, 'response': error},
         '6.5 exact real refusal method/path/query/body/status/error')


def pending_ids(engine):
    # The conditional row is now on this original draft, across the actual400.
    return expected_ids(engine)


def complete_pending(records, engine):
    keys = [row['id'] for row in records]
    check(len(keys) == len(set(keys)), '6.5 duplicate pending boolean observations')
    same(sorted(keys), sorted(pending_ids(engine)), '6.5 complete pending boolean matrix')
    for row in records:
        check(type(row.get('offered')) is bool, '6.5 field has no offered observation')
        if not row['offered']:
            check(bool(row.get('source_reason')), '6.5 absent field has no independent source reason')


def _at(report, step, action):
    try:
        return action()
    finally:
        report['last_step'] = step


def _common(report):
    return json.loads((ROOT / f'ui/src/i18n/locales/{report["locale"]}/common.json').read_text())


def _mount_fields(page, labels, body, report, phase, surface):
    expected = body['container_overrides'].get('extra_mounts', [])
    mounts = page.locator('.image-defaults-mount-row')
    expect(mounts).to_have_count(len(expected))
    for index, literal in enumerate(expected):
        row = mounts.nth(index)
        expect(row.get_by_placeholder(labels['containerOverrides']['hostPathPlaceholder'], exact=True)).to_have_value(literal['host_path'])
        expect(row.get_by_placeholder(labels['containerOverrides']['containerPathPlaceholder'], exact=True)).to_have_value(literal['container_path'])
        expect(row.get_by_role('combobox')).to_have_count(1)
        expect(row.locator('.ant-select-selection-item')).to_have_text(labels['containerOverrides']['purpose']['data'])
        checkbox = row.get_by_role('checkbox', name=labels['containerOverrides']['readOnly'], exact=True)
        expect(checkbox).to_be_visible()
        expect(checkbox).to_be_enabled()
        expect(checkbox).to_be_checked() if literal['read_only'] else expect(checkbox).not_to_be_checked()
    pending = _pending(report)
    count = mount_contract(report['boolean_rejection']['baseline'], pending)
    active = phase != 'original-restored'
    same(expected, ([*report['boolean_rejection']['baseline']['container_overrides'].get('extra_mounts', []), pending]
                    if active else report['boolean_rejection']['baseline']['container_overrides'].get('extra_mounts', [])),
         '6.5 complete independently declared ' + surface + ' mount rows')
    return {'surface': surface, 'owned_index': count, 'present': active,
            'literal_rows': copy.deepcopy(expected)}


def _append_mount(page, labels, body, report):
    pending = _pending(report)
    index = mount_contract(body, pending)
    mounts = page.locator('.image-defaults-mount-row')
    expect(mounts).to_have_count(index)
    page.get_by_role('button', name=labels['containerOverrides']['addMount'], exact=True).click()
    expect(mounts).to_have_count(index + 1)
    row = mounts.nth(index)
    checkbox = row.get_by_role('checkbox', name=labels['containerOverrides']['readOnly'], exact=True)
    expect(checkbox).to_be_visible()
    expect(checkbox).to_be_enabled()
    expect(checkbox).not_to_be_checked()
    row.get_by_placeholder(labels['containerOverrides']['hostPathPlaceholder'], exact=True).fill(pending['host_path'])
    row.get_by_placeholder(labels['containerOverrides']['containerPathPlaceholder'], exact=True).fill(pending['container_path'])
    expect(row.locator('.ant-select-selection-item')).to_have_text(labels['containerOverrides']['purpose']['data'])
    checkbox.check()
    expect(checkbox).to_be_checked()
    report['boolean_rejection']['mount_seed'] = {'owned_index': index, 'initial_read_only': False,
        'pending': copy.deepcopy(pending), 'native_checkbox_action': 'check'}


def _remove_mount(page, labels, body, report):
    index = mount_contract(body, _pending(report))
    mounts = page.locator('.image-defaults-mount-row')
    expect(mounts).to_have_count(index + 1)
    row = mounts.nth(index)
    expect(row.get_by_placeholder(labels['containerOverrides']['containerPathPlaceholder'], exact=True)).to_have_value(MOUNT_TARGET)
    row.get_by_role('button', name=labels['containerOverrides']['deleteMountAria'], exact=True).click()
    expect(mounts).to_have_count(index)
    report['boolean_rejection']['mount_removed'] = {'owned_index': index, 'remaining_count': index}


@contextmanager
def expiry_guard(page, report):
    """Observe the entire existing remove/create400/restore finally window."""
    if not enabled(report):
        yield
        return
    requests = []
    def seen(request):
        if request.method == 'POST' and urlsplit(request.url).path == '/api/v1/evaluate':
            requests.append({'method': request.method, 'path': urlsplit(request.url).path,
                             'body': request.post_data_json})
    page.on('request', seen)
    primary = None
    try:
        yield
    except BaseException as error:
        primary = error
        raise
    finally:
        page.remove_listener('request', seen)
        report['boolean_rejection']['expired_window_evaluations'] = requests
        if primary is None:
            check(not requests, '6.5 mode/read observations emitted evaluate while provider expired')


def _preconditions(page, body, report, token, evidence):
    base = urljoin(page.url, '/').rstrip('/')
    response = page.request.get(base + '/api/v1/engines')
    check(response.status == 200, '6.5 dictionary GET failed')
    profile = next(row for row in response.json()['items'] if row['name'] == body['engine'])
    reviewed_profile(profile)
    same(profile['sleep']['supported'], SLEEP_SUPPORTED[body['engine']], '6.5 fixed engine capability')
    observed = {}
    for name, path in [('model', '/api/v1/models/' + str(body['model_id'])),
                       ('template', '/api/v1/inference-templates/model/' + report['owned_template_model_id']),
                       ('node', '/api/v1/nodes/' + body['node_uuid'])]:
        status, document = http(base, 'GET', path, token=token)
        check(status == 200, '6.5 independent ' + name + ' GET failed')
        observed[name] = document
    evidence['health_source'] = health_contract(body, observed['model'], observed['template'])
    fixture_hardware(observed['node'], body['node_uuid'])
    evidence['fixed_profile'] = {'name': profile['name'], 'sleep': profile['sleep']}
    evidence['registered_model_operations'] = observed['model']['operations']


def _matrix(page, labels, body, report, token, phase):
    """Read state across real surfaces; deliberately NEVER evaluate here.

    Provider expiry is still active during the retained checkpoint. The owned
    row was seeded before expiry. No field changes or API mocks occur here.
    """
    import deployment_launch_checks as launch
    # The page shows the values it is actually carrying, which include every key
    # this session edited (ADR 0020 rule 3). seeded_body describes the create
    # body - where the whole displayed set is submitted - so reading it directly
    # would expect the fixture's pristine values and call a correctly retained
    # control a defect. Mirror the live state onto the inspection copy so
    # initial() judges the controls by what they show.
    body = copy.deepcopy(body)
    carried = _carried_quick(report)
    if carried:
        body[CARRIED_MARK] = carried
    report['boolean_rejection'].setdefault('matrix_debug', []).append(
        {'phase': phase, 'carried': carried,
         'merged': {k: v for k, v in (carried or {}).items()
                    if k in ('enable_prefix_caching', 'enable_cache_report')}})
    evidence = {'records': [], 'gaps': [], 'phase': phase}
    report['boolean_rejection']['checkpoints'][phase] = evidence
    _preconditions(page, body, report, token, evidence)
    common = _common(report)
    launch.mode(page, labels, 'quick')
    _at(report, 'boolean-rejection-' + phase + '-static-quick',
        lambda: inventory._quick_static(page, labels, common, body, report, evidence))
    quick_mount = _at(report, 'boolean-rejection-' + phase + '-mount-read-only-quick',
        lambda: _mount_fields(page, labels, body, report, phase, 'quick'))
    header = page.locator('.deploy-advanced-collapse .ant-collapse-header')
    expect(header).to_have_count(1)
    opened = header.get_attribute('aria-expanded') == 'true'
    if not opened:
        header.click()
    for key in rows(body['engine']):
        _at(report, f'boolean-rejection-{phase}-{key}-quick',
            lambda key=key: inventory._dictionary(page, labels, body, report, 'quick', key, evidence))
    if not opened:
        header.click()
    launch.mode(page, labels, 'expert')
    for group in ('model', 'performance', 'performance_advanced'):
        keys = [key for key, row in rows(body['engine']).items() if row[2] == group]
        if not keys:
            continue
        page.get_by_role('tab', name=labels['page']['step']['model' if group == 'model' else 'performance'], exact=True).click()
        if group == 'performance':
            inventory._memory_tier(page, labels, body, evidence)
        if group == 'performance_advanced':
            header = page.locator('.deploy-expert-card .ant-collapse-header')
            expect(header).to_have_count(1)
            expect(header).to_have_text(labels['performance']['advanced'])
            if header.get_attribute('aria-expanded') != 'true':
                header.click()
        for key in keys:
            _at(report, f'boolean-rejection-{phase}-{key}-expert',
                lambda key=key: inventory._dictionary(page, labels, body, report, 'expert', key, evidence))
    _at(report, 'boolean-rejection-' + phase + '-static-expert',
        lambda: inventory._expert_static(page, labels, body, report, evidence))
    expert_mount = _at(report, 'boolean-rejection-' + phase + '-mount-read-only-expert',
        lambda: _mount_fields(page, labels, body, report, phase, 'expert'))
    conditional = {'id': 'conditional/mount-read-only', 'offered': phase != 'original-restored',
                   'quick': quick_mount, 'expert': expert_mount, 'independent_page': False}
    if phase == 'original-restored':
        conditional['source_reason'] = 'owned pending row removed through native Delete; original complete row set restored'
    evidence['records'].append(conditional)
    launch.mode(page, labels, 'quick')
    complete_pending(evidence['records'], body['engine'])
    check(not evidence['gaps'], '6.5 pending boolean contract gaps: ' + str(evidence['gaps']))


def _empty(page, labels, report, token, phase):
    expect(page.locator('.ant-message-success').filter(has_text=labels['action']['created'])).to_have_count(0)
    same(urlsplit(page.url).path, '/deploy', '6.5 failed save must retain the deployment draft route')
    status, document = http(urljoin(page.url, '/').rstrip('/'), 'GET', '/api/v1/deployments', token=token)
    check(status == 200 and document['items'] == [] and document['total'] == 0,
          '6.5 failed draft must not have created a deployment')
    report['boolean_rejection'].setdefault('empty_collection', {})[phase] = {
        'status': status, 'items': [], 'total': 0, 'false_success': False}


def _positions(page, labels, positions, body, report):
    import deployment_launch_checks as launch
    dictionary = rows(body['engine'])
    for key in (CACHE, PREFIX):
        row = dictionary[key]
        title = row[5] if report['locale'] == 'en' else row[6]
        launch.choose(page, page.get_by_role('combobox', name=title, exact=True),
                      labels['workbench']['triState' + {'on': 'On', 'off': 'Off', 'auto': 'Auto'}[positions[key]]])



def _carried_quick(report):
    """The quick values the page was already sending before this action.

    An edit stays explicit for the rest of the session (ADR 0020 rule 3), so the
    case that walked the inventory and the tristate controls earlier is still
    carrying those keys now. Reading them from the last real wire describes what
    the page actually does instead of guessing which controls were touched.
    """
    latest = {}
    for record in reversed(report.get('launch_evaluations') or []):
        request = record.get('request') or {}
        quick = request.get('quick_params')
        if isinstance(quick, dict):
            # The newest evaluation describes the current authored set, and the
            # dtype pinned by manual command mode is first seen here too - it never
            # appears in an evaluation that only touched a boolean, so a plain
            # "last request wins" would lose it after the first quick-mode step.
            if not latest:
                latest.update(quick)
            if 'dtype' in quick:
                latest.setdefault('dtype', quick['dtype'])
    return latest


def _wire(page, labels, body, report, phase, positions=None, mount_action=None):
    import deployment_launch_checks as launch
    route = page.get_by_role('textbox', name=labels['basic']['servedName'], exact=True)
    marker = body['served_name'] + '-boolean-rejection-' + phase
    def action():
        if positions is not None:
            _positions(page, labels, positions, body, report)
        if mount_action is not None:
            mount_action()
        route.fill(marker)
    expected = evaluate_body(body)
    expected['served_name'] = marker
    # The action sets exactly the two controls in _positions, so those two keys
    # are the authored ones and the only quick values the evaluation carries
    # (ADR 0020 rule 3). seeded_body still describes the create body, where the
    # complete displayed set is submitted, which is why body's other fixture
    # values must not appear here.
    # ADR 0020 rule 3 keeps a key explicit from the moment the operator edits it
    # until the page is replaced, so a case that walked the whole inventory
    # earlier in the same session still carries those keys here with the values
    # they were restored to. `authored` names them; this action adds CACHE and
    # PREFIX. Every other fixture value must stay absent, so an extra key is
    # still a failure.
    sent_quick = {}
    for key, value in _carried_quick(report).items():
        # dtype is pinned in the form but never travels through an evaluation -
        # EvaluateQuickParams only ever carries keys the operator authored there.
        # The create bodies below still describe it; an evaluate body must not.
        if key == 'dtype':
            continue
        sent_quick[key] = value
    if positions is not None:
        for key in (CACHE, PREFIX):
            # Auto states no value, and EvaluateQuickParams drops a key that has
            # no authored value rather than sending false - "off" and "unstated"
            # are different answers, which is the whole point of the tri-state.
            if positions[key] == 'auto':
                sent_quick.pop(key, None)
            else:
                sent_quick[key] = positions[key] == 'on'
    if sent_quick:
        expected['quick_params'] = sent_quick
    else:
        expected.pop('quick_params', None)
    # Seed from what the page is already sending: an earlier phase's edits stay
    # explicit (ADR 0020 rule 3) and re-deriving them here would only guess at
    # which controls this session touched. The action's own keys override theirs,
    # and the whole-object comparison below still rejects any key that neither
    # source named.
    observed = launch.evaluation(page, action, lambda wire: wire.get('served_name') == marker,
        report, 'boolean-rejection-' + phase + '-wire')
    same(observed['request'], expected, '6.5 complete independent ' + phase + ' evaluate wire')
    same(observed['response'].get('ok'), True, '6.5 corrected draft evaluate success')
    restored = launch.evaluation(page, lambda: route.fill(body['served_name']),
        lambda wire: wire.get('served_name') == body['served_name'], report,
        'boolean-rejection-' + phase + '-restore-route')
    # Restoring the served name does not unauthor the quick keys the phase set;
    # they travel with the values they hold (ADR 0020 rule 3), so the expected
    # body is the same one this phase just accepted.
    restoration = evaluate_body(body)
    if sent_quick:
        restoration['quick_params'] = sent_quick
    else:
        restoration.pop('quick_params', None)
    same(restored['request'], restoration, '6.5 complete ' + phase + ' route restoration')


def seed(page, labels, body, report, token, pending_mount, pending_storage_id=None):
    if not enabled(report):
        return
    mount_contract(body, pending_mount)
    check(Path(pending_mount['host_path']).is_dir(), '6.5 independently declared existing mount host directory missing')
    report['boolean_rejection'] = {'baseline': copy.deepcopy(body), 'plan': plan(body),
        'pending_mount': copy.deepcopy(pending_mount), 'checkpoints': {}}
    if 'E2E_ACQUISITION' in body['container_overrides']['env']:
        check(type(pending_storage_id) is int and pending_storage_id > 0, '6.5 acquisition requires owned store-b ID')
        status, storage = http(urljoin(page.url, '/').rstrip('/'), 'GET',
            '/api/v1/storages/' + str(pending_storage_id) + '?all_nodes=1', token=token)
        check(status == 200, '6.5 owned store-b independent GET failed')
        same(storage['path'], pending_mount['host_path'], '6.5 independently declared store-b path')
        nodes = storage['nodes'].get('all', [])
        check(len(nodes) == 1 and nodes[0]['node_uuid'] == body['node_uuid']
              and nodes[0]['state'] == 'ready' and nodes[0]['writable'] is True,
              '6.5 declared store-b must be ready and writable on owned eligible node')
        report['boolean_rejection']['pending_storage'] = {'id': pending_storage_id,
            'path': storage['path'], 'nodes': nodes}
    _wire(page, labels, seeded_body(body, pending_mount), report, 'seeded', positions=plan(body)['seed'],
          mount_action=lambda: _append_mount(page, labels, body, report))
    _matrix(page, labels, seeded_body(body, pending_mount), report, token, 'seeded')
    _empty(page, labels, report, token, 'seeded')


def retained(page, labels, body, report, token, response, kind):
    if not enabled(report):
        return
    parts = urlsplit(response.url)
    record = {'method': response.request.method, 'path': parts.path, 'query': parts.query,
        'body': response.request.post_data_json, 'status': response.status, 'response': response.json()}
    assert_refusal(record, body, kind, _pending(report), report)
    report['boolean_rejection']['rejected_wire'] = record
    common = _common(report)
    _, error = refusal(body, kind, _pending(report))
    reason = common['apiError'][error['code']]
    for key, value in error['params'].items():
        reason = reason.replace('{{' + key + '}}', str(value))
    rejected_notice(page, labels['action']['createFailed'].replace('{{detail}}', reason), labels, report)
    _empty(page, labels, report, token, 'retained-before-matrix')
    # Crucially, no served-name edit/evaluate while provider is still expired.
    _matrix(page, labels, seeded_body(body, _pending(report)), report, token, 'retained')
    _empty(page, labels, report, token, 'retained-after-matrix')


def restore(page, labels, body, report, token):
    if not enabled(report):
        return
    name = page.get_by_role('textbox', name=re.compile(re.escape(labels['basic']['name']) + '$'))
    expect(name).to_have_value(body['name'])
    # Caller has corrected the name or completed the original provider finally.
    # First prove that correction itself did not reset the pending booleans.
    _wire(page, labels, seeded_body(body, _pending(report)), report, 'corrected-seeded')
    _wire(page, labels, body, report, 'original-restored', positions=plan(body)['restore'],
          mount_action=lambda: _remove_mount(page, labels, body, report))
    _matrix(page, labels, body, report, token, 'original-restored')
    _wire(page, labels, body, report, 'final-original')
    _empty(page, labels, report, token, 'before-sole-201')
    report['last_step'] = 'boolean-rejection-original-draft-restored-before-sole-201'
