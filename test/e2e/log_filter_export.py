#!/usr/bin/env python3
"""Logs / real server warning collection, browser filters and exact CSV export.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-log-filter-export  # builds, then runs en and zh separately
After build: python3 test/e2e/log_filter_export.py --locale en  # or zh
Fixture tests: python3 -m unittest discover -s test/e2e -p 'test_log_filter_export.py'
Requires Python Playwright and system Chrome; owns a fresh LocalServer/SQLite.
No GPU, agent or container; only the real server WARN collector is exercised.
This entry is independent of test-e2e-local; invocation does not imply CI selection.
Reports/CSV and private diagnosis state: secrets/e2e/<runId>/; stop owned server,
retain failure evidence, and remove only this run directory after diagnosis within seven days.
"""
from __future__ import annotations

import argparse
import csv
from datetime import datetime, timezone
from http.client import HTTPConnection
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import traceback
from urllib.parse import parse_qs, urlencode, urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check
from log_filter_export_checks import (
    exercise_inclusive_boundaries, exercise_text_inputs, option_inventory, text_inventory)

CONTRACT = 'gpuworks/logs/server-warning-filter-export'
SEARCH = '/api/v1/logs/search'
HISTOGRAM = '/api/v1/logs/histogram'
CSV_FIELDS = ['TimeMS', 'Level', 'Source', 'Instance', 'Node', 'Line']
PRODUCT_FILES = ['ui/src/pages/LogsPage.tsx', 'ui/src/i18n/locales/en/logs.json', 'ui/src/i18n/locales/zh/logs.json']


def validate_csv(raw, entries):
    """Decode downloaded bytes independently of the product CSV serializer."""
    text = raw.decode('utf-8-sig')  # BOM is optional in this product's contract.
    rows = list(csv.reader(io.StringIO(text, newline=''), strict=True))
    expected = [CSV_FIELDS] + [[str(e[k]) for k in ('time_ms', 'level', 'source', 'instance', 'node', 'line')] for e in entries]
    check(rows == expected, 'CSV columns/rows differ from the actual loaded entries')
    return rows


def emit_warning(server, marker):
    """Exercise real WS validation; the server's own WARN collector supplies rows."""
    target = urlsplit(server.base)
    conn = HTTPConnection(target.hostname, target.port, timeout=10)
    try:
        conn.putrequest('GET', '/api/v1/agent/connect')
        conn.putheader('Connection', 'Upgrade')
        conn.putheader('Upgrade', 'websocket')
        conn.putheader('Sec-WebSocket-Version', marker.encode('utf-8'))
        conn.endheaders()
        response = conn.getresponse()
        body = response.read().decode('utf-8')
        check(response.status == 400, 'invalid WebSocket version did not produce a real rejection')
        return {'method': 'GET', 'path': '/api/v1/agent/connect', 'version_marker': marker,
            'status': response.status, 'response': body}
    finally:
        conn.close()


def collected_warning(page, server, marker):
    query = {'source': 'server', 'q': '"' + marker + '"', 'with_total': '1'}
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        response = page.request.get(server.base + SEARCH + '?' + urlencode(query))
        check(response.status == 200, 'warning fixture query failed')
        value = response.json()
        check(isinstance(value.get('items'), list), 'invalid log envelope')
        if value['items']:
            check(len(value['items']) == 1 and value.get('total') == 1, 'warning marker is not unique')
            row = value['items'][0]
            check(row['source'] == 'server' and row['level'] == 'warn' and marker in row['line'], 'wrong collected warning')
            check(row['id'] > 0 and row['time_ms'] > 0 and row['node'] == '' and row['instance'] == '', 'invalid server log identity')
            raw_lines = [line for line in (server.work / 'server.log').read_text().splitlines() if marker in line and 'level=WARN' in line]
            check(len(raw_lines) == 1, 'raw server WARN evidence missing or duplicated')
            return row, raw_lines[0]
        page.evaluate('() => new Promise(resolve => requestAnimationFrame(resolve))')
    raise AssertionError('real WARN was not collected before deadline')


