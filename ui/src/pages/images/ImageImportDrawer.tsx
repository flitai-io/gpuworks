import {
  Alert,
  Button,
  Col,
  Drawer,
  Form,
  Input,
  Row,
  Select,
  Switch,
} from "antd";
import { useCallback, useEffect, useRef, useState } from "react";
import type { FormInstance } from "antd/es/form";
import {
  api,
  type ImageArchivePreview,
  type ArchiveImageMetadata,
  type NodeImageSelection,
  type ImageCatalogEntry,
  type ImageEngineOption,
} from "../../api";
import { useForm } from "react-hook-form";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import ImageNodeArchiveFields, { type NodeArchive } from "./ImageNodeArchiveFields";
import RuntimeImagePicker from "./RuntimeImagePicker";
import ImageArchiveURLImport from "./ImageArchiveURLImport";
import ImageCommunityBrowser from "./ImageCommunityBrowser";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import { isArchiveURL } from "./ImagePageHelpers";
import { useTranslation } from "react-i18next";

type Props = {
  open: boolean;
  onClose: () => void;
  form: FormInstance;
  importing: boolean;
  importCustom: (source?: NodeArchive | NodeImageSelection) => Promise<void> | void;
  hasExplicitTag: (ref: string) => boolean;
  sourceTab: "community" | "custom";
  /** Catalog versions searched for a reference matching a pasted archive. */
  versions: ImageCatalogEntry[];
  /** Opens the matched version and records the archive URL on it. */
  onArchiveURLMatched: (version: ImageCatalogEntry, preview: ImageArchivePreview) => void;
};

/**
 * ImageImportDrawer adds one community or custom image. Any reference is
 * accepted and any engine of the server's image engine dictionary can be
 * declared, callable included; the system catalog is never edited here.
 */
