import { useEffect, useMemo, useRef, useState } from "react";
import { Alert, AlertTitle, AlertAction } from "../components/ui/alert";
import { Button } from "../components/ui/button";
import { Tooltip } from "../components/ui/tooltip";
import { Accordion, AccordionItem, AccordionTrigger, AccordionContent } from "../components/ui/accordion";
import { Input } from "../components/ui/input";
import { Spinner } from "../ui-kit/Spinner";
import EmptyState from "../components/EmptyState";
import { message, confirm } from "../ui-kit/feedback";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { engineDisplayName } from "../engineLabels";
import { FilterChips } from "../ui-kit/FilterChips";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";

import { api, inferenceCatalogFileAccept, type DetectedTemplateChip, type InferenceTemplate, type InferenceTemplateChip, type InferenceTemplateIssue } from "../api";
import TemplateWarnings from "../components/TemplateWarnings";
import { fetchAllPages } from "../pagedList";
import PageHeader from "../components/PageHeader";
import { TemplateCatalogTable } from "./inferenceTemplates/TemplateCatalogTable";
import { TemplateHardwareFilters } from "./inferenceTemplates/TemplateHardwareFilters";
import { TemplateChipPicker } from "./inferenceTemplates/TemplateChipPicker";
import { chipDisplayLabel, chipKey } from "./inferenceTemplateFlat.ts";
import { operationLabelKey, operationTypes } from "../modelInference";
import type { OperationType } from "../inferenceApi";
import { modelOrganizationColor, modelOrganizationIcon, modelOrganizationKey, modelOrganizationName } from "./inferenceTemplatePresentation.ts";

const hypotheticalKey = "gpuworks.inferenceTemplates.hypotheticalChips";
const pageSize = 200;

export function loadHypotheticalChips(): DetectedTemplateChip[] {
  try {
    const value = JSON.parse(localStorage.getItem(hypotheticalKey) || "[]") as DetectedTemplateChip[];
    return Array.isArray(value) ? value.filter((item) => item?.vendor && item?.name) : [];
  } catch {
    return [];
  }
}

// The catalog is small enough to hold whole; it is read page by page all the
// same, the way every paged list is.
export function loadAllInferenceTemplates(fetchPage = api.inferenceTemplates, chip?: DetectedTemplateChip): Promise<InferenceTemplate[]> {
  const extra = chip ? {
    gpu: [chip.name], vendor: [chip.vendor],
    ...(chip.cc ? { cc: [chip.cc] } : {}),
    ...(chip.arch_id ? { arch_id: [chip.arch_id] } : {}),
  } : undefined;
  return fetchAllPages(fetchPage, { extra }, pageSize);
}

function initials(value: string) {
  const words = value.split(/[^a-zA-Z0-9]+/).filter(Boolean);
  return (words.length > 1 ? `${words[0][0]}${words[1][0]}` : words[0]?.slice(0, 2) || "AI").toUpperCase();
}

function OrganizationAvatar({ group, size }: { group: { key: string; organization: string; label: string }; size: number }) {
  const [failed, setFailed] = useState(false);
  const icon = group.key === "my-templates" ? undefined : modelOrganizationIcon(`${group.organization}/model`);
  if (icon && !failed) return <img className="template-organization-logo" width={size} height={size} src={icon} alt="" onError={() => setFailed(true)} />;
  return <span className="template-organization-fallback" data-color={modelOrganizationColor(group.key)} style={{ width: size, height: size, background: modelOrganizationColor(group.key) }}>{initials(group.label)}</span>;
}

interface OrganizationGroup { key: string; organization: string; label: string; items: InferenceTemplate[] }

export function compareModelsByReleaseDateDesc(a: InferenceTemplate, b: InferenceTemplate) {
  const dateOrder = (b.release_date || "").localeCompare(a.release_date || "");
  return dateOrder || a.model_id.localeCompare(b.model_id);
}

