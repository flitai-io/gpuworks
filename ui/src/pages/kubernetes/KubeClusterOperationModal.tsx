import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster, type KubeClusterOperation, type KubeObjectAction, type KubeObjectResult } from "../../api";
import { apiErrorText } from "../../apiError";
import { Badge, type BadgeStatus } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table";
import { Notice } from "../../ui-kit/Notice";

const ACTION_STATUS: Record<KubeObjectAction, BadgeStatus> = {
  create: "success",
  update: "info",
  replace: "warning",
  delete: "danger",
  absent: "neutral",
  failed: "danger",
  not_tried: "neutral",
  dependent: "neutral",
};

type Stage = "checking" | "checked" | "running" | "done";

/**
 * Applies a cluster entry's manifest to its cloud cluster, or removes it,
 * on the operator's confirmation: it first asks the API server to check
 * every change (a dry run), shows what each object would go through, and
 * only then does it.
 */
export default function KubeClusterOperationModal({ cluster, op, onClose, onDone }: {
  cluster: KubeCluster;
  op: "apply" | "remove";
  onClose: () => void;
  onDone?: () => void;
}) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [stage, setStage] = useState<Stage>("checking");
  const [result, setResult] = useState<KubeClusterOperation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let alive = true;
    setStage("checking");
    setResult(null);
    setError(null);
    api.kubeClusterOperation(cluster.uuid, op, true)
      .then((r) => { if (alive) { setResult(r); setStage("checked"); } })
      .catch((e) => { if (alive) { setError(apiErrorText(tCommon, e)); setStage("checked"); } });
    return () => { alive = false; };
  }, [cluster.uuid, op, attempt, tCommon]);

  const run = async () => {
    setStage("running");
    setError(null);
    try {
      const r = await api.kubeClusterOperation(cluster.uuid, op, false);
      setResult(r);
      if (!r.code) onDone?.();
    } catch (e) {
      setError(apiErrorText(tCommon, e));
    } finally {
      setStage("done");
    }
  };

  const failed = Boolean(error || result?.code);
  const source = cluster.source;
  const rows = result?.results ?? [];
  const running = stage === "running";

  const title = t(op === "apply" ? "operation.applyTitle" : "operation.removeTitle", { name: cluster.name });
  const footer = stage === "done" || (stage === "checked" && failed) ? (
    <>
      {stage === "checked" && failed && <Button variant="secondary" onClick={() => setAttempt((n) => n + 1)}>{t("operation.retry")}</Button>}
      <Button variant="primary" onClick={onClose}>{t("operation.close")}</Button>
    </>
  ) : (
    <>
      <Button variant="ghost" onClick={onClose} disabled={running}>{t("operation.cancel")}</Button>
      <Button variant={op === "remove" ? "destructive" : "primary"} disabled={stage !== "checked"} loading={running} onClick={() => void run()}>
        {t(op === "apply" ? "operation.confirmApply" : "operation.confirmRemove")}
      </Button>
    </>
  );

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !running) onClose(); }} disablePointerDismissal>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={running} style={{ width: "min(760px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{title}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 min-w-0 flex-col gap-3 overflow-auto">
          <p>
            {t(op === "apply" ? "operation.applyIntro" : "operation.removeIntro", {
              account: source?.provider_name || source?.provider_kind || "",
              cluster: source?.cluster_id ?? "",
            })}
          </p>
          {stage === "checking" && <Notice tone="info" title={t("operation.checking")} />}
          {stage === "checked" && !failed && <Notice tone="info" title={t(op === "apply" ? "operation.checkedApply" : "operation.checkedRemove")} />}
          {running && <Notice tone="info" title={t(op === "apply" ? "operation.applying" : "operation.removing")} />}
          {stage === "done" && !failed && (
            <Notice tone="info" title={t(op === "apply" ? "operation.applied" : "operation.removed")}>
              {op === "apply" ? t("operation.appliedNext") : undefined}
            </Notice>
          )}
          {failed && (
            <Notice tone="danger" title={result?.params ? t(`operation.failed.${result.params.step}`) : t("operation.failed.other")}>
              {result?.params?.reason ?? error}
            </Notice>
          )}
          {rows.length > 0 && (
            <div className="flex min-w-0 flex-col gap-2">
              <strong className="font-semibold text-fg-default">{t(stage === "done" ? "operation.resultTitle" : "operation.planTitle")}</strong>
              <Table variant="bordered" size="sm">
                <TableHeader>
                  <TableRow>
                    <TableHead style={{ width: 170 }}>{t("operation.column.kind")}</TableHead>
                    <TableHead>{t("operation.column.name")}</TableHead>
                    <TableHead style={{ width: 260 }}>{t("operation.column.action")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((r: KubeObjectResult, index) => (
                    <TableRow key={`${r.kind}/${r.namespace ?? ""}/${r.name}`} index={index}>
                      <TableCell>{r.kind}</TableCell>
                      <TableCell><code>{r.namespace ? `${r.namespace}/${r.name}` : r.name}</code></TableCell>
                      <TableCell>
                        <div className="flex flex-col items-start gap-1">
                          <Badge status={ACTION_STATUS[r.action] ?? "neutral"}>{t(`operation.${stage === "done" ? "done" : "plan"}.${r.action}`, { defaultValue: r.action })}</Badge>
                          {(r.error || r.reason) && <span className={`text-label ${r.error ? "text-fg-danger" : "text-fg-muted"}`}>{r.error || r.reason}</span>}
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </div>
        <DialogFooter>{footer}</DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
