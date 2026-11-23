import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type ModelCore } from "../../api";
import { apiErrorText } from "../../apiError";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";

export function ModelDeleteDialog({ targets, onClose, onDeleted }: {
  targets: ModelCore[];
  onClose: () => void;
  onDeleted: (ids: number[]) => void;
}) {
  const { t } = useTranslation("models");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState(false);
  const [failures, setFailures] = useState<Map<number, string> | null>(null);
  const single = targets.length === 1;
  const submit = async () => {
    if (busy) return;
    setBusy(true);
    const errors = new Map<number, string>();
    const deleted: number[] = [];
    // Independent deletions share a queue, bounding writes even for a full page.
    const pending = targets.values();
    const removeNext = async () => {
      for (const model of pending) {
        try {
          await api.deleteModel(model.id);
          deleted.push(model.id);
        } catch (cause) {
          errors.set(model.id, apiErrorText(tCommon, cause));
        }
      }
    };
    await Promise.all(Array.from({ length: Math.min(4, targets.length) }, removeNext));
    onDeleted(deleted);
    setBusy(false);
    if (errors.size) {
      setFailures(errors);
    } else {
      onClose();
    }
  };

  return <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
    <DialogContent size="sm" showCloseButton={false}>
      <DialogHeader>
        <DialogTitle>{failures
          ? t("delete.result", {done: targets.length - failures.size, failed: failures.size})
          : single ? t("delete.confirm", {name: targets[0].name || targets[0].source_id}) : t("delete.confirmSelected", {count: targets.length})}</DialogTitle>
        <DialogDescription>{t("delete.listDescription")}</DialogDescription>
      </DialogHeader>
      {(!single || failures) && <ul className="max-h-60 overflow-y-auto break-words">
        {targets.filter((m) => !failures || failures.has(m.id)).map((m) => <li key={m.id}>
          {m.name || m.source_id}{failures && <p className="text-label text-fg-danger">{failures.get(m.id)}</p>}
        </li>)}
      </ul>}
      <DialogFooter>
        <Button variant="ghost" disabled={busy} onClick={onClose}>{t(failures ? "action.close" : "action.cancel")}</Button>
        {!failures && <Button variant="destructive" loading={busy} onClick={() => void submit()}>{t("action.delete")}</Button>}
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}
