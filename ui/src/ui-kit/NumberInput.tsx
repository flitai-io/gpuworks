import { forwardRef, useEffect, useState } from "react";
import { Input, type InputProps } from "../components/ui/input";
export type NumberInputProps = Omit<InputProps, "value" | "onChange" | "type" | "min" | "max" | "step"> & {
  value: number | null; onChange: (value: number | null) => void;
  min?: number; max?: number; step?: number; precision?: number;
};
/** Bounds come from the caller's dictionary. Keep empty nullable and clamp only on blur. */
export const NumberInput = forwardRef<HTMLInputElement, NumberInputProps>(function NumberInput({ value, onChange, min, max, step = 1, precision, onBlur, ...props }, ref) {
  const [draft, setDraft] = useState(value == null ? "" : String(value));
  useEffect(() => { setDraft(value == null ? "" : String(value)); }, [value]);
  return <Input {...props} ref={ref} type="number" inputMode="decimal" min={min} max={max} step={step} value={draft}
    onChange={(event) => {
      const text = event.target.value;
      setDraft(text);
      onChange(text === "" || !Number.isFinite(Number(text)) ? null : Number(text));
    }} onBlur={(event) => {
      if (draft !== "" && Number.isFinite(Number(draft))) {
        let next = Number(draft);
        if (precision !== undefined) next = Number(next.toFixed(precision));
        if (min !== undefined) next = Math.max(min, next);
        if (max !== undefined) next = Math.min(max, next);
        setDraft(String(next));
        if (next !== value) onChange(next);
      }
      onBlur?.(event);
    }} />;
});
