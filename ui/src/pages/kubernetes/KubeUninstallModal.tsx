import { useTranslation } from "react-i18next";
import type { KubeCluster } from "../../api";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import KubeCommandList from "./KubeCommandList";

/**
 * How to take an entry's agents out of a cluster gpuworks cannot reach: the
 * commands to run there, in order. Nothing is sent from here; the entry is
 * deleted afterwards, once its nodes have gone.
 */
export default function KubeUninstallModal({ cluster, onClose }: { cluster: KubeCluster; onClose: () => void }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} style={{ width: "min(760px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{t("uninstall.title", { name: cluster.name })}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 min-w-0 flex-col gap-3 overflow-auto">
          <p>{t("uninstall.intro")}</p>
          <KubeCommandList commands={cluster.remove_commands ?? []} />
          <p className="text-fg-muted">{t("uninstall.next")}</p>
        </div>
        <DialogFooter>
          <Button variant="primary" onClick={onClose}>{t("uninstall.close")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
