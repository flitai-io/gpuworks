import { Modal, Radio, Select, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { WeightSource } from "../../api";
import { productMessageText } from "../../apiError";
import { Button } from "../../components/ui/button";
import { Notice } from "../../ui-kit/Notice";
import { ModelSourceTag } from "../ModelsPageParts";
import type { TemplateSpecSelection } from "./deployHelpers";
import type { SourceTest, WeightModel } from "./useTemplateWeightSource";
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
  sourceTest,
  onRetest,
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
  /** The server's test of the chosen source; registering waits for it to say proceed. */
  sourceTest?: SourceTest;
  onRetest: () => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t, i18n } = useTranslation("deploy");
  const sources = selection?.spec.weight_sources || [];
  const remoteSources = sources.filter((source) => ["huggingface", "modelscope"].includes(source.provider.toLowerCase()));
  const localOnly = sources.length > 0 && remoteSources.length === 0 && sources.some((source) => source.provider.toLowerCase() === "local");
  const hasMatches = matches.length > 0;
  const choosingSource = !hasMatches || useModel === "template";
  const choosingSourceOnly = useModel === "template";
  const usingModel = typeof useModel === "number";
  const ready = (usingModel && matches.some((model) => model.id === useModel)) ||
    (choosingSource && !!choice && !!sourceTest && !sourceTest.testing && !!sourceTest.answer?.proceed);
  const answer = sourceTest && !sourceTest.testing ? sourceTest.answer : undefined;
  const retest = <Button variant="secondary" size="sm" onClick={onRetest}>{t("sourceTest.again")}</Button>;
  const testResult = !choice || !sourceTest ? null
    : sourceTest.testing ? <Notice title={t("sourceTest.testing")} />
    : answer?.proceed ? <Notice title={answer.present ? t("sourceTest.present") : t("sourceTest.served", { host: answer.host })} />
    : <Notice tone="danger" action={retest}
        title={answer?.reason ? productMessageText((key, options) => i18n.t(key, { ns: "common", ...options }), answer.reason) : t("sourceTest.failed", { detail: sourceTest.error })} />;
  const sourceSelect = (<>
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
    {testResult && <div style={{ marginTop: 12 }}>{testResult}</div>}
  </>);
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
