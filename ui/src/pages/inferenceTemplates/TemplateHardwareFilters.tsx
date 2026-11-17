import { useTranslation } from "react-i18next";
import type { DetectedTemplateChip } from "../../api";
import VendorLogo from "../../components/VendorLogo";
import { FilterChips } from "../../ui-kit/FilterChips";
import { chipDisplayLabel, chipKey } from "../inferenceTemplateFlat";
import { compareVendors } from "../../vendors";

export function TemplateHardwareFilters({ chips, detectedKeys, value, onChange }: {
  chips: DetectedTemplateChip[]; detectedKeys: Set<string>; value: string; onChange: (value: string) => void;
}) {
  const { t } = useTranslation("inferenceTemplates");
  const vendors = [...new Set(chips.map(chip => chip.vendor))].sort(compareVendors);
  return <section className="template-hardware-filters template-filter-row">
    <span className="template-filter-label">{t("chips.mine")}</span>
    <div className="min-w-0">
    <div className="template-hardware-groups">{vendors.map(vendor => <div className="template-hardware-group" key={vendor}>
      <VendorLogo vendor={vendor} size={18} />
      <FilterChips label={t(`vendor.${vendor}`, { defaultValue: vendor })} value={value} onChange={onChange}
        items={chips.filter(chip => chip.vendor === vendor).map(chip => ({ value: chipKey(chip), label: <span>
          <i aria-hidden="true" className={`template-chip-dot${detectedKeys.has(chipKey(chip)) ? "" : " is-hypothetical"}`} />{chipDisplayLabel(chip)}
        </span> }))} />
    </div>)}</div>
    {!chips.length && <span className="text-label text-fg-muted">{t("chips.noOnlineNode")}</span>}
    </div>
  </section>;
}
