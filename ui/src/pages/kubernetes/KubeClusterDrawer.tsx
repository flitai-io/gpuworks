import { useEffect, useMemo, useRef, useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster, type KubeClusterEdit } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { useKubeDefaults } from "../../kubeClusters";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../../components/ui/accordion";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { Switch } from "../../components/ui/switch";
import { message } from "../../ui-kit/feedback";
import { FormField } from "../../ui-kit/form/FormField";
import { formSubmit } from "../../ui-kit/form/submit";
import { Notice } from "../../ui-kit/Notice";
import { NumberInput } from "../../ui-kit/NumberInput";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import ApplyCommand from "./ApplyCommand";
import KubePoolPicker from "./KubePoolPicker";
import KubeClusterOperationModal from "./KubeClusterOperationModal";
import KubeEditSaved from "./KubeEditSaved";
import { ADVANCED_FIELDS, FIELD_OF, defaultsOf, formImageSource, formSchema, requestOf, updateOf, type FormValues, type KubeClusterPrefill } from "./kubeClusterForm";

export type { KubeClusterPrefill };

/**
 * Connects a Kubernetes cluster: the form, then the command to run in the
 * cluster. The list reloads as soon as the cluster exists; the drawer stays
 * on the command until it is closed.
 *
 * With `edit` it changes that entry instead: the same form, prefilled, with
 * the namespace, server address and state directory fixed. Saving sends only
 * the fields that changed; when the cluster then has to be brought in line, the
 * drawer says how (applying again, or the commands to run).
 */
