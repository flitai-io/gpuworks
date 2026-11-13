import { useEffect, useRef, useState } from "react";
import type { FormInstance } from "antd";
import type { EvaluateResponse } from "../../api";
import { api } from "../../api";
import {
  APIError,
  apiErrorText,
  type APIErrorTranslator,
} from "../../apiError.ts";
import { applyEvaluateFormValues } from "./deployHelpers";
import { EvaluateQuickParams } from "./evaluateQuickParams";

// Server field paths to the deploy form's own control names. Everything the
// server attributes must land on a control the deployer can see; the wire name
// and the form name differ only where noted here.
const formFieldName: Record<string, string> = {
  node_uuids: "distributed_nodes",
  // The automatic placement of a multi-node or PD request: nothing to place
  // on, a missing TP, units that span nodes - beside the auto / manual switch.
  placement_mode: "units_placement",
  node_requirements: "node_labels",
  "placement.tp": "tp",
  "placement.pp": "pp",
  "placement.gpu_indexes": "gpu_indexes",
  "placement.gpus": "gpu_indexes",
};

// Fields the deploy form renders as a Form.List (or, for "pd", a whole
// section with no single input of its own) rather than a plain control: antd
// never registers a ref for these, so form.getFieldInstance(name) stays
// undefined even while the section is on screen. These always count as
// having a control - the target for their error is the section itself (see
// ContainerOverridesEditor's Form.ErrorList and DeployPdSection's own error box),
// never the page-level line.
const listControlFields = new Set(["container_mount_rows", "pd"]);

// hasRenderedControl reports whether an attributed error can land beside a
// control the deployer can actually see right now. Which controls are
// mounted changes with quick/expert mode and with fields the deploy page
// never gives a control at all (e.g. engine, quick_params_unset) - checking
// the live form registration, rather than hard-coding every such field,
// covers both without drifting out of sync as the form changes.
// controlHasFocus reports whether the deployer is typing in the control of a
// form field: antd's field instance is the input itself (Switch), or carries
// its root as nativeElement (InputNumber, Input) or the input as input.
function controlHasFocus(form: FormInstance, name: string): boolean {
  const active = typeof document === "undefined" ? null : document.activeElement;
  if (!active || active === document.body) return false;
  const instance: unknown = form.getFieldInstance(name);
  if (!instance || typeof instance !== "object") return false;
  const refs = instance as Partial<{ nativeElement: Element; input: Element }>;
  const root =
    typeof Element !== "undefined" && instance instanceof Element
      ? instance
      : (refs.nativeElement ?? refs.input);
  return Boolean(root && root.contains(active));
}

function hasRenderedControl(form: FormInstance, name: string): boolean {
  return (
    listControlFields.has(name) || form.getFieldInstance(name) !== undefined
  );
}

