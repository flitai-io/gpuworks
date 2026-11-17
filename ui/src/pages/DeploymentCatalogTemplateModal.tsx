import { CopyOutlined, DownloadOutlined, ReloadOutlined, SaveOutlined } from "@ant-design/icons";
import { Alert, Button, Checkbox, Input, Modal, Space, Spin, Typography, message } from "antd";
import { Component, Suspense, lazy, useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { api, type CatalogTemplateExport, type ProductWarning } from "../api";
import { APIError, apiErrorText, productMessageText } from "../apiError";
import { CheckStatusBadge } from "../checkStatus";
import { useCopy } from "../components/CopyButton";
import { useIsAdmin } from "../components/useIsAdmin";
import { yamlModelID } from "./inferenceTemplateFlat";

// CodeMirror needs requestAnimationFrame; environments without it (test
// runners, restricted embedders) edit the YAML in a plain textarea.
const codeMirrorSupported = typeof window !== "undefined" && typeof window.requestAnimationFrame === "function";
const LazyYamlEditor = lazy(() => import("./yamlEditor"));

class EditorBoundary extends Component<{ children: ReactNode; fallback: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  render() { return this.state.failed ? this.props.fallback : this.props.children; }
}

const TEMPLATE_EXPORT_UNAVAILABLE = "TEMPLATE_EXPORT_UNAVAILABLE";

// DeploymentCatalogTemplateModal saves a running deployment as an inference
// template. The server tests the deployment's operations and writes the YAML
// from what the deployment ran; this dialog shows what was tested and what is
// missing, lets the user keep env vars the server left out, edit the YAML, and
// download it with its test record or save it into the catalog (an
// administrator's action, the same import the template editor uses).
export default function DeploymentCatalogTemplateModal({ deploymentID, onClose }: { deploymentID: number; onClose: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [copy, copyHolder] = useCopy();
  const [exported, setExported] = useState<CatalogTemplateExport>();
  const [yaml, setYAML] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [keepEnv, setKeepEnv] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [savedYAML, setSavedYAML] = useState<string>();
  const saved = savedYAML !== undefined && savedYAML === yaml;
  const generation = useRef(0);
  const controller = useRef<AbortController>(undefined);

  const exportErrorText = useCallback((reason: unknown): string => {
    if (reason instanceof APIError && reason.code === TEMPLATE_EXPORT_UNAVAILABLE) {
      const why = String(reason.params?.reason ?? "");
      return t(`catalogTemplate.reason.${why}`, { defaultValue: apiErrorText(tCommon, reason) });
    }
    return apiErrorText(tCommon, reason);
  }, [t, tCommon]);
  const errorText = useRef(exportErrorText);
  errorText.current = exportErrorText;

  const run = useCallback(async (includeEnv: string[]) => {
    const current = ++generation.current;
    controller.current?.abort();
    const request = new AbortController();
    controller.current = request;
    setLoading(true);
    setExported(undefined);
    setYAML("");
    setSaving(false);
    setError("");
    setSaveError("");
    try {
      const answer = await api.exportCatalogTemplate(deploymentID, includeEnv, request.signal);
      if (generation.current !== current) return;
      setExported(answer);
      setYAML(answer.yaml);
      setSavedYAML(undefined);
    } catch (reason) {
      if (generation.current !== current) return;
      setExported(undefined);
      setError(errorText.current(reason));
    } finally {
      if (generation.current === current) setLoading(false);
    }
  }, [deploymentID]);

  useEffect(() => {
    setKeepEnv([]);
    void run([]);
    return () => { generation.current++; controller.current?.abort(); };
  }, [run]);

  const download = () => {
    if (!exported) return;
    const blob = new Blob([yaml], { type: "application/yaml;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `${exported.model_id.replace(/[^a-z0-9._-]+/gi, "_")}.${exported.spec_name}.yaml`;
    a.click();
    URL.revokeObjectURL(url);
  };

  const save = async () => {
    const current = generation.current;
    const submittedYAML = yaml;
    setSaving(true);
    setSaveError("");
    try {
      await api.importInferenceTemplates(new File([submittedYAML], "catalog-template.yaml", { type: "application/yaml" }), false);
      if (generation.current !== current) return;
      setSavedYAML(submittedYAML);
      message.success(t("catalogTemplate.saved"));
    } catch (reason) {
      if (generation.current !== current) return;
      setSaveError(apiErrorText(tCommon, reason));
    } finally {
      if (generation.current === current) setSaving(false);
    }
  };

  const editor = (
    <EditorBoundary fallback={<YamlTextArea value={yaml} onChange={setYAML} ariaLabel={t("catalogTemplate.yaml")} />}>
      <Suspense fallback={<YamlTextArea value={yaml} onChange={setYAML} ariaLabel={t("catalogTemplate.yaml")} />}>
        {codeMirrorSupported
          ? <LazyYamlEditor value={yaml} onChange={setYAML} ariaLabel={t("catalogTemplate.yaml")} />
          : <YamlTextArea value={yaml} onChange={setYAML} ariaLabel={t("catalogTemplate.yaml")} />}
      </Suspense>
    </EditorBoundary>
  );

  const footer = !loading && exported ? [
    <Button key="download" icon={<DownloadOutlined />} onClick={download}>{t("catalogTemplate.download")}</Button>,
    <Button key="evidence" icon={<CopyOutlined />} onClick={() => void copy(exported.evidence, { success: t("catalogTemplate.evidenceCopied") })}>{t("catalogTemplate.copyEvidence")}</Button>,
    isAdmin && <Button key="save" type="primary" icon={<SaveOutlined />} loading={saving} disabled={!exported.saveable || saved} onClick={() => void save()}>{t("catalogTemplate.save")}</Button>,
  ].filter(Boolean) : [<Button key="close" onClick={onClose}>{t("catalogTemplate.close")}</Button>];

  return (
    <Modal open width={920} title={t("catalogTemplate.title")} onCancel={onClose} footer={footer} destroyOnHidden>
      {copyHolder}
      {loading && <div className="catalog-template-loading" role="status"><Spin /> <Typography.Text type="secondary">{t("catalogTemplate.testing")}</Typography.Text></div>}
      {!loading && error && <Alert type="error" showIcon message={t("catalogTemplate.failed")} description={error} action={<Button onClick={() => void run(keepEnv)}>{t("catalogTemplate.retry")}</Button>} />}
      {!loading && exported && <Space direction="vertical" size={12} style={{ width: "100%" }}>
        {exported.verified
          ? <Alert type="success" showIcon message={t("catalogTemplate.verified", { model: exported.model_id, spec: exported.spec_name })} />
          : <Alert type="warning" showIcon message={t("catalogTemplate.notVerified")} />}
        <section aria-label={t("catalogTemplate.checks")}>
          <Typography.Text strong>{t("catalogTemplate.checks")}</Typography.Text>
          <ul className="catalog-template-list">
            {exported.checks.length === 0 && <li>{t("catalogTemplate.noOperations")}</li>}
            {exported.checks.map((check) => <li key={check.operation}>
              <CheckStatusBadge status={check.status} />{" "}
              {productMessageText(tCommon, check)}
              {check.evidence && <>{" "}<Typography.Text code>{check.evidence}</Typography.Text></>}
            </li>)}
          </ul>
        </section>
        <section aria-label={t("catalogTemplate.notes")}>
          <Typography.Text strong>{t("catalogTemplate.notes")}</Typography.Text>
          <ul className="catalog-template-list">
            {exported.notes.filter((note) => !note.code.startsWith("operation_")).map((note, index) => <li key={`${note.code}-${index}`}>{noteText(t, note)}</li>)}
          </ul>
        </section>
        {(exported.excluded_env.length > 0 || keepEnv.length > 0) && <section aria-label={t("catalogTemplate.env")}>
          <Typography.Text strong>{t("catalogTemplate.env")}</Typography.Text>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 4 }}>{t("catalogTemplate.envHint")}</Typography.Paragraph>
          <Space wrap>
            <Checkbox.Group value={keepEnv} onChange={(values) => setKeepEnv(values as string[])}
              options={[...new Set([...keepEnv, ...exported.excluded_env])].sort().map((key) => ({ label: <code>{key}</code>, value: key }))} />
            <Button size="small" icon={<ReloadOutlined />} onClick={() => void run(keepEnv)}>{t("catalogTemplate.rebuild")}</Button>
          </Space>
        </section>}
        {editor}
        {!isAdmin && <Typography.Text type="secondary">{t("catalogTemplate.saveNeedsAdmin")}</Typography.Text>}
        {saveError && <Alert type="error" showIcon message={t("catalogTemplate.saveFailed")} description={saveError} />}
        {saved && <Alert type="success" showIcon message={<Link to={yamlModelID(savedYAML) === exported.model_id ? `/inference-templates/${exported.model_id.split("/").map(encodeURIComponent).join("/")}` : "/inference-templates"}>{t("catalogTemplate.openTemplate")}</Link>} />}
      </Space>}
    </Modal>
  );
}

// noteText renders a note the server sends by code, in the reader's language.
function noteText(t: (key: string, options?: Record<string, unknown>) => string, note: ProductWarning): string {
  const params = { ...note.params } as Record<string, unknown>;
  if (Array.isArray(params.keys)) params.keys = (params.keys as string[]).join(", ");
  return t(`catalogTemplate.note.${note.code}`, { ...params, defaultValue: note.code });
}

function YamlTextArea({ value, onChange, ariaLabel }: { value: string; onChange: (value: string) => void; ariaLabel: string }) {
  return <Input.TextArea aria-label={ariaLabel} value={value} onChange={(event) => onChange(event.target.value)} autoSize={{ minRows: 12, maxRows: 28 }} spellCheck={false} style={{ fontFamily: "monospace" }} />;
}
