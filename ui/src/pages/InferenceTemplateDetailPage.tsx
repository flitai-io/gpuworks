import { playgroundRoute } from "./playground/route";
import { Component, Suspense, lazy, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Alert, Button, Descriptions, Drawer, Empty, Input, Modal, Popconfirm, Segmented, Space, Spin, Tabs, Tag, Typography, message } from "antd";
import type { TextAreaRef } from "antd/es/input/TextArea";
import { ArrowLeftOutlined, CloseCircleOutlined, CopyOutlined, DeleteOutlined, EditOutlined, RocketOutlined, SaveOutlined, UndoOutlined } from "@ant-design/icons";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";

import { api, type EvaluateResponse, type InferenceSpec, type InferenceTemplate, type InferenceTemplateIssue } from "../api";
import TemplateWarnings from "../components/TemplateWarnings";
import PageHeader from "../components/PageHeader";
import MarkdownContent from "../components/MarkdownContent";
import VendorLogo from "../components/VendorLogo";
import { compareVendors } from "../vendors";
import { compactModelMetadata, editErrorOf, predicateLabel, specDisplayName, yamlModelID } from "./inferenceTemplateFlat.ts";
import { engineOptionLabel } from "../engineLabels";
import { inputModalityLabelKey, inputModalitiesOf, operationLabelKey, operationTypesOf } from "../modelInference";

// CodeMirror needs requestAnimationFrame; environments without it (test
// runners, restricted embedders) keep the plain textarea implementation.
const codeMirrorSupported = typeof window !== "undefined" && typeof window.requestAnimationFrame === "function";
const LazyYamlEditor = lazy(() => import("./yamlEditor"));

let editorSchemaPromise: Promise<unknown> | undefined;
function loadInferenceTemplateSchema(): Promise<unknown> {
  editorSchemaPromise ??= api.inferenceTemplateSchema().catch(() => undefined);
  return editorSchemaPromise;
}

class EditorBoundary extends Component<{ children: ReactNode; fallback: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  render() { return this.state.failed ? this.props.fallback : this.props.children; }
}

const GIB = 1024 ** 3;

interface EditError { message: string; line?: number }

