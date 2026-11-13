import { Tag, Tabs, Typography } from "antd";
import type {
  EngineProfile,
  EvaluateResponse,
  ImageCatalogEntry,
  ImageSeries,
  Model,
  Node,
  PartitionCapability,
  PreflightSummary,
} from "../../api";
import { useTranslation } from "react-i18next";
import DeployImageFields from "./DeployImageFields";
import DeployModelStep from "./DeployModelStep";
import DeployPerformanceStep from "./DeployPerformanceStep";
import DeployResourceStep from "./DeployResourceStep";

// The expert view's tab set. Everything it renders is passed in from the page;
// the component holds no state of its own.
export default function DeployExpertTabs({
  engineProfile,
  startTimeoutDefaultSec,
  templateShmBytes,
  selectedEngine,
  selectedModel,
  selectedNode,
  selectedPartitionCapability,
  engines,
  images,
  imageMode,
  imageSeriesLoading,
  selectedSeriesID,
  selectedVersionID,
  selectedImage,
  inferenceSeries,
  autoResolutionText,
  runtimeSeries,
  evalResult,
  evaluating = false,
}: {
  engineProfile?: EngineProfile;
  startTimeoutDefaultSec?: number;
  // The /dev/shm size the selected template spec states (shm_bytes), if any.
  templateShmBytes?: number;
  selectedEngine: string;
  selectedModel?: Model;
  selectedNode?: Node;
  selectedPartitionCapability?: PartitionCapability;
  engines: { data?: EngineProfile[] | null; loading: boolean };
  images: ImageCatalogEntry[];
  imageMode?: string;
  imageSeriesLoading: boolean;
  selectedSeriesID?: number;
  selectedVersionID?: number;
  selectedImage?: string;
  inferenceSeries: ImageSeries[];
  autoResolutionText: string;
  runtimeSeries?: ImageSeries;
  evalResult: EvaluateResponse | null;
  evaluating?: boolean;
}) {
  const { t } = useTranslation("deploy");
  return (
    <section className="deploy-expert-card">
      <div className="deploy-expert-heading">
        <div>
          <Typography.Title level={4}>{t("expert.allParams")}</Typography.Title>
          <Typography.Text type="secondary">
            {t("expert.dictionaryDescription")}
          </Typography.Text>
        </div>
        <Tag color="blue">
          {t("expert.paramCount", {
            count: engineProfile?.param_specs?.length || 0,
          })}
        </Tag>
      </div>
      <Tabs
        items={[
          {
            key: "model",
            label: t("page.step.model"),
            children: (
              <DeployModelStep
                profile={engineProfile}
                engine={selectedEngine}
                model={selectedModel}
              />
            ),
          },
          {
            key: "performance",
            label: t("page.step.performance"),
            children: (
              <DeployPerformanceStep
                profile={engineProfile}
                engine={selectedEngine}
                model={selectedModel}
              />
            ),
          },
          {
            key: "resource",
            label: t("page.step.resource"),
            children: (
              <DeployResourceStep
                profile={engineProfile}
                eligibleNodes={evalResult?.eligible_nodes}
                selectedNode={selectedNode}
                capability={selectedPartitionCapability}
                preflight={
                  evalResult?.preflight as PreflightSummary | undefined
                }
                result={evalResult}
                evaluating={evaluating}
              />
            ),
          },
          {
            key: "runtime",
            label: t("page.step.runtime"),
            children: (
              <>
                <Typography.Title level={4}>
                  {t("page.step.runtime")}
                </Typography.Title>
                <DeployImageFields
                  engines={engines}
                  startTimeoutDefaultSec={startTimeoutDefaultSec}
                  templateShmBytes={templateShmBytes}
                  imageMode={
                    (imageMode as "auto" | "catalog" | "custom") || "auto"
                  }
                  inferenceSeries={inferenceSeries}
                  imageSeriesLoading={imageSeriesLoading}
                  selectedSeriesID={selectedSeriesID}
                  catalogVersions={images.filter(
                    (image) =>
                      image.series_id === selectedSeriesID &&
                      image.effective_enabled !== false &&
                      !image.deprecated,
                  )}
                  selectedImage={selectedImage}
                  autoResolutionText={autoResolutionText}
                  selectedModel={selectedModel}
                  chosenCatalogImage={images.find(
                    (image) => image.id === selectedVersionID,
                  )}
                  runtimeSeries={runtimeSeries}
                  compatibilityWarnings={evalResult?.warnings ?? []}
                />
              </>
            ),
          },
        ]}
      />
    </section>
  );
}
