import {
  Col,
  Form,
  Input,
  Row,
  Select,
  Space,
  Tag,
  Typography,
} from "antd";
import type { DetectedTemplateChip, InferenceTemplateIndexItem, Model } from "../../api";
import { useTranslation } from "react-i18next";
import ModelPicker from "./ModelPicker";
import DeployServedNameField from "./DeployServedNameField";
import TemplateRecommendations from "./TemplateRecommendations";
import { LockedGroup } from "./DeployEditContext";

export default function DeployBasicStep({
  templateIndex,
  chips,
  selectedModel,
}: {
  templateIndex: InferenceTemplateIndexItem[];
  /** The card models the online nodes have (GET /nodes/facets). */
  chips: DetectedTemplateChip[];
  selectedModel?: Model;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const scenario = Form.useWatch("scenario", { form, preserve: true });
  const templateID = Form.useWatch("template_id", {
    form,
    preserve: true,
  }) as number | undefined;
  const specName = Form.useWatch("template_spec_name", {
    form,
    preserve: true,
  }) as string | undefined;
  const selectedNodeUUID = Form.useWatch("node_uuid", { form, preserve: true }) as string | undefined;
  const nodeSelectionMode = Form.useWatch("node_selection_mode", { form, preserve: true }) as string | undefined;
  const targetVendor = Form.useWatch("target_vendor", {
    form,
    preserve: true,
  }) as string | undefined;
  const targetVendors = [
    ...new Set(
      [
        ...chips.map((chip) => chip.vendor?.toLowerCase()).filter(Boolean),
        ...templateIndex.flatMap((item) =>
          item.profiles
            .map((profile) => profile.gpus.vendor?.toLowerCase())
            .filter(Boolean),
        ),
        targetVendor,
      ].filter((vendor): vendor is string => Boolean(vendor)),
    ),
  ].sort();

  const applyTemplate = (
    item: InferenceTemplateIndexItem,
    profile: InferenceTemplateIndexItem["profiles"][number],
  ) => {
    const source = profile.source || item.source;
    form.setFieldsValue({
      template_id: item.id,
      template_spec_source: source,
      template_spec_name: profile.name,
      engine: profile.engine,
      scenario: profile.scenario || undefined,
      target_vendor: profile.gpus.vendor,
      image: profile.image || undefined,
    });
  };

  return (
    <div>
      <Typography.Title level={4}>{t("basic.title")}</Typography.Title>
      <LockedGroup controls={["model_id", "template", "target_vendor", "name", "served_name"]}>
      <Row gutter={16}>
        <Col xs={24} md={12}>
          <Form.Item
            label={t("basic.model")}
            name="model_id"
            rules={[{ required: true, message: t("basic.modelRequired") }]}
          >
            <ModelPicker
              selected={selectedModel}
              onChange={() =>
                form.setFieldsValue({
                  template_id: undefined,
                  template_spec_source: undefined,
                  template_spec_name: undefined,
                  // A template's image and engine leave with it; a draft
                  // without a template keeps the deployer's own choice.
                  ...(form.getFieldValue("template_id") !== undefined
                    ? { image: undefined, engine: undefined }
                    : {}),
                })
              }
            />
          </Form.Item>
        </Col>
        <Col xs={24} md={12}>
          <Form.Item
            label={t("basic.targetVendor")}
            name="target_vendor"
            rules={[{ required: true, message: t("basic.targetVendorRequired") }]}
          >
            <Select
              placeholder={t("basic.targetVendorPlaceholder")}
              options={targetVendors.map((vendor) => ({
                value: vendor,
                label: t(`basic.vendor.${vendor}`, {
                  defaultValue: vendor.toUpperCase(),
                }),
              }))}
            />
          </Form.Item>
        </Col>
        <Col span={24}>
          <Form.Item
            label={
              <Space>
                {t("basic.recommendedInferenceTemplate")}
                {templateID && <Tag color="green">{t("basic.selected")}</Tag>}
              </Space>
            }
          >
            <TemplateRecommendations
              nodeUUID={nodeSelectionMode === "auto" ? undefined : selectedNodeUUID}
              model={selectedModel}
              scenario={scenario}
              targetVendor={targetVendor}
              selectedID={templateID}
              selectedSpecName={specName}
              onSelect={applyTemplate}
            />
          </Form.Item>
        </Col>
        <Col xs={24} md={12}>
          <Form.Item
            label={t("basic.name")}
            name="name"
            rules={[{ required: true, whitespace: true, message: t("basic.nameRequired") }]}
          >
            <Input placeholder="my-qwen" />
          </Form.Item>
        </Col>
        <Col xs={24} md={12}>
          <DeployServedNameField model={selectedModel} />
        </Col>
      </Row>
      </LockedGroup>
    </div>
  );
}
