"""Shared launch checks for the six existing, single-rank deployment contracts.

Fixture facts anchor the evaluated launch; the UI, persisted instance, Docker
configuration and the running fake engine must then agree. This proves real
process dispatch, not GPU inference or a multi-node/PD topology.
"""
import copy
import hashlib
import io
import json
import re
from pathlib import Path
import re
import shlex
import subprocess
import tarfile
import time
from urllib.parse import urljoin, urlsplit

import deployment_boolean_checks as boolean_checks
import deployment_boolean_inventory as boolean_inventory
import deployment_boolean_rejection_checks as boolean_rejection

from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check

FAMILIES = ('all', 'quick-expert', 'manual-roundtrip', 'launch-contract', 'manual-editor', 'boolean-tristate', 'boolean-inventory', 'numeric-inventory', 'boolean-rejection', 'param-inventory')
SOURCE_FILES = ('deployment_numeric_contract.py', 'deployment_numeric_checks.py', 'deployment_editor_checks.py', 'deployment_launch_checks.py', 'deployment_batch_checks.py',
    'deployment_selector_checks.py', 'deployment_wizard_create.py', 'deployment_wizard_fixture.py',
    'deployment_override_create.py', 'deployment_override_fixture.py', 'deployment_acquisition.py',
    'deployment_acquisition_browser.py', 'deployment_acquisition_fixture.py',
    'deployment_acquisition_oracles.py', 'deployment_boolean_checks.py',
    'deployment_boolean_contracts.py', 'deployment_boolean_inventory.py',
    'deployment_boolean_inventory_contracts.py', 'deployment_boolean_inventory_probe.py',
    'deployment_boolean_rejection_checks.py', 'deployment_param_checks.py')


def enabled(report, family):
    return report.get('deployment_check_family', 'all') in ('all', family)


def same(actual, expected, label):
    canonical = lambda value: json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)
    check(canonical(actual) == canonical(expected), f'{label}: expected={expected!r}; actual={actual!r}')


def collection(value, empty, label):
    check(value is None or type(value) is type(empty), label + ': wrong collection wire type')
    return empty if value is None else value


def freeze(report, family=None):
    if family is not None:
        check(family in FAMILIES, 'unknown deployment check family')
        selection = {'family': family, 'probe_only': family != 'all'}
        if 'deployment_check_selection' in report:
            same(selection, report['deployment_check_selection'], 'deployment diagnostic selection changed during run')
        report['deployment_check_family'] = family
        report['deployment_check_selection'] = selection
        report['probe_only'] = family != 'all'
    same(report['deployment_check_selection'],
         {'family': report['deployment_check_family'], 'probe_only': report['probe_only']},
         'deployment diagnostic selection changed during run')
    current = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    if 'launch_checks_sha256' in report:
        same(current, report['launch_checks_sha256'], 'launch helper changed before draft checks')
    report['launch_checks_sha256'] = current
    hashes = {name: hashlib.sha256((ROOT / 'test/e2e' / name).read_bytes()).hexdigest()
              for name in SOURCE_FILES}
    if 'deployment_launch_source_hashes' in report:
        same(hashes, report['deployment_launch_source_hashes'], 'deployment test sources changed during run')
    report['deployment_launch_source_hashes'] = hashes


def frozen(report):
    check('deployment_launch_source_hashes' in report, 'missing initial deployment source identity')
    freeze(report)


def evaluation(page, action, predicate, report, label):
    """Subscribe before the user action and accept only the matching real wire.

    A mode switch can legitimately send no request. Callers handle that by
    changing the served name through its real textbox after the switch.
    """
    report['last_step'] = label
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/evaluate'
                              and r.request.method == 'POST'
                              and predicate(r.request.post_data_json), timeout=30000) as observed:
        action()
    response = observed.value
    check(response.status == 200, label + ': evaluate refused')
    record = {'checkpoint': label, 'request': response.request.post_data_json, 'response': response.json()}
    report.setdefault('launch_evaluations', []).append(record)
    return record


