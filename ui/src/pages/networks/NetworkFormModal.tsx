import { Checkbox } from "../../components/ui/checkbox";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../../components/ui/accordion";
import { Segmented } from "../../ui-kit/Segmented";
import { useForm } from "react-hook-form";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../../components/ui/dialog";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { FormField } from "../../ui-kit/form/FormField";
import { Notice } from "../../ui-kit/Notice";
import { message } from "../../ui-kit/feedback";
import { useEffect, useState } from "react";

import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { Network, NetworkDetail, NetworkRequest, NetworkRole, NetworkType } from "../../networkTypes";
import { isAcceleratorRDMA, usesRails } from "./networkDisplay";
import { HelpTip } from "../../ui-kit/HelpTip";

type FormValues = Required<Pick<NetworkRequest, "name" | "roles" | "type">> & Omit<NetworkRequest, "name" | "roles" | "type">;

export default function NetworkFormModal({
  open, network, roleOptions, typeOptions, typeRoles, onClose, onSaved,
}: {
  open: boolean;
  network?: Network | null;
  roleOptions: NetworkRole[];
  typeOptions: NetworkType[];
  /** Per type, the roles it may carry; a type missing here may carry any. */
  typeRoles?: Record<string, NetworkRole[]>;
  onClose: () => void;
  onSaved: (network: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  const form = useForm<FormValues>();
  const [saving, setSaving] = useState(false);
  const type = form.watch("type");
  const railsType = !!type && usesRails(type);
  const acceleratorType = !!type && isAcceleratorRDMA(type);
  const allowed = (t: NetworkType | undefined, role: NetworkRole) => !t || !typeRoles?.[t] || typeRoles[t].includes(role);

  // Roles the chosen type cannot carry are unticked as well as disabled, so
  // the form never submits a combination the server refuses.
  useEffect(() => {
    if (!open || !type) return;
    const roles: NetworkRole[] = form.getValues("roles") ?? [];
    const kept = roles.filter((role) => allowed(type, role));
    if (kept.length !== roles.length) form.setValue("roles", kept);
  }, [open, type, typeRoles, form]);

  useEffect(() => {
    if (!open) return;
    form.reset(network
      ? { name: network.name, description: network.description, roles: network.roles, type: network.type, cidr: network.cidr, interface: network.interface }
      : { name: "", description: "", roles: [], type: typeOptions[0] ?? "host_ethernet", cidr: "", interface: "" });
  }, [open, network, form, typeOptions]);

  const submit = form.handleSubmit(async (values) => {
    // A network that uses rails has no interface glob: its bootstrap is inferred per node.
    const body: NetworkRequest = { ...values, interface: usesRails(values.type) ? "" : values.interface };
    setSaving(true);
    try {
      const saved = network ? await api.updateNetwork(network.id, body) : await api.createNetwork(body);
      message.success(t("message.saved"));
      onSaved(saved);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  });

  return <Dialog open={open} onOpenChange={(next) => { if (!next && !saving) onClose(); }}>
    <DialogContent size="lg" className="max-h-[calc(100dvh-32px)] overflow-y-auto" closeButtonDisabled={saving} closeButtonLabel={t("actions.close", { ns: "common" })}>
      <form onSubmit={submit} onKeyDown={event => { if (event.key === "Enter" && (event.target as HTMLElement).tagName === "INPUT") event.preventDefault(); }}>
        <DialogHeader><DialogTitle>{t(network ? "form.editTitle" : "form.createTitle")}</DialogTitle></DialogHeader>
        <div className="flex flex-col gap-4">
          <FormField control={form.control} name="name" label={t("form.name")} required rules={{ validate: value => !!value?.trim() || t("form.nameRequired") }}>{field => <Input {...field} value={field.value ?? ""} maxLength={64} />}</FormField>
          <FormField control={form.control} name="description" label={t("form.description")}>{field => <Input {...field} value={field.value ?? ""} maxLength={256} />}</FormField>
          <FormField control={form.control} name="roles" label={t("form.roles")} description={t("form.rolesHint")} required rules={{ validate: value => !!value?.length || t("form.rolesRequired") }}>{field => <div id={field.id} role="group" aria-label={t("form.roles")} aria-required={field["aria-required"]} aria-invalid={field["aria-invalid"]} aria-describedby={field["aria-describedby"]} className="flex flex-wrap gap-3">
            {roleOptions.map(role => <label key={role} className="inline-flex items-center gap-2 text-body"><Checkbox aria-label={t(`role.${role}`)} checked={(field.value ?? []).includes(role)} disabled={!allowed(type, role)}
              onCheckedChange={checked => field.onChange(checked ? [...(field.value ?? []), role] : (field.value ?? []).filter(value => value !== role))} /><span>{t(`role.${role}`)}</span>{!allowed(type, role) && <span tabIndex={0} aria-label={`${t(`role.${role}`)}: ${t("form.roleNotOnType")}`}><HelpTip content={t("form.roleNotOnType")} /></span>}</label>)}
          </div>}</FormField>
          <FormField control={form.control} name="type" label={t("form.type")} description={<span className="inline-flex items-center gap-1">{t("form.typeHint")}<HelpTip content={t("form.typeHelp")} /></span>}>{field => <Segmented label={t("form.type")} value={field.value} onChange={field.onChange} options={typeOptions.map(value => ({ value, label: t(`type.${value}`, { defaultValue: value }) }))} />}</FormField>
          {railsType ? <>
            <Notice title={t(acceleratorType ? "form.acceleratorInferred" : "form.rdmaInferred")} />
            <Accordion type="single" collapsible defaultValue={network?.cidr ? "advanced" : undefined}><AccordionItem value="advanced">
              <AccordionTrigger>{t("form.advanced")}</AccordionTrigger>
              <AccordionContent><FormField control={form.control} name="cidr" label={t("form.bootstrapCidr")} description={t(acceleratorType ? "form.acceleratorBootstrapCidrHint" : "form.bootstrapCidrHint")}>{field => <Input {...field} value={field.value ?? ""} placeholder="10.20.0.0/16" />}</FormField></AccordionContent>
            </AccordionItem></Accordion>
          </> : <>
            <FormField control={form.control} name="cidr" label={t("form.cidr")} description={t("form.cidrHint")}>{field => <Input {...field} value={field.value ?? ""} placeholder="10.20.0.0/16" />}</FormField>
            <FormField control={form.control} name="interface" label={t("form.interface")} description={t("form.interfaceHint")}>{field => <Input {...field} value={field.value ?? ""} placeholder={t("form.interfacePlaceholder")} />}</FormField>
            <Notice title={t("form.selectorNote")} />
          </>}
        </div>
        <DialogFooter><Button variant="ghost" disabled={saving} onClick={onClose}>{t("actions.cancel")}</Button><Button type="submit" loading={saving}>{t("actions.save")}</Button></DialogFooter>
      </form>
    </DialogContent>
  </Dialog>;
}
