import { useEffect, useMemo, useRef, useState } from "react";
import { Alert, Avatar, Button, Empty, Input, Modal, Select, Space, Spin, Tag, Tooltip, Typography, message } from "antd";
import { FilterChips } from "../ui-kit/FilterChips";
import { Segmented } from "../ui-kit/Segmented";
import { PlusOutlined, RightOutlined, UploadOutlined } from "@ant-design/icons";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";

import { api, inferenceCatalogFileAccept, type DetectedTemplateChip, type InferenceSpec, type InferenceTemplate, type InferenceTemplateChip, type InferenceTemplateIssue } from "../api";
import TemplateWarnings from "../components/TemplateWarnings";
import { fetchAllPages } from "../pagedList";
import PageHeader from "../components/PageHeader";
import VendorLogo from "../components/VendorLogo";
import { compareVendors } from "../vendors";
import { chipDisplayLabel, chipKey, compactModelMetadata } from "./inferenceTemplateFlat.ts";
import { inputModalityLabelKey, inputModalitiesOf, operationLabelKey, operationTypes } from "../modelInference";
import type { OperationType } from "../inferenceApi";
import { modelOrganizationIcon, modelOrganizationKey, modelOrganizationName } from "./inferenceTemplatePresentation.ts";

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

function avatarColor(value: string) {
  const colors = ["#6157e8", "#1677ff", "#08979c", "#d4380d", "#531dab", "#389e0d"];
  let hash = 0;
  for (const char of value) hash = (hash * 31 + char.charCodeAt(0)) >>> 0;
  return colors[hash % colors.length];
}

interface WeightArtifact { key: string; label: string; sources: string[] }

function weightArtifacts(specs: InferenceSpec[]): WeightArtifact[] {
  const artifacts = new Map<string, WeightArtifact>();
  for (const spec of specs) {
    const sources = spec.weight_sources.map((source) => `${source.provider}: ${source.repo_id}`);
    const key = `${spec.quantization || "unknown"}\u0000${sources.join("\u0000")}`;
    if (!artifacts.has(key)) {
      const primary = spec.weight_sources[0]?.repo_id || spec.name;
      artifacts.set(key, { key, label: `${(spec.quantization || "unknown").toUpperCase()} · ${primary}`, sources });
    }
  }
  return [...artifacts.values()];
}

function modelVendors(model: InferenceTemplate) {
  return [...new Set(model.specs.map((spec) => spec.gpus.vendor.trim().toLowerCase()).filter(Boolean))].sort(compareVendors);
}

// A published logo draws on its own: no tinted disc behind its transparent
// pixels and no crop to a circle. Only the initials fallback gets a colour.
function OrganizationAvatar({ group, size, shape = "circle" }: { group: { key: string; organization: string; label: string }; size: number; shape?: "circle" | "square" }) {
  const icon = group.key === "my-templates" ? undefined : modelOrganizationIcon(`${group.organization}/model`);
  if (icon) return <Avatar className="template-organization-logo" shape="square" size={size} src={icon} alt="" />;
  return <Avatar shape={shape} size={size} style={{ background: avatarColor(group.key) }}>{initials(group.label)}</Avatar>;
}

function ModelVendorMarks({ model }: { model: InferenceTemplate }) {
  const { t } = useTranslation("inferenceTemplates");
  const vendors = modelVendors(model);
  if (!vendors.length) return <Typography.Text type="secondary">—</Typography.Text>;
  return <div className="template-model-vendors">{vendors.map((vendor) => <Tooltip title={t(`vendor.${vendor}`, { defaultValue: vendor })} key={vendor}>
    <span className={`template-model-vendor is-${vendor}`}><VendorLogo vendor={vendor} size={18} /></span>
  </Tooltip>)}</div>;
}

