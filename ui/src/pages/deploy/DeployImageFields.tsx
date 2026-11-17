import {
  Alert,
  Button,
  Col,
  Collapse,
  Form,
  Input,
  Row,
  Select,
  Space,
  Tooltip,
  Typography,
} from "antd";
import { useTranslation } from "react-i18next";
import type {
  EngineProfile,
  ImageCatalogEntry,
  ImageSeries,
  Model,
  Node,
  ProductWarning,
} from "../../api";
import { localizedText } from "../../i18n/localizedText";
import { type ImageSelectionMode } from "./deployHelpers";
import { warningText } from "./deployWarnings";
import ContainerOverridesEditor from "./ContainerOverridesEditor";
import { effectiveShmDefaultBytes } from "./shmDefault";

const { Text } = Typography;
type Props = {
  engines: { data?: EngineProfile[] | null; loading: boolean };
  // The cluster default the start timeout field shows while it is empty.
  startTimeoutDefaultSec?: number;
  // The /dev/shm size the selected template spec states (shm_bytes), if any.
  templateShmBytes?: number;
  models: { data?: Model[] | null };
  imageMode: ImageSelectionMode;
  inferenceSeries: ImageSeries[];
  imageSeriesLoading: boolean;
  selectedSeriesID?: number;
  catalogVersions: ImageCatalogEntry[];
  selectedImage?: string;
  autoResolutionText: string;
  selectedModel?: Model;
  chosenCatalogImage?: ImageCatalogEntry;
  runtimeSeries?: ImageSeries;
  compatibilityWarnings: ProductWarning[];
  onlineNodes: Node[];
};

