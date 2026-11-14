import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { invokeJSON } from "./api";
import { modelsForOperation, operationOf } from "./operations";
import RequestDrawer from "./RequestDrawer";
import UnsupportedModel from "./UnsupportedModel";
import type { PlaygroundCatalog, PlaygroundOperation } from "./types";
import EmptyState from "../../components/EmptyState";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { FileUpload } from "../../ui-kit/FileUpload";
import { Input } from "../../components/ui/input";
import { Textarea } from "../../components/ui/textarea";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { Notice } from "../../ui-kit/Notice";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { Progress } from "../../ui-kit/Progress";
import { Segmented } from "../../ui-kit/Segmented";

// The System One wire format's question types (TypeSafe's protocol, which
// systemone.decide declares): they are vocabulary of the request, not a rule
// the page decides.
type QuestionType = "noul" | "choice" | "score";
const questionTypes: QuestionType[] = ["noul", "choice", "score"];

export interface QuestionDraft {
  key: string;
  name: string;
  type: QuestionType;
  instructions: string;
  // One option per line: "name: description" for choice, one level per line
  // (lowest first) for score; unused by noul.
  criteria: string;
}

interface Answer {
  type?: string;
  probabilities?: Record<string, number>;
  confidence?: number;
  decision?: string;
  choice?: string;
  noul?: number;
  score?: number;
  legend?: Record<string, string>;
}

interface DecisionResult {
  answers?: Record<string, Answer>;
  usage?: { input_tokens?: number };
}

let nextKey = 0;
const newKey = () => `q${++nextKey}`;

// draftsFromExample turns the server's example request (inferenceapi
// BuildExample) into editable rows, so the page starts with a request the
// model accepts.
export function draftsFromExample(questions: unknown): QuestionDraft[] {
  if (!questions || typeof questions !== "object") return [];
  return Object.entries(questions as Record<string, Record<string, unknown>>).map(([name, question]) => {
    const type = questionTypes.includes(question?.type as QuestionType) ? question.type as QuestionType : "noul";
    const criteria = question?.criteria;
    let text = "";
    if (Array.isArray(criteria)) text = criteria.map(String).join("\n");
    else if (criteria && typeof criteria === "object") text = Object.entries(criteria).map(([option, description]) => `${option}: ${description}`).join("\n");
    return { key: newKey(), name, type, instructions: String(question?.instructions ?? ""), criteria: text };
  });
}

// questionNameProblem says why the drafts cannot become a request: a question
// is keyed by its name, so an empty or repeated name would silently drop one.
export function questionNameProblem(drafts: QuestionDraft[]): "empty" | "duplicate" | undefined {
  const names = drafts.map((draft) => draft.name.trim());
  if (names.some((name) => !name)) return "empty";
  if (new Set(names).size !== names.length) return "duplicate";
  return undefined;
}

// questionsBody is the wire form of the drafts.
export function questionsBody(drafts: QuestionDraft[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const draft of drafts) {
    const name = draft.name.trim();
    if (!name) continue;
    const question: Record<string, unknown> = { type: draft.type, instructions: draft.instructions };
    const lines = draft.criteria.split("\n").map((line) => line.trim()).filter(Boolean);
    if (draft.type === "score") question.criteria = lines;
    if (draft.type === "choice") {
      question.criteria = Object.fromEntries(lines.map((line) => {
        const at = line.indexOf(":");
        return at < 0 ? [line, ""] : [line.slice(0, at).trim(), line.slice(at + 1).trim()];
      }));
    }
    out[name] = question;
  }
  return out;
}

