import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { Instance, InstanceEvent } from "../api";
import { instanceEventStage } from "./instanceEvents";
import { HelpTip } from "../ui-kit/HelpTip";

function validDuration(value: number | undefined): value is number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0;
}

function duration(seconds: number, t: TFunction<"deployments">): string {
  const rounded = Math.round(seconds * 10) / 10;
  if (rounded < 60) return t("instance.startup.duration.seconds", { seconds: rounded });
  const whole = Math.round(seconds);
  if (whole < 3600) return t("instance.startup.duration.minutes", { minutes: Math.floor(whole / 60), seconds: whole % 60 });
  return t("instance.startup.duration.hours", { hours: Math.floor(whole / 3600), minutes: Math.floor(whole / 60) % 60, seconds: whole % 60 });
}

export default function InstanceStartupStatus({ inst, events }: { inst: Instance; events: InstanceEvent[] }) {
  const { t } = useTranslation("deployments");
  const [now, setNow] = useState(Date.now);
  const waiting = inst.state === "starting" && inst.phase === "health_check";
  useEffect(() => {
    if (!waiting) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [waiting]);
  const started = events.reduce<number | undefined>((latest, event) => {
    if (instanceEventStage(event)?.phase !== "health_check") return latest;
    const time = Date.parse(event.created_at);
    return Number.isFinite(time) && (latest == null || time > latest) ? time : latest;
  }, undefined);
  const elapsed = waiting && started != null && started <= now ? (now - started) / 1000 : undefined;
  const estimate = inst.startup_estimate;
  const budget = validDuration(inst.start_timeout_sec) && inst.start_timeout_sec > 0 ? inst.start_timeout_sec : undefined;
  const estimatedSeconds = estimate?.method === "weight_reference"
    && validDuration(estimate.estimated_seconds) && estimate.estimated_seconds > 0
    && validDuration(estimate.weight_bytes) && estimate.weight_bytes > 0
    && validDuration(estimate.reference_mib_per_sec) && estimate.reference_mib_per_sec > 0
    && validDuration(estimate.initialization_seconds)
    ? estimate.estimated_seconds : undefined;
  const stages = (["ModelLoadingSeconds", "CompilationSeconds", "GraphCaptureSeconds"] as const)
    .flatMap((key) => {
      const seconds = inst.measured?.[key];
      return validDuration(seconds) ? [{ key, seconds }] : [];
    });
  const readiness = inst.measured?.StartupSeconds;
  if (!estimate && budget == null && !stages.length && !validDuration(readiness)) return null;
  const unavailable = ({
    missing_weight: "instance.startup.unknown",
    unsupported: "instance.startup.unsupported",
  } as Record<string, string>)[estimate?.method ?? ""] ?? "instance.startup.unavailable";
  const source = t(`instance.startup.source.${inst.start_timeout_source || "unknown"}`, { defaultValue: t("instance.startup.source.unknown") });
  const estimateText = estimate && (estimatedSeconds != null
    ? t("instance.startup.estimate", { duration: duration(estimatedSeconds, t) })
    : t(unavailable));
  const summary = [budget != null && t("instance.startup.budget", { duration: duration(budget, t), source }), estimateText].filter(Boolean).join(" · ");
  const wait = waiting && (elapsed == null
    ? t("instance.startup.elapsedUnknown")
    : [t("instance.startup.elapsed", { duration: duration(elapsed, t) }),
      estimatedSeconds != null && elapsed > estimatedSeconds && budget != null && elapsed < budget && t("instance.startup.slow")].filter(Boolean).join(" · "));
  const measured = [
    validDuration(readiness) && t("instance.startup.observedReady", { duration: duration(readiness, t) }),
    ...stages.map(({ key, seconds }) => t(`instance.startup.measured.${key}`, { duration: duration(seconds, t) })),
  ].filter(Boolean).join(" · ");
  return (
    <section aria-label={t("instance.startup.title")} className="mt-2 space-y-1 text-xs text-muted-foreground">
      {summary && <p className="flex items-center gap-1">{summary}{estimatedSeconds != null && estimate && <HelpTip content={t("instance.startup.estimateHelp", {
        weight: Math.round(estimate.weight_bytes / 1024 ** 2 * 10) / 10,
        rate: estimate.reference_mib_per_sec,
        initialization: duration(estimate.initialization_seconds!, t),
      })} />}</p>}
      {inst.start_timeout_conflict && validDuration(inst.engine_start_timeout_sec) && <p className="text-amber-700 dark:text-amber-400">{t("instance.startup.conflict", { duration: duration(inst.engine_start_timeout_sec, t) })}</p>}
      {estimatedSeconds != null && validDuration(inst.engine_start_timeout_sec) && inst.engine_start_timeout_sec > 0 && inst.engine_start_timeout_sec < estimatedSeconds && <p className="text-amber-700 dark:text-amber-400">{t("instance.startup.engineDeadlineShort", { duration: duration(inst.engine_start_timeout_sec, t) })}</p>}
      {wait && <p>{wait}</p>}
      {measured && <p className="flex items-center gap-1">{measured}{!!stages.length && <HelpTip content={t("instance.startup.measuredHelp")} />}</p>}
    </section>
  );
}
