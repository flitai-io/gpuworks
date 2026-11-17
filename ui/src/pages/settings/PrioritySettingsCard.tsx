import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { api, type PriorityClass, type PriorityClasses } from "../../api";
import { apiErrorText } from "../../apiError";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Container, ContainerBody, ContainerHeader } from "../../components/ui/container";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Input } from "../../components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table";
import { useIsAdmin } from "../../components/useIsAdmin";
import { confirm, message } from "../../ui-kit/feedback";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { NumberInput } from "../../ui-kit/NumberInput";

export const PRIORITY_ANCHOR = "request-priority";

// classLabel is how a class reads in the console: its display name, else its name.
export function classLabel(c: Pick<PriorityClass, "name" | "display_name">): string {
  return c.display_name || c.name;
}

interface Draft {
  name: string;
  display_name: string;
  description: string;
  value: number | null;
}

// ClassDialog creates a class, or edits one (editing set). A built-in class
// keeps its name and value; the server refuses either and says so.
function ClassDialog({ open, editing, bounds, onClose, onSaved }: {
  open: boolean;
  editing: PriorityClass | null;
  bounds: Pick<PriorityClasses, "min_value" | "max_value">;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation("settings");
  const { t: tc } = useTranslation("common");
  const [draft, setDraft] = useState<Draft>({ name: "", display_name: "", description: "", value: null });
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (!open) return;
    setError(null);
    setDraft(editing
      ? { name: editing.name, display_name: editing.display_name, description: editing.description, value: editing.value }
      : { name: "", display_name: "", description: "", value: 0 });
  }, [open, editing]);
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      if (editing) {
        await api.updatePriorityClass(editing.name, {
          display_name: draft.display_name, description: draft.description,
          ...(editing.builtin ? {} : { value: draft.value ?? 0 }),
        });
      } else {
        await api.createPriorityClass({ name: draft.name.trim(), display_name: draft.display_name, description: draft.description, value: draft.value ?? 0 });
      }
      onSaved();
    } catch (e) {
      setError(apiErrorText(tc, e));
    } finally {
      setSaving(false);
    }
  };
  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) onClose(); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{editing ? t("priority.editTitle", { name: editing.name }) : t("priority.createTitle")}</DialogTitle>
        </DialogHeader>
        <div className="flex flex-col gap-3">
          <label className="flex flex-col gap-1">
            <span>{t("priority.name")}</span>
            <Input aria-label={t("priority.name")} value={draft.name} disabled={!!editing} placeholder="gold"
              onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
            {!editing && <span className="text-label text-fg-muted">{t("priority.nameHelp")}</span>}
          </label>
          <label className="flex flex-col gap-1">
            <span>{t("priority.displayName")}</span>
            <Input aria-label={t("priority.displayName")} value={draft.display_name}
              onChange={(e) => setDraft({ ...draft, display_name: e.target.value })} />
          </label>
          <label className="flex flex-col gap-1">
            <span>{t("priority.value")}</span>
            <NumberInput aria-label={t("priority.value")} value={draft.value} min={bounds.min_value} max={bounds.max_value} precision={0}
              disabled={editing?.builtin} onChange={(value) => setDraft({ ...draft, value })} />
            <span className="text-label text-fg-muted">
              {editing?.builtin ? t("priority.builtinValue") : t("priority.valueHelp", { min: bounds.min_value, max: bounds.max_value })}
            </span>
          </label>
          <label className="flex flex-col gap-1">
            <span>{t("priority.description")}</span>
            <Input aria-label={t("priority.description")} value={draft.description}
              onChange={(e) => setDraft({ ...draft, description: e.target.value })} />
          </label>
          {error && <Notice tone="danger" title={error} />}
        </div>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose}>{tc("actions.cancel")}</Button>
          <Button variant="primary" loading={saving} onClick={save}>{t("priority.save")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// PrioritySettingsCard lists the request priority classes, most important
// first, and lets an administrator add, edit and delete them and choose the
// default (the class of a request whose key names none). Everything it shows
// and allows comes from the server; it judges nothing.
export default function PrioritySettingsCard() {
  const { t } = useTranslation("settings");
  const { t: tc } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [data, setData] = useState<PriorityClasses | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [dialog, setDialog] = useState<{ open: boolean; editing: PriorityClass | null }>({ open: false, editing: null });
  const load = useCallback(() => {
    api.priorityClasses().then(
      (found) => { setData(found); setLoadError(null); },
      (e: unknown) => setLoadError(apiErrorText(tc, e)),
    );
  }, [tc]);
  useEffect(load, [load]);
  const act = async (run: () => Promise<unknown>, done: string) => {
    try {
      await run();
      message.success(done);
      load();
    } catch (e) {
      message.error(apiErrorText(tc, e));
    }
  };
  const remove = (c: PriorityClass) => confirm({
    title: t("priority.deleteConfirm", { name: classLabel(c) }),
    okText: t("priority.delete"),
    okButtonProps: { danger: true },
    onOk: () => act(() => api.deletePriorityClass(c.name), t("priority.deleted")),
  });
  return (
    <Container id={PRIORITY_ANCHOR}>
      <ContainerHeader>
        <span className="inline-flex items-center gap-1 font-semibold">
          {t("priority.title")}
          <HelpTip content={t("priority.help")} label={t("priority.helpLabel")} />
        </span>
        {isAdmin && <Button variant="primary" size="sm" onClick={() => setDialog({ open: true, editing: null })}>{t("priority.create")}</Button>}
      </ContainerHeader>
      <ContainerBody>
        {loadError && <Notice tone="danger" title={loadError} action={<Button size="sm" variant="secondary" onClick={load}>{tc("actions.retry")}</Button>} />}
        {!isAdmin && <p className="mb-2 text-label text-fg-muted">{t("priority.adminOnly")}</p>}
        <Table aria-label={t("priority.title")}>
          <TableHeader>
            <TableRow>
              <TableHead>{t("priority.class")}</TableHead>
              <TableHead>{t("priority.value")}</TableHead>
              <TableHead>{t("priority.description")}</TableHead>
              {isAdmin && <TableHead>{tc("resourceTable.actions")}</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {(data?.items ?? []).map((c) => (
              <TableRow key={c.name} data-testid={`priority-class-${c.name}`}>
                <TableCell>
                  <span className="inline-flex flex-wrap items-center gap-2">
                    <span>{classLabel(c)}</span>
                    {c.display_name && <code className="text-label text-fg-muted">{c.name}</code>}
                    {c.default && <Badge status="info">{t("priority.default")}</Badge>}
                    {c.builtin && <Badge>{t("priority.builtin")}</Badge>}
                  </span>
                </TableCell>
                <TableCell>{c.value}</TableCell>
                <TableCell>{c.description || "—"}</TableCell>
                {isAdmin && (
                  <TableCell>
                    <span className="inline-flex flex-wrap gap-1">
                      <Button size="sm" variant="ghost" onClick={() => setDialog({ open: true, editing: c })}>{t("priority.edit")}</Button>
                      {!c.default && (
                        <Button size="sm" variant="ghost" onClick={() => act(() => api.setDefaultPriorityClass(c.name), t("priority.defaultSet", { name: classLabel(c) }))}>
                          {t("priority.makeDefault")}
                        </Button>
                      )}
                      {c.deletable && (
                        <Button size="sm" variant="ghost" onClick={() => remove(c)}>{t("priority.delete")}</Button>
                      )}
                    </span>
                  </TableCell>
                )}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ContainerBody>
      {data && (
        <ClassDialog open={dialog.open} editing={dialog.editing} bounds={data}
          onClose={() => setDialog({ open: false, editing: null })}
          onSaved={() => { setDialog({ open: false, editing: null }); message.success(t("priority.saved")); load(); }} />
      )}
    </Container>
  );
}
