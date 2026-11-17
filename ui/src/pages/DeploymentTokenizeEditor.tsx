import { message } from "antd";
import { Badge } from "../components/ui/badge";
import { Switch } from "../components/ui/switch";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { useOrder } from "./useOrder";

// TokenizeEditor is the deployment detail's tokenize switch: whether the
// gateway forwards /v1/tokenize and /v1/detokenize. A change takes effect on
// the next request; nothing restarts.
export function TokenizeEditor({
  dep,
  onChanged,
}: {
  dep: Deployment;
  onChanged: () => void;
}) {
  const { t } = useTranslation("common");
  const { busy, run } = useOrder();
  const save = async (on: boolean) => {
    const refused = await run(() => api.setDeploymentTokenize(dep.id, on));
    if (refused !== null) {
      message.error(refused);
      return;
    }
    message.success(t("tokenize.saved"));
    onChanged();
  };
  return (
    <span className="deployment-switch-status">
      <Badge variant="plain" size="sm" status={dep.tokenize_enabled ? "success" : "neutral"}>
        {dep.tokenize_enabled ? t("tokenize.on") : t("tokenize.off")}
      </Badge>
      <Switch
        className="p-0"
        labelVisibility="sr-only"
        checked={dep.tokenize_enabled}
        aria-busy={busy}
        disabled={busy}
        label={t("tokenize.enable")}
        onCheckedChange={(on) => void save(on)}
      />
    </span>
  );
}
