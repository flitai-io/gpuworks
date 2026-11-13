"""Checklist 14.4: the Expert draft renders every non-boolean dictionary spec.

The oracle is the server's own `GET /api/v1/engines` payload, read at run time
from the same release under test - not a vLLM-only hardcoded subset. For each
published non-boolean `param_spec` the surface must draw the control its
declared `type`/`control` maps to, with the dictionary's own bounds, option set
and localized text:

  type/control                control the product must render
  --------------------------  ------------------------------------------------
  control == "segmented"      Segmented radiogroup, one radio per option
  control == "combobox"       AutoComplete textbox (free text plus suggestions)
  control == "slider"         Slider with aria-valuemin/aria-valuemax
  type == "enum" | "select"   Select whose options are the dictionary's options
  integer | number            InputNumber spinbutton with the dictionary bounds
  type == "json" | "json"     multi-line text box
  type == "string"            single-line text box

Source of the mapping: ui/src/pages/deploy/DeployParamField.tsx branch order and
ui/src/pages/deploy/SliderControl.tsx. Source of the data: internal/server/
deploy/param_specs.go as published by internal/server/api/engines.go, with the
localized label/placeholder/help text from ui/src/i18n/locales/{en,zh}/*.json.

A key the surface does not draw is reported as a conditional-visibility fact (the
dictionary publishes it, this draft shape does not reach it), never silently
dropped and never counted as verified. The probe owns its own page and never
submits: creation, persistence and runtime stay with the consumer contract.
"""
import json
import re
from pathlib import Path
from urllib.parse import urlencode, urljoin, urlsplit

from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check

TABS = ('model', 'performance', 'runtime', 'resource')
GROUPS = {'model': ('model',), 'performance': ('performance', 'speculative', 'performance_advanced'),
          'runtime': (), 'resource': ()}
# Groups the Expert draft only draws once the model itself qualifies for them.
MULTIMODAL_GROUP = 'multimodal'
# Groups whose specs the Expert draft only draws on a named tab, so an absence in
# that tab is measured rather than assumed.
ABSENT_PUBLISHED = {
    'data_parallel_size': 'published parallel-group integer with no renderer on any surface',
}
TIER_KEYS = ('kv_offloading_size', 'shared_kv_dir', 'kv_offload_read_threads',
             'kv_offload_write_threads', 'kv_disk_limit_gib')
# A non-default published option per engine, and one out-of-range typed value
# per engine, chosen from keys whose flag the engine builder emits for any
# stated value (internal/server/deploy/vllm.go:165-250).
ENUM_PROBES = {'vllm': (('kv_cache_dtype', 'mxfp8'), ('performance_mode', 'throughput')),
               'sglang': (('kv_cache_dtype', 'fp8_e4m3'), ('speculative_algorithm', 'EAGLE3'))}
BOUNDARY_PROBES = {'vllm': (('watermark', 2), ('stream_interval', 0)),
                   'sglang': (('speculative_tokens', 0), ('speculative_num_steps', 0))}


def non_boolean(spec):
    return spec.get('type') != 'boolean' and spec.get('control') != 'switch'


def unconditional(spec):
    """Whether this draft shape can draw the spec at all, before any tier switch."""
    return (not spec.get('multimodal_only') and (spec.get('group') or '') != MULTIMODAL_GROUP
            and spec['key'] not in TIER_KEYS)


# The only engine profile that declares a KV disk tier, and therefore the only one
# whose dictionary entry carries kv_disk_min_kernel (internal/server/deploy/vllm.go:49).
KNOWN_FLOOR_ENGINE = 'vllm'

ADVANCED_GROUP = 'performance_advanced'


def draws_advanced_group(specs):
    """Whether this engine's dictionary publishes any performance_advanced spec.

    The fold is drawn by the draft only when the engine has rows to put in it. All
    eleven performance_advanced specs are vllm-only in internal/server/deploy/
    param_specs.go, so an sglang profile legitimately renders no fold at all. Asking
    for the fold unconditionally turned a correct absence into a failure, so the
    fold is expanded when the dictionary puts something in it, and the specs in it
    are still asserted one by one by visit().
    """
    return any((spec.get('group') or '') == ADVANCED_GROUP for spec in specs)


