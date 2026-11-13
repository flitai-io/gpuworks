import { useState } from "react";
import { Alert, Button, Card, Collapse, Descriptions, Form, Input, Modal, Select, message } from "antd";
import { useTranslation } from "react-i18next";
import {
  api,
  type LocalModelPreview,
  type Model,
  type URLModelPreview,
} from "../../api";
import { productMessageText } from "../../apiError";
import MarkdownContent from "../../components/MarkdownContent";
import { fetchHuggingFaceMetadata } from "../../modelmeta";
import { Modalities } from "../ModelsPageParts";
import { formatBytes } from "../../format";
import { useLocalStorageChoice } from "./useStorages";
import NodePicker from "../../nodes/NodePicker";
import { localRegistrationErrorText } from "./localRegistrationError";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";

// The two-step sources are the ones whose identity cannot be typed from
// memory: a node path and an HTTP directory are both read first and confirmed
// second, so the operator sees what will be registered before anything is
// written.
const previewSources = new Set(["local", "url"]);

export function RegisterModal({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const { t, i18n } = useTranslation("models");
  const [form] = Form.useForm();
  const [submitting, setSubmitting] = useState(false);
  const [previewing, setPreviewing] = useState(false);
  const [preview, setPreview] = useState<LocalModelPreview | null>(null);
  const [urlPreview, setURLPreview] = useState<URLModelPreview | null>(null);
  const source = Form.useWatch("source", form) as string | undefined;
  const directoryURL = Form.useWatch("source_url", form) as string | undefined;
  const outbound = useOutboundHostAccess([directoryURL ?? ""], open && source === "url");
  // Which local storage a registered path belongs to is only asked when there
  // is more than one; otherwise the request carries no storage_id, as before.
  const localStorages = useLocalStorageChoice(open && source === "local");
  const [storageId, setStorageId] = useState<number>();
  const effectiveStorageId = localStorages.choose ? storageId ?? localStorages.defaultId : undefined;
  const parsed = source === "url" ? urlPreview : preview;
  const reset = () => { form.resetFields(); setPreview(null); setURLPreview(null); setStorageId(undefined); };
  const previewLocal = async () => {
    const values = await form.validateFields(["node_uuid", "path"]);
    setPreviewing(true);
    try {
      const result = await api.previewLocalModel(values.node_uuid, values.path);
      setPreview(result);
      form.setFieldsValue({ source_id: result.source_id, name: result.name });
    } catch (e) {
      message.error(t("register.previewFailed", { detail: localRegistrationErrorText(t, e) }));
    } finally {
      setPreviewing(false);
    }
  };
  const previewURL = async () => {
    // A missing directory URL is already reported by the field itself; the
    // parse simply does not start, rather than raising past the button.
    let values: { source_url: string };
    try {
      values = await form.validateFields(["source_url"]);
    } catch {
      return;
    }
    setPreviewing(true);
    try {
      await outbound.authorize();
      const result = await api.previewURLModel(values.source_url);
      setURLPreview(result);
      form.setFieldsValue({ source_url: result.source_url, source_id: result.source_id, name: result.name });
    } catch (e) {
      message.error(t("register.previewFailed", { detail: (e as Error).message }));
    } finally {
      setPreviewing(false);
    }
  };
  const submit = async () => {
    // A rejected validation leaves the dialog open on the fields it marked; it is not a failed
    // registration, so it returns without a message and without a request.
    const v = await form.validateFields().catch(() => null);
    if (!v) return;
    if (v.source === "local" && !preview) { await previewLocal(); return; }
    if (v.source === "url" && !urlPreview) { await previewURL(); return; }
    setSubmitting(true);
    try {
      let registered: Model | undefined;
      if (v.source === "local") await api.registerLocalModel({ node_uuid: v.node_uuid, path: v.path, source_id: v.source_id, name: v.name, ...(effectiveStorageId ? { storage_id: effectiveStorageId } : {}) });
      else if (v.source === "url") {
        await outbound.authorize();
        registered = await api.registerURLModel({ source_url: v.source_url, source_id: v.source_id, name: v.name });
      }
      else {
        let metadata: Awaited<ReturnType<typeof fetchHuggingFaceMetadata>> | undefined;
        if (v.source === "huggingface") {
          // Only what the browser really read is sent as browser metadata; when
          // it reads nothing the server reads (and says so if it cannot).
          try {
            metadata = await fetchHuggingFaceMetadata(v.source_id, v.revision || "main");
          } catch {
            metadata = undefined;
          }
        }
        registered = await api.createModel({
          source: v.source,
          source_id: v.source_id,
          // Pin the commit the browser saw when no revision was asked for.
          revision: v.revision || metadata?.sha || "",
          name: v.name,
          readme: metadata?.readme,
          config_raw: metadata?.config_raw,
          weight_bytes: metadata?.weight_bytes,
          logo_data: metadata?.logo_data,
          ...(metadata ? { browser_metadata: true } : {}),
        });
      }
      message.success(t("register.registered"));
      // Registered without metadata: say why (the management node's rules
      // or reach), so the operator knows where to look.
      if (registered?.metadata_notice) message.warning(productMessageText((key, options) => i18n.t(key, { ns: "common", ...options }), registered.metadata_notice), 8);
      reset();
      onDone();
    } catch (e) {
      message.error(t("register.failed", { detail: v.source === "local" ? localRegistrationErrorText(t, e) : (e as Error).message }));
    } finally {
      setSubmitting(false);
    }
  };
  const okText = t(previewSources.has(source ?? "") && !parsed ? "register.parsePreview" : "register.confirm");
  return <Modal title={t("action.register")} open={open} onCancel={() => { reset(); onClose(); }} onOk={submit} confirmLoading={submitting || previewing} okText={okText} cancelText={t("action.cancel")} destroyOnHidden width={previewSources.has(source ?? "") ? 720 : 520}>
    <Form form={form} layout="vertical" initialValues={{ revision: "" }} onValuesChange={(changed) => {
      if ("node_uuid" in changed || "path" in changed) setPreview(null);
      if ("source_url" in changed) setURLPreview(null);
      if ("source" in changed) { setPreview(null); setURLPreview(null); }
    }}>
      <Form.Item label={t("column.source")} name="source" rules={[{ required: true, message: t("register.sourceRequired") }]}>
        <Select placeholder={t("register.chooseSource")} options={[
          { value: "huggingface", label: "Hugging Face" },
          { value: "modelscope", label: "ModelScope" },
          { value: "url", label: t("source.url") },
          { value: "local", label: t("register.localPath") },
        ]} />
      </Form.Item>
      {source === "local" ? <>
        {localStorages.choose && <Form.Item label={t("register.storage")}><Select aria-label={t("register.storage")} value={effectiveStorageId} onChange={setStorageId} options={localStorages.options.map((s) => ({ value: s.id, label: s.name }))} /></Form.Item>}
        <Form.Item label={t("detail.node")} name="node_uuid" rules={[{ required: true, whitespace: true, message: t("register.nodeRequired") }]}><NodePicker placeholder={t("register.selectOnlineNode")} aria-label={t("detail.node")} filter={{ state: ["online"] }} notFoundContent={t("download.noOnlineNodes")} /></Form.Item>
        <Form.Item label={t("register.modelDirectory")} name="path" rules={[{ required: true, whitespace: true, message: t("register.modelDirectoryRequired") }]} extra={t("register.modelDirectoryHelp")}><Input placeholder="/models/Qwen/Qwen3-8B" /></Form.Item>
      </> : source === "url" ? <>
        <Form.Item label={t("register.directoryURL")} name="source_url" rules={[{ required: true, whitespace: true, message: t("register.directoryURLRequired") }]} extra={t("register.directoryURLHelp")}>
          <Input placeholder="https://hf-mirror.example.com/Qwen/Qwen3-0.6B" onPressEnter={(event) => { event.preventDefault(); void previewURL(); }} />
        </Form.Item>
        <OutboundHostAccess access={outbound} />
        <div className="mb-6">
          <Button loading={previewing} onClick={() => { void previewURL(); }}>{t("register.parseDirectory")}</Button>
        </div>
      </> : <>
        <Form.Item label={t("column.sourceID")} name="source_id" rules={[{ required: true, whitespace: true }]}><Input placeholder="Qwen/Qwen3-8B" /></Form.Item>
        <Collapse size="small" ghost items={[{ key: "advanced", label: t("register.advanced"), children: <Form.Item label={t("detail.revision")} name="revision" extra={t("register.revisionHelp")}><Input placeholder={t("register.revisionPlaceholder")} /></Form.Item> }]} />
      </>}
      {source === "local" && preview && <Alert type="success" showIcon message={t("register.localParsed")} description={<Descriptions size="small" column={2}><Descriptions.Item label={t("register.path")} span={2}>{preview.path}</Descriptions.Item><Descriptions.Item label={t("detail.architecture")}>{preview.arch || t("unrecognized")}</Descriptions.Item><Descriptions.Item label={t("detail.precision")}>{preview.precision || t("unrecognized")}</Descriptions.Item><Descriptions.Item label={t("register.context")}>{preview.context_length?.toLocaleString() || t("unrecognized")}</Descriptions.Item><Descriptions.Item label={t("register.directorySize")}>{formatBytes(preview.weight_bytes)}</Descriptions.Item><Descriptions.Item label={t("column.inputModalities")} span={2}><Modalities values={preview.input_modalities} /></Descriptions.Item></Descriptions>} style={{ marginBottom: 16 }} />}
      {source === "url" && urlPreview && <Alert type="success" showIcon message={t("register.urlParsed")} description={<Descriptions size="small" column={2}><Descriptions.Item label={t("register.directoryURLLabel")} span={2}>{urlPreview.source_url}</Descriptions.Item><Descriptions.Item label={t("detail.architecture")}>{urlPreview.arch || t("unrecognized")}</Descriptions.Item><Descriptions.Item label={t("detail.precision")}>{urlPreview.precision || t("unrecognized")}</Descriptions.Item><Descriptions.Item label={t("register.context")}>{urlPreview.context_length?.toLocaleString() || t("unrecognized")}</Descriptions.Item><Descriptions.Item label={t("register.directorySize")}>{formatBytes(urlPreview.weight_bytes)}</Descriptions.Item><Descriptions.Item label={t("register.fileCount")} span={2}>{urlPreview.files_count}</Descriptions.Item><Descriptions.Item label={t("column.inputModalities")} span={2}><Modalities values={urlPreview.input_modalities} /></Descriptions.Item></Descriptions>} style={{ marginBottom: 16 }} />}
      {previewSources.has(source ?? "") && parsed && <>
        <Form.Item label={t("register.modelID")} name="source_id" rules={[{ required: true, whitespace: true }]}><Input onPressEnter={(event) => { event.preventDefault(); void submit(); }} /></Form.Item>
        {source === "local" && preview?.readme && <Card size="small" title={t("register.readmePreview", { truncated: preview.readme_truncated ? t("register.truncatedSuffix") : "" })} style={{ maxHeight: 220, overflow: "auto", marginBottom: 16 }}><MarkdownContent content={preview.readme} allowHtml /></Card>}
      </>}
      <Form.Item label={t("column.name")} name="name"><Input placeholder={t("register.displayNamePlaceholder")} /></Form.Item>
      {submitting && (source === "huggingface" || source === "modelscope") && (
        <Alert type="info" showIcon message={t("register.fetchingMetadata", { source: source === "huggingface" ? "Hugging Face" : "ModelScope" })} />
      )}
    </Form>
  </Modal>;
}
