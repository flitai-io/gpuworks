import { Select, SelectContent, SelectItem, SelectTrigger } from "./ui/select";
import { DatePicker, Select as AntSelect } from "antd";
import type { Dayjs } from "dayjs";
import dayjs from "dayjs";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { QUICK_RANGES, TimeRangeSelection, type QuickRangeKey } from "../timeRange";

// A null value means "no explicit window": the live/recent first option that
// only the log viewer offers (metrics always have an explicit window).
export type TimeRangeValue = TimeRangeSelection | null;

interface Props {
  value: TimeRangeValue;
  onChange: (next: TimeRangeValue) => void;
  // When set, a first option with this label is offered and selecting it
  // emits null (e.g. 实时 for a running instance, 最近 for an exited one).
  liveLabel?: string;
  // The quick ranges to offer, in QUICK_RANGES order; all of them when unset.
  quickRanges?: readonly QuickRangeKey[];
  size?: "small";
  deploymentDetail?: boolean;
  className?: string;
}

const LIVE_OPTION = "live";
const CUSTOM_OPTION = "custom";

export default function TimeRangePicker({ value, onChange, liveLabel, quickRanges, size, deploymentDetail = false, className = "inline-flex items-center gap-2" }: Props) {
  const { t } = useTranslation("deployments");
  // Picking 自定义 only reveals the range picker; the selection is emitted
  // once the user confirms concrete dates there.
  const [draftingCustom, setDraftingCustom] = useState(false);
  const showCustomPicker = draftingCustom || value?.kind === "custom";
  const selectValue = value === null ? LIVE_OPTION : value.kind === "custom" ? CUSTOM_OPTION : value.key;

  const options = [
    ...(liveLabel ? [{ value: LIVE_OPTION, label: liveLabel }] : []),
    ...QUICK_RANGES.filter((range) => !quickRanges || quickRanges.includes(range.key)).map((range) => ({ value: range.key, label: t(range.labelKey) })),
    { value: CUSTOM_OPTION, label: t("timeRange.custom") },
  ];

  const handleSelect = (next: string) => {
    setDraftingCustom(next === CUSTOM_OPTION);
    if (next === LIVE_OPTION) {
      onChange(null);
      return;
    }
    if (next === CUSTOM_OPTION) {
      // Emitting nothing keeps the current window active until dates are
      // confirmed in the picker that this choice reveals.
      return;
    }
    onChange({ kind: "quick", key: next as QuickRangeKey });
  };

  return (
    <span className={className}>
      {deploymentDetail ? (
        <Select size={size === "small" ? "sm" : "md"} value={showCustomPicker ? CUSTOM_OPTION : selectValue} onValueChange={handleSelect}>
          <SelectTrigger className="min-w-0" wrapperClassName="time-range-select"
            style={{ width: liveLabel ? 110 : 130 }} aria-label={t("timeRange.ariaLabel")} />
          <SelectContent animated={false}>
            {options.map(item => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}
          </SelectContent>
        </Select>
      ) : (
        <AntSelect size={size} style={{ width: liveLabel ? 110 : 130 }} value={selectValue}
          options={options} onChange={handleSelect} aria-label={t("timeRange.ariaLabel")} />
      )}
      {showCustomPicker && (
        <DatePicker.RangePicker
          size={size}
          showTime
          value={value?.kind === "custom" ? [dayjs(value.startMS), dayjs(value.endMS)] : null}
          onOk={(dates: Array<Dayjs | null>) => {
            if (dates?.[0] && dates?.[1]) {
              setDraftingCustom(false);
              onChange({ kind: "custom", startMS: dates[0].valueOf(), endMS: dates[1].valueOf() });
            }
          }}
        />
      )}
    </span>
  );
}
