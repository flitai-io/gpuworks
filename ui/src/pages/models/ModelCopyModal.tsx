import { useEffect, useMemo, useState } from "react";
import { Alert, Button, Form, InputNumber, Modal, Select, Spin, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Model } from "../../api";
import { LIVE_AGENT_STATES } from "../../peerSync";
import NodePicker from "../../nodes/NodePicker";
import { storageErrorText, storageTypeLabel } from "../../storageDisplay";
import { canWriteStorage, isModelFileReady, storageTypeInfo, type ModelLocationRef, type Storage } from "../../storageTypes";
import { preferredWritable, useStorages } from "./useStorages";
import { nodeDisplayName } from "../../nodeDisplay";

interface CopySource {
  key: string;
  storageId: number;
  nodeUuid?: string;
  label: string;
}

/** The locations a model can be copied from: its finished node files (not caches) and its finished copy in each shared storage. */
function copySources(model: Model, storages: Storage[]): CopySource[] {
  const fallbackLocal = storages.find((s) => s.type === "local" && s.is_default) ?? storages.find((s) => s.type === "local");
  const out: CopySource[] = [];
  for (const row of model.nodes ?? []) {
    if (row.cache || !isModelFileReady(row.state)) continue;
    const storage = storages.find((s) => s.id === row.storage_id) ?? (row.storage_id ? undefined : fallbackLocal);
    const storageId = row.storage_id ?? storage?.id;
    if (!storageId) continue;
    const node = nodeDisplayName({ uuid: row.node_uuid, name: row.node_name });
    out.push({ key: `n:${storageId}:${row.node_uuid}`, storageId, nodeUuid: row.node_uuid, label: `${row.storage_name || storage?.name || ""} · ${node}`.replace(/^ · /, "") });
  }
  for (const row of model.storages ?? []) {
    if (!isModelFileReady(row.state)) continue;
    out.push({ key: `s:${row.storage_id}`, storageId: row.storage_id, label: row.storage_name });
  }
  return out;
}

/**
 * Copies a model's files from one location to another. A location is a
 * storage, plus a node when the storage is local. The server picks how the
 * bytes travel; here the person only says from where, to where, and how fast.
 */
