import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { DetectedTemplateChip, InferenceTemplateChip } from "../../api";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../../components/ui/dialog";
import { Button } from "../../components/ui/button";
import { Checkbox } from "../../components/ui/checkbox";
import { Input } from "../../components/ui/input";
import VendorLogo from "../../components/VendorLogo";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { useVendorDictionary } from "../../hooks/useVendorDictionary";
import { compareVendors, vendorFallbackName } from "../../vendors";
import { chipDisplayLabel, chipKey } from "../inferenceTemplateFlat";

export function normalizeCatalogChip(chip: InferenceTemplateChip): DetectedTemplateChip {
  return { vendor: chip.vendor, name: chip.name, cc: chip.cc == null ? undefined : String(chip.cc), arch_id: chip.arch_id };
}

export function TemplateChipPicker({ catalog, existing, detectedKeys, onClose, onApply }: {
  catalog: InferenceTemplateChip[]; existing: DetectedTemplateChip[]; detectedKeys: Set<string>; onClose: () => void; onApply: (chips: DetectedTemplateChip[]) => void;
}) {
  const { t } = useTranslation("inferenceTemplates");
  const chips = [...new Map([...catalog.map(normalizeCatalogChip), ...existing].map(chip => [chipKey(chip), chip])).values()];
  useVendorDictionary();
  const vendors = [...new Set(chips.map(chip => chip.vendor))].sort(compareVendors);
  // The brand shown is the one the operator picked, else the first in the
  // current order, which changes when the dictionary arrives.
  const [vendor, setVendor] = useState("");
  const activeVendor = vendors.includes(vendor) ? vendor : vendors[0] || "";
  const [query, setQuery] = useState("");
  // Keep unsaved edits separate from the live defaults: catalogs can finish loading
  // after this dialog opens, and saved chips can outlive their catalog entry.
  const [changes, setChanges] = useState<Map<string, boolean>>(new Map());
  const existingKeys = new Set(existing.map(chipKey).filter(key => !detectedKeys.has(key)));
  const isSelected = (key: string) => changes.get(key) ?? existingKeys.has(key);
  const setSelected = (key: string, checked: boolean) => setChanges(current => new Map(current).set(key, checked));
  const pending = chips.filter(chip => !detectedKeys.has(chipKey(chip)) && isSelected(chipKey(chip)));
  const changed = pending.length !== existingKeys.size || pending.some(chip => !existingKeys.has(chipKey(chip)));
  const needle = query.trim().toLowerCase();
  const shown = chips.filter(chip => needle ? `${chipDisplayLabel(chip)} ${t(`vendor.${chip.vendor}`, { defaultValue: vendorFallbackName(chip.vendor) })}`.toLowerCase().includes(needle) : chip.vendor === activeVendor);
  return <Dialog open onOpenChange={open => { if (!open) onClose(); }}>
    <DialogContent size="lg" className="w-[min(720px,calc(100vw-32px))] max-w-none">
      <DialogHeader><DialogTitle>{t("chips.edit")}</DialogTitle></DialogHeader>
      <Input aria-label={t("chips.search")} placeholder={t("chips.search")} value={query} onChange={event => setQuery(event.target.value)} />
      <div className="template-chip-picker">
        <nav aria-label={t("columns.vendor")}>{vendors.map(item => <Button key={item} variant="ghost" active={item === activeVendor} onClick={() => { setVendor(item); setQuery(""); }} aria-label={t(`vendor.${item}`, { defaultValue: vendorFallbackName(item) })}><VendorLogo vendor={item} size={18} /></Button>)}</nav>
        <div className="template-chip-options">{shown.map(chip => { const key = chipKey(chip); const locked = detectedKeys.has(key); return <label key={key}>
          <Checkbox checked={locked || isSelected(key)} disabled={locked} onCheckedChange={checked => setSelected(key, checked)} />
          <span>{chipDisplayLabel(chip)}</span>{locked && <small>{t("chips.detected")}</small>}
        </label>; })}{!shown.length && <p className="text-fg-muted">{t("flat.empty")}</p>}</div>
      </div>
      <div className="template-chip-selection">{pending.map(chip => <Button key={chipKey(chip)} size="sm" variant="tertiary" trailingIcon={ConsoleIcon.close} aria-label={t("chips.remove", { name: chipDisplayLabel(chip) })} onClick={() => setSelected(chipKey(chip), false)}>{chipDisplayLabel(chip)}</Button>)}</div>
      <DialogFooter><Button variant="ghost" onClick={onClose}>{t("flat.cancel")}</Button><Button disabled={!changed} onClick={() => onApply(pending)}>{t("chips.saveSelection")}</Button></DialogFooter>
    </DialogContent>
  </Dialog>;
}