export default function InferenceTemplatesPage() {
  const { t } = useTranslation("inferenceTemplates");
  const [urlSearch, setURLSearch] = useSearchParams();
  const fileRef = useRef<HTMLInputElement>(null);
  const [models, setModels] = useState<InferenceTemplate[]>([]);
  const [chips, setChips] = useState<DetectedTemplateChip[]>([]);
  const [catalogChips, setCatalogChips] = useState<InferenceTemplateChip[]>([]);
  const [selectedChip, setSelectedChip] = useState(() => urlSearch.get("chip") || "");
  const [addingChip, setAddingChip] = useState(false);
  const [filtersOpen, setFiltersOpen] = useState(true);
  const [importWarnings, setImportWarnings] = useState<InferenceTemplateIssue[]>([]);
  const [detectedKeys, setDetectedKeys] = useState<Set<string>>(new Set());
  const [query, setQuery] = useState(() => urlSearch.get("query") || "");
  const [engine, setEngine] = useState(() => urlSearch.get("engine") || "");
  const [operation, setOperation] = useState<OperationType | "">(() => {
    const value = urlSearch.get("operation") as OperationType | null;
    return value && operationTypes.includes(value) ? value : "";
  });
  const [activeOrganization, setActiveOrganization] = useState(() => urlSearch.get("organization") || "");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const reload = async () => {
    setLoading(true);
    setError("");
    try {
      const [catalog, detected, available] = await Promise.all([loadAllInferenceTemplates(), api.detectedInferenceTemplateChips(), api.inferenceTemplateChips()]);
      setModels(catalog);
      setCatalogChips(available.items);
      setDetectedKeys(new Set(detected.items.map(chipKey)));
      const unique = new Map([...loadHypotheticalChips(), ...detected.items].map((chip) => [chipKey(chip), chip]));
      setChips([...unique.values()]);
    } catch (reason) {
      setError((reason as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { void reload(); }, []);
  const catalogQuery = useMemo(() => {
    const next = new URLSearchParams();
    if (activeOrganization) next.set("organization", activeOrganization);
    if (query) next.set("query", query);
    if (selectedChip) next.set("chip", selectedChip);
    if (engine) next.set("engine", engine);
    if (operation) next.set("operation", operation);
    return next.toString();
  }, [activeOrganization, query, selectedChip, engine, operation]);
  useEffect(() => {
    if (catalogQuery !== urlSearch.toString()) setURLSearch(catalogQuery, { replace: true });
  }, [catalogQuery, urlSearch, setURLSearch]);
  const activeChip = useMemo(() => chips.find((chip) => chipKey(chip) === selectedChip), [chips, selectedChip]);
  const [hardwareCatalog, setHardwareCatalog] = useState<{ key: string; models?: InferenceTemplate[]; error?: string }>();
  useEffect(() => {
    if (!activeChip) { setHardwareCatalog(undefined); return; }
    let active = true;
    const key = chipKey(activeChip);
    setHardwareCatalog({ key });
    void loadAllInferenceTemplates(api.inferenceTemplates, activeChip).then(
      (models) => { if (active) setHardwareCatalog({ key, models }); },
      (reason) => { if (active) setHardwareCatalog({ key, error: (reason as Error).message }); },
    );
    return () => { active = false; };
  }, [activeChip, models]);
  const currentHardwareCatalog = activeChip && hardwareCatalog?.key === chipKey(activeChip) ? hardwareCatalog : undefined;
  const catalogModels = activeChip ? currentHardwareCatalog?.models || [] : models;
  const catalogLoading = loading || Boolean(activeChip && !currentHardwareCatalog?.models && !currentHardwareCatalog?.error);
  const catalogError = error || currentHardwareCatalog?.error;
  const operationFacetModels = useMemo(() => catalogModels.filter((model) => {
    const needle = query.trim().toLowerCase();
    const inSearch = !needle || `${model.model_id} ${model.display_title || ""} ${model.specs.flatMap((spec) => spec.weight_sources.map((source) => source.repo_id)).join(" ")}`.toLowerCase().includes(needle);
    return inSearch && model.specs.some((spec) => !engine || spec.engine === engine);
  }), [catalogModels, query, engine]);
  const operationCounts = useMemo(() => Object.fromEntries(operationTypes.map((item) => [item, operationFacetModels.filter((model) => model.specs.some((spec) => (!engine || spec.engine === engine)
    && (spec.api?.operations || []).some((candidate) => candidate.type === item))).length])) as Record<OperationType, number>, [engine, operationFacetModels]);
  const visible = useMemo(() => operation ? operationFacetModels.filter((model) => model.specs.some((spec) => (!engine || spec.engine === engine)
    && (spec.api?.operations || []).some((candidate) => candidate.type === operation))) : operationFacetModels, [engine, operation, operationFacetModels]);
  const groups = useMemo<OrganizationGroup[]>(() => {
    const grouped = new Map<string, InferenceTemplate[]>();
    for (const model of visible) {
      const key = model.source === "user" ? "my-templates" : modelOrganizationKey(model.model_id);
      grouped.set(key, [...(grouped.get(key) || []), model]);
    }
    return [...grouped.entries()].map(([key, items]) => ({
      key,
      organization: key,
      label: key === "my-templates" ? t("flat.myTemplates") : modelOrganizationName(key, t),
      items: [...items].sort(compareModelsByReleaseDateDesc),
    })).sort((a, b) => a.key === "my-templates" ? -1 : b.key === "my-templates" ? 1 : a.label.localeCompare(b.label));
  }, [visible, t]);

  useEffect(() => {
    if (catalogLoading) return;
    if (!groups.some((group) => group.key === activeOrganization)) setActiveOrganization(groups[0]?.key || "");
  }, [groups, activeOrganization, catalogLoading]);

  const activeGroup = groups.find((group) => group.key === activeOrganization);
  const applyHypothetical = (selected: DetectedTemplateChip[]) => {
    const next = selected.filter(chip => !detectedKeys.has(chipKey(chip)));
    localStorage.setItem(hypotheticalKey, JSON.stringify(next));
    setChips(current => [...next, ...current.filter(chip => detectedKeys.has(chipKey(chip)))]);
    const retainedKeys = new Set([...detectedKeys, ...next.map(chipKey)]);
    setSelectedChip(current => retainedKeys.has(current) ? current : "");
    setAddingChip(false);
  };
  const importCatalog = async (file: File, overwrite = false) => {
    try {
      const result = await api.importInferenceTemplates(file, overwrite);
      setImportWarnings(result?.warnings ?? []);
      message.success(overwrite ? t("flat.overwritten") : t("flat.imported"));
      await reload();
    } catch (reason) {
      const errorMessage = (reason as Error).message;
      // Only a same-named user spec collision is worth an overwrite prompt;
      // parse and validation errors must surface as errors instead.
      if (!overwrite && /already exists/i.test(errorMessage)) {
        confirm({
          title: t("flat.overwriteTitle"),
          content: t("flat.overwritePrompt", { detail: errorMessage }),
          okText: t("flat.overwriteOk"),
          okButtonProps: { danger: true },
          cancelText: t("flat.cancel"),
          onOk: () => importCatalog(file, true),
        });
        return;
      }
      message.error(errorMessage);
    }
  };

  return <div className="resource-page inference-template-page inference-templates-page">
    <PageHeader title={t("page.title")} description={t("flat.description")} extra={<div className="flex flex-wrap items-center gap-2">
      <input ref={fileRef} aria-label={t("flat.importTemplates")} hidden type="file" accept={inferenceCatalogFileAccept} onChange={(event) => { const file = event.target.files?.[0]; if (file) void importCatalog(file); event.target.value = ""; }} />
      <Button variant="secondary" onClick={() => setAddingChip(true)}>{t("chips.edit")}</Button>
      <Tooltip content={t("flat.importHint")}><Button variant="primary" leadingIcon={ConsoleIcon.add} onClick={() => fileRef.current?.click()}>{t("flat.importTemplates")}</Button></Tooltip>
    </div>} />
    <TemplateWarnings warnings={importWarnings} onClose={() => setImportWarnings([])} />

    <Accordion className="template-filter-panel" value={filtersOpen ? "filters" : ""} onValueChange={(value: string) => setFiltersOpen(value === "filters")}>
      <AccordionItem value="filters" className="[&>div.pointer-events-none]:hidden">
        <div className="template-filter-heading">
          <div className="min-w-0 w-full">
            <AccordionTrigger className="px-3 py-2 [&>span]:min-w-0 [&>span:first-child]:pr-28">
              <span>{t("filters.title")}</span>
              <span className="ml-3 break-words text-label font-normal text-fg-muted">{[activeChip && chipDisplayLabel(activeChip), operation && t(operationLabelKey[operation]), engine && engineDisplayName(engine)].filter(Boolean).join(" · ")}</span>
            </AccordionTrigger>
          </div>
          <Button className="template-filter-clear z-action disabled:pointer-events-auto" variant="ghost" size="sm" disabled={!selectedChip && !operation && !engine}
            onClick={() => { setSelectedChip(""); setOperation(""); setEngine(""); }}>{t("filters.clear")}</Button>
        </div>
        <AccordionContent className="relative bg-surface-floating">
          <TemplateHardwareFilters chips={chips} detectedKeys={detectedKeys} value={selectedChip}
            onChange={key => setSelectedChip(current => current === key ? "" : key)} />
          <div className="template-filter-row template-task-filter">
            <span className="template-filter-label">{t("filters.modelType")}</span>
            <FilterChips label={t("filters.operationAria")} value={operation} onChange={(value) => setOperation((current) => current === value ? "" : value)}
              items={operationTypes.map((value) => ({ value, label: t(operationLabelKey[value]), count: operationCounts[value] }))} />
          </div>
          <div className="template-filter-row">
            <span className="template-filter-label">{t("columns.engine")}</span>
            <FilterChips label={t("columns.engine")} value={engine || "all"} onChange={(value) => setEngine(value === "all" || value === engine ? "" : value)}
              items={[{ value: "all", label: t("filters.all") }, ...["vllm", "sglang"].map(value => ({ value, label: engineDisplayName(value) }))]} />
          </div>
        </AccordionContent>
      </AccordionItem>
    </Accordion>

    <section className="template-catalog-card">
      <div className="template-filter-band">
        <Input className="template-search" aria-label={t("flat.search")} value={query} onChange={(event) => setQuery(event.target.value)} placeholder={t("flat.search")} />
        <span className="text-label text-fg-muted">{t("flat.modelCount", { count: visible.length })}</span>
      </div>
      {catalogLoading && <div className="template-loading"><Spinner label={t("page.loading")} /></div>}
      {!catalogLoading && catalogError && <Alert status="danger"><AlertTitle>{t("page.loadFailed", { detail: catalogError })}</AlertTitle><AlertAction><Button onClick={() => void reload()}>{t("flat.retry")}</Button></AlertAction></Alert>}
      {!catalogLoading && !catalogError && !groups.length && <EmptyState description={t("flat.empty")} />}
      {!catalogLoading && !catalogError && groups.length > 0 && <div className="template-organization-browser">
        <aside className="template-organization-sidebar"><header><strong>{t("organization.title")}</strong><span>{groups.length}</span></header><nav aria-label={t("organization.aria")}>
          {groups.map((group) => <button type="button" key={group.key} className={group.key === activeOrganization ? "is-active" : ""} onClick={() => setActiveOrganization(group.key)}>
            <OrganizationAvatar group={group} size={30} />
            <span><strong>{group.label}</strong><small>{t("models.count", { count: group.items.length })}</small></span><ConsoleIcon.chevronRight size={16} />
          </button>)}
        </nav></aside>
        <main className="template-organization-main">{activeGroup && <>
          <header className="template-organization-heading"><OrganizationAvatar key={activeGroup.key} group={activeGroup} size={42} /><div><h3>{activeGroup.label}</h3><span className="text-label text-fg-muted">{t("models.count", { count: activeGroup.items.length })}</span></div></header>
          <TemplateCatalogTable key={activeGroup.key} models={activeGroup.items} catalogQuery={catalogQuery} onRefresh={() => void reload()} />
        </>}</main>
      </div>}
    </section>
    {addingChip && <TemplateChipPicker catalog={catalogChips} existing={chips} detectedKeys={detectedKeys} onClose={() => setAddingChip(false)} onApply={applyHypothetical} />}

  </div>;
}
