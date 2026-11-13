import { useEffect, useRef, useState } from "react";
import { message } from "antd";
import type { FormInstance } from "antd";
import { useTranslation } from "react-i18next";
import {
  api,
  type EngineProfile,
  type InferenceTemplate,
  type ModelCore,
  type ModelPresenceSummary,
  type TemplateWeightModels,
  type WeightSource,
} from "../../api";

/** A registered model as the lookup lists it; presence says where its files are. */
export type WeightModel = ModelCore & { presence?: ModelPresenceSummary };

const LOOKUP_RETRY_MS = 15000;

/**
 * The registered model the server says a choice of this declared source
 * reuses, if any. The source is found by its declared fields, which the
 * answer echoes; which model serves it is the server's judgement.
 */
export function reusedModel(
  answer: { models: WeightModel[]; sources: TemplateWeightModels["sources"] },
  source: WeightSource,
): WeightModel | undefined {
  const same = (a?: string, b?: string) => (a || "") === (b || "");
  const declared = answer.sources.find(
    (item) =>
      same(item.provider, source.provider) &&
      same(item.repo_id, source.repo_id) &&
      same(item.revision, source.revision) &&
      same(item.subfolder, source.subfolder),
  );
  if (!declared?.reuse_model_id) return undefined;
  return answer.models.find((model) => model.id === declared.reuse_model_id);
}

