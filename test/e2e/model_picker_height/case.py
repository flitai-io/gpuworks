#!/usr/bin/env python3
"""Model selection stays one control line, including long names and revisions.

Git prerequisite: fetch the MR target branch and use its local tracking ref.
The example assumes upstream points to the target repository and master has
been fetched; check with git rev-parse --verify upstream/master^{commit}.
Set --target-ref (or E2E_TARGET_REF for Make) to match your checkout.
This records target_sha only; it does not fetch or switch revisions.
Omitting it retains the shared runner's wei/master default, which must exist.

After building the server (no agent/GPU required):
  PYTHONPATH=test/e2e python3 -m model_picker_height.case --locale en --target-ref upstream/master --browser-channel chromium
Repeat with --locale zh. Use --server-bin to check another built revision;
--mutation labels a product mutation without modifying the assertions.
Fresh server/SQLite, real model registration and browser requests; no API mocks.
Only this run's model and temporary server are reclaimed. Diagnosis artifacts
stay in secrets/e2e/<runId>; an existing manual preview is never touched.
"""
import json
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect

from image_catalog_runner import login
from local_browser_fixture import ROOT, http
from local_business_runner import case_parser, checked, run_case
from model_hub_fixture import CONFIG, SHA, WEIGHT_BYTES
from model_hub_fixture_setup import cleanup


MODELS = '/api/v1/models'
VIEWPORTS = ((1440, 900), (1024, 900), (390, 844))
# DOMRects use fractional CSS pixels; allow only rounding, not an extra text line.
PIXEL_TOLERANCE = 0.5


def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
    prefix = 'e2e-' + run_id.lower()
    report['owned_model_repo'] = repo = prefix + '/model'
    expected = {'name': prefix + '-long-model-name', 'source': 'huggingface',
                'source_id': repo, 'revision': SHA}
    body = {**expected, 'browser_metadata': True, 'config_raw': CONFIG,
            'weight_bytes': WEIGHT_BYTES}
    code, created = http(server.base, 'POST', MODELS, token, body)
    if isinstance(created, dict) and type(created.get('id')) is int:
        report['owned_model_ids'] = [created['id']]
    checked(code == 201 and type(created.get('id')) is int, f'model prerequisite failed: HTTP {code}')
    # A second candidate in the same owned repository, so a keyboard walk has somewhere to go. The
    # cleanup already enumerates and deletes every model under this source_id, so it is reclaimed with
    # the first one.
    second_expected = {'name': prefix + '-second-model', 'source': 'huggingface',
                       'source_id': repo, 'revision': SHA + '-second'}
    code, second = http(server.base, 'POST', MODELS, token, {**second_expected, 'browser_metadata': True,
                                                              'config_raw': CONFIG, 'weight_bytes': WEIGHT_BYTES})
    if isinstance(second, dict) and type(second.get('id')) is int:
        report['owned_model_ids'].append(second['id'])
    checked(code == 201 and type(second.get('id')) is int, f'second model prerequisite failed: HTTP {code}')
    code, stored = http(server.base, 'GET', MODELS + '/' + str(created['id']), token)
    checked(code == 200 and {key: stored.get(key) for key in expected} == expected,
            'registered model differs from the supplied fixture identity')
    report['evidence'].append({'layer': 'persistence', 'id': created['id'],
                               'expected': expected, 'independent_get': code})
    return {**expected, 'id': created['id'], 'second': {**second_expected, 'id': second['id']}}


def selected_geometry(select):
    return select.evaluate('''el => {
      const rect = node => {
        const r = node.getBoundingClientRect();
        return {top:r.top, bottom:r.bottom, height:r.height, width:r.width};
      };
      const item = el.querySelector('.ant-select-selection-item');
      const lines = [];
      const walker = document.createTreeWalker(item, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (!node.textContent.trim()) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        for (const r of range.getClientRects()) {
          lines.push({text:node.textContent, top:r.top, bottom:r.bottom});
        }
      }
      return {control:rect(el), selected:rect(item), textLines:lines};
    }''')


def assert_single_line(geometry, empty_height):
    control, selected = geometry['control'], geometry['selected']
    checked(abs(control['height'] - empty_height) <= PIXEL_TOLERANCE,
            f'select grew after choosing a model: empty={empty_height}, selected={control}')
    checked(selected['top'] >= control['top'] - PIXEL_TOLERANCE
            and selected['bottom'] <= control['bottom'] + PIXEL_TOLERANCE,
            f'selected label escapes the single-line control: {geometry}')
    checked(bool(geometry['textLines']), 'selected model has no rendered text')
    for line in geometry['textLines']:
        checked(line['top'] >= control['top'] - PIXEL_TOLERANCE
                and line['bottom'] <= control['bottom'] + PIXEL_TOLERANCE,
                f'selected text is clipped or outside the control: {line}; control={control}')