def choose(page, control_label, option_label):
    control = page.get_by_role('combobox', name=control_label, exact=True)
    control.press('ArrowDown')
    # AntD only mirrors the active options into its virtual ARIA list; the
    # visible business label remains the actual pointer-selection surface.
    option = page.get_by_text(option_label, exact=True).filter(visible=True)
    expect(option).to_have_count(1)
    option.click()
    expect(control).to_have_attribute('aria-expanded', 'false')
    expect(page.get_by_text(option_label, exact=True).filter(visible=True)).to_have_count(1)


# Mainline: 两条真实server WARN -> UI按marker/来源/级别/固定时间与快捷词过滤 -> 搜索和直方图一致 -> GET与刷新回读同一日志 -> 下载CSV逐字段等于当前行集。
# Contract: gpuworks/logs/server-warning-filter-export
def test_log_filter_export(page, context, server, artifacts, report, locale):
    texts = ROOT / 'ui/src/i18n/locales' / locale
    auth = json.loads((texts / 'auth.json').read_text())
    logs = json.loads((texts / 'logs.json').read_text())
    errors, console_errors, bad_http, downloads = [], [], [], []
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
    if report.get('check_family', 'all') in ('all', 'text-inventory'):
        text_inventory(page, logs, report)
    if report.get('check_family', 'all') in ('all', 'selector-inventory'):
        option_inventory(page, logs, report)

    # Fixture traffic is outside the browser and tracing; expected 400 is not a UI failure.
    target_marker = 'e2e-' + report['run_id'] + '-target'
    other_marker = 'e2e-' + report['run_id'] + '-other'
    fixture = []
    for marker in (target_marker, other_marker):
        request = emit_warning(server, marker + ' WARNING 中文,"CSV"')
        row, raw_line = collected_warning(page, server, marker)
        check('WARNING' in row['line'] and 'ERROR' not in row['line'], 'quick-filter fixture lacks a warning-only match')
        check('中文' in row['line'] and ',' in row['line'] and '"' in row['line'], 'CSV fixture lacks encoding/escaping characters')
        fixture.append({'request': request, 'row': row, 'raw_server_line': raw_line})
    target, other = [item['row'] for item in fixture]
    check(target['id'] != other['id'], 'fixture warnings share identity')
    report['evidence'].append({'layer': 'collection', 'source': 'real server selflog WARN collector', 'items': fixture})
    artifacts.write_json('fixture-warnings.json', fixture)
    artifacts.start_trace(context)
    page.on('console', lambda message: console_errors.append(message.text) if message.type == 'error' else None)
    page.on('response', lambda response: bad_http.append({'path': urlsplit(response.url).path, 'status': response.status}) if response.status >= 400 else None)
    page.on('download', lambda download: downloads.append(download.suggested_filename))
    lower, upper = target['time_ms'] - 1, target['time_ms'] + 1
    query = '"' + target_marker + '"'
    base_query = {'source': 'instance', 'from': str(lower), 'to': str(upper), 'limit': '500', 'with_total': '1'}
    export = page.get_by_role('button', name=re.compile(re.escape(logs['search']['export']) + '$'))
    search = page.get_by_placeholder(logs['search']['placeholder'], exact=True)
    # 2.7: a log line is text, and it is rendered as text. An extra warning is collected whose line
    # carries markup, and the row is then required to show those characters literally - the probe
    # element must not exist, and the line must not have been parsed into DOM. The marker deliberately
    # avoids quotes so the fixture's own search expression stays well formed.
    markup_probe = 'e2e-' + report['run_id'] + '-markup'
    markup_marker = markup_probe + ' <b>bold</b> <script>window.__e2eXss=1</script> &amp; <img src=x onerror=1>'
    emit_warning(server, markup_marker)
    markup_row, _ = collected_warning(page, server, markup_probe)
    # The collector quotes the offending protocol value, so the stored line carries the marker inside
    # quotes rather than equalling it. What matters is that the markup survived storage unchanged -
    # no entity doubling on the way in - and that rendering then shows it as text.
    check(markup_marker in markup_row['line'],
          f"the stored line was altered before rendering: {markup_row['line']!r}")
    page.reload()
    expect(page.get_by_role('heading', name=logs['title'], exact=True)).to_be_visible(timeout=20000)
    # The fixture's own range is a fixed window in the past, so the fresh warning is not inside it.
    # The probe is searched with the real request the page sends after a reload, which uses the
    # default range, and the search is driven through the box rather than the API so the rendering
    # under test is what the page produced.
    choose(page, logs['search']['sourceLabel'], logs['search']['sourceServer'])
    with page.expect_response(lambda r: urlsplit(r.url).path == SEARCH
                              and 'source=server' in r.url.split('?')[-1]) as markup_response:
        search.fill(markup_probe)
    check(markup_response.value.status == 200, 'markup fixture query failed')
    check(markup_probe in parse_qs(urlsplit(markup_response.value.url).query, keep_blank_values=True).get('q', [''])[0],
          'the markup probe was not the keyword that was sent')
    # The row is rendered as several spans (the highlight splits it), so the text is located by the
    # element whose textContent is the whole line rather than by an exact text match.
    stored_markup = markup_row['line']
    for _ in range(100):
        if page.evaluate("""(line) => [...document.querySelectorAll('*')]
                .some(n => n.textContent === line)""", stored_markup):
            break
        page.wait_for_timeout(150)
    rendering = page.evaluate('''(line) => {
        const nodes = [...document.querySelectorAll('*')].filter(n => n.textContent === line);
        const holder = nodes.length ? nodes[nodes.length - 1] : null;
        return {found: !!holder, tag: holder ? holder.tagName : null,
                probes: document.querySelectorAll('.log-rows b, .log-rows script, .log-rows img').length,
                executed: typeof window.__e2eXss !== 'undefined'};
    }''', stored_markup)
    check(rendering['found'], f"the markup warning was not rendered: {rendering}")
    check(rendering['probes'] == 0, f"markup in a log line became DOM: {rendering}")
    check(rendering['executed'] is False, f"a log line executed script: {rendering}")
    report['evidence'].append({'layer': 'ui', 'scenario': 'log-line-markup-escaped',
                               'line': markup_row['line'], 'rendering': rendering})

    def result(action, expected_query, expected_rows, label):
        def matches(response):
            return response.request.method == 'GET' and urlsplit(response.url).path == SEARCH and parse_qs(urlsplit(response.url).query, keep_blank_values=True) == {k: [v] for k, v in expected_query.items()}
        with page.expect_response(matches) as response:
            action()
        actual = response.value
        check(actual.status == 200, 'browser search failed: ' + label)
        value = actual.json()
        expected = {'items': expected_rows, **({'total': len(expected_rows)} if expected_rows else {})}
        check(value == expected, 'search envelope/IDs/times/content/total mismatch: ' + label)
        rows = page.get_by_test_id('log-row')
        expect(rows).to_have_count(len(expected_rows))
        if expected_rows:
            row = rows.filter(has_text=target_marker)
            expect(row).to_have_count(1)
            expect(row).to_contain_text(target['line'])
            expect(row.get_by_test_id('log-level')).to_have_text('warn')
            stamp = datetime.fromtimestamp(target['time_ms'] / 1000, timezone.utc).strftime('%Y-%m-%d %H:%M:%S')
            expect(row.get_by_text(stamp, exact=True)).to_be_visible()
            expect(page.get_by_text(logs['search']['matches'].replace('{{count}}', '1'), exact=True)).to_be_visible()
            expect(page.get_by_text(other['line'], exact=True)).to_have_count(0)
            expect(export).to_be_enabled()
        else:
            expect(page.get_by_text(logs['search']['empty'], exact=True)).to_be_visible()
            expect(export).to_be_disabled()
        report['evidence'].append({'layer': 'request', 'scenario': label, 'method': 'GET', 'path': SEARCH,
            'query': expected_query, 'status': 200, 'response': value})
        report['evidence'].append({'layer': 'ui', 'scenario': label, 'row_count': len(expected_rows),
            'timestamp_zone': 'UTC', 'source_control': expected_query['source'], 'export_enabled': bool(expected_rows)})
        return value

    # UI selects a supported fixed-time deep link, then combines keyword/source/level.
    deep_link = server.base + '/logs?' + urlencode({'from': lower, 'to': upper})
    result(lambda: page.goto(deep_link), base_query, [], 'custom deep link defaults to instance')
    expect(page.get_by_text(logs['range']['custom'], exact=True)).to_be_visible()
    expect(page.get_by_role('switch', name=logs['search']['autoRefresh'], exact=True)).not_to_be_checked()
    result(lambda: search.fill(query), {**base_query, 'q': query}, [], 'target keyword with default source')
    result(lambda: choose(page, logs['search']['sourceLabel'], logs['search']['sourceServer']),
        {**base_query, 'source': 'server', 'q': query}, [target], 'server source matches target')
    selected_query = {**base_query, 'source': 'server', 'q': query, 'level': 'warn'}
    loaded = result(lambda: choose(page, logs['search']['levelPlaceholder'], '≥ warn'), selected_query, [target], 'combined keyword source level time')
    independent = page.request.get(server.base + SEARCH + '?' + urlencode(selected_query))
    check(independent.status == 200 and independent.json() == loaded, 'independent query differs from browser response')
    report['evidence'].append({'layer': 'persistence', 'independent_read': loaded, 'time_semantics': 'from <= time_ms <= to', 'bounds': [lower, upper]})

    # Real chip clicks must preserve the text AND (chip OR chip) contract in both APIs.
    def quick_result(action, expression, expected_rows, label):
        histogram_query = {k: v for k, v in selected_query.items() if k not in ('limit', 'with_total')}
        histogram_query.update(q=expression, bucket='60000')
        def matches(response):
            return response.request.method == 'GET' and urlsplit(response.url).path == HISTOGRAM and parse_qs(urlsplit(response.url).query, keep_blank_values=True) == {k: [v] for k, v in histogram_query.items()}
        with page.expect_response(matches) as histogram_response:
            result(action, {**selected_query, 'q': expression}, expected_rows, label)
        actual = histogram_response.value
        check(actual.status == 200, 'browser histogram failed: ' + label)
        buckets = [{'start_ms': target['time_ms'] // 60000 * 60000, 'counts': {'warn': 1}}] if expected_rows else []
        value = actual.json()
        check(value == {'items': buckets, 'total': len(buckets)}, 'histogram differs from known search matches: ' + label)
        report['evidence'].append({'layer': 'request', 'scenario': label, 'method': 'GET', 'path': HISTOGRAM,
            'query': histogram_query, 'status': 200, 'response': value})

    warning_chip = page.get_by_test_id('log-quick-WARNING')
    error_chip = page.get_by_test_id('log-quick-ERROR')
    quick_result(warning_chip.click, '(' + query + ') AND WARNING', [target], 'quoted text plus warning chip')
    quick_result(error_chip.click, '(' + query + ') AND (WARNING OR ERROR)', [target], 'quoted text plus two OR chips')
    or_query = query + ' OR "e2e-' + report['run_id'] + '-absent"'
    quick_result(lambda: search.fill(or_query), '(' + or_query + ') AND (WARNING OR ERROR)', [target], 'free-text OR grouped before chips')
    quick_result(warning_chip.click, '(' + or_query + ') AND ERROR', [], 'free-text OR cannot bypass nonmatching chip')
    quick_result(error_chip.click, or_query, [target], 'clearing chips restores original free-text OR')
    quick_result(lambda: search.fill(query), query, [target], 'restore quoted text before export')

    # Reverse the input order: chips first, then text. Each observation uses
    # the same independently collected row and fixed window, not merely q.
    if report.get('check_family', 'all') in ('all', 'composition'):
        report['last_step'] = 'logs-chip-first-composition'
        result(lambda: search.fill(''), {k: v for k, v in selected_query.items() if k != 'q'},
               [target], 'clear text before chip-first path')
        quick_result(warning_chip.click, 'WARNING', [target], 'warning chip before free text')
        quick_result(error_chip.click, '(WARNING OR ERROR)', [target], 'two OR chips before free text')
        quick_result(lambda: search.fill(or_query), '(' + or_query + ') AND (WARNING OR ERROR)',
                     [target], 'chip-first free-text OR grouped before chip AND')
        quick_result(warning_chip.click, '(' + or_query + ') AND ERROR', [],
                     'chip-first removing matching warning leaves the error-only intersection empty')
        quick_result(error_chip.click, or_query, [target], 'chip-first clear chips restores text query')
        quick_result(lambda: search.fill(query), query, [target], 'restore target after both input orders')
        report['chip_input_orders'] = {'text_first': True, 'chips_first': True,
                                      'empty_and_known_hit_asserted': True, 'remove_and_clear': True}

    # Each empty intersection disables export; restoring that filter recovers the same fact.
    result(lambda: choose(page, logs['search']['sourceLabel'], logs['search']['sourceAgent']),
        {**selected_query, 'source': 'agent'}, [], 'wrong source excludes target')
    result(lambda: choose(page, logs['search']['sourceLabel'], logs['search']['sourceServer']), selected_query, [target], 'restore server source')
    result(lambda: choose(page, logs['search']['levelPlaceholder'], '≥ error'), {**selected_query, 'level': 'error'}, [], 'error minimum excludes warning')
    result(lambda: choose(page, logs['search']['levelPlaceholder'], '≥ warn'), selected_query, [target], 'restore warning minimum')
    absent = '"e2e-' + report['run_id'] + '-absent"'
    result(lambda: search.fill(absent), {**selected_query, 'q': absent}, [], 'nonexistent keyword')
    loaded = result(lambda: search.fill(query), selected_query, [target], 'restore target keyword')
    # 2.3: the page size is the field with a length contract here, and its boundary behaviour is
    # asserted rather than its happy path. The store clamps a request to a default below the floor and
    # to a ceiling above it, so the four values around those thresholds are sent and the number of rows
    # that comes back is compared with what the contract says - not with what a caller might prefer.
    # Counting is in whole rows, which is the unit the API documents.
    boundary_warning = 'e2e-' + report['run_id'] + '-boundary'
    for _ in range(3):
        emit_warning(server, boundary_warning)
    # The collector is asynchronous, so the fixture is required to be visible before the boundaries
    # are probed - otherwise a page size would be judged against an empty match set.
    settle = time.monotonic() + 20
    while time.monotonic() < settle:
        seen = page.request.get(server.base + SEARCH + '?' + urlencode(
            {'source': 'server', 'q': boundary_warning, 'limit': '10', 'with_total': '1'}))
        if seen.status == 200 and len((seen.json().get('items') or [])) >= 3:
            break
        time.sleep(0.25)
    else:
        raise AssertionError('the boundary warnings never became searchable')
    # A page size is a ceiling, not a promise: a request below the match count returns everything
    # there is. So the floor case asserts the whole match set rather than the default's numeric value,
    # and the two ceiling cases assert that a page far above the match count still returns all of it.
    matched = 3
    limit_cases = [
        ('below-floor', '0', matched, 'clamped up to the default, which exceeds the match count'),
        ('floor', '1', 1, 'one row, and the page reports itself truncated'),
        ('ceiling', '5000', matched, 'accepted as sent; the match count fits inside it'),
        ('above-ceiling', '5001', matched, 'clamped down to the ceiling, no error'),
    ]
    limit_evidence = []
    for label, value, expected_rows, note in limit_cases:
        probe = {'source': 'server', 'q': boundary_warning, 'limit': value, 'with_total': '1'}
        answer = page.request.get(server.base + SEARCH + '?' + urlencode(probe))
        check(answer.status == 200, f'limit {value} was rejected: {answer.status}')
        body = answer.json()
        rows = len(body.get('items') or [])
        check(rows == expected_rows,
              f'limit {value} returned {rows} rows, the contract says {expected_rows}: {note}')
        # Only a page that came back full while more matches wait behind it is truncated. The ceiling
        # cases return everything there is, so requiring the marker there would be requiring a lie.
        if expected_rows is not None and rows < matched:
            check(answer.headers.get('x-logs-truncated') == '1',
                  f'limit {value} returned a full page without marking it truncated')
        else:
            check(answer.headers.get('x-logs-truncated') is None,
                  f'limit {value} marked a complete page as truncated')
        limit_evidence.append({'value': value, 'rows': rows, 'note': note,
                               'truncated': answer.headers.get('x-logs-truncated')})
    report['evidence'].append({'layer': 'request', 'scenario': 'page-size-boundaries',
                               'unit': 'rows', 'cases': limit_evidence})
    # 2.4: the keyword control is required to carry the characters a log line actually contains, and
    # what it sends is asserted rather than what it displays. Each shape is typed and the query that
    # reaches the server is compared exactly, so a control that accepted the text but mangled,
    # dropped or silently stripped part of it fails here instead of only looking fine in the box.
    for label, value, sent in (
            ('chinese', '错误 警告', '错误 警告'),
            ('punctuation', 'ERROR: "quoted" (paren) [bracket] {brace}',
             'ERROR: "quoted" (paren) [bracket] {brace}'),
            ('percent-and-ampersand', 'a%b&c=d', 'a%b&c=d'),
            ('single-line-newline-collapsed', 'first line\nsecond line', 'first line second line')):
        result(lambda v=value: search.fill(v), {**selected_query, 'q': sent}, [], f'keyword characters: {label}')
        report['evidence'].append({'layer': 'request', 'scenario': f'keyword-{label}',
                                   'typed': value, 'sent': sent})
    # Back to a query that matches, so the export below is not disabled by the last probe.
    loaded = result(lambda: search.fill(query), selected_query, [target], 'restore target after characters')

    # Consume the real download bytes, including Chinese and CSV escaping, not just its name.
    with page.expect_download() as download_event:
        export.click()
    download = download_event.value
    check(download.failure() is None, 'CSV download failed')
    destination = artifacts.directory / 'filtered-logs.csv'
    download.save_as(destination)
    raw = destination.read_bytes()
    parsed = validate_csv(raw, loaded['items'])
    check('中文'.encode('utf-8') in raw and b'""' in raw, 'download lacks actual UTF-8/CSV escaping bytes')
    check(download.suggested_filename.startswith('gpuworks-logs-') and download.suggested_filename.endswith('.csv'), 'CSV filename contract changed')
    report['evidence'].append({'layer': 'download', 'file': destination.name, 'sha256': digest(destination),
        'bytes': len(raw), 'encoding': 'UTF-8', 'bom': raw.startswith(b'\xef\xbb\xbf'), 'parsed_fields': parsed,
        'contract': 'current loaded entries; not server-wide export'})

    result(page.reload, base_query, [], 'reload resets source query level; preserves URL custom time')
    expect(search).to_have_value('')
    expect(page.get_by_text(logs['search']['sourceInstance'], exact=True)).to_be_visible()
    expect(page.get_by_role('switch', name=logs['search']['autoRefresh'], exact=True)).not_to_be_checked()
    result(lambda: search.fill(query), {**base_query, 'q': query}, [], 'reapply keyword after reload')
    result(lambda: choose(page, logs['search']['sourceLabel'], logs['search']['sourceServer']),
        {**base_query, 'source': 'server', 'q': query}, [target], 'reapply server after reload')
    restored = result(lambda: choose(page, logs['search']['levelPlaceholder'], '≥ warn'), selected_query, [target], 'same persisted row after reload')
    check(restored == loaded, 'refresh/reapply changed authoritative log identity')
    report['evidence'].append({'layer': 'persistence', 'refresh_resets_unstored_filters': True,
        'custom_url_preserved': True, 'reapplied_response': restored})
    if report.get('check_family', 'all') in ('all', 'text-inputs', 'whitespace', 'clipboard'):
        exercise_text_inputs(page, context, server, logs, report, selected_query, target, result)
    if report.get('check_family', 'all') in ('all', 'time-boundaries'):
        exercise_inclusive_boundaries(page, server, logs, report, target, query, result, choose)
    check(len(downloads) == 1, 'unexpected downloads')
    check(not errors and not console_errors and not bad_http, f'browser errors: {errors}, {console_errors}, {bad_http}')
    check(page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'logs page overflows horizontally')
    page.screenshot(path=artifacts.directory / 'filtered-logs.png')
    report['browser_errors'] = {'page': errors, 'console': console_errors, 'http': bad_http}
    report['evidence'].append({'layer': 'cleanup', 'scope': 'isolated LocalServer/SQLite only; stopped by runner',
        'retained_logs': 'owned run directory for diagnosis; no shared server resources modified'})

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument('--check-family', default='all', choices=(
        'all', 'text-inventory', 'selector-inventory', 'text-inputs', 'whitespace', 'clipboard',
        'time-boundaries', 'composition'),
        help='Isolate a new-check family for product-mutation diagnostics. Formal en/zh evidence must use all.')
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / "test/e2e/coverage/log-filter-export.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_log_filter_export.__name__, "manifest binding drift")
    report = {"contract": CONTRACT, "case": manifest["name"], "status": "NOT_RUN", "run_id": run_id,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin browser / real server warning collector", "locale": args.locale,
        "viewport": {"width": 1440, "height": 900}, "timezone": "UTC", "selection": [CONTRACT], "retries": 0, "evidence": [],
        "check_family": args.check_family, "probe_only": args.check_family != 'all',
        "test_sha256": digest(Path(__file__)), "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/platform_token_read_revoke.py"), "coverage_manifest_sha256": digest(manifest_path),
        "batch_checks_sha256": digest(ROOT / "test/e2e/log_filter_export_checks.py"),
        "product_sha256": {name: digest(ROOT / name) for name in PRODUCT_FILES},
        "command": ["python3", "test/e2e/log_filter_export.py", *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"], report["owned_server_pid"] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_log_filter_export(page, context, server, artifacts, report, args.locale)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "missing required evidence")
                check(report["test_sha256"] == digest(Path(__file__)) and report["coverage_manifest_sha256"] == digest(manifest_path), "test or manifest changed during execution")
                check(report['batch_checks_sha256'] == digest(ROOT / 'test/e2e/log_filter_export_checks.py'), 'batch checks changed during execution')
                check(report["product_sha256"] == {name: digest(ROOT / name) for name in PRODUCT_FILES}, "product source changed during execution")
                report["status"] = "PASS"
            except Exception:
                report["status"], report["failure"] = "FAIL_TEST", traceback.format_exc()
                if report.get("preflight") == "PASS" and not any(s in page.content() for s in artifacts.secrets if len(s) > 8):
                    page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        report["private_state"] = "isolated DB/logs retained for diagnosis; remove only this run directory within 7 days"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts"
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