# Keys the draft draws through a dedicated component that wires the control to the
# form by hand instead of through Form.Item's name, so no [id="<key>"] exists:
# DeployParamField.tsx:117 hands log_level to DeployLogLevelField, whose Form.Item
# carries only a label (DeployLogLevelField.tsx:52) and whose Select is identified
# by aria-label (DeployLogLevelField.tsx:24). Looking these up by id fails on a
# control that is drawn and operable, so they are located by their row label like
# the slider and switch specs are.
NO_FORM_ID_KEYS = ('log_level',)


def expected_shape(spec):
    control, kind = spec.get('control'), spec.get('type')
    if control == 'segmented':
        return 'segmented'
    if control == 'combobox':
        return 'combobox'
    if control == 'slider':
        return 'slider'
    if kind == 'enum' or control == 'select':
        return 'select'
    if kind in ('integer', 'number') or control == 'number':
        return 'number'
    if kind == 'json' or control == 'json':
        return 'json'
    return 'text'


def option_labels(control, count):
    """Read every live ARIA option with its real list index.

    The keyboard walk starts at the currently selected option, so the visiting
    order is a rotation of the rendered order. rc-select encodes the true
    position in each option's id (``<listId>_list_<index>``,
    ui/node_modules/rc-select/es/OptionList.js:265-271), so the index recovers
    the exact order instead of inferring it from where focus happened to land.
    """
    control.focus()
    control.press('ArrowDown')
    expect(control).to_have_attribute('aria-expanded', 'true')
    found = {}
    for _ in range(count + 1):
        active_id = control.get_attribute('aria-activedescendant')
        check(active_id, 'open select has no active option')
        active = control.page.locator('[id="' + active_id + '"]')
        expect(active).to_have_attribute('role', 'option')
        match = re.search(r'_list_(\d+)$', active_id)
        check(match, 'select option id does not carry a list index: ' + active_id)
        index = int(match.group(1))
        found.setdefault(index, active.get_attribute('aria-label') or active.inner_text())
        control.press('ArrowDown')
    control.press('Escape')
    expect(control).to_have_attribute('aria-expanded', 'false')
    check(sorted(found) == list(range(count)),
          'visible option indices are not 0..%d: %s' % (count - 1, json.dumps(found, ensure_ascii=False)))
    return [found[index] for index in range(count)]


def localized(text, locale):
    return (text or {}).get(locale)


def dictionary(page, engine, report):
    response = page.request.get(urljoin(page.url, '/api/v1/engines'))
    check(response.status == 200, 'param dictionary GET failed')
    profile = next((row for row in response.json()['items'] if row['name'] == engine), None)
    check(profile is not None, 'live dictionary does not publish the draft engine ' + engine)
    specs = [spec for spec in profile.get('param_specs', [])
             if non_boolean(spec) and (not spec.get('engines') or engine in spec['engines'])]
    report['last_step'] = 'param-inventory-dictionary'
    check(specs, 'live dictionary publishes no non-boolean param spec for ' + engine)
    same_type = {}
    for spec in specs:
        same_type.setdefault(spec['type'], []).append(spec['key'])
    engine_others = [row['name'] for row in response.json()['items']
                     if any(non_boolean(s) for s in row.get('param_specs', []))]
    report['param_dictionary'] = {'engine': engine, 'non_boolean_keys': sorted(s['key'] for s in specs),
                                  'by_type': {kind: sorted(keys) for kind, keys in same_type.items()},
                                  'engines_publishing_specs': sorted(engine_others),
                                  'read': {'method': 'GET', 'path': '/api/v1/engines', 'status': response.status}}
    return specs


def field(page, key):
    control = page.locator('[id=' + json.dumps(key, ensure_ascii=False) + ']').filter(visible=True)
    expect(control).to_have_count(1)
    return control