def test_model_picker_height(page, context, server, artifacts, report, locale, data, token, holder):
    report['sensitive_phase'] = True
    page.add_init_script('localStorage.setItem("gpuworks.locale", ' + json.dumps(locale) + ')')
    login(page, context, server, artifacts, report, locale)
    report['sensitive_phase'] = False
    messages = json.loads((ROOT / f'ui/src/i18n/locales/{locale}/deploy.json').read_text())
    revision = messages['weightSourceModal']['revision'].replace('{{revision}}', data['revision'])
    no_files = messages['weightSourceModal']['noFiles']
    page_errors = []
    page.on('pageerror', lambda error: page_errors.append(str(error)))
    report['layouts'] = []

    for width, height in VIEWPORTS:
        report['step'] = f'model selection at {width}x{height}'
        page.set_viewport_size({'width': width, 'height': height})
        with page.expect_response(lambda r: r.request.method == 'GET'
                                  and urlsplit(r.url).path == MODELS
                                  and 'page_size' in parse_qs(urlsplit(r.url).query)) as listed:
            page.goto(server.base + '/deploy')
        response = listed.value
        checked(response.status == 200, 'browser model list failed')
        document = response.json()
        expected = {key: data[key] for key in ('id', 'name', 'source', 'source_id', 'revision')}
        second_expected = {key: data['second'][key] for key in expected}
        seen = [{key: row.get(key) for key in expected} for row in document['items']]
        checked(document['total'] == 2 and sorted(map(json.dumps, seen)) ==
                sorted(json.dumps(item) for item in (expected, second_expected)),
                'browser candidates differ from the registered fixtures')
        query = parse_qs(urlsplit(response.url).query, keep_blank_values=True)
        checked(query == {'page': ['1'], 'page_size': ['50']}, f'unexpected model lookup: {query}')
        report['evidence'].append({'layer': 'request', 'method': 'GET', 'path': MODELS,
                                   'query': query, 'status': 200, 'candidates': [expected]})

        control = page.locator('#model_id')
        select = page.locator('.ant-select').filter(has=control)
        expect(select.locator('.ant-select-selection-placeholder')).to_be_visible()
        empty_height = select.bounding_box()['height']
        control.focus()
        control.press('ArrowDown')
        controls = control.get_attribute('aria-controls')
        checked(bool(controls), 'model picker has no associated popup')
        popup = page.locator('[id=' + json.dumps(controls) + ']').locator('..')
        option = popup.locator('.ant-select-item-option').filter(has_text=data['name'])
        expect(option).to_have_count(1)
        expect(option.get_by_text(data['name'], exact=True)).to_be_visible()
        expect(option.get_by_text(revision, exact=True)).to_be_visible()
        expect(option.get_by_text(no_files, exact=True)).to_be_visible()
        # 5.6: the arrow keys are required to move the active candidate, not merely to open the popup.
        # The active option is read from the popup's own active class, which is what the control
        # highlights and what Enter would commit - a walk that only opened the menu would pass without
        # this.
        def active_label():
            node = popup.locator('.ant-select-item-option-active')
            return node.count() and node.first.inner_text() or ''

        first_active = active_label()
        control.press('ArrowDown')
        second_active = active_label()
        checked(first_active != second_active,
                f'ArrowDown did not move the active candidate: {first_active!r} == {second_active!r}')
        control.press('ArrowUp')
        checked(active_label() == first_active,
                f'ArrowUp did not return to the first candidate: {active_label()!r} != {first_active!r}')
        report['keyboard_candidates'] = {'opened_on': first_active, 'after_down': second_active,
                                         'returned_to': active_label()}
        option.click()
        summary = select.locator('.ant-select-selection-item')
        expect(summary.get_by_text(data['name'], exact=True)).to_be_visible()
        expect(summary.get_by_text(revision, exact=True)).to_be_visible()
        select.scroll_into_view_if_needed()
        geometry = selected_geometry(select)
        report['layouts'].append({'viewport': [width, height], 'empty_height': empty_height, **geometry})
        assert_single_line(geometry, empty_height)

        with page.expect_response(lambda r: r.request.method == 'GET'
                                  and urlsplit(r.url).path == MODELS + '/' + str(data['id'])) as reread:
            page.goto(server.base + '/deploy?model=' + str(data['id']))
        checked(reread.value.status == 200 and
                {key: reread.value.json().get(key) for key in expected} == expected,
                'model deep link did not read the same registration')
        expect(summary.get_by_text(data['name'], exact=True)).to_be_visible()
        expect(summary.get_by_text(revision, exact=True)).to_be_visible()
        assert_single_line(selected_geometry(select), empty_height)
    checked(page_errors == [], f'browser errors: {page_errors}')
    report['evidence'].append({'layer': 'ui', 'layouts': report['layouts'],
                               'deep_link_refill': True, 'page_errors': page_errors})


def main():
    args = case_parser(__doc__).parse_args()
    return run_case(args, contract_id='gpuworks/deployment/model-picker-height',
        manifest='test/e2e/coverage/model-picker-height.json',
        test_file='test/e2e/model_picker_height/case.py', prepare=prepare,
        exercise=test_model_picker_height, cleanup=cleanup, docker_required=False,
        server_options={'auto_check_updates': False, 'env_overrides': {'GPUWORKS_USAGE_REPORT': 'off'}},
        helpers=('test/e2e/model_hub_fixture.py', 'test/e2e/model_hub_fixture_setup.py',
                 'test/e2e/image_catalog_runner.py', 'test/e2e/platform_token_read_revoke.py',
                 'ui/src/pages/deploy/ModelPicker.tsx', 'ui/src/pages/deploy/ModelRegistrationDetails.tsx',
                 'ui/src/theme/tokens.ts', 'ui/src/styles.css'))


if __name__ == '__main__':
    raise SystemExit(main())