function ModelCatalogRow({ model, openDetail }: { model: InferenceTemplate; openDetail: (model: InferenceTemplate) => void }) {
  const { t } = useTranslation("inferenceTemplates");
  const title = model.display_title || model.model_id.split("/").pop() || model.model_id;
  const artifacts = weightArtifacts(model.specs);
  const engines = [...new Set(model.specs.map((spec) => spec.engine))];
  return <article className="template-model-catalog-row">
    <div className="template-model-catalog-main">
      <button type="button" className="template-model-title" onClick={() => openDetail(model)}>{title}</button>
      {compactModelMetadata(model) && <Typography.Text type="secondary">{compactModelMetadata(model)}</Typography.Text>}
      <Typography.Text type="secondary" className="template-model-id">{model.model_id}</Typography.Text>
      {model.release_date && <Typography.Text type="secondary" className="template-model-release-date">{model.release_date}</Typography.Text>}
    </div>
    <div className="template-model-input-modalities">
      <Typography.Text type="secondary" className="template-model-column-label">{t("columns.inputModalities")}</Typography.Text>
      <Space size={[4, 4]} wrap>
        {inputModalitiesOf(model.input_modalities).map((value) => <Tag key={value}>{t(inputModalityLabelKey[value])}</Tag>)}
        {!inputModalitiesOf(model.input_modalities).length && <Typography.Text type="secondary">—</Typography.Text>}
      </Space>
    </div>
    <div className="template-model-variants">{artifacts.map((artifact) => <Tooltip title={artifact.sources.join("\n")} key={artifact.key}>
      <Tag>{artifact.label}</Tag>
    </Tooltip>)}</div>
    <ModelVendorMarks model={model} />
    <div className="template-model-engines">{engines.map((item) => <Tag key={item}>{item}</Tag>)}</div>
    <button type="button" className="template-model-open" aria-label={t("models.view", { title })} onClick={() => openDetail(model)}><RightOutlined /></button>
  </article>;
}

interface OrganizationGroup { key: string; organization: string; label: string; items: InferenceTemplate[] }

export function compareModelsByReleaseDateDesc(a: InferenceTemplate, b: InferenceTemplate) {
  const dateOrder = (b.release_date || "").localeCompare(a.release_date || "");
  return dateOrder || a.model_id.localeCompare(b.model_id);
}