export default function DecisionPage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation("playground");
  const [search, setSearch] = useSearchParams();
  const models = useMemo(() => modelsForOperation(catalog.models, "systemone.decide"), [catalog.models]);
  const requested = search.get("model") || "";
  const modelID = requested || models[0]?.id || "";
  const model = models.find((item) => item.id === modelID);
  const operation = operationOf(model, "systemone.decide");
  const acceptsImages = (model?.input_modalities || []).includes("image");
  const [stateFormat, setStateFormat] = useState<"text" | "json">("text");
  const [stateText, setStateText] = useState("");
  const [drafts, setDrafts] = useState<QuestionDraft[]>([]);
  const [images, setImages] = useState<{ name: string; dataURL: string }[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string>();
  const [result, setResult] = useState<DecisionResult>();
  // Each send gets a number; a reply to an older send (another model, a
  // request replaced meanwhile) is dropped.
  const sent = useRef(0);

  useEffect(() => {
    const next = new URLSearchParams(search);
    next.set("operation", "systemone.decide");
    if (modelID) next.set("model", modelID); else next.delete("model");
    if (next.toString() !== search.toString()) setSearch(next, { replace: true });
  }, [modelID, search, setSearch]);

  // Start from the server's example when the model or its example changes.
  // A refreshed catalog is a new object with the same content: keying on the
  // content keeps what the user has typed.
  const example = operation?.example?.request.body;
  const exampleKey = `${modelID}\n${JSON.stringify(example ?? null)}`;
  useEffect(() => {
    sent.current++;
    setImages([]);
    setLoading(false);
    if (!example) return;
    const state = example.state;
    setStateFormat(typeof state === "string" ? "text" : "json");
    setStateText(typeof state === "string" ? state : JSON.stringify(state ?? "", null, 2));
    setDrafts(draftsFromExample(example.questions));
    setResult(undefined);
    setError(undefined);
  }, [exampleKey]);

  const body = (): Record<string, unknown> => {
    const state = stateFormat === "json" ? JSON.parse(stateText) as unknown : stateText;
    const out: Record<string, unknown> = { state, questions: questionsBody(drafts) };
    if (acceptsImages && images.length) out.images = images.map((image) => image.dataURL);
    return out;
  };
  let preview: Record<string, unknown> = {};
  try { preview = { model: modelID, ...body() }; } catch { preview = { model: modelID }; }

  const send = async (op: PlaygroundOperation, request: Record<string, unknown>) => {
    const id = ++sent.current;
    setLoading(true); setError(undefined); setResult(undefined);
    try {
      const reply = await invokeJSON(op, modelID, request) as DecisionResult;
      if (id === sent.current) setResult(reply);
    } catch (next) {
      if (id === sent.current) setError((next as Error).message);
    } finally {
      if (id === sent.current) setLoading(false);
    }
  };
  const submit = () => {
    if (!operation || loading) return;
    const problem = questionNameProblem(drafts);
    if (problem) { setError(t(`decision.names.${problem}`)); return; }
    let request: Record<string, unknown>;
    try { request = body(); } catch { setError(t("decision.invalidJSON")); return; }
    void send(operation, request);
  };
  const update = (key: string, patch: Partial<QuestionDraft>) => setDrafts((rows) => rows.map((row) => row.key === key ? { ...row, ...patch } : row));
  const addImage = (file: File | null) => {
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => setImages((current) => [...current, { name: file.name, dataURL: String(reader.result || "") }]);
    reader.onerror = () => setError(t("decision.imageReadFailed", { name: file.name }));
    reader.readAsDataURL(file);
  };
  const typeOptions = questionTypes.map((type) => ({ value: type, label: t(`decision.type.${type}`) }));

  return <div className="playground-operation-page">
    <div className="playground-operation-toolbar flex flex-wrap items-center gap-2">
      <RequestDrawer operation={operation} model={modelID} body={preview} onSend={(raw) => { if (!operation || loading) return; try { void send(operation, JSON.parse(raw)); } catch { setError(t("decision.invalidJSON")); } }} />
      <div className="playground-model-select"><OptionSelect aria-label={t("decision.model")} placeholder={t("decision.modelPlaceholder")} value={modelID || undefined} options={models.map((item) => ({ value: item.id, label: item.label }))} onChange={(value) => { const next = new URLSearchParams(search); next.set("model", value); setSearch(next, { replace: true }); }} /></div>
    </div>
    {requested && !operation ? <UnsupportedModel modelID={requested} models={catalog.models} /> : !models.length ? <EmptyState description={t("decision.noModels")} /> : <div className="playground-operation-grid">
      <section className="flex flex-col gap-3">
        <div className="flex items-center justify-between gap-2">
          <h5 className="text-fg-default font-medium">{t("decision.state")}</h5>
          <Segmented label={t("decision.stateFormat")} value={stateFormat} options={[{ value: "text", label: t("decision.stateText") }, { value: "json", label: "JSON" }]} onChange={(value) => setStateFormat(value as "text" | "json")} />
        </div>
        <Textarea aria-label={t("decision.state")} value={stateText} onChange={(event) => setStateText(event.target.value)} rows={3} />
        {acceptsImages && <div className="flex flex-col gap-2">
          <h5 className="text-fg-default font-medium">{t("decision.images")}</h5>
          <FileUpload aria-label={t("decision.addImages")} accept="image/png,image/jpeg,image/webp" value={null} onChange={addImage} />
          <div className="flex flex-wrap gap-2">{images.map((image, index) => <Badge key={`${image.name}-${index}`} color="gray">{image.name}<Button variant="ghost" size="xs" iconOnly leadingIcon={ConsoleIcon.close} aria-label={t("decision.removeImage", { name: image.name })} onClick={() => setImages((current) => current.filter((_, at) => at !== index))} /></Badge>)}</div>
        </div>}
        <h5 className="text-fg-default font-medium">{t("decision.questions")}</h5>
        {drafts.map((draft, index) => <div key={draft.key} className="flex flex-col gap-2 rounded-lg border border-border p-3">
          <div className="flex flex-wrap items-center gap-2">
            <Input className="w-40" aria-label={t("decision.questionName", { index: index + 1 })} value={draft.name} onChange={(event) => update(draft.key, { name: event.target.value })} />
            <div className="w-36"><OptionSelect aria-label={t("decision.questionType", { index: index + 1 })} value={draft.type} options={typeOptions} onChange={(type) => update(draft.key, { type: type as QuestionType })} /></div>
            <Button className="ml-auto" variant="ghost" size="sm" iconOnly leadingIcon={ConsoleIcon.delete} aria-label={t("decision.removeQuestion", { index: index + 1 })} onClick={() => setDrafts((rows) => rows.filter((row) => row.key !== draft.key))} />
          </div>
          <Input aria-label={t("decision.instructions", { index: index + 1 })} placeholder={t("decision.instructionsPlaceholder")} value={draft.instructions} onChange={(event) => update(draft.key, { instructions: event.target.value })} />
          {draft.type !== "noul" && <Textarea aria-label={t("decision.criteria", { index: index + 1 })} placeholder={t(`decision.criteriaPlaceholder.${draft.type}`)} value={draft.criteria} onChange={(event) => update(draft.key, { criteria: event.target.value })} rows={3} />}
        </div>)}
        <div className="flex gap-2">
          <Button variant="secondary" leadingIcon={ConsoleIcon.add} onClick={() => setDrafts((rows) => [...rows, { key: newKey(), name: `q${rows.length + 1}`, type: "noul", instructions: "", criteria: "" }])}>{t("decision.addQuestion")}</Button>
          <Button variant="primary" loading={loading} disabled={!drafts.length} onClick={submit}>{t("decision.run")}</Button>
        </div>
      </section>
      <section className="flex flex-col gap-3">
        <h5 className="text-fg-default font-medium">{t("decision.result")}</h5>
        {error && <Notice tone="danger" role="alert" title={t("decision.failed")}>{error}</Notice>}
        {result ? <DecisionAnswers result={result} /> : !error && <EmptyState compact description={t("decision.empty")} />}
      </section>
    </div>}
  </div>;
}