export default function KubeClusterDrawer({ open, onClose, onCreated, onEdited, prefill, edit }: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
  /** Called when an edit was saved. */
  onEdited?: () => void;
  prefill?: KubeClusterPrefill;
  /** The entry to edit; absent connects a new one. */
  edit?: KubeCluster;
}) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const schema = useMemo(() => formSchema(t), [t]);
  const form = useForm<FormValues>({ resolver: zodResolver(schema), shouldFocusError: false, defaultValues: defaultsOf(prefill, edit) });
  const [keepCards, imageSource, allowPreemption] = useWatch({ control: form.control, name: ["keep_cards", "agent_image_source", "allow_preemption"] });
  const { defaults, error: defaultsError } = useKubeDefaults(open);
  const formRef = useRef<HTMLFormElement>(null);
  const nameInput = useRef<HTMLInputElement | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<KubeCluster | null>(null);
  const [edited, setEdited] = useState<KubeClusterEdit | null>(null);
  const [poolIds, setPoolIds] = useState<string[]>(prefill?.pool_ids ?? []);
  const [applying, setApplying] = useState(false);
  const [advancedOpen, setAdvancedOpen] = useState(false);

  // Each opening starts from the defaults (and the prefill), like a fresh form.
  useEffect(() => {
    if (!open) return;
    form.reset(defaultsOf(prefill, edit));
    setPoolIds(prefill?.pool_ids ?? []);
    setCreated(null);
    setEdited(null);
    setError(null);
    setApplying(false);
    setAdvancedOpen(false);
    // The drawer takes focus itself when it opens; hand it to the first field once it has.
    const timer = setTimeout(() => nameInput.current?.focus(), 0);
    return () => clearTimeout(timer);
  }, [open, prefill, edit]);

  const submit = async (values: FormValues) => {
    setSaving(true);
    setError(null);
    try {
      if (edit) {
        const update = updateOf(values, edit, defaults);
        if (Object.keys(update).length === 0) { onClose(); return; }
        const result = await api.updateKubeCluster(edit.uuid, update);
        onEdited?.();
        if (result.reapply) setEdited(result);
        else { message.success(t("edit.saved", { name: result.cluster.name })); onClose(); }
      } else {
        const cluster = await api.createKubeCluster(requestOf(values, defaults, poolIds, prefill));
        setCreated(cluster);
        onCreated();
      }
    } catch (e) {
      const text = apiErrorText(tCommon, e);
      const field = e instanceof APIError
        ? e.code === "KUBE_CLUSTER_NAME_TAKEN" ? "name" : e.code === "KUBE_CLUSTER_INVALID" ? FIELD_OF[String(e.params?.field ?? "")] : undefined
        : undefined;
      if (field) {
        form.setError(field, { type: "server", message: text });
        if (ADVANCED_FIELDS.has(field)) setAdvancedOpen(true);
      } else setError(text);
    } finally {
      setSaving(false);
    }
  };

  const advanced = (
    <div className="flex flex-col gap-4 pt-2">
      <FormField control={form.control} name="namespace" label={t("drawer.namespace")} description={edit ? t("drawer.fixedAfterCreate") : t("drawer.namespaceHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.namespace} disabled={Boolean(edit)} />}
      </FormField>
      <FormField control={form.control} name="agent_image_source" label={t("drawer.agentImageSource")} description={t("drawer.agentImageSourceHelp")}>
        {({ ref: _ref, onChange, ...binding }) => (
          <OptionSelect {...binding} value={formImageSource(binding.value, defaults, prefill)} onChange={onChange}
            options={(defaults?.agent_image_sources ?? []).map(({ id }) => ({ value: id, label: t(`drawer.agentImageSources.${id}`, { defaultValue: id }) }))} />
        )}
      </FormField>
      <FormField control={form.control} name="agent_image" label={t("drawer.agentImage")} description={t("drawer.agentImageHelp")}>
        {(binding) => <Input {...binding} placeholder={(defaults?.agent_image_sources ?? []).find((s) => s.id === formImageSource(imageSource, defaults, prefill))?.image ?? t("drawer.agentImagePlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="image_pull_secret" label={t("drawer.imagePullSecret")} description={t("drawer.imagePullSecretHelp")}>
        {(binding) => <Input {...binding} placeholder={t("drawer.imagePullSecretPlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="state_dir" label={t("drawer.stateDir")} description={edit ? t("drawer.fixedAfterCreate") : t("drawer.stateDirHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.state_dir} disabled={Boolean(edit)} />}
      </FormField>
      <FormField control={form.control} name="runtime_class" label={t("drawer.runtimeClass")} description={t("drawer.runtimeClassHelp")}>
        {(binding) => <Input {...binding} placeholder={t("drawer.runtimeClassPlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="containerd_socket" label={t("drawer.containerdSocket")} description={t("drawer.containerdSocketHelp")}>
        {(binding) => <Input {...binding} placeholder={t("drawer.containerdSocketPlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="api_server" label={t("drawer.apiServer")} description={t("drawer.apiServerHelp")}>
        {(binding) => <Input {...binding} placeholder={t("drawer.apiServerPlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="engine_ports" label={t("drawer.enginePorts")} description={t("drawer.enginePortsHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.engine_ports} />}
      </FormField>
      <FormField control={form.control} name="priority" label={t("drawer.priority")} description={t("drawer.priorityHelp")}>
        {(binding) => <NumberInput {...binding} min={0} max={defaults?.priority_max} precision={0} placeholder={defaults ? String(defaults.priority) : undefined} className="w-40" />}
      </FormField>
      <FormField control={form.control} name="allow_preemption" label={t("drawer.allowPreemption")} description={t("drawer.allowPreemptionHelp")}>
        {() => <Switch label={t("drawer.allowPreemptionSwitch")} checked={allowPreemption ?? defaults?.allow_preemption ?? false} disabled={!defaults} onCheckedChange={(checked) => form.setValue("allow_preemption", checked)} />}
      </FormField>
      <FormField control={form.control} name="idle_return_seconds" label={t("drawer.idleReturn")} description={keepCards ? t("drawer.keepCardsHelp") : t("drawer.idleReturnHelp")}>
        {(binding) => (
          <div className="flex flex-wrap items-center gap-3">
            <Switch label={t("drawer.keepCards")} checked={keepCards} disabled={!defaults} onCheckedChange={(checked) => form.setValue("keep_cards", checked)} />
            <div className="flex items-center gap-2">
              <NumberInput {...binding} aria-label={t("drawer.idleReturnSeconds")} min={0} precision={0} disabled={keepCards} placeholder={defaults ? String(defaults.idle_return_seconds) : undefined} className="w-32" />
              <span className="text-fg-muted">{t("drawer.secondsUnit")}</span>
            </div>
          </div>
        )}
      </FormField>
    </div>
  );

  const body = edited ? (
    <KubeEditSaved edit={edited} onApplied={onEdited} />
  ) : created ? (
    <div className="flex flex-col gap-4">
      <Notice tone="info" title={t("created.title", { name: created.name })}>{created.source ? t("created.noteSource") : t("created.note")}</Notice>
      {created.source && created.can_apply && (
        <div className="flex flex-col items-start gap-2">
          <strong className="font-semibold text-fg-default">{t("created.byGpuworks")}</strong>
          <span className="text-fg-muted">{t("created.byGpuworksHelp")}</span>
          <Button variant="primary" onClick={() => setApplying(true)}>{t("created.applyNow")}</Button>
          <strong className="mt-2 font-semibold text-fg-default">{t("created.byKubectl")}</strong>
        </div>
      )}
      {created.source && !created.can_apply && created.operation_blocked && (
        <Notice tone="warning" title={t("created.applyBlocked")}>{t(`blocked.${created.operation_blocked}`)}</Notice>
      )}
      {created.apply_command && <ApplyCommand command={created.apply_command} />}
      <span className="text-fg-muted">{t("created.secret")}</span>
      {applying && <KubeClusterOperationModal cluster={created} op="apply" onClose={() => setApplying(false)} onDone={onCreated} />}
    </div>
  ) : (
    <form id="kube-cluster-form" ref={formRef} noValidate onSubmit={formSubmit(form, () => formRef.current, submit)} className="flex flex-col gap-4">
      {/* The footer's button is outside the form; this one lets Enter in a field submit it. */}
      <button type="submit" hidden aria-hidden="true" tabIndex={-1} />
      {error && <Notice tone="danger" title={error} />}
      {defaultsError && <Notice tone="warning" title={t("drawer.defaultsFailed")} children={t("drawer.defaultsFailedHelp")} />}
      {prefill && <Notice tone="info" title={t("drawer.fromSource", { source: prefill.source_label })} />}
      <FormField control={form.control} name="name" label={t("drawer.name")} required>
        {({ ref, ...binding }) => <Input {...binding} ref={(node) => { ref(node); nameInput.current = node; }} placeholder={t("drawer.namePlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="server_url" label={t("drawer.serverUrl")} description={edit ? t("drawer.fixedAfterCreate") : t("drawer.serverUrlHelp")} required>
        {(binding) => <Input {...binding} disabled={Boolean(edit)} />}
      </FormField>
      {prefill && (
        <div className="flex flex-col gap-1.5">
          <strong className="font-semibold text-fg-default">{t("drawer.pools")}</strong>
          <span className="text-fg-muted">{t("drawer.poolsHelp")}</span>
          <KubePoolPicker pools={prefill.pools} selected={poolIds} onChange={setPoolIds} disabled={saving} label={t("drawer.pools")} />
        </div>
      )}
      {!prefill && <FormField control={form.control} name="node_selector" label={t("drawer.nodeSelector")}
        description={edit?.source ? t("drawer.nodeSelectorCloud") : t("drawer.nodeSelectorHelp")}>
        {(binding) => <Input {...binding} placeholder={t("drawer.nodeSelectorPlaceholder")} disabled={Boolean(edit?.source)} />}
      </FormField>}
      <Accordion type="single" collapsible value={advancedOpen ? "advanced" : ""} onValueChange={(next: string) => setAdvancedOpen(next === "advanced")}>
        <AccordionItem value="advanced">
          <AccordionTrigger>{t("drawer.advanced")}</AccordionTrigger>
          <AccordionContent>{advanced}</AccordionContent>
        </AccordionItem>
      </Accordion>
    </form>
  );

  return (
    <SideDrawer
      open={open}
      onClose={onClose}
      title={edit ? t("drawer.editTitle") : t("drawer.title")}
      width={560}
      busy={saving}
      footer={created || edited ? (
        <Button variant="primary" onClick={onClose}>{t("drawer.close")}</Button>
      ) : (
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>{t("drawer.cancel")}</Button>
          <Button type="submit" form="kube-cluster-form" variant="primary" loading={saving}>{edit ? t("drawer.save") : t("drawer.submit")}</Button>
        </>
      )}
    >
      {body}
    </SideDrawer>
  );
}