export default function InferenceTemplatesPage() {
  const { t } = useTranslation("inferenceTemplates");
  const navigate = useNavigate();
  const [urlSearch, setURLSearch] = useSearchParams();
  const fileRef = useRef<HTMLInputElement>(null);
  const [models, setModels] = useState<InferenceTemplate[]>([]);
  const [chips, setChips] = useState<DetectedTemplateChip[]>([]);
  const [catalogChips, setCatalogChips] = useState<InferenceTemplateChip[]>([]);
  const [selectedChip, setSelectedChip] = useState(() => urlSearch.get("chip") || "");
  const [addingChip, setAddingChip] = useState(false);
  const [importWarnings, setImportWarnings] = useState<InferenceTemplateIssue[]>([]);
  const [candidateChip, setCandidateChip] = useState("");
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
      const unique = new Map([...detected.items, ...loadHypotheticalChips()].map((chip) => [chipKey(chip), chip]));
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
  const addHypothetical = () => {
    const selected = catalogChips.find((chip) => chipKey({ ...chip, cc: chip.cc == null ? undefined : String(chip.cc) }) === candidateChip);
    if (!selected) return;
    const chip = { vendor: selected.vendor, name: selected.name, cc: selected.cc == null ? undefined : String(selected.cc), arch_id: selected.arch_id };
    const next = [...loadHypotheticalChips().filter((item) => chipKey(item) !== chipKey(chip)), chip];
    localStorage.setItem(hypotheticalKey, JSON.stringify(next));
    setChips((current) => [...current.filter((item) => chipKey(item) !== chipKey(chip)), chip]);
    setSelectedChip(chipKey(chip));
    setAddingChip(false);
    setCandidateChip("");
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
        Modal.confirm({
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
  const openDetail = (model: InferenceTemplate) => {
    const detailSearch = new URLSearchParams();
    if (catalogQuery) detailSearch.set("from", catalogQuery);
    navigate(`/inference-templates/${model.model_id}${detailSearch.size ? `?${detailSearch}` : ""}`);
  };

  return <div className="resource-page inference-template-page inference-templates-page">
    <PageHeader title={t("page.title")} description={t("flat.description")} extra={<Space wrap>
      <input ref={fileRef} aria-label={t("flat.importTemplates")} hidden type="file" accept={inferenceCatalogFileAccept} onChange={(event) => { const file = event.target.files?.[0]; if (file) void importCatalog(file); event.target.value = ""; }} />
      <Tooltip title={t("flat.importHint")}><Button icon={<UploadOutlined />} onClick={() => fileRef.current?.click()}>{t("flat.importTemplates")}</Button></Tooltip>
      <Button icon={<PlusOutlined />} onClick={() => setAddingChip(true)}>{t("chips.add")}</Button>
    </Space>} />
    <TemplateWarnings warnings={importWarnings} onClose={() => setImportWarnings([])} />

    <section className="template-chip-band">
      <div className="template-chip-band-label"><Typography.Text strong>{t("chips.mine")}</Typography.Text><span>{t("chips.selectToFilter")}</span></div>
      <div className="template-chip-pills">
        {/* Same small filter buttons as the task row below; the dot says the chip is on a node. */}
        <FilterChips label={t("chips.mine")} value={selectedChip} onChange={(key) => setSelectedChip((current) => current === key ? "" : key)}
          items={chips.map((chip) => ({ value: chipKey(chip), label: <><i aria-hidden="true" className="template-chip-dot" />{chipDisplayLabel(chip)}</> }))} />
        {!chips.length && <Typography.Text type="secondary">{t("chips.noOnlineNode")}</Typography.Text>}
      </div>
    </section>

    <section className="template-catalog-card">
      <div className="template-task-filter">
        <FilterChips label={t("filters.operationAria")} value={operation} onChange={(value) => setOperation((current) => current === value ? "" : value)}
          items={operationTypes.map((value) => ({ value, label: t(operationLabelKey[value]), count: operationCounts[value] }))} />
      </div>
      <div className="template-filter-band">
        <Segmented label={t("columns.engine")} value={engine || "all"} onChange={(value) => setEngine(value === "all" ? "" : value)}
          options={[{ value: "all", label: t("filters.all") }, { value: "vllm", label: "vLLM" }, { value: "sglang", label: "SGLang" }]} />
        <Space wrap><Input.Search allowClear value={query} onChange={(event) => setQuery(event.target.value)} placeholder={t("flat.search")} /><Tag color="green">{t("flat.modelCount", { count: visible.length })}</Tag></Space>
      </div>
      {activeChip && <Alert className="template-compatibility-alert" type="info" showIcon message={t("flat.selectedChip", { chip: chipDisplayLabel(activeChip) })} />}
      {catalogLoading && <div className="template-loading"><Spin size="large" /></div>}
      {!catalogLoading && catalogError && <Alert type="error" showIcon message={t("page.loadFailed", { detail: catalogError })} action={<Button onClick={() => void reload()}>{t("flat.retry")}</Button>} />}
      {!catalogLoading && !catalogError && !groups.length && <Empty description={t("flat.empty")} />}
      {!catalogLoading && !catalogError && groups.length > 0 && <div className="template-organization-browser">
        <aside className="template-organization-sidebar"><header><Typography.Text strong>{t("organization.title")}</Typography.Text><span>{groups.length}</span></header><nav aria-label={t("organization.aria")}>
          {groups.map((group) => <button type="button" key={group.key} className={group.key === activeOrganization ? "is-active" : ""} onClick={() => setActiveOrganization(group.key)}>
            <OrganizationAvatar group={group} size={30} />
            <span><strong>{group.label}</strong><small>{t("models.count", { count: group.items.length })}</small></span><RightOutlined />
          </button>)}
        </nav></aside>
        <main className="template-organization-main">{activeGroup && <>
          <header className="template-organization-heading"><OrganizationAvatar group={activeGroup} size={54} shape="square" /><div><Typography.Title level={3}>{activeGroup.label}</Typography.Title><Typography.Text type="secondary">{activeGroup.organization} · {t("models.count", { count: activeGroup.items.length })}</Typography.Text></div></header>
          <div className="template-model-list-head"><span>{t("columns.model")}</span><span>{t("columns.inputModalities")}</span><span>{t("columns.weightVariants")}</span><span>{t("columns.vendor")}</span><span>{t("columns.engine")}</span><span /></div>
          <div className="template-model-list">{activeGroup.items.map((model) => <ModelCatalogRow key={model.model_id} model={model} openDetail={openDetail} />)}</div>
        </>}</main>
      </div>}
    </section>
    <Modal open={addingChip} title={t("flat.addHypothetical")} okText={t("flat.add")} onOk={addHypothetical} okButtonProps={{ disabled: !candidateChip }} onCancel={() => setAddingChip(false)}>
      <Select showSearch optionFilterProp="label" style={{ width: "100%" }} value={candidateChip || undefined} onChange={setCandidateChip} placeholder={t("flat.chooseChip")} options={catalogChips.map((chip) => { const normalized = { ...chip, cc: chip.cc == null ? undefined : String(chip.cc) }; return { value: chipKey(normalized), label: chipDisplayLabel(normalized) }; })} />
    </Modal>
  </div>;
}
