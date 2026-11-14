// Advanced/expert parameter fold: dictionary fields not on the quick
// surface, the custom-args textarea, and the selected spec's residual
// template arguments (read-only).
import { Collapse, Form, Input, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { EngineParamSpec } from "../../api";
import DeployParamField from "./DeployParamField";
import { HelpTip } from "../../ui-kit/HelpTip";

type Props = {
  advancedSpecs: EngineParamSpec[];
  manualCommand: boolean;
  templateArgs: string[];
};

export default function DeployAdvancedSection({
  advancedSpecs,
  manualCommand,
  templateArgs,
}: Props) {
  const { t } = useTranslation("deploy");
  return (
    <Collapse
      className="deploy-advanced-collapse"
      ghost
      items={[
        {
          key: "advanced",
          label: t("workbench.advanced.title"),
          forceRender: true,
          children: (
            <div className="deploy-advanced-content">
              {advancedSpecs.length > 0 && (
                <div className="deploy-resource-grid">
                  {advancedSpecs.map((spec) => (
                    <DeployParamField key={spec.key} spec={spec} />
                  ))}
                </div>
              )}
              <Form.Item
                label={t("advanced.customArgs")}
                name="extra_args"
                extra={<span className="inline-flex items-center gap-1">{t("launchConfig.extraArgsLine")}<HelpTip content={t("launchConfig.extraArgsHelp")} /></span>}
              >
                <Input.TextArea
                  autoSize={{ minRows: 2, maxRows: 8 }}
                  disabled={manualCommand}
                />
              </Form.Item>
              {templateArgs.length > 0 && (
                <div className="deploy-template-args">
                  <Typography.Text strong>
                    {t("launchConfig.templateArgs")}
                  </Typography.Text>
                  <div className="deploy-template-args-list">
                    {templateArgs.map((arg, index) => (
                      <Tag key={`${index}-${arg}`}>{arg}</Tag>
                    ))}
                  </div>
                  <Typography.Text type="secondary">
                    {t("launchConfig.templateArgsHint")}
                  </Typography.Text>
                </div>
              )}
            </div>
          ),
        },
      ]}
    />
  );
}