export default function ImageImportDrawer({
  open,
  onClose,
  form,
  importing,
  importCustom,
  hasExplicitTag,
  sourceTab,
  versions,
  onArchiveURLMatched,
}: Props) {
  const { t } = useTranslation("images");
  // The engines an image may declare, their labels and their order are the
  // server's dictionary (GET /images/engines); the form adds nothing to it.
  const [engines, setEngines] = useState<ImageEngineOption[] | null>(null);
  const [enginesError, setEnginesError] = useState<string | null>(null);
  const [savePhase, setSavePhase] = useState<"idle" | "validating" | "authorizing">("idle");
  const authorizing = savePhase === "authorizing";
  const [previewing, setPreviewing] = useState(false);
  const [importMethod, setImportMethod] = useState("local");
  const loadNodeFile = open && sourceTab === "custom" && importMethod === "local";
  const fromRuntime = open && sourceTab === "custom" && importMethod === "runtime";
  const [selectedImage, setSelectedImage] = useState<NodeImageSelection>();
  const archive = useForm<NodeArchive>({ defaultValues: { node: "", path: "" } });
  const previous = useRef<{ fields: ArchiveImageMetadata & { image_ref: string }; url?: string } | null>(null);
  const clearPreview = useCallback(() => {
    if (!previous.current) return;
    const { fields, url } = previous.current;
    previous.current = null;
    for (const [key, value] of Object.entries(fields)) {
      if (form.getFieldValue(key) === value) form.setFieldValue(key, key === "arch" ? "multi" : undefined);
    }
    if (url) {
      const sources: string[] = form.getFieldValue("registry_sources") ?? [];
      form.setFieldValue("registry_sources", sources.filter(source => source !== url));
    }
  }, [form]);
  function clearRuntimeSelection() {
    setSelectedImage(undefined);
    clearPreview();
    form.setFieldsValue({ image_ref: undefined, engine: undefined, engine_version: undefined,
      framework: undefined, framework_version: undefined, arch: "multi" });
  }
  const prefill = (ref: string, metadata?: ArchiveImageMetadata, url?: string) => {
    clearPreview();
    const fields = { ...metadata, image_ref: ref };
    form.setFieldsValue(fields);
    const sources: string[] = form.getFieldValue("registry_sources") ?? [];
    const addedURL = url && !sources.includes(url) ? url : undefined;
    if (addedURL) form.setFieldValue("registry_sources", [...sources, addedURL]);
    previous.current = { fields, url: addedURL };
  };
  useEffect(() => {
    if (!open) {
      clearPreview();
      setImportMethod("local");
      setSelectedImage(undefined);
      archive.reset();
    }
  }, [open, archive, clearPreview]);
  const fields = Form.useWatch(values => ({
    sources: (values.registry_sources ?? []) as string[],
    hasMetadata: Boolean(values.engine && values.framework && values.framework_version?.trim()),
  }), { form, preserve: true });
  const runtimeReady = selectedImage && fields?.hasMetadata;
  const access = useOutboundHostAccess((fields?.sources ?? []).filter(isArchiveURL), open && !fromRuntime);
  const saving = useRef(false);
  const save = async () => {
    if (saving.current || importing || previewing || (fromRuntime && !runtimeReady)) return;
    saving.current = true;
    setSavePhase("validating");
    try {
      if (loadNodeFile && !(await archive.trigger())) return;
      await form.validateFields();
      setSavePhase("authorizing");
      const file = loadNodeFile ? archive.getValues() : undefined;
      if (!fromRuntime) await access.authorize();
      await importCustom(fromRuntime ? selectedImage : file);
    } catch { /* The form and permission control retain their own errors. */ }
    finally { saving.current = false; setSavePhase("idle"); }
  };
  useEffect(() => {
    if (!open || engines) return;
    let alive = true;
    setEnginesError(null);
    api
      .imageEngines()
      .then((options) => alive && setEngines(options.items))
      .catch((error: unknown) => {
        if (alive) setEnginesError(error instanceof Error ? error.message : String(error));
      });
    return () => {
      alive = false;
    };
  }, [open, engines]);
  const engineOptions = (engines ?? []).map((option) => ({ value: option.name, label: option.label }));
  return (
    <Drawer title={t(`import.title.${sourceTab}`)} open={open} onClose={authorizing || importing ? undefined : onClose} width={640}>
      <div style={{ display: "flex", flexDirection: "column", gap: 20, width: "100%" }}>
        {sourceTab === "community" && (
          <ImageCommunityBrowser
            onPick={(pick) => {
              form.setFieldsValue({
                image_ref: pick.imageRef,
                engine: pick.engine || undefined,
                framework: pick.framework || undefined,
                engine_version: pick.engineVersion,
                framework_version: pick.frameworkVersion,
                arch: pick.arch || "multi",
                rolling_tag: pick.rollingTag,
              });
            }}
          />
        )}
        {sourceTab === "custom" && (
          <div className="grid gap-2">
            <label htmlFor="image-import-method">{t("import.method")}</label>
            <OptionSelect
              id="image-import-method"
              aria-label={t("import.method")}
              value={importMethod}
              onChange={method => {
                if (method === "runtime" || fromRuntime) clearRuntimeSelection();
                else clearPreview();
                setImportMethod(method);
              }}
              disabled={importing || savePhase !== "idle"}
              options={[
                { value: "local", label: t("import.local.enable") },
                { value: "url", label: t("import.url.title") },
                { value: "runtime", label: t("import.runtime.title") },
              ]}
            />
          </div>
        )}
        <fieldset disabled={importing || savePhase !== "idle"}>
          {fromRuntime ? (
            <RuntimeImagePicker disabled={importing || savePhase !== "idle"}
              onClear={clearRuntimeSelection}
              onSelect={(selection, metadata) => { prefill(selection.ref, metadata); setSelectedImage(selection); }} />
          ) : loadNodeFile ? (
            <ImageNodeArchiveFields
              archive={archive}
              busy={previewing}
              setBusy={setPreviewing}
              onPick={prefill}
              onInvalidate={clearPreview}
            />
          ) : open && (
            <ImageArchiveURLImport
              setBusy={setPreviewing}
              showTitle={sourceTab === "community"}
              versions={versions}
              onMatched={onArchiveURLMatched}
              onInvalidate={clearPreview}
              onPrefill={preview => {
                const ref = preview.repo_tags[0];
                prefill(ref, preview.metadata_by_tag?.[ref], preview.url);
              }}
            />
          )}
        </fieldset>
        {!fromRuntime && <Alert
          type="info"
          showIcon
          message={t("import.customNotice")}
          description={t("import.customDescription")}
        />}
        <Form form={form} layout="vertical" initialValues={{ arch: "multi", rolling_tag: false }}>
          <Form.Item label={t("import.seriesName")} name="series_name">
            <Input placeholder={t("import.seriesNamePlaceholder")} />
          </Form.Item>
          <Form.Item
            label={t("import.imageRef")}
            name="image_ref"
            hidden={fromRuntime}
            rules={[
              { required: true, message: t("import.imageRefRequired") },
              {
                validator: (_, value?: string) =>
                  !value || hasExplicitTag(value.trim())
                    ? Promise.resolve()
                    : Promise.reject(new Error(t("import.tagRequired"))),
              },
            ]}
          >
            <Input placeholder="registry.example/team/image:v1" />
          </Form.Item>
          <Row gutter={12}>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.engine")}
                name="engine"
                rules={[{ required: true, message: t("import.engineRequired") }]}
              >
                <Select
                  allowClear
                  loading={engines === null && !enginesError}
                  options={engineOptions}
                  status={enginesError ? "error" : undefined}
                />
              </Form.Item>
              {enginesError && (
                <Alert
                  type="error"
                  showIcon
                  style={{ marginTop: -16, marginBottom: 16 }}
                  message={t("import.enginesFailed")}
                  description={enginesError}
                />
              )}
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("import.engineVersion")} name="engine_version">
                <Input placeholder={t("import.exampleEngineVersion")} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.framework")}
                name="framework"
                rules={[{ required: true, message: t("import.frameworkRequired") }]}
              >
                <Select
                  allowClear
                  options={[
                    { value: "cuda", label: "CUDA" },
                    { value: "cann", label: "CANN" },
                    { value: "rocm", label: "ROCm" },
                    { value: "hggc", label: "HGGC" },
                    { value: "dtk", label: "DTK" },
                    { value: "metal", label: "Metal" },
                    { value: "xre", label: "XRE" },
                  ]}
                />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.frameworkVersion")}
                name="framework_version"
                rules={[{ required: true, whitespace: true, message: t("import.frameworkVersionRequired") }]}
              >
                <Input placeholder={t("import.exampleFrameworkVersion")} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("import.hostArch")} name="arch">
                <Select
                  options={[
                    { value: "multi", label: "multi" },
                    { value: "amd64", label: "amd64" },
                    { value: "arm64", label: "arm64" },
                  ]}
                />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item
                label={t("import.rollingTag")}
                name="rolling_tag"
                valuePropName="checked"
                extra={t("import.rollingTagHelp")}
              >
                <Switch />
              </Form.Item>
            </Col>
          </Row>
          <Form.Item
            label={t("import.registrySources")}
            hidden={fromRuntime}
            name="registry_sources"
            extra={t("import.registrySourcesHelp")}
          >
            <Select mode="tags" tokenSeparators={[",", " "]} placeholder="https://containerimages.example/runner.tar" />
          </Form.Item>
          {!fromRuntime && <OutboundHostAccess access={access} />}
          <Button type="primary" disabled={previewing || (fromRuntime && !runtimeReady)} loading={importing || authorizing} onClick={() => void save()}>
            {t("import.addImage")}
          </Button>
        </Form>
      </div>
    </Drawer>
  );
}