// useDeployEvaluation is the deploy form's whole feedback loop: send the
// serialized draft (debounced), then render the answer. Every judgement lives
// in the response - field errors are attached beside their controls, the
// display instruction lands on the form, and anything the server could not
// attribute to a field becomes the page-level error line.
//
// Stale answers cannot land: the request is aborted and its id compared, and
// a draft revised while a request was in flight wins.
export default function useDeployEvaluation({
  form,
  signal,
  buildPayload,
  translate,
  applyFormValues = () => true,
}: {
  form: FormInstance;
  // The watched form values: one identity per draft, driving the debounce.
  signal: unknown;
  buildPayload: () => Record<string, unknown> | null;
  translate: APIErrorTranslator;
  // Whether the answer's form_values may be written onto the form. A form
  // opened from a deployment's draft already holds the deployment's values,
  // and an answer built from dictionary defaults must not replace them.
  applyFormValues?: () => boolean;
}) {
  const [result, setResult] = useState<EvaluateResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [failure, setFailure] = useState<Error | null>(null);
  const [evaluating, setEvaluating] = useState(false);
  const [reevaluateSeq, setReevaluateSeq] = useState(0);
  // Counts answered evaluations so a report that repeats itself is still acted
  // on: the same moved flags typed again are a new event, not a stale one.
  const [responseSeq, setResponseSeq] = useState(0);
  const abortRef = useRef<AbortController | null>(null);
  const requestIdRef = useRef(0);
  const revisionRef = useRef(0);
  // A changed draft is pending during render, before child passive effects can
  // act on the previous acquisition. Async replies also check this identity.
  const currentSignalRef = useRef(signal);
  const settledSignalRef = useRef(signal);
  currentSignalRef.current = signal;
  const appliedKeysRef = useRef<string[]>([]);
  const quickParamsRef = useRef(new EvaluateQuickParams());
  const flaggedFieldsRef = useRef<string[]>([]);
  // Keys an answer skipped because their control had focus.
  const deferredRef = useRef<string[]>([]);

  const invalidate = (message: string | null) => {
    abortRef.current?.abort();
    requestIdRef.current += 1;
    settledSignalRef.current = signal;
    setResult(null);
    setError(message);
    setFailure(null);
    setEvaluating(false);
  };

  useEffect(() => {
    // Each draft generation invalidates every in-flight answer for the
    // previous one, however the change arrived - a keystroke, a card click or
    // a programmatic setFieldsValue all change the watched values.
    revisionRef.current += 1;
    const revision = revisionRef.current;
    // The answer on screen describes the previous draft from this moment on;
    // report "evaluating" through the debounce so nothing acts on it.
    setEvaluating(true);
    const timer = setTimeout(() => {
      const payload = buildPayload();
      if (!payload) {
        invalidate(null);
        return;
      }
      const overrides = payload.container_overrides as
        | { command_mode?: string }
        | undefined;
      const manual = overrides?.command_mode === "manual";
      const request = manual ? payload : quickParamsRef.current.prepare(payload);
      const unset = new Set((request.quick_params_unset ?? []) as string[]);
      // A user-cleared field remains a tombstone when the server omits it.
      // Clearing it to undefined would let the next template reintroduce it.
      const previousKeys = appliedKeysRef.current.filter((key) => !unset.has(key));
      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;
      const requestId = ++requestIdRef.current;
      setEvaluating(true);
      api
        .evaluate(request, controller.signal)
        .then((response) => {
          if (
            !Object.is(signal, currentSignalRef.current) ||
            controller.signal.aborted ||
            requestId !== requestIdRef.current ||
            revision !== revisionRef.current
          )
            return;
          setResult(response);
          setResponseSeq((seq) => seq + 1);
          setError(null);
          setFailure(null);
          // Field errors belong beside their controls; the codes translate the
          // same way every other product error does. An error the server could
          // not attribute (empty field), or attributed to a field with no
          // rendered control to show it beside, is page-level - never dropped.
          const flagged = new Set<string>();
          const fields: { name: string; errors: string[] }[] = [];
          const unattributed: string[] = [];
          for (const fieldError of response.errors ?? []) {
            const text = apiErrorText(
              translate,
              new APIError(
                fieldError.code,
                fieldError.params ?? {},
                fieldError.code,
                200,
              ),
            );
            const name = fieldError.field
              ? (formFieldName[fieldError.field] ??
                fieldError.field.replace(/^placement\./, ""))
              : "";
            if (name && hasRenderedControl(form, name)) {
              flagged.add(name);
              fields.push({ name, errors: [text] });
            } else {
              unattributed.push(text);
            }
          }
          // A field the previous answer flagged and this one does not is clean
          // again; leaving the old message would accuse a fixed control.
          for (const name of flaggedFieldsRef.current) {
            if (!flagged.has(name)) fields.push({ name, errors: [] });
          }
          flaggedFieldsRef.current = [...flagged];
          if (fields.length) form.setFields(fields);
          if (unattributed.length) setError(unattributed.join(" · "));
          // Manual previews describe their command, not the structured draft
          // restored by explicit discard. Keep its keys and quick-param origins.
          if (!manual && applyFormValues()) {
            quickParamsRef.current.accept(response);
            deferredRef.current = applyEvaluateFormValues(
              form,
              response,
              previousKeys,
              (key) => controlHasFocus(form, key),
            );
            // Early errors contain no display instruction; keep previous keys
            // so the next successful answer can clear them.
            if (response.form_values)
              appliedKeysRef.current = Object.keys(response.form_values);
          }
        })
        .catch((reason: Error) => {
          if (
            !Object.is(signal, currentSignalRef.current) ||
            controller.signal.aborted ||
            requestId !== requestIdRef.current ||
            revision !== revisionRef.current
          )
            return;
          setError(reason.message);
          setFailure(reason);
          setResult(null);
        })
        .finally(() => {
          if (
            Object.is(signal, currentSignalRef.current) &&
            !controller.signal.aborted &&
            requestId === requestIdRef.current &&
            revision === revisionRef.current
          ) {
            settledSignalRef.current = signal;
            setEvaluating(false);
          }
        });
    }, 400);
    return () => clearTimeout(timer);
  }, [signal, reevaluateSeq]);

  // A skipped key takes the server's display value once the deployer leaves
  // its control: evaluate the draft again then, through the ordinary path.
  useEffect(() => {
    if (typeof document === "undefined") return;
    const onFocusOut = () => {
      if (!deferredRef.current.length) return;
      // focusout fires before focus moves; read the new focus next turn.
      setTimeout(() => {
        const still = deferredRef.current.filter((key) => controlHasFocus(form, key));
        if (still.length === deferredRef.current.length) return;
        deferredRef.current = still;
        setReevaluateSeq((seq) => seq + 1);
      }, 0);
    };
    document.addEventListener("focusout", onFocusOut);
    return () => document.removeEventListener("focusout", onFocusOut);
  }, [form]);

  useEffect(() => () => abortRef.current?.abort(), []);

  return {
    result,
    error,
    failure,
    evaluating: evaluating || !Object.is(settledSignalRef.current, signal),
    responseSeq,
    invalidate,
    noteDraftChanged: (changed: Record<string, unknown> = {}) => {
      revisionRef.current += 1;
      quickParamsRef.current.noteChanged(changed);
    },
    reevaluate: () => setReevaluateSeq((seq) => seq + 1),
  };
}
