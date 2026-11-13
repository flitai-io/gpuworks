import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Combobox, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList, ComboboxTrigger } from "../components/ui/combobox";

interface Choice {
  id: string;
  text: string;
  content?: ReactNode;
}

export default function InstanceRecordPicker({ choices, selected, label, emptyText, popupClassName, onSelect }: {
  choices: Choice[]; selected?: string; label: string; emptyText?: string; popupClassName?: string; onSelect: (id: string) => void;
}) {
  const { t } = useTranslation("deployments");
  const current = choices.find(choice => choice.id === selected);
  return <Combobox disabled={choices.length === 0} items={choices} value={current ?? null} size="sm"
    isItemEqualToValue={(a, b) => a.id === b.id} itemToStringLabel={choice => choice.text}
    filter={(choice, query) => choice.text.toLowerCase().includes(query.trim().toLowerCase())}
    onValueChange={choice => choice && onSelect(choice.id)}>
    <ComboboxTrigger className="deployment-instance-picker min-w-0 h-auto min-h-control-sm px-[5px] gap-0.5" aria-label={label}>{current?.content ?? current?.text ?? emptyText}</ComboboxTrigger>
    <ComboboxContent className={popupClassName}>
      <ComboboxInput aria-label={label} placeholder={label} showTrigger={false} />
      <ComboboxEmpty>{t("detailLayout.noMatches")}</ComboboxEmpty>
      <ComboboxList>{(choice: Choice) => <ComboboxItem key={choice.id} value={choice} title={choice.text} aria-label={choice.text}>
        {choice.content ?? choice.text}
      </ComboboxItem>}</ComboboxList>
    </ComboboxContent>
  </Combobox>;
}
