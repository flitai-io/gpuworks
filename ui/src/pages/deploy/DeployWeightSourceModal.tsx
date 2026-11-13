import { Modal, Radio, Select, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { WeightSource } from "../../api";
import { ModelSourceTag } from "../ModelsPageParts";
import type { TemplateSpecSelection } from "./deployHelpers";
import type { WeightModel } from "./useTemplateWeightSource";
import ModelRegistrationDetails from "./ModelRegistrationDetails";

export default function DeployWeightSourceModal({
  open,
  loading,
  selection,
  choice,
  matches = [],
  useModel,
  onUseModel,
  onChoice,
  onCancel,
  onConfirm,
}: {
  open: boolean;
  loading: boolean;
  selection?: TemplateSpecSelection;
  choice?: WeightSource;
  /** Registered models whose repo id the template declares; asked to confirm when any. */
  matches?: WeightModel[];
  useModel?: number | "template";
  onUseModel?: (value: number | "template") => void;
  onChoice: (value: WeightSource) => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t } = useTranslation("deploy");
  const sources = selection?.spec.weight_sources || [];
  const remoteSources = sources.filter((source) => ["huggingface", "modelscope"].includes(source.provider.toLowerCase()));
  const localOnly = sources.length > 0 && remoteSources.length === 0 && sources.some((source) => source.provider.toLowerCase() === "local");
  const hasMatches = matches.length > 0;
  const choosingSource = !hasMatches || useModel === "template";
  const choosingSourceOnly = useModel === "template";
  const usingModel = typeof useModel === "number";
  const ready = (usingModel && matches.some((model) => model.id === useModel)) ||
    (choosingSource && !!choice && remoteSources.some((source) => JSON.stringify(source) === JSON.stringify(choice)));
  const sourceSelect = (
    <Select
      value={choice ? JSON.stringify(choice) : undefined}
      onChange={(value) => onChoice(JSON.parse(value) as WeightSource)}
      style={{ width: "100%" }}
      placeholder={t("action.selectSourcePlaceholder")}
      aria-label={t("action.selectSourcePlaceholder")}
      options={remoteSources.map((source) => ({
        value: JSON.stringify(source),
        label: `${source.provider === "modelscope" ? t("action.modelScope") : t("action.huggingFace")} · ${source.repo_id}`,
      }))}
    />
  );
  return (
    <Modal
      open={open}
      title={hasMatches ? t("weightSourceModal.confirmTitle") : t("action.selectWeightSource")}
      okText={localOnly || (hasMatches && !choosingSourceOnly) ? t("weightSourceModal.useModel") : t("action.registerAndDeploy")}
      cancelText={t("shared.cancel")}
      confirmLoading={loading}
      okButtonProps={{ disabled: !ready }}
      onCancel={onCancel}
      onOk={onConfirm}
    >
      {hasMatches ? (
        <>
          <Typography.Paragraph>{t(localOnly ? "weightSourceModal.localDescription" : "weightSourceModal.confirmDescription")}</Typography.Paragraph>
          <Radio.Group
            value={useModel}
            onChange={(event) => onUseModel?.(event.target.value as number | "template")}
            style={{ width: "100%" }}
          >
            <Space direction="vertical" style={{ width: "100%" }}>
              {matches.map((model) => (
                <Radio key={model.id} value={model.id}>
                  <Space size={8} wrap>
                    <Typography.Text strong>{model.name || model.source_id}</Typography.Text>
                    <ModelSourceTag source={model.source} />
                    <Typography.Text type="secondary">{model.source_id}</Typography.Text>
                    <ModelRegistrationDetails model={model} />
                  </Space>
                </Radio>
              ))}
              {remoteSources.length > 0 && <Radio value="template">{t("weightSourceModal.registerFromTemplate")}</Radio>}
            </Space>
          </Radio.Group>
          {useModel === "template" && <div style={{ marginTop: 12 }}>{sourceSelect}</div>}
        </>
      ) : (
        <>
          <Typography.Paragraph>{t(localOnly ? "weightSourceModal.localMissing" : "action.selectWeightSourceDescription")}</Typography.Paragraph>
          {!localOnly && sourceSelect}
        </>
      )}
    </Modal>
  );
}
