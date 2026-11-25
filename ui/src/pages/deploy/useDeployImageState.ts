import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import type {
  EvaluateResponse,
  ImageCatalogEntry,
  ImageSeries,
  Node,
} from "../../api";

// useDeployImageState answers the runtime-image facts the page renders: the
// evaluated image resolution text, the chosen/evaluated image pair, and the
// inference series list for the selected engine. Every value comes from a
// poller or the evaluate response; nothing is derived client-side.
export default function useDeployImageState({
  images,
  imageSeries,
  evalResult,
  selectedEngine,
  selectedVersionID,
  selectedNode,
}: {
  images: ImageCatalogEntry[] | null | undefined;
  imageSeries: ImageSeries[] | null | undefined;
  evalResult: EvaluateResponse | null;
  selectedEngine: string;
  selectedVersionID?: number;
  selectedNode?: Node;
}) {
  const { t } = useTranslation("deploy");
  const resolvedAutoImage = useMemo(
    () => (images ?? []).find((image) => image.image_ref === evalResult?.image),
    [evalResult?.image, images],
  );
  const resolvedAutoSeries = useMemo(
    () =>
      (imageSeries ?? []).find(
        (series) => series.id === resolvedAutoImage?.series_id,
      ),
    [imageSeries, resolvedAutoImage?.series_id],
  );
  const runtimeImage = useMemo(
    () =>
      (images ?? []).find((image) => image.id === selectedVersionID) ??
      resolvedAutoImage,
    [images, resolvedAutoImage, selectedVersionID],
  );
  const runtimeSeries = useMemo(
    () =>
      (imageSeries ?? []).find(
        (series) => series.id === runtimeImage?.series_id,
      ),
    [imageSeries, runtimeImage?.series_id],
  );
  const autoResolutionText = resolvedAutoImage
    ? t("action.autoImage", {
        image:
          resolvedAutoSeries?.name ||
          resolvedAutoImage.name ||
          resolvedAutoImage.engine,
        version: resolvedAutoImage.engine_version
          ? `v${resolvedAutoImage.engine_version}`
          : "",
        framework: resolvedAutoImage.framework || "framework",
        frameworkVersion:
          resolvedAutoImage.framework_version || t("action.notDetected"),
        nodeFramework:
          selectedNode?.framework?.toUpperCase() || t("action.framework"),
        nodeVersion: selectedNode?.framework_version || t("action.notDetected"),
      })
    : evalResult?.image
      ? t("action.willUseImage", { image: evalResult.image })
      : t("action.resolveAfterSelection");
  const inferenceSeries = useMemo(
    () =>
      (imageSeries ?? []).filter(
        (series) =>
          series.purpose === "inference" &&
          series.enabled &&
          (series.engine === selectedEngine ||
            (selectedEngine === "vllm" && series.engine === "vllm-omni")),
      ),
    [imageSeries, selectedEngine],
  );
  return {
    resolvedAutoImage,
    runtimeImage,
    runtimeSeries,
    autoResolutionText,
    inferenceSeries,
  };
}