def labeled_field(page, key, label):
    """The one visible form row whose own label is the dictionary label.

    Slider and switch specs register no Form field (DeployParamField.tsx
    ``name={isSwitch || spec.control === "slider" ? undefined : spec.key}``), so
    the draft draws them with no ``#<key>`` id. Their real label is the row's
    label element, which is also what the control's accessible name comes from.
    """
    rows = page.locator('.ant-form-item').filter(
        has=page.locator('label').filter(has_text=re.compile('^' + re.escape(label) + '$'))).filter(visible=True)
    expect(rows).to_have_count(1)
    row = rows.first
    check(not page.locator('[id=' + json.dumps(key, ensure_ascii=False) + ']').filter(visible=True).count(),
          key + ': slider row unexpectedly registered a form field id')
    return row


def form_row(control):
    return control.locator(
        'xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-form-item ")][1]')


def number_shape(control, spec, label, locale, report, key):
    expect(control).to_have_role('spinbutton')
    expect(control).to_have_accessible_name(label)
    for name, expected in (('aria-valuemin', spec.get('min')), ('aria-valuemax', spec.get('max'))):
        if expected is None:
            check(control.get_attribute(name) is None, key + ': unexpected ' + name + ' bound')
        else:
            expect(control).to_have_attribute(name, str(expected))
    # rc-input-number defaults step to 1 when the dictionary states none
    # (ui/node_modules/rc-input-number/es/InputNumber.js:47-48); a stated step
    # must win over that default.
    expect(control).to_have_attribute('step', str(spec['step'] if spec.get('step') is not None else 1))
    placeholder = localized(spec.get('placeholder'), locale)
    if not placeholder and spec.get('automatic'):
        placeholder = report['param_labels']['automatic']
    if placeholder:
        expect(control).to_have_attribute('placeholder', placeholder)
    expect(control).to_be_enabled()


def select_shape(control, spec, locale, report, key):
    expect(control).to_have_role('combobox')
    options = spec.get('options') or []
    expected = [(option['value'], localized(option.get('label'), locale) or option['value'])
                for option in options] or [(value, value) for value in (spec.get('enum') or [])]
    check(expected, key + ': live enum spec publishes no option')
    check(sorted(value for value, _ in expected) == sorted(spec.get('enum') or [value for value, _ in expected]),
          key + ': published options disagree with the published enum')
    # DeployLogLevelField.tsx:30 puts a "no override" entry in front of the dictionary
    # options, because an untouched log level and an explicit empty one mean different
    # things (DeployLogLevelField.tsx:20-21). That entry is drawn by the product and is
    # part of this control's real option set, so it is expected here rather than
    # tolerated; the dictionary's own options must still follow in their stated order.
    unset = report['param_labels']['unset'] if key in NO_FORM_ID_KEYS else None
    wanted = ([unset] if unset else []) + [text for _, text in expected]
    labels = option_labels(control, len(wanted))
    check(labels == wanted,
          key + ': select option set/order differs from the live dictionary: ' + json.dumps(
              {'rendered': labels, 'dictionary': wanted}, ensure_ascii=False))
    report.setdefault('param_enum_options', {})[key] = [value for value, _ in expected]


