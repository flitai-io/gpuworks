import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { DetectedTemplateChip, InferenceTemplateChip } from "../../api";
import VendorLogo from "../../components/VendorLogo";
import { Button } from "../../components/ui/button";
import { Combobox, ComboboxChip, ComboboxChips, ComboboxChipsInput, ComboboxContent, ComboboxItem, ComboboxList, useComboboxAnchor } from "../../components/ui/combobox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger } from "../../components/ui/select";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { knownVendors } from "../../vendors";
import { chipDisplayLabel, chipKey } from "../inferenceTemplateFlat";

type Chip = DetectedTemplateChip;

export function normalizeCatalogChip(chip: InferenceTemplateChip): Chip {
  return { vendor: chip.vendor, name: chip.name, cc: chip.cc == null ? undefined : String(chip.cc), arch_id: chip.arch_id };
}

// The agent can report CC as "9.0" while the catalog sends 9. Keep stored
// filter keys stable, but show one option for the same physical identity.
function optionKey(chip: Chip) {
  const cc = chip.cc && Number.isFinite(Number(chip.cc)) ? String(Number(chip.cc)) : chip.cc;
  return chipKey({ ...chip, name: chip.name.toLowerCase(), cc });
}

function searchNames(chip: Chip) {
  return [chip.name, chipDisplayLabel(chip), chip.name.replace(/^(?:AMD Instinct|PPU|Hygon|Ascend|Kunlunxin)\s+/i, "")].map(name => name.trim().toLowerCase());
}

