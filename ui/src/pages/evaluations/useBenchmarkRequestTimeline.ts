import { useEffect, useState } from "react";
import { api, type BenchmarkRequestTimeline } from "../../api";

export default function useBenchmarkRequestTimeline(runID: number, pointSeq?: number) {
	const key = `${runID}:${pointSeq}`;
	const [attempt, setAttempt] = useState(0);
	const [result, setResult] = useState<{ key: string; attempt: number; data?: BenchmarkRequestTimeline; error?: Error }>();
	useEffect(() => {
		if (pointSeq == null) return;
		const controller = new AbortController();
		api.benchmarkPointTimeline(runID, pointSeq, controller.signal).then(
			(data) => { if (!controller.signal.aborted) setResult({ key, attempt, data }); },
			(error: Error) => { if (!controller.signal.aborted) setResult({ key, attempt, error }); },
		);
		return () => controller.abort();
	}, [runID, pointSeq, key, attempt]);
	// A different point (or retry) must not temporarily show the previous
	// request's records while the replacement request is in flight.
	const current = result?.key === key && result.attempt === attempt ? result : undefined;
	return {
		data: current?.data,
		error: current?.error,
		loading: pointSeq != null && current == null,
		refetch: () => setAttempt((value) => value + 1),
	};
}