def visible_button(scope, text):
    pattern = r'\s*'.join(map(re.escape, text)) if re.fullmatch(r'[\u3400-\u9fff]{2}', text) else re.escape(text)
    action = scope.get_by_role('button').filter(has_text=re.compile('^' + pattern + '$'))
    expect(action).to_have_count(1)
    return action


def choose(page, control, text):
    selector = control.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-select-selector ')][1]")
    expect(selector).to_have_count(1)
    expect(selector).to_be_visible()
    selector.click()
    popup_id = control.get_attribute('aria-controls')
    check(bool(popup_id), 'select has no popup relationship')
    popup = page.locator('[id=' + json.dumps(popup_id) + ']').locator('..')
    # AntD's role=option nodes are a hidden accessibility mirror; click the
    # actual option in this control's associated popup.
    popup.locator('.ant-select-item-option[title=' + json.dumps(text, ensure_ascii=False) + ']').click()


def mode(page, labels, value):
    modes = page.locator('.ant-segmented').filter(has=page.get_by_role(
        'radio', name=labels['page']['mode']['expert'], exact=True))
    radio = modes.get_by_role('radio', name=labels['page']['mode'][value], exact=True)
    modes.locator('.ant-segmented-item-label').filter(has_text=re.compile(
        '^' + re.escape(labels['page']['mode'][value]) + '$')).click()
    expect(radio).to_be_checked()
    if value == 'expert':
        expect(page.locator('.deploy-expert-card')).to_be_visible()
    else:
        expect(page.locator('.deploy-expert-card')).to_have_count(0)


def probe(page, labels, body, report, label, expected_quick, expected_unset=None):
    """A reversible, visible field edit makes preserved form state observable.

    Never read React internals or synthesize an evaluation request. Compare the
    entire quick-parameter object, not just the one field edited by this probe.
    """
    route = page.get_by_role('textbox', name=labels['basic']['servedName'], exact=True)
    old = route.input_value()
    marker = old + '-roundtrip'
    record = evaluation(page, lambda: route.fill(marker),
        lambda b: b.get('served_name') == marker, report, label)
    # Evaluation carries only the quick values the operator authored, never the
    # whole projected set: EvaluateQuickParams.prepare deletes quick_params when
    # nothing was authored (evaluateQuickParams.ts:36, ADR 0020), because the
    # answer is a display projection and re-sending it would pin defaults the
    # template has not confirmed. Asserting the full fixture object here was
    # therefore asserting the opposite of the contract. What the probe can still
    # require is that whatever does travel is a subset of the fixture's values -
    # no invented key, no altered value - and the caller keeps the whole-body
    # comparison for the fields that must be complete.
    sent_quick = record['request'].get('quick_params') or {}
    unexpected = {key: value for key, value in sent_quick.items()
                  if key not in expected_quick or expected_quick[key] != value}
    same(unexpected, {}, label + ' evaluated quick params outside the fixture')

    stable = ('model_id', 'model_source', 'engine', 'image', 'container_overrides', 'node_uuid',
              'nnodes', 'replicas', 'placement_mode', 'placement_strategy', 'target_vendor',
              'template_id', 'template_spec_source', 'template_spec_name', 'scenario')
    expected_draft = {key: body.get(key) for key in stable}
    # Automatic evaluation asks the scheduler to choose; submission then pins
    # that returned choice (deployHelpers/useDeploySubmit). Anchor both shapes
    # to the independent fixture's eligible node, rather than conflating them.
    if body.get('placement_mode') == 'auto':
        expected_draft['node_uuid'] = None
        same(record['response'].get('selected_node', {}).get('uuid'), body['node_uuid'],
             label + ' automatic selected node')
        ranks = record['response'].get('rank_commands', [])
        same([rank.get('node_uuid') for rank in ranks], [body['node_uuid']],
             label + ' automatic rank node')
    same({key: record['request'].get(key) for key in stable}, expected_draft,
         label + ' preserved independent draft fields')
    same(record['request'].get('quick_params_unset', []), expected_unset or [], label + ' unset keys')
    check(record['request'].get('engine') == body['engine'], label + ' engine changed')
    restored = evaluation(page, lambda: route.fill(old),
        lambda b: b.get('served_name') == old, report, label + '-restore-route')
    restored_quick = restored['request'].get('quick_params') or {}
    same({key: value for key, value in restored_quick.items()
          if key not in expected_quick or expected_quick[key] != value}, {},
         label + ' restored quick params outside the fixture')

    return restored