def shape(page, spec, locale, labels, report, tab):
    key = spec['key']
    report['last_step'] = 'param-inventory-' + tab + '-' + key
    wanted = expected_shape(spec)
    label = localized(spec.get('label'), locale)
    check(label, key + ': live dictionary publishes no ' + locale + ' label')
    if wanted == 'slider' or key in NO_FORM_ID_KEYS:
        row = labeled_field(page, key, label)
        if wanted == 'slider':
            control = row.get_by_role('slider')
            expect(control).to_have_count(1)
        else:
            # An antd Select reports the combobox role on its inner input, which is
            # also what carries the aria-label DeployLogLevelField.tsx:24 sets.
            control = row.get_by_role('combobox')
    else:
        control = field(page, key)
        row = form_row(control)
        expect(row).to_have_count(1)
        expect(row.locator('label').first).to_have_text(label)
    if wanted == 'number':
        number_shape(control, spec, label, locale, report, key)
    elif wanted == 'select':
        select_shape(control, spec, locale, report, key)
        expect(control).to_have_accessible_name(label)
    elif wanted == 'segmented':
        expect(control).to_have_role('radiogroup')
        expected = [localized(option.get('label'), locale) or option['value']
                    for option in spec.get('options') or []]
        check(expected, key + ': live segmented spec publishes no option')
        actual = control.get_by_role('radio').evaluate_all(
            'es => es.map(e => (e.nextElementSibling && e.nextElementSibling.textContent || "").trim())')
        check(actual == expected,
              key + ': segmented option labels differ from the live dictionary: ' + json.dumps(
                  {'rendered': actual, 'dictionary': expected}, ensure_ascii=False))
        report.setdefault('param_enum_options', {})[key] = [option['value'] for option in spec['options']]
    elif wanted == 'slider':
        expect(control).to_have_role('slider')
        for name, expected in (('aria-valuemin', spec.get('min')), ('aria-valuemax', spec.get('max'))):
            if expected is None:
                check(control.get_attribute(name) is None, key + ': unexpected ' + name)
            else:
                expect(control).to_have_attribute(name, str(expected))
    elif wanted == 'combobox':
        expect(control).to_have_role('combobox')
        expect(control).to_have_accessible_name(label)
    elif wanted == 'json':
        check(control.evaluate('e => e.tagName') == 'TEXTAREA', key + ': json spec is not a text area')
        placeholder = localized(spec.get('placeholder'), locale)
        if placeholder:
            expect(control).to_have_attribute('placeholder', placeholder)
        expect(control).to_be_enabled()
    else:
        check(control.evaluate('e => e.tagName') == 'INPUT', key + ': string spec is not a text box')
        check(control.get_attribute('role') is None, key + ': string spec should not claim a widget role')
        placeholder = localized(spec.get('placeholder'), locale)
        if placeholder:
            expect(control).to_have_attribute('placeholder', placeholder)
        expect(control).to_be_enabled()
    rendered = {'key': key, 'type': spec['type'], 'control': spec.get('control'), 'shape': wanted,
                'tab': tab, 'group': spec.get('group'), 'label': label,
                'min': spec.get('min'), 'max': spec.get('max'), 'step': spec.get('step'),
                'multimodal_only': bool(spec.get('multimodal_only'))}
    report.setdefault('param_matrix', []).append(rendered)
    return rendered


def same(actual, expected, label):
    canonical = lambda value: json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)
    check(canonical(actual) == canonical(expected), label + ': expected=' + repr(expected) + '; actual=' + repr(actual))


def collapse(page, label):
    header = page.locator('.ant-collapse-header').filter(has=page.get_by_text(label, exact=True))
    expect(header).to_have_count(1)
    if header.get_attribute('aria-expanded') != 'true':
        header.click()
    expect(header).to_have_attribute('aria-expanded', 'true')
    expect(header.locator('..').locator('.ant-collapse-content')).to_be_visible()


def visit(page, tab, specs, locale, labels, report):
    """Walk one Expert tab and assert every drawable spec the dictionary assigns to it."""
    page.get_by_role('tab', name=labels['page']['step'][tab], exact=True).click()
    if tab == 'performance' and draws_advanced_group(specs):
        collapse(page, labels['performance']['advanced'])
    groups = GROUPS[tab]
    on_tab = [spec for spec in specs
              if (spec.get('group') or '') in groups and unconditional(spec)]
    for spec in on_tab:
        shape(page, spec, locale, labels, report, tab)
    return {spec['key'] for spec in on_tab}


