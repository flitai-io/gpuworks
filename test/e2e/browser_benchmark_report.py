#!/usr/bin/env python3
"""Read-only browser acceptance of an existing run's PNG/offline HTML exports.

The caller provides a real server with its embedded UI from the build under test
and a terminal benchmark containing at least one completed point. No management
route, database, agent, benchmark, or inference response is mocked or mutated.
Only login/logout create and revoke this runner's own management session.

  GPUWORKS_E2E_PASSWORD=... python test/e2e/browser_benchmark_report.py \
    --base-url http://127.0.0.1:8802 --run-id 7 --build-sha <server-build-sha> \
    --locale en --output secrets/e2e/<unique-run-id>/report-export-en

Run separately with --locale zh. This focused case proves report rendering and
export of already persisted facts, not benchmark creation, inference execution,
GPU accuracy, or complete Evaluation-page coverage. Failure/empty-state and
assertion-sensitivity checks must be recorded separately under the E2E standard.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import struct
import subprocess
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import parse_qs, urlsplit
from uuid import uuid4

from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parents[2]
CASE = 'benchmark-report-readonly-visual-export'


def png_data(data):
    return 'data:image/png;base64,' + base64.b64encode(data).decode()


def cropped_reference(page, screenshot, bounds):
    encoded = page.evaluate('''async ([src, bounds]) => {
        const img = new Image(); img.src = src; await img.decode();
        const canvas = document.createElement('canvas');
        canvas.width = bounds.width; canvas.height = bounds.height;
        const ctx = canvas.getContext('2d');
        ctx.drawImage(img, bounds.x, bounds.y, bounds.width, bounds.height,
                      0, 0, bounds.width, bounds.height);
        return canvas.toDataURL('image/png').split(',')[1];
    }''', [png_data(screenshot), bounds])
    return base64.b64decode(encoded)


def compare_pixels(page, reference, rendered):
    """Use the browser's canvas so the runner needs no image-processing package."""
    return page.evaluate('''async ([a, b]) => {
        const images = await Promise.all([a,b].map(async src => {
            const img = new Image(); img.src = src; await img.decode(); return img;
        }));
        const [width,height] = [images[0].naturalWidth, images[0].naturalHeight];
        const pixels = images.map(img => {
            const canvas = document.createElement('canvas');
            canvas.width = width; canvas.height = height;
            const ctx = canvas.getContext('2d'); ctx.drawImage(img,0,0,width,height);
            return ctx.getImageData(0,0,width,height).data;
        });
        let difference = 0;
        for (let i=0;i<pixels[0].length;i++) difference += Math.abs(pixels[0][i]-pixels[1][i]);
        return difference / pixels[0].length;
    }''', [png_data(reference), png_data(rendered)])


def check_legacy_tables(page, original):
    # Localized headers carry their original field name in the title attribute.
    result = page.evaluate("""source => {
      const doc = new DOMParser().parseFromString(source, 'text/html');
      const text = cell => cell.textContent.replace(/\\s+/g, ' ').trim();
      const tables = [...document.querySelectorAll('table')].map(t => [...t.rows].map(row => {
        const full = [...row.cells].filter(c => c.classList.contains('full-metric'));
        return full.length ? full : [...row.cells];
      }));
      let rows = 0, cells = 0;
      for (const table of doc.querySelectorAll('table')) {
        const expected = [...table.rows].map(row => [...row.cells].map(text));
        const match = tables.findIndex(candidate => candidate[0]?.some((_, offset) => {
          let previous = -1;
          return expected.every(row => {
            previous = candidate.findIndex((r,i) => i > previous && r.length >= row.length + offset &&
              row.every((v,j) => { const c = r[offset+j]; return text(c) === v ||
                c.getAttribute('title') === v || (!v && text(c) === '—'); }));
            return previous >= 0;
          });
        }));
        if (match < 0) throw Error('Missing legacy rows or values in ' + expected[0].join(', '));
        tables.splice(match, 1);
        rows += expected.length;
        cells += expected.reduce((count, row) => count + row.length, 0);
      }
      for (const anchor of doc.querySelectorAll('[id]')) {
        if (document.querySelectorAll('#'+CSS.escape(anchor.id)).length !== 1) throw Error('Missing/duplicate anchor '+anchor.id);
      }
      return { tables: doc.querySelectorAll('table').length, rows, cells };
    }""", original)
    return result