def quick_expert(page, labels, body, report):
    if not enabled(report, 'quick-expert'):
        return
    baseline = copy.deepcopy(body['quick_params'])
    # Prefix caching has no dictionary default: Auto must remain omitted;
    # false is a deliberate value, not a falsy synonym for omission.
    check('enable_prefix_caching' not in baseline, 'fixture prefix caching must start unstated')
    probe(page, labels, body, report, 'quick-expert-default-quick', baseline)
    mode(page, labels, 'expert')
    probe(page, labels, body, report, 'quick-expert-default-expert', baseline)
    page.get_by_role('tab', name=labels['page']['step']['performance'], exact=True).click()
    # The registered dictionary supplies the localized accessible control name.
    response = page.request.get(urljoin(page.url, '/api/v1/engines'))
    check(response.status == 200, 'engine dictionary GET failed')
    profiles = response.json()['items']
    profile = next(p for p in profiles if p['name'] == body['engine'])
    locale = report['locale']
    def label_for(key):
        label = next(s for s in profile['param_specs'] if s['key'] == key)['label']
        return label.get(locale, label.get('en')) if isinstance(label, dict) else label
    cache = page.get_by_role('switch', name=label_for('enable_cache_report'), exact=True)
    expect(cache).to_be_checked()
    cache.uncheck()
    explicit = {**baseline, 'enable_cache_report': False}
    probe(page, labels, body, report, 'quick-expert-false-expert', explicit)
    mode(page, labels, 'quick')
    probe(page, labels, body, report, 'quick-expert-false-quick', explicit)
    choose(page, page.get_by_role('combobox', name=label_for('enable_cache_report'), exact=True),
           labels['workbench']['triStateOn'])
    prefix = page.get_by_role('combobox', name=label_for('enable_prefix_caching'), exact=True)
    choose(page, prefix, labels['workbench']['triStateOff'])
    prefix_false = {**baseline, 'enable_prefix_caching': False}
    probe(page, labels, body, report, 'quick-expert-prefix-false-quick', prefix_false)
    mode(page, labels, 'expert')
    probe(page, labels, body, report, 'quick-expert-prefix-false-expert', prefix_false)
    page.get_by_role('tab', name=labels['page']['step']['performance'], exact=True).click()
    choose(page, page.get_by_role('combobox', name=label_for('enable_prefix_caching'), exact=True),
           labels['workbench']['triStateAuto'])
    probe(page, labels, body, report, 'quick-expert-unset-expert', baseline)
    mode(page, labels, 'quick')
    probe(page, labels, body, report, 'quick-expert-unset-quick', baseline)
    report['quick_expert_roundtrip'] = {'defaults': baseline, 'explicit_false': explicit,
        'prefix_false': prefix_false, 'unset_omitted': 'enable_prefix_caching', 'draft_restored': True}


