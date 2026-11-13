#!/usr/bin/env python3
"""Logs / six quick signatures with real collected, distinguishable records.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-log-quick-controls (build + each signature, en and zh).
One case: python3 test/e2e/log_quick_controls.py --chip 'out of memory' --locale en
Requires system Chrome/Python Playwright; owns a real server and SQLite, no GPU.
Reports: secrets/e2e/<runId>/report.json. Private diagnosis state is retained
for at most seven days; remove only that run directory after review.
"""
from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import traceback
from urllib.parse import parse_qs, urlencode, urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from log_filter_export import SEARCH, HISTOGRAM, choose, collected_warning, emit_warning
from platform_token_read_revoke import button, check, theme_color

CHIPS = ('Traceback', 'ERROR', 'WARNING', 'ERR99999', 'out of memory', 'no such container')
TERMS = ('Traceback', 'ERROR', 'WARNING', 'ERR99999', '"out of memory"', '"no such container"')
CONTRACT = 'gpuworks/logs/quick-signature-composition'
PRODUCT_FILES = ('ui/src/pages/LogsPage.tsx', 'ui/src/pages/logsPageRange.ts',
    'ui/src/i18n/locales/en/logs.json', 'ui/src/i18n/locales/zh/logs.json', 'ui/src/theme/tokens.ts')


