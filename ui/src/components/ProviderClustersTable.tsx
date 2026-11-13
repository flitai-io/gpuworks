import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster, type ProviderCluster } from "../api";
import { apiErrorText } from "../apiError";
import KubeClusterDrawer, { type KubeClusterPrefill } from "../pages/kubernetes/KubeClusterDrawer";
import KubePoolPicker from "../pages/kubernetes/KubePoolPicker";
import KubePoolsSaved from "../pages/kubernetes/KubePoolsSaved";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Tooltip } from "./ui/tooltip";
import { Notice } from "../ui-kit/Notice";
import { Spinner } from "../ui-kit/Spinner";

/** The pools a cluster's row starts with ticked: the ones its entry holds; a cluster with one pool and no entry offers it ticked. */
function initialSelection(c: ProviderCluster): string[] {
  if (c.entry) return c.gpu_pools.filter((p) => p.in_entry).map((p) => p.id);
  return c.gpu_pools.length === 1 ? [c.gpu_pools[0].id] : [];
}

/**
 * A cloud account's managed Kubernetes clusters, read live from the cloud.
 * A cluster has one entry, which holds the GPU node pools ticked in its row:
 * ticking pools and connecting makes the entry, and ticking others later
 * changes the pools it holds. The entry's manifest is then applied by gpuworks
 * on confirmation, or with kubectl.
 */
export default function ProviderClustersTable({ providerId, providerName }: { providerId: number; providerName: string }) {
  const { t } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const [rows, setRows] = useState<ProviderCluster[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [selection, setSelection] = useState<Record<string, string[]>>({});
  const [connecting, setConnecting] = useState<KubeClusterPrefill | null>(null);
  const [saving, setSaving] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState<KubeCluster | null>(null);

  const load = useCallback(() => {
    setRows(null);
    setLoadError(null);
    api.providerClusters(providerId)
      .then((list) => {
        const items = list.items ?? [];
        setRows(items);
        setSelection(Object.fromEntries(items.map((c) => [c.external_id, initialSelection(c)])));
      })
      .catch((e: Error) => { setRows([]); setLoadError(e.message); });
  }, [providerId]);
  useEffect(load, [load]);

  const chosen = (c: ProviderCluster) => selection[c.external_id] ?? [];

  const connect = (c: ProviderCluster) => setConnecting({
    name: c.name,
    pools: c.gpu_pools,
    pool_ids: chosen(c),
    provider_id: providerId,
    provider_cluster_id: c.external_id,
    agent_image_source: c.agent_image_source,
    source_label: `${providerName} · ${c.name} (${c.external_id})`,
  });

  const savePools = async (c: ProviderCluster) => {
    if (!c.entry) return;
    setSaving(c.external_id);
    setSaveError(null);
    setSaved(null);
    try {
      setSaved(await api.setKubeClusterPools(c.entry.uuid, chosen(c)));
      load();
    } catch (e) {
      setSaveError(apiErrorText(tCommon, e));
    } finally {
      setSaving(null);
    }
  };

  const pools = (c: ProviderCluster) => {
    if (!c.entry && !c.running) return <span className="text-fg-muted">{t("clusters.notRunning")}</span>;
    if (c.gpu_pools.length === 0) return <span className="text-fg-muted">{t("clusters.noGPUPools")}</span>;
    const held = c.gpu_pools.filter((p) => p.in_entry).map((p) => p.id);
    const picked = chosen(c);
    const changed = picked.length !== held.length || picked.some((id) => !held.includes(id));
    return (
      <div className="flex flex-col gap-2">
        <KubePoolPicker pools={c.gpu_pools} selected={picked} onChange={(ids) => setSelection((s) => ({ ...s, [c.external_id]: ids }))}
          disabled={saving === c.external_id} label={t("clusters.poolsOf", { name: c.name })} />
        <div className="flex flex-wrap items-center gap-2">
          {c.entry ? (
            <>
              <Badge status="success">{t("clusters.connected")}</Badge>
              <Link to="/kubernetes">{c.entry.name}</Link>
              <Button size="sm" variant="secondary" onClick={() => void savePools(c)} loading={saving === c.external_id}
                disabled={!changed || picked.length === 0}>{t("clusters.savePools")}</Button>
            </>
          ) : (
            <Button size="sm" variant="primary" onClick={() => connect(c)} disabled={picked.length === 0}>{t("clusters.connect")}</Button>
          )}
        </div>
      </div>
    );
  };

  const items = rows ?? [];
  return (
    <>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-fg-muted">{t("clusters.intro")}</span>
        <Button size="sm" variant="secondary" onClick={load} loading={rows === null}>{t("clusters.refresh")}</Button>
      </div>
      {loadError && <div className="mb-3"><Notice tone="danger" title={t("clusters.loadFailed")} children={loadError} /></div>}
      {saveError && <div className="mb-3"><Notice tone="danger" title={t("clusters.saveFailed")} children={saveError} /></div>}
      {saved && <div className="mb-3"><KubePoolsSaved cluster={saved} onApplied={load} /></div>}
      {rows === null && <Spinner label={t("clusters.refresh")} />}
      <Table variant="bordered" size="sm" aria-busy={rows === null}>
        <TableHeader>
          <TableRow>
            <TableHead style={{ width: 240 }}>{t("clusters.column.name")}</TableHead>
            <TableHead style={{ width: 130 }}>{t("clusters.column.region")}</TableHead>
            <TableHead style={{ width: 150 }}>{t("clusters.column.version")}</TableHead>
            <TableHead style={{ width: 170 }}>{t("clusters.column.state")}</TableHead>
            <TableHead>{t("clusters.column.gpuPools")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((c, index) => (
            <TableRow key={c.external_id} index={index}>
              <TableCell>
                <div className="flex min-w-0 flex-col">
                  <span>{c.name}</span>
                  <span className="text-label text-fg-muted">{c.external_id}</span>
                </div>
              </TableCell>
              <TableCell>{c.region}</TableCell>
              <TableCell>{c.version || "—"}</TableCell>
              <TableCell>
                <div className="flex flex-wrap items-center gap-1">
                  <Badge status={c.running ? "success" : "neutral"}>{c.running ? t("clusters.running") : c.state}</Badge>
                  {c.public_api
                    ? <Badge status="info">{t("clusters.publicAPI")}</Badge>
                    : <Tooltip content={t("clusters.privateAPIHint")}><Badge status="warning">{t("clusters.privateAPI")}</Badge></Tooltip>}
                </div>
              </TableCell>
              <TableCell>{pools(c)}</TableCell>
            </TableRow>
          ))}
          {rows !== null && items.length === 0 && (
            <TableRow><TableCell colSpan={5} className="text-center text-fg-muted">{t("clusters.empty")}</TableCell></TableRow>
          )}
        </TableBody>
      </Table>
      <KubeClusterDrawer
        open={connecting !== null}
        prefill={connecting ?? undefined}
        onClose={() => setConnecting(null)}
        onCreated={load}
      />
    </>
  );
}
