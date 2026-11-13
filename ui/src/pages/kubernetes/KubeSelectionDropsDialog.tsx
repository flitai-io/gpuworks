import { useTranslation } from "react-i18next";
import type { KubeSelectionDrops } from "../../api";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Notice } from "../../ui-kit/Notice";

/**
 * Asks whether to save a node selection that leaves out nodes which still run
 * instances: the server refused it (KUBE_SELECTION_DROPS_INSTANCES) and lists
 * the nodes and their deployments. Confirming repeats the request with the
 * confirmation; the server decides what is affected, this only shows it.
 */
export default function KubeSelectionDropsDialog({ drops, busy, onCancel, onConfirm }: {
  drops: KubeSelectionDrops;
  busy?: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onCancel(); }} disablePointerDismissal>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={busy} style={{ width: "min(640px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{t("selectionDrops.title")}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 min-w-0 flex-col gap-3 overflow-auto">
          {!drops.verified && <Notice tone="warning" title={t("selectionDrops.unverified")} />}
          <p>{t("selectionDrops.intro")}</p>
          <ul className="flex flex-col gap-2 pl-5" style={{ listStyle: "disc" }}>
            {drops.nodes.map((n) => (
              <li key={n.node_uuid}>
                <strong className="font-semibold text-fg-default">{n.node_name || n.kube_node}</strong>
                {n.kube_node && n.kube_node !== n.node_name && <span className="text-fg-muted"> ({n.kube_node})</span>}
                <div className="text-fg-muted">
                  {n.deployments.length > 0 ? t("selectionDrops.deployments", { names: n.deployments.join(", ") }) : t("selectionDrops.noDeployments")}
                </div>
              </li>
            ))}
          </ul>
          <p className="text-fg-muted">{t("selectionDrops.later")}</p>
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={onCancel} disabled={busy}>{t("selectionDrops.cancel")}</Button>
          <Button variant="destructive" onClick={onConfirm} loading={busy}>{t("selectionDrops.confirm")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
