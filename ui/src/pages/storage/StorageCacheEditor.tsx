import { useEffect, useRef, useState } from "react";
import { Alert, Button, Checkbox, Descriptions, Drawer, Form, Input, Modal, Radio, Space, Typography, message } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { Storage, StorageModelFile, StorageRequest } from "../../storageTypes";
import { storageErrorText } from "../../storageDisplay";
import { formatBytes } from "../../format";
import StorageCachePolicyFields from "./StorageCachePolicyFields";
import { validCacheBytes } from "./storageForm";
import { cacheDiskKnown, cacheOverrideRequest, cachePolicyImpact, type CacheNodeRow } from "./storageCachePolicy";

/** Each opening gets a frozen baseline; polling never replaces an in-progress edit. */
export interface CacheEditorSession {
  kind: "node" | "bulk" | "defaults";
  storage: Storage;
  rows: CacheNodeRow[];
  restore?: boolean;
}

export default function StorageCacheEditor({ session, files, onClose, onSaved }: {
  session: CacheEditorSession;
  files: StorageModelFile[] | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation("storage");
  const { storage, rows, kind } = session;
  const node = kind === "node" ? rows[0] : undefined;
  const defaults = kind === "defaults";
  const initialLimit = defaults ? storage.cache_max_bytes ?? 0 : node?.target?.cache_max_bytes;
  const [limit, setLimit] = useState<number | undefined>(session.restore ? undefined : initialLimit);
  const [changeLimit, setChangeLimit] = useState(kind !== "bulk" || !!session.restore);
  const [pathMode, setPathMode] = useState<"keep" | "inherit" | "custom">("keep");
  const [path, setPath] = useState(defaults ? storage.cache_path ?? "" : node?.target?.cache_path || storage.cache_path || "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const alive = useRef(true);
  // The parent mounts this editor conditionally, so returning from onClose unmounts the whole
  // subtree at once. The Drawer and Modal then never run their close transition, and antd has no
  // chance to hand focus back to the control that opened the editor - it lands on the body
  // instead. Closing is a two-step: flip open to false, then let the transition finish.
  const [closing, setClosing] = useState(false);
  const requestClose = () => setClosing(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const changedPath = defaults ? path.trim() !== (storage.cache_path ?? "") : pathMode !== "keep";
  const impact = cachePolicyImpact(storage, rows, limit, defaults ? limit !== initialLimit : changeLimit, defaults,
    changedPath ? { mode: defaults ? "custom" : pathMode === "inherit" ? "inherit" : "custom", value: path.trim() } : undefined);
  const affected = defaults ? rows.filter((row) => row.active && ((limit !== initialLimit && row.target?.cache_max_bytes == null) || (changedPath && !row.target?.cache_path))) : rows;
  const knownUsage = affected.filter((row) => row.active && row.state !== "offline" && row.cache && !changedPath && (!row.cache_path || row.cache_path === (row.target?.cache_path || storage.cache_path)));
  const usageValue = (field: "used_bytes" | "reclaimable_bytes" | "protected_bytes") => {
    if (!knownUsage.length) return t("cacheManage.unknown");
    const value = formatBytes(knownUsage.reduce((sum, row) => sum + row.cache![field], 0));
    return knownUsage.length === affected.length ? value : t("cacheManage.knownUsage", { value, count: knownUsage.length, total: affected.length });
  };
  const hasChanges = defaults ? limit !== initialLimit || changedPath : kind === "bulk" ? changeLimit || changedPath : limit !== initialLimit || changedPath;
  const validate = () => {
    if (changeLimit && (!validCacheBytes(limit) || (defaults && limit === undefined))) return t("cacheManage.invalidLimit");
    if ((defaults || pathMode === "custom") && (!path.trim().startsWith("/") || path.trim().split("/").includes(".."))) return t("cacheManage.invalidPath");
    return "";
  };
  const save = async () => {
    if (busy) return;
    const invalid = validate();
    if (invalid) { setError(invalid); return; }
    if (!hasChanges) return;
    setBusy(true); setError("");
    try {
      if (defaults) {
        const req: StorageRequest = {};
        if (limit !== initialLimit) req.cache_max_bytes = limit;
        if (changedPath) req.cache_path = path.trim();
        if (storage.updated_at) req.expected_updated_at = storage.updated_at;
        await api.updateStorage(storage.id, req);
      } else {
        await api.updateStorageNodeOverrides(storage.id, cacheOverrideRequest(rows.map((row) => row.node_uuid), kind === "bulk" ? changeLimit : limit !== initialLimit, limit, pathMode, path));
      }
      if (!alive.current) return;
      message.success(t("cacheManage.saved")); onSaved(); onClose();
    } catch (e) {
      if (alive.current) setError(storageErrorText(t, e));
    } finally { if (alive.current) setBusy(false); }
  };
  const title = defaults ? t("cacheManage.defaults") : kind === "bulk" ? t("cacheManage.batchTitle", { count: rows.length }) : t("cacheManage.nodeTitle", { node: node!.node_name });
  const footer = <Space><Button onClick={requestClose}>{t("actions.cancel")}</Button><Button type="primary" loading={busy} disabled={busy || !hasChanges} onClick={() => void save()}>{t("cacheManage.save")}</Button></Space>;
  const content = <Space direction="vertical" size={16} style={{ width: "100%" }}>
    {kind === "bulk" && <><Alert type="info" showIcon message={t("cacheManage.fixedSelection", { count: rows.length })} /><details><summary>{t("cacheManage.selectedNodes")}</summary><Typography.Paragraph>{rows.map((row) => row.node_name).join(", ")}</Typography.Paragraph></details></>}
    {node && !node.active && <Alert type="info" showIcon message={t("cacheManage.inactiveHelp")} />}
    <Form layout="vertical" disabled={busy} style={{ width: "100%" }}>
      <Form.Item label={t(defaults ? "cacheManage.defaultPath" : "form.cachePath")}>
        {!defaults && <Radio.Group aria-label={t("cacheManage.pathMode")} value={pathMode} onChange={(e) => setPathMode(e.target.value)} options={[{ value: "keep", label: t("cacheManage.keep") }, { value: "inherit", label: t("cacheManage.followPath") }, { value: "custom", label: t("cacheManage.customPath") }]} />}
        <Input aria-label={t("cacheManage.pathValue")} value={path} disabled={busy || (!defaults && pathMode !== "custom")} onChange={(e) => setPath(e.target.value)} style={{ marginTop: 8 }} />
        {!defaults && <Typography.Text type="secondary">{t("cacheManage.defaultPathValue", { path: storage.cache_path ?? "—" })}</Typography.Text>}
      </Form.Item>
      {kind === "bulk" && <Checkbox checked={changeLimit} onChange={(e) => setChangeLimit(e.target.checked)}>{t("cacheManage.changeLimit")}</Checkbox>}
      {(kind !== "bulk" || changeLimit) && <StorageCachePolicyFields value={limit} onChange={setLimit} allowInherit={!defaults} defaultBytes={storage.cache_max_bytes ?? 0} label={t(defaults ? "cacheManage.defaultLimit" : "cacheManage.limit")} />}
    </Form>
    <Alert type="info" showIcon message={t("cacheManage.budgetHelp")} />
    <Descriptions bordered size="small" column={2}>
      <Descriptions.Item label={t("cacheManage.used")}>{usageValue("used_bytes")}</Descriptions.Item>
      <Descriptions.Item label={t("cacheManage.protected")}>{usageValue("protected_bytes")}</Descriptions.Item>
      <Descriptions.Item label={t("cacheManage.reclaimable")}>{usageValue("reclaimable_bytes")}</Descriptions.Item>
      {node && <Descriptions.Item label={t("cacheManage.diskFree")}>{cacheDiskKnown(storage, node) && !changedPath ? formatBytes(node.cache_available_bytes ?? 0) : t("cacheManage.unverified")}</Descriptions.Item>}
    </Descriptions>
    <Alert type={impact.over || impact.blocked ? "warning" : "info"} showIcon message={t("cacheManage.impact", impact)} description={<><div>{t("cacheManage.protectedHelp")}</div>{impact.unknown > 0 && <div>{t("cacheManage.unknownImpact", { count: impact.unknown })}</div>}{defaults && <div>{t("cacheManage.defaultImpact", { count: rows.filter((row) => row.active).length - impact.count })}</div>}</>} />
    {changedPath && <Alert type="warning" showIcon message={t("cacheManage.pathChangeHelp")} />}
    {node && <div><Typography.Text strong>{t("cacheManage.cachedModels")}</Typography.Text>{files === null ? <Typography.Paragraph type="secondary">{t("cacheManage.filesUnknown")}</Typography.Paragraph> : <ul>{files.filter((file) => file.cache && file.node_uuid === node.node_uuid).map((file) => <li key={file.id}><Typography.Text>{file.model_name || file.source_id}</Typography.Text>{file.deployment_ids.map((id) => <Link key={id} to={`/deployments?deployment=${id}`} style={{ marginLeft: 8 }}>{t("cacheManage.deployment", { id })}</Link>)}</li>)}</ul>}</div>}
    {error && <Alert type="error" role="alert" showIcon message={error} />}
  </Space>;
  // afterOpenChange also fires for the opening transition, so the parent is only told to drop
  // this editor once a close is actually in progress.
  const closed = (shown: boolean) => { if (!shown && closing && alive.current) onClose(); };
  return kind === "node"
    ? <Drawer open={!closing} title={title} width={560} onClose={requestClose} afterOpenChange={closed} footer={footer}>{content}</Drawer>
    : <Modal open={!closing} title={title} width={680} onCancel={requestClose} afterOpenChange={closed} footer={footer}>{content}</Modal>;
}