export function ModelCopyModal({ model, onClose, onDone }: { model: Model | null; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation("models");
  const { storages, loading, error, reload } = useStorages(!!model);
  const [sourceKey, setSourceKey] = useState<string>();
  const [targetStorageId, setTargetStorageId] = useState<number>();
  const [targetNode, setTargetNode] = useState<string>();
  const [rateMiB, setRateMiB] = useState<number | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);

  const sources = useMemo(() => (model ? copySources(model, storages) : []), [model, storages]);
  useEffect(() => {
    setSourceKey(sources[0]?.key);
    setTargetStorageId(preferredWritable(storages)?.id);
    setTargetNode(undefined);
    setRateMiB(null);
    setStartError(null);
    // Only a different model or a fresh storage list starts the form over; a
    // background refresh of the model's rows must not wipe the person's choices.
  }, [model?.id, storages]);

  const source = sources.find((s) => s.key === sourceKey);
  const target = storages.find((s) => s.id === targetStorageId);
  const targetShared = storageTypeInfo(target?.type).shared;
  const defaultLocalId = (storages.find((s) => s.type === "local" && s.is_default) ?? storages.find((s) => s.type === "local"))?.id;
  const holders = new Set((model?.nodes ?? [])
    .filter((row) => !row.cache && (row.storage_id ?? defaultLocalId) === targetStorageId && row.state !== "failed" && row.state !== "insufficient_disk_space")
    .map((row) => row.node_uuid));
  // A node already holding the model there, or the source itself, is no target.
  const notTarget = (uuid: string) => holders.has(uuid) || uuid === source?.nodeUuid;
  const sourceIsShared = !!source && !source.nodeUuid;
  const sameShared = sourceIsShared && targetShared && source.storageId === targetStorageId;
  const ready = !!source && !!target && !sameShared && (targetShared || !!targetNode);

  const submit = async () => {
    if (!model || !source || !target) return;
    const from: ModelLocationRef = { storage_id: source.storageId, ...(source.nodeUuid ? { node_uuid: source.nodeUuid } : {}) };
    const to: ModelLocationRef = { storage_id: target.id, ...(targetShared ? {} : { node_uuid: targetNode }) };
    setSubmitting(true);
    setStartError(null);
    try {
      await api.copyModel(model.id, { source: from, target: to, ...(rateMiB && rateMiB > 0 ? { rate_limit_kib_s: Math.round(rateMiB * 1024) } : {}) });
      message.success(t("copy.started"));
      onDone();
    } catch (e) {
      setStartError(t("copy.failed", { detail: storageErrorText(t, e) }));
    } finally {
      setSubmitting(false);
    }
  };

  const way = !source || !target ? null : !sourceIsShared && !targetShared ? "copy.wayDirect" : "copy.wayServerPicks";
  return <Modal title={t("copy.title", { name: model?.name || model?.source_id || "" })} open={!!model} onCancel={onClose} onOk={() => void submit()} confirmLoading={submitting} okText={t("copy.start")} cancelText={t("action.cancel")} okButtonProps={{ disabled: !ready }} destroyOnHidden>
    {loading ? <div style={{ textAlign: "center", padding: 16 }}><Spin /></div>
      : error ? <Alert type="error" showIcon message={t("copy.storageLoadFailed", { detail: error.message })} action={<Button size="small" onClick={reload}>{t("action.retry")}</Button>} />
      : sources.length === 0 ? <Alert type="warning" showIcon message={t("copy.noSources")} />
      : <Form layout="vertical">
        <Form.Item label={t("copy.source")}>
          <Select aria-label={t("copy.source")} style={{ width: "100%" }} placeholder={t("copy.sourcePlaceholder")} value={sourceKey} onChange={setSourceKey} options={sources.map((s) => ({ value: s.key, label: s.label }))} />
        </Form.Item>
        <Form.Item label={t("copy.targetStorage")}>
          <Select
            aria-label={t("copy.targetStorage")}
            style={{ width: "100%" }}
            placeholder={t("copy.targetStoragePlaceholder")}
            value={targetStorageId}
            onChange={(id) => { setTargetStorageId(id); setTargetNode(undefined); }}
            options={storages.map((s) => {
              const same = !!source && !source.nodeUuid && source.storageId === s.id;
              const note = !canWriteStorage(s) ? ` ${t("copy.readOnly")}` : same ? ` ${t("copy.sameAsSource")}` : "";
              return { value: s.id, disabled: !canWriteStorage(s) || same, label: `${s.name} · ${storageTypeLabel(t, s.type)}${note}` };
            })}
          />
        </Form.Item>
        {target && !targetShared && <Form.Item label={t("copy.targetNode")}>
          {/* Nodes with a live agent, searched on the server: the fleet is not listed whole. */}
          <NodePicker
            aria-label={t("copy.targetNode")}
            style={{ width: "100%" }}
            placeholder={t("copy.targetNodePlaceholder")}
            value={targetNode}
            onChange={(next) => setTargetNode(Array.isArray(next) ? next[0] : next)}
            filter={{ state: LIVE_AGENT_STATES }}
            optionDisabled={(n) => notTarget(n.uuid)}
            notFoundContent={t("copy.noTargetNodes")}
          />
        </Form.Item>}
        <Form.Item label={t("copy.rateLimit")} extra={t("copy.rateHelp")}>
          <InputNumber aria-label={t("copy.rateLimit")} min={0} value={rateMiB} onChange={setRateMiB} style={{ width: 160 }} />
        </Form.Item>
        {way && <Typography.Paragraph type="secondary" role="status">{t(way)}</Typography.Paragraph>}
        {startError && <Alert type="error" showIcon message={startError} />}
      </Form>}
  </Modal>;
}
