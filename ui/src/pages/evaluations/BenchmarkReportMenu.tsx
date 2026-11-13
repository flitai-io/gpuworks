import { DownloadOutlined } from "@ant-design/icons";
import { Button, Dropdown, message } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { BenchmarkVisualFormat } from "./benchmarkReportExport";

export type BenchmarkReportFormat = "markdown" | "csv" | "html";

export function benchmarkReportURL(id: number, format: BenchmarkReportFormat, language?: string) {
	const locale = format === "html" && language ? `&lang=${encodeURIComponent(language)}` : "";
	return `/api/v1/benchmarks/${id}/report?format=${format}${locale}`;
}

export function benchmarkReportDownloads(id: number, language?: string) {
	return (["markdown", "csv", "html"] as const).map((format) => ({ format, href: benchmarkReportURL(id, format, language) }));
}

export default function BenchmarkReportMenu({ runID, onExportVisual, visualDisabled = false }: { runID: number; onExportVisual?: (format: BenchmarkVisualFormat) => Promise<void>; visualDisabled?: boolean }) {
	const { t, i18n } = useTranslation("evaluations");
	const [exporting, setExporting] = useState(false);
	const exportVisual = async (format: BenchmarkVisualFormat) => {
		if (!onExportVisual || exporting) return;
		setExporting(true);
		try { await onExportVisual(format); }
		catch { message.error(t("agentic.export.visualFailed")); }
		finally { setExporting(false); }
	};
	return <Dropdown disabled={exporting} menu={{ items: [
		...(onExportVisual ? (["html", "png"] as const).map((format) => ({ key: format, label: t(`agentic.export.${format}`), disabled: visualDisabled, onClick: () => { void exportVisual(format); } })) : []),
		...benchmarkReportDownloads(runID, i18n.resolvedLanguage || i18n.language).filter(({ format }) => !onExportVisual || format !== "html").map(({ format, href }) => ({
		key: `download-${format}`,
		label: <a href={href} download>{t(`agentic.export.${format}`)}</a>,
		})),
	] }} trigger={["click"]}>
		<Button loading={exporting} icon={<DownloadOutlined />}>{t(exporting ? "agentic.export.exporting" : "agentic.export.button")}</Button>
	</Dropdown>;
}
