import { useState } from "react";
import { Button } from "../../components/ui/button";
import { Tooltip } from "../../components/ui/tooltip";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../../components/ui/dialog";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { Spinner } from "../../ui-kit/Spinner";
import { Notice } from "../../ui-kit/Notice";
import { message } from "../../ui-kit/feedback";
import { fromAntd } from "../../ui-kit/antdIcon";
import CopyButton from "../../components/CopyButton";
import { BatchFacts } from "./BatchFacts";
import { DownloadOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { BATCH_POLL_MS, batchApi, batchFileURL, isBatchActive, type Batch, type BatchFileKind } from "../../batchApi";
import { formatTimeSec } from "../../i18n/datetime";
import { batchErrorText } from "./batchErrors";
import { useModelStateText } from "./BatchModelField";
import { BatchProgress, BatchStatusTag } from "./batchDisplay";
import { useBatchPolling } from "./useBatchPolling";

const TIME_FIELDS = [
  "created_at", "in_progress_at", "finalizing_at", "completed_at", "failed_at", "expired_at", "cancelling_at", "cancelled_at", "expires_at",
] as const;

/** The file ids by kind; a file that was never produced, or was deleted at the server's output_expires_at, has none to download. */
function fileAvailable(batch: Batch, kind: BatchFileKind, now = Date.now()): { available: boolean; expired: boolean } {
  if (kind === "input") return { available: !!batch.input_file_id, expired: false };
  const id = kind === "output" ? batch.output_file_id : batch.error_file_id;
  const expiresAt = batch.output_expires_at ? new Date(batch.output_expires_at).getTime() : Number.POSITIVE_INFINITY;
  const expired = !!id && now >= expiresAt;
  return { available: !!id && !expired, expired };
}

function DownloadButton({ batch, kind }: { batch: Batch; kind: BatchFileKind }) {
  const { t } = useTranslation("batches");
  const label = t(`detail.download.${kind}`);
  const { available, expired } = fileAvailable(batch, kind);
  // The session cookie authenticates a plain link, as it does for storage file downloads.
  if (available) return <a className="inline-flex items-center gap-2 rounded-lg border border-border px-3 py-2 text-fg-default" href={batchFileURL(batch.id, kind)} download><DownloadOutlined />{label}</a>;
  return (
    <Tooltip content={expired ? t("detail.download.expired") : t("detail.download.unavailable")}>
      <span><Button variant="secondary" leadingIcon={fromAntd(DownloadOutlined)} disabled>{label}</Button></span>
    </Tooltip>
  );
}

function Metadata({ metadata }: { metadata: Batch["metadata"] }) {
  const { t } = useTranslation("batches");
  const entries = Object.entries(metadata ?? {});
  if (entries.length === 0) return <span className="text-fg-muted">{t("detail.noMetadata")}</span>;
  return (
    <BatchFacts items={entries.map(([key, value]) => ({ key, label: key, value: value }))} />
  );
}

/**
 * WaitingAlert says why requests of a running batch are being retried, per
 * model: its state now in words, how many requests wait and since when, and the
 * gateway's last answer as the technical detail. The state is the server's.
 */
function WaitingAlert({ waiting }: { waiting: NonNullable<Batch["waiting"]> }) {
  const { t } = useTranslation("batches");
  const stateText = useModelStateText();
  return (
    <Notice
      tone="warning"
      title={t("detail.waiting.title")}
      children={
        <ul className="batch-waiting">
          {waiting.map((wait) => (
            <li key={wait.model}>
              <div>
                {t("detail.waiting.line", {
                  model: wait.model,
                  state: wait.state ? stateText(wait.state) : t("detail.waiting.gone"),
                  count: wait.requests,
                  since: formatTimeSec(wait.since),
                })}
              </div>
              {wait.detail && (
                <span className="text-fg-muted">
                  {wait.status > 0 ? t("detail.waiting.answer", { status: wait.status, detail: wait.detail }) : wait.detail}
                </span>
              )}
            </li>
          ))}
        </ul>
      }
    />
  );
}

function BatchBody({ batch, canManage, onChanged }: { batch: Batch; canManage: boolean; onChanged: () => void }) {
  const { t } = useTranslation("batches");
  const { t: tCommon } = useTranslation("common");
  const [actionError, setActionError] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const counts = batch.request_counts;
  const pending = Math.max(0, counts.total - counts.completed - counts.failed);
  const ended = !isBatchActive(batch.status);

  const cancel = async () => {
    setActionError(null);
    setCancelling(true);
    try {
      await batchApi.cancel(batch.id);
      setConfirming(false);
      message.success(t("detail.cancelled", { id: batch.id }));
      onChanged();
    } catch (e) {
      setConfirming(false);
      setActionError(t("detail.cancelFailed", { detail: batchErrorText(t, tCommon, e).message }));
      // A 409 means the batch just ended: show what it is now.
      onChanged();
    } finally { setCancelling(false); }
  };

  return (
    <div className="flex min-w-0 flex-col gap-6">
      {actionError && <Notice tone="danger" title={actionError} action={<Button variant="ghost" onClick={() => setActionError(null)}>{tCommon("actions.close")}</Button>} />}
      {batch.status === "cancelling" && <Notice tone="info" title={t("detail.cancelPending")} />}
      {batch.waiting && batch.waiting.length > 0 && <WaitingAlert waiting={batch.waiting} />}
      <section aria-label={t("detail.sections.summary")}>
        <BatchFacts
          title={t("detail.sections.summary")}
          action={canManage && batch.cancellable && (
            <Button variant="destructive" onClick={() => setConfirming(true)}>{t("action.cancel")}</Button>
          )}
          items={[
            { key: "id", label: t("detail.id"), value: <span className="inline-flex items-center gap-2"><code>{batch.id}</code><CopyButton text={batch.id} label={tCommon("copy.code")} /></span> },
            { key: "status", label: t("detail.status"), value: <BatchStatusTag status={batch.status} /> },
            { key: "endpoint", label: t("detail.endpoint"), value: <code>{batch.endpoint}</code> },
            ...(batch.model ? [{ key: "model", label: t("detail.model"), value: <code>{batch.model}</code> }] : []),
            { key: "window", label: t("detail.completionWindow"), value: batch.completion_window },
            ...(batch.input_filename ? [{ key: "file", label: t("detail.inputFilename"), value: batch.input_filename }] : []),
            { key: "owner", label: t("detail.owner"), value: batch.user_id },
            { key: "apiKey", label: t("detail.apiKey"), value: batch.api_key_id },
          ]}
        />
      </section>
      {batch.failure && (
        <Notice
          tone="danger"
              title={t("detail.sections.failure")}
          children={<><div>{batch.failure.message}</div><span className="text-fg-muted">{t("detail.failureCode")}: {batch.failure.code}</span></>}
        />
      )}
      <section aria-label={t("detail.sections.counts")}>
        <BatchFacts
          title={t("detail.sections.counts")}
          items={[
            { key: "total", label: t("detail.total"), value: counts.total },
            { key: "completed", label: t("detail.completed"), value: counts.completed },
            { key: "failed", label: t("detail.failed"), value: counts.failed },
            ...(!ended ? [{ key: "pending", label: <Tooltip content={t("detail.pendingHint")}><span tabIndex={0}>{t("detail.pending")}</span></Tooltip>, value: pending }] : []),
            { key: "progress", label: t("columns.progress"), value: <BatchProgress batch={batch} /> },
          ]}
        />
      </section>
      <section aria-label={t("detail.sections.times")}>
        <BatchFacts
          title={t("detail.sections.times")}
          items={TIME_FIELDS.filter((field) => batch[field]).map((field) => ({
            key: field, label: t(`detail.times.${field}`), value: formatTimeSec(batch[field]),
          }))}
        />
      </section>
      <section aria-label={t("detail.sections.metadata")}>
        <h3 className="mb-3 text-body font-semibold">{t("detail.sections.metadata")}</h3>
        <Metadata metadata={batch.metadata} />
      </section>
      <section aria-label={t("detail.sections.files")}>
        <h3 className="mb-3 text-body font-semibold">{t("detail.sections.files")}</h3>
        <div className="flex flex-wrap gap-2">
          <DownloadButton batch={batch} kind="input" />
          <DownloadButton batch={batch} kind="output" />
          <DownloadButton batch={batch} kind="error" />
        </div>
        {batch.output_expires_at && (
          <p className="mt-2 text-fg-muted">
            {t("detail.download.hint", { time: formatTimeSec(batch.output_expires_at) })}
          </p>
        )}
      </section>
      <Dialog open={confirming} onOpenChange={next => { if (!cancelling) setConfirming(next); }} disablePointerDismissal>
        <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={cancelling}>
          <DialogHeader><DialogTitle>{t("action.cancelTitle")}</DialogTitle></DialogHeader>
          <p>{t("action.cancelBody")}</p>
          <DialogFooter>
            <Button variant="ghost" disabled={cancelling} onClick={() => setConfirming(false)}>{t("action.cancelKeep")}</Button>
            <Button variant="destructive" loading={cancelling} onClick={() => void cancel()}>{t("action.cancelOk")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/**
 * BatchDetailDrawer shows one batch: its figures, every timestamp it reached,
 * metadata, the files to download and, for an admin while it can still be
 * stopped, the cancel action behind an in-page confirmation. It refreshes
 * itself while the batch is moving.
 */
export default function BatchDetailDrawer({ id, canManage, pollMs = BATCH_POLL_MS, onClose, onChanged }: {
  id: string | null;
  canManage: boolean;
  pollMs?: number;
  onClose: () => void;
  /** Called when the batch may have changed, so the list follows. */
  onChanged: () => void;
}) {
  const { t } = useTranslation("batches");
  const detail = usePolling<Batch | null>(() => (id ? batchApi.get(id) : Promise.resolve(null)), 0, [id]);
  const batch = detail.data && detail.data.id === id ? detail.data : null;
  useBatchPolling(!!batch && isBatchActive(batch.status), () => { void detail.refresh(); }, pollMs);

  const changed = () => {
    void detail.refresh();
    onChanged();
  };
  let body;
  if (!id) body = null;
  else if (!batch && detail.error) {
    body = (
      <Notice
        tone="danger"
          title={t("page.detailLoadFailed")}
        children={detail.error.message}
        action={<Button variant="secondary" onClick={() => void detail.refetch()}>{t("action.retry")}</Button>}
      />
    );
  } else if (!batch) body = <Spinner label={t("create.dictionary.loading")} />;
  else body = <BatchBody batch={batch} canManage={canManage} onChanged={changed} />;

  return (
    <SideDrawer open={!!id} width={640} title={id ? t("detail.title", { id }) : null} onClose={onClose}>
      {body}
    </SideDrawer>
  );
}