export default function InferenceTemplateDetailPage() {
  const { t, i18n } = useTranslation("inferenceTemplates");
  const navigate = useNavigate();
  const params = useParams();
  const [search, setSearch] = useSearchParams();
  // The list this detail was opened from, with its filters (?from=): the
  // breadcrumb, the back button and leaving after a delete all return to it.
  const fromList = search.get("from");
  const listURL = `/inference-templates${fromList ? `?${fromList}` : ""}`;
  const modelID = params["*"] || "";
  const [model, setModel] = useState<InferenceTemplate>();
  // The route model_id `model` was fetched for. Clone navigates in place, so
  // right after it `model` is still the source template; its YAML must not be
  // loaded into the copy's editor.
  const [modelFor, setModelFor] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [engine, setEngine] = useState("");
  const [drawerSpec, setDrawerSpec] = useState<InferenceSpec>();
  const [preview, setPreview] = useState<EvaluateResponse>();
  const [previewLoading, setPreviewLoading] = useState(false);
  const previewRequest = useRef(0);
  const [editing, setEditing] = useState(search.get("edit") === "1");
  const [yaml, setYAML] = useState("");
  const [savedYAML, setSavedYAML] = useState("");
  const [editError, setEditError] = useState<EditError>();
  // The editor's text comes from the server (api.inferenceTemplateYAML), which
  // writes it from the types the import reads, so saving it unchanged is
  // lossless. It is fetched when the editor opens and again after every reload.
  const [yamlState, setYAMLState] = useState<"idle" | "loading" | "ready" | "failed">("idle");
  const [yamlFailure, setYAMLFailure] = useState("");
  const yamlRequest = useRef(0);
  const [saving, setSaving] = useState(false);
  const [saveWarnings, setSaveWarnings] = useState<InferenceTemplateIssue[]>([]);
  const [errorFocus, setErrorFocus] = useState<{ line: number; nonce: number }>();
  const [editorSchema, setEditorSchema] = useState<unknown>();

  const load = async () => {
    const requested = modelID;
    setLoading(true); setError("");
    try {
      const value = await api.inferenceTemplate(requested);
      setModel(value); setModelFor(requested);
      yamlRequest.current += 1;
      setYAML(""); setSavedYAML(""); setYAMLState("idle"); setEditError(undefined);
      const values = [...new Set(value.specs.map((spec) => spec.engine))];
      setEngine((current) => values.includes(current) ? current : (values.includes(value.detected_support?.preferred_engine || "") ? value.detected_support!.preferred_engine : values[0] || ""));
    } catch (reason) { setError((reason as Error).message); }
    finally { setLoading(false); }
  };
  useEffect(() => { void load(); }, [modelID]);
  const loadYAML = async (value: InferenceTemplate) => {
    const request = ++yamlRequest.current;
    setYAMLState("loading"); setYAMLFailure("");
    try {
      // An upstream template is edited through its user specs only.
      const text = await api.inferenceTemplateYAML(value.model_id, value.source === "upstream" ? "user" : undefined);
      if (request !== yamlRequest.current) return;
      setYAML(text); setSavedYAML(text); setYAMLState("ready");
    } catch (reason) {
      if (request !== yamlRequest.current) return;
      setYAMLFailure((reason as Error).message); setYAMLState("failed");
    }
  };
  useEffect(() => {
    if (editing && model && modelFor === modelID && yamlState === "idle") void loadYAML(model);
  }, [editing, model, modelFor, modelID, yamlState]);
  // React Router keeps this component mounted across model_id changes (clone
  // navigates in place), so the edit=1 initial state only fires on a real
  // mount. Re-sync from the URL whenever the model identity changes.
  useEffect(() => { setEditing(search.get("edit") === "1"); }, [modelID]);
  useEffect(() => {
    if (!editing || !codeMirrorSupported) return;
    void loadInferenceTemplateSchema().then((schema) => { if (schema) setEditorSchema(schema); });
  }, [editing]);

  const engines = useMemo(() => [...new Set(model?.specs.map((spec) => spec.engine) || [])].sort((left, right) =>
    Number(right === "vllm") - Number(left === "vllm") || left.localeCompare(right)), [model]);
  const specs = useMemo(() => model?.specs.filter((spec) => !engine || spec.engine === engine) || [], [model, engine]);
  const specGroups = useMemo(() => {
    const groups = new Map<string, InferenceSpec[]>();
    for (const spec of specs) {
      const vendor = spec.gpus.vendor.trim().toLowerCase();
      groups.set(vendor, [...(groups.get(vendor) || []), spec]);
    }
    return [...groups.entries()].sort(([left], [right]) => compareVendors(left, right));
  }, [specs]);

  const openSpec = async (spec: InferenceSpec) => {
    const request = ++previewRequest.current;
    setDrawerSpec(spec); setPreview(undefined); setPreviewLoading(true);
    try {
      if (!model) return;
      // The server applies the template predicate, unsupported rules and
      // placement constraints before choosing a preview node.
      const value = await api.evaluate({ template_id: model.id, template_spec_source: spec.source, template_spec_name: spec.name, placement_mode: "auto", engine: spec.engine, quick_params: {}, placement: {} });
      if (request === previewRequest.current) setPreview(value);
    } catch (reason) { if (request === previewRequest.current) message.error((reason as Error).message); }
    finally { if (request === previewRequest.current) setPreviewLoading(false); }
  };

  const clone = async () => {
    if (!model) return;
    try {
      const copy = await api.cloneInferenceTemplate(model.model_id);
      const next = new URLSearchParams({ edit: "1" });
      if (search.get("from")) next.set("from", search.get("from")!);
      navigate(`/inference-templates/${copy.model_id}?${next}`);
    }
    catch (reason) { message.error(t("detail.cloneFailed", { detail: (reason as Error).message })); }
  };
  const syncEditParam = () => {
    const next = new URLSearchParams(search); next.delete("edit"); setSearch(next);
  };
  const closeEditor = () => {
    if (yaml === savedYAML) { setEditing(false); setEditError(undefined); syncEditParam(); return; }
    Modal.confirm({
      title: t("editor.discardTitle"),
      content: t("editor.discardBody"),
      okText: t("editor.discard"),
      okButtonProps: { danger: true },
      cancelText: t("editor.keepEditing"),
      onOk: () => { setEditing(false); setEditError(undefined); syncEditParam(); },
    });
  };
  const focusErrorLine = (line: number) => {
    setErrorFocus({ line, nonce: Date.now() });
  };
  const save = () => {
    // The import path saves with overwrite semantics: renaming model_id onto
    // an existing user template silently replaces its same-named specs, so a
    // rename always needs an explicit confirmation first.
    const target = yamlModelID(yaml);
    if (model && target && target !== model.model_id) {
      Modal.confirm({
        title: t("editor.modelChangedTitle"),
        content: t("editor.modelChangedBody", { from: model.model_id, to: target }),
        okText: t("editor.overwriteSave"),
        okButtonProps: { danger: true },
        cancelText: t("editor.keepEditing"),
        onOk: () => { void doSave(); },
      });
      return;
    }
    void doSave();
  };
  const doSave = async () => {
    setSaving(true);
    try {
      const target = yamlModelID(yaml);
      const result = await api.importInferenceTemplates(new File([yaml], "custom.yaml", { type: "application/yaml" }), true);
      setSaveWarnings(result?.warnings ?? []);
      message.success(t("flat.saved")); setEditing(false); setEditError(undefined);
      if (model && target && target !== model.model_id) {
        // the saved template lives under the new identity now
        navigate(`/inference-templates/${target.split("/").map(encodeURIComponent).join("/")}`);
        return;
      }
      syncEditParam();
      await load();
    } catch (reason) {
      setEditError(editErrorOf((reason as Error).message));
    } finally { setSaving(false); }
  };
  const resetEditor = () => { setYAML(savedYAML); setEditError(undefined); };
  const deleteSpec = async (spec: InferenceSpec) => {
    if (!model) return;
    try {
      const result = await api.deleteInferenceTemplateSpec(model.model_id, spec.name);
      message.success(t(result.model_deleted ? "editor.modelDeleted" : "editor.specDeleted", { name: result.model_deleted ? model.model_id : spec.name }));
      if (result.model_deleted) {
        navigate(listURL);
        return;
      }
      await load();
    } catch (reason) { message.error((reason as Error).message); }
  };

  if (loading) return <div className="template-detail-loading"><Spin size="large" /></div>;
  if (error) return <Alert type="error" showIcon message={t("detail.loadFailed", { detail: error })} action={<Button onClick={() => void load()}>{t("flat.retry")}</Button>} />;
  if (!model) return <Empty description={t("detail.notFound")} />;

  return <div className="resource-page inference-template-detail-page">
    <PageHeader title={model.display_title || model.model_id} description={compactModelMetadata(model)} parents={[<Link key="list" to={listURL}>{t("breadcrumb.templates")}</Link>]} extra={<Space wrap><Button icon={<ArrowLeftOutlined />} onClick={() => navigate(listURL)}>{t("detail.back")}</Button><Button icon={<CopyOutlined />} onClick={() => void clone()}>{t("detail.clone")}</Button>{model.specs.some((spec) => spec.source === "user") && <Button icon={<EditOutlined />} onClick={() => setEditing(true)}>{t("flat.edit")}</Button>}</Space>} />
    <TemplateWarnings warnings={saveWarnings} onClose={() => setSaveWarnings([])} />
    <Descriptions bordered size="small" column={{ xs: 1, md: 4 }}><Descriptions.Item label={t("detail.modelID")}><Typography.Text copyable>{model.model_id}</Typography.Text></Descriptions.Item><Descriptions.Item label={t("detail.source")}><Tag color={model.source === "user" ? "purple" : "blue"}>{t(model.source === "user" ? "flat.userSource" : "flat.builtInSource")}</Tag></Descriptions.Item><Descriptions.Item label={t("flat.context")}>{model.context_length || "-"}</Descriptions.Item><Descriptions.Item label={t("flat.specCount")}>{model.specs.length}</Descriptions.Item><Descriptions.Item label={t("detail.operations")} span={2}>{operationTypesOf(model.specs.flatMap((spec) => spec.api?.operations ?? [])).map((value) => <Tag color="blue" key={value}>{t(operationLabelKey[value])}</Tag>)}</Descriptions.Item><Descriptions.Item label={t("detail.inputModalities")} span={2}>{inputModalitiesOf(model.input_modalities).map((value) => <Tag key={value}>{t(inputModalityLabelKey[value])}</Tag>)}</Descriptions.Item></Descriptions>
    {engines.length >= 2 && <Segmented className="template-engine-switch" value={engine} options={engines.map((value) => ({ value, label: engineOptionLabel(value) }))} onChange={(value) => setEngine(String(value))} />}
    {model.detected_support?.warnings.map((item, index) => <Alert key={`${item.reason}:${index}`} type="warning" showIcon message={item.reason} description={item.alternative_available ? t("flat.alternativeAvailable") : undefined} />)}
    <div className="template-spec-groups">{specGroups.map(([vendor, vendorSpecs]) => <section className="template-spec-vendor-group" key={vendor}>
      <header><VendorLogo vendor={vendor} size={22} /><Typography.Text strong>{t("detail.vendorGroup", { vendor: t(`vendor.${vendor}`, { defaultValue: vendor.toUpperCase() }), count: vendorSpecs.length })}</Typography.Text></header>
      <div className="template-spec-list">{vendorSpecs.map((spec) => <article className="template-detail-table-row" key={`${spec.source}:${spec.name}`}><div><Typography.Text strong>{specDisplayName(spec, i18n.language)}</Typography.Text><Space wrap><Tag color={spec.source === "user" ? "purple" : "blue"}>{t(spec.source === "user" ? "flat.userSource" : "flat.builtInSource")}</Tag><Tag>{spec.scenario || t("scenario.general")}</Tag><Tag color="geekblue">{spec.quantization}</Tag><Typography.Text type="secondary">{spec.weight_bytes ? `${(spec.weight_bytes / GIB).toFixed(1)} GiB` : t("detail.sizePending")}</Typography.Text></Space></div><Typography.Text type="secondary" code>{spec.name}</Typography.Text><Typography.Text ellipsis={{ tooltip: spec.image }}>{spec.image}</Typography.Text><Space><Button onClick={() => void openSpec(spec)}>{t("flat.details")}</Button><Button type="primary" icon={<RocketOutlined />} onClick={() => navigate(`/deploy?template=${model.id}&spec_source=${encodeURIComponent(spec.source)}&spec=${encodeURIComponent(spec.name)}`)}>{t("detail.deploy")}</Button>{spec.source === "user" && <Popconfirm title={t("editor.deleteSpecTitle")} description={t("editor.deleteSpecBody", { name: spec.name })} okText={t("editor.deleteSpecOk")} okButtonProps={{ danger: true }} cancelText={t("flat.cancel")} onConfirm={() => void deleteSpec(spec)}><Button danger icon={<DeleteOutlined />} aria-label={t("editor.deleteSpec", { name: spec.name })} /></Popconfirm>}</Space></article>)}</div>
    </section>)}{!specs.length && <Empty description={t("flat.emptySpecs")} />}</div>
    <Drawer open={Boolean(drawerSpec)} width={760} title={drawerSpec ? specDisplayName(drawerSpec, i18n.language) : undefined} onClose={() => { previewRequest.current += 1; setDrawerSpec(undefined); }}>{drawerSpec && <Tabs items={[
      { key: "details", label: t("detail.configuration"), children: <Space direction="vertical" size="middle" style={{ width: "100%" }}><Descriptions bordered size="small" column={1}><Descriptions.Item label={t("flat.specID")}><Typography.Text copyable>{drawerSpec.name}</Typography.Text></Descriptions.Item><Descriptions.Item label={t("flat.gpus")}><Typography.Text code>{predicateLabel(drawerSpec)}</Typography.Text></Descriptions.Item>{drawerSpec.image_sources?.length ? <Descriptions.Item label={t("flat.imageSources")}><Space direction="vertical" size={4} style={{ width: "100%" }}>{drawerSpec.image_sources.map((source) => <Typography.Text key={source} copyable>{source}</Typography.Text>)}</Space></Descriptions.Item> : null}<Descriptions.Item label={t("weightSources.title")}><Space direction="vertical" size={6} style={{ width: "100%" }}>{drawerSpec.weight_sources.map((source, index) => <Space key={`${source.provider}:${source.repo_id}:${index}`} wrap><Tag color={source.provider.toLowerCase() === "modelscope" ? "green" : "blue"}>{t(`weightSources.${source.provider.toLowerCase()}`, { defaultValue: source.provider })}</Tag><Typography.Text copyable>{source.repo_id}</Typography.Text>{source.revision && <Typography.Text type="secondary">@{source.revision}</Typography.Text>}{source.subfolder && <Typography.Text type="secondary">/{source.subfolder}</Typography.Text>}</Space>)}</Space></Descriptions.Item><Descriptions.Item label="env"><pre>{JSON.stringify(drawerSpec.env || {}, null, 2)}</pre></Descriptions.Item><Descriptions.Item label="pre_start"><pre>{drawerSpec.pre_start_script || JSON.stringify(drawerSpec.pre_start_argv || [], null, 2)}</pre></Descriptions.Item></Descriptions><Typography.Title level={5}>{t("flat.commandPreview")}</Typography.Title>{previewLoading ? <Spin /> : preview ? <pre className="template-command-preview">{JSON.stringify(preview.command || [...(preview.entrypoint || []), ...(preview.argv || [])], null, 2)}</pre> : null}</Space> },
      { key: "usage", label: t("detail.usage"), children: <SpecUsage spec={drawerSpec} templateID={model.id} navigate={navigate} t={t} /> },
      { key: "readme", label: t("detail.instructions"), children: (i18n.language.startsWith("zh") ? model.readme_zh_cn_content : model.readme_en_content) ? <MarkdownContent content={(i18n.language.startsWith("zh") ? model.readme_zh_cn_content : model.readme_en_content) || ""} /> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("detail.noInstructions")} /> },
    ]} />}</Drawer>
    <Drawer open={editing} width={860} title={t("flat.editYAML")} onClose={closeEditor} styles={{ body: { display: "flex", flexDirection: "column", paddingBottom: 16 } }} extra={<Space>
      <Button icon={<UndoOutlined />} disabled={yamlState !== "ready" || yaml === savedYAML} onClick={resetEditor}>{t("editor.reset")}</Button>
      <Button type="primary" icon={<SaveOutlined />} loading={saving} disabled={yamlState !== "ready"} onClick={save}>{t("flat.save")}</Button>
    </Space>}>
      {yamlState === "loading" && <div className="template-detail-loading"><Spin /></div>}
      {yamlState === "failed" && <Alert type="error" showIcon message={t("editor.loadFailed", { detail: yamlFailure })} action={<Button onClick={() => setYAMLState("idle")}>{t("flat.retry")}</Button>} />}
      {yamlState === "ready" && <EditorBoundary fallback={<EditorTextArea ariaLabel={t("flat.editYAML")} value={yaml} onChange={setYAML} errorFocus={errorFocus} />}>
        <Suspense fallback={<EditorTextArea ariaLabel={t("flat.editYAML")} value={yaml} onChange={setYAML} />}>
          {codeMirrorSupported
            ? <LazyYamlEditor value={yaml} onChange={setYAML} schema={editorSchema} ariaLabel={t("flat.editYAML")} errorFocus={errorFocus} />
            : <EditorTextArea ariaLabel={t("flat.editYAML")} value={yaml} onChange={setYAML} errorFocus={errorFocus} />}
        </Suspense>
      </EditorBoundary>}
      {editError && <div className="template-yaml-error" role="alert">
        <div className="template-yaml-error-title"><CloseCircleOutlined />{t("editor.saveFailed")}</div>
        <div className="template-yaml-error-line">
          {editError.line ? <button type="button" className="template-yaml-error-jump" onClick={() => focusErrorLine(editError.line!)}>{t("editor.line", { line: editError.line })}</button> : null}
          <span className="template-yaml-error-text">{editError.message}</span>
        </div>
      </div>}
    </Drawer>
  </div>;
}

