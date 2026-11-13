import { formatGPUCapacity } from "../../gpuMemory";
import type { ReactNode } from "react";
import { Form, Typography } from "antd";
import type { Node } from "../../api";
import { useTranslation } from "react-i18next";
import { gpuHealthStatus, gpuStateReason } from "../../components/GPUHealth";
import { cardKey, cardOrdinal, cardPickable } from "../../nodes/cardIdentity";
import { gpuResourceHint, type GPUResourceContext } from "./gpuResourceHint";

export function GPUGrid({
  node,
  automatic,
  resourceContext,
}: {
  node?: Node;
  automatic?: number[];
  resourceContext?: GPUResourceContext;
}) {
  const { t } = useTranslation("deploy");
  const { t: tc } = useTranslation("common");
  const form = Form.useFormInstance();
  const nodeMode =
    Form.useWatch("node_selection_mode", { form, preserve: true }) || "auto";
  const selected = (Form.useWatch("gpu_indexes", { form, preserve: true }) ||
    []) as number[];
  const picked = nodeMode === "auto" ? automatic || [] : selected;
  if (!node?.gpus?.length)
    return (
      <Typography.Text type="secondary">
        {t("workbench.gpu.notReported")}
      </Typography.Text>
    );
  return (
    <div
      className="deploy-gpu-grid"
      role="group"
      aria-label={t("workbench.gpu.selectionAria")}
    >
      {node.gpus.map((gpu) => {
        const numbered = gpu.index >= 0;
        const active = numbered && picked.includes(gpu.index);
        // The server's verdict, not a guess from health: a card held by a
        // process outside the platform is healthy and still takes no work,
        // and one lent to a virtual machine is not faulty. A card with no
        // device number cannot be named to the engine at all.
        const usable = cardPickable(gpu);
        const disabled = nodeMode === "auto" || (!usable && !active);
        const hint = gpuResourceHint(gpu, resourceContext);
        const status = gpuHealthStatus(gpu);
        // Why it cannot be picked: a finding if there is one, else
        // whoever holds the card.
        const health = gpuStateReason(tc, gpu);
        return (
          <button
            type="button"
            key={cardKey(gpu)}
            className={`${active ? "is-picked" : ""}${!usable ? " is-unavailable" : ""}${usable && (status === "Warning" || hint?.unavailable) ? " is-degraded" : ""}`}
            disabled={disabled}
            title={
              !usable
                ? t("workbench.gpu.unavailableHealth", { health })
                : [
                    nodeMode === "auto"
                      ? t("workbench.gpu.schedulerSelected")
                      : t("workbench.gpu.selectIndex", { index: gpu.index }),
                    // Which card the number is: a mixed node's cards differ
                    // in model and memory, and the button shows only its index.
                    t("workbench.gpu.cardFacts", { name: gpu.name.replace(/^NVIDIA\s+/i, ""), memory: formatGPUCapacity(gpu, (bytes) => (bytes / 1024 ** 3).toFixed(0)) }),
                    health && t("workbench.gpu.degradedHealth", { health }),
                    hint?.text,
                  ].filter(Boolean).join(" · ")
            }
            onClick={() =>
              form.setFieldValue(
                "gpu_indexes",
                active
                  ? selected.filter((item) => item !== gpu.index)
                  : [...selected, gpu.index],
              )
            }
          >
            {cardOrdinal(gpu)}
          </button>
        );
      })}
    </div>
  );
}

export function ChoiceCard({
  active,
  disabled,
  title,
  description,
  icon,
  onClick,
}: {
  active?: boolean;
  disabled?: boolean;
  title: string;
  description?: string;
  icon?: ReactNode;
  onClick?: () => void;
}) {
  return (
    <button
      type="button"
      className={`deploy-choice-card${active ? " is-active" : ""}${disabled ? " is-disabled" : ""}`}
      disabled={disabled}
      onClick={onClick}
    >
      {icon && <span className="deploy-choice-icon">{icon}</span>}
      <span>
        <strong>{title}</strong>
        {description && <small>{description}</small>}
      </span>
    </button>
  );
}
