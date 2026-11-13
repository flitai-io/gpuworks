import { pointTimingValue, pointTokenLength, pointTokenRate } from "./evaluationUtils";
import { useMemo, useState } from "react";
import { DownloadOutlined, HolderOutlined, SettingOutlined } from "@ant-design/icons";
import { Button, Checkbox, Dropdown, Flex, Table, Tag, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { DndContext, KeyboardSensor, PointerSensor, closestCenter, useSensor, useSensors, type DragEndEvent } from "@dnd-kit/core";
import { SortableContext, arrayMove, sortableKeyboardCoordinates, verticalListSortingStrategy, useSortable } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import type { BenchmarkPoint, BenchmarkRun } from "../../api";
import { bestPoint, format, isOpenLoop, pointMeetsSLA } from "./evaluationUtils";
import MetricLabel, { type MetricHelpKey } from "./MetricLabel";
import { DEFAULT_VISIBLE_POINT_COLUMNS, POINT_COLUMNS_STORAGE_KEY, defaultPointColumnPreference, parsePointColumnPreference, pointDerivedValues, type PointColumnKey } from "./pointColumnPrefs";

function SortableColumnItem({ id, title, checked, onToggle }: { id: PointColumnKey; title: string; checked: boolean; onToggle: (value: boolean) => void }) {
	const sortable = useSortable({ id, disabled: id === "point" });
	return <Flex ref={sortable.setNodeRef} align="center" gap={8} style={{ transform: CSS.Transform.toString(sortable.transform), transition: sortable.transition, padding: "5px 8px", background: "#fff", minWidth: 230 }}>
		<span {...sortable.attributes} {...sortable.listeners} style={{ cursor: id === "point" ? "not-allowed" : "grab", color: "#96989b" }}><HolderOutlined /></span>
		<Checkbox checked={checked} disabled={id === "point"} onChange={(event) => onToggle(event.target.checked)}>{title}</Checkbox>
	</Flex>;
}

export default function PointResultsTable({ run, selectedSeq, onSelect }: { run: BenchmarkRun; selectedSeq?: number; onSelect: (point: BenchmarkPoint) => void }) {
	const { t } = useTranslation("evaluations");
	const titles = useMemo(() => pointColumnTitles(t), [t]);
	const [preference, setPreference] = useState(() => parsePointColumnPreference(typeof localStorage === "undefined" ? null : localStorage.getItem(POINT_COLUMNS_STORAGE_KEY)));
	const sensors = useSensors(
		useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
		useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
	);
	const save = (next: typeof preference) => {
		setPreference(next);
		localStorage.setItem(POINT_COLUMNS_STORAGE_KEY, JSON.stringify(next));
	};
	const dragEnd = ({ active, over }: DragEndEvent) => {
		if (!over || active.id === over.id || active.id === "point" || over.id === "point") return;
		const from = preference.order.indexOf(active.id as PointColumnKey), to = preference.order.indexOf(over.id as PointColumnKey);
		save({ ...preference, order: arrayMove(preference.order, from, to) });
	};
	const settings = <div onClick={(event) => event.stopPropagation()} style={{ background: "#fff", padding: 8, boxShadow: "0 6px 16px rgba(0,0,0,.14)", maxHeight: 480, overflowY: "auto" }}>
		<Typography.Text strong style={{ display: "block", padding: "4px 8px 8px" }}>{t("pointTable.settings")}</Typography.Text>
		<DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={dragEnd}><SortableContext items={preference.order} strategy={verticalListSortingStrategy}>{preference.order.map((key) => <SortableColumnItem key={key} id={key} title={titles[key]} checked={preference.visible.includes(key)} onToggle={(checked) => save({ ...preference, visible: checked ? [...preference.visible, key] : preference.visible.filter((item) => item !== key) })} />)}</SortableContext></DndContext>
		<Button type="link" onClick={() => save(defaultPointColumnPreference())}>{t("pointTable.restore")}</Button>
	</div>;
	const registry = useMemo(() => columnRegistry(run, t, titles), [run, t, titles]);
	const openLoop = (run.points || []).some(isOpenLoop);
	const columns = preference.order.filter((key) => preference.visible.includes(key) && (key !== "sla" || run.mode === "sla") && (key !== "request_rate" || openLoop)).map((key) => registry[key]);
	const scrollX = columns.reduce((sum, column) => sum + Number(column.width || 130), 0);
	return <Table<BenchmarkPoint>
		title={() => <Flex justify="space-between" align="center"><Typography.Text strong>{t("pointTable.title")}</Typography.Text><Dropdown trigger={["click"]} dropdownRender={() => settings}><Button data-report-exclude icon={<SettingOutlined />}>{t("pointTable.customize")}</Button></Dropdown></Flex>}
		rowKey="seq" pagination={false} sticky scroll={{ x: Math.max(900, scrollX) }} dataSource={run.points || []} columns={columns}
		onRow={(point) => ({ onClick: () => onSelect(point), style: { cursor: "pointer", background: selectedSeq === point.seq ? "#fff7e6" : undefined } })}
	/>;
}

function pointColumnTitles(t: TFunction): Record<PointColumnKey, string> {
	return Object.fromEntries(defaultPointColumnPreference().order.map((key) => [key, t(`pointTable.column.${key}`)])) as Record<PointColumnKey, string>;
}

function columnRegistry(run: BenchmarkRun, t: TFunction, titles: Record<PointColumnKey, string>): Record<PointColumnKey, ColumnsType<BenchmarkPoint>[number]> {
	const markedSeq = run.verdict?.best_point_seq ?? bestPoint(run)?.seq;
	const terminal = run.state === "done" || run.state === "failed" || run.state === "canceled";
	const metric = (title: string, render: (point: BenchmarkPoint) => number | undefined, suffix = "", help?: MetricHelpKey) => ({ title: help ? <MetricLabel label={title} help={help} /> : title, width: 145, render: (_: unknown, point: BenchmarkPoint) => { const value = render(point); return value == null || !Number.isFinite(value) ? "—" : `${format(value)}${suffix}`; } });
	return {
		point: { title: titles.point, dataIndex: "seq", fixed: "left", width: 72, render: (value) => `#${value}` },
		// An open-loop point's concurrency is its in-flight cap.
		concurrency: { title: <MetricLabel label={titles.concurrency} help="concurrency" />, dataIndex: "concurrency", width: 96, render: (value: number, point) => isOpenLoop(point) ? (value > 0 ? `≤${value}` : t("pointTable.unlimited")) : value },
		request_rate: { title: <MetricLabel label={titles.request_rate} help="requestRate" />, width: 120, render: (_, point) => isOpenLoop(point) ? `${format(point.request_rate, 2)} req/s` : "—" },
		length: { title: "ISL / OSL", width: 110, render: (_, point) => `${format(pointTokenLength(point, "input"))} / ${format(pointTokenLength(point, "output"))}` },
		avg_output: metric(titles.avg_output, (point) => pointTokenRate(point, "output_tput"), " tok/s", "output"),
		avg_total: metric(titles.avg_total, (point) => pointTokenRate(point, "total_tput"), " tok/s", "total"),
		request: metric(titles.request, (point) => point.request_tput, " req/s", "request"),
		interactivity: { ...metric(titles.interactivity, (point) => pointDerivedValues(point).interactivity, " tok/s/user", "interactivity"), width: 170 },
		ttft_p99: metric(titles.ttft_p99, (point) => pointTimingValue(point, "p99_ttft"), " ms", "ttft"), tpot_p99: metric(titles.tpot_p99, (point) => pointTimingValue(point, "p99_tpot"), " ms", "tpot"),
		latency_p99: metric(titles.latency_p99, (point) => pointTimingValue(point, "p99_latency"), " ms", "latency"),
		sla: { title: titles.sla, width: 105, render: (_, point) => pointMeetsSLA(run, point) ? <Tag color="green">{t("pointTable.slaMet")}</Tag> : <Tag color="red">{t("pointTable.slaFailed")}</Tag> },
		mark: { title: titles.mark, width: 105, render: (_, point) => markedSeq === point.seq ? <Tag color="blue">{terminal ? t("pointTable.verdictPoint") : t("pointTable.currentBest")}</Tag> : "—" },
		detail: { title: titles.detail, width: 90, render: (_, point) => point.detail_path ? <Button data-report-exclude type="link" size="small" icon={<DownloadOutlined />} href={`/api/v1/benchmarks/${run.id}/points/${point.seq}/detail`} onClick={(event) => event.stopPropagation()} /> : "—" },
		max_output: metric(titles.max_output, (point) => pointDerivedValues(point).maxOutput, " tok/s", "maxOutput"), max_total: metric(titles.max_total, (point) => pointDerivedValues(point).maxTotal, " tok/s", "maxTotal"),
		avg_ttft: metric(titles.avg_ttft, (point) => pointTimingValue(point, "avg_ttft"), " ms"), avg_tpot: metric(titles.avg_tpot, (point) => pointTimingValue(point, "avg_tpot"), " ms"),
		avg_latency: metric(titles.avg_latency, (point) => pointTimingValue(point, "avg_latency"), " ms"),
		success_rate: metric(titles.success_rate, (point) => pointDerivedValues(point).successRate, "%", "success"), ttft_p50: metric(titles.ttft_p50, (point) => pointTimingValue(point, "p50_ttft"), " ms"), tpot_p50: metric(titles.tpot_p50, (point) => pointTimingValue(point, "p50_tpot"), " ms"),
		latency_p50: metric(titles.latency_p50, (point) => pointTimingValue(point, "p50_latency"), " ms"),
		duration: metric(titles.duration, (point) => pointDerivedValues(point).durationSeconds, " s"), request_total: metric(titles.request_total, (point) => pointDerivedValues(point).requestTotal),
	};
}

export { DEFAULT_VISIBLE_POINT_COLUMNS };
