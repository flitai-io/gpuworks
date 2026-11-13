import { useState } from "react";
import { message } from "antd";
import type { FormInstance } from "antd";
import { useTranslation } from "react-i18next";
import type { EngineProfile, EvaluateResponse, Model, Node } from "../../api";
import { api } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { weightStorageInvalid } from "./deployWeightChoices";
import { nodeRefusals } from "./deployPlacementRefusal";
import {
  buildPayload,
  deploymentPartitionDraft,
  pdActive,
  submittedImage,
  unitsPlacementAuto,
} from "./deployHelpers";

// useDeploySubmit owns the create: it re-serializes the submitted draft,
// confirms the server's automatic placement (or the manual picks), carries the
// acquisition choices, and gates on the inline preflight for multi-node
// drafts. The create re-plans on the server; nothing here trusts the preview
// beyond the placement the deployer saw.
export default function useDeploySubmit({
  form,
  nodes,
  engineProfile,
  selectedModel,
  evalResult,
  evalErr,
  evaluating,
  onCreated,
}: {
  form: FormInstance;
  nodes: Node[];
  engineProfile?: EngineProfile;
  selectedModel?: Model;
  evalResult: EvaluateResponse | null;
  evalErr: string | null;
  evaluating: boolean;
  onCreated: () => void;
}) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<Error | null>(null);
  // Why an automatic draft has no node to create on, from the evaluation the
  // deployer is looking at.
  const placementRefusedText = () => {
    if (evalResult?.errors?.length) return t("action.evaluationFailed");
    const refusals = nodeRefusals(t, evalResult);
    if (refusals.length > 0) {
      const shown = refusals
        .slice(0, 3)
        .map((r) => (r.reason ? `${r.name}: ${r.reason}` : r.name));
      const more = refusals.length - shown.length;
      const detail =
        shown.join("; ") +
        (more > 0 ? ` ${t("action.noNodeFitsMore", { count: more })}` : "");
      return t("action.noNodeFits", { detail });
    }
    // A selection without a plan cannot be created on. A draft the server did
    // not evaluate (ok false, no candidates: its model or overrides were
    // refused) failed evaluation; one evaluated with no candidates at all has
    // no node that matches it.
    if (evalResult?.selected_node) return t("catalog.evaluationStale");
    if (evalResult && !evalResult.ok) return t("action.evaluationFailed");
    return t("catalog.noMatchingNode");
  };
  const submit = async () => {
    try {
      await form.validateFields();
    } catch (error) {
      // AntD has already placed these validation errors at their fields.
      // The button does not consume a rejected submit promise.
      if (
        error && typeof error === "object" &&
        "errorFields" in error && Array.isArray(error.errorFields) &&
        "values" in error && "outOfDate" in error &&
        typeof error.outOfDate === "boolean"
      ) return;
      throw error;
    }
    if (evaluating || evalErr || !evalResult) {
      message.error(
        t(evaluating ? "action.evaluationPending" : "action.evaluationFailed"),
      );
      return;
    }
    const raw = form.getFieldsValue(true);
    // The storage the user picked is no longer offered: say so and wait for a
    // new choice instead of sending a different one.
    if (
      evalResult.acquisition &&
      weightStorageInvalid(evalResult.acquisition, raw.model_source)
    ) {
      message.error(t("workbench.acquisition.storageGone"));
      return;
    }
    // A PD draft names its nodes per role unit, not through this field.
    const pd = pdActive(raw);
    // A multi-node or PD draft the platform places names no node: the server
    // picks them again when it creates (the evaluation was a preview).
    const unitsAuto = unitsPlacementAuto(raw);
    // The server found nothing to place the group on: its reason is the answer
    // (UNIT_PLACEMENT_NONE and the like), not a generic failure.
    const unplaced = unitsAuto
      ? evalResult.errors?.find((error) => error.field === "placement_mode")
      : undefined;
    if (unplaced) {
      message.error(
        apiErrorText(tCommon, new APIError(unplaced.code, unplaced.params ?? {}, unplaced.code, 200)),
      );
      return;
    }
    // Automatic placement names no node in the form; the server's selection
    // is the node the create confirms.
    const automatic =
      !pd &&
      Number(raw.nnodes || 1) <= 1 &&
      raw.node_selection_mode !== "manual";
    if (
      raw.model_id == null ||
      !raw.name ||
      (!pd &&
        raw.node_selection_mode !== "auto" &&
        Number(raw.nnodes || 1) <= 1 &&
        !raw.node_uuid)
    ) {
      message.error(t("action.requiredFields"));
      return;
    }
    const node = nodes.find((item) => item.uuid === raw.node_uuid);
    const v = deploymentPartitionDraft(raw, node, automatic);
    const body: Record<string, unknown> = {
      ...buildPayload(v, engineProfile, selectedModel?.input_modalities)
        .payload,
      // A manual PD draft states neither (its units name their nodes, and
      // placement_mode "auto" beside units that name nodes would place them
      // again); an automatic one states "auto" in buildPayload.
      ...(pd ? {} : { placement_mode: automatic || unitsAuto ? "auto" : "manual" }),
      name: v.name,
      // Everything the form displayed is confirmed by the user. In auto image
      // mode the displayed ref is the evaluated one, so it travels with the
      // request explicitly: the server treats it as the user's choice instead
      // of re-resolving a default the user never saw change.
      image: submittedImage(v.image, evalResult.image),
    };
    if (automatic) {
      const selected = evalResult.selected_node;
      const plan = evalResult.plan?.plan;
      if (!selected || !plan) {
        // The evaluation is settled (a pending one returned above), so the
        // missing selection is the server's answer: say what it is.
        message.error(placementRefusedText());
        return;
      }
      body.node_uuid = selected.uuid;
      // plan.tp is the size of the GPU group on the node (TP × PP); the
      // tensor parallel degree that takes effect is the evaluation's
      // parallelism, which is the typed TP when one was stated.
      body.placement = {
        ...((body.placement as Record<string, unknown> | undefined) || {}),
        gpus: plan.gpus,
        tp: evalResult.parallelism?.tp ?? plan.tp,
      };
    }
    if (!pd && Number(v.nnodes || 1) > 1) {
      // An automatic group has no node list to count: the preview below is
      // the nodes the server would pick now.
      const selected = Array.isArray(v.distributed_nodes)
        ? v.distributed_nodes
        : [];
      if (!unitsAuto && selected.length !== Number(v.nnodes || 1)) {
        message.error(
          t("action.distributedNodeCount", { count: Number(v.nnodes || 1) }),
        );
        return;
      }
      if (!evalResult.preflight?.ok) {
        message.error(t("action.preflightRequired"));
        return;
      }
    }
    setSubmitting(true);
    setSubmitError(null);
    try {
      await api.createDeployment(body);
      message.success(t("action.created"));
      onCreated();
    } catch (e) {
      setSubmitError(e instanceof Error ? e : new Error(String(e)));
      const detail = apiErrorText(tCommon, e);
      // The served name is the deployer's to change: say so at the field.
      if (e instanceof APIError && e.code === "SERVED_NAME_IN_USE") {
        form.setFields([{ name: "served_name", errors: [detail] }]);
      }
      message.error(t("action.createFailed", { detail }));
    } finally {
      setSubmitting(false);
    }
  };
  return { submit, submitting, submitError };
}
