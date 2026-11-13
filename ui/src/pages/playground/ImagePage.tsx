import UnsupportedModel from "./UnsupportedModel";
import RequestDrawer from "./RequestDrawer";
import { composeRequest, declaredValues } from "./compose";
import { Button, Empty, Input, Segmented, Select, Space, Typography, message } from "antd";
import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { invokeOperation } from "./api";
import { modelsForOperation, operationOf } from "./operations";
import type { PlaygroundCatalog } from "./types";
import ReferenceImages, { type ReferenceImage } from "./ReferenceImages";
import type { InferenceAPIOperation } from "../../inferenceApi";

export default function ImagePage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation(["playground", "common"]);
  const [search, setSearch] = useSearchParams();
  const generateModels = useMemo(() => modelsForOperation(catalog.models, "openai.image.generate"), [catalog.models]);
  const editModels = useMemo(() => modelsForOperation(catalog.models, "openai.image.edit"), [catalog.models]);
  const models = useMemo(() => [...generateModels, ...editModels.filter((model) => !generateModels.some((item) => item.id === model.id))], [editModels, generateModels]);
  const requested = search.get("model") || "";
  const modelID = requested || models[0]?.id || "";
  const selectedModel = models.find((model) => model.id === modelID);
  const generateOperation = operationOf(selectedModel, "openai.image.generate");
  const editOperation = operationOf(selectedModel, "openai.image.edit");
  const operation = search.get("operation") === "openai.image.edit" ? editOperation || generateOperation : generateOperation || editOperation;
  const editing = operation?.type === "openai.image.edit";
  const [prompt, setPrompt] = useState("A sunrise over mountains");
  const sizeChoice = declaredSizes(operation);
  const [size, setSize] = useState(sizeChoice.initial);
  const [references, setReferences] = useState<ReferenceImage[]>([]);
  // The server resolves how many references an edit takes; a server that
  // predates the field sends none, which is the single image it accepted.
  const referenceLimit = editing ? Math.max(1, editOperation?.max_reference_images || 1) : 0;
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<unknown>();

  // A model's declared canvases replace the generic list, and switching model
  // or operation starts again from that operation's declared default.
  useEffect(() => { setSize(sizeChoice.initial); }, [modelID, operation?.type, sizeChoice.initial]);
  useEffect(() => { setReferences((current) => current.length > referenceLimit ? current.slice(0, referenceLimit) : current); }, [referenceLimit]);
  useEffect(() => {
    if (!modelID || requested === modelID) return;
    const next = new URLSearchParams(search); next.set("model", modelID); setSearch(next, { replace: true });
  }, [modelID, requested, search, setSearch]);
  const submit = async () => {
    if (!operation || !modelID) return;
    if (editing && !references.length) {
      message.warning(t("image.inputRequired"));
      return;
    }
    setLoading(true); setResult(undefined);
    try {
      setResult(await invokeOperation(operation, modelID, {
        prompt,
        size,
        response_format: "b64_json",
        ...referenceInput(editing, references),
      }));
    }
    catch (error) { message.error((error as Error).message); }
    finally { setLoading(false); }
  };
  const chooseOperation = (value: string) => {
    const next = new URLSearchParams(search); next.set("operation", value); setSearch(next, { replace: true });
    setResult(undefined);
  };
  const imageURL = generatedImageURL(result);
  return <div className="playground-operation-page">
    <Space wrap className="playground-operation-toolbar"><Select className="playground-model-select" value={modelID || undefined} placeholder={t("image.modelPlaceholder")} options={models.map((model) => ({ value: model.id, label: model.label }))} onChange={(value) => { const next = new URLSearchParams(search); next.set("model", value); setSearch(next, { replace: true }); }} /><RequestDrawer operation={operation} model={modelID} body={operation ? composeRequest(operation, modelID, { prompt, ...referenceInput(editing, references) }, declaredValues(operation, { size })) : {}} onSend={(raw) => { void invokeOperation(operation!, modelID, JSON.parse(raw)).then(setResult); }} /><Select aria-label={t("image.size")} value={size} options={sizeChoice.options.map((value) => ({ value }))} onChange={setSize} />{generateOperation && editOperation && <Segmented value={operation?.type} options={[{ value: "openai.image.generate", label: t("common:operation.imageGenerate") }, { value: "openai.image.edit", label: t("common:operation.imageEdit") }]} onChange={(value) => chooseOperation(String(value))} />}</Space>
    {requested && !operation ? <UnsupportedModel modelID={requested} models={catalog.models} /> : !models.length ? <Empty description={t("image.noModels")} /> : <div className="playground-operation-grid"><section>{editing && <ReferenceImages limit={referenceLimit} images={references} onChange={setReferences} onWarning={(text) => message.warning(text)} />}<Typography.Title level={5}>{t("image.prompt")}</Typography.Title><Input.TextArea value={prompt} onChange={(event) => setPrompt(event.target.value)} autoSize={{ minRows: 6 }} /><Button type="primary" loading={loading} disabled={!prompt.trim() || (editing && !references.length)} onClick={() => void submit()}>{t(editing ? "image.edit" : "image.generate")}</Button></section><section><Typography.Title level={5}>{t("image.result")}</Typography.Title>{imageURL ? <img className="playground-generated-image" src={imageURL} alt={prompt} /> : result ? <pre>{JSON.stringify(result, null, 2)}</pre> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("image.empty")} />}</section></div>}
  </div>;
}

// referenceInput is the image field of an edit: one data URL for a single
// reference, a list - sent as one multipart part each, in order - for more.
function referenceInput(editing: boolean, references: ReferenceImage[]): { image?: string | string[] } {
  if (!editing || !references.length) return {};
  return { image: references.length === 1 ? references[0].dataURL : references.map((item) => item.dataURL) };
}

function generatedImageURL(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const data = (result as { data?: Array<{ b64_json?: string; url?: string }> }).data?.[0];
  if (data?.b64_json) return `data:image/png;base64,${data.b64_json}`;
  return data?.url || "";
}

const genericSizes = ["512x512", "768x768", "1024x1024"];

// declaredSizes reads the operation's size select: the options and default the
// template states, or the generic square sizes when it states none.
function declaredSizes(operation?: InferenceAPIOperation): { options: string[]; initial: string } {
  const field = operation?.inputs?.find((input) => input.name === "size" && input.kind === "select");
  const options = field?.options?.length ? field.options : genericSizes;
  const fallback = options.includes("1024x1024") ? "1024x1024" : options[0];
  const initial = typeof field?.default === "string" && options.includes(field.default) ? field.default : fallback;
  return { options, initial };
}
