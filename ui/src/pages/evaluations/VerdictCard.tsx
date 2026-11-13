import { Alert, Button, Card, Col, Empty, Row, Space, Statistic, Tag, Typography } from "antd";
import type { BenchmarkMeta, BenchmarkPoint, BenchmarkRun } from "../../api";
import { useTranslation } from "react-i18next";
import { bestPoint, format, pointMeetsSLA, pointParameterLabel, pointTokenRate, verdictPresentation } from "./evaluationUtils";
import { localizedText } from "../../i18n/localizedText";
import MetricLabel from "./MetricLabel";

export default function VerdictCard({ run, meta, onRerun }: { run: BenchmarkRun; meta?: BenchmarkMeta; onRerun?: (maxConcurrency?: number) => void }) {
	const { t, i18n } = useTranslation("evaluations");
	const language = i18n.resolvedLanguage || i18n.language;
	const presentation = verdictPresentation(run, t);
	const point = (run.points?.length ? bestPoint(run) : undefined) as BenchmarkPoint | undefined;
	const status = meta?.statuses.find((item) => item.value === run.state);
	const successRate = point ? point.success / Math.max(1, point.success + point.errors) * 100 : undefined;
	const terminal = run.state === "done" || run.state === "failed" || run.state === "canceled";
	const nearest = !terminal && run.mode === "sla" && point && !pointMeetsSLA(run, point);
	const cardTitle = terminal ? t("verdictCard.verdict") : nearest ? t("verdictCard.nearest") : t("verdictCard.best");
	return (
		<Card
			className="benchmark-verdict-card"
			title={<Space><span>{cardTitle}</span><Tag color={status?.color || "default"}>{localizedText(status?.label, language) || run.state}</Tag></Space>}
		>
			<Alert
				showIcon
				type={presentation.tone}
				message={presentation.title}
				description={presentation.description}
				action={presentation.suggestion && onRerun ? <Button data-report-exclude onClick={() => onRerun(presentation.suggestion?.maxConcurrency)}>{presentation.suggestion.label}</Button> : undefined}
			/>
			{point ? <Typography.Text type="secondary" style={{ display: "block", marginTop: 12 }}>{cardTitle} · {pointParameterLabel(point)}</Typography.Text> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("verdictCard.waiting")} style={{ marginBlock: 12 }} />}
			<Row gutter={[12, 12]} style={{ marginTop: 16 }}>
				<Col xs={24} sm={6}><Statistic title={<MetricLabel label={t("verdictCard.output")} help="output" />} value={format(point ? pointTokenRate(point, "output_tput") : undefined)} suffix="tok/s" /></Col>
				<Col xs={24} sm={6}><Statistic title={<MetricLabel label={t("verdictCard.total")} help="total" />} value={format(point ? pointTokenRate(point, "total_tput") : undefined)} suffix="tok/s" /></Col>
				<Col xs={24} sm={6}><Statistic title={<MetricLabel label={t("verdictCard.request")} help="request" />} value={format(point?.request_tput)} suffix="req/s" /></Col>
				<Col xs={24} sm={6}><Statistic title={<MetricLabel label={t("verdictCard.success")} help="success" />} value={format(successRate)} suffix="%" /></Col>
			</Row>
		</Card>
	);
}