def manual_roundtrip(page, labels, body, report):
    editor_checks = enabled(report, 'manual-editor')
    if not enabled(report, 'manual-roundtrip') and not editor_checks:
        return
    import deployment_editor_checks as editor_contract
    title = labels['runtimeCommand']
    collapse = page.locator('.deploy-advanced-collapse')
    collapse.get_by_text(labels['workbench']['advanced']['title'], exact=True).click()
    extra = page.get_by_role('textbox', name=labels['advanced']['customArgs'], exact=True)
    expect(extra).to_be_visible()
    original_extra = extra.input_value()
    check(original_extra == '', 'fixture must start without residual custom arguments')
    custom = '--host 0.0.0.0'
    action = (lambda: editor_contract.pending_action(
        page, extra, custom, title, visible_button, report)) if editor_checks else lambda: extra.fill(custom)
    record = evaluation(page, action,
        lambda b: b.get('extra_args') == custom, report, 'manual-roundtrip-structured-custom')
    structured = record['request']
    section = page.locator('.deploy-runtime-command')
    edit = visible_button(section, title['edit'])
    expect(edit).to_be_enabled(timeout=30000)
    launch = (record['response'].get('rank_commands') or [record['response']])[0]
    if editor_checks:
        editor_contract.structured_preview(page, title, report, launch, same)
    edit.click()
    pop = page.locator('.ant-popconfirm').filter(has_text=title['enterTitle'])
    expect(pop.get_by_text(title['enterDescription'], exact=True)).to_be_visible()
    button(pop, labels['shared']['cancel']).click()
    expect(page.locator('.deploy-command-editor')).to_have_count(0)
    expect(extra).to_have_value(custom)
    edit.click()
    # Entering manual mode submits the whole displayed configuration: useDeploy
    # Evaluation.ts:139 skips EvaluateQuickParams.prepare for command_mode manual,
    # because the command text is the source of truth and estimation does not
    # apply (ADR 0020 rule 6). The draft captured above is the structured body
    # before that, and its quick_params only hold the keys the case touched, so
    # the manual wire has to be anchored on this request instead.
    entered = evaluation(page, lambda: button(pop, title['enter']).click(),
        lambda b: b.get('container_overrides', {}).get('command_mode') == 'manual',
        report, 'manual-roundtrip-enter-text-mode')
    manual_wire = entered['request']
    # Entering manual mode pins dtype in the form; publish it so every later
    # create body in this run can be described without guessing.
    pinned_dtype = (manual_wire.get('quick_params') or {}).get('dtype')
    if isinstance(pinned_dtype, str):
        import deployment_boolean_rejection_checks as _rejection
        _rejection.form_pinned[report.get('run_id')] = pinned_dtype
    editor = page.locator('.deploy-command-editor')
    expect(editor).to_be_visible()
    generated = editor.input_value()
    check(custom in generated, 'structured residual argument was lost entering text mode')
    generated_script(generated, launch)
    if editor_checks:
        editor_contract.navigation(page, editor, generated, manual_wire, title, evaluation, same, report)
    edited = generated + '\n# retained draft edit'
    evaluation(page, lambda: editor.fill(edited),
        lambda b: b.get('container_overrides', {}).get('command_mode') == 'manual'
        and b.get('container_overrides', {}).get('cmd_display') == edited,
        report, 'manual-roundtrip-text-edit')
    visible_button(section, title['return']).click()
    returning = page.locator('.ant-popconfirm').filter(has_text=title['returnTitle'])
    expect(returning.get_by_text(title['returnDescription'], exact=True)).to_be_visible()
    button(returning, title['continueEditing']).click()
    expect(editor).to_have_value(edited)
    visible_button(section, title['return']).click()
    report['last_step'] = 'manual-roundtrip-explicit-discard'
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/evaluate'
            and r.request.method == 'POST'
            and r.request.post_data_json.get('container_overrides', {}).get('command_mode', 'engine') == 'engine') as returned:
        button(returning, title['discardAndReturn']).click()
    check(returned.value.status == 200, 'returned structured evaluate failed')
    actual = returned.value.request.post_data_json
    same(actual, structured, 'manual-roundtrip full structured draft after explicit discard')
    expect(editor).to_have_count(0)
    expect(extra).to_have_value(custom)
    for key in ('cmd_display', 'command_mode', 'command_includes_preparation'):
        check(key not in actual.get('container_overrides', {}), 'manual-only field leaked after return: ' + key)
    evaluation(page, lambda: extra.fill(original_extra), lambda b: 'extra_args' not in b,
        report, 'manual-roundtrip-restore-original-draft')
    report['manual_command_roundtrip'] = {'structured_request': structured,
        'entered_generated_command': generated, 'edited_command': edited,
        'cancel_retained_text': True, 'explicit_discard_warning': title['returnDescription'],
        'returned_request': actual, 'original_draft_restored': True,
        'scope': 'roundtrip only; actual creation remains engine mode'}


