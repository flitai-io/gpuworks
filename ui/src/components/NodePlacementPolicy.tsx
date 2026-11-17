import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type Node, type NodePlacementPolicySetting, type NodePlacementPolicyValue } from "../api";
import { Alert, AlertDescription, AlertTitle } from "./ui/alert";
import { Skeleton } from "./ui/skeleton";
import { Switch } from "./ui/switch";

/**
 * NodePlacementPolicy edits a node's placement policy: the per-node knobs on
 * how the scheduler may place work there. Which knobs exist, their kind and
 * default come from the server's dictionary (GET
 * /scheduling/node-placement-policies), and whether each takes effect on this
 * node from the node itself, so a knob the server adds needs only its
 * label and help (nodes:placementPolicy.<key>.*). A knob that has no effect
 * on the node now can still be set; the server says why it does not apply.
 */
export default function NodePlacementPolicy({ node, onUpdated }: { node: Node; onUpdated: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const [settings, setSettings] = useState<NodePlacementPolicySetting[] | null>(null);
  const [loadError, setLoadError] = useState<Error | null>(null);
  const [values, setValues] = useState<Record<string, NodePlacementPolicyValue>>(node.placement_policy ?? {});
  const [saving, setSaving] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.nodePlacementPolicyDictionary()
      .then((dictionary) => { if (!cancelled) setSettings(dictionary); })
      .catch((error) => { if (!cancelled) setLoadError(error as Error); });
    return () => { cancelled = true; };
  }, []);
  useEffect(() => { setValues(node.placement_policy ?? {}); }, [node.placement_policy]);

  if (loadError) {
    return (
      <Alert status="danger">
        <AlertTitle>{t("placementPolicy.loadFailed")}</AlertTitle>
        <AlertDescription>{loadError.message}</AlertDescription>
      </Alert>
    );
  }
  if (!settings) return <Skeleton className="h-6 w-full" data-testid="placement-policy-loading" />;
  if (settings.length === 0) return <p className="text-sm text-fg-muted">{t("placementPolicy.empty")}</p>;

  const save = async (key: string, value: NodePlacementPolicyValue) => {
    const previous = values;
    setValues({ ...values, [key]: value });
    setSaving(key);
    setSaveError(null);
    try {
      onUpdated(await api.setNodePlacementPolicy(node.uuid, { [key]: value }));
    } catch (error) {
      setValues(previous);
      setSaveError(t("placementPolicy.saveFailed", { detail: (error as Error).message }));
    } finally {
      setSaving(null);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      {saveError && <Alert status="danger"><AlertTitle>{saveError}</AlertTitle></Alert>}
      {settings.map((setting) => {
        const value = values[setting.key] ?? setting.default;
        const availability = node.placement_policy_availability?.find((item) => item.key === setting.key);
        const label = t(`placementPolicy.${setting.key}.label`, { defaultValue: setting.key });
        return (
          <div key={setting.key} className="flex flex-col gap-1">
            {setting.kind === "bool" ? (
              <Switch
                label={label}
                checked={value === true}
                disabled={saving === setting.key}
                onCheckedChange={(checked) => void save(setting.key, checked)}
              />
            ) : (
              <span>{label}: <code>{String(value)}</code></span>
            )}
            <p className="text-sm text-fg-muted">{t(`placementPolicy.${setting.key}.help`, { defaultValue: "" })}</p>
            {availability && !availability.applicable && (
              <Alert status="warning">
                <AlertTitle>
                  {t("placementPolicy.notApplicable", {
                    reason: t(`placementPolicy.reason.${availability.reason}`, { defaultValue: availability.reason ?? "" }),
                  })}
                </AlertTitle>
              </Alert>
            )}
          </div>
        );
      })}
    </div>
  );
}
