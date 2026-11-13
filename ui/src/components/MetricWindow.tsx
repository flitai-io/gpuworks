import { createContext, useContext, useState, type ReactNode } from "react";
import { Notice } from "../ui-kit/Notice";
import { useTranslation } from "react-i18next";
import TimeRangePicker from "./TimeRangePicker";
import { RETENTION_WARNING_DAYS, type TimeRangeSelection } from "../timeRange";

const initial: TimeRangeSelection = { kind: "quick", key: "15m" };
type WindowState = [TimeRangeSelection, (next: TimeRangeSelection) => void];
const SharedWindow = createContext<WindowState | null>(null);

export function MetricWindowProvider({ children }: { children: ReactNode }) {
  const state = useState<TimeRangeSelection>(initial);
  return <SharedWindow.Provider value={state}>{children}</SharedWindow.Provider>;
}

export function useMetricWindow(): WindowState {
  const shared = useContext(SharedWindow);
  const local = useState<TimeRangeSelection>(initial);
  return shared ?? local;
}

export function RangeHeader({
  selection,
  onChange,
  beyondRetention,
  children,
}: {
  selection: TimeRangeSelection;
  onChange: (next: TimeRangeSelection) => void;
  beyondRetention?: boolean;
  children?: ReactNode;
}) {
  const { t } = useTranslation("deployments");
  return (
    <div className="mb-3">
      <div className={children ? "deployment-metric-toolbar" : undefined}>
        {children}
        <TimeRangePicker deploymentDetail size="small" value={selection} onChange={(next) => next && onChange(next)}
          className={children ? "contents" : undefined} />
      </div>
      {beyondRetention && (
        <Notice className="mt-2" tone="warning" title={t("timeRange.beyondRetention", { days: RETENTION_WARNING_DAYS })} />
      )}
    </div>
  );
}
