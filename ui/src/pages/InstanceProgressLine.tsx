import { useEffect, useRef, useState } from "react";
import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import { formatBytes } from "../format";

const formatSize = (value?: number) => formatBytes(value, { compact: true });

// The agent stops trusting a transfer rate five seconds after the byte counter
// last moved. Extrapolating past that would invent throughput for a stalled
// transfer, which is exactly the case an operator needs to see.
const RATE_STALE_MS = 5000;
const TICK_MS = 500;

function prefersReducedMotion(): boolean {
  try {
    return typeof window !== "undefined" && !!window.matchMedia?.("(prefers-reduced-motion: reduce)")?.matches;
  } catch {
    return false;
  }
}

/**
 * The agent reports at most once a second
 * per instance, so the byte counter is interpolated from the reported rate
 * between updates - never past the total, never once the rate has gone stale,
 * and not at all when the viewer asked for reduced motion.
 */
export default function InstanceProgressLine({
  bytesDone, bytesTotal, rateBps,
}: {
  bytesDone?: number;
  bytesTotal?: number;
  rateBps?: number;
}) {
  const { t } = useTranslation("deployments");
  const reported = bytesDone ?? 0;
  const total = bytesTotal ?? 0;
  const rate = total > 0 && reported >= total ? 0 : Math.max(0, rateBps ?? 0);
  const [displayed, setDisplayed] = useState(reported);
  const since = useRef(Date.now());

  useEffect(() => {
    since.current = Date.now();
    setDisplayed(reported);
  }, [reported, total, rate]);

  useEffect(() => {
    if (rate <= 0 || reported <= 0 || prefersReducedMotion()) return;
    const timer = setInterval(() => {
      const elapsed = Date.now() - since.current;
      if (elapsed > RATE_STALE_MS) return;
      const projected = reported + Math.round((rate * elapsed) / 1000);
      setDisplayed(total > 0 ? Math.min(total, projected) : projected);
    }, TICK_MS);
    return () => clearInterval(timer);
  }, [rate, reported, total]);

  const hasBytes = total > 0 || reported > 0 || rate > 0;
  return (
    <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>
      {hasBytes ? <>{formatSize(displayed)} / {formatSize(total)}{rate > 0 && ` · ${formatSize(rate)}/s`}</> : t("instance.bytesUnknown")}
    </Typography.Text>
  );
}