function DecisionAnswers({ result }: { result: DecisionResult }) {
  const { t } = useTranslation("playground");
  const percent = (value?: number) => Math.round((value ?? 0) * 1000) / 10;
  const label = (answer: Answer, option: string) => answer.legend?.[option] ? `${option} · ${answer.legend[option]}` : option;
  return <div className="flex flex-col gap-3">
    {Object.entries(result.answers || {}).map(([name, answer]) => {
      const probabilities = Object.entries(answer.probabilities || {}).sort((a, b) => b[1] - a[1]);
      // `choice` is the System One contract's answer; some models also send `decision`.
      const verdict = answer.decision ?? answer.choice;
      return <section key={name} aria-label={name} className="flex flex-col gap-2 rounded-lg border border-border p-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-fg-default font-medium">{name}</span>
          {answer.type && <Badge color="gray" size="sm">{t(`decision.type.${answer.type}`, { defaultValue: answer.type })}</Badge>}
          {verdict !== undefined && <Badge color="blue" size="sm">{t("decision.decision", { value: label(answer, verdict) })}</Badge>}
        </div>
        {answer.noul !== undefined && <span className="text-fg-default">{t("decision.yesProbability", { value: percent(answer.noul) })}</span>}
        {answer.score !== undefined && <span className="text-fg-default">{t("decision.expectedScore", { value: answer.score.toFixed(2) })}</span>}
        {probabilities.map(([option, probability]) => <div key={option} className="flex flex-col gap-1">
          <div className="flex justify-between gap-2 text-fg-default"><span>{label(answer, option)}</span><span className="text-fg-muted">{percent(probability)}%</span></div>
          <Progress value={percent(probability)} label={t("decision.probabilityOf", { option })} />
        </div>)}
        {answer.confidence !== undefined && <span className="text-fg-muted">{t("decision.confidence", { value: percent(answer.confidence) })}</span>}
      </section>;
    })}
    {result.usage?.input_tokens !== undefined && <span className="text-fg-muted">{t("decision.inputTokens", { count: result.usage.input_tokens })}</span>}
    <details><summary className="cursor-pointer text-fg-muted">{t("decision.raw")}</summary><pre>{JSON.stringify(result, null, 2)}</pre></details>
  </div>;
}
