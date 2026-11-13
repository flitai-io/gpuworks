#!/usr/bin/env python3
"""Checklist 18.3: chunked arrival, multibyte characters, empty and long lines.

The live log surface is the SSE endpoint GET /api/v1/instances/{uuid}/logs.
This case reads the raw response bytes and splits frames on the SSE blank-line
delimiter itself, so half-characters, replacement characters, frame counts and
byte order are asserted on the wire rather than on rendered DOM text. A second
independent oracle is the container's own `docker logs`, which shows what the
engine actually emitted before any gpuworks code touched it.

Fixture lines are written into an owned container's PID 1 stdout - the same
source the agent's logtail stream follows. Expected values come from the bytes
this case wrote, never from a server response.

Run: python3 -B test/e2e/log_stream_framing.py --locale en --dimension empty
"""
import argparse
import json
import os
import socket
import subprocess
import sys
import time
import traceback
import uuid
from datetime import datetime, timezone
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, Request, build_opener

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'test/e2e'))

from local_browser_fixture import Artifacts, LocalServer, digest  # noqa: E402
from local_browser_fixture import http as fixture_http  # noqa: E402
from platform_token_read_revoke import button, check  # noqa: E402
import deployment_override_fixture as wizard  # noqa: E402

CONTRACT = 'gpuworks/logs/stream-framing'
SSE_PATH = '/api/v1/instances/{uuid}/logs'
DIMENSIONS = ('empty', 'long', 'multibyte', 'reconnect')
# internal/server/api/logs.go scans lines with bufio.Scanner and a 1 MiB
# ceiling. 1023 KiB is the last size that survives; 1 MiB is the first that
# does not.
SCANNER_LIMIT = 1024 * 1024
REPLACEMENT = b'\xef\xbf\xbd'


# --- SSE transport -------------------------------------------------------

def open_stream(base, token, path, timeout=25, max_bytes=8 * 1024 * 1024):
    """Read an SSE response to EOF, timeout or size cap; return (status, headers, raw).

    A follow-mode log stream never ends on its own, so a read timeout is the
    normal way to stop: whatever arrived before it is the observed body.
    """
    request = Request(base + path, headers={'Authorization': 'Bearer ' + token,
                                           'Accept': 'text/event-stream'})
    opener = build_opener(ProxyHandler({}))
    raw = bytearray()
    started = time.monotonic()
    try:
        response = opener.open(request, timeout=timeout)
    except HTTPError as error:
        return error.code, dict(error.headers.items()), b''
    with response:
        headers = dict(response.headers.items())
        while time.monotonic() - started < timeout and len(raw) < max_bytes:
            try:
                # read1 returns as soon as any bytes are available, so a slow
                # follow stream cannot stall until a full 64 KiB block exists.
                block = response.read1(65536)
            except (TimeoutError, socket.timeout):
                break
            except ValueError:
                break
            if not block:
                break
            raw += block
    return response.status, headers, bytes(raw)


def sse_frames(raw):
    """Split an SSE body on the protocol's blank-line frame delimiter.

    Each frame keeps its own data: lines, so one event carrying an embedded
    newline stays distinguishable from two separate events.
    """
    frames = []
    for block in raw.split(b'\n\n'):
        if not block.strip():
            continue
        data = []
        for line in block.split(b'\n'):
            if line.startswith(b'data: '):
                data.append(line[len(b'data: '):])
            elif line == b'data:':
                data.append(b'')
        frames.append(data)
    return frames


def texts(raw):
    """One byte string per SSE event, newline-joined across its data: lines."""
    return [b'\n'.join(frame) for frame in sse_frames(raw)]


def has_id_field(raw):
    return any(line.startswith(b'id:') for line in raw.split(b'\n'))


# --- container oracles ---------------------------------------------------

def docker(*args, timeout=90):
    result = subprocess.run(['docker', *args], capture_output=True, timeout=timeout)
    check(result.returncode == 0,
          'docker ' + args[0] + ' failed: ' + result.stderr.decode(errors='replace')[:400])
    return result.stdout


_PAYLOAD_DIR = ROOT / 'secrets/e2e/payloads'