export default function DeployImageFields({
  engines,
  startTimeoutDefaultSec,
  templateShmBytes,
  imageMode,
  inferenceSeries,
  imageSeriesLoading,
  selectedSeriesID,
  catalogVersions,
  selectedImage,
  autoResolutionText,
  chosenCatalogImage,
  compatibilityWarnings,
  runtimeSeries,
}: Omit<Props, "models" | "onlineNodes">) {
  const { t, i18n } = useTranslation("deploy");
  const language = i18n.resolvedLanguage || i18n.language;
  const form = Form.useFormInstance();
  // One subscription for both: neither field need be mounted here.
  const watched = Form.useWatch(
    (values: { engine?: string; command_mode?: string }) => ({
      engine: values.engine,
      commandMode: values.command_mode,
    }),
    { form, preserve: true },
  );
  const watchedEngine = watched?.engine;
  const commandMode = watched?.commandMode;
  const addCommonArg = (flag: string, value?: string) => {
    const token = [flag, value].filter(Boolean).join(" ");
    const current = String(form.getFieldValue("extra_args") || "").trim();
    if (!token || current.split(/\s+/).includes(flag)) return;
    form.setFieldsValue({
      extra_args: current ? `${current} ${token}` : token,
    });
  };
  const addCommonEnv = (key: string, value?: string) => {
    const current = form.getFieldValue("container_env_rows");
    const rows = Array.isArray(current) ? current : [];
    if (rows.some((row: { key?: string }) => row?.key === key)) return;
    form.setFieldsValue({
      container_env_rows: [...rows, { key, value: value || "" }],
    });
  };
  const commonArgSpec = (flag: string) =>
    engines.data
      ?.find((engine) => engine.name === watchedEngine)
      ?.param_specs?.find((spec) => spec.flag === flag);
  return (
    <Row gutter={16}>
      <Col span={12}>
        <Form.Item label={t("image.selection")} name="image_mode">
          <Select
            options={[
              { value: "auto", label: t("image.mode.auto") },
              { value: "catalog", label: t("image.mode.catalog") },
              { value: "custom", label: t("image.mode.custom") },
            ]}
          />
        </Form.Item>
      </Col>
      {imageMode === "catalog" && (
        <>
          <Col span={12}>
            <Form.Item
              label={t("image.series")}
              name="image_series_id"
              rules={[{ required: true, message: t("image.seriesRequired") }]}
            >
              <Select
                placeholder={t("image.seriesPlaceholder")}
                options={inferenceSeries.map((series) => ({
                  value: series.id,
                  label: `${series.name} · ${series.framework || t("image.frameworkUndeclared")}`,
                }))}
                loading={imageSeriesLoading}
              />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              label={t("image.version")}
              name="image_version_id"
              rules={[{ required: true, message: t("image.versionRequired") }]}
            >
              <Select
                disabled={!selectedSeriesID}
                placeholder={t("image.versionPlaceholder")}
                showSearch
                optionFilterProp="label"
                options={catalogVersions.map((image) => ({
                  value: image.id,
                  label: `${image.engine_version ? `v${image.engine_version}` : t("image.engineVersionUndeclared")} · ${image.framework || "framework"} ${image.framework_version || "?"} · ${image.arch || "multi"}`,
                }))}
              />
            </Form.Item>
          </Col>
        </>
      )}
      <Col span={12}>
        <Form.Item
          label={t("image.customRef")}
          name="image"
          extra={
            selectedImage ? t("catalog.imageUnverified") : autoResolutionText
          }
        >
          <Input placeholder="registry.example/team/image:v1" />
        </Form.Item>
      </Col>
      {imageMode === "catalog" && chosenCatalogImage && (
        <Col span={24}>
          <Alert
            type={compatibilityWarnings.length > 0 ? "warning" : "info"}
            showIcon
            message={t("image.manualSelection", {
              image: chosenCatalogImage.image_ref,
            })}
            description={
              compatibilityWarnings.length > 0
                ? compatibilityWarnings
                    .map((warning) => warningText(t, warning))
                    .join("; ")
                : t("image.compatibilityHelp")
            }
          />
        </Col>
      )}
      {(imageMode !== "catalog" || chosenCatalogImage) && (
        <>
          <Col span={24}>
            <Space direction="vertical" size={4} style={{ width: "100%" }}>
              <Text strong>{t("workbench.runtime.title")}</Text>
              <Text type="secondary">{t("workbench.runtime.description")}</Text>
              <ContainerOverridesEditor
                shmDefaultBytes={effectiveShmDefaultBytes(
                  engines.data?.find((engine) => engine.name === watchedEngine)
                    ?.shm_bytes,
                  templateShmBytes,
                )}
                startTimeoutDefaultSec={startTimeoutDefaultSec}
              />
            </Space>
          </Col>
          <Col span={24}>
            <Collapse
              items={[
                {
                  key: "runspec",
                  label: t("catalog.launchOverrides"),
                  children: (
                    <>
                      <Row gutter={12}>
                        <Col span={8}>
                          <Form.Item
                            label={t("image.commandMode")}
                            name="command_mode"
                            extra={t("image.commandModeHelp")}
                          >
                            <Select
                              options={[
                                {
                                  value: "engine",
                                  label: t("image.commandModeOption.engine"),
                                },
                                {
                                  value: "image-entry",
                                  label: t(
                                    "image.commandModeOption.imageEntry",
                                  ),
                                },
                                {
                                  value: "manual",
                                  label: t("image.commandModeOption.manual"),
                                },
                              ]}
                            />
                          </Form.Item>
                        </Col>
                        <Col span={8}>
                          <Form.Item
                            label={t("image.modelMount")}
                            name="container_model_mount_path"
                          >
                            <Input
                              placeholder={t("image.modelMountPlaceholder")}
                            />
                          </Form.Item>
                        </Col>
                        {runtimeSeries &&
                        (runtimeSeries.common_args?.length ||
                          runtimeSeries.common_env?.length) ? (
                          <Col span={24}>
                            <Space
                              direction="vertical"
                              size={4}
                              style={{ width: "100%" }}
                            >
                              {!!runtimeSeries.common_args?.length && (
                                <Space wrap>
                                  <Text type="secondary">
                                    {t("image.commonArgs")}
                                  </Text>
                                  {runtimeSeries.common_args.map((item) => {
                                    const spec = commonArgSpec(item.flag);
                                    const label =
                                      localizedText(spec?.label, language) ||
                                      item.flag;
                                    const title = [
                                      localizedText(spec?.help, language),
                                      spec?.flag && spec.flag !== item.flag
                                        ? spec.flag
                                        : "",
                                    ]
                                      .filter(Boolean)
                                      .join(" · ");
                                    return (
                                      <Tooltip
                                        key={`${item.flag}:${item.value || ""}`}
                                        title={title || undefined}
                                      >
                                        <Button
                                          size="small"
                                          onClick={() =>
                                            addCommonArg(item.flag, item.value)
                                          }
                                        >
                                          {[label, item.value]
                                            .filter(Boolean)
                                            .join(" ")}
                                        </Button>
                                      </Tooltip>
                                    );
                                  })}
                                </Space>
                              )}
                              {!!runtimeSeries.common_env?.length && (
                                <Space wrap>
                                  <Text type="secondary">
                                    {t("image.commonEnv")}
                                  </Text>
                                  {runtimeSeries.common_env.map((item) => (
                                    <Button
                                      key={`${item.key}:${item.value || ""}`}
                                      size="small"
                                      onClick={() =>
                                        addCommonEnv(item.key, item.value)
                                      }
                                    >
                                      {item.key}
                                      {item.value ? `=${item.value}` : ""}
                                    </Button>
                                  ))}
                                </Space>
                              )}
                            </Space>
                          </Col>
                        ) : null}
                        <Col span={12}>
                          <Form.Item
                            label={t("image.entrypointOverride")}
                            name="container_entrypoint"
                          >
                            <Input placeholder={t("image.inheritImage")} />
                          </Form.Item>
                        </Col>
                        <Col span={12}>
                          {/* A manual command is a multi-line script; a
                              single-line input bound to it would drop its
                              newlines on the first keystroke, so in manual
                              mode this control is unbound and read-only. */}
                          <Form.Item
                            key={commandMode === "manual" ? "manual" : "argv"}
                            label={t("image.cmdOverride")}
                            name={
                              commandMode === "manual"
                                ? undefined
                                : "container_cmd"
                            }
                            extra={
                              commandMode === "manual"
                                ? t("image.cmdOverrideManualHelp")
                                : undefined
                            }
                          >
                            <Input
                              disabled={commandMode === "manual"}
                              aria-label={t("image.cmdOverride")}
                              placeholder={t(
                                commandMode === "manual"
                                  ? "image.cmdOverrideManual"
                                  : "image.inheritImage",
                              )}
                            />
                          </Form.Item>
                        </Col>
                      </Row>
                      <Text type="secondary">
                        {t("catalog.launchOverridesHelp")}
                      </Text>
                    </>
                  ),
                },
              ]}
            />
          </Col>
        </>
      )}
      {imageMode === "custom" && selectedImage && (
        <Col span={24}>
          <Alert
            type="warning"
            showIcon
            message={t("image.customImage", { image: selectedImage })}
            description={t("image.customImageDescription")}
          />
        </Col>
      )}
    </Row>
  );
}