// useTemplateWeightSource owns the template's weight artifact choice: the
// spec's declared mirrors, the explicit source choice in the modal, and the
// one registration action through the existing register API. Which registered
// models serve the spec, and which one a declared source reuses, is asked of
// the server (GET /inference-templates/weight-models).
export default function useTemplateWeightSource({
  form,
  template,
  selection,
  engineProfile,
  adoptedKey,
}: {
  form: FormInstance;
  template?: InferenceTemplate;
  selection?: { spec: import("../../api").InferenceSpec };
  engineProfile?: EngineProfile;
  // The template spec (template id, spec source and name, as `key` below
  // spells them) a form opened from a deployment's draft already carries:
  // its model, image and environment are the deployment's, not the spec's,
  // so choosing it again is not asked for.
  adoptedKey?: string;
}) {
  const { t } = useTranslation("deploy");
  const [modalOpen, setModalOpen] = useState(false);
  const [choice, setChoice] = useState<WeightSource>();
  const [registering, setRegistering] = useState(false);
  // The registered models serving the selected spec's sources, once the
  // lookup for that spec (key) has answered; a pending or failed lookup
  // leaves it on another key, and nothing is registered meanwhile.
  // Invalidate already queued effects as well as late query responses when
  // an obsolete registration requires a fresh lookup of the current spec.
  const lookupRequest = useRef(0);
  const [lookup, setLookup] = useState<{ key: string; request: number; models: WeightModel[]; sources: TemplateWeightModels["sources"] }>();
  // A failed lookup is retried at the pace the model list used to poll.
  const [lookupRetry, setLookupRetry] = useState(0);
  const stateRef = useRef({ applied: adoptedKey ?? "", prompted: "", registering: "" });
  // What the user settled on for this spec: a registered model or a declared
  // source (whose model, registered now or already there, is then applied).
  const pickedRef = useRef<{ key: string; model?: WeightModel; source?: WeightSource }>(undefined);
  // The registered models matching a declared source, listed for the user to
  // confirm (5.13), and which one (or the template's own source) they picked.
  const [matches, setMatches] = useState<WeightModel[]>([]);
  const [useModel, setUseModel] = useState<number | "template">();
  // The model the form now carries for this template, to show its source.
  const [appliedModel, setAppliedModel] = useState<WeightModel>();
  const key =
    template && selection
      ? `${template.id}:${selection.spec.source}:${selection.spec.name}`
      : "";
  const sources: WeightSource[] = selection?.spec.weight_sources || [];
  // Identity changes also invalidate a response from an earlier visit to the
  // same spec, not only a response for a different current key.
  const activeSelection = useRef({ key });
  if (activeSelection.current.key !== key) activeSelection.current = { key };

  useEffect(() => {
    setModalOpen(false);
    setChoice(undefined);
    setMatches([]);
    setUseModel(undefined);
    setAppliedModel(undefined);
    setRegistering(false);
    stateRef.current.prompted = "";
  }, [key]);

  useEffect(() => {
    if (!key || key === adoptedKey) return;
    const provider = form.getFieldValue(["model_source", "provider"]);
    // A model-only default belongs to the previous scope. On a new spec,
    // retain a provider only when that spec declares it; storage is independent.
    if (
      typeof provider === "string" && provider.trim() &&
      !sources.some((source) => source.provider.toLowerCase() === provider.trim().toLowerCase())
    ) form.setFieldValue(["model_source", "provider"], undefined);
  }, [form, key]);


  useEffect(() => {
    if (!key || key === adoptedKey || !template || !selection || !sources.length || !sources[0]?.repo_id) return;
    let alive = true;
    const request = ++lookupRequest.current;
    let retry: ReturnType<typeof setTimeout> | undefined;
    api.templateWeightModels(template.id, selection.spec.source, selection.spec.name).then(
      (answer) => {
        if (alive && request === lookupRequest.current) setLookup({ key, request, ...answer });
      },
      (reason: Error) => {
        if (!alive || request !== lookupRequest.current) return;
        message.error(t("action.modelLookupFailed", { detail: reason.message }));
        retry = setTimeout(() => setLookupRetry((n) => n + 1), LOOKUP_RETRY_MS);
      },
    );
    return () => {
      alive = false;
      clearTimeout(retry);
    };
  }, [key, lookupRetry]);

  const register = async (source: WeightSource, registrationKey: string) => {
    const requestSelection = activeSelection.current;
    if (requestSelection.key !== registrationKey || !source.repo_id ||
      !["huggingface", "modelscope"].includes(source.provider.toLowerCase()) || stateRef.current.registering === registrationKey)
      return;
    stateRef.current.registering = registrationKey;
    setRegistering(true);
    try {
      const model = await api.quickRegisterModel({
        source: source.provider,
        source_id: source.repo_id,
        revision: source.revision,
        artifact_subfolder: source.subfolder,
        name: source.repo_id,
        weight_bytes: selection?.spec.weight_bytes,
        quantization: selection?.spec.quantization,
        architecture: template?.architecture,
        context_length: template?.context_length,
      });
      if (activeSelection.current !== requestSelection) return;
      form.setFieldValue("model_id", model.id);
      pickedRef.current = { key: registrationKey, model };
      // The registered model now serves the spec: the template is applied
      // to the form as for a model that was already registered.
      setLookup((prev) =>
        prev?.key === registrationKey
          ? { ...prev, models: [...prev.models, model] }
          : prev,
      );
      setModalOpen(false);
      setChoice(undefined);
      message.success(t("action.modelRegistered"));
      return model;
    } catch (reason) {
      if (activeSelection.current !== requestSelection) return;
      stateRef.current.registering = "";
      message.error(
        t("action.modelRegisterFailed", { detail: (reason as Error).message }),
      );
      return undefined;
    } finally {
      if (stateRef.current.registering === registrationKey) stateRef.current.registering = "";
      if (activeSelection.current === requestSelection) setRegistering(false);
      // Returning to the same spec must discover the model the obsolete
      // request just registered, without applying it or registering twice.
      else if (activeSelection.current.key === registrationKey) {
        lookupRequest.current++;
        setLookup(undefined);
        setLookupRetry((n) => n + 1);
      }
    }
  };

  useEffect(() => {
    if (!template || !selection || !key) return;
    if (stateRef.current.applied === key) return;
    if (!sources.length || !sources[0]?.repo_id) return;
    // Wait for the lookup before asking, choosing or registering: an answer
    // not yet in (or a failed one) must not create a duplicate model.
    if (lookup?.key !== key || lookup.request !== lookupRequest.current) return;
    const picked =
      pickedRef.current?.key === key ? pickedRef.current : undefined;
    if (!picked) {
      // A registered model serving a declared source is never taken
      // silently: the user confirms it, or picks a source.
      const found = lookup.models;
      // Several sources with nothing registered: the user chooses, with no
      // default. One source with nothing registered: nothing to ask.
      if (found.length || sources.length > 1 || sources[0]?.provider.toLowerCase() === "local") {
        if (stateRef.current.prompted === key) return;
        stateRef.current.prompted = key;
        setMatches(found);
        setAppliedModel(undefined);
        setUseModel(found.length === 1 ? found[0].id : undefined);
        setChoice(undefined);
        setModalOpen(true);
        return;
      }
    }
    // A chosen source is served only by its own provider's model.
    const model =
      picked?.model ??
      (picked?.source ? reusedModel(lookup, picked.source) : undefined);
    if (model) {
      if (!engineProfile || engineProfile.name !== selection.spec.engine)
        return;
      form.setFieldsValue({
        model_id: model.id,
        template_id: template.id,
        template_spec_source: selection.spec.source,
        template_spec_name: selection.spec.name,
        scenario: selection.spec.scenario || undefined,
        engine: selection.spec.engine,
        image: selection.spec.image,
        image_mode: "custom",
        container_env_rows: Object.entries(selection.spec.env || {}).map(
          ([envKey, value]) => ({ key: envKey, value }),
        ),
      });
      setAppliedModel(model);
      stateRef.current.applied = key;
      return;
    }
    if (stateRef.current.registering === key) return;
    if (sources.length === 1 && !picked) {
      void register(sources[0], key);
    }
  }, [engineProfile, lookup, selection, template]);

  const confirm = async () => {
    if (!selection || lookup?.key !== key || lookup.request !== lookupRequest.current) return;
    const requestSelection = activeSelection.current;
    const confirmKey = `${template?.id || 0}:${selection.spec.source}:${selection.spec.name}`;
    if (typeof useModel === "number") {
      // The user confirmed a registered model: it serves the template.
      const picked = matches.find((item) => item.id === useModel);
      if (!picked) return;
      pickedRef.current = { key: confirmKey, model: picked };
      form.setFieldValue("model_id", picked.id);
      setLookup((prev) => ({
        key: confirmKey,
        request: lookupRequest.current,
        models: prev?.key === confirmKey ? prev.models : [picked],
        sources: prev?.key === confirmKey ? prev.sources : [],
      }));
      setModalOpen(false);
      setChoice(undefined);
      return;
    }
    if (!choice) return;
    pickedRef.current = { key: confirmKey, source: choice };
    // The choice is matched against a fresh lookup, never a missing one: a
    // failed lookup registers nothing.
    let answer: TemplateWeightModels;
    try {
      answer = await api.templateWeightModels(template?.id || 0, selection.spec.source, selection.spec.name);
    } catch (reason) {
      if (activeSelection.current !== requestSelection) return;
      message.error(t("action.modelLookupFailed", { detail: (reason as Error).message }));
      return;
    }
    if (activeSelection.current !== requestSelection) return;
    const existing = reusedModel(answer, choice);
    if (existing) {
      form.setFieldValue("model_id", existing.id);
      // The chosen source's model applies the template, as a single
      // source's would.
      setLookup({ key: confirmKey, request: lookupRequest.current, ...answer });
      setModalOpen(false);
      setChoice(undefined);
      return;
    }
    void register(choice, confirmKey);
  };

  return {
    modalOpen,
    choice,
    setChoice,
    registering,
    setModalOpen,
    confirm,
    register,
    matches,
    useModel,
    setUseModel,
    appliedModel,
  };
}