# Mainline (Traceback): 真实 WARN 含文本/Traceback/两者/均无 -> UI 键盘与鼠标组合 -> 搜索/直方图精确集合 -> 刷新重选保持事实。
# Mainline (ERROR): 同上以 ERROR 独立准备六条记录 -> ERROR 与 WARNING 的 OR、文本 AND、空交集 -> 双视图与 GET 回读。
# Mainline (WARNING): 独立 WARNING 记录 -> 与 ERROR 增删及两种输入顺序 -> 请求、精确行和桶合计一致 -> 刷新回读。
# Mainline (ERR99999): 独立 ERR99999 记录 -> 单标签/多标签/文本顺序与恢复 -> 双视图精确匹配 -> 刷新回读。
# Mainline (out of memory): 真实多词记录 -> UI 生成引号短语并组合文本/第二标签 -> 精确匹配和排除 -> 刷新回读。
# Mainline (no such container): 真实多词记录 -> 引号短语、键盘选择、空/非空交集 -> 两个 API 与 UI 一致 -> 刷新回读。
def test_quick_controls(page, context, server, artifacts, report, locale, chip):
    texts = ROOT / 'ui/src/i18n/locales' / locale
    auth = json.loads((texts / 'auth.json').read_text())
    logs = json.loads((texts / 'logs.json').read_text())
    errors, console_errors, bad_http = [], [], []
    page.on('pageerror', lambda error: errors.append(str(error)))
    page.goto(server.base + '/logs')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login' and r.request.method == 'POST') as login:
        button(page, auth['submit']).click()
    check(login.value.status == 200, 'browser login failed')
    for cookie in context.cookies():
        artifacts.remember(cookie['value'])
    expect(page.get_by_role('heading', name=logs['title'], exact=True)).to_be_visible()
    report['preflight'] = 'PASS'

    partner = 'ERROR' if chip == 'WARNING' else 'WARNING'
    term = TERMS[CHIPS.index(chip)]
    # The truth table comes from emitted facts, never buildQuery or backend results.
    # A = quoted phrase; B = selected signature. C expands B by OR.
    facts = {'a': 'needle alpha neutral', 'ab': 'needle alpha ' + chip,
        'b': 'other ' + chip, 'neither': 'other neutral',
        'ac': 'needle alpha ' + partner, 'c': 'other ' + partner}
    rows, fixture = {}, []
    for key, content in facts.items():
        marker = 'e2e-' + report['run_id'] + '-' + key
        request = emit_warning(server, marker + ' ' + content)
        row, raw = collected_warning(page, server, marker)
        rows[key] = row
        fixture.append({'fact': key, 'request': request, 'row': row, 'raw_server_line': raw})
    check(len({r['id'] for r in rows.values()}) == len(rows), 'fixture identities overlap')
    lower = min(r['time_ms'] for r in rows.values()) - 1
    upper = max(r['time_ms'] for r in rows.values()) + 1
    check(upper - lower <= 30 * 60_000, 'fixture window exceeds one-minute histogram contract')
    report['evidence'].append({'layer': 'collection', 'items': fixture, 'bounds': [lower, upper]})
    artifacts.start_trace(context)
    page.on('console', lambda message: console_errors.append(message.text) if message.type == 'error' else None)
    page.on('response', lambda response: bad_http.append({'path': urlsplit(response.url).path, 'status': response.status}) if response.status >= 400 else None)
    base = {'source': 'server', 'from': str(lower), 'to': str(upper)}
    search = page.get_by_placeholder(logs['search']['placeholder'], exact=True)
    export = page.get_by_role('button', name=logs['search']['export'], exact=False)
    controls = {name: page.get_by_role('button', name=name, exact=True) for name in CHIPS}
    text = '"needle alpha"'

    def result(action, expression, keys, selected, label, source='server', level=None):
        query = {**base, 'source': source}
        if expression:
            query['q'] = expression
        if level:
            query['level'] = level
        search_query = {**query, 'limit': '500', 'with_total': '1'}
        histogram_query = {**query, 'bucket': '60000'}
        def matches(response, path, expected):
            return response.request.method == 'GET' and urlsplit(response.url).path == path and parse_qs(urlsplit(response.url).query, keep_blank_values=True) == {k: [v] for k, v in expected.items()}
        with page.expect_response(lambda r: matches(r, HISTOGRAM, histogram_query)) as histogram:
            with page.expect_response(lambda r: matches(r, SEARCH, search_query)) as response:
                action()
        check(response.value.status == histogram.value.status == 200, 'query failed: ' + label)
        expected_rows = sorted([rows[key] for key in keys], key=lambda row: row['id'], reverse=True)
        expected = {'items': expected_rows, **({'total': len(keys)} if keys else {})}
        actual = response.value.json()
        check(actual == expected, 'search IDs/content/order/total mismatch: ' + label)
        # Both endpoints must be asked the identical fixed filter: same q, level,
        # source and inclusive window, with node/instance pinned to the fixture's
        # own empty values. A hit count that differs between them cannot be read
        # as one measurement, so compare the filters before the numbers.
        pinned = ('q', 'level', 'source', 'node', 'instance', 'from', 'to')
        def filters(view):
            return {key: view.get(key, '') for key in pinned}
        check(filters(search_query) == filters(histogram_query),
              f'search and histogram were asked different filters: {label}; search={filters(search_query)} histogram={filters(histogram_query)}')
        check(filters(search_query)['source'] and filters(search_query)['from'] and filters(search_query)['to'],
              f'the fixed source and inclusive window are not both read: {label}')
        # node and instance are pinned by being unsent on both endpoints: the
        # fixture WARNs are server-sourced, so every row carries an empty node
        # and instance. Neither endpoint may narrow on a value the fixture does
        # not have, or the two readings would no longer describe one window.
        check('node' not in search_query and 'instance' not in search_query,
              f'a node/instance narrowing appeared on the request: {label}; {sorted(search_query)}')
        check({(row['node'], row['instance']) for row in expected_rows} <= {('', '')},
              f'fixture rows are not the pinned empty node/instance: {label}')
        if keys:
            check(actual.get('total') == len(actual['items']) == len(keys),
                  f'search total differs from the rows it returned: {label}; total={actual.get("total")} rows={len(actual["items"])}')
        counts = Counter(row['time_ms'] // 60000 * 60000 for row in expected_rows)
        # The store returns buckets newest-first, the same order as every other read
        # (logstore.histogramOnce sorts descending and the UI reverses for display).
        # Sorting ascending here only matched while the fixture happened to fall in a
        # single minute; once it straddled two, the expected order was reversed.
        buckets = [{'start_ms': stamp, 'counts': {'warn': count}}
                   for stamp, count in sorted(counts.items(), reverse=True)]
        actual_histogram = histogram.value.json()
        check(actual_histogram == {'items': buckets, 'total': len(buckets)},
              f'histogram mismatch: {label}; expected={buckets} actual={actual_histogram}')
        # The two readings must agree as one measurement. The fixture window is
        # a single histogram bucket, so every matched row lands in the one bar
        # returned, and the histogram total must stay that bar's own count while
        # the search total is the number of matched rows.
        bucket_sum = sum(count for bucket in actual_histogram['items'] for count in bucket['counts'].values())
        check(bucket_sum == len(keys),
              f'histogram bucket sum differs from the matched row count: {label}; buckets={bucket_sum} rows={len(keys)}')
        check(actual_histogram['total'] == len(actual_histogram['items']),
              f'histogram total is not its own bucket count: {label}')
        if keys:
            check(actual['total'] == bucket_sum,
                  f'search total differs from the histogram bucket sum: {label}; search={actual["total"]} buckets={bucket_sum}')
        visible = page.get_by_test_id('log-row')
        expect(visible).to_have_count(len(keys))
        for index, row in enumerate(expected_rows):
            expect(visible.nth(index)).to_contain_text(row['line'])
        # Verify rendered histogram too, not merely its response.
        # A bar publishes role="img" while it is a readout and role="button" once
        # onPick makes it clickable (LogsHistogram.tsx:35). Both are the same bar,
        # and pinning one role would go red the moment the page gains or loses the
        # pick handler, so the bars are addressed by their aria-label instead and
        # the count still has to match the buckets the response carried.
        histogram = page.get_by_test_id('logs-histogram')
        bars = histogram.locator('[role="img"], [role="button"]')
        expect(bars).to_have_count(len(buckets))
        for stamp, count in counts.items():
            label_text = datetime.fromtimestamp(stamp / 1000, timezone.utc).strftime('%Y-%m-%d %H:%M:%S') + ' ' + str(count)
            expect(histogram.locator(f'[aria-label="{label_text}"]')).to_be_visible()
        for name, control in controls.items():
            expect(control).to_have_attribute('aria-pressed', 'true' if name in selected else 'false')
        if keys:
            expect(export).to_be_enabled()
            expect(page.get_by_text(logs['search']['matches'].replace('{{count}}', str(len(keys))), exact=True)).to_be_visible()
        else:
            expect(page.get_by_text(logs['search']['empty'], exact=True)).to_be_visible()
            expect(export).to_be_disabled()
        report['evidence'].append({'layer': 'request', 'scenario': label, 'method': 'GET',
            'search': {'path': SEARCH, 'query': search_query, 'status': 200, 'response': actual},
            'histogram': {'path': HISTOGRAM, 'query': histogram_query, 'status': 200, 'response': actual_histogram}})
        report['evidence'].append({'layer': 'ui', 'scenario': label, 'selected_chips': list(selected),
            'row_ids': [r['id'] for r in expected_rows], 'bucket_sum': sum(counts.values())})
        return search_query, expected

    deep_link = server.base + '/logs?' + urlencode({'from': lower, 'to': upper})
    result(lambda: page.goto(deep_link), '', [], [], 'fixed window, default instance', source='instance')
    result(lambda: choose(page, logs['search']['sourceLabel'], logs['search']['sourceServer']), '', list(rows), [], 'all six server facts')
    # Chip before text: chips-only is a real nonempty predicate, not an empty oracle.
    expect(controls[chip]).to_be_in_viewport(ratio=1)
    controls[chip].focus()
    controls[chip].press('Shift+Tab')
    page.keyboard.press('Tab')
    expect(controls[chip]).to_be_focused()
    result(lambda: controls[chip].press('Space'), term, ['ab', 'b'], [chip], 'keyboard chip first')
    theme_color(controls[chip], 'backgroundColor', 'primary')
    result(lambda: search.fill(text), '(' + text + ') AND ' + term, ['ab'], [chip], 'text added after chip')
    result(lambda: controls[partner].click(), '(' + text + ') AND (' + term + ' OR ' + partner + ')', ['ab', 'ac'], [chip, partner], 'OR expands intersection')
    result(lambda: controls[chip].press('Enter'), '(' + text + ') AND ' + partner, ['ac'], [partner], 'keyboard removes primary')
    theme_color(controls[chip], 'backgroundColor', 'white')
    report['evidence'].append({'layer': 'ui', 'computed_style': {'selected_background': 'theme.colors.primary', 'unselected_background': 'theme.colors.white'}, 'keyboard': ['Tab', 'Space', 'Enter']})
    result(lambda: controls[partner].click(), text, ['a', 'ab', 'ac'], [], 'all chips removed restores text')
    # FTS syntax is its own dimension: these two expressions are only meaningful
    # if the real backend parser runs them. A literal/substring scan would find
    # nothing for the trailing-star prefix and would not honour NOT.
    result(lambda: search.fill('alph*'), 'alph*', ['a', 'ab', 'ac'], [], 'FTS prefix operator reaches the parser')
    result(lambda: search.fill(term + ' NOT ' + text), term + ' NOT ' + text, ['b'], [], 'FTS NOT with a quoted phrase')
    # A dangling operator is invalid FTS5. Whatever the backend does with it, it
    # must not silently drop the term and return the whole window: the known
    # facts above make a "returned everything" degradation visible here.
    result(lambda: search.fill(text + ' AND'), text + ' AND', [], [], 'dangling operator never returns the whole window')
    result(lambda: search.fill(text), text, ['a', 'ab', 'ac'], [], 'restore phrase after FTS operators')
    # ZSTAC-89654 regression form: text AND (a parenthesised chip OR-group) must
    # keep the known chip-only records. Space-joining these tokens would make the
    # backend reject the expression and return nothing, which is the exact
    # degradation the contract forbids.
    group = f'{term} AND ({term} OR {partner})'
    result(lambda: search.fill(group), group, ['ab', 'b'], [], 'ZSTAC-89654 AND over an OR group keeps known hits')
    result(lambda: search.fill(text), text, ['a', 'ab', 'ac'], [], 'restore phrase after the OR group')
    # Text before chip yields exactly the same row identity as chip before text.
    result(lambda: controls[chip].click(), '(' + text + ') AND ' + term, ['ab'], [chip], 'text first then chip')
    # Each predicate is nonempty separately; their intersection must be empty.
    only_a = '"e2e-' + report['run_id'] + '-a"'
    result(lambda: search.fill(only_a), '(' + only_a + ') AND ' + term, [], [chip], 'nonempty predicates have empty intersection')
    result(lambda: controls[chip].click(), only_a, ['a'], [], 'remove chip recovers A-only')
    result(lambda: search.fill(text), text, ['a', 'ab', 'ac'], [], 'restore phrase')
    result(lambda: controls[chip].click(), '(' + text + ') AND ' + term, ['ab'], [chip], 'restore intersection')
    result(lambda: search.fill(''), term, ['ab', 'b'], [chip], 'clear text restores chip-only')
    result(lambda: controls[partner].click(), '(' + term + ' OR ' + partner + ')', ['ab', 'b', 'ac', 'c'], [chip, partner], 'chips-only OR')
    result(lambda: controls[partner].click(), term, ['ab', 'b'], [chip], 'remove partner restores primary')
    result(lambda: search.fill(text), '(' + text + ') AND ' + term, ['ab'], [chip], 'restore before refresh')
    # A severity dropdown is distinct from signature matching: all facts are WARN,
    # even when their literal message contains ERROR/Traceback/etc.
    result(lambda: choose(page, logs['search']['levelPlaceholder'], '≥ error'), '(' + text + ') AND ' + term, [], [chip], 'minimum error excludes WARN facts', level='error')
    query, expected = result(lambda: choose(page, logs['search']['levelPlaceholder'], '≥ warn'), '(' + text + ') AND ' + term, ['ab'], [chip], 'minimum warn restores same fact', level='warn')
    independent = page.request.get(server.base + SEARCH + '?' + urlencode(query))
    check(independent.status == 200 and independent.json() == expected, 'independent persisted facts differ')
    # The severity dropdown and the quick-word label are two separate query
    # parameters, and passing with level=warn alone must not stand in for the
    # quick word joining q. Drop the quick word first, then press it again, and
    # read both parameters off the request each time.
    severity_only, severity_rows = result(lambda: controls[chip].click(), text, ['a', 'ab', 'ac'], [],
        'severity alone, quick word removed', level='warn')
    check(severity_only['level'] == 'warn' and severity_only['q'] == text,
          f'level and q are not two independent parameters: {severity_only}')
    check(term not in severity_only['q'],
          f'quick word {term} is in q although its label is not pressed: {severity_only["q"]}')
    expect(controls[chip]).to_have_attribute('aria-pressed', 'false')
    joined, joined_rows = result(lambda: controls[chip].click(), '(' + text + ') AND ' + term, ['ab'], [chip],
        'quick word joins q, severity parameter untouched', level='warn')
    check(joined['level'] == severity_only['level'] == 'warn',
          f'pressing a quick word changed the severity parameter: {joined}')
    check(joined['q'] != severity_only['q'] and term in joined['q'],
          f'quick word did not enter q independently of level: {joined["q"]}')
    expect(controls[chip]).to_have_attribute('aria-pressed', 'true')
    check({row['id'] for row in severity_rows['items']} != {row['id'] for row in joined_rows['items']},
          'the severity-only and quick-word readings returned the same rows, so one cannot be told from the other')
    report['evidence'].append({'layer': 'ui', 'scenario': 'level and quick word are separate parameters',
        'level_only': {'q': severity_only['q'], 'level': severity_only['level'],
                       'row_ids': [row['id'] for row in severity_rows['items']]},
        'quick_word': {'term': term, 'q': joined['q'], 'level': joined['level'],
                       'row_ids': [row['id'] for row in joined_rows['items']]},
        'assertions': ['level=warn passes without the quick word in q',
                       'pressing the quick word adds it to q and leaves level at warn',
                       'the two readings return different row identities']})
    result(page.reload, '', [], [], 'reload resets unsaved controls', source='instance')
    expect(search).to_have_value('')
    result(lambda: search.fill(text), text, [], [], 'reapply text', source='instance')
    result(lambda: choose(page, logs['search']['sourceLabel'], logs['search']['sourceServer']), text, ['a', 'ab', 'ac'], [], 'reapply source')
    _, restored = result(lambda: controls[chip].click(), '(' + text + ') AND ' + term, ['ab'], [chip], 'reapply primary after reload')
    check(restored == expected, 'refresh changed persisted log identity')
    report['evidence'].append({'layer': 'persistence', 'independent_get': expected, 'refresh_read': restored})
    check(not errors and not console_errors and not bad_http, f'browser errors: {errors}, {console_errors}, {bad_http}')
    check(page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'page overflows horizontally')
    page.screenshot(path=artifacts.directory / 'quick-controls.png')
    report['browser_errors'] = {'page': errors, 'console': console_errors, 'http': bad_http}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--artifact-dir', type=Path)
    parser.add_argument('--locale', choices=['en', 'zh'], default='en')
    parser.add_argument('--chip', choices=CHIPS, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / 'test/e2e/coverage/log-quick-controls.json'
    manifest = json.loads(manifest_path.read_text())
    check(manifest['contract_id'] == CONTRACT and manifest['test_function'] == test_quick_controls.__name__, 'manifest binding drift')
    check(manifest['parameters'] == list(CHIPS), 'chip selection drift')
    files = [Path(__file__), manifest_path, *(ROOT / name for name in PRODUCT_FILES),
        ROOT / 'test/e2e/log_filter_export.py', ROOT / 'test/e2e/local_browser_fixture.py', ROOT / 'test/e2e/platform_token_read_revoke.py']
    report = {'contract': CONTRACT, 'case': manifest['name'], 'parameter': args.chip, 'status': 'NOT_RUN', 'run_id': run_id,
        'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
        'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
        'lane': 'local-real-server-sqlite', 'identity': 'admin', 'locale': args.locale, 'timezone': 'UTC',
        'viewport': {'width': 1440, 'height': 900}, 'selection': [CONTRACT + '/' + args.chip], 'retries': 0, 'evidence': [],
        'source_digests': {str(path.relative_to(ROOT)): digest(path) for path in files},
        'command': ['python3', 'test/e2e/log_quick_controls.py', *sys.argv[1:]]}
    try:
        report['binary_sha256'] = digest(args.server_bin)
        report['health'] = server.start()
        report['base_url'], report['owned_server_pid'] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            context = browser.new_context(viewport=report['viewport'], locale='zh-CN' if args.locale == 'zh' else 'en-US', timezone_id='UTC')
            report['browser_version'] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_quick_controls(page, context, server, artifacts, report, args.locale, args.chip)
                report['status'] = 'CHECKS_PASSED'
            except Exception:
                report['status'], report['failure'] = 'FAIL_TEST', traceback.format_exc()
                if report.get('preflight') == 'PASS' and not any(s in page.content() for s in artifacts.secrets if len(s) > 8):
                    page.screenshot(path=artifacts.directory / 'failure.png')
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = traceback.format_exc()
    finally:
        report['server_stopped'] = server.stop()
        if not report['server_stopped']:
            report['status'] = 'FAIL_TEST'
        else:
            report['evidence'].append({'layer': 'cleanup', 'server_stopped': True, 'scope': 'owned server/port lock; private SQLite retained for seven-day diagnosis'})
        log = artifacts.private / 'server.log'
        if log.exists():
            (artifacts.directory / 'server.log').write_text(artifacts.redact(log.read_text()))
        artifacts.write_json('report.json', report)
        try:
            check(report['source_digests'] == {str(path.relative_to(ROOT)): digest(path) for path in files}, 'source changed during execution')
            artifacts.scan()
            if report['status'] == 'CHECKS_PASSED':
                check(set(manifest['required_evidence']) <= {e['layer'] for e in report['evidence']}, 'missing required evidence')
                report['status'] = 'PASS'
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_error'] = traceback.format_exc()
        artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