export function TemplateChipPicker({ catalog, existing, detectedKeys, onClose, onApply }: {
  catalog: InferenceTemplateChip[]; existing: Chip[]; detectedKeys: Set<string>; onClose: () => void; onApply: (chips: Chip[]) => void;
}) {
  const { t } = useTranslation("inferenceTemplates");
  const [vendor, setVendor] = useState<string>(knownVendors[0]);
  const [query, setQuery] = useState("");
  const [staged, setStaged] = useState<Chip[]>([]);
  const pending = staged.filter(chip => chip.vendor === vendor);
  function setPending(chips: Chip[]) {
    setStaged(current => [...current.filter(chip => chip.vendor !== vendor), ...chips]);
  }
  // Track edits against live props so a catalog or saved list arriving after opening is retained.
  const [edits, setEdits] = useState<Map<string, Chip | null>>(new Map());
  const saved = new Map(existing.filter(c => !detectedKeys.has(chipKey(c))).map(c => [chipKey(c), c]));
  const added = new Map(saved);
  for (const [key, chip] of edits) {
    if (chip && !detectedKeys.has(key)) added.set(key, chip);
    else added.delete(key);
  }
  const changed = added.size !== saved.size || [...added.keys()].some(key => !saved.has(key));
  const anchor = useComboboxAnchor();
  const detected = existing.filter(chip => detectedKeys.has(chipKey(chip)));
  const candidates = [...new Map([...catalog.map(normalizeCatalogChip), ...existing, ...added.values(), ...pending, ...detected]
    .filter(c => c.vendor === vendor).map(c => [optionKey(c), c])).values()];
  const needle = query.trim().toLowerCase();
  const exact = (chip: Chip) => searchNames(chip).includes(needle);
  const options = candidates.filter(c => searchNames(c).some(name => name.includes(needle)))
    .sort((a, b) => Number(exact(b)) - Number(exact(a)));
  if (needle && !options.some(exact)) options.unshift({ vendor, name: query.trim() });
  const visible = [...detected, ...added.values()];
  const groups = [...new Set(visible.map(chip => chip.vendor))];
  function updateAdded(chips: Chip[], add: boolean) {
    setEdits(current => {
      const next = new Map(current);
      for (const chip of chips) next.set(chipKey(chip), add ? chip : null);
      return next;
    });
  }
  function brandLabel(id: string) {
    return <span className="inline-flex items-center gap-2 align-middle"><span className="inline-flex items-center leading-none" aria-hidden="true"><VendorLogo vendor={id} size={16} /></span><span>{t(`vendor.${id}`, { defaultValue: id })}</span></span>;
  }
  return <Dialog open disablePointerDismissal onOpenChange={open => { if (!open) onClose(); }}>
    <DialogContent size="lg" className="template-chip-dialog w-[min(752px,calc(100vw-32px))] max-w-none">
      <DialogHeader><DialogTitle>{t("chips.edit")}</DialogTitle></DialogHeader>
      <section aria-label={t("chips.added")} className="flex min-h-0 flex-col rounded-lg border border-border-subtle bg-surface-base p-3">
        <div className="flex items-center justify-between gap-2"><strong>{t("chips.added")} ({visible.length})</strong>
          <Button variant="ghost" size="sm" disabled={!added.size} onClick={() => updateAdded([...added.values()], false)}>{t("chips.clear")}</Button></div>
        <div className="min-h-0 overflow-y-auto">{groups.map(id => <div key={id} role="group" aria-label={t(`vendor.${id}`, { defaultValue: id })} className="template-chip-vendor-row">
          <VendorLogo vendor={id} size={20} />
          <div className="flex min-w-0 flex-wrap items-center gap-2">{visible.filter(c => c.vendor === id).map(chip => detectedKeys.has(chipKey(chip)) ? (
            <span key={chipKey(chip)} className="inline-flex max-w-full items-center gap-1 rounded-md border border-border-subtle px-2 py-1">
              <span className="min-w-0 truncate" title={chip.name}>{chipDisplayLabel(chip)}</span><small className="shrink-0 text-fg-muted">{t("chips.detected")}</small>
            </span>
          ) : <Button key={chipKey(chip)} className="max-w-full" labelClassName="truncate" title={chip.name} size="sm" variant="tertiary" trailingIcon={ConsoleIcon.close}
            aria-label={t("chips.remove", { name: chipDisplayLabel(chip) })} onClick={() => updateAdded([chip], false)}>{chipDisplayLabel(chip)}</Button>)}</div>
        </div>)}</div>
        {!visible.length && <span className="text-fg-muted">{t("chips.empty")}</span>}
      </section>
      <div className="mt-5 grid grid-cols-[minmax(0,1fr)_auto] items-end gap-3 sm:grid-cols-[11rem_minmax(0,1fr)_auto]">
        <div className="col-span-2 min-w-0 sm:col-span-1"><label htmlFor="template-chip-vendor">{t("columns.vendor")}</label>
          <Select value={vendor} onValueChange={value => { setVendor(value); setQuery(""); }}>
            <SelectTrigger id="template-chip-vendor" className="w-full min-w-0" />
            <SelectContent>{knownVendors.map(id => <SelectItem key={id} value={id} textValue={t(`vendor.${id}`)}>{brandLabel(id)}</SelectItem>)}</SelectContent>
          </Select></div>
        <div className="min-w-0"><label htmlFor="template-chip-model">{t("chips.model")}</label>
          <Combobox<Chip, true> key={vendor} multiple modal={false} autoHighlight={Boolean(needle)} highlightItemOnHover={false} openOnInputClick items={options} value={pending} filter={null} inputValue={query}
            onInputValueChange={setQuery} itemToStringLabel={chipDisplayLabel} itemToStringValue={chipKey} isItemEqualToValue={(a, b) => chipKey(a) === chipKey(b)}
            onValueChange={chips => { setPending(chips); setQuery(""); }}>
            <ComboboxChips ref={anchor} className="w-full min-w-0">
              {pending.map(chip => <ComboboxChip key={chipKey(chip)} removeAriaLabel={t("chips.remove", { name: chipDisplayLabel(chip) })}>{chipDisplayLabel(chip)}</ComboboxChip>)}
              <ComboboxChipsInput id="template-chip-model" maxLength={128} placeholder={pending.length ? undefined : t("chips.placeholder")} />
            </ComboboxChips>
            <ComboboxContent anchor={anchor}><ComboboxList>{(chip: Chip) => {
              const locked = detectedKeys.has(chipKey(chip));
              return <ComboboxItem key={chipKey(chip)} value={chip} disabled={locked || added.has(chipKey(chip))}>
                {chipDisplayLabel(chip)}{locked && <small className="text-fg-muted">{t("chips.detected")}</small>}
              </ComboboxItem>;
            }}</ComboboxList></ComboboxContent>
          </Combobox></div>
        <Button variant="secondary" disabled={!pending.length} onClick={() => { updateAdded(pending, true); setPending([]); setQuery(""); }}>{t("chips.add")}</Button>
      </div>
      <DialogFooter><Button variant="ghost" onClick={onClose}>{t("flat.cancel")}</Button><Button disabled={!changed} onClick={() => onApply([...added.values()])}>{t("chips.saveSelection")}</Button></DialogFooter>
    </DialogContent>
  </Dialog>;
}