def generated_script(script, launch):
    """Parse this fixture's shell program instead of rebuilding an engine argv.

    These selected specs have no preparation script/argv. Refuse to silently
    skip such steps if the fixture ever changes to a different contract.
    """
    check(not launch.get('pre_start_script') and not launch.get('pre_start_argv'),
          'roundtrip fixture unexpectedly acquired preparation steps')
    lines = script.splitlines()
    same(shlex.split(lines[0]), ['set', '-eu'], 'manual initial shell policy')
    env, command = {}, None
    for line in lines[1:]:
        words = shlex.split(line)
        check(bool(words), 'unexpected blank manual initial line')
        if words[0] == 'export':
            check(len(words) == 2 and command is None, 'unexpected manual environment syntax/order')
            key, value = words[1].split('=', 1)
            check(key not in env, 'duplicate manual environment assignment')
            env[key] = value
        else:
            check(words[0] == 'exec' and command is None, 'unexpected manual initial shell statement')
            command = words[1:]
    same(env, launch.get('env', {}), 'manual initial full environment retained')
    same(command, launch['command'], 'manual initial full command retained')


def draft_checks(page, labels, body, report, token=None):
    freeze(report)
    if enabled(report, 'numeric-inventory'):
        from deployment_numeric_checks import inventory
        inventory(page, labels, body, report, mode, evaluation, same)
    if enabled(report, 'param-inventory'):
        import deployment_param_checks
        deployment_param_checks.exercise(page, labels, body, report)
    boolean_inventory.exercise(page, labels, body, report, token)
    boolean_checks.exercise(page, labels, body, report)
    quick_expert(page, labels, body, report)
    manual_roundtrip(page, labels, body, report)
    if not enabled(report, 'launch-contract'):
        return None
    record = probe(page, labels, body, report, 'launch-contract-final-preview', body['quick_params'])
    preview = record['response']
    ranks = preview.get('rank_commands') or []
    same(len(ranks), 1, 'fixture must evaluate exactly one actual rank')
    same({k: ranks[0][k] for k in ('node_uuid', 'node_rank', 'role', 'headless')},
         {'node_uuid': body['node_uuid'], 'node_rank': 0, 'role': 'standalone', 'headless': False},
         'fixture actual role/rank selection')
    same(preview['image'], body['image'], 'fixture preview image')
    same(preview['command_mode'], 'engine', 'fixture restored command source')
    flag = '--gpu-memory-utilization' if body['engine'] == 'vllm' else '--mem-fraction-static'
    argv = ranks[0]['argv']
    check(argv.count(flag) == 1, 'fixture utilization flag missing or duplicated')
    same(argv[argv.index(flag) + 1], str(body['quick_params']['gpu_memory_utilization']),
         'independent fixture utilization reaches preview')
    expected_env = {'HF_HUB_OFFLINE': '1', 'TRANSFORMERS_OFFLINE': '1'}
    if body['engine'] == 'vllm':
        expected_env.update(GLOO_SOCKET_IFNAME='lo', NCCL_SOCKET_IFNAME='lo', VLLM_HOST_IP='127.0.0.1')
    overrides = body['container_overrides']
    for key in overrides.get('env_unset', []):
        expected_env.pop(key, None)
    expected_env.update(overrides['env'])
    same(ranks[0]['env'], expected_env, 'independent fixture complete preview environment')
    section = page.locator('.deploy-runtime-command .deploy-rank-launch')
    expect(section).to_have_count(1)
    # Parse the displayed shell words; do not reconstruct the production engine
    # command or discard any token differences.
    expect(section.locator('pre').first).to_be_visible(timeout=30000)
    deadline = time.monotonic() + 30
    while True:
        command = shlex.split(section.locator('pre').first.inner_text())
        if command == ranks[0]['command']:
            break
        check(time.monotonic() < deadline, 'DOM command differs from the final evaluated command')
        page.wait_for_timeout(50)
    details = section.locator('details').filter(has=page.get_by_text(labels['catalog']['resolvedRuntime'], exact=True))
    details.locator('summary').click()
    actual = json.loads(details.locator('pre').inner_text())
    rank = ranks[0]
    expected = {k: rank[k] for k in ('image', 'command_mode', 'model_mount_path', 'shm_bytes') if k in rank}
    expected.update(mounts=rank.get('mounts') or [], docker_args=rank.get('docker_args') or [])
    same(actual, expected, 'DOM resolved runtime exact JSON projection')
    env_details = section.locator('details').filter(has=page.get_by_text(labels['runtimeCommand']['environment'], exact=True))
    env_details.locator('summary').click()
    displayed = {}
    for line in env_details.locator('pre').inner_text().splitlines():
        words = shlex.split(line)
        check(len(words) == 2 and words[0] == 'export', 'environment preview is not a complete export')
        key, value = words[1].split('=', 1)
        check(key not in displayed, 'duplicate preview environment key')
        displayed[key] = value
    same(displayed, rank.get('env', {}), 'DOM full environment')
    report['launch_preview'] = {'request': record['request'], 'rank': rank, 'runtime_json': actual}
    return rank



