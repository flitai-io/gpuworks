import type { ReactNode } from "react";
import { Form, InputNumber, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { EvaluateResponse } from "../../api";

interface Props {
  result: EvaluateResponse | null;
  multi: boolean;
  tpLabel: ReactNode;
  ppLabel: ReactNode;
  /** Wraps each field, e.g. in a grid column; fields render bare by default. */
  wrap?: (field: ReactNode, key: string) => ReactNode;
  disabled?: boolean;
  /** The platform picks the nodes: it needs the TP to know how many GPUs each rank takes. */
  autoPlaced?: boolean;
}

/**
 * The deployment's tensor and pipeline parallel degrees. Both describe the
 * whole deployment; how many GPUs that puts on each node, and the default a
 * multi-node draft starts from, come from the server's evaluation (its
 * form_values and parallelism summary), never from arithmetic here.
 */
export default function DeployParallelFields({ result, multi, tpLabel, ppLabel, wrap = (field) => field, disabled, autoPlaced }: Props) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const [tp, nodeCount] = Form.useWatch(
    (values) => [values.tp, values.nnodes] as const,
    { form, preserve: true },
  ) ?? [];
  const split = result?.parallelism;
  const tpRequired = multi && autoPlaced;
  const hint = split
    ? multi
      ? t("workbench.parallel.split", { tp: split.tp, pp: split.pp, perNode: split.gpus_per_node })
      : t("workbench.parallel.total", { gpus: split.gpus_per_node })
    : tpRequired
      ? (tp == null || tp === "" ? t("workbench.parallel.tpRequiredAuto") : undefined)
      : multi
        ? t("workbench.parallel.fromNodes")
        : t("workbench.parallel.tpAutomatic");
  return (
    <>
      {wrap(
        <Form.Item
          label={tpLabel}
          name="tp"
          rules={tpRequired ? [{ required: true, message: t("workbench.node.autoGroupRequiresTP") }] : undefined}
          extra={<Typography.Text type="secondary">{hint}</Typography.Text>}
        >
          <InputNumber min={1} disabled={disabled} placeholder={t(tpRequired ? "workbench.parallel.tpRequiredPlaceholder" : "workbench.automatic")} style={{ width: "100%" }} />
        </Form.Item>,
        "tp",
      )}
      {wrap(
        <Form.Item
          label={ppLabel}
          name="pp"
          extra={tpRequired && !split ? t("workbench.parallel.autoRankExample", { nodes: nodeCount || 2 }) : undefined}
        >
          <InputNumber min={1} disabled={disabled} placeholder="1" style={{ width: "100%" }} />
        </Form.Item>,
        "pp",
      )}
    </>
  );
}

/**
 * Across nodes the parallelism decides how many GPUs each node gives, so the
 * multi-node form shows it as its own row with the nodes, above the image and
 * the weights.
 */
export function DeployParallelRow({ result, disabled, autoPlaced }: { result: EvaluateResponse | null; disabled?: boolean; autoPlaced?: boolean }) {
  const { t } = useTranslation("deploy");
  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">{t("workbench.parallel.degrees")}</div>
      <div className="deploy-workbench-body">
        <div className="deploy-resource-grid">
          <DeployParallelFields
            result={result}
            multi
            tpLabel={t("workbench.resource.tp")}
            ppLabel={t("workbench.resource.pp")}
            disabled={disabled}
            autoPlaced={autoPlaced}
          />
        </div>
      </div>
    </div>
  );
}
