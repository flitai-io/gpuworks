import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster } from "../../api";
import { apiErrorText } from "../../apiError";
import { useCopy } from "../../components/CopyButton";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { confirm, message } from "../../ui-kit/feedback";
import RowActions, { type RowAction } from "../platforms/RowActions";
import KubeClusterDrawer from "./KubeClusterDrawer";
import KubeClusterOperationModal from "./KubeClusterOperationModal";
import KubeManifestModal from "./KubeManifestModal";
import KubeUninstallModal from "./KubeUninstallModal";

/**
 * What an administrator does to a cluster entry: editing it; for an entry
 * made from a cloud account's cluster, having gpuworks apply or remove the
 * manifest; where gpuworks cannot remove it, the commands that uninstall it by
 * hand; for every entry, the kubectl command, the manifest, revoking the
 * enrollment token and deleting the entry. The common ones stay in the row
 * (editing, and applying for a cloud entry or copying the command for one
 * made by hand); the rest fold into a menu. An entry that still has nodes
 * cannot be deleted: its confirmation says so and offers the way to
 * uninstall it first.
 */
export default function KubeEntryActions({ cluster, onChanged }: { cluster: KubeCluster; onChanged: () => void }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [copy, copyHolder] = useCopy();
  const [manifest, setManifest] = useState(false);
  const [editing, setEditing] = useState(false);
  const [uninstalling, setUninstalling] = useState(false);
  const [operation, setOperation] = useState<"apply" | "remove" | null>(null);

  const run = async (action: () => Promise<unknown>, success: string) => {
    try {
      await action();
      message.success(success);
      onChanged();
    } catch (e) {
      // The dialog stays open on the reason, so it can be retried or cancelled.
      throw new Error(`${t("actions.failed")}: ${apiErrorText(tCommon, e)}`);
    }
  };
  const blocked = cluster.operation_blocked ? t(`blocked.${cluster.operation_blocked}`) : "";
  // gpuworks cannot remove the entry from its cluster: the server gives the commands that do it by hand.
  const byHand = !cluster.can_remove && (cluster.remove_commands?.length ?? 0) > 0;

  const confirmDelete = () => {
    const dialog = confirm({
      title: t("actions.deleteConfirm", { name: cluster.name }),
      okText: t("actions.delete"),
      cancelText: t("actions.cancel"),
      okButtonProps: { danger: true },
      content: (
        <div className="flex flex-col items-start gap-2">
          <p className="m-0 text-fg-muted">{t("actions.deleteHelp")}</p>
          {cluster.node_count > 0 && (
            <>
              <p className="m-0 text-fg-danger">{t("actions.deleteHasNodes", { count: cluster.node_count })}</p>
              {(cluster.can_remove || byHand) && (
                <button type="button" className="text-fg-brand hover:underline" onClick={() => { dialog.destroy(); if (cluster.can_remove) setOperation("remove"); else setUninstalling(true); }}>
                  {t(cluster.can_remove ? "actions.remove" : "actions.uninstall")}
                </button>
              )}
            </>
          )}
        </div>
      ),
      onOk: () => run(() => api.deleteKubeCluster(cluster.uuid), t("actions.deleted", { name: cluster.name })),
    });
  };
  const confirmRevoke = () => confirm({
    title: t("actions.revokeConfirm", { name: cluster.name }),
    okText: t("actions.revoke"),
    cancelText: t("actions.cancel"),
    okButtonProps: { danger: true },
    content: <p className="m-0 text-fg-muted">{t("actions.revokeHelp")}</p>,
    onOk: () => run(() => api.revokeKubeCluster(cluster.uuid), t("actions.revoked", { name: cluster.name })),
  });

  const actions: RowAction[] = [
    { key: "edit", label: t("actions.edit"), icon: ConsoleIcon.edit, common: true, onSelect: () => setEditing(true) },
  ];
  if (cluster.source) {
    actions.push(
      { key: "apply", label: t("actions.apply"), icon: ConsoleIcon.upload, common: true, disabled: !cluster.can_apply, hint: blocked, onSelect: () => setOperation("apply") },
      { key: "remove", label: t("actions.remove"), icon: ConsoleIcon.unlink, danger: true, disabled: !cluster.can_remove, hint: blocked, onSelect: () => setOperation("remove") },
    );
  }
  if (cluster.apply_command) {
    const command = cluster.apply_command;
    actions.push({ key: "copy", label: t("actions.copyCommand"), icon: ConsoleIcon.copy, common: !cluster.source, disabled: cluster.revoked,
      onSelect: () => void copy(command, { success: t("actions.commandCopied") }) });
  }
  actions.push({ key: "manifest", label: t("actions.manifest"), icon: ConsoleIcon.manifest, onSelect: () => setManifest(true) });
  if (byHand) actions.push({ key: "uninstall", label: t("actions.uninstall"), icon: ConsoleIcon.terminal, danger: true, onSelect: () => setUninstalling(true) });
  actions.push(
    { key: "revoke", label: t("actions.revoke"), icon: ConsoleIcon.block, danger: true, disabled: cluster.revoked, onSelect: confirmRevoke },
    { key: "delete", label: t("actions.delete"), icon: ConsoleIcon.delete, danger: true, onSelect: confirmDelete },
  );

  return (
    <>
      <RowActions actions={actions} label={tCommon("resourceTable.actions")} />
      {copyHolder}
      {editing && <KubeClusterDrawer key={cluster.uuid} open edit={cluster} onClose={() => setEditing(false)} onCreated={onChanged} onEdited={onChanged} />}
      {uninstalling && <KubeUninstallModal cluster={cluster} onClose={() => setUninstalling(false)} />}
      {manifest && <KubeManifestModal cluster={cluster} onClose={() => setManifest(false)} />}
      {operation && <KubeClusterOperationModal cluster={cluster} op={operation} onClose={() => setOperation(null)} onDone={onChanged} />}
    </>
  );
}