def evaluate(page, action, predicate, report, label):
    """Subscribe before the edit and read only the matching real evaluate.

    The form debounces, so the predicate names the quick param this step edits
    instead of accepting whichever request happens to arrive first.
    """
    report['last_step'] = label
    with page.expect_response(lambda response: urlsplit(response.url).path == '/api/v1/evaluate'
                              and response.request.method == 'POST'
                              and predicate(response.request.post_data_json), timeout=40000) as seen:
        action()
    response = seen.value
    check(response.status == 200, label + ': evaluate refused')
    record = {'checkpoint': label, 'request': response.request.post_data_json,
              'response': response.json()}
    report.setdefault('param_evaluations', []).append(record)
    return record


def argv_value_of(response, flag):
    """The one value the server put after this flag in the real rank argv."""
    ranks = response.get('rank_commands') or []
    check(len(ranks) == 1, 'param probe must evaluate exactly one real rank')
    argv = ranks[0]['argv']
    check(argv.count(flag) == 1, flag + ' missing or duplicated in the evaluated argv: '
          + json.dumps(argv, ensure_ascii=False))
    return argv[argv.index(flag) + 1]


def boundary_probe(page, spec, typed, bound, report):
    """Type past a dictionary bound and measure what the control really does.

    rc-input-number does not clamp while the user types: ``collectInputValue``
    calls ``triggerValueUpdate(..., userTyping=true)``, which skips the align
    step, and the range check then suppresses the change. It clamps on blur or
    Enter through ``flushInputValue(false)`` (ui/node_modules/rc-input-number/
    es/InputNumber.js:246-262,306-329,443-449). The assertions below state the
    measured behaviour instead of presuming which one it is:

      * the text just typed stays visible and the draft is untouched;
      * leaving the field settles the control on the near bound;
      * the real evaluate request then carries that bound, not the typed value;
      * the server's own launch facts carry it too, read back without the DOM.
    """
    key = spec['key']
    report['last_step'] = 'param-boundary-' + key
    control = field(page, key)
    expect(control).to_be_enabled()
    typed_text = str(typed)
    control.fill(typed_text)
    expect(control).to_have_value(typed_text)
    # The typed value is only text: the control has published no such value.
    record = evaluate(page, lambda: control.press('Tab'),
                      lambda body: body.get('served_name') == report['param_marker'],
                      report, 'param-boundary-' + key + '-settled')
    settled = control.input_value()
    check(float(settled) == float(bound),
          key + ': the control did not settle on the near bound; typed=' + typed_text
          + ' settled=' + settled + ' bound=' + str(bound))
    sent = record['request'].get('quick_params', {}).get(key)
    check(sent is not None and float(sent) == float(bound),
          key + ': the settled draft did not reach the wire with the near bound; sent='
          + repr(sent) + ' typed=' + typed_text)
    same(record['response'].get('form_values', {}).get(key), sent,
         'param boundary server form-values readback for ' + key)
    flag = spec.get('flag') or ''
    argv_value = None
    if flag:
        argv_value = argv_value_of(record['response'], flag)
        check(float(argv_value) == float(sent),
              key + ': server argv readback disagrees with the settled value; argv='
              + repr(argv_value) + ' sent=' + repr(sent))
    verdict = {'key': key, 'type': spec['type'], 'typed': typed_text,
               'settled_dom': settled, 'wire_value': sent,
               'bound_min': spec.get('min'), 'bound_max': spec.get('max'),
               'near_bound': bound,
               'behaviour': ('out-of-range text never entered the draft; leaving the field '
                             'settled the control on the dictionary bound'),
               'typed_value_on_wire': False,
               'server_readback': {'form_values': sent, 'argv_flag': flag or None,
                                   'argv_value': argv_value}}
    report.setdefault('param_boundaries', []).append(verdict)
    return verdict


