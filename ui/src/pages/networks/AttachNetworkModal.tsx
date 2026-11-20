import { Combobox, ComboboxInput, ComboboxContent, ComboboxList, ComboboxItem } from "../../components/ui/combobox";
import { useForm } from "react-hook-form";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../../components/ui/dialog";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { FormField } from "../../ui-kit/form/FormField";
import { message } from "../../ui-kit/feedback";
import { useEffect, useMemo, useState } from "react";

import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import NodePicker from "../../nodes/NodePicker";
import { useNodeLookup } from "../../nodes/useNodeLookup";
import { nodeDisplayName } from "../../nodeDisplay";
import type { NetworkAttachment, NetworkDetail, NodeNetworks } from "../../networkTypes";
import { isAcceleratorRDMA, usesRails } from "./networkDisplay";

interface FormValues {
  node_uuids?: string[];
  interface?: string;
  ip?: string;
}

/**
 * Attaches a network to nodes (several at once) or, with
 * `attachment` set, edits that node attachment's address override: the data
 * address on Ethernet, the bootstrap address on RDMA. An RDMA network's rails
 * are edited in the node table, not here.
 */
export default function AttachNetworkModal({
  open, network, attachment, onClose, onSaved,
}: {
  open: boolean;
  network: NetworkDetail;
  attachment?: NetworkAttachment | null;
  onClose: () => void;
  onSaved: (detail: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  const { t: tCommon } = useTranslation("common");
  const form = useForm<FormValues>();
  const [inventory, setInventory] = useState<NodeNetworks | null>(null);
  const [saving, setSaving] = useState(false);
  const editing = !!attachment;
  const rdma = usesRails(network.type);

  useEffect(() => {
    if (!open) return;
    form.reset(attachment
      ? { node_uuids: attachment.node_uuid ? [attachment.node_uuid] : [], interface: attachment.interface, ip: attachment.ip }
      : { node_uuids: [] });
  }, [open, attachment, form]);

  useEffect(() => {
    setInventory(null);
    if (!open || !attachment?.node_uuid) return;
    let cancelled = false;
    api.nodeNetworks(attachment.node_uuid).then((inv) => { if (!cancelled) setInventory(inv); }).catch(() => undefined);
    return () => { cancelled = true; };
  }, [open, attachment]);

  const interfaceOptions = useMemo(() => (inventory?.interfaces ?? []).map((i) => ({
    value: i.name,
    label: [i.name, ...i.addrs].join(" · "),
  })), [inventory]);
  const attached = new Set(network.attachments.filter((a) => a.target === "node").map((a) => a.node_uuid));
  // The picker searches nodes on the server; the ones picked are read back
  // to name them in the outcome.
  const picked = useNodeLookup((form.watch("node_uuids") as string[] | undefined) ?? []);
  const nodeName = (uuid: string) => {
    const n = picked.get(uuid);
    return n ? nodeDisplayName(n) : uuid;
  };

  const submit = form.handleSubmit(async (v) => {
    setSaving(true);
    try {
      if (attachment) {
        onSaved(await api.updateNetworkAttachment(network.id, attachment.id, { interface: v.interface?.trim() ?? "", ip: v.ip?.trim() ?? "", hcas: attachment.hcas }));
        message.success(t("message.saved"));
      } else {
        const out = await api.attachNetworkNodes(network.id, v.node_uuids ?? []);
        const failed = out.results.filter((r) => r.status === "failed");
        if (failed.length > 0) {
          message.warning(t("attach.partial", {
            ok: out.results.length - failed.length,
            failed: failed.map((r) => `${nodeName(r.node_uuid)}（${r.error ?? r.code}）`).join("、"),
          }));
        } else {
          message.success(t("attach.done", { count: out.results.length }));
        }
        onSaved(out.network);
      }
    } catch (e) {
      message.error(apiErrorText(tCommon, e));
    } finally {
      setSaving(false);
    }
  });

  return <Dialog open={open} onOpenChange={(next) => { if (!next && !saving) onClose(); }}>
    <DialogContent size="lg" className="max-h-[calc(100dvh-32px)] overflow-y-auto" closeButtonDisabled={saving} closeButtonLabel={t("actions.close", { ns: "common" })}>
      <form onSubmit={submit}>
        <DialogHeader><DialogTitle>{editing ? t(rdma ? "attach.editBootstrapTitle" : "attach.editTitle") : t("detail.attachNodes")}</DialogTitle></DialogHeader>
        <div className="flex flex-col gap-4">
          <FormField control={form.control} name="node_uuids" label={t("attach.nodes")} description={!editing && rdma ? t(isAcceleratorRDMA(network.type) ? "attach.cardInferred" : "attach.rdmaInferred") : undefined} required rules={{ validate: value => !!value?.length || t("attach.nodeRequired") }}>{field => <NodePicker id={field.id} value={field.value ?? []} onChange={field.onChange} mode="multiple" disabled={editing} aria-label={t("attach.nodes")} optionDisabled={node => !editing && attached.has(node.uuid)} />}</FormField>
          {editing && <>
            <h3 className="text-body font-semibold">{t(rdma ? "attach.bootstrapOverride" : "attach.overrides")}</h3>
            <FormField control={form.control} name="interface" label={t(rdma ? "attach.interfaceIB" : "attach.interface")}>{field => <Combobox items={interfaceOptions.map(option => option.value)} value={field.value ?? ""} inputValue={field.value ?? ""} onInputValueChange={field.onChange} onValueChange={value => { if (value !== null) field.onChange(value); }}>
              <ComboboxInput id={field.id} aria-label={t(rdma ? "attach.interfaceIB" : "attach.interface")} triggerAriaLabel={t(rdma ? "attach.interfaceIB" : "attach.interface")} />
              <ComboboxContent><ComboboxList>{(value: string) => <ComboboxItem key={value} value={value}>{interfaceOptions.find(option => option.value === value)?.label ?? value}</ComboboxItem>}</ComboboxList></ComboboxContent>
            </Combobox>}</FormField>
            <FormField control={form.control} name="ip" label={t("attach.ip")}>{field => <Input {...field} value={field.value ?? ""} placeholder={t("attach.ipPlaceholder")} />}</FormField>
          </>}
        </div>
        <DialogFooter><Button variant="ghost" disabled={saving} onClick={onClose}>{t("actions.cancel")}</Button><Button type="submit" loading={saving}>{t("actions.save")}</Button></DialogFooter>
      </form>
    </DialogContent>
  </Dialog>;
}