def _form_pinned_dtype(report):
    """The dtype the form pinned, published by the wizard case that entered it."""
    import deployment_boolean_rejection_checks as rejection
    return rejection.form_pinned.get(report.get('run_id'))

def snapshot_checks(saved, body, report):
    if not enabled(report, 'launch-contract'):
        return
    report['last_step'] = 'launch-contract-create-snapshot'
    expected = body
    pinned = _form_pinned_dtype(report)
    if pinned:
        # The form pinned dtype to a concrete value when this case entered manual
        # command mode (ADR 0020 rule 6). What the server stored is that value, so
        # the fixture's "auto" would report a faithful round-trip as a defect.
        expected = copy.deepcopy(body)
        expected.setdefault('quick_params', {})['dtype'] = pinned
    for key in ('engine', 'image', 'served_name', 'quick_params', 'template_id',
                'template_spec_source', 'template_spec_name', 'node_uuid', 'nnodes', 'replicas'):
        same(saved[key], expected[key], 'independent persisted deployment ' + key)
    snapshot = saved['catalog_selection_snapshot']
    wanted = {'engine': body['engine'], 'image': body['image'], 'template_id': body['template_id'],
              'spec_source': body['template_spec_source'], 'spec_name': body['template_spec_name']}
    same({key: snapshot.get(key) for key in wanted}, wanted, 'persisted launch selection snapshot')
    acquisition = 'E2E_ACQUISITION' in body['container_overrides']['env']
    template_env = {'E2E_ACQUISITION' if acquisition else 'E2E_WIZARD': report['run_id']}
    if body['engine'] == 'sglang':
        template_env['SGLANG_LOGGING_CONFIG_PATH'] = '/tmp/e2e-fixture-owned-logging.json'
    same(snapshot.get('env') or {}, template_env, 'independent original template environment snapshot')
    args = ['--dialect', 'vllm'] if acquisition else []
    entrypoint = []
    if body['engine'] == 'sglang':
        args = ['--model-type', 'llm', '--dialect', 'sglang']
        entrypoint = ['sglang', 'serve']
    same(snapshot.get('extra_args') or [], args, 'independent template residual args snapshot')
    same(snapshot.get('entrypoint') or [], entrypoint, 'independent template entrypoint snapshot')
    report['launch_create_snapshot'] = {'selection': wanted, 'template_env': template_env,
        'template_args': args, 'template_entrypoint': entrypoint, 'quick_params': saved['quick_params']}


