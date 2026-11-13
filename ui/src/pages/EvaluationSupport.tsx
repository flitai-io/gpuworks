import { DownloadOutlined } from "@ant-design/icons";
import { Button, Card, Col, Collapse, Descriptions, Statistic, Table } from "antd";
import { Fragment, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { BenchmarkPoint, BenchmarkRun } from "../api";
import { advancedPercentiles, advancedPointFacts } from "./evaluations/advancedPoint";

const fmt = (value?: number, digits = 1) => value == null || !Number.isFinite(value) ? "-" : value.toLocaleString(undefined, { maximumFractionDigits: digits });

export function Metric({ title, value, suffix }: { title: ReactNode; value?: number; suffix: string }) {
	return <Col xs={12} md={6}><Card size="small"><Statistic title={title} value={value == null ? "-" : Number(value.toFixed(1))} suffix={suffix} /></Card></Col>;
}

export function fmtMs(value?: number) {
	return value == null ? "-" : `${fmt(value)} ms`;
}


export function AdvancedResult({ run, point, expanded = false }: { run: BenchmarkRun; point?: BenchmarkPoint; expanded?: boolean }) {
	const [activeKeys, setActiveKeys] = useState<string[]>([]);
	const { t } = useTranslation("evaluations");
	const rows = advancedPercentiles(point);
	return <Collapse activeKey={expanded ? ["advanced"] : activeKeys} onChange={(keys) => setActiveKeys(typeof keys === "string" ? [keys] : keys)} items={[{
		key: "advanced",
		label: t("support.advanced"),
		children: <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
			<Descriptions size="small" column={{ xs: 1, sm: 2 }} items={advancedPointFacts(point, t).map(({ key, label, lines }) => ({
				key, label, children: key === "detail" && point?.detail_path
					? <Button data-report-exclude size="small" icon={<DownloadOutlined />} href={`/api/v1/benchmarks/${run.id}/points/${point.seq}/detail`}>{lines[0]}</Button>
					: lines.map((line, index) => <Fragment key={index}>{index > 0 && <br />}<span>{line}</span></Fragment>),
			}))} />
			<Table size="small" rowKey="p" pagination={false} dataSource={rows} columns={[
				{ title: t("support.percentile"), dataIndex: "p" },
				{ title: "TTFT", dataIndex: "ttft", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
				{ title: "TPOT", dataIndex: "tpot", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
				{ title: "Latency", dataIndex: "latency", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
				{ title: t("evidence.chunkInterval"), dataIndex: "itl", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
			]} />
		</div>,
	}]} />;
}
