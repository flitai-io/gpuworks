import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster } from "../../api";
import CopyButton from "../../components/CopyButton";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Notice } from "../../ui-kit/Notice";
import { Spinner } from "../../ui-kit/Spinner";

/** The cluster's manifest, the YAML `kubectl apply` installs. Mount it while it is open. */
export default function KubeManifestModal({ cluster, onClose }: { cluster: KubeCluster; onClose: () => void }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [yaml, setYaml] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    api.kubeManifest(cluster.uuid)
      .then((text) => { if (alive) setYaml(text); })
      .catch((e: Error) => { if (alive) setError(e.message); });
    return () => { alive = false; };
  }, [cluster.uuid]);
  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} style={{ width: "min(760px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{t("manifest.title", { name: cluster.name })}</DialogTitle></DialogHeader>
        <div className="min-h-0 min-w-0 overflow-auto">
          {error !== null && <Notice tone="danger" title={t("manifest.failed")}>{error}</Notice>}
          {error === null && yaml === null && <Spinner label={tCommon("dataTable.refresh")} />}
          {yaml !== null && <pre tabIndex={0} className="code-block max-h-[60vh] overflow-auto p-4">{yaml}</pre>}
        </div>
        <DialogFooter>
          {yaml !== null && <CopyButton type="default" label={t("manifest.copy")} text={yaml}>{t("manifest.copy")}</CopyButton>}
          <Button variant="primary" onClick={onClose}>{t("manifest.close")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
