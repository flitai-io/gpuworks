import UnsupportedModel from "./UnsupportedModel";
import RequestDrawer from "./RequestDrawer";
import { composeRequest, declaredValues } from "./compose";
import InputsForm from "./InputsForm";
import { Button, Empty, Input, InputNumber, Progress, Select, Space, Tag, Typography, message } from "antd";
import { useEffect, useMemo, useRef, useState } from "react";
import { flushSync } from "react-dom";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { invokeOperation, retrieveAsync } from "./api";
import { modelsForOperation, operationOf } from "./operations";
import type { PlaygroundCatalog } from "./types";

export default function VideoPage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation("playground");
  const [search, setSearch] = useSearchParams();
  const models = useMemo(() => modelsForOperation(catalog.models, "openai.video.generate"), [catalog.models]);
  const requested = search.get("model") || "";
  const modelID = requested || models[0]?.id || "";
  const operation = operationOf(models.find((model) => model.id === modelID), "openai.video.generate");
  const [prompt, setPrompt] = useState("A sunrise over mountains");
  const [seconds, setSeconds] = useState(4);
  const [job, setJob] = useState<Record<string, unknown>>();
  const [videoURL, setVideoURL] = useState("");
  const [inputValues, setInputValues] = useState<Record<string, unknown>>({});
  const [loading, setLoading] = useState(false);
  const abortRef = useRef<AbortController>(undefined);

  useEffect(() => {
    if (!modelID || requested === modelID) return;
    const next = new URLSearchParams(search); next.set("model", modelID); setSearch(next, { replace: true });
  }, [modelID, requested, search, setSearch]);
  useEffect(() => () => { abortRef.current?.abort(); }, []);
  useEffect(() => () => { if (videoURL) URL.revokeObjectURL(videoURL); }, [videoURL]);

  const generate = async () => {
    if (!operation || !modelID) return;
    abortRef.current?.abort();
    const controller = new AbortController(); abortRef.current = controller;
    setLoading(true); setJob(undefined); setVideoURL("");
    try {
      const defaults = Object.fromEntries((operation.inputs || []).filter((field) => field.default !== undefined).map((field) => [field.name, field.default]));
      const secondsValue = operation.inputs?.find((field) => field.name === "seconds")?.default ?? seconds;
      const created = await invokeOperation(operation, modelID, { prompt, ...defaults, seconds: secondsValue, ...declaredValues(operation, inputValues) }, controller.signal) as unknown;
      if (created instanceof Blob && operation.mode === "sync") {
        setVideoURL(URL.createObjectURL(created));
        return;
      }
      if (operation.mode === "sync") {
        setJob(created as Record<string, unknown>);
        return;
      }
      // The task ID must become cancellable before entering a potentially
      // long first poll. Flush this one boundary synchronously so React cannot
      // batch it until the async handler finishes.
      const asyncJob = created as Record<string, unknown>;
      flushSync(() => setJob(asyncJob));
      const taskID = String(asyncJob[operation.async?.id_field || "id"] || "");
      if (!taskID) throw new Error(t("video.missingID"));
      await poll(operation, modelID, taskID, controller.signal, setJob);
      const content = await retrieveAsync(operation, modelID, taskID, "content", controller.signal) as Blob;
      setVideoURL(URL.createObjectURL(content));
    } catch (error) { if ((error as Error).name !== "AbortError") message.error((error as Error).message); }
    finally { setLoading(false); }
  };
  const cancel = async () => {
    abortRef.current?.abort();
    const taskID = String(job?.[operation?.async?.id_field || "id"] || "");
    if (operation && modelID && taskID) {
      try { setJob(await retrieveAsync(operation, modelID, taskID, "cancel") as Record<string, unknown>); }
      catch (error) { message.error((error as Error).message); }
    }
    setLoading(false);
  };
  // A declared required input (such as the reference image a Ref2VA model needs)
  // must be filled before the request can be sent.
  const missingRequired = (operation?.inputs || []).some((field) => field.required && field.default === undefined && (inputValues[field.name] === undefined || inputValues[field.name] === ""));
  const status = String(job?.[operation?.async?.status_field || "status"] || "");
  const progress = Number(job?.progress || 0);
  const requestPreview = operation ? (() => {
    const values = { ...declaredValues(operation, { seconds: operation.inputs?.find((field) => field.name === "seconds")?.default ?? seconds }), ...declaredValues(operation, inputValues) };
    for (const field of operation.inputs || []) {
      if (field.required && values[field.name] === undefined) {
        values[field.name] = field.kind === "image" ? "data:image/png;base64,AA==" : "data:application/octet-stream;base64,AA==";
      }
    }
    return composeRequest(operation, modelID, { prompt }, values);
  })() : {};
  if (requested && !operation) return <UnsupportedModel modelID={requested} models={catalog.models} />;
  return <div className="playground-operation-page">
    <Space wrap className="playground-operation-toolbar"><RequestDrawer operation={operation} model={modelID} body={requestPreview} onSend={(raw) => { void invokeOperation(operation!, modelID, JSON.parse(raw)).then((value) => { if (value instanceof Blob) setVideoURL(URL.createObjectURL(value)); else setJob(value as Record<string, unknown>); }); }} /><Select className="playground-model-select" value={modelID || undefined} placeholder={t("video.modelPlaceholder")} options={models.map((model) => ({ value: model.id, label: model.label }))} onChange={(value) => { const next = new URLSearchParams(search); next.set("model", value); setSearch(next, { replace: true }); }} /><InputNumber min={1} max={60} value={seconds} onChange={(value) => setSeconds(value || 4)} addonAfter={t("video.seconds")} /></Space>
    {!models.length ? <Empty description={t("video.noModels")} /> : <div className="playground-operation-grid"><section><Typography.Title level={5}>{t("video.prompt")}</Typography.Title><Input.TextArea value={prompt} onChange={(event) => setPrompt(event.target.value)} autoSize={{ minRows: 6 }} />{operation && <InputsForm fields={(operation.inputs || []).filter((field) => field.name !== "seconds")} values={{ ...Object.fromEntries((operation.inputs || []).filter((field) => field.default !== undefined).map((field) => [field.name, field.default])), ...declaredValues(operation, inputValues) }} onChange={(name, value) => setInputValues((current) => ({ ...current, [name]: value }))} />}<Space><Button type="primary" loading={loading} disabled={!prompt.trim() || missingRequired} onClick={() => void generate()}>{t("video.generate")}</Button>{loading && <Button danger onClick={() => void cancel()}>{t("video.cancel")}</Button>}</Space></section><section><Space><Typography.Title level={5}>{t("video.result")}</Typography.Title>{status && <Tag>{status}</Tag>}</Space>{loading && <Progress percent={Number.isFinite(progress) ? progress : 0} status="active" />}{videoURL ? <video className="playground-generated-video" src={videoURL} controls /> : job ? <pre>{JSON.stringify(job, null, 2)}</pre> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("video.empty")} />}</section></div>}
  </div>;
}

async function poll(operation: NonNullable<ReturnType<typeof operationOf>>, model: string, taskID: string, signal: AbortSignal, update: (job: Record<string, unknown>) => void): Promise<void> {
  const lifecycle = operation.async;
  if (!lifecycle) throw new Error("Missing async lifecycle");
  while (!signal.aborted) {
    const job = await retrieveAsync(operation, model, taskID, "poll", signal) as Record<string, unknown>;
    update(job);
    const status = String(job[lifecycle.status_field] || "");
    if (lifecycle.success_statuses.includes(status)) return;
    if (lifecycle.failure_statuses.includes(status)) throw new Error(`Video generation ended with status ${status}`);
    await new Promise<void>((resolve, reject) => { const timer = window.setTimeout(resolve, 1000); signal.addEventListener("abort", () => { window.clearTimeout(timer); reject(new DOMException("Aborted", "AbortError")); }, { once: true }); });
  }
}