// Plain-textarea editor: the always-available implementation, also serving as
// the Suspense placeholder and the ErrorBoundary fallback for the CodeMirror
// path. Behavior parity matters - Tab indenting and error-line focus are part
// of the quick-win editor contract.
function EditorTextArea({ ariaLabel, value, onChange, errorFocus }: { ariaLabel: string; value: string; onChange: (value: string) => void; errorFocus?: { line: number; nonce: number } }) {
  const editorRef = useRef<TextAreaRef>(null);
  useEffect(() => {
    if (!errorFocus) return;
    const element = editorRef.current?.resizableTextArea?.textArea;
    if (!element) return;
    const lines = element.value.split("\n");
    let start = 0;
    for (let index = 0; index < Math.min(errorFocus.line - 1, lines.length); index++) start += lines[index].length + 1;
    const target = lines[errorFocus.line - 1] ?? "";
    element.focus();
    element.setSelectionRange(start, Math.min(start + target.length, element.value.length));
    const lineHeight = Number.parseFloat(getComputedStyle(element).lineHeight) || 20;
    element.scrollTop = Math.max(0, (errorFocus.line - 3) * lineHeight);
  }, [errorFocus]);
  return <Input.TextArea
    ref={editorRef}
    className="template-yaml-editor"
    aria-label={ariaLabel}
    value={value}
    onChange={(event) => { onChange(event.target.value); }}
    onKeyDown={(event) => {
      if (event.key !== "Tab" || event.shiftKey || event.ctrlKey || event.metaKey || event.altKey) return;
      event.preventDefault();
      const element = event.currentTarget;
      const start = element.selectionStart;
      const end = element.selectionEnd;
      onChange(`${element.value.slice(0, start)}  ${element.value.slice(end)}`);
      const restoreCaret = () => element.setSelectionRange(start + 2, start + 2);
      if (typeof requestAnimationFrame === "function") requestAnimationFrame(restoreCaret);
      else restoreCaret();
    }}
    spellCheck={false}
  />;
}

function SpecUsage({ spec, templateID, navigate, t }: { spec: InferenceSpec; templateID: number; navigate: ReturnType<typeof useNavigate>; t: ReturnType<typeof useTranslation>["t"] }) {  const operations = spec.api?.operations || [];
  if (!operations.length) return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("detail.noOperations")} />;
  return <Space direction="vertical" size="large" style={{ width: "100%" }}>{operations.map((operation) => {
    const route = playgroundRoute(operation, undefined, { template_id: String(templateID), spec_source: spec.source, spec: spec.name });
    return <section key={operation.type} className="template-operation-usage"><Space wrap><Typography.Title level={5}>{t(operationLabelKey[operation.type], { defaultValue: operation.type })}</Typography.Title><Tag>{operation.mode}</Tag><Tag color="blue">{operation.endpoint}</Tag><Button type="primary" onClick={() => navigate(route)}>{t("detail.openPlayground")}</Button></Space>{operation.example && <Tabs size="small" items={[{ key: "curl", label: "curl", children: <pre>{operation.example.curl}</pre> }, { key: "python", label: "Python", children: <pre>{operation.example.python}</pre> }]} />}</section>;
  })}</Space>;
}
