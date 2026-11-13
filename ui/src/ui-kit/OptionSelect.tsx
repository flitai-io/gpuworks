import type { ComponentProps, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Select, SelectTrigger, SelectContent, SelectItem } from "../components/ui/select";
import { Button } from "../components/ui/button";
/** A labeled single choice; clearing is an explicit keyboard-accessible action. */
export function OptionSelect({ options, value, onChange, placeholder, disabled, clearable, ...trigger }: Omit<ComponentProps<typeof SelectTrigger>, "value" | "onChange" | "children"> & {
  options: { value: string; label: ReactNode }[]; value?: string; onChange: (value: string) => void;
  placeholder?: string; disabled?: boolean; clearable?: boolean;
}) {
  const { t } = useTranslation("common");
  return <div className="flex min-w-0 items-center gap-1">
    <Select value={value ?? ""} onValueChange={onChange} disabled={disabled}>
      <SelectTrigger {...trigger} placeholder={placeholder} wrapperClassName="min-w-0 flex-1" className={`min-w-0 w-full ${trigger.className ?? ""}`} />
      <SelectContent>{options.map(option => <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>)}</SelectContent>
    </Select>
    {clearable && value && <Button variant="ghost" size="sm" disabled={disabled} onClick={() => onChange("")}>{t("actions.remove")}</Button>}
  </div>;
}
