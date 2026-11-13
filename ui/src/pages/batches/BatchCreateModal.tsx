import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useForm, useWatch } from "react-hook-form";
import { z } from "zod";
import { zodResolver } from "@hookform/resolvers/zod";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../../components/ui/dialog";
import { FormField } from "../../ui-kit/form/FormField";
import { formSubmit } from "../../ui-kit/form/submit";
import { FileUpload } from "../../ui-kit/FileUpload";
import { NumberInput } from "../../ui-kit/NumberInput";
import { Spinner } from "../../ui-kit/Spinner";
import { Notice } from "../../ui-kit/Notice";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { useTranslation } from "react-i18next";
import { batchApi, type Batch, type BatchDictionary, type BatchEndpoint } from "../../batchApi";
import { batchErrorText, type BatchErrorText } from "./batchErrors";
import { batchSampleJSONL, downloadBatchSample, sampleFileName } from "./batchSample";
import { modelsInFile, FILE_MODEL_SCAN_MAX_BYTES } from "./batchModels";
import BatchFileModels from "./BatchFileModels";
import BatchModelField, { useModelStateText } from "./BatchModelField";

export const BATCH_FILE_ACCEPT = ".jsonl";

const MB = 1024 * 1024;

// The i18n key naming a sample by its endpoint; an endpoint without one shows as is.
const sampleEndpointKey: Record<string, string> = {
  "/v1/chat/completions": "chat",
  "/v1/completions": "completions",
  "/v1/embeddings": "embeddings",
};