def enum_probe(page, spec, value, report, locale):
    """Pick a non-default dictionary option and follow it to the real launch."""
    key = spec['key']
    report['last_step'] = 'param-enum-' + key
    label = next((localized(option.get('label'), locale) or option['value']
                  for option in spec.get('options') or [] if option['value'] == value), value)
    if spec.get('control') == 'segmented':
        # rc-segmented draws each position as a label + radio + titled div; the
        # visible title is the dictionary label, so aim at that instead of
        # inventing an accessible name the control does not publish.
        item = page.locator('[id=' + json.dumps(key, ensure_ascii=False) + ']') \
            .filter(visible=True).get_by_text(label, exact=True)
        expect(item).to_have_count(1)
        record = evaluate(page, item.click,
                          lambda body: body.get('quick_params', {}).get(key) == value,
                          report, 'param-enum-' + key + '-pick')
    else:
        control = field(page, key)
        index = [option['value'] for option in spec.get('options') or []] .index(value) \
            if spec.get('options') else (spec.get('enum') or []).index(value)
        record = evaluate(page, lambda: choose_index(control, index),
                          lambda body: body.get('quick_params', {}).get(key) == value,
                          report, 'param-enum-' + key + '-pick')
    sent = record['request'].get('quick_params', {}).get(key)
    same(sent, value, 'param enum real request body carries the chosen value for ' + key)
    same(record['response'].get('form_values', {}).get(key), value,
         'param enum server form-values readback for ' + key)
    same(record['response'].get('effective_params', {}).get(key), value,
         'param enum server effective-params readback for ' + key)
    flag = spec.get('flag') or ''
    if flag:
        same(argv_value_of(record['response'], flag), value,
             'param enum server argv readback for ' + key)
    verdict = {'key': key, 'type': spec['type'], 'chosen': value,
               'default': spec.get('default'),
               'dictionary_enum': spec.get('enum'),
               'rendered_label': label,
               'request_quick_params': sent,
               'server_readback': {'form_values': value, 'effective_params': value,
                                   'argv_flag': flag or None}}
    report.setdefault('param_enums', []).append(verdict)
    return verdict


def choose_index(control, index):
    """Move the live ARIA active option to one index and confirm it.

    AntD renders long dictionaries in a virtual list, so an option far down the
    list has no DOM node to click until it is scrolled into view. The active
    option is addressable by its list index in every case
    (ui/node_modules/rc-select/es/OptionList.js:265-271), so walking with the
    keyboard reaches the real option instead of depending on what happens to be
    rendered.
    """
    control.focus()
    control.press('ArrowDown')
    expect(control).to_have_attribute('aria-expanded', 'true')
    for _ in range(index + 2):
        active_id = control.get_attribute('aria-activedescendant')
        check(active_id, 'open select has no active option')
        match = re.search(r'_list_(\d+)$', active_id)
        check(match, 'select option id does not carry a list index: ' + active_id)
        if int(match.group(1)) == index:
            break
        control.press('ArrowDown')
    active_id = control.get_attribute('aria-activedescendant')
    same(int(re.search(r'_list_(\d+)$', active_id).group(1)), index,
         'select active option did not reach the requested index')
    control.press('Enter')
    expect(control).to_have_attribute('aria-expanded', 'false')


def kv_kernel_publication(parent, engine, report):
    """The engine-only release boundary the dictionary publishes (version visibility)."""
    response = parent.request.get(urljoin(parent.url, '/api/v1/engines'))
    check(response.status == 200, 'engine dictionary GET failed')
    items = response.json()['items']
    published = {row['name']: row.get('kv_disk_min_kernel') for row in items}
    # The floor is a property of an engine with a KV disk tier, and vllm is the only
    # profile that declares one: internal/server/deploy/vllm.go:49 is the sole place
    # that sets KVDiskMinKernel, the field carries omitempty, and engines_test.go:156
    # requires sglang and callable to publish none so a client cannot read "no tier"
    # as "any kernel is too old". Requiring the floor of every engine therefore failed
    # on exactly the engines the product leaves without one. The invariant is what is
    # asserted instead: nobody but vllm may publish a floor, and vllm must publish one.
    floors = {name: value for name, value in published.items() if value is not None}
    same(sorted(name for name in floors if name != KNOWN_FLOOR_ENGINE), [],
         'an engine without a KV disk tier published a kernel floor')
    if engine == KNOWN_FLOOR_ENGINE:
        check(engine in floors,
              engine + ': the live dictionary publishes no kv_disk_min_kernel release')
    report['param_engine_release'] = {'engine': engine,
                                      'kv_disk_min_kernel': published.get(engine),
                                      'publishing_engines': sorted(floors),
                                      'read': {'method': 'GET', 'path': '/api/v1/engines',
                                               'status': response.status}}