def runtime_checks(instance, body, preview, model_path, report):
    if not enabled(report, 'launch-contract'):
        return
    check(preview is not None, 'missing evaluated launch oracle')
    report['last_step'] = 'launch-contract-real-process'
    same(instance['node_uuid'], body['node_uuid'], 'actual launch node')
    same(instance['node_rank'], 0, 'actual single rank')
    same(instance.get('nnodes'), 1, 'actual standalone topology')
    same(instance.get('image'), body['image'], 'persisted instance image')
    check(instance.get('role', '') in ('', 'standalone'), 'unexpected actual role')
    same(instance.get('command_mode'), 'engine', 'actual command source')
    normalized = copy.deepcopy(preview['argv'])
    check(normalized.count('--port') == 1, 'preview must have exactly one bound API port')
    port_index = normalized.index('--port') + 1
    same(normalized[port_index], '30000', 'planning.PreviewAPIPort contract')
    normalized[port_index] = str(instance['api_port'])
    for key, empty in (('env', {}), ('mounts', []), ('docker_args', [])):
        same(collection(instance.get(key), empty, 'instance ' + key),
             collection(preview.get(key), empty, 'preview ' + key), 'persisted full ' + key)
    same(instance.get('shm_bytes'), preview.get('shm_bytes'), 'persisted shm')
    same(instance.get('model_mount_path'), preview.get('model_mount_path'), 'persisted model mount path')
    container = instance['container_id']
    inspect = json.loads(subprocess.check_output(['docker', 'inspect', container], text=True))[0]
    config = inspect['Config']
    same(config['Image'], body['image'], 'runtime image ref')
    command = (config.get('Entrypoint') or []) + (config.get('Cmd') or [])
    same(command, instance['entrypoint'] + instance['argv'], 'complete Docker dispatch command')
    raw = subprocess.check_output(['docker', 'cp', container + ':/tmp/fake-engine-argv.json', '-'])
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:*') as archive:
        entries = archive.getmembers()
        check(len(entries) == 1 and entries[0].isfile(), 'process argv archive schema')
        process_argv = json.loads(archive.extractfile(entries[0]).read())
    report['launch_runtime_observed'] = {'container': container, 'command': command,
        'process_argv': process_argv, 'expected_entrypoint': preview['entrypoint'],
        'expected_argv': normalized, 'persisted_entrypoint': instance['entrypoint'],
        'persisted_argv': instance['argv']}
    same(instance['entrypoint'], preview['entrypoint'], 'persisted full entrypoint')
    same(instance['argv'], normalized, 'persisted full argv (only API port rebound)')
    same([body['engine']] + process_argv, command, 'real fake-engine full argv')
    # OCI ENV is outside preview by contract. Account for its exact independent
    # image configuration, then compare the complete resolved Docker environment.
    image = json.loads(subprocess.check_output(['docker', 'image', 'inspect', inspect['Image']], text=True))[0]
    def env_map(values):
        result = {}
        for item in values or []:
            key, value = item.split('=', 1)
            check(key not in result, 'duplicate Docker environment key')
            result[key] = value
        return result
    expected_env = env_map(image['Config'].get('Env'))
    expected_env.update(instance.get('env') or {})
    same(env_map(config.get('Env')), expected_env, 'complete Docker environment including OCI defaults')
    expected_mounts = [{'Source': model_path, 'Destination': instance['model_mount_path'], 'RW': False}]
    for mount in instance.get('mounts') or []:
        check('source' in mount and 'volume' not in mount, 'fixture expected bind mounts only')
        expected_mounts.append({'Source': mount['source'], 'Destination': mount['target'],
                                'RW': not mount.get('read_only', False)})
    actual_mounts = [{k: m[k] for k in ('Source', 'Destination', 'RW')} for m in inspect['Mounts']]
    by_target = lambda entries: sorted(entries, key=lambda m: m['Destination'])
    same(by_target(actual_mounts), by_target(expected_mounts), 'complete actual Docker mount set')
    same(inspect['HostConfig']['ShmSize'], instance['shm_bytes'], 'actual Docker shm bytes')
    report['launch_runtime_contract'] = {'node_uuid': instance['node_uuid'], 'node_rank': 0,
        'role': 'standalone', 'container': container, 'command': command, 'actual_argv': process_argv,
        'environment': expected_env, 'mounts': actual_mounts, 'preview_port': 30000,
        'actual_port': instance['api_port'], 'only_normalization': 'planning.PreviewAPIPort → instance api_port',
        'scope': 'all one actual standalone rank; real fake engine, not multi-node or GPU inference'}
    frozen(report)