function sizeText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < MB) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / MB).toFixed(1)} MB`;
}

/**
 * useDictionary reads what a batch can be created with each time the dialog
 * opens, so the models' states are the server's of now; retry reads it again.
 */
function useDictionary(open: boolean) {
  const [dictionary, setDictionary] = useState<BatchDictionary | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (!open) return;
    let live = true;
    setError(null);
    batchApi.dictionary().then(
      (found) => { if (live) setDictionary(found); },
      (e: unknown) => { if (live) setError(e instanceof Error ? e : new Error(String(e))); },
    );
    return () => { live = false; };
  }, [open, attempt]);
  return { dictionary, error, retry: () => setAttempt((n) => n + 1) };
}

/**
 * BatchCreateModal uploads a JSONL requests file; the batch calls the url its
 * requests ask for. What it offers - the sample endpoints, the completion
 * window, the file limits and the models - is the server's batch dictionary.
 * An invalid file is answered with the line to fix; the dialog stays open so
 * the file can be replaced, and closes once the batch exists.
 */
export default function BatchCreateModal({ open, onClose, onCreated }: {
  open: boolean;
  onClose: () => void;
  onCreated: (batch: Batch) => void;
}) {
  const { t } = useTranslation("batches");
  const { t: tCommon } = useTranslation("common");
  const { dictionary, error: dictionaryError, retry } = useDictionary(open);
  // Only mirror limits supplied by the dictionary; file contents remain server-validated.
  const schema = z.object({
    file: z.custom<File | null>().refine((value): boolean => !!value, t("create.fileRequired")).refine(value => !value || !dictionary || value.size <= dictionary.max_file_bytes, t("create.tooLarge", { limit: dictionary ? Math.floor(dictionary.max_file_bytes / MB) : 0 })),
    windowHours: z.number().nullable().optional().superRefine((value, ctx) => {
      const bounds = dictionary?.completion_window_hours;
      const hours = value === undefined ? bounds?.default : value;
      if (bounds && (hours == null || !Number.isInteger(hours) || hours < bounds.min || hours > bounds.max)) ctx.addIssue({ code: "custom", message: t("create.window.help", bounds) });
    }),
    model: z.string().optional(),
  });
  const formRef = useRef<HTMLFormElement>(null);
  const form = useForm<z.infer<typeof schema>>({ resolver: zodResolver(schema), shouldFocusError: false, defaultValues: { file: null } });
  const [file, chosenWindow, model] = useWatch({ control: form.control, name: ["file", "windowHours", "model"] });
  const setFile = (value: File | null) => form.setValue("file", value, { shouldValidate: form.formState.isSubmitted });
  const setWindowHours = (value: number | null | undefined) => form.setValue("windowHours", value, { shouldValidate: form.formState.isSubmitted });
  const setModel = (value: string | undefined) => form.setValue("model", value);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<BatchErrorText | null>(null);
  const [showWaiting, setShowWaiting] = useState(false);
  const [sampleModel, setSampleModel] = useState<string>("");
  const [chosenSampleEndpoint, setSampleEndpoint] = useState<string | undefined>(undefined);
  // The endpoint of the sample to download or use; the batch itself takes the file's.
  const sampleEndpoint = (chosenSampleEndpoint ?? dictionary?.endpoints[0] ?? "") as BatchEndpoint;
  const bounds = dictionary?.completion_window_hours;
  const windowHours = chosenWindow === undefined ? (bounds?.default ?? null) : chosenWindow;
  const windowValid = !!bounds && windowHours !== null && Number.isInteger(windowHours) && windowHours >= bounds.min && windowHours <= bounds.max;
  const maxFileMB = dictionary ? Math.floor(dictionary.max_file_bytes / MB) : 0;
  const tooLarge = !!file && !!dictionary && file.size > dictionary.max_file_bytes;
  // The models the platform serves, as the server judges them for a batch: the
  // batch's model to choose, and the sample's, so it runs as downloaded.
  const modelOptions = useMemo(() => dictionary?.models.filter((option) => showWaiting || option.ready) ?? [], [dictionary, showWaiting]);
  const known = useMemo(() => (dictionary ? new Set(dictionary.models.map((option) => option.name)) : null), [dictionary]);
  const stateText = useModelStateText();
  // The sample's models are those serving its endpoint, in the server's order
  // (the best for a batch first), so a sample runs as downloaded.
  const sampleOptions = useMemo(() => modelOptions.filter((option) => option.endpoints.includes(sampleEndpoint)), [modelOptions, sampleEndpoint]);
  const servesSample = (name: string | undefined) => !!name && sampleOptions.some((option) => option.name === name);
  const defaultSample = sampleOptions[0]?.name ?? "";
  // The models the chosen file names, read here when it is small enough.
  const [fileModels, setFileModels] = useState<string[] | null>(null);
  const skipped = !!file && file.size > FILE_MODEL_SCAN_MAX_BYTES;
  useEffect(() => {
    setFileModels(null);
    if (!file || file.size > FILE_MODEL_SCAN_MAX_BYTES) return;
    let live = true;
    void modelsInFile(file).then((found) => { if (live) setFileModels(found); });
    return () => { live = false; };
  }, [file]);
  // The batch's own model when it serves the sample's endpoint, else the one picked for the sample.
  const chosenSample = [model, sampleModel].find(servesSample) || defaultSample;
  const sampleModelName = chosenSample || "your-model";
  const useSample = () => {
    pick(new File([batchSampleJSONL(sampleEndpoint, sampleModelName)], sampleFileName(sampleEndpoint), { type: "application/jsonl" }));
  };

  const reset = () => {
    form.reset({ file: null });
    setError(null);
    setShowWaiting(false);
    setSampleModel("");
  };
  const changeShowWaiting = (show: boolean) => {
    setShowWaiting(show);
    if (!show) {
      if (!dictionary?.models.some((option) => option.name === model && option.ready)) setModel(undefined);
      if (!dictionary?.models.some((option) => option.name === sampleModel && option.ready)) setSampleModel("");
    }
  };
  const pick = (picked: File | null) => {
    setFile(picked);
    setError(null);
  };
  const submit = async () => {
    if (!dictionary || !file || tooLarge || !windowValid) return;
    setSubmitting(true);
    setError(null);
    try {
      const batch = await batchApi.create(file, { model, completionWindowHours: windowHours ?? undefined });
      reset();
      onCreated(batch);
    } catch (e) {
      setError(batchErrorText(t, tCommon, e));
    } finally {
      setSubmitting(false);
    }
  };

  let body: ReactNode;
  if (!dictionary && dictionaryError) {
    body = (
      <Notice
        tone="danger"
        title={t("create.dictionary.failed")}
        children={dictionaryError.message}
        action={<Button variant="secondary" size="sm" onClick={retry}>{t("create.dictionary.retry")}</Button>}
      />
    );
  } else if (!dictionary || !bounds) {
    body = <div className="batch-create-loading"><Spinner label={t("create.dictionary.loading")} /></div>;
  } else {
    body = (
      <div className="batch-create-form">
        <FormField control={form.control} name="file" label={t("create.file")} required
          description={t("create.fileHelp")}>
          {binding => <FileUpload {...binding} value={file ?? null} meta={file ? sizeText(file.size) : undefined} onChange={pick} accept={BATCH_FILE_ACCEPT} disabled={submitting} />}
        </FormField>
        <BatchFileModels fileModels={fileModels} skipped={skipped && !tooLarge} known={known} override={model} />
        <FormField control={form.control} name="windowHours" label={t("create.window.label")} required
          description={<span className={windowValid ? undefined : "text-fg-danger"} role={windowValid ? undefined : "alert"}>{t("create.window.help", { min: bounds.min, max: bounds.max, default: bounds.default })}</span>}>
          {binding => <div className="flex items-center gap-2"><NumberInput {...binding} value={windowHours} onChange={setWindowHours}
            min={bounds.min} max={bounds.max} precision={0} disabled={submitting} className="w-40" /><span>{t("create.window.unit")}</span></div>}
        </FormField>
        <BatchModelField options={modelOptions} value={model} onChange={setModel} disabled={submitting} showWaiting={showWaiting} onShowWaitingChange={changeShowWaiting} />
        <div className="batch-create-field">
          <label htmlFor="batch-create-sample-model">{t("create.sample.title")}</label>
          <div className="batch-create-sample">
            <OptionSelect
              aria-label={t("create.sample.endpoint")}
              value={sampleEndpoint}
              onChange={setSampleEndpoint}
              disabled={submitting}
              options={dictionary.endpoints.map((value) => ({ value, label: sampleEndpointKey[value] ? t(`create.sample.endpoints.${sampleEndpointKey[value]}`, { endpoint: value }) : value }))}
              style={{ minWidth: 200 }}
            />
            <OptionSelect
              id="batch-create-sample-model"
              aria-label={t("create.sample.model")}
              value={chosenSample || undefined}
              placeholder={t("create.sample.noModel")}
              onChange={setSampleModel}
              disabled={submitting || servesSample(model) || sampleOptions.length === 0}
              options={sampleOptions.map((option) => ({ value: option.name, label: `${option.name} · ${stateText(option.state)}` }))}
              style={{ minWidth: 200 }}
            />
            <Button variant="secondary" onClick={() => downloadBatchSample(sampleEndpoint, sampleModelName)} disabled={submitting}>
              {t("create.sample.download")}
            </Button>
            <Button variant="secondary" onClick={useSample} disabled={submitting || !chosenSample}>{t("create.sample.use")}</Button>
          </div>
        </div>
        <Notice
          tone="info"
            title={t("create.hintTitle")}
          children={
            <ul className="batch-create-hint">
              <li>{t("create.hint.format", { size: maxFileMB, requests: dictionary.max_file_lines })}</li>
              <li>{t("create.hint.line")}</li>
              <li>{t("create.hint.unique")}</li>
              <li>{t("create.hint.sameUrl")}</li>
              <li>{t("create.hint.noStream")}</li>
              <li>{t("create.hint.retention", { days: dictionary.output_retention_days })}</li>
            </ul>
          }
        />
        {tooLarge && <Notice tone="danger" title={t("create.tooLarge", { limit: maxFileMB })} />}
        {error && (
          <Notice
            tone="danger"
                title={t("create.failed")}
            children={<>
              <div>{error.message}</div>
              {error.detail && <span className="text-fg-muted">{t("errors.detail", { detail: error.detail })}</span>}
            </>}
          />
        )}
      </div>
    );
  }

  const close = () => { if (!submitting) { reset(); onClose(); } };
  return (
    <Dialog open={open} onOpenChange={next => { if (!next) close(); }} disablePointerDismissal>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={submitting} style={{ width: "min(520px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col">
        <DialogHeader><DialogTitle>{t("create.title")}</DialogTitle></DialogHeader>
        <form ref={formRef} noValidate onSubmit={formSubmit(form, () => formRef.current, submit)} className="flex min-h-0 flex-col gap-4">
          <div className="min-h-0 overflow-auto">{body}</div>
          <DialogFooter>
            <Button variant="ghost" onClick={close} disabled={submitting}>{t("create.cancel")}</Button>
            <Button type="submit" variant="primary" loading={submitting} disabled={!dictionary || !file || tooLarge || !windowValid}>
              {submitting ? t("create.submitting") : t("create.submit")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