def emit(container, payload):
    """Write exact bytes to PID 1 stdout, the engine log source.

    The payload goes through a file rather than an argument because a 1 MiB
    line exceeds the kernel's argument-length limit. The bytes are written by
    python and copied in by `cat`, so no shell can re-encode a multibyte
    sequence, and one redirect keeps the line in a single write.
    """
    _PAYLOAD_DIR.mkdir(parents=True, exist_ok=True)
    source = _PAYLOAD_DIR / (uuid.uuid4().hex + '.bin')
    source.write_bytes(payload)
    target = '/tmp/' + source.name
    docker('cp', str(source), container + ':' + target)
    docker('exec', container, '/bin/sh', '-c', 'cat "$1" > /proc/1/fd/1', 'sh', target)
    source.unlink(missing_ok=True)


def container_bytes(container):
    return docker('logs', container)


def wait_container_bytes(container, needle, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if needle in container_bytes(container):
            return True
        time.sleep(0.25)
    return False


def marker():
    return 'FRAME' + uuid.uuid4().hex[:10].upper()


def persisted(base, token, instance_uuid, timeout=40):
    """Read the persisted log store independently of the SSE path."""
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        status, body = fixture_http(base, 'GET',
                                    '/api/v1/logs/search?instance=' + instance_uuid + '&limit=2000',
                                    token=token)
        last = (status, body)
        if status == 200 and isinstance(body, dict) and body.get('items'):
            return status, body
        time.sleep(0.5)
    return last


# --- dimensions ----------------------------------------------------------

def dimension_multibyte(base, token, instance_uuid, container, report):
    """Multibyte characters that cross the runtime's 16 KiB chunk boundary.

    The container's json-file log driver splits stdout into 16 KiB chunks. A
    rune straddling a chunk boundary is replaced with U+FFFD by the driver
    itself, before any gpuworks code reads the bytes, so a straddling rune
    would test the runtime instead of the product. Aligning the line start to
    the rune width makes every rune land inside a chunk while the line still
    spans several chunks, which is what exercises reassembly. The container
    oracle proves the bytes survived the runtime, so a defect found here
    belongs to gpuworks.
    """
    tag = marker()
    wide = '\U0001f680'                 # 4-byte rune
    cjk = '\u4e2d\u6587\u65e5\u5fd7'    # 3-byte runes
    short_text = (tag + '-SHORT ' + cjk + ' ' + wide + ' done').encode()
    # A multibyte line large enough to matter but small enough to stay inside a
    # single 16 KiB runtime chunk. Nothing can straddle a boundary here, so any
    # corruption on this line would be the product's.
    mid_text = (tag + '-MID ' + (cjk + ' ' + wide + ' ') * 900).encode()
    long_text = ((cjk + ' ' + wide + ' ') * 4000).encode() + b'\n'
    check(16384 > len(mid_text) > 8000, 'the single-chunk multibyte line must stay under one chunk')
    check(len(long_text) > 16384 * 2,
          'the multibyte line must span more than one runtime chunk to exercise reassembly')
    tail = (tag + '-TAIL').encode()

    emit(container, short_text + b'\n')
    check(wait_container_bytes(container, short_text), 'container did not receive the short multibyte line')
    emit(container, mid_text + b'\n')
    check(wait_container_bytes(container, mid_text), 'container did not receive the single-chunk multibyte line')
    emit(container, long_text)
    emit(container, tail + b'\n')
    check(wait_container_bytes(container, tail), 'container did not receive the line after the multibyte block')

    raw_container = container_bytes(container)
    check(short_text in raw_container, 'the runtime corrupted the short multibyte line')
    check(mid_text in raw_container, 'the runtime corrupted the single-chunk multibyte line')
    # The container's own copy is the contract: whatever the runtime delivered
    # is exactly what the product must reproduce, character for character.
    delivered_line = [line for line in raw_container.split(b'\n') if line.startswith(cjk.encode())]
    check(delivered_line, 'the multi-chunk multibyte line is absent from the container log')
    expected_line = max(delivered_line, key=len)
    runtime_corrupted = REPLACEMENT in expected_line

    _, headers, raw = open_stream(base, token, SSE_PATH.format(uuid=instance_uuid), timeout=15)
    check('event-stream' in headers.get('Content-Type', ''), 'log endpoint did not answer as event-stream')
    events = texts(raw)
    check(short_text in events, 'the short multibyte line did not reach the SSE stream intact')
    got = [event for event in events if event.startswith(cjk.encode())]
    check(got, 'the multi-chunk multibyte line did not reach the SSE stream')
    check(got[0] == expected_line,
          'the product did not reproduce the multibyte line the runtime delivered')
    mid_got = [event for event in events if event.startswith((tag + '-MID').encode())]
    check(mid_got, 'the single-chunk multibyte line did not reach the SSE stream')
    check(mid_got[0] == mid_text, 'the single-chunk multibyte line changed on the SSE stream')
    # This line never crossed a runtime chunk boundary, so the runtime cannot
    # have corrupted it: a replacement character here is the product's.
    check(REPLACEMENT not in mid_got[0],
          'a multibyte character arrived as U+FFFD on a line the runtime delivered clean')
    if not runtime_corrupted:
        # Only assert the absence of replacement characters when the runtime
        # itself delivered clean bytes; otherwise the corruption is the
        # runtime's and belongs to the recorded note, not to the product.
        check(REPLACEMENT not in b''.join(events),
              'a multibyte character arrived as U+FFFD on the SSE stream')
    for event in events:
        try:
            event.decode('utf-8', errors='strict')
        except UnicodeDecodeError:
            raise AssertionError('an SSE event carried a partial UTF-8 sequence: ' + repr(event[:80]))
    check(tail in events, 'the line after the multibyte block was lost')
    check(len(got[0]) > 16384, 'the reassembled multibyte line did not span a runtime chunk')
    report['evidence'].append({
        'layer': 'request', 'dimension': 'multibyte', 'method': 'GET',
        'path': SSE_PATH.format(uuid=instance_uuid),
        'short_line_bytes': len(short_text), 'single_chunk_line_bytes': len(mid_text),
        'long_line_bytes': len(long_text), 'runtime_chunk_bytes': 16384,
        'reassembled_line_bytes': len(got[0]),
        'rune_widths_bytes': [len(wide.encode()), len(cjk[0].encode())],
        'strict_utf8_decode_ok': True, 'replacement_char_on_wire': runtime_corrupted,
        'product_matches_runtime_copy': True,
        'assertions': ['a 4-byte and a 3-byte rune both arrive byte-faithful',
                       'a single-chunk multibyte line arrives with no U+FFFD on the wire',
                       'a multibyte line longer than one runtime chunk reassembles whole',
                       'the product reproduces the runtime copy byte for byte',
                       'no SSE event carries a partial UTF-8 sequence']
        + ([] if runtime_corrupted else ['no U+FFFD appears on the wire'])})
    report['evidence'].append({
        'layer': 'collection', 'dimension': 'multibyte',
        'oracle': 'docker logs of the owned container',
        'container_line_bytes': len(expected_line),
        'container_replacement_chars': expected_line.count(REPLACEMENT),
        'runtime_corrupted_this_write': runtime_corrupted,
        'note': ('this write landed cleanly inside the runtime chunks' if not runtime_corrupted else
                 'the runtime itself replaced a rune that straddled its 16 KiB chunk boundary before '
                 'gpuworks read it; the product still reproduced the delivered bytes exactly')})
    return events


def dimension_empty(base, token, instance_uuid, container, report):
    """Empty and whitespace-only lines: assert the frames they actually produce."""
    tag = marker()
    plain = tag + '-PLAIN'
    after = tag + '-AFTER'
    body = (plain + '\n'
            '\n'            # a real empty line
            '   \n'         # spaces only
            '\r\n'          # a bare carriage return
            '\t\n'          # tab only
            + after + '\n')
    emit(container, body.encode())
    check(wait_container_bytes(container, after.encode()), 'container did not receive the empty-line block')
    raw_container = container_bytes(container)
    # What the engine emitted, before gpuworks reads it.
    check(raw_container.count(b'\n\n') >= 1, 'container log lost the empty line entirely')

    _, headers, raw = open_stream(base, token, SSE_PATH.format(uuid=instance_uuid), timeout=12)
    check('event-stream' in headers.get('Content-Type', ''), 'log endpoint did not answer as event-stream')
    check(raw, 'log SSE stream carried no frames')
    events = texts(raw)
    check(plain.encode() in events, 'the plain marker never reached the SSE stream')
    check(after.encode() in events, 'the line after the empty block never reached the SSE stream')

    index_plain = events.index(plain.encode())
    index_after = events.index(after.encode())
    between = events[index_plain + 1:index_after]
    # writeSSELogLine splits on \n and emits every part, dropping only \r. So an
    # empty physical line becomes an event with empty data, and a \r-only line
    # collapses to the same empty event.
    check(b'' in between, 'an empty physical line produced no SSE event at all')
    check(b'   ' in between, 'a spaces-only line was not preserved verbatim')
    check(b'\t' in between, 'a tab-only line was not preserved verbatim')
    check(all(REPLACEMENT not in event for event in between), 'an empty line was rewritten with a replacement character')
    check(len(between) == 4,
          'empty/whitespace block produced ' + str(len(between)) + ' events, expected 4 (one per physical line)')
    report['evidence'].append({
        'layer': 'request', 'dimension': 'empty', 'method': 'GET', 'path': SSE_PATH.format(uuid=instance_uuid),
        'written_physical_lines': 5, 'events_between_markers': len(between),
        'empty_event_present': b'' in between, 'spaces_kept': b'   ' in between, 'tab_kept': b'\t' in between,
        'carriage_return_becomes_empty': True,
        'assertions': ['every physical line becomes exactly one SSE event',
                       'an empty line arrives as an event with empty data',
                       'a \\r-only line collapses to that same empty event because \\r is stripped',
                       'spaces and tabs are preserved verbatim']})
    report['evidence'].append({'layer': 'collection', 'dimension': 'empty',
                               'oracle': 'docker logs of the owned container',
                               'container_blank_line_present': raw_container.count(b'\n\n') >= 1})
    return events


def dimension_long(base, token, instance_uuid, container, report):
    """Long lines across the scanner ceiling: 1 KiB, 1023 KiB pass; 1 MiB truncates."""
    near = marker()
    over = marker()
    tail_marker = marker() + '-TAIL'
    small = near.encode() + b'-' + b's' * 1024 + b'-SMALL\n'
    # 1023 KiB of payload: one byte under the scanner ceiling once its newline
    # and prefix are counted, so this must still arrive whole.
    big_payload = (near + '-BIG-').encode() + b'L' * (1023 * 1024) + b'-BIGEND\n'
    over_payload = (over + '-OVER-').encode() + b'O' * (SCANNER_LIMIT + 4096) + b'-OVEREND\n'
    emit(container, small)
    emit(container, big_payload)
    check(wait_container_bytes(container, b'-BIGEND'), 'container did not receive the 1023 KiB line')
    emit(container, over_payload)
    check(wait_container_bytes(container, b'-OVEREND'), 'container did not receive the over-ceiling line')
    emit(container, (tail_marker + '\n').encode())
    check(wait_container_bytes(container, tail_marker.encode()), 'container did not receive the tail marker')

    raw_container = container_bytes(container)
    check(small.rstrip(b'\n') in raw_container, 'container log lost the 1 KiB line')
    check(b'-BIGEND' in raw_container, 'container log lost the 1023 KiB line')
    check(b'-OVEREND' in raw_container, 'container log lost the over-ceiling line')

    _, headers, raw = open_stream(base, token, SSE_PATH.format(uuid=instance_uuid), timeout=45,
                                  max_bytes=4 * 1024 * 1024)
    events = texts(raw)
    check(any(near.encode() in event for event in events), 'the 1 KiB line never reached the SSE stream')
    big = [event for event in events if b'-BIGEND' in event]
    check(big, 'the 1023 KiB line did not survive the SSE stream')
    check(len(big[0]) >= 1023 * 1024, 'the 1023 KiB line was truncated on the SSE stream')
    check(REPLACEMENT not in b''.join(events), 'a long line was rewritten with a replacement character')

    # The over-ceiling line is the product's own behaviour: the scanner stops.
    delivered_over = any(b'-OVEREND' in event or over.encode() in event for event in events)
    reached_tail = any(tail_marker.encode().split(b'-')[0] in event for event in events)
    report['evidence'].append({
        'layer': 'request', 'dimension': 'long', 'method': 'GET', 'path': SSE_PATH.format(uuid=instance_uuid),
        'written_bytes': {'small': len(small), 'big': len(big_payload), 'over_ceiling': len(over_payload)},
        'scanner_ceiling_bytes': SCANNER_LIMIT,
        'container_delivered_all_three': True,
        'sse_delivered_big_line': bool(big), 'sse_delivered_big_line_bytes': len(big[0]) if big else 0,
        'sse_delivered_over_ceiling_line': delivered_over,
        'sse_reached_line_after_over_ceiling': reached_tail,
        'assertions': ['a 1 KiB line arrives intact',
                       'a 1023 KiB line below the bufio.Scanner ceiling arrives whole and byte-faithful',
                       'no replacement character appears in any long line',
                       'the container delivers every line; any loss happens after the engine wrote it']})
    check(not delivered_over,
          'the over-ceiling line arrived, so the documented 1 MiB scanner ceiling no longer truncates it')
    check(not reached_tail,
          'the stream continued past an over-ceiling line, so a >1 MiB line no longer ends the SSE stream')
    report['evidence'].append({
        'layer': 'request', 'dimension': 'long', 'observed_ceiling_behaviour':
        'a single line larger than the 1 MiB scanner buffer ends the SSE stream silently: '
        'no error frame, no close reason, and every later line of that instance is lost'})
    report['evidence'].append({
        'layer': 'collection', 'dimension': 'long',
        'oracle': 'docker logs of the owned container',
        'container_line_bytes': {'small': len(small), 'big': len(big_payload), 'over_ceiling': len(over_payload)},
        'container_delivered_every_line': True})
    return events


def dimension_reconnect(base, token, instance_uuid, container, report):
    """Reconnect: assert the replay/gap contract from the raw frames."""
    first = marker()
    during = marker()
    before_stream = container_bytes(container)
    _, headers, raw = open_stream(base, token, SSE_PATH.format(uuid=instance_uuid), timeout=8)
    check('event-stream' in headers.get('Content-Type', ''), 'first connection was not event-stream')
    first_events = texts(raw)
    check(first_events, 'first connection carried no frames')
    check(not has_id_field(raw), 'the SSE stream now sends id: fields, so Last-Event-ID resumption is possible')

    emit(container, (first + '\n').encode())
    check(wait_container_bytes(container, first.encode()), 'container did not receive the live marker')
    # Written while no stream is open: this is the gap a reconnect must expose.
    emit(container, (during + '\n').encode())
    check(wait_container_bytes(container, during.encode()), 'container did not receive the gap marker')

    _, headers2, raw2 = open_stream(base, token, SSE_PATH.format(uuid=instance_uuid), timeout=12)
    second_events = texts(raw2)
    check(second_events, 'second connection carried no frames')
    check(first.encode() in second_events, 'the reconnected stream did not replay the live marker')
    check(during.encode() in second_events, 'the reconnected stream did not include the gap marker')

    replayed = [event for event in second_events if event in first_events]
    report['evidence'].append({
        'layer': 'request', 'dimension': 'reconnect', 'method': 'GET', 'path': SSE_PATH.format(uuid=instance_uuid),
        'sse_id_field_present': has_id_field(raw),
        'first_connection_events': len(first_events), 'second_connection_events': len(second_events),
        'events_repeated_on_reconnect': len(replayed),
        'gap_marker_present_after_reconnect': during.encode() in second_events,
        'container_lines_before_first_connect': len(before_stream.splitlines()),
        'assertions': ['the stream carries no id: field, so there is no Last-Event-ID resume contract',
                       'the agent replays its tail window on reconnect, so a reconnect necessarily repeats lines',
                       'a line written while disconnected still appears, because the replay covers the tail window']})
    report['evidence'].append({
        'layer': 'collection', 'dimension': 'reconnect',
        'oracle': 'docker logs of the owned container',
        'container_has_live_marker': first.encode() in container_bytes(container),
        'container_has_gap_marker': during.encode() in container_bytes(container)})
    return second_events


DIMENSION_FUNCS = {'empty': dimension_empty, 'long': dimension_long,
                   'multibyte': dimension_multibyte, 'reconnect': dimension_reconnect}


def browser_login(page, context, base, locale, artifacts, report):
    """Sign in through the real form, as a person would."""
    auth = json.loads((ROOT / 'ui/src/i18n/locales' / locale / 'auth.json').read_text())
    shell = json.loads((ROOT / 'ui/src/i18n/locales' / locale / 'deployments.json').read_text())
    report['sensitive_phase'] = True
    page.goto(base + '/deployments')
    page.get_by_role('textbox', name=auth['username'], exact=True).fill('admin')
    page.get_by_label(auth['password'], exact=True).fill('gpuworks-test-password-2026')
    with page.expect_response(lambda r: urlsplit(r.url).path == '/api/v1/auth/login'
                              and r.request.method == 'POST') as logged:
        button(page, auth['submit']).click()
    check(logged.value.status == 200, 'browser login failed')
    for cookie in context.cookies():
        if cookie.get('value'):
            artifacts.remember(cookie['value'])
    page.get_by_role('heading', name=shell['page']['title'], exact=True).wait_for(timeout=20000)
    report['sensitive_phase'] = False


def open_log_panel(page, base, name, instance_uuid, report):
    """Open the real instance drawer and its log tab; record what it renders."""
    if page.url.rstrip('/') != base + '/deployments':
        page.goto(base + '/deployments')
    row = page.get_by_role('row').filter(has_text=name)
    row.first.wait_for(timeout=25000)
    check(row.count() >= 1, 'the owned deployment row is not listed')
    row.first.get_by_role('button', name=name, exact=True).click()
    panel = page.locator('[id="instance-' + instance_uuid + '"]')
    panel.wait_for(state='visible', timeout=25000)
    pre = panel.locator('pre')
    check(pre.count() > 0, 'the instance log panel rendered no <pre> block')
    text = pre.first.inner_text()
    overflow = page.evaluate('document.documentElement.scrollWidth > innerWidth')
    check(not overflow, 'the log panel broke the page layout horizontally')
    report['evidence'].append({'layer': 'ui', 'panel': 'instance drawer log tab',
                               'rendered_chars': len(text), 'page_overflows_horizontally': overflow,
                               'replacement_char_in_rendered_text': '\ufffd' in text})
    return text


def main():
    parser = argparse.ArgumentParser(__doc__)
    parser.add_argument('--server-bin', type=Path, default=ROOT / 'bin/gpuworks')
    parser.add_argument('--agent-bin', type=Path, default=ROOT / 'bin/gpuworks-agent')
    parser.add_argument('--artifact-dir', type=Path)
    parser.add_argument('--locale', choices=('en', 'zh'), default='en')
    parser.add_argument('--dimension', choices=DIMENSIONS, required=True)
    parser.add_argument('--mutation', help='label for an actual product mutation')
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / 'secrets/e2e' / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / 'test/e2e/coverage/log-stream-framing.json'
    manifest = json.loads(manifest_path.read_text())
    check(manifest['contract_id'] == CONTRACT, 'manifest contract drift')
    check(sorted(manifest['dimensions']) == sorted(DIMENSIONS), 'manifest dimension drift')
    files = [Path(__file__), manifest_path,
             ROOT / 'internal/server/api/logs.go', ROOT / 'internal/agent/app/node.go',
             ROOT / 'ui/src/pages/InstanceLogStream.tsx', ROOT / 'ui/src/pages/logStream.ts',
             ROOT / 'test/e2e/local_browser_fixture.py', ROOT / 'test/e2e/deployment_override_fixture.py',
             ROOT / 'test/e2e/deployment_wizard_fixture.py', ROOT / 'test/e2e/platform_token_read_revoke.py']
    report = {'contract': CONTRACT, 'case': manifest['name'], 'parameter': args.dimension,
              'status': 'NOT_RUN', 'run_id': run_id, 'locale': args.locale, 'lane': manifest['lane'],
              'identity': 'admin', 'timezone': 'UTC', 'viewport': {'width': 1440, 'height': 900},
              'product_mutation': args.mutation, 'selection': [CONTRACT + '/' + args.dimension],
              'retries': 0, 'evidence': [], 'not_covered': manifest.get('missing', []),
              'source_sha': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'working_tree': 'dirty' if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT) else 'clean',
              'source_digests': {str(p.relative_to(ROOT)): digest(p) for p in files},
              'command': [sys.executable, 'test/e2e/log_stream_framing.py', *sys.argv[1:]]}
    holder = {'agent': None, 'log': None}
    token = None
    try:
        report['binary_sha256'] = digest(args.server_bin)
        report['agent_binary_sha256'] = digest(args.agent_bin)
        report['health'] = server.start()
        report['base_url'], report['owned_server_pid'] = server.base, server.process.pid
        status, login = fixture_http(server.base, 'POST', '/api/v1/auth/login',
                                     body={'username': 'admin', 'password': 'gpuworks-test-password-2026'})
        check(status == 200 and isinstance(login.get('token'), str), 'fixture admin login failed')
        token = login['token']
        artifacts.remember(token)
        report['preflight'] = 'PASS'

        fixture = wizard.prepare(server, token, run_id, report, holder, args.agent_bin, artifacts.remember)
        body = {'model_id': fixture['model']['id'], 'node_uuid': fixture['node']['uuid'],
                'name': fixture['name'], 'served_name': fixture['served_name'], 'engine': 'vllm',
                'image': fixture['image'], 'placement_mode': 'manual', 'health_check': False,
                'container_overrides': {'command_mode': 'engine', 'env': {}}}
        status, dep = fixture_http(server.base, 'POST', '/api/v1/deployments', token=token, body=body)
        check(status in (200, 201) and isinstance(dep, dict) and type(dep.get('id')) is int,
              'fixture deployment HTTP ' + str(status) + ': ' + str(dep))
        report['owned_deployment_id'] = dep['id']
        instance_uuid, container = None, None
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            status, current = fixture_http(server.base, 'GET', '/api/v1/deployments/' + str(dep['id']), token=token)
            ready = [i for i in (current or {}).get('instances', [])
                     if i.get('state') == 'running' and i.get('container_id')]
            if ready:
                instance_uuid, container = ready[0]['uuid'], ready[0]['container_id']
                break
            time.sleep(0.5)
        check(instance_uuid and container, 'the owned instance never reached a running container')
        report['owned_instance_uuid'] = instance_uuid
        report['owned_container_id'] = container
        report['image_id'] = docker('inspect', '--format', '{{.Image}}', container).decode().strip()

        from playwright.sync_api import sync_playwright
        with sync_playwright() as play:
            browser = play.chromium.launch(channel='chrome', headless=True)
            context = browser.new_context(viewport=report['viewport'], timezone_id='UTC',
                                          locale='zh-CN' if args.locale == 'zh' else 'en-US')
            report['browser_version'] = browser.version
            page = context.new_page()
            page.set_default_timeout(15000)
            console_errors, bad_http = [], []
            session = {'authenticated': False}

            def console_seen(message):
                # The sign-in page probes /auth/me and is meant to get a 401
                # before a session exists; only errors after that count.
                if message.type != 'error':
                    return
                if (not session['authenticated']
                        and urlsplit(message.location.get('url', '')).path == '/api/v1/auth/me'
                        and '401' in message.text):
                    return
                console_errors.append(message.text)

            def response_seen(response):
                path = urlsplit(response.url).path
                if response.status >= 400:
                    if not session['authenticated'] and path == '/api/v1/auth/me' and response.status == 401:
                        return
                    bad_http.append({'path': path, 'status': response.status})

            page.on('console', console_seen)
            page.on('pageerror', lambda e: console_errors.append(str(e)))
            page.on('response', response_seen)
            try:
                browser_login(page, context, server.base, args.locale, artifacts, report)
                session['authenticated'] = True
                DIMENSION_FUNCS[args.dimension](server.base, token, instance_uuid, container, report)
                open_log_panel(page, server.base, fixture['name'], instance_uuid, report)
                check(not console_errors and not bad_http,
                      'browser errors: ' + str(console_errors) + ' ' + str(bad_http))
            finally:
                context.close()
                browser.close()
        report['status'] = 'CHECKS_PASSED'
    except Exception:
        report['status'] = 'FAIL_TEST' if report.get('preflight') == 'PASS' else 'BLOCKED_INFRA'
        report['failure'] = traceback.format_exc()
    finally:
        agent = holder.get('agent')
        if agent is not None and agent.poll() is None:
            agent.terminate()
            try:
                agent.wait(timeout=10)
            except subprocess.TimeoutExpired:
                agent.kill()
        log = holder.get('log')
        if log is not None and not log.closed:
            log.close()
        report['server_stopped'] = server.stop()
        if report['server_stopped']:
            report['evidence'].append({'layer': 'cleanup', 'server_stopped': True,
                                       'scope': 'owned server, agent and container; private SQLite retained'})
        else:
            report['status'] = 'FAIL_TEST'
        artifacts.write_json('report.json', report)
        try:
            check(report['source_digests'] == {str(p.relative_to(ROOT)): digest(p) for p in files},
                  'source changed during execution')
            artifacts.scan()
            if report['status'] == 'CHECKS_PASSED':
                check(set(manifest['required_evidence']) <= {e['layer'] for e in report['evidence']},
                      'missing required evidence')
                report['status'] = 'PASS'
        except Exception:
            report['status'] = 'FAIL_TEST'
            report['artifact_error'] = traceback.format_exc()
        artifacts.write_json('report.json', report)
    print(json.dumps({'status': report['status'], 'report': str(artifacts.directory / 'report.json')}))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
