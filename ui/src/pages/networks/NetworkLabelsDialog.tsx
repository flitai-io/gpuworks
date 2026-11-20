import { useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Input } from "../../components/ui/input";
import { formatTime } from "../../i18n/datetime";
import type { NetworkAttachment, NetworkDetail } from "../../networkTypes";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";
import { HelpTip } from "../../ui-kit/HelpTip";
import { FormField } from "../../ui-kit/form/FormField";
import NetworkTargetTable from "./NetworkTargetTable";

type Props = {
  open: boolean;
  onClose: () => void;
  detail: NetworkDetail;
  onDetail: (detail: NetworkDetail) => void;
};

const fieldsFor = (a?: NetworkAttachment | null) => ({
  label_key: a?.label_key ?? "",
  label_value: a?.label_value ?? "",
});

export default function NetworkLabelsDialog({ open, onClose, detail, onDetail }: Props) {
  const { t } = useTranslation("networks");
  const { t: common } = useTranslation("common");
  const labels = useMemo(() => detail.attachments.filter(a => a.target === "label"), [detail.attachments]);
  const [editing, setEditing] = useState<NetworkAttachment | null>(null);
  const [busy, setBusy] = useState(false);
  const pending = useRef(false);
  const [error, setError] = useState("");
  const form = useForm({ defaultValues: fieldsFor(null) });
  const current = editing && labels.find(a => a.id === editing.id);
  const changed = !!editing && (!current || current.label_key !== editing.label_key || current.label_value !== editing.label_value);

  function select(attachment: NetworkAttachment | null) {
    setEditing(attachment);
    setError("");
    form.reset(fieldsFor(attachment));
  }

  async function change(request: () => Promise<NetworkDetail>, success: () => void) {
    if (pending.current) return;
    pending.current = true;
    setBusy(true);
    setError("");
    try {
      const result = await request();
      onDetail(result);
      success();
    } catch (cause) {
      setError(apiErrorText(common, cause));
    } finally {
      pending.current = false;
      setBusy(false);
    }
  }

  const save = form.handleSubmit(values => {
    if (changed) return;
    const body = { label_key: values.label_key.trim(), label_value: values.label_value.trim() };
    return change(() => editing ? api.updateNetworkLabel(detail.id, editing.id, body) : api.attachNetwork(detail.id, body),
      () => select(null));
  });

  return <Dialog open={open} onOpenChange={next => { if (!next && !pending.current) onClose(); }}>
    <DialogContent size="lg" className="max-h-[calc(100dvh-32px)] overflow-y-auto" closeButtonDisabled={busy} closeButtonLabel={common("actions.close")}>
      <DialogHeader><DialogTitle>{t("detail.manageLabels")}</DialogTitle></DialogHeader>
      <div className="flex flex-wrap items-center gap-1 text-label text-fg-muted"><span>{t("detail.directAttachmentCount", { count: detail.attachments.length - labels.length })} · {t("table.attachmentLabels", { count: labels.length })}</span><HelpTip content={t("attach.modeHint")} /></div>
      <form className="flex flex-col gap-3" onSubmit={save}>
        {editing && <div><h3 className="break-all text-body font-semibold">{t("detail.editLabel")} · {editing.label_key}={editing.label_value}</h3>
          <p className="text-label text-fg-muted">{t("detail.labelRematchHint")}</p></div>}
        <div className="grid gap-3 sm:grid-cols-2">
          <FormField control={form.control} name="label_key" label={t("attach.labelKey")} required rules={{ validate: value => !!value.trim() || t("attach.labelKeyRequired") }}>
            {field => <Input {...field} disabled={busy || changed} placeholder="pool" />}
          </FormField>
          <FormField control={form.control} name="label_value" label={t("attach.labelValue")}>
            {field => <Input {...field} disabled={busy || changed} placeholder="h20-a" />}
          </FormField>
        </div>
        {changed && <p role="alert" className="text-label text-fg-danger">{t("detail.labelChanged")}</p>}
        {error && <p role="alert" className="text-label text-fg-danger">{error}</p>}
        <div className="flex justify-end gap-2">
          {editing && <Button variant="secondary" size="sm" disabled={busy} onClick={() => select(null)}>{t("actions.cancel")}</Button>}
          <Button type="submit" size="sm" disabled={busy || changed}>{t("actions.save")}</Button>
        </div>
      </form>
      {labels.length === 0 ? <p className="text-label text-fg-muted">{t("detail.noLabels")}</p> :
        <NetworkTargetTable<NetworkAttachment> networkId={detail.id} kind="attachments" data={labels} rowKey="id"
          searchLabel={t("detail.searchLabels")} tableId="network-attachment-labels" columnSettings={false} columns={[
            { key: "target", title: t("attach.modeLabel"), role: "name", width: 200,
              render: (_, a) => <span className="whitespace-normal break-all">{a.label_key}={a.label_value}</span> },
            { key: "time", title: t("detail.attachedTime"), width: 160, render: (_, a) => formatTime(a.created_at) },
            { key: "actions", role: "actions", width: 140, render: (_, a) => <div className="inline-flex gap-2">
              <Button size="sm" variant="link" disabled={busy} onClick={() => { select(a); form.setFocus("label_key"); }}>{t("actions.edit")}</Button>
              <ConfirmPopover destructive title={t("actions.confirmDetach")} confirmText={t("actions.detach")} cancelText={t("actions.cancel")}
                onConfirm={() => change(() => api.detachNetwork(detail.id, a.id), () => { if (editing?.id === a.id) select(null); })}
                trigger={<Button size="sm" variant="link" disabled={busy} className="text-fg-danger">{t("actions.detach")}</Button>} />
            </div> },
          ]} />}
    </DialogContent>
  </Dialog>;
}
