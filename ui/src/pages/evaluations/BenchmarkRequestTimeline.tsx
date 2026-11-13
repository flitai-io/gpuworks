import { useState } from 'react';
import { Alert, Button, Card, Descriptions, Empty, Pagination, Space, Spin, Typography } from 'antd';
import { useTranslation } from 'react-i18next';
import { colors as themeColors } from '../../theme/tokens';
import type { BenchmarkRequestTimeline as Timeline, BenchmarkTimelineRecord } from '../../api';

interface Props {
  data?: Timeline;
  loading: boolean;
  error?: Error | null;
  onRetry: () => void;
}

const colors = { wait: themeColors.chartPrimary, generate: themeColors.chartTertiary, finish: themeColors.muted, content: themeColors.warning, failure: themeColors.danger };
const pageSize = 25;
function time(value: string | null | undefined): number | null {
  if (!value) return null;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : null;
}
function events(record: BenchmarkTimelineRecord) {
  return [record.metrics.started_at, record.metrics.first_output_at, record.metrics.first_content_at,
    record.metrics.last_output_at, record.metrics.finished_at].map(time);
}

export default function BenchmarkRequestTimeline({ data, loading, error, onRetry }: Props) {
  const { t } = useTranslation('evaluations');
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<BenchmarkTimelineRecord>();
  const records = data?.records ?? [];
  const currentPage = Math.min(page, Math.max(1, Math.ceil(records.length / pageSize)));
  const visible = records.slice((currentPage - 1) * pageSize, currentPage * pageSize);
  const known = records.flatMap(events).filter((value): value is number => value !== null);
  const start = known.length ? Math.min(...known) : 0;
  const end = known.length ? Math.max(...known) : start;
  const width = 1000, left = 230, right = 975, rowHeight = 34;
  const x = (value: number) => left + (value - start) / (end - start || 1) * (right - left);
  const unknown = t('timeline.unknown');
  const identity = (record: BenchmarkTimelineRecord) => `${t('timeline.session')} ${record.session_id || unknown} · ${t('timeline.turn')} ${record.turn ?? unknown} · ${t('timeline.attempt')} ${record.attempt ?? unknown}`;
  const value = (input: string | number | null | undefined) => input === null || input === undefined || input === '' || (typeof input === 'number' && !Number.isFinite(input)) ? unknown : typeof input === 'number' ? input.toLocaleString(undefined, { maximumFractionDigits: 3 }) : input;
  const timestamp = (input: string | null) => time(input) === null ? unknown : input;
  const active = selected && visible.includes(selected) ? selected : visible[0];
  const details = active ? [
    { key: 'session', label: t('timeline.session'), children: value(active.session_id) },
    { key: 'turn', label: t('timeline.turn'), children: value(active.turn) },
    { key: 'attempt', label: t('timeline.attempt'), children: value(active.attempt) },
    { key: 'plannedTurns', label: t('timeline.plannedTurns'), children: value(active.planned_turns) },
    { key: 'outputBudget', label: t('timeline.outputBudget'), children: value(active.output_budget) },
    { key: 'sent', label: t('timeline.sent'), children: timestamp(active.metrics.started_at) },
    { key: 'firstOutput', label: t('timeline.firstOutput'), children: timestamp(active.metrics.first_output_at) },
    { key: 'firstContent', label: t('timeline.firstContent'), children: timestamp(active.metrics.first_content_at) },
    { key: 'lastOutput', label: t('timeline.lastOutput'), children: timestamp(active.metrics.last_output_at) },
    { key: 'finished', label: t('timeline.finished'), children: timestamp(active.metrics.finished_at) },
    { key: 'ttft', label: t('timeline.ttft'), children: value(active.metrics.ttft_ms) },
    { key: 'tpot', label: t('timeline.tpot'), children: value(active.metrics.tpot_ms) },
    { key: 'completion', label: t('timeline.completion'), children: value(active.metrics.completion_ms) },
    { key: 'latency', label: t('timeline.latency'), children: value(active.metrics.latency_ms) },
    { key: 'httpStatus', label: t('timeline.httpStatus'), children: value(active.http_status) },
    { key: 'errorStage', label: t('timeline.errorStage'), children: value(active.error_stage) },
    { key: 'errorText', label: t('timeline.errorText'), children: value(active.error_text) },
    { key: 'finishReason', label: t('timeline.finishReason'), children: value(active.finish_reason) },
    { key: 'inputTokens', label: t('timeline.inputTokens'), children: active.input_source ? value(active.metrics.input_tokens) : unknown },
    { key: 'outputTokens', label: t('timeline.outputTokens'), children: active.output_source ? value(active.metrics.output_tokens) : unknown },
    { key: 'inputSource', label: `${t('timeline.inputTokens')} · ${t('timeline.source')}`, children: active.input_source ? t(`evidence.source.${active.input_source}`, { defaultValue: active.input_source }) : unknown },
    { key: 'outputSource', label: `${t('timeline.outputTokens')} · ${t('timeline.source')}`, children: active.output_source ? t(`evidence.source.${active.output_source}`, { defaultValue: active.output_source }) : unknown },
  ] : [];

  return <Card className="benchmark-request-timeline" title={t('timeline.title')}>
    {loading ? <Spin tip={t('timeline.loading')}><div style={{ minHeight: 80 }} aria-label={t('timeline.loading')} /></Spin>
      : error ? <Alert type="error" showIcon message={t('timeline.loadFailed')} description={error.message} action={<Button data-report-exclude onClick={onRetry}>{t('timeline.retry')}</Button>} />
        : !data || data.status === 'disabled' || data.status === 'unavailable' ? <Alert type="info" showIcon message={t(data?.status === 'disabled' ? 'timeline.disabled' : 'timeline.unavailable')} action={data?.status !== 'disabled' ? <Button data-report-exclude onClick={onRetry}>{t('timeline.retry')}</Button> : undefined} />
          : <Space direction="vertical" style={{ width: '100%' }} size="middle">
            {data.status === 'truncated' && <Alert type="warning" showIcon message={t('timeline.truncated')} />}
            {records.length === 0 ? <Empty description={t('timeline.empty')} /> : <>
              <Typography.Text>{t('timeline.scope', { start: (currentPage - 1) * pageSize + 1, end: Math.min(currentPage * pageSize, records.length), total: records.length })}</Typography.Text>
              <Space wrap>{(['wait', 'generate', 'finish', 'content', 'failure'] as const).map((kind) => <span key={kind} style={{ display: 'inline-flex', alignItems: 'center', gap: 5 }}><span style={{ width: 12, height: 12, background: colors[kind], display: 'inline-block' }} />{t(`timeline.${{ wait: 'legendWait', generate: 'legendGenerate', finish: 'legendFinish', content: 'legendContent', failure: 'legendFailure' }[kind]}`)}</span>)}</Space>
              <Typography.Text data-report-exclude type="secondary">{t('timeline.selectHint')}</Typography.Text>
              <div style={{ overflowX: 'auto' }}>
                <svg role="group" aria-label={t('timeline.title')} viewBox={`0 0 ${width} ${visible.length * rowHeight + 50}`} style={{ width: '100%', minWidth: 700, display: 'block' }}>
                  {known.length > 0 && [0, 0.25, 0.5, 0.75, 1].map((fraction) => <g key={fraction}>
                    <line x1={left + fraction * (right - left)} x2={left + fraction * (right - left)} y1={25} y2={visible.length * rowHeight + 25} stroke={themeColors.border} />
                    <text x={left + fraction * (right - left)} y={16} textAnchor="middle" fontSize={11} fill="currentColor">{((end - start) * fraction / 1000).toFixed(2)} {t('timeline.seconds')}</text>
                  </g>)}
                  {visible.map((record, index) => {
                    const [sent, firstOutput, firstContent, lastOutput, finished] = events(record);
                    const y = 40 + index * rowHeight;
                    const failure = Boolean(record.error_text || record.error_stage || record.canceled || (record.http_status && record.http_status >= 400));
                    const segments = firstOutput === null && failure ? [[sent, finished, 'failure']] as const : [[sent, firstOutput, 'wait'], [firstOutput, lastOutput, 'generate'], [lastOutput, finished, 'finish']] as const;
                    const hasBar = segments.some(([from, to]) => from !== null && to !== null && to >= from);
                    return <g key={(currentPage - 1) * pageSize + index} role="button" tabIndex={0} aria-label={identity(record)} aria-pressed={active === record} onClick={() => setSelected(record)} onKeyDown={(event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); setSelected(record); } }} style={{ cursor: 'pointer', outlineOffset: 2 }}>
                      <title>{identity(record)}</title>
                      <rect x={0} y={y - 14} width={width} height={rowHeight - 2} fill={active === record ? themeColors.primarySoft : 'transparent'} />
                      <text x={5} y={y + 4} fontSize={11} fill="currentColor">{identity(record).slice(0, 42)}</text>
                      {segments.map(([from, to, kind]) => from !== null && to !== null && to >= from ? <rect key={kind} data-phase={kind} x={x(from)} y={y - 7} width={Math.max(1, x(to) - x(from))} height={14} rx={2} fill={colors[kind]} /> : null)}
                      {firstContent !== null && <line data-phase="content" x1={x(firstContent)} x2={x(firstContent)} y1={y - 11} y2={y + 11} stroke={colors.content} strokeWidth={3} />}
                      {!hasBar && <text x={left + 5} y={y + 4} fontSize={11} fill="currentColor">{t('timeline.noTiming')}</text>}
                    </g>;
                  })}
                </svg>
              </div>
              <div data-report-exclude><Pagination current={currentPage} total={records.length} pageSize={pageSize} showSizeChanger={false} onChange={(nextPage) => { setPage(nextPage); setSelected(undefined); }} /></div>
              {active && <Descriptions title={t('timeline.requestDetails')} bordered size="small" column={{ xs: 1, sm: 2, lg: 3 }} items={details} />}
            </>}
          </Space>}
  </Card>;
}
