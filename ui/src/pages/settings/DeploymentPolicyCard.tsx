import SettingsSaveButton from "./SettingsSaveButton";
import { useEffect, useState } from "react";
import { Alert, Card, Form, InputNumber, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { NumberInput } from "../../ui-kit/NumberInput";

const { Text } = Typography;

// The server's bounds for deployment.start_timeout_sec
// (store.DeploymentStartTimeoutSec*); it rejects anything outside them.
const minSeconds = 60;
const maxSeconds = 24 * 60 * 60;
// And for the headroom beside processes outside the platform
// (store.DeploymentForeignHeadroom*Hi); zero is allowed for both.
const maxHeadroomPct = 50;
const maxHeadroomGiB = 64;
const GIB = 1024 ** 3;

// headroomBytes is the floor as the server stores it, or undefined when the
// field does not hold a value in range.
export function headroomBytes(gib: number | null): number | undefined {
  if (gib == null || !Number.isFinite(gib) || gib < 0 || gib > maxHeadroomGiB) return undefined;
  return Math.round(gib * GIB);
}

// DeploymentPolicyCard edits the cluster's instance start timeout (how long a
// started instance may take to pass its health check when its deployment
// states no timeout of its own) and the memory kept free beside processes
// outside the platform on a card shared with them. The PUT names only these
// fields; the server keeps the rest of the deployment policy as stored.
export default function DeploymentPolicyCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [seconds, setSeconds] = useState<number | null>(null);
  // A server that serves no headroom keys is not asked to store any.
  const [hasHeadroom, setHasHeadroom] = useState(false);
  const [headroomPct, setHeadroomPct] = useState<number | null>(null);
  const [headroomGiB, setHeadroomGiB] = useState<number | null>(null);
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    api.settings()
      .then((settings) => {
        const value = Number(settings?.deployment?.start_timeout_sec);
        setSeconds(value > 0 ? value : null);
        const pct = settings?.deployment?.foreign_headroom_pct;
        const floor = settings?.deployment?.foreign_headroom_min_bytes;
        const served = typeof pct === "number" && typeof floor === "number";
        setHasHeadroom(served);
        setHeadroomPct(served ? pct : null);
        setHeadroomGiB(served ? Math.round((floor / GIB) * 100) / 100 : null);
        setLoadError("");
      })
      .catch((error) => setLoadError(apiErrorText(tCommon, error)));
  }, [tCommon]);

  const secondsValid = seconds != null && Number.isInteger(seconds) && seconds >= minSeconds && seconds <= maxSeconds;
  const pctValid = headroomPct != null && Number.isInteger(headroomPct) && headroomPct >= 0 && headroomPct <= maxHeadroomPct;
  const floorBytes = headroomBytes(headroomGiB);
  const headroomValid = !hasHeadroom || (pctValid && floorBytes !== undefined);
  const valid = secondsValid && headroomValid;
  const noHeadroom = hasHeadroom && headroomValid && headroomPct === 0 && floorBytes === 0;
  const save = async () => {
    if (!valid) return;
    setSaving(true);
    try {
      await api.putSetting("deployment", {
        start_timeout_sec: seconds,
        ...(hasHeadroom ? { foreign_headroom_pct: headroomPct, foreign_headroom_min_bytes: floorBytes } : {}),
      });
      message.success(t("deployment.saved"));
    } catch (error) {
      message.error(t("error.save", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card
      title={t("deployment.title")}
      extra={<SettingsSaveButton loading={saving} disabled={!valid} onSave={save} />}
    >
      <Space direction="vertical" size={4} style={{ width: "100%", maxWidth: 720 }}>
        <Text type="secondary">{t("deployment.description")}</Text>
        {loadError && <Alert type="error" showIcon message={loadError} />}
        <Form layout="vertical" style={{ marginTop: 8 }}>
          <Form.Item
            label={t("deployment.startTimeout")}
            htmlFor="deployment-start-timeout"
            extra={t("deployment.startTimeoutHelp", { min: minSeconds, max: maxSeconds })}
            validateStatus={seconds != null && !secondsValid ? "error" : undefined}
            help={seconds != null && !secondsValid ? t("deployment.startTimeoutInvalid", { min: minSeconds, max: maxSeconds }) : undefined}
            style={{ marginBottom: 12 }}
          >
            <InputNumber
              id="deployment-start-timeout"
              step={60}
              value={seconds}
              onChange={(value) => setSeconds(value == null ? null : Number(value))}
              suffix={t("deployment.seconds")}
              style={{ width: 200 }}
            />
          </Form.Item>
        </Form>
        {hasHeadroom && (
          // Composed from ui-kit: the card's older field above is antd, and
          // new antd use is not added (ui/antd-inventory.json).
          <div className="flex flex-col gap-1.5" role="group" aria-labelledby="deployment-foreign-headroom">
            <div className="flex items-center gap-1">
              <span id="deployment-foreign-headroom">{t("deployment.foreignHeadroom")}</span>
              <HelpTip content={t("deployment.foreignHeadroomHelp")} label={t("deployment.foreignHeadroomTipLabel")} />
            </div>
            <div className="flex flex-wrap items-center gap-x-6 gap-y-2">
              <label className="inline-flex items-center gap-2 whitespace-nowrap">
                <span className="text-fg-muted">{t("deployment.foreignHeadroomPct")}</span>
                <NumberInput className="w-24" aria-label={t("deployment.foreignHeadroomPct")} aria-invalid={!headroomValid} value={headroomPct} onChange={setHeadroomPct} />
                <span className="text-fg-muted">%</span>
              </label>
              <label className="inline-flex items-center gap-2 whitespace-nowrap">
                <span className="text-fg-muted">{t("deployment.foreignHeadroomMin")}</span>
                <NumberInput className="w-24" aria-label={t("deployment.foreignHeadroomMin")} aria-invalid={!headroomValid} value={headroomGiB} onChange={setHeadroomGiB} />
                <span className="text-fg-muted">GiB</span>
              </label>
            </div>
            {headroomValid
              ? <span className="text-label text-fg-muted">{t("deployment.foreignHeadroomHint")}</span>
              : <span className="text-label text-fg-danger" role="alert">{t("deployment.foreignHeadroomInvalid", { pctMax: maxHeadroomPct, gibMax: maxHeadroomGiB })}</span>}
            {noHeadroom && <Notice tone="warning" title={t("deployment.warning.noForeignHeadroom")} />}
          </div>
        )}
      </Space>
    </Card>
  );
}
