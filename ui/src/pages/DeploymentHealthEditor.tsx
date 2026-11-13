import { useState } from "react";
import { Space, message } from "antd";
import { NumberInput } from "../ui-kit/NumberInput";
import { Button } from "../components/ui/button";
import { Switch } from "../components/ui/switch";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { HealthTag, healthCheckable } from "../components/DeploymentHealth";
import { useOrder } from "./useOrder";

// HealthCheckEditor is the deployment detail's health check: what it found,
// the switch and the threshold. A change takes effect at the next round of
// checks; nothing restarts.
export function HealthCheckEditor({
  dep,
  onChanged,
}: {
  dep: Deployment;
  onChanged: () => void;
}) {
  const { t } = useTranslation("common");
  const { busy, run } = useOrder();
  const enabled = dep.health.status !== "off";
  const [draft, setDraft] = useState<number | null>();
  const threshold = draft === undefined ? dep.health.threshold_sec : draft;
  const checkable = healthCheckable(dep.operations);
  const save = async (on: boolean, seconds: number) => {
    const refused = await run(() =>
      api.setDeploymentHealthCheck(dep.id, on, seconds),
    );
    if (refused !== null) {
      message.error(refused);
      return;
    }
    setDraft(undefined);
    message.success(t("healthCheck.saved"));
    onChanged();
  };
  return (
    <Space size={8} wrap>
      <span className="deployment-switch-status">
        <HealthTag health={dep.health} detail />
        <Switch
          className="p-0"
          labelVisibility="sr-only"
          checked={enabled}
          aria-busy={busy}
          // Turning off is always possible; on needs a chat or embedding model.
          disabled={busy || (!enabled && !checkable)}
          label={t("healthCheck.enable")}
          title={!checkable ? t("healthCheck.unsupported") : undefined}
          onCheckedChange={(on) => void save(on, dep.health.threshold_sec)}
        />
      </span>
      {enabled && (
        <Space size={4} wrap>
          <label className="deployment-health-threshold inline-flex max-w-full items-center gap-1 whitespace-nowrap">
            <span>{t("healthCheck.threshold")}</span>
            <NumberInput
              size="sm"
              className="w-16 min-w-0 @max-[280px]:px-1"
              aria-label={t("healthCheck.threshold")}
              min={1}
              max={600}
              precision={0}
              value={threshold}
              disabled={busy}
              onChange={setDraft}
            />
            <span>{t("healthCheck.seconds")}</span>
          </label>
          <Space size={4}>
            <Button variant="tertiary" size="xs"
              loading={busy}
              disabled={
                threshold === null || !Number.isInteger(threshold) || threshold < 1 || threshold > 600 || threshold === dep.health.threshold_sec
              }
              onClick={() => threshold !== null && void save(true, threshold)}
            >
              {t("healthCheck.save")}
            </Button>
            {threshold !== dep.health.threshold_sec && <Button variant="tertiary" size="xs" disabled={busy} onClick={() => setDraft(undefined)}>{t("deployments:detail.cancel")}</Button>}
          </Space>
        </Space>
      )}
    </Space>
  );
}