def preview_text(root):
    # These fields have separate export contracts: redacted configuration and
    # the additional control for showing historical point columns.
    return root.evaluate("""element => {
      const nodes = [...element.querySelectorAll('[data-report-config],[data-report-endpoint],.offline-table-controls')];
      const previous = nodes.map(node => node.style.cssText);
      try { nodes.forEach(node => node.style.setProperty('display','none','important')); return element.innerText; }
      finally { nodes.forEach((node,i) => node.style.cssText = previous[i]); }
    }""")


def run_case(args, report, secrets):
    labels = json.loads((ROOT / f'ui/src/i18n/locales/{args.locale}/evaluations.json').read_text())['agentic']['export']
    base = args.base_url.rstrip('/')
    benchmark_path = f'/api/v1/benchmarks/{args.run_id}'
    browser = context = offline = None
    session_created = False
    failures = []
    report['failures'] = failures

    def begin(name):
        report['last_completed_step'] = report.get('current_step')
        report['current_step'] = name
        return name

    step = begin('start browser')
    with sync_playwright() as pw:
        try:
            browser = pw.chromium.launch(headless=True)
            report['browser'] = browser.version
            context = browser.new_context(viewport={'width': 1440, 'height': 1080},
                                          timezone_id='UTC', locale='en-US' if args.locale == 'en' else 'zh-CN',
                                          accept_downloads=True)
            context.set_default_timeout(args.timeout_ms)
            expect.set_options(timeout=args.timeout_ms)

            def request(method, path, expected=200, **kwargs):
                observed = {'method': method, 'path': path, 'status': None}
                report['requests'].append(observed)
                response = context.request.fetch(base + path, method=method, **kwargs)
                observed['status'] = response.status
                assert response.status == expected, f'{method} {path}: expected {expected}, got {response.status}'
                return response

            step = begin('health and management login')
            health = request('GET', '/healthz').json()
            assert health.get('status') == 'ok', f'Health status: {health.get("status")}'
            report['server_version'] = health.get('version')
            login_response = request('POST', '/api/v1/auth/login', data={'username': args.username,
                                     'password': secrets[0]}, headers={'Origin': base})
            session_created = True
            login = login_response.json()
            if login.get('token'):
                secrets.append(login['token'])
            assert request('GET', '/api/v1/auth/me').json().get('username') == args.username

            step = begin('read terminal benchmark through real management API')
            run = request('GET', benchmark_path).json()
            assert run.get('id') == args.run_id, 'GET returned a different benchmark ID'
            assert run.get('state') in ('done', 'failed', 'canceled'), 'Run must be terminal for stable visual comparison'
            points = [p for p in run.get('points', []) if p.get('state') != 'running']
            assert points, 'A persisted completed point is required; this runner does not manufacture results'
            selected_seq = points[1 if len(points) > 1 else 0]['seq']
            timeline_path = f'{benchmark_path}/points/{selected_seq}/timeline'
            timeline = request('GET', timeline_path).json()
            source_curves = request('GET', benchmark_path + '/curve').json()['points'] or []
            legacy_html = request('GET', benchmark_path + '/report?format=html').text()
            assert timeline.get('status') in ('complete', 'disabled', 'unavailable', 'truncated')
            assert isinstance(timeline.get('records'), list)
            if args.require_timeline:
                assert timeline['status'] in ('complete', 'truncated') and timeline['records'], 'Retained request timeline records are required'
            assert all('text' not in record and 'usage' not in record for record in timeline['records']), 'Timeline API exposes request body or usage payloads'
            report['request_timeline'] = {'status': timeline['status'], 'retained_records': len(timeline['records'])}
            title = run.get('name') or f'Run #{args.run_id}'
            page = context.new_page()
            page.add_init_script('localStorage.setItem("gpuworks.locale", ' + json.dumps(args.locale) + ')')
            page_errors = []
            page.on('pageerror', lambda error: page_errors.append(str(error)))

            def observed_response(response):
                parsed = urlsplit(response.url)
                if parsed.path.startswith(benchmark_path) or parsed.path == '/api/v1/benchmarks/meta':
                    report['browser_requests'].append({'method': response.request.method, 'path': parsed.path,
                                                       'query': parse_qs(parsed.query), 'status': response.status})

            page.on('response', observed_response)
            step = begin('load real benchmark page and select persisted point')
            with page.expect_response(lambda response: urlsplit(response.url).path == benchmark_path
                                      and response.request.method == 'GET') as browser_run:
                page.goto(base + f'/evaluations/{args.run_id}')
            assert browser_run.value.status == 200
            assert browser_run.value.json() == run, 'Page GET differs from independent API read'
            expect(page.get_by_role('heading', name=title, exact=True)).to_be_visible()
            expect(page.locator('canvas')).to_have_count(2)
            page.locator(f'.ant-table-row[data-row-key="{selected_seq}"]').first.click()
            page.locator('.ant-segmented-item').filter(has_text='TTFT').click()
            report['selected_point'] = selected_seq
            # Instrument only the final PNG callback. Product DOM/CSS and all API
            # responses remain intact; pause lets us photograph the actual export
            # layout after the product has expanded and resized its own content.
            page.evaluate('''() => {
                const original = HTMLCanvasElement.prototype.toBlob;
                HTMLCanvasElement.prototype.toBlob = function(...args) {
                    HTMLCanvasElement.prototype.toBlob = original;
                    window.reportReady = true;
                    window.releaseReport = () => original.apply(this,args);
                };
            }''')
            step = begin('export PNG and capture actual report layout')
            page.get_by_role('button', name=labels['button']).click()
            page.get_by_text(labels['png'], exact=True).click()
            page.wait_for_function('window.reportReady === true')
            root = page.locator('.benchmark-report-export')
            page.evaluate('window.scrollTo(0, 0)')
            page.mouse.move(0, 0)
            for table in root.locator('.ant-table-body, .ant-table-content').all():
                assert table.evaluate('(el) => el.scrollWidth <= el.clientWidth + 1'), 'Export clips selected table columns'
            bounds = root.evaluate('''el => {
                const r=el.getBoundingClientRect();
                return {x:r.x+scrollX,y:r.y+scrollY,width:Math.ceil(r.width),height:Math.ceil(el.scrollHeight)};
            }''')
            reference = cropped_reference(page, page.screenshot(full_page=True), bounds)
            (args.output / 'reference.png').write_bytes(reference)
            source_text = preview_text(root)
            if args.require_timeline:
                assert timeline['records'][0]['session_id'] in source_text, 'The timeline session identity is missing from the rendered report'
            with page.expect_download() as download:
                page.evaluate('window.releaseReport()')
            png_path = args.output / 'report.png'
            download.value.save_as(png_path)
            png = png_path.read_bytes()
            assert png.startswith(b'\x89PNG\r\n\x1a\n'), 'Download is not PNG'
            width, height = struct.unpack('>II', png[16:24])
            assert width >= bounds['width'] and height >= bounds['height'], 'PNG is smaller than the report'
            expect(root).to_have_count(0)

            step = begin('export HTML through the visible report menu')
            page.get_by_role('button', name=labels['button']).click()
            with page.expect_download() as download:
                page.get_by_text(labels['html'], exact=True).click()
            html_path = args.output / 'report.html'
            download.value.save_as(html_path)
            expect(root).to_have_count(0)
            assert request('GET', benchmark_path).json() == run, 'Read-only export changed the persisted benchmark'
            assert request('GET', timeline_path).json() == timeline, 'Read-only export changed the retained timeline'

            step = begin('open exported single HTML file offline')
            offline = browser.new_context(offline=True, viewport={'width': 1440, 'height': 1080}, timezone_id='UTC')
            offline.set_default_timeout(args.timeout_ms)
            html = offline.new_page()
            external_requests = []
            html.on('request', lambda req: external_requests.append(urlsplit(req.url).scheme)
                    if urlsplit(req.url).scheme in ('http', 'https') else None)
            html.on('pageerror', lambda error: page_errors.append(str(error)))
            html.goto(html_path.resolve().as_uri())
            expect(html.get_by_role('heading', name=title, exact=True)).to_be_visible()
            assert html.locator('script[src], link[rel=stylesheet]').count() == 0
            legacy_counts = check_legacy_tables(html, legacy_html)
            empty_sections = html.locator('.report-archive > details').evaluate_all("""groups => groups.flatMap(group => {
                const content = group.cloneNode(true);
                const title = content.querySelector('summary');
                title.remove();
                return content.textContent.trim() || content.querySelector('table, [id], canvas, img') ? [] : [title.textContent];
            })""")
            assert not empty_sections, f'Empty detailed sections: {empty_sections}'
            if source_curves:
                expect(html.locator('.native-curve canvas')).to_be_visible()
            assert preview_text(html.locator('.benchmark-report-export')) == source_text, 'Offline report text differs from source'
            assert not external_requests, 'Offline HTML attempted external HTTP(S) requests'
            rendered = html.locator('.benchmark-report-export').screenshot(path=str(args.output / 'offline-html.png'))
            html_difference = compare_pixels(html, reference, rendered)
            png_difference = compare_pixels(html, reference, png)
            assert html_difference < 3, f'HTML mean pixel difference {html_difference:.3f} exceeds 3/255'
            assert png_difference < 4, f'PNG mean pixel difference {png_difference:.3f} exceeds 4/255'
            # Changing the chosen metric must actually redraw the offline ECharts canvas.
            if source_curves:
                curve = html.locator('.native-curve canvas').first
                before = curve.evaluate('c => c.toDataURL()')
                radio = html.locator('[data-report-curve] input[value="output_tput"]')
                radio.locator('..').click()
                expect(radio).to_be_checked()
                assert curve.evaluate('c => c.toDataURL()') != before, 'Switching the offline metric did not redraw the chart'
            payload_element = html.locator('#benchmark-chart-data')
            payload = json.loads(payload_element.text_content()) if payload_element.count() else {}
            assert [
                (p['seq'], p['samples']) for p in payload.get('curves', [])
            ] == [(p['seq'], p['samples']) for p in source_curves], 'Exported samples differ from the independent curve API'
            toggle = html.locator('#full-point-columns')
            toggle.check()
            full_rows = html.locator('#all-points tr[data-seq]:visible')
            sequences = full_rows.evaluate_all("rows => rows.map(row => row.dataset.seq)")
            assert len(sequences) == len(set(sequences)), 'Full results repeat a point'
            first_row = full_rows.first
            header = first_row.locator('xpath=ancestor::table[1]').locator('thead th.full-metric:visible')
            expect(header.first).to_be_visible()
            assert header.count() == first_row.locator('.full-metric:visible').count(), 'Full results columns lack headers'
            toggle.uncheck()
            if payload.get('pareto'):
                expect(html.locator('.native-pareto canvas')).to_be_visible()
                seq = next((p['seq'] for p in payload['pareto'] if p['seq'] != selected_seq), selected_seq)
                html.locator(f'#all-points tr[data-seq="{seq}"] td').first.click()
                expect(html.locator(f'#all-points tr[data-seq="{seq}"]')).to_have_attribute('data-offline-selected','')
                expect(html.locator('[data-report-pareto] .ant-descriptions-item-content').first).to_contain_text(f'#{seq}')
            assert not external_requests, 'Chart interaction attempted an external request'
            assert not page_errors, f'Browser errors: {page_errors}'
            report['checks'] = {'html_pixel_difference': html_difference, 'png_pixel_difference': png_difference,
                                'png_dimensions': [width, height], 'offline_http_requests': 0,
                                'persisted_run_unchanged': True, 'source_text_preserved': True, 'legacy': legacy_counts}
            report['files'] = {path.name: {'sha256': hashlib.sha256(path.read_bytes()).hexdigest(),
                                         'bytes': path.stat().st_size}
                               for path in (png_path, html_path, args.output / 'reference.png', args.output / 'offline-html.png')}
            report['last_completed_step'] = step
        except BaseException as error:
            failures.append({'phase': step, 'type': type(error).__name__, 'error': str(error)})
        finally:
            if session_created and context is not None:
                try:
                    response = context.request.post(base + '/api/v1/auth/logout', headers={'Origin': base})
                    assert response.status == 200, f'Logout status {response.status}'
                    probe = context.request.get(base + '/api/v1/auth/me')
                    assert probe.status == 401, f'Revoked session still accepted: {probe.status}'
                    report['cleanup']['management_session'] = 'revoked; independent GET returned 401'
                except BaseException as error:
                    failures.append({'phase': 'logout cleanup', 'type': type(error).__name__, 'error': str(error)})
            for name, resource in [('offline_context', offline), ('authenticated_context', context)]:
                if resource is not None:
                    try:
                        closed = []
                        resource.on('close', lambda *_, closed=closed: closed.append(True))
                        resource.close()
                        assert closed, 'Context close event was not observed'
                        report['cleanup'][name] = 'close observed'
                    except BaseException as error:
                        failures.append({'phase': name + ' cleanup', 'type': type(error).__name__, 'error': str(error)})
            if browser is not None:
                try:
                    browser.close()
                    assert not browser.is_connected(), 'Browser remains connected'
                    report['cleanup']['browser'] = 'disconnected'
                except BaseException as error:
                    failures.append({'phase': 'browser cleanup', 'type': type(error).__name__, 'error': str(error)})
    report['failures'] = failures
    report['status'] = 'FAIL' if failures else 'PASS'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base-url', required=True)
    parser.add_argument('--run-id', required=True, type=int, help='Existing terminal benchmark ID')
    parser.add_argument('--build-sha', required=True, help='Deployer-attested SHA of the server and its embedded UI')
    parser.add_argument('--username', default='admin')
    parser.add_argument('--password-env', default='GPUWORKS_E2E_PASSWORD')
    parser.add_argument('--locale', choices=['en', 'zh'], required=True)
    parser.add_argument('--output', type=Path, required=True, help='New private/ignored evidence directory')
    parser.add_argument('--require-timeline', action='store_true', help='Require retained request records and their visible session identity')
    parser.add_argument('--timeout-ms', type=int, default=30000)
    args = parser.parse_args()
    parsed = urlsplit(args.base_url)
    if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        parser.error('--base-url must be an HTTP(S) server URL without credentials, query, or fragment')
    if args.run_id < 1 or args.timeout_ms < 1:
        parser.error('--run-id and --timeout-ms must be positive')
    password = os.environ.get(args.password_env)
    if not password:
        parser.error(f'The {args.password_env} environment variable is required')
    args.output.mkdir(parents=True, exist_ok=False)
    report = {'case': CASE, 'run_id': datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid4().hex[:8],
              'benchmark_id': args.run_id, 'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'build_sha': args.build_sha, 'build_identity_source': 'caller attestation: server and embedded UI share this build',
              'locale': args.locale, 'timezone': 'UTC', 'lane': 'existing-server-readonly-report-export',
              'scope': 'Report rendering/export of existing facts; no claim of benchmark execution or whole-page E2E coverage',
              'not_applicable': {'agent_runtime_gpu': 'The tested action only reads an existing terminal run and generates browser files',
                                 'benchmark_mutation': 'The caller provides a persisted run; this case does not execute or modify it'},
              'not_run': ['empty/error export variants', 'assertion mutation sensitivity', 'independent standard review'],
              'requests': [], 'browser_requests': [], 'cleanup': {}, 'status': 'NOT_RUN'}
    secrets = [password]
    try:
        run_case(args, report, secrets)
    except BaseException as error:
        report.setdefault('failures', []).append({'phase': 'browser runner', 'type': type(error).__name__, 'error': str(error)})
    report['status'] = 'FAIL' if report.get('failures') else 'PASS'
    encoded = json.dumps(report, ensure_ascii=False, indent=2)
    for secret in secrets:
        if secret:
            encoded = encoded.replace(secret, '[REDACTED]')
    (args.output / 'result.json').write_text(encoded + '\n')
    print(f'{report["status"]}: {CASE}; locale={args.locale}; benchmark={args.run_id}; evidence={args.output}')
    raise SystemExit(0 if report['status'] == 'PASS' else 1)


if __name__ == '__main__':
    main()