def exercise(parent, labels, body, report):
    """14.4: item-by-item dictionary validation on the real Expert draft surface."""
    locale, engine = report['locale'], body['engine']
    specs = dictionary(parent, engine, report)
    kv_kernel_publication(parent, engine, report)
    deploy = json.loads((ROOT / ('ui/src/i18n/locales/' + locale + '/deploy.json')).read_text())
    report['param_labels'] = {'automatic': deploy['shared']['automatic'],
                              'unset': deploy['workbench']['unset']}
    page = parent.context.new_page()
    page.set_default_timeout(15000)
    observed = {'creates': [], 'console_errors': [], 'page_errors': [], 'http_errors': []}
    report['param_probe_page'] = observed

    def on_request(request):
        if request.method == 'POST' and urlsplit(request.url).path == '/api/v1/deployments':
            observed['creates'].append({'method': 'POST', 'path': urlsplit(request.url).path,
                                        'body': request.post_data_json})

    def on_response(response):
        if response.status >= 400:
            observed['http_errors'].append({'method': response.request.method,
                                            'path': urlsplit(response.url).path, 'status': response.status})

    def on_console(message):
        if message.type == 'error':
            observed['console_errors'].append({'text': message.text, 'location': message.location})

    def on_pageerror(error):
        observed['page_errors'].append(str(error))

    parent.context.on('request', on_request)
    page.on('response', on_response)
    page.on('console', on_console)
    page.on('pageerror', on_pageerror)
    primary = None
    try:
        query = urlencode({'model': body['model_id'], 'template': body['template_id'],
                           'spec': body['template_spec_name'], 'spec_source': body['template_spec_source']})
        page.goto(urljoin(parent.url, '/deploy?' + query))
        report['last_step'] = 'param-inventory-fixture-model'
        modal = page.get_by_role('dialog', name=labels['weightSourceModal']['confirmTitle'], exact=True)
        # The confirmation is raised by a lookup the page runs after mount
        # (useTemplateWeightSource.ts:177 holds until lookup.key === key), so it can
        # arrive well after load. deployment_numeric_checks.py waits for the same
        # dialog for the same reason; a short fixed window turned a slow lookup into
        # a failure on a surface that answers correctly a moment later.
        expect(modal).to_be_visible(timeout=60000)
        button(modal, labels['weightSourceModal']['useModel']).click()
        expect(modal).not_to_be_visible()
        marker = 'e2e-' + report['run_id'] + '-params'
        report['param_marker'] = marker
        page.get_by_role('textbox', name=labels['basic']['servedName'], exact=True).fill(marker)
        expect(button(page, labels['page']['deploy'])).to_be_enabled(timeout=30000)
        modes = page.locator('.ant-segmented').filter(
            has=page.get_by_role('radio', name=labels['page']['mode']['expert'], exact=True))
        modes.locator('.ant-segmented-item-label').filter(
            has_text=re.compile('^' + re.escape(labels['page']['mode']['expert']) + '$')).click()
        expect(page.locator('.deploy-expert-card')).to_be_visible()
        drawn = set()
        for tab in TABS:
            drawn |= visit(page, tab, specs, locale, labels, report)
        # The draft under test carries a text-only model and its tiers are off,
        # so these published keys have no control yet - recorded as a
        # conditional-visibility fact with the condition that hides each one.
        hidden = {}
        for spec in specs:
            if spec['key'] in drawn:
                continue
            if spec['key'] in ABSENT_PUBLISHED:
                hidden[spec['key']] = ABSENT_PUBLISHED[spec['key']]
            elif spec.get('multimodal_only') or (spec.get('group') or '') == MULTIMODAL_GROUP:
                hidden[spec['key']] = ('published multimodal_only in the multimodal group; this fixture '
                                       'model declares input_modalities ["text"], so the Expert model tab '
                                       'draws no multimodal subsection')
            elif spec['key'] in TIER_KEYS:
                hidden[spec['key']] = ('tier companion/level; the Expert draft draws it only after the '
                                       'memory tier is enabled and its capacity stated')
            else:
                check(False, 'published non-boolean key ' + spec['key'] + ' is drawn on no Expert tab')
        same(sorted(set(spec['key'] for spec in specs) - set(hidden)),
             sorted(item['key'] for item in report['param_matrix']),
             'every reachable non-boolean spec measured exactly once')
        report['param_conditional'] = hidden
        # Enum and boundary probes reuse this probe page's own draft: the
        # dictionary is the oracle for both the option set and the bound, and
        # each verdict is read back from the server, not from the DOM.
        by_key = {spec['key']: spec for spec in specs}
        page.get_by_role('tab', name=labels['page']['step']['performance'], exact=True).click()
        if draws_advanced_group(specs):
            collapse(page, labels['performance']['advanced'])
        for key, value in ENUM_PROBES.get(engine, ()):
            spec = by_key[key]
            check(value in (spec.get('enum') or []), key + ': probe value is not a published option')
            check(value != spec.get('default'), key + ': probe value must not be the dictionary default')
            enum_probe(page, spec, value, report, locale)
        for key, typed in BOUNDARY_PROBES.get(engine, ()):
            spec = by_key[key]
            if spec.get('max') is not None and typed > spec['max']:
                bound = spec['max']
            else:
                check(spec.get('min') is not None and typed < spec['min'],
                      key + ': probe value is not outside a published bound')
                bound = spec['min']
            boundary_probe(page, spec, typed, bound, report)
        report['param_inventory'] = {
            'engine': engine,
            'measured_keys': sorted(item['key'] for item in report['param_matrix']),
            'by_shape': {kind: sorted(item['key'] for item in report['param_matrix'] if item['shape'] == kind)
                         for kind in sorted({item['shape'] for item in report['param_matrix']})},
            'conditional_keys': hidden,
            'enum_probes': [{key: item[key] for key in ('key', 'chosen', 'default', 'request_quick_params')}
                            for item in report.get('param_enums', [])],
            'boundary_probes': [{key: item[key] for key in ('key', 'typed', 'settled_dom', 'wire_value',
                                                            'near_bound', 'behaviour')}
                                for item in report.get('param_boundaries', [])],
            'create_posts': observed['creates'],
            'scope': ('dictionary-driven control shape, enum option set, real enum wire and out-of-range '
                      'boundary behaviour on the Expert draft; tiers-off, text-only fixture model'),
        }
        same(observed['creates'], [], 'param inventory emitted a deployment create')
        same(observed['console_errors'], [], 'param inventory browser console errors')
        same(observed['page_errors'], [], 'param inventory page errors')
    except BaseException as error:
        primary = error
        raise
    finally:
        errors = []
        try:
            page.close()
            observed['closed'] = page.is_closed()
            check(observed['closed'] is True, 'param probe page did not close')
        except Exception as error:
            errors.append({'stage': 'close-page', 'error': str(error)})
        for owner, event, listener in ((parent.context, 'request', on_request), (page, 'response', on_response),
                                       (page, 'console', on_console), (page, 'pageerror', on_pageerror)):
            try:
                owner.remove_listener(event, listener)
            except Exception as error:
                errors.append({'stage': 'remove-listener-' + event, 'error': str(error)})
        if errors:
            observed['cleanup_errors'] = errors
            if primary is None:
                raise AssertionError('param probe cleanup failed: ' + repr(errors))
    return report['param_inventory']
