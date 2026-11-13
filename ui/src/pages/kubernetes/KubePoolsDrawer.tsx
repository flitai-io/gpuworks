import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster, type ProviderClusterPool } from "../../api";
import { apiErrorText } from "../../apiError";
import { Button } from "../../components/ui/button";
import { Notice } from "../../ui-kit/Notice";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { Spinner } from "../../ui-kit/Spinner";
import KubePoolPicker from "./KubePoolPicker";
import KubePoolsSaved from "./KubePoolsSaved";

/**
 * Edits the node pools an entry made from a cloud cluster holds. The cluster's
 * pools are read live from the cloud account; saving changes the entry, and
 * the cluster follows when the manifest is applied again.
 */
export default function KubePoolsDrawer({ cluster, onClose, onSaved }: { cluster: KubeCluster | null; onClose: () => void; onSaved: () => void }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [pools, setPools] = useState<ProviderClusterPool[] | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState<KubeCluster | null>(null);

  useEffect(() => {
    setPools(null);
    setLoadError(null);
    setError(null);
    setSaved(null);
    if (!cluster?.source) return;
    setSelected(cluster.pool_ids);
    let alive = true;
    api.providerClusters(cluster.source.provider_id)
      .then((list) => {
        if (!alive) return;
        const found = (list.items ?? []).find((c) => c.external_id === cluster.source?.cluster_id);
        if (found) setPools(found.gpu_pools);
        else setLoadError(t("pools.clusterGone"));
      })
      .catch((e) => { if (alive) setLoadError(apiErrorText(tCommon, e)); });
    return () => { alive = false; };
  }, [cluster?.uuid]);

  const save = async () => {
    if (!cluster) return;
    setSaving(true);
    setError(null);
    try {
      setSaved(await api.setKubeClusterPools(cluster.uuid, selected));
      onSaved();
    } catch (e) {
      setError(apiErrorText(tCommon, e));
    } finally {
      setSaving(false);
    }
  };

  const unchanged = !!cluster && selected.length === cluster.pool_ids.length && selected.every((id) => cluster.pool_ids.includes(id));
  return (
    <SideDrawer
      open={cluster !== null}
      onClose={onClose}
      title={t("pools.title", { name: cluster?.name ?? "" })}
      width={520}
      busy={saving}
      footer={saved ? (
        <Button variant="primary" onClick={onClose}>{t("drawer.close")}</Button>
      ) : (
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>{t("drawer.cancel")}</Button>
          <Button variant="primary" onClick={() => void save()} loading={saving} disabled={!pools || selected.length === 0 || unchanged}>{t("pools.save")}</Button>
        </>
      )}
    >
      <div className="flex flex-col gap-4">
        {saved ? <KubePoolsSaved cluster={saved} onApplied={onSaved} /> : (
          <>
            <span className="text-fg-muted">{t("pools.intro")}</span>
            {cluster && cluster.pool_ids.length === 0 && <Notice tone="warning" title={t("pools.legacy")} children={t("pools.legacyHelp", { selector: cluster.node_selector })} />}
            {error && <Notice tone="danger" title={error} />}
            {loadError && <Notice tone="danger" title={t("pools.loadFailed")} children={loadError} />}
            {!pools && !loadError && <Spinner label={t("pools.loading")} />}
            {pools && pools.length === 0 && <span className="text-fg-muted">{t("pools.none")}</span>}
            {pools && pools.length > 0 && <KubePoolPicker pools={pools} selected={selected} onChange={setSelected} disabled={saving} label={t("pools.group")} />}
          </>
        )}
      </div>
    </SideDrawer>
  );
}
