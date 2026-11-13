import { Alert, Button, Space, Spin, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { benchmarkErrorText } from "./evaluationUtils";

export default function BenchmarkCurveNotice({ active, loading, hasResponse, hasSamples, runState, completed, runError, error, onRetry, warmup }: {
	warmup?: boolean;
	active: boolean;
	loading: boolean;
	hasResponse: boolean;
	hasSamples: boolean;
	runState?: string;
	completed: number;
	runError?: string;
	error?: Error | null;
	onRetry: () => void;
}) {
	const { t } = useTranslation("evaluations");
	if (active && warmup) { return <Alert style={{ marginBottom: 12 }} type="info" showIcon message={t("detail.warmupRecovery")} description={runError} />; }
	if (loading && !hasResponse) {
		return <Space role="status" style={{ marginBottom: 12 }}><Spin size="small" /><Typography.Text type="secondary">{t("detail.curveLoading")}</Typography.Text></Space>;
	}
	if (error && !hasResponse) {
		return <Alert style={{ marginBottom: 12 }} type="error" showIcon message={t("detail.curveLoadFailed")} description={error.message} action={<Button data-report-exclude size="small" onClick={onRetry}>{t("common.retry")}</Button>} />;
	}
	if (!active && !hasSamples) {
		const failedBeforeRequest = runState === "failed" && completed === 0;
		return <Alert style={{ marginBottom: 12 }} type="info" showIcon message={t(failedBeforeRequest ? "detail.curveNoRequests" : "detail.curveLegacyMissing")} description={failedBeforeRequest && runError ? benchmarkErrorText(t, runError) : undefined} />;
	}
	if (error) {
		return <Alert style={{ marginBottom: 12 }} type="warning" showIcon message={t("detail.curveRefreshFailed")} description={error.message} action={<Button data-report-exclude size="small" onClick={onRetry}>{t("common.retry")}</Button>} />;
	}
	return null;
}
