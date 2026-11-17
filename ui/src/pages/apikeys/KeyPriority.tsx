import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { api, type ApiKey, type ApiKeyPriority, type PriorityClasses } from "../../api";
import { apiErrorText } from "../../apiError";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { useIsAdmin } from "../../components/useIsAdmin";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { classLabel } from "../settings/PrioritySettingsCard";

// The empty class name means "the default class" (or, for the ceiling, "the
// key's own class"); the select needs a non-empty value for it.
const NONE = "__none__";

function nameOf(classes: PriorityClasses | null, name: string): string {
  const found = classes?.items.find((c) => c.name === name);
  return found ? classLabel(found) : name;
}

// KeyPriorityDialog binds a key's requests to a priority class; only an
// administrator may. The classes and policies offered are the server's.
function KeyPriorityDialog({ apiKey, classes, open, onClose, onSaved }: {
  apiKey: ApiKey;
  classes: PriorityClasses;
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation("apikeys");
  const { t: tc } = useTranslation("common");
  const [draft, setDraft] = useState<ApiKeyPriority>(apiKey.priority);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (open) {
      setDraft({ ...apiKey.priority, policy: apiKey.priority.policy || classes.default_policy });
      setError(null);
    }
  }, [open, apiKey, classes.default_policy]);
  const classOptions = (noneLabel: string) => [
    { value: NONE, label: noneLabel },
    ...classes.items.map((c) => ({ value: c.name, label: `${classLabel(c)} (${c.value})` })),
  ];
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      await api.setApiKeyPriority(apiKey.prefix, draft);
      onSaved();
    } catch (e) {
      setError(apiErrorText(tc, e));
    } finally {
      setSaving(false);
    }
  };
  const defaultClass = classes.items.find((c) => c.default);
  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) onClose(); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("priority.title", { name: apiKey.name })}</DialogTitle>
        </DialogHeader>
        <div className="flex flex-col gap-3">
          <label className="flex flex-col gap-1">
            <span>{t("priority.class")}</span>
            <OptionSelect aria-label={t("priority.class")} value={draft.class || NONE}
              options={classOptions(t("priority.useDefault", { name: defaultClass ? classLabel(defaultClass) : "" }))}
              onChange={(value) => setDraft({ ...draft, class: value === NONE ? "" : value })} />
          </label>
          <label className="flex flex-col gap-1">
            <span className="inline-flex items-center gap-1">{t("priority.policy")}<HelpTip content={t("priority.policyHelp")} /></span>
            <OptionSelect aria-label={t("priority.policy")} value={draft.policy}
              options={classes.policies.map((p) => ({ value: p, label: t(`priority.policies.${p}`, { defaultValue: p }) }))}
              onChange={(value) => setDraft({ ...draft, policy: value, ceiling: classes.ceiling_policies.includes(value) ? draft.ceiling : "" })} />
          </label>
          {classes.ceiling_policies.includes(draft.policy) && (
            <label className="flex flex-col gap-1">
              <span>{t("priority.ceiling")}</span>
              <OptionSelect aria-label={t("priority.ceiling")} value={draft.ceiling || NONE}
                options={classOptions(t("priority.ownClass"))}
                onChange={(value) => setDraft({ ...draft, ceiling: value === NONE ? "" : value })} />
            </label>
          )}
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

// KeyPriorityCell shows a key's class and policy, with a button that opens
// the binding dialog for an administrator. classes is the server's list; until
// it loads the cell shows the raw names.
export function KeyPriorityCell({ apiKey, classes, onChanged }: {
  apiKey: ApiKey;
  classes: PriorityClasses | null;
  onChanged: () => void;
}) {
  const { t } = useTranslation("apikeys");
  const isAdmin = useIsAdmin();
  const [open, setOpen] = useState(false);
  const p = apiKey.priority ?? { class: "", ceiling: "", policy: "" };
  const label = p.class ? nameOf(classes, p.class) : t("priority.defaultClass");
  const policy = p.policy || classes?.default_policy || "";
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      <span>{label}</span>
      {policy && <span className="text-label text-fg-muted">{t(`priority.policies.${policy}`, { defaultValue: policy })}</span>}
      {isAdmin && !apiKey.revoked_at && classes && (
        <>
          <Button size="sm" variant="ghost" aria-label={t("priority.editAria", { name: apiKey.name })} onClick={() => setOpen(true)}>
            {t("priority.edit")}
          </Button>
          <KeyPriorityDialog apiKey={apiKey} classes={classes} open={open} onClose={() => setOpen(false)}
            onSaved={() => { setOpen(false); onChanged(); }} />
        </>
      )}
    </span>
  );
}

// usePriorityClasses reads the classes once for the page; a failure leaves the
// cells showing names without the edit button.
export function usePriorityClasses(): PriorityClasses | null {
  const [classes, setClasses] = useState<PriorityClasses | null>(null);
  useEffect(() => {
    let live = true;
    api.priorityClasses().then((found) => { if (live) setClasses(found); }, () => {});
    return () => { live = false; };
  }, []);
  return classes;
}
