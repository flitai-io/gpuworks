import { useTranslation } from "react-i18next";

/** Keep the deployment draft open while the existing model is registered. */
export default function ModelDiskRegistrationLink({ modelId }: { modelId?: number }) {
  const { t } = useTranslation("deploy");
  if (!modelId || !Number.isSafeInteger(modelId) || modelId <= 0) return null;
  return <a href={`/models?detail=${modelId}`} target="_blank" rel="noopener noreferrer">{t("scheduleNote.disk_register_action")}</a>;
}
