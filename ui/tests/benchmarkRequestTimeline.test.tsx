import assert from 'node:assert/strict';
import { after, afterEach, test } from 'node:test';
import { JSDOM } from 'jsdom';
import React from 'react';
import { createInstance } from 'i18next';
import { I18nextProvider, initReactI18next } from 'react-i18next';
import type { BenchmarkRequestTimeline as Timeline, BenchmarkTimelineRecord } from '../src/api';
const dom = new JSDOM('<!doctype html><html><body></body></html>', { url: 'http://localhost/' });
for (const name of ['window', 'document', 'navigator', 'HTMLElement', 'SVGElement', 'Element', 'Node', 'MutationObserver', 'ShadowRoot'] as const) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
Object.defineProperty(globalThis, 'getComputedStyle', { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, 'matchMedia', { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, 'MessageChannel', { value: undefined, configurable: true });
Object.defineProperty(globalThis, 'ResizeObserver', { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: 'en', defaultNS: 'evaluations', interpolation: { escapeValue: false }, resources: { en: { evaluations: { timeline: {
  title: 'Request timeline', loading: 'Loading timeline', loadFailed: 'Failed to load', retry: 'Retry', disabled: 'Disabled', unavailable: 'Unavailable', truncated: 'Retained subset', empty: 'No requests', unknown: 'Unknown', noTiming: 'No timing', scope: '{{start}}–{{end}} of {{total}} retained requests', session: 'Session', turn: 'Turn', attempt: 'Attempt', requestDetails: 'Request details', sent: 'Sent', firstOutput: 'First output', firstContent: 'First content', lastOutput: 'Last output', finished: 'Finished', ttft: 'TTFT (ms)', tpot: 'TPOT (ms)', completion: 'Completion (ms)', httpStatus: 'HTTP status', errorStage: 'Error stage', errorText: 'Error text', finishReason: 'Finish reason', inputTokens: 'Input tokens', outputTokens: 'Output tokens', source: 'Source', seconds: 's', selectHint: 'Select a request', legendWait: 'Wait', legendGenerate: 'Generate', legendFinish: 'Finish', legendContent: 'Content', legendFailure: 'Failure',
} } } } });
const { cleanup, render, screen, fireEvent } = await import('@testing-library/react');
const { default: BenchmarkRequestTimeline } = await import('../src/pages/evaluations/BenchmarkRequestTimeline.tsx');
const { colors: themeColors } = await import('../src/theme/tokens.ts');
afterEach(cleanup);
after(() => dom.window.close());
function record(overrides: Partial<BenchmarkTimelineRecord> = {}): BenchmarkTimelineRecord {
  return { version: 1, run_id: 1, point_seq: 1, session_id: 'session-1', turn: 2, planned_turns: 3, attempt: 1, input_source: '', output_source: 'server_usage', output_budget: 20, metrics: {
    started_at: '2026-11-09T00:00:00Z', first_output_at: '2026-11-09T00:00:01Z', first_content_at: '2026-11-09T00:00:02Z', last_output_at: '2026-11-09T00:00:03Z', finished_at: '2026-11-09T00:00:04Z', ttft_ms: 1000, tpot_ms: null, latency_ms: 3000, completion_ms: 4000, input_tokens: 10, output_tokens: 20,
  }, ...overrides };
}
function view(data?: Timeline, props: { loading?: boolean; error?: Error; onRetry?: () => void } = {}) {
  return render(<I18nextProvider i18n={i18n}><BenchmarkRequestTimeline data={data} loading={props.loading ?? false} error={props.error} onRetry={props.onRetry ?? (() => {})} /></I18nextProvider>);
}
test('missing and invalid events do not produce zero-time bars', () => {
  const missing = record();
  missing.metrics.first_output_at = null;
  missing.metrics.last_output_at = 'invalid';
  missing.metrics.first_content_at = null;
  const { container } = view({ status: 'complete', records: [missing] });
  assert.equal(container.querySelectorAll('[data-phase]').length, 0);
  assert.ok(screen.getByText('No timing'));
  fireEvent.keyDown(screen.getByRole('button', { name: /Session session-1/ }), { key: 'Enter' });
  assert.ok(screen.getAllByText('Unknown').length > 0);
  assert.equal(container.querySelector('svg')?.textContent?.includes('NaN'), false);
});
test('failed request without output uses a red send-to-finish bar', () => {
  const failed = record({ error_stage: 'http', error_text: 'Service unavailable', http_status: 503 });
  failed.metrics.first_output_at = null;
  failed.metrics.last_output_at = null;
  failed.metrics.first_content_at = null;
  const { container } = view({ status: 'complete', records: [failed] });
  const bar = container.querySelector('[data-phase="failure"]');
  assert.equal(bar?.getAttribute('fill'), themeColors.danger, 'the theme danger color');
  assert.equal(bar?.getAttribute('width'), '745');
  assert.equal(container.querySelector('[data-phase="wait"]'), null);
  fireEvent.keyDown(screen.getByRole('button', { name: /Session session-1/ }), { key: ' ' });
  assert.ok(screen.getByText('503'));
  assert.ok(screen.getByText('Service unavailable'));
});
test('keyboard selection retains timing and nullable metric/source details', () => {
  const { container } = view({ status: 'complete', records: [record({ session_id: 'initial-session' }), record()] });
  for (const phase of ['wait', 'generate', 'finish', 'content']) assert.ok(container.querySelector(`[data-phase="${phase}"]`));
  const row = screen.getByRole('button', { name: /Session session-1.*Turn 2.*Attempt 1/ });
  assert.equal(row.getAttribute('aria-pressed'), 'false');
  fireEvent.keyDown(row, { key: 'Enter' });
  assert.equal(row.getAttribute('aria-pressed'), 'true');
  assert.ok(screen.getByText('Request details'));
  assert.ok(screen.getByText('2026-11-09T00:00:02Z'));
  assert.ok(screen.getAllByText('Unknown').length > 0);
  assert.ok(screen.getByText('server_usage'));
  assert.equal(screen.queryByText('10', { selector: '.ant-descriptions-item-content' }), null);
  assert.equal(container.querySelector('.ant-descriptions')?.closest('[data-report-exclude]'), null);
});
test('states explain disabled, unavailable, truncated, empty and loading data', () => {
  for (const [status, label] of [['disabled', 'Disabled'], ['unavailable', 'Unavailable'], ['truncated', 'Retained subset']] as const) {
    const result = view({ status, records: [] });
    assert.ok(screen.getByText(label));
    result.unmount();
  }
  const empty = view({ status: 'complete', records: [] });
  assert.ok(screen.getByText('No requests'));
  empty.unmount();
  view(undefined, { loading: true });
  assert.ok(screen.getByLabelText('Loading timeline'));
});
test('errors allow retry and pagination shows at most 25 retained requests', () => {
  let retries = 0;
  const failed = view(undefined, { error: new Error('network'), onRetry: () => { retries++; } });
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  assert.equal(retries, 1);
  assert.ok(screen.getByRole('button', { name: 'Retry' }).closest('[data-report-exclude]'));
  failed.unmount();
  const { container } = view({ status: 'truncated', records: Array.from({ length: 26 }, (_, index) => record({ session_id: `session-${index}` })) });
  assert.equal(container.querySelectorAll('svg [role="button"]').length, 25);
  assert.ok(screen.getByText('1–25 of 26 retained requests'));
  fireEvent.click(screen.getByTitle('2'));
  assert.equal(container.querySelectorAll('svg [role="button"]').length, 1);
  assert.ok(screen.getByText('26–26 of 26 retained requests'));
  assert.ok(screen.getByText('session-25'));
});
