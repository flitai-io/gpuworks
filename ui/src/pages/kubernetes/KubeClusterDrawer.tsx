import { useTranslation } from "react-i18next";
import type { KubeCluster } from "../../api";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { useKubeClusterForm, type KubeClusterPrefill } from "./useKubeClusterForm";

export type { KubeClusterPrefill };

/**
 * The cluster form in a drawer: connecting a cluster of a cloud account
 * (prefilled from it), or editing an entry. A local cluster is connected
 * through the Add External Platform dialog.
 */
export default function KubeClusterDrawer({ open, onClose, onCreated, onEdited, prefill, edit }: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
  /** Called when an edit was saved. */
  onEdited?: () => void;
  prefill?: KubeClusterPrefill;
  /** The entry to edit; absent connects a new one. */
  edit?: KubeCluster;
}) {
  const { t } = useTranslation("kubernetes");
  const form = useKubeClusterForm({ open, onCreated, onEdited, onClose, prefill, edit });
  return (
    <SideDrawer open={open} onClose={onClose} title={edit ? t("drawer.editTitle") : t("drawer.title")} width={560} busy={form.saving} footer={form.footer(onClose)}>
      {form.body}
    </SideDrawer>
  );
}
