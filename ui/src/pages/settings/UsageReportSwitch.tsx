import { useEffect, useState } from "react";
import { Switch } from "../../components/ui/switch";
import { HelpTip } from "../../ui-kit/HelpTip";
import { message } from "../../ui-kit/feedback";
import { useTranslation } from "react-i18next";
import { api, type UsageReportStatus } from "../../api";

// UsageReportSwitch is the user experience improvement program inside
// Version and updates: a switch, its name and a hint on hover, styled like
// the automatic update check switch beside it.
export default function UsageReportSwitch() {
  const { t } = useTranslation("settings");
  const [status, setStatus] = useState<UsageReportStatus | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    // A server that cannot say leaves the switch out rather than guessing.
    api.usageReport().then((next) => { if (!cancelled) setStatus(next); }).catch(() => {});
    return () => { cancelled = true; };
  }, []);

  if (!status) return null;
  const toggle = async (enabled: boolean) => {
    setSaving(true);
    try {
      setStatus(await api.setUsageReport(enabled));
      void message.success(t("usageReport.saved"));
    } catch (err) {
      void message.error(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };
  const hint = status.editable ? t("usageReport.hint") : `${t("usageReport.hint")} ${t("usageReport.readOnly")}`;
  return (
    <div className="inline-flex items-center gap-2">
      <Switch id="usage-report-enabled" label={t("usageReport.title")} className="p-0" checked={status.enabled}
        aria-busy={saving} disabled={saving || !status.editable} onCheckedChange={value => void toggle(value)} />
      <HelpTip content={hint} />
    </div>
  );
}
