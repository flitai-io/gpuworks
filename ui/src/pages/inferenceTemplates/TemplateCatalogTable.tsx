import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { InferenceSpec, InferenceTemplate } from "../../api";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import VendorLogo from "../../components/VendorLogo";
import { engineDisplayName } from "../../engineLabels";
import { Badge } from "../../components/ui/badge";
import { Tooltip } from "../../components/ui/tooltip";
import { compactModelMetadata } from "../inferenceTemplateFlat";
import { inputModalitiesOf, inputModalityLabelKey } from "../../modelInference";
import { useVendorDictionary } from "../../hooks/useVendorDictionary";
import { compareVendors, vendorFallbackName } from "../../vendors";

// One spelling per value, shared by a column's filter options, its filter test
// and its cell, so what a cell shows is always something its filter can pick.
export function specVendor(spec: InferenceSpec) {
  return spec.gpus.vendor.trim().toLowerCase();
}

export function specQuantization(spec: InferenceSpec) {
  return spec.quantization?.trim().toLowerCase() || "unknown";
}

function modelVendors(model: InferenceTemplate) {
  return [...new Set(model.specs.map(specVendor).filter(Boolean))].sort(compareVendors);
}

interface WeightArtifact { key: string; label: string; sources: string[] }

// Specs with the same quantization and the same download sources are one
// artifact; a different mirror keeps its own entry. The tooltip lists every
// source, the label names the first.
export function weightArtifacts(specs: InferenceSpec[]): WeightArtifact[] {
  const artifacts = new Map<string, WeightArtifact>();
  for (const spec of specs) {
    const sources = spec.weight_sources.map(source => `${source.provider}: ${source.repo_id}`);
    const key = `${specQuantization(spec)}\u0000${sources.join("\u0000")}`;
    if (!artifacts.has(key)) {
      const primary = spec.weight_sources[0]?.repo_id || spec.name;
      artifacts.set(key, { key, label: `${specQuantization(spec).toUpperCase()} · ${primary}`, sources });
    }
  }
  return [...artifacts.values()];
}

export function templateDetailPath(modelID: string, catalogQuery: string) {
  const search = new URLSearchParams();
  if (catalogQuery) search.set("from", catalogQuery);
  return `/inference-templates/${modelID}${search.size ? `?${search}` : ""}`;
}

export function TemplateCatalogTable({ models, catalogQuery, onRefresh }: {
  models: InferenceTemplate[]; catalogQuery: string; onRefresh: () => void;
}) {
  const { t } = useTranslation("inferenceTemplates");
  useVendorDictionary();
  const vendors = [...new Set(models.flatMap(modelVendors))].sort(compareVendors);
  const quantizations = [...new Set(models.flatMap(model => model.specs.map(specQuantization)))];
  const columns: ResourceColumn<InferenceTemplate>[] = [
    { key: "model", role: "name", title: t("columns.model"), width: 260, render: (_, model) => <>
      <Link className="text-fg-brand font-normal" to={templateDetailPath(model.model_id, catalogQuery)} title={model.display_title || model.model_id}>{model.display_title || model.model_id.split("/").pop()}</Link>
      <span className="resource-table-sub" title={model.model_id}>{compactModelMetadata(model) || model.model_id}</span>
    </> },
    { key: "vendor", title: t("columns.vendor"), width: 230,
      filters: vendors.map(value => ({ value, text: t(`vendor.${value}`, { defaultValue: vendorFallbackName(value) }) })),
      onFilter: (value, model) => model.specs.some(spec => specVendor(spec) === value),
      render: (_, model) => <div className="template-table-vendors">{modelVendors(model).map(vendor => <VendorLogo key={vendor} vendor={vendor} size={18} />)}</div> },
    { key: "input", title: t("columns.inputModalities"), width: 150, render: (_, model) => <div className="template-table-tags">{inputModalitiesOf(model.input_modalities).map(value => <Badge key={value} size="sm">{t(inputModalityLabelKey[value])}</Badge>)}</div> },
    { key: "weights", title: t("columns.weightVariants"), width: 300,
      filters: quantizations.map(value => ({ value, text: value.toUpperCase() })),
      onFilter: (value, model) => model.specs.some(spec => specQuantization(spec) === value),
      render: (_, model) => <div className="template-table-tags">{weightArtifacts(model.specs).map(artifact => {
        if (!artifact.sources.length) return <Badge key={artifact.key} size="sm" className="max-w-full truncate">{artifact.label}</Badge>;
        return <Tooltip key={artifact.key} content={<span className="whitespace-pre-line">{artifact.sources.join("\n")}</span>}>
          <Badge size="sm" tabIndex={0} className="max-w-full truncate">{artifact.label}</Badge>
        </Tooltip>;
      })}</div> },
    { key: "engine", title: t("columns.engine"), width: 150, render: (_, model) => <div className="template-table-tags">{[...new Set(model.specs.map(spec => spec.engine))].map(engine => <Badge key={engine} size="sm">{engineDisplayName(engine)}</Badge>)}</div> },
  ];
  return <ResourceTable tableId="inference-template-catalog" rowKey="model_id" columns={columns} data={models}
    onRefresh={onRefresh} pagination={false} emptyDescription={t("flat.empty")} filteredEmptyDescription={t("flat.empty")} />;
}
