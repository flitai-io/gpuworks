import { Button, Dropdown, Modal, Switch, Tooltip, message } from "antd";
import type { MenuProps } from "antd";
import { DownOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { storageTypeLabel } from "../../storageDisplay";
import { formatBytes } from "../../format";
import { storageTypeInfo, type Storage } from "../../storageTypes";
import { storageDeleteConfirmText } from "./StorageDeleteButton";
import type { useStorageRunner } from "./useStorageRunner";

/**
 * The things an admin can do to one storage. The header keeps the switch and
 * the two everyday actions (scan, edit); the rarer ones (default, type,
 * cache, delete) sit in a "more" menu, each confirmed in a dialog.
 */
export default function StorageDetailActions({ storage, runner, onEdit, onScan, onDeleted }: {
  storage: Storage;
  runner: ReturnType<typeof useStorageRunner>;
  onEdit: () => void;
  onScan: () => void;
  onDeleted: () => void;
}) {
  const { t } = useTranslation("storage");
  const { busy, run, reload } = runner;
  const shared = storageTypeInfo(storage.type).shared;
  const { cache, switchTo } = storageTypeInfo(storage.type);
  const otherLabel = switchTo ? storageTypeLabel(t, switchTo) : "";

  const confirm = (title: string, content: string, okText: string, onOk: () => void, danger = false) =>
    Modal.confirm({ title, content, okText, cancelText: t("actions.cancel"), okButtonProps: { danger }, onOk });
  const deleteText = storageDeleteConfirmText(t, storage);

  const items: MenuProps["items"] = [
    ...(storage.enabled && !storage.is_default ? [{ key: "default", label: t("actions.setDefault") }] : []),
    ...(switchTo ? [{ key: "type", label: t("actions.switchTo", { type: otherLabel }) }] : []),
    ...(cache ? [{ key: "cache", label: t("actions.clearCache") }] : []),
    { type: "divider" as const },
    storage.is_default
      ? {
          key: "delete",
          disabled: true,
          label: (
            <Tooltip title={t("actions.deleteDefaultBlocked")}>
              <span>
                {t("actions.delete")}
                <span style={{ position: "absolute", width: 1, height: 1, padding: 0, margin: -1, overflow: "hidden", clip: "rect(0, 0, 0, 0)", whiteSpace: "nowrap", border: 0 }}>
                  {t("actions.deleteDefaultBlocked")}
                </span>
              </span>
            </Tooltip>
          ),
        }
      : { key: "delete", label: t("actions.delete"), danger: true },
  ];
  const onMenu: MenuProps["onClick"] = ({ key }) => {
    if (key === "delete" && storage.is_default) return;
    switch (key) {
      case "default":
        void run("default", () => api.updateStorage(storage.id, { is_default: true }), t("actions.defaultSet", { name: storage.name }));
        break;
      case "type":
        // Leaving nfs_cache first removes the node caches, which nfs no longer
        // manages; the server refuses the switch while any remain.
        confirm(t("actions.switchConfirm", { type: otherLabel }), t(cache ? "actions.switchFromCacheHelp" : "actions.switchHelp"), t("actions.switch"),
          () => void run("type", async () => {
            if (!cache) return api.updateStorage(storage.id, { type: switchTo });
            try {
              await api.clearStorageCache(storage.id);
              await api.updateStorage(storage.id, { type: switchTo });
            } catch (e) {
              // Some caches may be gone even though the switch did not
              // happen; show what is left.
              reload();
              throw e;
            }
          }, t("actions.switched", { name: storage.name, type: otherLabel })));
        break;
      case "cache":
        confirm(t("actions.clearCacheConfirm"), t("actions.clearCacheHelp"), t("actions.clearCache"),
          () => void run("cache", async () => {
            const result = await api.clearStorageCache(storage.id);
            message.success(t("actions.cacheCleared", { count: result.removed, freed: formatBytes(result.freed_bytes) }));
          }));
        break;
      case "delete":
        confirm(deleteText.title, deleteText.description, deleteText.okText,
          () => void run("delete", () => api.deleteStorage(storage.id), t("actions.deleted", { name: storage.name }), false).then((ok) => { if (ok) onDeleted(); }),
          true);
        break;
    }
  };

  return (
    <>
      <Switch
        aria-label={t("actions.enabledSwitch", { name: storage.name })}
        checked={storage.enabled}
        loading={busy === "enabled"}
        checkedChildren={t("actions.enabled")}
        unCheckedChildren={t("actions.disabled")}
        onChange={(next) => void run("enabled", () => api.updateStorage(storage.id, { enabled: next }), t(next ? "actions.enabledMessage" : "actions.disabledMessage", { name: storage.name }))}
      />
      {shared && <Button type="primary" onClick={onScan}>{t("actions.scan")}</Button>}
      <Button onClick={onEdit}>{t("actions.edit")}</Button>
      <Dropdown menu={{ items, onClick: onMenu }} trigger={["click"]}>
        <Button loading={busy === "default" || busy === "type" || busy === "cache" || busy === "delete"}>
          {t("actions.more")} <DownOutlined />
        </Button>
      </Dropdown>
    </>
  );
}
